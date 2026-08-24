package plugin

import (
	"context"
	"fmt"
	"strings"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
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
