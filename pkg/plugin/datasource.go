package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/httpclient"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
	"github.com/grafana/grafana-plugin-sdk-go/data"
)

var (
	_ backend.QueryDataHandler      = (*Datasource)(nil)
	_ backend.CheckHealthHandler    = (*Datasource)(nil)
	_ backend.CallResourceHandler   = (*Datasource)(nil)
	_ instancemgmt.InstanceDisposer = (*Datasource)(nil)
)

const defaultLimit = 1000

// What a query coming in from Grafana will contain
// Defined in types.ts as ThrukQuery in frontend part.
type queryModel struct {
	Table     string   `json:"table"`
	Columns   []string `json:"columns"`
	Condition string   `json:"condition"`
	Limit     int      `json:"limit"`
	// can be a string
	// can be a object {"label": "Timeseries","value": "graph"}
	Type any `json:"type"`

	// metadata injected by the frontend for backend logging/auditing
	DashboardUID   string `json:"dashboardUID,omitempty"`
	DashboardTitle string `json:"dashboardTitle,omitempty"`
	PanelId        int64  `json:"panelId,omitempty"`
	PanelName      string `json:"panelName,omitempty"`
	PanelPluginId  string `json:"panelPluginId,omitempty"`
	App            string `json:"app,omitempty"`
	RequestUrl     string `json:"requestUrl,omitempty"`
}

// This struct contains our own definition of the Datasource and the components it needs
// It should implement CheckHealth() , Query() , Dispose() , CallResource() etc.
type Datasource struct {
	url        string
	httpClient *http.Client
	logger     *log.Logger
	logFile    *os.File
	uid        string
}

// There are more fields in the settings.JSONData of type json.RawMessage , but not all of them are parsed or need to be parsed.
// Only the necessary ones are defined here to be unmarshalled.
//
// DatasourceSettingsJSONData is a partial type to parse backend.DataSourceInstanceSettings.JSONData with
// jsonData is assembled by the Grafana datasource config UI (src/components/ConfigEditor.tsx).
// It mixes the plugin's own options ThrukDataSourceOptions, fields written by @grafana/plugin-ui components ConnectionSettings, Auth, AdvancedHttpSettings, and Grafana core:
type DatasourceSettingsJSONData struct {
	// the plugin's own options
	// from interface ThrukDataSourceOptions in src/types.ts
	// ======================
	// 'thruk_auth' is always added when parsing props in ConfigEditor.tsx
	KeepCookies []string `json:"keepCookies"`
	// Has its own <Input> field in ConfigEditor.tsx
	LogLevel int64 `json:"logLevel"`
	// Has its own <Input> field in ConfigEditor.tsx
	LogPath string `json:"logPath"`
	// ======================

	// from Auth part part of the ConfigEditor.tsx
	// Tls configuration is parsed in grafana-plugin-sdk-go/backend/http_settings.go:parseHTTPSettings
	// TlsAuth       *bool   `json:"tlsAuth,omitempty"`
	// TlsSkipVerify *bool   `json:"tlsSkipVerify,omitempty"`
	// ServerName      *string  `json:"serverName,omitempty"`

	// from Auth part part of the ConfigEditor.tsx
	// Headers are parsed in grafana-plugin-sdk-go/backend/http_settings.go:parseHTTPSettings
	// HTTPHeaderName1 *string  `json:"httpHeaderName1,omitempty"`
}

// This function is to be implemented accoring to the SDK interface
func NewDatasource(ctx context.Context, settings backend.DataSourceInstanceSettings) (instancemgmt.Instance, error) {
	u := strings.TrimRight(settings.URL, "/")

	var jsonData DatasourceSettingsJSONData
	if settings.JSONData != nil {
		if err := json.Unmarshal(settings.JSONData, &jsonData); err != nil {
			return nil, fmt.Errorf("failed to parse jsonData: %w", err)
		}
	}

	logger, logFile := createLoggerFromDatasourceSettings(&jsonData)
	logger.Printf("[NewDatasource] setttings:\n%s", DataSourceInstanceSettingsToString(&settings))

	// SDK provides a way of building http client options directly from context. This sets
	// Headers to forward, TLS configuration, Basic HTTP Authentication, Proxy, Timeouts, SigV4
	httpOpts, err := settings.HTTPClientOptions(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get http client options from context: %w", err)
	}
	httpclientOptionsSetDefaults(&httpOpts)

	logger.Printf("[NewDatasource] http client options: %s", HTTPClientOptionsToString(httpOpts))

	provider := httpclient.NewProvider()
	client, err := provider.New(httpOpts)
	if err != nil {
		return nil, fmt.Errorf("couldnt create http client using provider: %w", err)
	}

	logger.Printf("[NewDatasource] creating new datasource with uid: %s", settings.UID)

	return &Datasource{
		url:        u,
		httpClient: client,
		logger:     logger,
		logFile:    logFile,
		uid:        settings.UID,
	}, nil
}

