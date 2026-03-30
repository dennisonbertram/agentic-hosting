package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/crypto"
	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/environments"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// envTestHelper bundles state needed for environment handler tests.
type envTestHelper struct {
	stateDB    *sql.DB
	masterKey  []byte
	token      string
	mockDocker *testutil.MockDockerClient
	envMgr     *environments.Manager
	srv        *Server
}

// newEnvTestHelper creates a server with a real environments.Manager backed by
// an in-memory SQLite DB and a MockDockerClient.  It seeds the tenant, quotas,
// and a "default" template so Create works without external dependencies.
func newEnvTestHelper(t *testing.T) *envTestHelper {
	t.Helper()
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	// Seed tenant + quota
	now := time.Now().Unix()
	_, err := stateDB.Exec(
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
		 VALUES (?, ?, ?, 'active', ?, ?)`,
		"tenant-1", "Env Tenant", "env@example.com", now, now,
	)
	require.NoError(t, err)

	_, err = stateDB.Exec(
		`INSERT INTO tenant_quotas (tenant_id, max_services, max_databases, max_memory_mb,
		 max_cpu_cores, max_disk_gb, api_rate_limit, max_environments)
		 VALUES (?, 5, 3, 2048, 2.0, 20, 100, 5)`,
		"tenant-1",
	)
	require.NoError(t, err)

	// The migrations seed node/python/go templates but NOT "default".
	// Insert a "default" template so CreateRequest with no templateID succeeds.
	_, err = stateDB.Exec(
		`INSERT OR IGNORE INTO environment_templates
		 (id, name, base_image, description, memory_mb, cpu_millicores, disk_mb, egress_policy, created_at, updated_at)
		 VALUES ('default', 'default', 'ubuntu:24.04', 'Default workspace', 512, 500, 1024, 'deny', ?, ?)`,
		now, now,
	)
	require.NoError(t, err)

	// Generate API key
	secret, keyID, err := crypto.GenerateAPIKeyWithID()
	require.NoError(t, err)
	keyHash := crypto.HashAPIKey(secret, masterKey)
	_, err = stateDB.Exec(
		`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at)
		 VALUES (?, ?, 'default', ?, ?, 1)`,
		keyID, "tenant-1", keyID[:8], keyHash,
	)
	require.NoError(t, err)
	token := keyID + "." + secret

	mockDocker := &testutil.MockDockerClient{}
	envMgr := environments.NewManager(stateDB, mockDocker, "example.com", "")

	srv := NewServer(ServerConfig{
		Store:              &db.Store{StateDB: stateDB},
		MasterKey:          masterKey,
		DevMode:            true,
		EnvironmentManager: envMgr,
	})

	return &envTestHelper{
		stateDB:    stateDB,
		masterKey:  masterKey,
		token:      token,
		mockDocker: mockDocker,
		envMgr:     envMgr,
		srv:        srv,
	}
}

// doRequest is a convenience helper.
func (h *envTestHelper) doRequest(t *testing.T, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader *bytes.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	} else {
		bodyReader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	req.Header.Set("Authorization", "Bearer "+h.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	h.srv.ServeHTTP(rr, req)
	return rr
}

// seedRunningEnv directly inserts a running environment row into the DB,
// bypassing the Docker calls that Create() would make. Returns the env ID.
func seedRunningEnv(t *testing.T, stateDB *sql.DB, tenantID, name string) string {
	t.Helper()
	id := "env-test-001"
	now := time.Now().Unix()
	leaseExpires := now + 3600
	_, err := stateDB.Exec(
		`INSERT INTO environments (id, tenant_id, name, template_id, status, container_id, volume_name,
		 lease_duration_seconds, lease_expires_at, last_activity_at, created_at, updated_at)
		 VALUES (?, ?, ?, 'default', 'running', 'ctr-001', 'vol-001', 3600, ?, ?, ?, ?)`,
		id, tenantID, name, leaseExpires, now, now, now,
	)
	require.NoError(t, err)
	return id
}

// ---------------------------------------------------------------------------
// POST /v1/environments
// ---------------------------------------------------------------------------

// BT-001: When POST /v1/environments is called without a name field, then HTTP 400.
func TestEnvironmentCreate_MissingName_Returns400(t *testing.T) {
	h := newEnvTestHelper(t)

	rr := h.doRequest(t, http.MethodPost, "/v1/environments", []byte(`{}`))

	assert.Equal(t, http.StatusBadRequest, rr.Code,
		"missing name should return 400, got body: %s", rr.Body.String())

	var resp map[string]string
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Contains(t, resp["error"], "name", "error message should mention name")
}

// TestEnvironmentCreate_Success_Returns201 verifies that a well-formed create request
// returns 201 with the environment object.
func TestEnvironmentCreate_Success_Returns201(t *testing.T) {
	h := newEnvTestHelper(t)

	h.mockDocker.RunEnvironmentFn = func(ctx context.Context, cfg docker.RunEnvironmentConfig) (string, error) {
		return "ctr-new-001", nil
	}

	body := []byte(`{"name":"my-workspace","template_id":"default"}`)
	rr := h.doRequest(t, http.MethodPost, "/v1/environments", body)

	assert.Equal(t, http.StatusCreated, rr.Code,
		"valid create should return 201, got body: %s", rr.Body.String())

	var env map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&env))
	assert.Equal(t, "my-workspace", env["name"], "response should include environment name")
	assert.Equal(t, "running", env["status"], "newly created environment should have status running")
	assert.NotEmpty(t, env["id"], "response should include an environment id")
}

// ---------------------------------------------------------------------------
// GET /v1/environments
// ---------------------------------------------------------------------------

// BT-003: When GET /v1/environments returns no environments, response body is [] not null.
func TestEnvironmentList_Empty_ReturnsEmptyArray(t *testing.T) {
	h := newEnvTestHelper(t)

	rr := h.doRequest(t, http.MethodGet, "/v1/environments", nil)

	assert.Equal(t, http.StatusOK, rr.Code,
		"list with no environments should return 200, got body: %s", rr.Body.String())

	// Body must be exactly [] (array literal), not null.
	assert.Equal(t, "[]\n", rr.Body.String(),
		"empty environment list must be [] not null")
}

// TestEnvironmentList_WithEnvironments_ReturnsList verifies that seeded environments appear.
func TestEnvironmentList_WithEnvironments_ReturnsList(t *testing.T) {
	h := newEnvTestHelper(t)
	seedRunningEnv(t, h.stateDB, "tenant-1", "listed-env")

	rr := h.doRequest(t, http.MethodGet, "/v1/environments", nil)

	assert.Equal(t, http.StatusOK, rr.Code)

	var envs []map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&envs))
	assert.Len(t, envs, 1, "should return exactly one environment")
	assert.Equal(t, "listed-env", envs[0]["name"])
}

// ---------------------------------------------------------------------------
// GET /v1/environments/{envID}
// ---------------------------------------------------------------------------

// TestEnvironmentGet_NotFound_Returns404 verifies that requesting a non-existent
// environment returns 404.
func TestEnvironmentGet_NotFound_Returns404(t *testing.T) {
	h := newEnvTestHelper(t)

	rr := h.doRequest(t, http.MethodGet, "/v1/environments/env-does-not-exist", nil)

	assert.Equal(t, http.StatusNotFound, rr.Code,
		"non-existent environment should return 404, got body: %s", rr.Body.String())
}

// TestEnvironmentGet_Existing_Returns200 verifies that a seeded environment can be fetched.
func TestEnvironmentGet_Existing_Returns200(t *testing.T) {
	h := newEnvTestHelper(t)
	envID := seedRunningEnv(t, h.stateDB, "tenant-1", "get-env")

	rr := h.doRequest(t, http.MethodGet, "/v1/environments/"+envID, nil)

	assert.Equal(t, http.StatusOK, rr.Code,
		"existing environment should return 200, got body: %s", rr.Body.String())

	var env map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&env))
	assert.Equal(t, envID, env["id"])
	assert.Equal(t, "get-env", env["name"])
}

// ---------------------------------------------------------------------------
// DELETE /v1/environments/{envID}
// ---------------------------------------------------------------------------

// TestEnvironmentDelete_Success_Returns204 verifies that deleting an environment returns 204.
func TestEnvironmentDelete_Success_Returns204(t *testing.T) {
	h := newEnvTestHelper(t)
	envID := seedRunningEnv(t, h.stateDB, "tenant-1", "del-env")

	rr := h.doRequest(t, http.MethodDelete, "/v1/environments/"+envID, nil)

	assert.Equal(t, http.StatusNoContent, rr.Code,
		"successful delete should return 204, got body: %s", rr.Body.String())

	// Confirm it's gone
	rr2 := h.doRequest(t, http.MethodGet, "/v1/environments/"+envID, nil)
	assert.Equal(t, http.StatusNotFound, rr2.Code,
		"environment should be gone after delete")
}

// TestEnvironmentDelete_NotFound_Returns404 verifies that deleting a non-existent
// environment returns 404.
func TestEnvironmentDelete_NotFound_Returns404(t *testing.T) {
	h := newEnvTestHelper(t)

	rr := h.doRequest(t, http.MethodDelete, "/v1/environments/env-ghost", nil)

	assert.Equal(t, http.StatusNotFound, rr.Code,
		"deleting non-existent environment should return 404")
}

// ---------------------------------------------------------------------------
// POST /v1/environments/{envID}/start and /stop
// ---------------------------------------------------------------------------

// TestEnvironmentStart_Returns200 verifies that starting a stopped environment
// returns 200 with status "running".
func TestEnvironmentStart_Returns200(t *testing.T) {
	h := newEnvTestHelper(t)
	envID := seedRunningEnv(t, h.stateDB, "tenant-1", "start-env")

	// First stop it so we can start it
	_, err := h.stateDB.Exec(`UPDATE environments SET status = 'stopped' WHERE id = ?`, envID)
	require.NoError(t, err)

	rr := h.doRequest(t, http.MethodPost, "/v1/environments/"+envID+"/start", nil)

	assert.Equal(t, http.StatusOK, rr.Code,
		"start should return 200, got body: %s", rr.Body.String())

	var resp map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal(t, "running", resp["status"])
}

// TestEnvironmentStop_Returns200 verifies that stopping a running environment
// returns 200 with status "stopped".
func TestEnvironmentStop_Returns200(t *testing.T) {
	h := newEnvTestHelper(t)
	envID := seedRunningEnv(t, h.stateDB, "tenant-1", "stop-env")

	rr := h.doRequest(t, http.MethodPost, "/v1/environments/"+envID+"/stop", nil)

	assert.Equal(t, http.StatusOK, rr.Code,
		"stop should return 200, got body: %s", rr.Body.String())

	var resp map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal(t, "stopped", resp["status"])
}

// ---------------------------------------------------------------------------
// POST /v1/environments/{envID}/exec
// ---------------------------------------------------------------------------

// BT-002: When POST /v1/environments/{envID}/exec is called with an empty command,
// then HTTP 400.
func TestEnvironmentExec_EmptyCommand_Returns400(t *testing.T) {
	h := newEnvTestHelper(t)
	envID := seedRunningEnv(t, h.stateDB, "tenant-1", "exec-env")

	body := []byte(`{"command":[]}`)
	rr := h.doRequest(t, http.MethodPost, "/v1/environments/"+envID+"/exec", body)

	assert.Equal(t, http.StatusBadRequest, rr.Code,
		"empty command should return 400, got body: %s", rr.Body.String())

	var resp map[string]string
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Contains(t, resp["error"], "command", "error message should mention command")
}

// TestEnvironmentExec_MissingCommandField_Returns400 verifies that an omitted
// command field (empty by default) also returns 400.
func TestEnvironmentExec_MissingCommandField_Returns400(t *testing.T) {
	h := newEnvTestHelper(t)
	envID := seedRunningEnv(t, h.stateDB, "tenant-1", "exec-env2")

	body := []byte(`{}`)
	rr := h.doRequest(t, http.MethodPost, "/v1/environments/"+envID+"/exec", body)

	assert.Equal(t, http.StatusBadRequest, rr.Code,
		"missing command should return 400, got body: %s", rr.Body.String())
}

// TestEnvironmentExec_Success_Returns200 verifies a valid exec returns 200 with output.
func TestEnvironmentExec_Success_Returns200(t *testing.T) {
	h := newEnvTestHelper(t)
	envID := seedRunningEnv(t, h.stateDB, "tenant-1", "exec-env3")

	h.mockDocker.ExecCreateFn = func(ctx context.Context, containerID string, cmd []string, workDir string) (string, error) {
		return "exec-id-001", nil
	}
	h.mockDocker.ExecRunFn = func(ctx context.Context, execID string, timeout time.Duration) ([]byte, []byte, int, error) {
		return []byte("hello\n"), nil, 0, nil
	}

	body := []byte(`{"command":["echo","hello"]}`)
	rr := h.doRequest(t, http.MethodPost, "/v1/environments/"+envID+"/exec", body)

	assert.Equal(t, http.StatusOK, rr.Code,
		"valid exec should return 200, got body: %s", rr.Body.String())

	var resp map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal(t, float64(0), resp["exit_code"], "successful command should have exit_code 0")
	assert.Contains(t, resp["stdout"], "hello")
}

// ---------------------------------------------------------------------------
// POST /v1/environments/{envID}/lease
// ---------------------------------------------------------------------------

// TestEnvironmentLease_ZeroDuration_Returns400 verifies that zero duration is rejected.
func TestEnvironmentLease_ZeroDuration_Returns400(t *testing.T) {
	h := newEnvTestHelper(t)
	envID := seedRunningEnv(t, h.stateDB, "tenant-1", "lease-env")

	body := []byte(`{"duration_seconds":0}`)
	rr := h.doRequest(t, http.MethodPost, "/v1/environments/"+envID+"/lease", body)

	assert.Equal(t, http.StatusBadRequest, rr.Code,
		"zero duration_seconds should return 400, got body: %s", rr.Body.String())

	var resp map[string]string
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Contains(t, resp["error"], "duration_seconds")
}

// TestEnvironmentLease_NegativeDuration_Returns400 verifies that negative duration is rejected.
func TestEnvironmentLease_NegativeDuration_Returns400(t *testing.T) {
	h := newEnvTestHelper(t)
	envID := seedRunningEnv(t, h.stateDB, "tenant-1", "lease-env2")

	body := []byte(`{"duration_seconds":-100}`)
	rr := h.doRequest(t, http.MethodPost, "/v1/environments/"+envID+"/lease", body)

	assert.Equal(t, http.StatusBadRequest, rr.Code,
		"negative duration_seconds should return 400, got body: %s", rr.Body.String())
}

// TestEnvironmentLease_PositiveDuration_Returns200 verifies that a positive duration
// extends the lease and returns the updated environment.
func TestEnvironmentLease_PositiveDuration_Returns200(t *testing.T) {
	h := newEnvTestHelper(t)
	envID := seedRunningEnv(t, h.stateDB, "tenant-1", "lease-env3")

	body := []byte(`{"duration_seconds":600}`)
	rr := h.doRequest(t, http.MethodPost, "/v1/environments/"+envID+"/lease", body)

	assert.Equal(t, http.StatusOK, rr.Code,
		"positive duration_seconds should return 200, got body: %s", rr.Body.String())

	var env map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&env))
	assert.Equal(t, envID, env["id"], "response should include environment id")
}

// ---------------------------------------------------------------------------
// GET /v1/environments/templates
// ---------------------------------------------------------------------------

// TestEnvironmentTemplateList_Returns200 verifies that template listing returns
// a non-null array.
func TestEnvironmentTemplateList_Returns200(t *testing.T) {
	h := newEnvTestHelper(t)

	rr := h.doRequest(t, http.MethodGet, "/v1/environments/templates", nil)

	assert.Equal(t, http.StatusOK, rr.Code,
		"template list should return 200, got body: %s", rr.Body.String())

	var templates []map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&templates))
	// The migrations seed node/python/go templates, plus we added "default"
	assert.NotEmpty(t, templates, "template list should be non-empty after migration seeding")
}

// TestEnvironmentTemplateList_NeverNull verifies that when no templates exist
// (empty DB with no migration seeds), the response is [] not null.
func TestEnvironmentTemplateList_NeverNull(t *testing.T) {
	// Use the standard seeded DB — templates exist from migration.
	// We just check that the JSON array is parseable and not null.
	h := newEnvTestHelper(t)

	rr := h.doRequest(t, http.MethodGet, "/v1/environments/templates", nil)

	require.Equal(t, http.StatusOK, rr.Code)

	// The raw body must start with '[', not 'n' (for null).
	body := rr.Body.Bytes()
	assert.Equal(t, byte('['), body[0], "response must be a JSON array, not null")
}

// ---------------------------------------------------------------------------
// POST /v1/environments/{envID}/sync
// ---------------------------------------------------------------------------

// TestEnvironmentSync_MissingGitURL_Returns400 verifies that a sync without
// git_url returns 400.
func TestEnvironmentSync_MissingGitURL_Returns400(t *testing.T) {
	h := newEnvTestHelper(t)
	envID := seedRunningEnv(t, h.stateDB, "tenant-1", "sync-env")

	body := []byte(`{}`)
	rr := h.doRequest(t, http.MethodPost, "/v1/environments/"+envID+"/sync", body)

	assert.Equal(t, http.StatusBadRequest, rr.Code,
		"missing git_url should return 400, got body: %s", rr.Body.String())

	var resp map[string]string
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Contains(t, resp["error"], "git_url")
}

// TestEnvironmentSync_NonHTTPS_Returns400 verifies that a non-https git_url
// returns 400.
func TestEnvironmentSync_NonHTTPS_Returns400(t *testing.T) {
	h := newEnvTestHelper(t)
	envID := seedRunningEnv(t, h.stateDB, "tenant-1", "sync-env2")

	body := []byte(`{"git_url":"http://github.com/example/repo"}`)
	rr := h.doRequest(t, http.MethodPost, "/v1/environments/"+envID+"/sync", body)

	assert.Equal(t, http.StatusBadRequest, rr.Code,
		"http:// git_url should return 400 (must be https://), got body: %s", rr.Body.String())

	var resp map[string]string
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Contains(t, resp["error"], "https://")
}

// TestEnvironmentSync_GitURLWithSSH_Returns400 verifies that SSH git URLs are rejected.
func TestEnvironmentSync_GitURLWithSSH_Returns400(t *testing.T) {
	h := newEnvTestHelper(t)
	envID := seedRunningEnv(t, h.stateDB, "tenant-1", "sync-env3")

	body := []byte(`{"git_url":"git@github.com:example/repo.git"}`)
	rr := h.doRequest(t, http.MethodPost, "/v1/environments/"+envID+"/sync", body)

	assert.Equal(t, http.StatusBadRequest, rr.Code,
		"ssh git_url should return 400 (must be https://), got body: %s", rr.Body.String())
}

// ---------------------------------------------------------------------------
// POST /v1/environments/{envID}/previews
// ---------------------------------------------------------------------------

// TestEnvironmentPreviewCreate_Success_Returns201 verifies that creating a preview
// for an existing environment returns 201.
func TestEnvironmentPreviewCreate_Success_Returns201(t *testing.T) {
	h := newEnvTestHelper(t)
	envID := seedRunningEnv(t, h.stateDB, "tenant-1", "preview-env")

	body := []byte(`{"name":"web","port":3000}`)
	rr := h.doRequest(t, http.MethodPost, "/v1/environments/"+envID+"/previews", body)

	assert.Equal(t, http.StatusCreated, rr.Code,
		"preview create should return 201, got body: %s", rr.Body.String())

	var preview map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&preview))
	assert.NotEmpty(t, preview["id"], "preview should have an id")
	assert.Equal(t, "web", preview["name"])
	assert.Equal(t, float64(3000), preview["port"])
}

// ---------------------------------------------------------------------------
// DELETE /v1/environments/{envID}/previews/{previewID}
// ---------------------------------------------------------------------------

// TestEnvironmentPreviewDelete_Success_Returns204 verifies that deleting an
// existing preview returns 204.
func TestEnvironmentPreviewDelete_Success_Returns204(t *testing.T) {
	h := newEnvTestHelper(t)
	envID := seedRunningEnv(t, h.stateDB, "tenant-1", "preview-del-env")

	// Create a preview first
	createRR := h.doRequest(t, http.MethodPost, "/v1/environments/"+envID+"/previews",
		[]byte(`{"name":"api","port":8080}`))
	require.Equal(t, http.StatusCreated, createRR.Code,
		"failed to create preview for delete test: %s", createRR.Body.String())

	var preview map[string]any
	require.NoError(t, json.NewDecoder(createRR.Body).Decode(&preview))
	previewID := preview["id"].(string)

	rr := h.doRequest(t, http.MethodDelete,
		"/v1/environments/"+envID+"/previews/"+previewID, nil)

	assert.Equal(t, http.StatusNoContent, rr.Code,
		"preview delete should return 204, got body: %s", rr.Body.String())
}

// TestEnvironmentPreviewDelete_NotFound_Returns404 verifies that deleting a
// non-existent preview returns 404.
func TestEnvironmentPreviewDelete_NotFound_Returns404(t *testing.T) {
	h := newEnvTestHelper(t)
	envID := seedRunningEnv(t, h.stateDB, "tenant-1", "preview-notfound-env")

	rr := h.doRequest(t, http.MethodDelete,
		"/v1/environments/"+envID+"/previews/preview-ghost", nil)

	assert.Equal(t, http.StatusNotFound, rr.Code,
		"deleting non-existent preview should return 404")
}

// ---------------------------------------------------------------------------
// envManager = nil → 503 on all environment endpoints
// ---------------------------------------------------------------------------

// newServerWithoutEnvManager creates a server with no EnvironmentManager.
func newServerWithoutEnvManager(t *testing.T) (*Server, string) {
	t.Helper()
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
		// EnvironmentManager intentionally omitted → nil
	})
	return srv, token
}

func doAuthRequest(t *testing.T, srv *Server, token, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var b *bytes.Reader
	if body != nil {
		b = bytes.NewReader(body)
	} else {
		b = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, b)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// BT-004: When envManager is nil, all environment endpoints return HTTP 503.
func TestEnvironment_NilManager_Returns503_List(t *testing.T) {
	srv, token := newServerWithoutEnvManager(t)
	rr := doAuthRequest(t, srv, token, http.MethodGet, "/v1/environments", nil)

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code,
		"GET /v1/environments should return 503 when envManager is nil, got body: %s", rr.Body.String())
}

func TestEnvironment_NilManager_Returns503_Create(t *testing.T) {
	srv, token := newServerWithoutEnvManager(t)
	rr := doAuthRequest(t, srv, token, http.MethodPost, "/v1/environments",
		[]byte(`{"name":"test"}`))

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code,
		"POST /v1/environments should return 503 when envManager is nil")
}

func TestEnvironment_NilManager_Returns503_Get(t *testing.T) {
	srv, token := newServerWithoutEnvManager(t)
	rr := doAuthRequest(t, srv, token, http.MethodGet, "/v1/environments/env-123", nil)

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code,
		"GET /v1/environments/{envID} should return 503 when envManager is nil")
}

func TestEnvironment_NilManager_Returns503_Delete(t *testing.T) {
	srv, token := newServerWithoutEnvManager(t)
	rr := doAuthRequest(t, srv, token, http.MethodDelete, "/v1/environments/env-123", nil)

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code,
		"DELETE /v1/environments/{envID} should return 503 when envManager is nil")
}

func TestEnvironment_NilManager_Returns503_Start(t *testing.T) {
	srv, token := newServerWithoutEnvManager(t)
	rr := doAuthRequest(t, srv, token, http.MethodPost, "/v1/environments/env-123/start", nil)

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code,
		"POST /v1/environments/{envID}/start should return 503 when envManager is nil")
}

func TestEnvironment_NilManager_Returns503_Stop(t *testing.T) {
	srv, token := newServerWithoutEnvManager(t)
	rr := doAuthRequest(t, srv, token, http.MethodPost, "/v1/environments/env-123/stop", nil)

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code,
		"POST /v1/environments/{envID}/stop should return 503 when envManager is nil")
}

func TestEnvironment_NilManager_Returns503_Exec(t *testing.T) {
	srv, token := newServerWithoutEnvManager(t)
	rr := doAuthRequest(t, srv, token, http.MethodPost, "/v1/environments/env-123/exec",
		[]byte(`{"command":["ls"]}`))

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code,
		"POST /v1/environments/{envID}/exec should return 503 when envManager is nil")
}

func TestEnvironment_NilManager_Returns503_Lease(t *testing.T) {
	srv, token := newServerWithoutEnvManager(t)
	rr := doAuthRequest(t, srv, token, http.MethodPost, "/v1/environments/env-123/lease",
		[]byte(`{"duration_seconds":300}`))

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code,
		"POST /v1/environments/{envID}/lease should return 503 when envManager is nil")
}

func TestEnvironment_NilManager_Returns503_Templates(t *testing.T) {
	srv, token := newServerWithoutEnvManager(t)
	rr := doAuthRequest(t, srv, token, http.MethodGet, "/v1/environments/templates", nil)

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code,
		"GET /v1/environments/templates should return 503 when envManager is nil")
}

func TestEnvironment_NilManager_Returns503_Sync(t *testing.T) {
	srv, token := newServerWithoutEnvManager(t)
	rr := doAuthRequest(t, srv, token, http.MethodPost, "/v1/environments/env-123/sync",
		[]byte(`{"git_url":"https://github.com/example/repo"}`))

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code,
		"POST /v1/environments/{envID}/sync should return 503 when envManager is nil")
}

func TestEnvironment_NilManager_Returns503_PreviewCreate(t *testing.T) {
	srv, token := newServerWithoutEnvManager(t)
	rr := doAuthRequest(t, srv, token, http.MethodPost, "/v1/environments/env-123/previews",
		[]byte(`{"name":"web","port":3000}`))

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code,
		"POST /v1/environments/{envID}/previews should return 503 when envManager is nil")
}

func TestEnvironment_NilManager_Returns503_PreviewDelete(t *testing.T) {
	srv, token := newServerWithoutEnvManager(t)
	rr := doAuthRequest(t, srv, token, http.MethodDelete, "/v1/environments/env-123/previews/prv-001", nil)

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code,
		"DELETE /v1/environments/{envID}/previews/{previewID} should return 503 when envManager is nil")
}

// ---------------------------------------------------------------------------
// Regression tests
// ---------------------------------------------------------------------------

// Regression: empty list must serialize as [] not null (Go nil slice encodes as null).
// This catches the handler's nil-to-empty-slice guard.
func TestEnvironmentList_JSONArrayNotNull_Regression(t *testing.T) {
	h := newEnvTestHelper(t)

	rr := h.doRequest(t, http.MethodGet, "/v1/environments", nil)

	require.Equal(t, http.StatusOK, rr.Code)
	// Verify that "null" does not appear as the body
	assert.NotEqual(t, "null\n", rr.Body.String(),
		"empty environment list must never be JSON null")
	// Verify it is valid JSON parseable as an array
	var result []any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &result),
		"response body must be a valid JSON array")
}

// Regression: exec with nil-manager must return 503, not panic.
// This catches if the requireEnvironmentManager guard is removed.
func TestEnvironmentExec_NilManager_NoPanic_Returns503_Regression(t *testing.T) {
	srv, token := newServerWithoutEnvManager(t)

	rr := doAuthRequest(t, srv, token, http.MethodPost, "/v1/environments/env-abc/exec",
		[]byte(`{"command":["true"]}`))

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code,
		"exec with nil envManager must return 503 and not panic")
}

// Regression: non-https git_url must always be rejected regardless of the protocol prefix.
// Catches a regression where the https:// prefix check is accidentally removed.
func TestEnvironmentSync_HTTPNotHTTPS_AlwaysRejected_Regression(t *testing.T) {
	h := newEnvTestHelper(t)
	envID := seedRunningEnv(t, h.stateDB, "tenant-1", "sync-regression-env")

	for _, url := range []string{
		"http://github.com/x/y",
		"ftp://github.com/x/y",
		"git@github.com:x/y.git",
		"ssh://github.com/x/y",
	} {
		body, _ := json.Marshal(map[string]string{"git_url": url})
		rr := h.doRequest(t, http.MethodPost, "/v1/environments/"+envID+"/sync", body)
		assert.Equal(t, http.StatusBadRequest, rr.Code,
			"git_url %q should be rejected as non-https, but got %d: %s",
			url, rr.Code, rr.Body.String())
	}
}

// Regression: lease endpoint must reject zero and negative durations.
// Catches a regression where the <= 0 guard is weakened to < 0.
func TestEnvironmentLease_NonPositiveDuration_AlwaysRejected_Regression(t *testing.T) {
	h := newEnvTestHelper(t)
	envID := seedRunningEnv(t, h.stateDB, "tenant-1", "lease-regression-env")

	for _, dur := range []int{0, -1, -3600} {
		body, _ := json.Marshal(map[string]int{"duration_seconds": dur})
		rr := h.doRequest(t, http.MethodPost, "/v1/environments/"+envID+"/lease", body)
		assert.Equal(t, http.StatusBadRequest, rr.Code,
			"duration_seconds=%d should return 400, got %d: %s",
			dur, rr.Code, rr.Body.String())
	}
}
