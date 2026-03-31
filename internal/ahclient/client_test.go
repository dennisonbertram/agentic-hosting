package ahclient_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- Config tests ----

// BT-001: Config.Load() returns error when nothing is configured
func TestConfigLoad_NothingConfigured(t *testing.T) {
	// Clear env and use a temp home dir so no config file exists
	t.Setenv("AH_URL", "")
	t.Setenv("AH_KEY", "")

	cfg, err := ahclient.LoadConfig(ahclient.LoadOptions{
		ConfigPath: filepath.Join(t.TempDir(), "nonexistent", "config.json"),
	})
	assert.Nil(t, cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ahc configure")
}

// BT-002: Config.Load() returns env vars when set
func TestConfigLoad_FromEnvVars(t *testing.T) {
	t.Setenv("AH_URL", "https://example.com")
	t.Setenv("AH_KEY", "testkey123")

	cfg, err := ahclient.LoadConfig(ahclient.LoadOptions{
		ConfigPath: filepath.Join(t.TempDir(), "nonexistent", "config.json"),
	})
	require.NoError(t, err)
	assert.Equal(t, "https://example.com", cfg.URL)
	assert.Equal(t, "testkey123", cfg.Key)
}

// BT-003: Config.Load() reads from file when no env vars
func TestConfigLoad_FromFile(t *testing.T) {
	t.Setenv("AH_URL", "")
	t.Setenv("AH_KEY", "")

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	data, _ := json.Marshal(ahclient.Config{URL: "https://from-file.com", Key: "filekey"})
	require.NoError(t, os.WriteFile(cfgPath, data, 0600))

	cfg, err := ahclient.LoadConfig(ahclient.LoadOptions{ConfigPath: cfgPath})
	require.NoError(t, err)
	assert.Equal(t, "https://from-file.com", cfg.URL)
	assert.Equal(t, "filekey", cfg.Key)
}

// BT-004: Env vars take priority over file
func TestConfigLoad_EnvOverridesFile(t *testing.T) {
	t.Setenv("AH_URL", "https://from-env.com")
	t.Setenv("AH_KEY", "envkey")

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	data, _ := json.Marshal(ahclient.Config{URL: "https://from-file.com", Key: "filekey"})
	require.NoError(t, os.WriteFile(cfgPath, data, 0600))

	cfg, err := ahclient.LoadConfig(ahclient.LoadOptions{ConfigPath: cfgPath})
	require.NoError(t, err)
	assert.Equal(t, "https://from-env.com", cfg.URL)
	assert.Equal(t, "envkey", cfg.Key)
}

// BT-005: Config.Save() writes to file with 0600 permissions
func TestConfigSave(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	cfg := &ahclient.Config{URL: "https://saved.com", Key: "savedkey"}
	require.NoError(t, cfg.Save(cfgPath))

	info, err := os.Stat(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())

	loaded, err := ahclient.LoadConfig(ahclient.LoadOptions{ConfigPath: cfgPath})
	require.NoError(t, err)
	assert.Equal(t, cfg.URL, loaded.URL)
	assert.Equal(t, cfg.Key, loaded.Key)
}

// ---- Client HTTP tests ----

// BT-006: GET requests set Authorization: Bearer header
func TestClient_AuthHeaderSet(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "mytoken")
	_, err := c.Health()
	require.NoError(t, err)
	assert.Equal(t, "Bearer mytoken", gotHeader)
}

// BT-007: POST requests set Content-Type: application/json
func TestClient_ContentTypeOnPost(t *testing.T) {
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"tenant_id": "t1", "api_key": "k1"})
	}))
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "")
	_, err := c.Register("Alice", "alice@example.com", "bootstrap-token")
	require.NoError(t, err)
	assert.Equal(t, "application/json", gotContentType)
}

// BT-008: Non-2xx responses return APIError with status code and message
func TestClient_Non2xxReturnsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "service not found"})
	}))
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "mykey")
	_, err := c.GetService("svc-123")
	require.Error(t, err)

	apiErr, ok := err.(*ahclient.APIError)
	require.True(t, ok, "expected *ahclient.APIError, got %T", err)
	assert.Equal(t, 404, apiErr.StatusCode)
	assert.Contains(t, apiErr.Message, "service not found")
}