// This function is to be implemented accoring to the SDK interface
func (d *Datasource) Dispose() {
	if d.logger != nil {
		d.logger.Println("Plugin instance disposed")
	}
	if d.logFile != nil {
		d.logFile.Close()
	}
}

// This function is to be implemented accoring to the SDK interface
func (d *Datasource) CheckHealth(ctx context.Context, _ *backend.CheckHealthRequest) (*backend.CheckHealthResult, error) {
	d.logger.Println("[CheckHealth] testing connection")

	thrukURL := d.url + "/r/v1/thruk?columns=thruk_version"
	d.logger.Printf("[datasource: %s] [CheckHealth] GET %s\n", d.uid, thrukURL)

	req, err := http.NewRequestWithContext(ctx, "GET", thrukURL, nil)
	if err != nil {
		d.logger.Printf("[CheckHealth] failed to create request: %v", err)
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: fmt.Sprintf("Failed to create request: %v", err),
		}, nil
	}
	d.logger.Printf("[datasource: %s] [CheckHealth] Cookie Header: %s", d.uid, req.Header.Values("Cookie"))

	start := time.Now()
	resp, err := d.httpClient.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		d.logger.Printf("[datasource: %s] [CheckHealth] connection failed after %v: %v", d.uid, elapsed, err)
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: fmt.Sprintf("Connection failed: %v", err),
		}, nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)

	var CheckHealthResponseType struct {
		ThrukVersion string `json:"thruk_version"`
	}

	d.logger.Printf("[datasource: %s] [CheckHealth] response code: %d , elapsed: %v", d.uid, resp.StatusCode, elapsed)
	d.logger.Printf("[datasource: %s] [CheckHealth] response body: %s", d.uid, string(body))

	if resp.StatusCode != http.StatusOK {
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: fmt.Sprintf("Unexpected status %d", resp.StatusCode),
		}, nil
	}

	if err := json.Unmarshal(body, &CheckHealthResponseType); err != nil {
		d.logger.Printf("[CheckHealth] failed to parse response: %v", err)
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: fmt.Sprintf("Failed to parse response: %v", err),
		}, nil
	}

	if CheckHealthResponseType.ThrukVersion == "" {
		d.logger.Println("[CheckHealth] no thruk_version in response")
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: "Invalid URL, did not find Thruk version in response",
		}, nil
	}

	d.logger.Printf("[CheckHealth] connected to Thruk v%s", CheckHealthResponseType.ThrukVersion)
	return &backend.CheckHealthResult{
		Status:  backend.HealthStatusOk,
		Message: "Successfully connected to Thruk v" + CheckHealthResponseType.ThrukVersion,
	}, nil
}

// This function is to be implemented accoring to the SDK interface
func (d *Datasource) QueryData(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
	meta := buildQueryMetadata(ctx, req)
	d.logger.Printf("[datasource: %s] [QueryData] %s received %d queries", d.uid, meta.String(), len(req.Queries))

	response := backend.NewQueryDataResponse()
	for _, q := range req.Queries {
		res := d.query(ctx, q, meta)
		response.Responses[q.RefID] = res
	}

	//responseJSON, _ := response.DeepCopy().MarshalJSON()
	//d.logger.Printf("[QueryData] response:\n%v", string(responseJSON))

	return response, nil
}

