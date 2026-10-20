package ahclient_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// apiRecorder answers every request with body and records the method,
// escaped path and query of the last request.
type apiRecorder struct {
	*httptest.Server
	method, path, query, bootstrap string
	body                           string
}

func newAPIRecorder(t *testing.T, respBody string) *apiRecorder {
	rs := &apiRecorder{}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.method = r.Method
		rs.path = r.URL.EscapedPath()
		rs.query = r.URL.RawQuery
		rs.bootstrap = r.Header.Get("X-Bootstrap-Token")
		b, _ := io.ReadAll(r.Body)
		rs.body = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(rs.Close)
	return rs
}

// TestClient_Routes checks every typed endpoint hits the documented method
// and path, so a typo in a route string cannot ship silently.
func TestClient_Routes(t *testing.T) {
	obj := []struct {
		name   string
		call   func(c *ahclient.Client) error
		method string
		path   string
		query  string
	}{
		{"HealthDetailed", func(c *ahclient.Client) error { _, err := c.HealthDetailed(true); return err }, "GET", "/v1/system/health/detailed", "fresh=true"},
		{"GetTenant", func(c *ahclient.Client) error { _, err := c.GetTenant(); return err }, "GET", "/v1/tenant", ""},
		{"UpdateTenant", func(c *ahclient.Client) error {
			n := "x"
			_, err := c.UpdateTenant(ahclient.UpdateTenantRequest{Name: &n})
			return err
		}, "PATCH", "/v1/tenant", ""},
		{"GetTenantUsage", func(c *ahclient.Client) error { _, err := c.GetTenantUsage(); return err }, "GET", "/v1/tenant/usage", ""},
		{"DeleteTenant", func(c *ahclient.Client) error { return c.DeleteTenant() }, "DELETE", "/v1/tenant", ""},
		{"ReactivateTenant", func(c *ahclient.Client) error { _, err := c.ReactivateTenant("t1", "tok"); return err }, "POST", "/v1/tenants/t1/reactivate", ""},
		{"CreateKey", func(c *ahclient.Client) error {
			_, err := c.CreateKey(ahclient.CreateKeyRequest{Name: "k"})
			return err
		}, "POST", "/v1/auth/keys", ""},
		{"RevokeKey", func(c *ahclient.Client) error { return c.RevokeKey("k1") }, "DELETE", "/v1/auth/keys/k1", ""},
		{"RecoverKey", func(c *ahclient.Client) error { _, err := c.RecoverKey("a@b.c", "tok"); return err }, "POST", "/v1/auth/recover", ""},
		{"GetService", func(c *ahclient.Client) error { _, err := c.GetService("s1"); return err }, "GET", "/v1/services/s1", ""},
		{"UpdateService", func(c *ahclient.Client) error { _, err := c.UpdateService("s1", "n"); return err }, "PATCH", "/v1/services/s1", ""},
		{"StartService", func(c *ahclient.Client) error { _, err := c.StartService("s1"); return err }, "POST", "/v1/services/s1/start", ""},
		{"StopService", func(c *ahclient.Client) error { _, err := c.StopService("s1"); return err }, "POST", "/v1/services/s1/stop", ""},
		{"RestartService", func(c *ahclient.Client) error { _, err := c.RestartService("s1"); return err }, "POST", "/v1/services/s1/restart", ""},
		{"RedeployService", func(c *ahclient.Client) error { _, err := c.RedeployService("s1"); return err }, "POST", "/v1/services/s1/redeploy", ""},
		{"ResetService", func(c *ahclient.Client) error { _, err := c.ResetService("s1"); return err }, "POST", "/v1/services/s1/reset", ""},
		{"GetServiceEnv", func(c *ahclient.Client) error { _, err := c.GetServiceEnv("s1", true); return err }, "GET", "/v1/services/s1/env", "reveal=true"},
		{"SetServiceEnv", func(c *ahclient.Client) error {
			_, err := c.SetServiceEnv("s1", map[string]string{"A": "b"})
			return err
		}, "POST", "/v1/services/s1/env", ""},
		{"DeleteServiceEnv", func(c *ahclient.Client) error { return c.DeleteServiceEnv("s1", "FOO") }, "DELETE", "/v1/services/s1/env/FOO", ""},
		{"CancelDeployment", func(c *ahclient.Client) error { _, err := c.CancelDeployment("s1", "d1"); return err }, "POST", "/v1/services/s1/deployments/d1/cancel", ""},
		{"GetSnapshot", func(c *ahclient.Client) error { _, err := c.GetSnapshot("sn1", true); return err }, "GET", "/v1/snapshots/sn1", "reveal=true"},
		{"CreateSnapshot", func(c *ahclient.Client) error {
			_, err := c.CreateSnapshot("s1", ahclient.CreateSnapshotRequest{Name: "x"})
			return err
		}, "POST", "/v1/services/s1/snapshots", ""},
		{"DeleteSnapshot", func(c *ahclient.Client) error { return c.DeleteSnapshot("sn1") }, "DELETE", "/v1/snapshots/sn1", ""},
		{"GetBuild", func(c *ahclient.Client) error { _, err := c.GetBuild("s1", "b1"); return err }, "GET", "/v1/services/s1/builds/b1", ""},
		{"StartBuild", func(c *ahclient.Client) error {
			_, err := c.StartBuild("s1", ahclient.StartBuildRequest{SourceType: "git"})
			return err
		}, "POST", "/v1/services/s1/builds", ""},
		{"CancelBuild", func(c *ahclient.Client) error { _, err := c.CancelBuild("s1", "b1"); return err }, "DELETE", "/v1/services/s1/builds/b1", ""},
		{"GetDatabase", func(c *ahclient.Client) error { _, err := c.GetDatabase("d1"); return err }, "GET", "/v1/databases/d1", ""},
		{"CreateDatabase", func(c *ahclient.Client) error {
			_, err := c.CreateDatabase(ahclient.CreateDatabaseRequest{Name: "x", Type: "redis"})
			return err
		}, "POST", "/v1/databases", ""},
		{"DeleteDatabase", func(c *ahclient.Client) error { return c.DeleteDatabase("d1") }, "DELETE", "/v1/databases/d1", ""},
		{"GetDatabaseConnectionString", func(c *ahclient.Client) error { _, err := c.GetDatabaseConnectionString("d1"); return err }, "GET", "/v1/databases/d1/connection-string", ""},
		{"GetEnvironment", func(c *ahclient.Client) error { _, err := c.GetEnvironment("e1"); return err }, "GET", "/v1/environments/e1", ""},
		{"CreateEnvironment", func(c *ahclient.Client) error {
			_, err := c.CreateEnvironment(ahclient.CreateEnvironmentRequest{Name: "x"})
			return err
		}, "POST", "/v1/environments", ""},
		{"DeleteEnvironment", func(c *ahclient.Client) error { return c.DeleteEnvironment("e1") }, "DELETE", "/v1/environments/e1", ""},
		{"StartEnvironment", func(c *ahclient.Client) error { _, err := c.StartEnvironment("e1"); return err }, "POST", "/v1/environments/e1/start", ""},
		{"StopEnvironment", func(c *ahclient.Client) error { _, err := c.StopEnvironment("e1"); return err }, "POST", "/v1/environments/e1/stop", ""},
		{"ExtendEnvironmentLease", func(c *ahclient.Client) error { _, err := c.ExtendEnvironmentLease("e1", 60); return err }, "POST", "/v1/environments/e1/lease", ""},
		{"GetEnvironmentTemplate", func(c *ahclient.Client) error { _, err := c.GetEnvironmentTemplate("tmpl"); return err }, "GET", "/v1/environments/templates/tmpl", ""},
		{"SyncWorkspace", func(c *ahclient.Client) error {
			_, err := c.SyncWorkspace("e1", ahclient.SyncRequest{GitURL: "https://x"})
			return err
		}, "POST", "/v1/environments/e1/sync", ""},
		{"CreatePreview", func(c *ahclient.Client) error {
			_, err := c.CreatePreview("e1", ahclient.CreatePreviewRequest{Name: "p", Port: 80})
			return err
		}, "POST", "/v1/environments/e1/previews", ""},
		{"DeletePreview", func(c *ahclient.Client) error { return c.DeletePreview("e1", "p1") }, "DELETE", "/v1/environments/e1/previews/p1", ""},
	}
	for _, tc := range obj {
		t.Run(tc.name, func(t *testing.T) {
			rs := newAPIRecorder(t, `{}`)
			require.NoError(t, tc.call(ahclient.NewClient(rs.URL, "k")))
			assert.Equal(t, tc.method, rs.method)
			assert.Equal(t, tc.path, rs.path)
			assert.Equal(t, tc.query, rs.query)
		})
	}

	lists := []struct {
		name string
		call func(c *ahclient.Client) error
		path string
	}{
		{"ListBuilds", func(c *ahclient.Client) error { _, err := c.ListBuilds(); return err }, "/v1/builds"},
		{"ListBuildsForService", func(c *ahclient.Client) error { _, err := c.ListBuildsForService("s1"); return err }, "/v1/services/s1/builds"},
		{"ListDeployments", func(c *ahclient.Client) error { _, err := c.ListDeployments("s1"); return err }, "/v1/services/s1/deployments"},
		{"ListEnvironmentTemplates", func(c *ahclient.Client) error { _, err := c.ListEnvironmentTemplates(); return err }, "/v1/environments/templates"},
		{"ListPreviews", func(c *ahclient.Client) error { _, err := c.ListPreviews("e1"); return err }, "/v1/environments/e1/previews"},
	}
	for _, tc := range lists {
		t.Run(tc.name, func(t *testing.T) {
			rs := newAPIRecorder(t, `[]`)
			require.NoError(t, tc.call(ahclient.NewClient(rs.URL, "k")))
			assert.Equal(t, http.MethodGet, rs.method)
			assert.Equal(t, tc.path, rs.path)
		})
	}
}

