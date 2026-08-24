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
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/httpclient"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
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
		res := query(ctx, d, q, meta)
		response.Responses[q.RefID] = res
	}

	//responseJSON, _ := response.DeepCopy().MarshalJSON()
	//d.logger.Printf("[QueryData] response:\n%v", string(responseJSON))

	return response, nil
}

// This function is to be implemented accoring to the SDK interface
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
			Body:   fmt.Appendf([]byte{}, "failed to create request: %v", err),
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
			Body:   fmt.Appendf([]byte{}, "request failed: %v", err),
		})
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		d.logger.Printf("[Resource] failed to read response: %v", err)
		return sender.Send(&backend.CallResourceResponse{
			Status: http.StatusInternalServerError,
			Body:   fmt.Appendf([]byte{}, "failed to read response: %v", err),
		})
	}

	d.logger.Printf("[Resource] response %d (%v, %d bytes)", resp.StatusCode, elapsed, len(body))

	return sender.Send(&backend.CallResourceResponse{
		Status: resp.StatusCode,
		Body:   body,
	})
}