func (d *Datasource) query(ctx context.Context, query backend.DataQuery, meta *queryMetadata) backend.DataResponse {
	var qm queryModel
	if err := json.Unmarshal(query.JSON, &qm); err != nil {
		d.logger.Printf("[QueryData] refId=%s unmarshal error: %v", query.RefID, err)
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

	d.logger.Printf("[QueryData] %s refId=%s table=%s columns=%v condition=%q limit=%d type=%v",
		qmeta.String(), query.RefID, qm.Table, qm.Columns, qm.Condition, qm.Limit, qm.Type)

	rewriteAliasedEndpoints(&qm)

	d.logger.Printf("[QueryData] rewritten refId=%s table=%s columns=%v condition=%q limit=%d type=%v",
		query.RefID, qm.Table, qm.Columns, qm.Condition, qm.Limit, qm.Type)

	thrukURL := d.buildQueryURL(qm)
	d.logger.Printf("[HTTP] GET %s", thrukURL)

	cachedResult, err := getCachedResult(&qm, d.uid, thrukURL, &qmeta.authHeaders)
	if err != nil {
		d.logger.Printf("[CACHE] error when getting cached result: %s", err.Error())
	}
	if cachedResult != nil {
		d.logger.Printf("[CACHE] using cached result for query %s", thrukURL)
		return *cachedResult.result
	}

	req, err := http.NewRequestWithContext(ctx, "GET", thrukURL, nil)
	if err != nil {
		d.logger.Printf("[HTTP] failed to create request: %v", err)
		return backend.ErrDataResponse(backend.StatusBadRequest, fmt.Sprintf("failed to create request: %v", err))
	}
	req.Header.Set("X-THRUK-OutputFormat", "wrapped_json")

	// The SDK forwards headers itself when ForwardHTTPHeaders is enabled, but still
	// set the Cookie explicitly as a safety net for requests without a Grafana frontend session (e.g. alerting).
	if cookies := qmeta.authHeaders["Cookie"]; len(cookies) > 0 {
		req.Header.Set("Cookie", strings.Join(cookies, "; "))
	}

	start := time.Now()
	resp, err := d.httpClient.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		d.logger.Printf("[HTTP] request failed after %v: %v", elapsed, err)
		return backend.ErrDataResponse(backend.StatusBadRequest, fmt.Sprintf("request failed: %v", err))
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		d.logger.Printf("[HTTP] failed to read response: %v", err)
		return backend.ErrDataResponse(backend.StatusBadRequest, fmt.Sprintf("failed to read response: %v", err))
	}

	d.logger.Printf("[HTTP] response code: %d %s , elapsed: %v , bytes: %d", resp.StatusCode, resp.Status, elapsed, len(body))

	if resp.StatusCode != http.StatusOK {
		return backend.ErrDataResponse(backend.StatusBadRequest, fmt.Sprintf("thruk returned status: %d , body: %s", resp.StatusCode, string(body)))
	}

	parseStart := time.Now()
	result := d.parseThrukResponse(body, qm, query.TimeRange)
	d.logger.Printf("[QueryData] refId=%s parsed in %v", query.RefID, time.Since(parseStart))

	err = writeCachedResult(&qm, d.uid, thrukURL, &qmeta.authHeaders, &result)
	if err != nil {
		d.logger.Printf("[CACHE] error when writing cached result: %s", err.Error())
	}

	return result
}

func (d *Datasource) buildQueryURL(qm queryModel) string {
	rewriteAliasedEndpoints(&qm)

	path := strings.TrimPrefix(qm.Table, "/")
	u := fmt.Sprintf("%s/r/v1/%s", d.url, path)

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
func (d *Datasource) parseThrukResponse(body []byte, qm queryModel, timeRange backend.TimeRange) backend.DataResponse {
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
				d.logger.Printf("[QueryData] failed to parse response: %v", err)
				return backend.ErrDataResponse(backend.StatusBadRequest, fmt.Sprintf("failed to parse response: %v", err))
			}
			thrukResp.Data = []map[string]any{singleJsonObject}
		}
	}

	if len(thrukResp.Data) == 0 {
		d.logger.Printf("[QueryData] empty response, 0 rows returned")
		return backend.DataResponse{Frames: data.Frames{data.NewFrame("response")}}
	}

	visType := parseVisualizationType(qm.Type)

	if visType == "graph" {
		return d.buildTimeseriesFrames(&thrukResp, timeRange, qm)
	}

	return d.buildTableFrame(&qm, &thrukResp, visType)
}

// This function assumes that thrukResponse.Data is of type []map[string]any
// Even when the response was a single object, it is converted in parseThrukResponse method
func (d *Datasource) buildTableFrame(qm *queryModel, thrukResp *ThrukWrappedJsonResponse, visType string) backend.DataResponse {

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

		d.logger.Printf("[TableFrame] building column: %s , fieldType is unknown: %t , final fieldType: %s", col, unknownFieldType, FieldTypeToString(fieldType))

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

	d.logger.Printf("[QueryData] table: %d rows, %d columns", len(thrukResp.Data), len(columns))
	frame.Meta = &data.FrameMeta{PreferredVisualization: data.VisType(visType)}
	return backend.DataResponse{Frames: data.Frames{frame}}
}

