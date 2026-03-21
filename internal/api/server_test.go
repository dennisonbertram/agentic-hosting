package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/crypto"
	"github.com/dennisonbertram/agentic-hosting/internal/databases"
	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/kanbans"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeDatabaseManager struct {
	createCalls         int
	stopAllForTenantCalls []string
	createFn            func(ctx context.Context, tenantID string, req databases.CreateRequest) (*databases.Database, error)
}

func (f *fakeDatabaseManager) Create(ctx context.Context, tenantID string, req databases.CreateRequest) (*databases.Database, error) {
	f.createCalls++
	if f.createFn != nil {
		return f.createFn(ctx, tenantID, req)
	}
	return &databases.Database{
		ID:       "db-1",
		TenantID: tenantID,
		Name:     req.Name,
		Type:     req.Type,
		Status:   "ready",
	}, nil
}

func (f *fakeDatabaseManager) List(ctx context.Context, tenantID string) ([]*databases.Database, error) {
	return nil, nil
}

func (f *fakeDatabaseManager) ListPaginated(ctx context.Context, tenantID string, limit, offset int) ([]*databases.Database, error) {
	return nil, nil
}

func (f *fakeDatabaseManager) Get(ctx context.Context, tenantID, dbID string) (*databases.Database, error) {
	return nil, nil
}

func (f *fakeDatabaseManager) GetConnectionString(ctx context.Context, tenantID, dbID string) (string, error) {
	return "", nil
}

func (f *fakeDatabaseManager) Delete(ctx context.Context, tenantID, dbID string) error {
	return nil
}

func (f *fakeDatabaseManager) StopAllForTenant(ctx context.Context, tenantID string) {
	f.stopAllForTenantCalls = append(f.stopAllForTenantCalls, tenantID)
}

type fakeKanbanManager struct {
	stopForTenantCalls []string
}

func (f *fakeKanbanManager) Create(ctx context.Context, tenantID string) (*kanbans.Kanban, error) {
	return nil, nil
}

func (f *fakeKanbanManager) Get(ctx context.Context, tenantID string) (*kanbans.Kanban, error) {
	return nil, nil
}

func (f *fakeKanbanManager) GetAdminToken(ctx context.Context, tenantID string) (string, error) {
	return "", nil
}

func (f *fakeKanbanManager) Delete(ctx context.Context, tenantID string) error {
	return nil
}

func (f *fakeKanbanManager) StopForTenant(ctx context.Context, tenantID string) {
	f.stopForTenantCalls = append(f.stopForTenantCalls, tenantID)
}

func TestDatabaseCreate_UsesIdempotencyMiddleware(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	dbMgr := &fakeDatabaseManager{}
	srv := NewServer(ServerConfig{
		Store:           &db.Store{StateDB: stateDB},
		MasterKey:       masterKey,
		DevMode:         true,
		DatabaseManager: dbMgr,
	})

	body := []byte(`{"name":"main-db","type":"postgres"}`)
	first := httptest.NewRecorder()
	firstReq := httptest.NewRequest(http.MethodPost, "/v1/databases", bytes.NewReader(body))
	firstReq.Header.Set("Authorization", "Bearer "+token)
	firstReq.Header.Set("Content-Type", "application/json")
	firstReq.Header.Set("Idempotency-Key", "db-create-1")
	srv.ServeHTTP(first, firstReq)

	second := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodPost, "/v1/databases", bytes.NewReader(body))
	secondReq.Header.Set("Authorization", "Bearer "+token)
	secondReq.Header.Set("Content-Type", "application/json")
	secondReq.Header.Set("Idempotency-Key", "db-create-1")
	srv.ServeHTTP(second, secondReq)

	require.Equal(t, http.StatusCreated, first.Code)
	require.Equal(t, http.StatusCreated, second.Code)
	assert.Equal(t, 1, dbMgr.createCalls, "database creation should be replayed, not executed twice")
	assert.Equal(t, "true", second.Header().Get("Idempotency-Replayed"))
	assert.JSONEq(t, first.Body.String(), second.Body.String())
}