// BT-009: Successful GET /v1/system/health returns HealthResponse
func TestClient_Health(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/system/health", r.URL.Path)
		assert.Equal(t, http.MethodGet, r.Method)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "key")
	resp, err := c.Health()
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Status)
}

// BT-010: Successful GET /v1/services returns []Service
func TestClient_ListServices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/services", r.URL.Path)
		assert.Equal(t, http.MethodGet, r.Method)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode([]map[string]any{
			{"id": "svc-1", "name": "myservice", "status": "running", "image": "nginx:latest", "port": 80, "crash_count": 0, "circuit_open": false, "created_at": 1000, "updated_at": 1001, "tenant_id": "t1"},
		})
	}))
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "key")
	svcs, err := c.ListServices()
	require.NoError(t, err)
	require.Len(t, svcs, 1)
	assert.Equal(t, "svc-1", svcs[0].ID)
	assert.Equal(t, "myservice", svcs[0].Name)
	assert.Equal(t, "running", svcs[0].Status)
}

// BT-011: Successful POST /v1/services returns Service
func TestClient_CreateService(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/services", r.URL.Path)
		assert.Equal(t, http.MethodPost, r.Method)

		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "mysvc", body["name"])

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"id": "new-svc", "name": "mysvc", "status": "deploying",
			"image": "nginx:latest", "port": 80, "crash_count": 0,
			"circuit_open": false, "created_at": 1000, "updated_at": 1000, "tenant_id": "t1",
		})
	}))
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "key")
	svc, err := c.CreateService(ahclient.CreateServiceRequest{Name: "mysvc", Image: "nginx:latest", Port: 80})
	require.NoError(t, err)
	assert.Equal(t, "new-svc", svc.ID)
	assert.Equal(t, "deploying", svc.Status)
}

// BT-012: DELETE /v1/services/{id} returns nil error on 204
func TestClient_DeleteService(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/services/svc-del", r.URL.Path)
		assert.Equal(t, http.MethodDelete, r.Method)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "key")
	err := c.DeleteService("svc-del")
	require.NoError(t, err)
}

// BT-013: GetServiceLogs returns an io.ReadCloser
func TestClient_GetServiceLogs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/services/svc-1/logs", r.URL.Path)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("log line 1\nlog line 2\n"))
	}))
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "key")
	rc, err := c.GetServiceLogs("svc-1", false, 100)
	require.NoError(t, err)
	require.NotNil(t, rc)
	rc.Close()
}

// BT-014: Environment list returns []Environment
func TestClient_ListEnvironments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/environments", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode([]map[string]any{
			{"id": "env-1", "name": "myenv", "status": "running", "tenant_id": "t1", "template_id": "tmpl-1", "lease_duration_seconds": 3600, "created_at": 1000, "updated_at": 1001},
		})
	}))
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "key")
	envs, err := c.ListEnvironments()
	require.NoError(t, err)
	require.Len(t, envs, 1)
	assert.Equal(t, "env-1", envs[0].ID)
	assert.Equal(t, "myenv", envs[0].Name)
}

// BT-015: Exec returns ExecResult
func TestClient_EnvironmentExec(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/environments/env-1/exec", r.URL.Path)
		assert.Equal(t, http.MethodPost, r.Method)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"exit_code": 0, "stdout": "hello\n", "stderr": "", "truncated": false, "timed_out": false, "duration_ms": 50,
		})
	}))
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "key")
	res, err := c.EnvironmentExec("env-1", []string{"echo", "hello"}, "", 30)
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitCode)
	assert.Equal(t, "hello\n", res.Stdout)
}

// BT-016: POST /v1/tenants/register sends X-Bootstrap-Token header
func TestClient_Register_SendsBootstrapToken(t *testing.T) {
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Bootstrap-Token")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"tenant_id": "t1", "api_key": "k1"})
	}))
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "")
	_, err := c.Register("Bob", "bob@example.com", "my-bootstrap-tok")
	require.NoError(t, err)
	assert.Equal(t, "my-bootstrap-tok", gotToken)
}