// buildTimeseriesFrames converts tabular Thruk data into Grafana time series frames.
// This is the Go equivalent of the older frontend only plugin _fakeTimeseries() method.
// Each data row becomes its own frame. Columns with aggregation functions (e.g. "count()")
// or numeric values become the value column; remaining columns form the series alias.
// The value is spread across 10 evenly-spaced time points covering the query's time range.
func (d *Datasource) buildTimeseriesFrames(thrukResp *ThrukWrappedJsonResponse, timeRange backend.TimeRange, qm queryModel) backend.DataResponse {
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
	d.logger.Printf("[QueryData] timeseries: %d rows, valueCol=%s, nameCols=%v", len(dataRows), valueCol, nameCols)

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

func (d *Datasource) CallResource(ctx context.Context, req *backend.CallResourceRequest, sender backend.CallResourceResponseSender) error {
	d.logger.Printf("[Resource] path: %s url: %s", req.Path, req.URL)

	var thrukPath string
	var extraHeaders map[string]string

	switch req.Path {
	case "tables":
		thrukPath = "/r/v1/index?columns=url&protocol=get"
	case "columns":
		table := getQueryParam(req.URL, "table")
		if table == "" {
			d.logger.Printf("[Resource] missing table parameter")
			return sender.Send(&backend.CallResourceResponse{
				Status: http.StatusBadRequest,
				Body:   []byte("missing 'table' query parameter"),
			})
		}
		table = strings.TrimPrefix(table, "/")
		thrukPath = "/r/v1/" + table
		extraHeaders = map[string]string{"X-Thruk-Output-Metadata-Only": "true"}
	case "variable-query":
		table := getQueryParam(req.URL, "table")
		q := getQueryParam(req.URL, "q")
		columns := getQueryParam(req.URL, "columns")
		limit := getQueryParam(req.URL, "limit")
		if table == "" {
			d.logger.Printf("[Resource] variable-query missing table parameter")
			return sender.Send(&backend.CallResourceResponse{
				Status: http.StatusBadRequest,
				Body:   []byte("missing 'table' query parameter"),
			})
		}
		table = strings.TrimPrefix(table, "/")
		thrukPath = "/r/v1/" + table + "?columns=" + url.QueryEscape(columns) +
			"&q=" + url.QueryEscape(q) +
			"&limit=" + url.QueryEscape(limit)
		extraHeaders = map[string]string{"X-Thruk-Output-Metadata-Only": "true"}
	default:
		thrukPath = "/r/v1/" + strings.TrimPrefix(req.Path, "/")
	}

	thrukURL := d.url + thrukPath
	d.logger.Printf("[Resource] GET thrukUrl: %s", thrukURL)

	httpReq, err := http.NewRequestWithContext(ctx, "GET", thrukURL, nil)
	if err != nil {
		d.logger.Printf("[Resource] failed to create request: %v", err)
		return sender.Send(&backend.CallResourceResponse{
			Status: http.StatusInternalServerError,
			Body:   []byte(fmt.Sprintf("failed to create request: %v", err)),
		})
	}

	for k, v := range extraHeaders {
		httpReq.Header.Set(k, v)
	}

	// switch req.Path {
	// case "columns", "variable-query":
	// 	cachedResult, err := getCachedResult(nil, d.uid, thrukURL, (*map[string][]string)(&httpReq.Header))
	// 	if err != nil {
	// 		d.logger.Printf("[CACHE] error when getting cached result: %s", err.Error())
	// 	}
	// 	if cachedResult != nil {
	// 		d.logger.Printf("[CACHE] using cached result for query %s", thrukURL)
	// 		return *cachedResult.result
	// 	}
	// }

	start := time.Now()
	resp, err := d.httpClient.Do(httpReq)
	elapsed := time.Since(start)
	if err != nil {
		d.logger.Printf("[Resource] request failed after %v: %v", elapsed, err)
		return sender.Send(&backend.CallResourceResponse{
			Status: http.StatusInternalServerError,
			Body:   []byte(fmt.Sprintf("request failed: %v", err)),
		})
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		d.logger.Printf("[Resource] failed to read response: %v", err)
		return sender.Send(&backend.CallResourceResponse{
			Status: http.StatusInternalServerError,
			Body:   []byte(fmt.Sprintf("failed to read response: %v", err)),
		})
	}

	d.logger.Printf("[Resource] response %d (%v, %d bytes)", resp.StatusCode, elapsed, len(body))

	return sender.Send(&backend.CallResourceResponse{
		Status: resp.StatusCode,
		Body:   body,
	})
}
