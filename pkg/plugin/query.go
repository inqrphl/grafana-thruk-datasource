package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data"
)

// queryMetadata captures per-request metadata from Grafana (user, org, headers)
// plus the frontend-injected dashboard/panel context from the query JSON.
type queryMetadata struct {
	// OrgID is deprecated on the SDK, do not define or use it
	// OrgID          int64
	Namespace      string
	PluginID       string
	PluginVersion  string
	DatasourceUID  string
	GrafanaVersion string
	User           *backend.User

	DashboardUID   string
	DashboardTitle string
	PanelId        int64
	PanelName      string
	PanelPluginId  string
	App            string
	RequestUrl     string

	// authHeaders are the forwarded headers relevant for authenticating the
	// request against Thruk. They are also used to scope the response cache.
	authHeaders map[string][]string
}

// buildQueryMetadata extracts user/org/header metadata from the query context and the request.
// The frontend-injected dashboard/panel fields are filled in later per query, once the query JSON has been parsed.
func buildQueryMetadata(ctx context.Context, req *backend.QueryDataRequest) *queryMetadata {
	pc := backend.PluginConfigFromContext(ctx)
	meta := &queryMetadata{
		// OrgID is deprecated on the SDK, do not define or use it
		// OrgID:         pc.OrgID,
		Namespace:     pc.Namespace,
		PluginID:      pc.PluginID,
		PluginVersion: pc.PluginVersion,
		User:          backend.UserFromContext(ctx),
	}
	if pc.DataSourceInstanceSettings != nil {
		meta.DatasourceUID = pc.DataSourceInstanceSettings.UID
	}
	if ua := backend.UserAgentFromContext(ctx); ua != nil {
		meta.GrafanaVersion = ua.GrafanaVersion()
	}
	if req != nil {
		meta.authHeaders = buildAuthHeaders(req.GetHTTPHeaders())
	}
	return meta
}

// hasCookie reports whether the forwarded Cookie header contains a cookie with the given name.
func (m *queryMetadata) hasCookie(name string) bool {
	for _, value := range m.authHeaders["Cookie"] {
		for _, part := range strings.Split(value, ";") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, name+"=") {
				return true
			}
		}
	}
	return false
}

// String returns a compact single-line representation of the request metadata for logging/auditing.
func (m *queryMetadata) String() string {
	user := "none"
	if m.User != nil {
		user = fmt.Sprintf("%s (%s, %s, role=%s)", m.User.Login, m.User.Name, m.User.Email, m.User.Role)
	}

	org := m.Namespace

	// orgID is depreacted and therefore not saved
	// if org == "" {
	// 	org = fmt.Sprintf("%d", m.OrgID)
	// }

	return fmt.Sprintf(
		"user=%s org=%s grafanaVersion=%s dsUid=%s dashboardUid=%s dashboardTitle=%q panelId=%d panelName=%q app=%s url=%q thruk_auth=%t",
		user, org, m.GrafanaVersion, m.DatasourceUID,
		m.DashboardUID, m.DashboardTitle, m.PanelId, m.PanelName, m.App, m.RequestUrl,
		m.hasCookie("thruk_auth"),
	)
}