func TestClient_ReactivateTenant_SendsBootstrapHeader(t *testing.T) {
	rs := newAPIRecorder(t, `{"tenant_id":"t1","api_key":"k","status":"active"}`)
	resp, err := ahclient.NewClient(rs.URL, "").ReactivateTenant("t1", "tok")
	require.NoError(t, err)
	assert.Equal(t, "tok", rs.bootstrap)
	assert.Equal(t, "active", resp.Status)
}

func TestClient_RecoverKey_SendsBody(t *testing.T) {
	rs := newAPIRecorder(t, `{"id":"k1","key":"ak_x"}`)
	resp, err := ahclient.NewClient(rs.URL, "").RecoverKey("a@b.c", "tok")
	require.NoError(t, err)
	assert.Equal(t, "ak_x", resp.Key)
	assert.JSONEq(t, `{"email":"a@b.c","bootstrap_token":"tok"}`, rs.body)
}

func TestClient_EnvironmentExec_OmitsEmptyOptionalFields(t *testing.T) {
	rs := newAPIRecorder(t, `{"exit_code":0}`)
	c := ahclient.NewClient(rs.URL, "")
	_, err := c.EnvironmentExec("e1", []string{"ls"}, "", 0)
	require.NoError(t, err)
	assert.JSONEq(t, `{"command":["ls"]}`, rs.body)

	_, err = c.EnvironmentExec("e1", []string{"ls"}, "/w", 10)
	require.NoError(t, err)
	assert.JSONEq(t, `{"command":["ls"],"work_dir":"/w","timeout":10}`, rs.body)
}