func TestDatabaseCreate_LongRunningRouteHasNoTimeoutDeadline(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	sawDeadline := false
	dbMgr := &fakeDatabaseManager{
		createFn: func(ctx context.Context, tenantID string, req databases.CreateRequest) (*databases.Database, error) {
			_, sawDeadline = ctx.Deadline()
			return &databases.Database{
				ID:       "db-1",
				TenantID: tenantID,
				Name:     req.Name,
				Type:     req.Type,
				Status:   "ready",
			}, nil
		},
	}
	srv := NewServer(ServerConfig{
		Store:           &db.Store{StateDB: stateDB},
		MasterKey:       masterKey,
		DevMode:         true,
		DatabaseManager: dbMgr,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/databases", bytes.NewBufferString(`{"name":"main-db","type":"postgres"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusCreated, rr.Code)
	assert.False(t, sawDeadline, "long-running database create route should not inherit the short request timeout")
}

func TestServiceLogsRoute_IsRegisteredAndHasNoTimeoutDeadline(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	_, err := stateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, container_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-1", "tenant-1", "web", "running", "nginx:latest", 8080, "ctr-1", 1, 1,
	)
	require.NoError(t, err)

	sawDeadline := false
	dockerClient := &testutil.MockDockerClient{
		LogsContainerFn: func(ctx context.Context, containerID string, follow bool, tail int) (io.ReadCloser, error) {
			_, sawDeadline = ctx.Deadline()
			return io.NopCloser(strings.NewReader("hello from logs\n")), nil
		},
	}
	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
		Docker:    dockerClient,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/services/svc-1/logs?follow=true&tail=50", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	assert.False(t, sawDeadline, "streaming service logs should not inherit the short request timeout")
	assert.Equal(t, "hello from logs\n", rr.Body.String())
	assert.Equal(t, []string{"ctr-1"}, dockerClient.LogsContainerCalls)
}

func TestTypedErrorRouting(t *testing.T) {
	// Typed errors are now handled by apierr.WriteAPIError, no string matching needed.
	// This test verifies the apierr package is correctly integrated (tested in apierr_test.go).
}

// TestTenantDelete_CleansUpAllManagers verifies that DELETE /v1/tenant triggers
// StopAllForTenant on the database manager and StopForTenant on the kanban
// manager in addition to stopping services. This is the fix for GitHub issue #55:
// previously only services were stopped on tenant suspension, leaving database and
// kanban containers running and consuming resources.
func TestTenantDelete_CleansUpAllManagers(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	dbMgr := &fakeDatabaseManager{}
	kanbanMgr := &fakeKanbanManager{}

	srv := NewServer(ServerConfig{
		Store:           &db.Store{StateDB: stateDB},
		MasterKey:       masterKey,
		DevMode:         true,
		DatabaseManager: dbMgr,
		KanbanManager:   kanbanMgr,
	})

	req := httptest.NewRequest(http.MethodDelete, "/v1/tenant", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusNoContent, rr.Code)

	assert.Equal(t, []string{"tenant-1"}, dbMgr.stopAllForTenantCalls,
		"database manager StopAllForTenant should be called with the tenant ID on deletion")
	assert.Equal(t, []string{"tenant-1"}, kanbanMgr.stopForTenantCalls,
		"kanban manager StopForTenant should be called with the tenant ID on deletion")
}

// TestServiceRedeploy_CallsRestartAndReturnsService verifies that POST /v1/services/{id}/redeploy
// invokes the Restart path (stop + recreate container) and returns the updated service object.
func TestServiceRedeploy_CallsRestartAndReturnsService(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	// Insert a running service with a container so Restart() can proceed.
	_, err := stateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, container_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-redeploy", "tenant-1", "web", "running", "nginx:latest", 8080, "ctr-old", 1000, 1000,
	)
	require.NoError(t, err)

	runCalls := 0
	realMock := &testutil.MockDockerClient{
		RunContainerFn: func(ctx context.Context, tenantID, serviceID, img string, port int, envVars map[string]string, extraLabels map[string]string, limits *docker.ResourceLimits) (string, error) {
			runCalls++
			return "ctr-new", nil
		},
	}

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
		Docker:    realMock,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/services/svc-redeploy/redeploy", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "unexpected body: %s", rr.Body.String())

	// Response must be a service object (has an "id" field).
	var body map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&body))
	assert.Equal(t, "svc-redeploy", body["id"], "response should include service id")
	assert.Equal(t, 1, runCalls, "RunContainer should have been called once for the redeploy")
}

// TestServiceRedeploy_NoContainer_Returns409 verifies that redeploying a service
// with no container returns a 409 Conflict (same as restart with no container).
func TestServiceRedeploy_NoContainer_Returns409(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	// Insert a service with no container_id (never deployed).
	_, err := stateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, container_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-nodeploy", "tenant-1", "web", "stopped", "nginx:latest", 8080, "", 1000, 1000,
	)
	require.NoError(t, err)

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
		Docker:    &testutil.MockDockerClient{},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/services/svc-nodeploy/redeploy", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusConflict, rr.Code, "redeploy with no container should return 409")
}