func query(ctx context.Context, datasource *Datasource, query backend.DataQuery, meta *queryMetadata) backend.DataResponse {
	var qm queryModel
	if err := json.Unmarshal(query.JSON, &qm); err != nil {
		logger.Debugf("refId=%s unmarshal error: %v", query.RefID, err)
		return backend.ErrDataResponse(backend.StatusBadRequest, fmt.Sprintf("json unmarshal: %v", err.Error()))
	}

	// merge the frontend-injected dashboard/panel context into a per-query copy
	qmeta := *meta
	qmeta.DashboardUID = qm.DashboardUID
	qmeta.DashboardTitle = qm.DashboardTitle
	qmeta.PanelId = qm.PanelId
	qmeta.PanelName = qm.PanelName
	qmeta.PanelPluginId = qm.PanelPluginId
	qmeta.App = qm.App
	qmeta.RequestUrl = qm.RequestUrl

	logger.Debugf("%s refId=%s table=%s columns=%v condition=%q limit=%d type=%v",
		qmeta.String(), query.RefID, qm.Table, qm.Columns, qm.Condition, qm.Limit, qm.Type)

	rewriteAliasedEndpoints(&qm)

	logger.Debugf("rewritten refId=%s table=%s columns=%v condition=%q limit=%d type=%v",
		query.RefID, qm.Table, qm.Columns, qm.Condition, qm.Limit, qm.Type)

	thrukURL := buildQueryURL(datasource, qm)
	logger.Debugf("thrukURL: %s", thrukURL)

	cachedResult, err := getCachedResult(&qm, datasource.uid, thrukURL, &qmeta.authHeaders)
	if err != nil {
		logger.Debugf("error when getting cached result: %s", err.Error())
	}
	if cachedResult != nil {
		logger.Debugf("found and using cached result for query %s", thrukURL)
		return *cachedResult.result
	}

	req, err := http.NewRequestWithContext(ctx, "GET", thrukURL, nil)
	if err != nil {
		logger.Debugf("failed to create request: %v", err)
		return backend.ErrDataResponse(backend.StatusBadRequest, fmt.Sprintf("failed to create request: %v", err))
	}
	req.Header.Set("X-THRUK-OutputFormat", "wrapped_json")

	// The SDK forwards headers itself when ForwardHTTPHeaders is enabled, but still
	// set the Cookie explicitly as a safety net
	// this is for requests without a Grafana web frontend session, e.g. the ones done by the Alerting system
	if cookies := qmeta.authHeaders["Cookie"]; len(cookies) > 0 {
		req.Header.Set("Cookie", strings.Join(cookies, "; "))
	}

	logger.Debugf("HTTP GET %s\n", thrukURL)
	start := time.Now()
	resp, err := datasource.httpClient.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		logger.Debugf("request failed after %v: %v", elapsed, err)
		return backend.ErrDataResponse(backend.StatusBadRequest, fmt.Sprintf("request failed: %v", err))
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Debugf("failed to read response: %v", err)
		return backend.ErrDataResponse(backend.StatusBadRequest, fmt.Sprintf("failed to read response: %v", err))
	}

	logger.Debugf("response code: %d %s , elapsed: %v , bytes: %d", resp.StatusCode, resp.Status, elapsed, len(body))

	if resp.StatusCode != http.StatusOK {
		return backend.ErrDataResponse(backend.StatusBadRequest, fmt.Sprintf("thruk returned status: %d , body: %s", resp.StatusCode, string(body)))
	}

	parseStart := time.Now()
	result := parseThrukResponse(body, qm, query.TimeRange)
	logger.Debugf("refId=%s parsed in %v", query.RefID, time.Since(parseStart))

	err = writeCachedResult(&qm, datasource.uid, thrukURL, &qmeta.authHeaders, &result)
	if err != nil {
		logger.Debugf("error when writing cached result: %s", err.Error())
	}

	return result
}

func buildQueryURL(datasource *Datasource, qm queryModel) string {
	rewriteAliasedEndpoints(&qm)

	path := strings.TrimPrefix(qm.Table, "/")
	u := fmt.Sprintf("%s/r/v1/%s", datasource.url, path)

	limit := qm.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	u += "?limit=" + strconv.Itoa(limit)

	if len(qm.Columns) > 0 && !(len(qm.Columns) == 1 && qm.Columns[0] == "*") {
		u += "&columns=" + url.QueryEscape(strings.Join(qm.Columns, ","))
	}
	if qm.Condition != "" {
		u += "&q=" + url.QueryEscape(qm.Condition)
	}

	return u
}

// intended to parse thruk reponses in wrapped_json format
// The "data" field of the json can either be an array of objects or simply an object
// Take a look under /docs/call-r-v1-hosts.sh for an array response.
// Take a look under /docs/call-r-v1-services-totals.sh for an object example
func parseThrukResponse(body []byte, qm queryModel, timeRange backend.TimeRange) backend.DataResponse {
	var thrukResp ThrukWrappedJsonResponse

	// Try wrapped_json format: { "data": <array|object> , "meta": {...} }
	var rawResponse struct {
		Data json.RawMessage               `json:"data"`
		Meta *ThrukWrappedJsonResponseMeta `json:"meta"`
	}

	if err := json.Unmarshal(body, &rawResponse); err == nil && rawResponse.Data != nil {
		thrukResp.Meta = rawResponse.Meta
		// Try data as array first, then as single object
		if err := json.Unmarshal(rawResponse.Data, &thrukResp.Data); err != nil {
			// if unmarshalling did not work, it is probably a single json object
			var dataObj map[string]any
			if err2 := json.Unmarshal(rawResponse.Data, &dataObj); err2 == nil {
				thrukResp.Data = []map[string]any{dataObj}
			}
		}
	} else {
		// Not wrapped_json

		// Try as an array of jsonObjects
		var arrayOfJsonObjects []map[string]any
		if err := json.Unmarshal(body, &arrayOfJsonObjects); err == nil {
			thrukResp.Data = arrayOfJsonObjects
		} else {
			// Try as single JsonObject
			var singleJsonObject map[string]any
			if err := json.Unmarshal(body, &singleJsonObject); err != nil {
				logger.Debugf("[QueryData] failed to parse response: %v", err)
				return backend.ErrDataResponse(backend.StatusBadRequest, fmt.Sprintf("failed to parse response: %v", err))
			}
			thrukResp.Data = []map[string]any{singleJsonObject}
		}
	}

	if len(thrukResp.Data) == 0 {
		logger.Debugf("[QueryData] empty response, 0 rows returned")
		return backend.DataResponse{Frames: data.Frames{data.NewFrame("response")}}
	}

	visType := parseVisualizationType(qm.Type)

	if visType == "graph" {
		return buildTimeseriesFrames(&thrukResp, timeRange, qm)
	}

	return buildTableFrame(&qm, &thrukResp, visType)
}

