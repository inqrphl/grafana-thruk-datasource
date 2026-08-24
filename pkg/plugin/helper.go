package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/httpclient"
	"github.com/grafana/grafana-plugin-sdk-go/data"
)

// authHeadersToScopeCache are the forwarded headers relevant for identifying the authenticated user/request.
// They are used both for logging and to scope the response cache so that one user's Thruk data is never served to another.
var authHeadersToScopeCache = []string{"Cookie", "Authorization", "X-Id-Token", "X-Grafana-User"}

// cookieNames extracts the cookie names from the raw values of a Cookie header.
// It never returns the cookie values themselves, so it is safe to log.
func cookieNames(cookieHeaderValues []string) []string {
	var names []string
	for _, value := range cookieHeaderValues {
		for _, part := range strings.Split(value, ";") {
			name := strings.TrimSpace(part)
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				name = name[:eq]
			}
			if name != "" {
				names = append(names, name)
			}
		}
	}
	return names
}

// buildAuthHeaders picks the auth-relevant headers out of the headers forwarded by Grafana. Returns nil when there is no auth context.
func buildAuthHeaders(headers http.Header) map[string][]string {
	var authHeaders map[string][]string
	for _, name := range authHeadersToScopeCache {
		if values, ok := headers[name]; ok {
			if authHeaders == nil {
				authHeaders = make(map[string][]string)
			}
			authHeaders[name] = values
		}
	}
	return authHeaders
}

func getQueryParam(rawURL string, key string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Query().Get(key)
}

// works on wrapped_json calls where metadata is present, in such calls it looks for resp.Meta.Columns
// or normal json calls where everything is on the same object, in such calls it looks for first row
// docs/r-v1-hosts-response.json and docs/r1-v1-thruk-response.json
func determineColumnsFromThrukResponse(resp *ThrukWrappedJsonResponse) []string {
	if resp.Meta != nil && len(resp.Meta.Columns) > 0 {
		cols := make([]string, 0, len(resp.Meta.Columns))
		for _, c := range resp.Meta.Columns {
			cols = append(cols, c.Name)
		}
		return cols
	}
	if len(resp.Data) > 0 {
		cols := make([]string, 0, len(resp.Data[0]))
		for key := range resp.Data[0] {
			cols = append(cols, key)
		}
		return cols
	}
	return nil
}

// builds a map from columnMetadata.Name -> columnMetadata
// useful for fast lookups directly from name
func buildColumnMetadataMap(resp *ThrukWrappedJsonResponse) map[string]ThrukWrappedJsonResponseMetaColumn {
	m := make(map[string]ThrukWrappedJsonResponseMetaColumn)
	if resp.Meta != nil {
		for _, c := range resp.Meta.Columns {
			m[c.Name] = c
		}
	}
	return m
}

func parseVisualizationType(typeVal any) string {
	if s, ok := typeVal.(string); ok {
		if s == "timeseries" {
			return "graph"
		}
		return s
	}
	if obj, ok := typeVal.(map[string]any); ok {
		if v, ok := obj["value"].(string); ok {
			if v == "timeseries" {
				return "graph"
			}
			return v
		}
	}
	return "table"
}

// if we know the table used in query model, we can iterate through the columns and add their backend types by hand
// this is a band-aid fix, only use it if thruk does not report column type metadata incorrectly.
func overrideKnownGrafanaDataTypes(qm *queryModel, meta *ThrukWrappedJsonResponseMeta) {

	findAndChangeType := func(meta *ThrukWrappedJsonResponseMeta, name string, t data.FieldType) {
		for i := range meta.Columns {
			if meta.Columns[i].Name == name {
				meta.Columns[i].GrafanaDataType = t
			}
		}
	}

	switch qm.Table {
	case "example-non-existent-table":
		findAndChangeType(meta, "example-field", data.FieldTypeInt64)
	}
}

// Applies some default http Client settings and modifies defaults of backend.DatasourceInstanceSettings.HTTPClientOpts
func httpclientOptionsSetDefaults(opts *httpclient.Options) {
	// Always forward the headers, this is how 'thruk_auth' cookies should be passed
	opts.ForwardHTTPHeaders = true

	// Modify some of the timeouts
	opts.Timeouts = &httpclient.TimeoutOptions{
		Timeout:               30 * time.Second,
		DialTimeout:           10 * time.Second,
		KeepAlive:             httpclient.DefaultTimeoutOptions.KeepAlive,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: httpclient.DefaultTimeoutOptions.ExpectContinueTimeout,
		MaxConnsPerHost:       httpclient.DefaultTimeoutOptions.MaxConnsPerHost,
		MaxIdleConns:          httpclient.DefaultTimeoutOptions.MaxIdleConns,
		MaxIdleConnsPerHost:   httpclient.DefaultTimeoutOptions.MaxIdleConnsPerHost,
		IdleConnTimeout:       httpclient.DefaultTimeoutOptions.IdleConnTimeout,
	}
}