// TestServiceDeployments_ReturnsLastDeployRecord verifies that GET /v1/services/{id}/deployments
// returns a non-empty array with the current service state as the last deploy record.
func TestServiceDeployments_ReturnsLastDeployRecord(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	_, err := stateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, container_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-hist", "tenant-1", "web", "running", "nginx:latest", 8080, "ctr-abc", 1000, 2000,
	)
	require.NoError(t, err)

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
		Docker:    &testutil.MockDockerClient{},
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/services/svc-hist/deployments", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "unexpected body: %s", rr.Body.String())

	var records []map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&records))
	require.Len(t, records, 1, "expected exactly one deployment record")

	rec := records[0]
	assert.Equal(t, "svc-hist", rec["service_id"])
	assert.Equal(t, "running", rec["status"])
	assert.Equal(t, "nginx:latest", rec["image"])
	assert.EqualValues(t, 2000, rec["started_at"])
}

// TestServiceDeployments_NotFound_Returns404 verifies 404 for an unknown service.
func TestServiceDeployments_NotFound_Returns404(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
		Docker:    &testutil.MockDockerClient{},
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/services/does-not-exist/deployments", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code)
}

// TestKeyCreate_QuotaExceeded_Returns409 verifies that creating an API key
// when the tenant has reached maxKeysPerTenant returns 409 Conflict (not 403).
// This is part of the fix for issue #100: standardize quota exceeded errors.
func TestKeyCreate_QuotaExceeded_Returns409(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	// Fill up all key slots (seedAuthenticatedTenant already created 1 key)
	for i := 1; i < maxKeysPerTenant; i++ {
		_, err := stateDB.Exec(
			`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at)
			 VALUES (?, ?, ?, ?, ?, 1)`,
			"fill-key-"+strconv.Itoa(i), "tenant-1", "fill", "fill"+strconv.Itoa(i)+"xx", "hash",
		)
		require.NoError(t, err)
	}

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
	})

	body := []byte(`{"name":"one-too-many"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/keys", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusConflict, rr.Code, "API key quota exceeded should return 409 Conflict")
	assert.Contains(t, rr.Body.String(), "quota exceeded")
}

// TestTenantRegister_MaxTenants_Returns409 verifies that registering a new tenant
// when the maximum is reached returns 409 Conflict (not 403).
func TestTenantRegister_MaxTenants_Returns409(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	// Insert maxTenants active tenants to fill the cap
	for i := 0; i < maxTenants; i++ {
		id := "tenant-fill-" + strconv.Itoa(i)
		_, err := stateDB.Exec(
			`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
			 VALUES (?, ?, ?, 'active', 1, 1)`,
			id, "T"+strconv.Itoa(i), "t"+strconv.Itoa(i)+"@example.com",
		)
		require.NoError(t, err)
	}

	srv := NewServer(ServerConfig{
		Store:            &db.Store{StateDB: stateDB},
		MasterKey:        masterKey,
		DevMode:          true,
		OpenRegistration: true, // skip bootstrap token for test simplicity
	})

	body := []byte(`{"name":"New Tenant","email":"new@example.com"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/tenants/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Use a unique IP to avoid rate limiter collisions with other tests
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Real-Ip", "10.99.99.1")

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusConflict, rr.Code, "max tenants reached should return 409 Conflict")
	assert.Contains(t, rr.Body.String(), "quota exceeded")
}

func seedAuthenticatedTenant(t *testing.T, stateDB *sql.DB, masterKey []byte) string {
	t.Helper()
	_, err := stateDB.Exec(
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at) VALUES (?, ?, ?, 'active', 1, 1)`,
		"tenant-1", "Tenant", "tenant@example.com",
	)
	require.NoError(t, err)
	_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`, "tenant-1")
	require.NoError(t, err)

	secret, keyID, err := crypto.GenerateAPIKeyWithID()
	require.NoError(t, err)
	keyHash := crypto.HashAPIKey(secret, masterKey)
	_, err = stateDB.Exec(
		`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at) VALUES (?, ?, 'default', ?, ?, 1)`,
		keyID, "tenant-1", keyID[:8], keyHash,
	)
	require.NoError(t, err)

	return keyID + "." + secret
}