// This function assumes that thrukResponse.Data is of type []map[string]any
// Even when the response was a single object, it is converted in parseThrukResponse method
func buildTableFrame(qm *queryModel, thrukResp *ThrukWrappedJsonResponse, visType string) backend.DataResponse {

	// add known query types from query model and columns
	overrideKnownGrafanaDataTypes(qm, thrukResp.Meta)

	columnMetadatas := buildColumnMetadataMap(thrukResp)
	columns := determineColumnsFromThrukResponse(thrukResp)

	frame := data.NewFrame("response")
	for _, col := range columns {

		processUnitType(col, columnMetadatas)

		unknownFieldType := false
		fieldType, metadataWritenType := inferFieldType(col, columnMetadatas)
		if fieldType == data.FieldTypeUnknown {
			unknownFieldType = true
			fieldType = data.FieldTypeString
		}

		field := data.NewFieldFromFieldType(fieldType, 0)
		field.Name = col

		logger.Debugf("[TableFrame] building column: %s , fieldType is unknown: %t , final fieldType: %s", col, unknownFieldType, FieldTypeToString(fieldType))

		for _, row := range thrukResp.Data {
			val := row[col]
			// d.logger.Printf("[TableFrame] [Column: %s] val: %v", col, val)

			// unknown field types default to strings with white background
			if unknownFieldType {
				field.Append(anyToString(val))
				field.Config = &data.FieldConfig{
					Description: "string",
					Color:       map[string]any{"mode": "fixed", "fixedColor": "white"},
					Custom:      map[string]any{"cellOptions": map[string]any{"type": "color-background"}},
				}
				continue
			}

			switch fieldType {
			case data.FieldTypeInt64:
				field.Append(anyToInt64(val))
				field.Config = &data.FieldConfig{
					Description: "int64",
					Color:       map[string]any{"mode": "fixed", "fixedColor": "blue"},
					Custom:      map[string]any{"cellOptions": map[string]any{"type": "color-background"}},
				}
			case data.FieldTypeFloat64:
				field.Append(anyToFloat64(val))
				field.Config = &data.FieldConfig{
					Description: "float64",
					Color:       map[string]any{"mode": "fixed", "fixedColor": "silver"},
					Custom:      map[string]any{"cellOptions": map[string]any{"type": "color-background"}},
				}
			case data.FieldTypeTime:
				field.Append(anyToTime(val))
				field.Config = &data.FieldConfig{
					Description: "time",
					Color:       map[string]any{"mode": "fixed", "fixedColor": "green"},
					Custom:      map[string]any{"cellOptions": map[string]any{"type": "color-background"}},
				}
			case data.FieldTypeBool:
				field.Append(anyToBool(val))
				// Bool fields have built in coloring , light green and light red
				field.Config = &data.FieldConfig{
					Description: "bool",
					Custom:      map[string]any{"cellOptions": map[string]any{"mode": "thresholds", "type": "color-background"}},
				}
			case data.FieldTypeString:
				switch metadataWritenType {
				// array of strings
				// gets a different color, fuchisia
				case "array_of_strings":
					val2 := []string{}
					if valAsAnyArray, ok := val.([]any); ok {
						for _, elem := range valAsAnyArray {
							val2 = append(val2, anyToString(elem))
						}
					}
					field.Append(anyToString(val2))
					field.Config = &data.FieldConfig{
						Description: "string",
						Color:       map[string]any{"mode": "fixed", "fixedColor": "fuchsia"},
						Custom:      map[string]any{"cellOptions": map[string]any{"type": "color-background"}},
					}
				// normal string that is to be displayed as a string
				default:
					field.Append(anyToString(val))
					field.Config = &data.FieldConfig{
						Description: "string",
						Color:       map[string]any{"mode": "fixed", "fixedColor": "purple"},
						Custom:      map[string]any{"cellOptions": map[string]any{"type": "color-background"}},
					}
				}

			default:
				field.Append(anyToString(val))
				field.Config = &data.FieldConfig{
					Description: "string",
					Color:       map[string]any{"mode": "fixed", "fixedColor": "black"},
					Custom:      map[string]any{"cellOptions": map[string]any{"type": "color-background"}},
				}
			}
		}

		frame.Fields = append(frame.Fields, field)
	}

	logger.Debugf("[QueryData] table: %d rows, %d columns", len(thrukResp.Data), len(columns))
	frame.Meta = &data.FrameMeta{PreferredVisualization: data.VisType(visType)}
	return backend.DataResponse{Frames: data.Frames{frame}}
}