// BT-017: Database list returns []Database
func TestClient_ListDatabases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/databases", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode([]map[string]any{
			{"id": "db-1", "name": "mydb", "type": "postgres", "status": "ready", "tenant_id": "t1", "created_at": 1000, "updated_at": 1001},
		})
	}))
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "key")
	dbs, err := c.ListDatabases()
	require.NoError(t, err)
	require.Len(t, dbs, 1)
	assert.Equal(t, "db-1", dbs[0].ID)
	assert.Equal(t, "postgres", dbs[0].Type)
}

// BT-018: Build logs streaming returns io.ReadCloser
func TestClient_GetBuildLogs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/services/svc-1/builds/bld-1/logs", r.URL.Path)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("build step 1\nbuild step 2\n"))
	}))
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "key")
	rc, err := c.GetBuildLogs("svc-1", "bld-1", false)
	require.NoError(t, err)
	require.NotNil(t, rc)
	rc.Close()
}

// BT-019: API error message is parsed from response body {"error":"..."}
func TestClient_APIError_MessageParsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(map[string]string{"error": "name must be at least 2 characters"})
	}))
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "key")
	_, err := c.CreateService(ahclient.CreateServiceRequest{Name: "x", Image: "nginx:latest"})
	require.Error(t, err)
	apiErr, ok := err.(*ahclient.APIError)
	require.True(t, ok)
	assert.Equal(t, 422, apiErr.StatusCode)
	assert.Equal(t, "name must be at least 2 characters", apiErr.Message)
}

// BT-020: Snapshot list returns []Snapshot
func TestClient_ListSnapshots(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/snapshots", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode([]map[string]any{
			{"id": "snap-1", "name": "my-snap", "tenant_id": "t1", "service_id": "svc-1", "image_ref": "127.0.0.1:5000/svc/img:tag", "port": 8080, "created_at": 2000},
		})
	}))
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "key")
	snaps, err := c.ListSnapshots()
	require.NoError(t, err)
	require.Len(t, snaps, 1)
	assert.Equal(t, "snap-1", snaps[0].ID)
}

// BT-021: Activity list returns []ActivityEvent
func TestClient_ListActivity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/activity", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode([]map[string]any{
			{"id": "evt-1", "resource_type": "service", "resource_id": "svc-1", "action": "service.created", "message": "Service created", "created_at": 1000},
		})
	}))
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "key")
	events, err := c.ListActivity(ahclient.ActivityFilter{})
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "evt-1", events[0].ID)
	assert.Equal(t, "service.created", events[0].Action)
}

// BT-022: Key list returns []APIKey
func TestClient_ListKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/auth/keys", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode([]map[string]any{
			{"id": "key-1", "name": "default", "prefix": "abcd1234", "created_at": 1000},
		})
	}))
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "key")
	keys, err := c.ListKeys()
	require.NoError(t, err)
	require.Len(t, keys, 1)
	assert.Equal(t, "key-1", keys[0].ID)
}

// BT-023: GetBuildLogs passes follow=true as query param
func TestClient_GetBuildLogs_FollowQueryParam(t *testing.T) {
	var gotFollow string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotFollow = r.URL.Query().Get("follow")
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("streaming\n"))
	}))
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "key")
	rc, err := c.GetBuildLogs("svc-1", "bld-1", true)
	require.NoError(t, err)
	rc.Close()
	assert.Equal(t, "true", gotFollow)
}

// BT-024: GetServiceLogs passes tail and follow query params
func TestClient_GetServiceLogs_QueryParams(t *testing.T) {
	var gotTail, gotFollow string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTail = r.URL.Query().Get("tail")
		gotFollow = r.URL.Query().Get("follow")
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("log\n"))
	}))
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "key")
	rc, err := c.GetServiceLogs("svc-1", true, 50)
	require.NoError(t, err)
	rc.Close()
	assert.Equal(t, "50", gotTail)
	assert.Equal(t, "true", gotFollow)
}

// BT-025: Regression: APIError implements the error interface
func TestAPIError_ImplementsError(t *testing.T) {
	var err error = &ahclient.APIError{StatusCode: 404, Message: "not found"}
	assert.Contains(t, err.Error(), "404")
	assert.Contains(t, err.Error(), "not found")
}