// String returns a string representation of the DataSourceInstanceSettings.
func DataSourceInstanceSettingsToString(s *backend.DataSourceInstanceSettings) string {
	var jsonDataBytes []byte
	jsonDataStr := "nil"
	if s.JSONData != nil {
		jsonDataBytes = []byte(s.JSONData)
		// Pretty-print the JSON
		var prettyJSON bytes.Buffer
		if err := json.Indent(&prettyJSON, jsonDataBytes, "", "  "); err == nil {
			jsonDataStr = "\n" + prettyJSON.String()
		} else {
			jsonDataStr = "\n" + string(jsonDataBytes)
		}
	}

	var decryptedDataStr string
	if s.DecryptedSecureJSONData != nil {
		// log only the keys, never the secret values
		keys := make([]string, 0, len(s.DecryptedSecureJSONData))
		for k := range s.DecryptedSecureJSONData {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		decryptedDataStr = "\n" + strings.Join(keys, "\n")
	} else {
		decryptedDataStr = "nil"
	}

	return fmt.Sprintf(`DataSourceInstanceSettings{
  ID: %d
  UID: %s
  Type: %s
  Name: %s
  URL: %s
  User: %s
  Database: %s
  BasicAuthEnabled: %t
  BasicAuthUser: %s
  JSONData: %s
  DecryptedSecureJSONData: %s
  Updated: %s
  APIVersion: %s
}`,
		s.ID,
		s.UID,
		s.Type,
		s.Name,
		s.URL,
		s.User,
		s.Database,
		s.BasicAuthEnabled,
		s.BasicAuthUser,
		jsonDataStr,
		decryptedDataStr,
		s.Updated.Format(time.RFC3339),
		s.APIVersion,
	)
}

func FieldTypeToString(ft data.FieldType) string {
	switch ft {
	case data.FieldTypeUnknown:
		return "FieldTypeUnknown"
	case data.FieldTypeInt8:
		return "FieldTypeInt8"
	case data.FieldTypeNullableInt8:
		return "FieldTypeNullableInt8"
	case data.FieldTypeInt16:
		return "FieldTypeInt16"
	case data.FieldTypeNullableInt16:
		return "FieldTypeNullableInt16"
	case data.FieldTypeInt32:
		return "FieldTypeInt32"
	case data.FieldTypeNullableInt32:
		return "FieldTypeNullableInt32"
	case data.FieldTypeInt64:
		return "FieldTypeInt64"
	case data.FieldTypeNullableInt64:
		return "FieldTypeNullableInt64"
	case data.FieldTypeUint8:
		return "FieldTypeUint8"
	case data.FieldTypeNullableUint8:
		return "FieldTypeNullableUint8"
	case data.FieldTypeUint16:
		return "FieldTypeUint16"
	case data.FieldTypeNullableUint16:
		return "FieldTypeNullableUint16"
	case data.FieldTypeUint32:
		return "FieldTypeUint32"
	case data.FieldTypeNullableUint32:
		return "FieldTypeNullableUint32"
	case data.FieldTypeUint64:
		return "FieldTypeUint64"
	case data.FieldTypeNullableUint64:
		return "FieldTypeNullableUint64"
	case data.FieldTypeFloat32:
		return "FieldTypeFloat32"
	case data.FieldTypeNullableFloat32:
		return "FieldTypeNullableFloat32"
	case data.FieldTypeFloat64:
		return "FieldTypeFloat64"
	case data.FieldTypeNullableFloat64:
		return "FieldTypeNullableFloat64"
	case data.FieldTypeString:
		return "FieldTypeString"
	case data.FieldTypeNullableString:
		return "FieldTypeNullableString"
	case data.FieldTypeBool:
		return "FieldTypeBool"
	case data.FieldTypeNullableBool:
		return "FieldTypeNullableBool"
	case data.FieldTypeTime:
		return "FieldTypeTime"
	case data.FieldTypeNullableTime:
		return "FieldTypeNullableTime"
	case data.FieldTypeJSON:
		return "FieldTypeJSON"
	case data.FieldTypeNullableJSON:
		return "FieldTypeNullableJSON"
	case data.FieldTypeEnum:
		return "FieldTypeEnum"
	case data.FieldTypeNullableEnum:
		return "FieldTypeNullableEnum"
	default:
		return "FieldTypeUnknown"
	}
}

func HTTPClientOptionsToString(opts httpclient.Options) string {
	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf("HTTPClientOptions{ForwardHTTPHeaders:%t", opts.ForwardHTTPHeaders))
	if opts.Timeouts != nil {
		buf.WriteString(fmt.Sprintf(", Timeout:%s", opts.Timeouts.Timeout))
	}
	if opts.TLS != nil {
		buf.WriteString(fmt.Sprintf(", TLS.InsecureSkipVerify:%t", opts.TLS.InsecureSkipVerify))
	}
	if opts.BasicAuth != nil {
		buf.WriteString(fmt.Sprintf(", BasicAuth.User:%s", opts.BasicAuth.User))
	}
	if len(opts.Header) > 0 {
		// log only the header names, never the values (they may contain secrets)
		keys := make([]string, 0, len(opts.Header))
		for key := range opts.Header {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		buf.WriteString(", Headers:[" + strings.Join(keys, ", ") + "]")
	}
	if len(opts.Labels) > 0 {
		buf.WriteString(", Labels:[")
		for key, values := range opts.Labels {
			buf.WriteString(fmt.Sprintf("%s=%v, ", key, values))
		}
		buf.WriteString("]")
	}
	buf.WriteByte('}')
	return buf.String()
}