// buildTimeseriesFrames converts tabular Thruk data into Grafana time series frames.
// This is the Go equivalent of the older frontend only plugin _fakeTimeseries() method.
// Each data row becomes its own frame. Columns with aggregation functions (e.g. "count()")
// or numeric values become the value column; remaining columns form the series alias.
// The value is spread across 10 evenly-spaced time points covering the query's time range.
func buildTimeseriesFrames(thrukResp *ThrukWrappedJsonResponse, timeRange backend.TimeRange, qm queryModel) backend.DataResponse {
	const steps = 10
	from := timeRange.From.Unix()
	to := timeRange.To.Unix()
	step := (to - from) / steps
	if step <= 0 {
		step = 1
	}

	metaColumns := buildColumnMetadataMap(thrukResp)
	columns := determineColumnsFromThrukResponse(thrukResp)

	// Use user-specified columns if provided, otherwise use all response columns
	orderedColumns := columns
	if len(qm.Columns) > 0 && !(len(qm.Columns) == 1 && qm.Columns[0] == "*") {
		orderedColumns = qm.Columns
	}

	dataRows := thrukResp.Data

	// Convert single-row with many columns into key-value pairs, same as frontend
	if len(dataRows) == 1 && len(orderedColumns) > 2 {
		converted := make([]map[string]any, 0, len(orderedColumns))
		for _, key := range orderedColumns {
			converted = append(converted, map[string]any{
				"__key":   key,
				"__value": dataRows[0][key],
			})
		}
		dataRows = converted
		orderedColumns = []string{"__key", "__value"}
		// Override meta types for the converted columns
		metaColumns["__key"] = ThrukWrappedJsonResponseMetaColumn{Name: "__key"}
		metaColumns["__value"] = ThrukWrappedJsonResponseMetaColumn{Name: "__value",
			GrafanaDataType: data.FieldTypeFloat64}
	}

	// Find value column: first aggregation column, or first numeric, or first available
	valueCol := findValueColumn(orderedColumns, metaColumns, dataRows)

	// Name columns are all remaining columns not used as value
	var nameCols []string
	for _, col := range orderedColumns {
		if col != valueCol {
			nameCols = append(nameCols, col)
		}
	}

	var frames data.Frames
	logger.Debugf("[QueryData] timeseries: %d rows, valueCol=%s, nameCols=%v", len(dataRows), valueCol, nameCols)

	for _, row := range dataRows {
		val := row[valueCol]
		alias := valueCol
		if len(nameCols) > 0 {
			parts := make([]string, 0, len(nameCols))
			for _, nc := range nameCols {
				parts = append(parts, fmt.Sprintf("%v", row[nc]))
			}
			alias = strings.Join(parts, ";")
		}

		frame := data.NewFrame(alias)
		frame.Fields = append(frame.Fields,
			data.NewField("time", nil, make([]time.Time, steps)),
			data.NewField(alias, nil, make([]float64, steps)),
		)

		for i := 0; i < steps; i++ {
			frame.Set(0, i, time.Unix(from+step*int64(i), 0).UTC())
			frame.Set(1, i, anyToFloat64(val))
		}

		frame.Meta = &data.FrameMeta{
			PreferredVisualization: data.VisTypeGraph,
		}
		frames = append(frames, frame)
	}

	return backend.DataResponse{Frames: frames}
}

// finds the column with numerical values to use in timeseries visualization
func findValueColumn(columns []string, metaColumns map[string]ThrukWrappedJsonResponseMetaColumn, dataRows []map[string]any) string {
	if len(columns) == 0 {
		return ""
	}

	// First preference: column using aggregation function e.g. "count()" , "max()"
	for _, col := range columns {
		if strings.Contains(col, "(") && strings.Contains(col, ")") {
			return col
		}
	}

	// Second preference: first numeric column
	if len(dataRows) > 0 {
		for _, col := range columns {
			if mc, ok := metaColumns[col]; ok {
				if mc.GrafanaDataType != data.FieldTypeUnknown {
					if mc.GrafanaDataType == data.FieldTypeFloat64 || mc.GrafanaDataType == data.FieldTypeInt64 {
						return col
					}
				}
				if mc.Type == "number" {
					return col
				}
			}
			// Fallback: check the actual value
			if _, isNum := dataRows[0][col].(float64); isNum {
				return col
			}
			if _, isNum := dataRows[0][col].(json.Number); isNum {
				return col
			}
		}
	}

	// Third preference: first available column
	return columns[0]
}