func TestClient_ListActivity_AllFilters(t *testing.T) {
	rs := newAPIRecorder(t, `[]`)
	_, err := ahclient.NewClient(rs.URL, "").ListActivity(ahclient.ActivityFilter{
		ResourceType: "service", Action: "deploy", ServiceID: "s1", Since: 5, Limit: 10, Offset: 20,
	})
	require.NoError(t, err)
	assert.Equal(t, "action=deploy&limit=10&offset=20&resource_type=service&service_id=s1&since=5", rs.query)
}

func TestClient_ErrorBodyIsCapped(t *testing.T) {
	big := strings.Repeat("x", 1<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, big)
	}))
	defer srv.Close()

	_, err := ahclient.NewClient(srv.URL, "").GetTenant()
	var apiErr *ahclient.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusBadGateway, apiErr.StatusCode)
	assert.Less(t, len(apiErr.Message), len(big))
	assert.Contains(t, apiErr.Error(), "API error 502")
}

func TestClient_StreamErrorReturnsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"no such build"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := ahclient.NewClient(srv.URL, "").GetBuildLogs("s1", "b1", false)
	var apiErr *ahclient.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, "no such build", apiErr.Message)
}

// Streaming requests ignore the client-wide timeout but keep the rest of the
// configured client (here: a custom transport).
func TestClient_StreamKeepsTransportAndIgnoresTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(60 * time.Millisecond)
		_, _ = io.WriteString(w, "line\n")
	}))
	defer srv.Close()

	used := false
	c := ahclient.NewClient(srv.URL, "")
	c.HTTPClient.Timeout = 20 * time.Millisecond
	c.HTTPClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		used = true
		return http.DefaultTransport.RoundTrip(r)
	})

	rc, err := c.GetServiceLogs("s1", true, 10)
	require.NoError(t, err)
	defer rc.Close()
	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "line\n", string(data))
	assert.True(t, used, "configured transport must be used for streams")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNewClientNoTimeout(t *testing.T) {
	c := ahclient.NewClientNoTimeout("http://x/", "k")
	assert.Equal(t, "http://x", c.BaseURL)
	assert.Zero(t, c.HTTPClient.Timeout)
}
