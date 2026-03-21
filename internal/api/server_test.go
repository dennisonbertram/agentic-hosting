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

	"github.com/dennisonbertram/agentic-hosting/internal/builds"
	"github.com/dennisonbertram/agentic-hosting/internal/crypto"
	"github.com/dennisonbertram/agentic-hosting/internal/databases"
	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/deployments"
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

// TestTenantDelete_CancelsActiveBuilds verifies that DELETE /v1/tenant cancels
// all pending and running builds for the tenant, while leaving completed builds
// unaffected. This is the fix for GitHub issue #88.
func TestTenantDelete_CancelsActiveBuilds(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	// Insert a service first
	_, err := stateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, created_at, updated_at)
		 VALUES ('svc-1', 'tenant-1', 'web', 'running', 'nginx:latest', 8080, 10, 20)`,
	)
	require.NoError(t, err)

	// Create build manager before inserting builds to avoid reconcileStaleBuilds
	// marking pending/running builds as failed on startup
	buildMgr := builds.NewManager(stateDB, apiStubBuilder{}, nil)

	srv := NewServer(ServerConfig{
		Store:        &db.Store{StateDB: stateDB},
		MasterKey:    masterKey,
		DevMode:      true,
		BuildManager: buildMgr,
	})

	// Insert builds in various states AFTER manager creation
	_, err = stateDB.Exec(
		`INSERT INTO builds (id, service_id, tenant_id, status, source_type, source_url, source_ref, image, created_at)
		 VALUES ('b-pending', 'svc-1', 'tenant-1', 'pending', 'git', 'https://github.com/example/repo', 'main', 'img:1', 10)`,
	)
	require.NoError(t, err)
	_, err = stateDB.Exec(
		`INSERT INTO builds (id, service_id, tenant_id, status, source_type, source_url, source_ref, image, created_at, started_at)
		 VALUES ('b-running', 'svc-1', 'tenant-1', 'running', 'git', 'https://github.com/example/repo', 'main', 'img:2', 10, 11)`,
	)
	require.NoError(t, err)
	_, err = stateDB.Exec(
		`INSERT INTO builds (id, service_id, tenant_id, status, source_type, source_url, source_ref, image, created_at, started_at, finished_at)
		 VALUES ('b-done', 'svc-1', 'tenant-1', 'succeeded', 'git', 'https://github.com/example/repo', 'main', 'img:3', 5, 6, 9)`,
	)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodDelete, "/v1/tenant", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusNoContent, rr.Code)

	// Active builds should be cancelled
	var status string
	err = stateDB.QueryRow(`SELECT status FROM builds WHERE id = 'b-pending'`).Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, "cancelled", status, "pending build should be cancelled on tenant suspension")

	err = stateDB.QueryRow(`SELECT status FROM builds WHERE id = 'b-running'`).Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, "cancelled", status, "running build should be cancelled on tenant suspension")

	// Completed build should be unaffected
	err = stateDB.QueryRow(`SELECT status FROM builds WHERE id = 'b-done'`).Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, "succeeded", status, "completed build should not be affected by tenant suspension")
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

// TestServiceDeployments_ReturnsPaginatedHistory verifies that GET /v1/services/{id}/deployments
// returns deployment records from the real deployments table with pagination.
func TestServiceDeployments_ReturnsPaginatedHistory(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	_, err := stateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, container_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-hist", "tenant-1", "web", "running", "nginx:latest", 8080, "ctr-abc", 1000, 2000,
	)
	require.NoError(t, err)

	deployStore := deployments.NewStore(stateDB)

	// Insert two deployment records.
	require.NoError(t, deployStore.Create(context.Background(), &deployments.Deployment{
		ID:        "deploy-1",
		ServiceID: "svc-hist",
		TenantID:  "tenant-1",
		Image:     "nginx:1.0",
		Status:    deployments.StatusFailed,
		Trigger:   deployments.TriggerManual,
		StartedAt: 1000,
		CreatedAt: 1000,
	}))
	require.NoError(t, deployStore.Create(context.Background(), &deployments.Deployment{
		ID:        "deploy-2",
		ServiceID: "svc-hist",
		TenantID:  "tenant-1",
		Image:     "nginx:latest",
		Status:    deployments.StatusRunning,
		Trigger:   deployments.TriggerRestart,
		StartedAt: 2000,
		CreatedAt: 2000,
	}))

	srv := NewServer(ServerConfig{
		Store:           &db.Store{StateDB: stateDB},
		MasterKey:       masterKey,
		DevMode:         true,
		Docker:          &testutil.MockDockerClient{},
		DeploymentStore: deployStore,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/services/svc-hist/deployments", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "unexpected body: %s", rr.Body.String())

	var records []map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&records))
	require.Len(t, records, 2, "expected two deployment records")

	// Newest first.
	assert.Equal(t, "deploy-2", records[0]["id"])
	assert.Equal(t, "svc-hist", records[0]["service_id"])
	assert.Equal(t, "running", records[0]["status"])
	assert.Equal(t, "restart", records[0]["trigger"])
	assert.Equal(t, "nginx:latest", records[0]["image"])

	assert.Equal(t, "deploy-1", records[1]["id"])
	assert.Equal(t, "failed", records[1]["status"])
}

// TestServiceDeployments_EmptyHistory verifies an empty array is returned when
// a service has no deployment records yet.
func TestServiceDeployments_EmptyHistory(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	_, err := stateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, container_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-empty", "tenant-1", "web", "running", "nginx:latest", 8080, "ctr-abc", 1000, 2000,
	)
	require.NoError(t, err)

	deployStore := deployments.NewStore(stateDB)

	srv := NewServer(ServerConfig{
		Store:           &db.Store{StateDB: stateDB},
		MasterKey:       masterKey,
		DevMode:         true,
		Docker:          &testutil.MockDockerClient{},
		DeploymentStore: deployStore,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/services/svc-empty/deployments", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "unexpected body: %s", rr.Body.String())

	var records []map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&records))
	assert.Len(t, records, 0, "expected empty deployment records for service with no history")
}

// TestServiceDeployments_NotFound_Returns404 verifies 404 for an unknown service.
func TestServiceDeployments_NotFound_Returns404(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	deployStore := deployments.NewStore(stateDB)

	srv := NewServer(ServerConfig{
		Store:           &db.Store{StateDB: stateDB},
		MasterKey:       masterKey,
		DevMode:         true,
		Docker:          &testutil.MockDockerClient{},
		DeploymentStore: deployStore,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/services/does-not-exist/deployments", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code)
}

// --- parsePagination tests ---

func TestParsePagination_DefaultValues(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/test", nil)
	limit, offset, err := parsePagination(r)
	require.NoError(t, err)
	assert.Equal(t, 100, limit, "default limit should be 100")
	assert.Equal(t, 0, offset, "default offset should be 0")
}

func TestParsePagination_ExplicitValues(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/test?limit=50&offset=10", nil)
	limit, offset, err := parsePagination(r)
	require.NoError(t, err)
	assert.Equal(t, 50, limit)
	assert.Equal(t, 10, offset)
}

func TestParsePagination_LimitAt200(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/test?limit=200", nil)
	limit, _, err := parsePagination(r)
	require.NoError(t, err)
	assert.Equal(t, 200, limit, "limit=200 should be accepted as-is")
}

func TestParsePagination_LimitAbove200_CapsAt200(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/test?limit=500", nil)
	limit, _, err := parsePagination(r)
	require.NoError(t, err)
	assert.Equal(t, 200, limit, "limit > 200 should be capped at 200, not rejected or defaulted to 100")
}

func TestParsePagination_LimitAt1(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/test?limit=1", nil)
	limit, _, err := parsePagination(r)
	require.NoError(t, err)
	assert.Equal(t, 1, limit, "limit=1 should be accepted")
}

func TestParsePagination_LimitZero_ReturnsError(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/test?limit=0", nil)
	_, _, err := parsePagination(r)
	assert.Error(t, err, "limit=0 should be rejected")
}

func TestParsePagination_NegativeLimit_ReturnsError(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/test?limit=-5", nil)
	_, _, err := parsePagination(r)
	assert.Error(t, err, "negative limit should be rejected")
}

func TestParsePagination_InvalidLimit_ReturnsError(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/test?limit=abc", nil)
	_, _, err := parsePagination(r)
	assert.Error(t, err, "non-numeric limit should be rejected")
}

func TestParsePagination_NegativeOffset_ReturnsError(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/test?offset=-1", nil)
	_, _, err := parsePagination(r)
	assert.Error(t, err, "negative offset should be rejected")
}

// TestCreateService_PortValidation verifies port validation on service creation:
// - Invalid ports (negative, >65535) return 400
// - Valid ports (1-65535) are accepted
// - Omitted port (zero value) defaults to 8000
func TestCreateService_PortValidation(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
		Docker:    &testutil.MockDockerClient{},
	})

	// --- Invalid ports: should return 400 ---
	invalidCases := []struct {
		name string
		port int
	}{
		{"negative port", -1},
		{"large negative port", -100},
		{"port above max", 70000},
		{"port way above max", 99999},
		{"port 65536", 65536},
	}

	for _, tc := range invalidCases {
		t.Run("invalid/"+tc.name, func(t *testing.T) {
			body := []byte(`{"name":"web","image":"nginx:latest","port":` + strconv.Itoa(tc.port) + `}`)
			req := httptest.NewRequest(http.MethodPost, "/v1/services", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")

			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)

			assert.Equal(t, http.StatusBadRequest, rr.Code, "port %d should be rejected", tc.port)
			assert.Contains(t, rr.Body.String(), "port must be between 1 and 65535")
		})
	}

	// --- Valid ports: should return 201 ---
	validCases := []struct {
		name    string
		svcName string
		port    int
	}{
		{"port 3000", "web-p3000", 3000},
		{"port 8080", "web-p8080", 8080},
		{"port 65535", "web-p65535", 65535},
		{"port 1", "web-p1", 1},
	}

	for _, tc := range validCases {
		t.Run("valid/"+tc.name, func(t *testing.T) {
			body := []byte(`{"name":"` + tc.svcName + `","image":"nginx:latest","port":` + strconv.Itoa(tc.port) + `}`)
			req := httptest.NewRequest(http.MethodPost, "/v1/services", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")

			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)

			assert.Equal(t, http.StatusCreated, rr.Code, "port %d should be accepted, got body: %s", tc.port, rr.Body.String())

			var resp map[string]any
			require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
			assert.Equal(t, float64(tc.port), resp["port"], "response should reflect the requested port")
		})
	}

	// --- Omitted port: should default to 8000 ---
	t.Run("default/omitted port defaults to 8000", func(t *testing.T) {
		body := []byte(`{"name":"web-default","image":"nginx:latest"}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/services", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusCreated, rr.Code, "omitted port should be accepted, got body: %s", rr.Body.String())

		var resp map[string]any
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
		assert.Equal(t, float64(8000), resp["port"], "omitted port should default to 8000")
	})
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

// TestPatchService_UpdatesName verifies that PATCH /v1/services/{id} renames a service
// and returns the updated service object with the new name.
func TestPatchService_UpdatesName(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	_, err := stateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, dns_label, status, image, port, container_id, url, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-rename", "tenant-1", "old-name", "", "running", "nginx:latest", 8080, "ctr-1", "", 1000, 1000,
	)
	require.NoError(t, err)

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
		Docker:    &testutil.MockDockerClient{},
	})

	body := bytes.NewBufferString(`{"name":"new-name"}`)
	req := httptest.NewRequest(http.MethodPatch, "/v1/services/svc-rename", body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "unexpected body: %s", rr.Body.String())

	var result map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&result))
	assert.Equal(t, "svc-rename", result["id"])
	assert.Equal(t, "new-name", result["name"])

	// Verify the name was persisted in the database.
	var dbName string
	err = stateDB.QueryRow(`SELECT name FROM services WHERE id = ?`, "svc-rename").Scan(&dbName)
	require.NoError(t, err)
	assert.Equal(t, "new-name", dbName)
}

// TestPatchService_InvalidName_Returns400 verifies that invalid names return 400.
func TestPatchService_InvalidName_Returns400(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	_, err := stateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, dns_label, status, image, port, container_id, url, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-val", "tenant-1", "web", "", "running", "nginx:latest", 8080, "", "", 1000, 1000,
	)
	require.NoError(t, err)

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
		Docker:    &testutil.MockDockerClient{},
	})

	tests := []struct {
		name string
		body string
		desc string
	}{
		{"empty name", `{"name":""}`, "empty name should be rejected"},
		{"too long", `{"name":"` + strings.Repeat("a", 129) + `"}`, "name over 128 chars should be rejected"},
		{"invalid chars", `{"name":"bad!@#name"}`, "name with special characters should be rejected"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPatch, "/v1/services/svc-val", bytes.NewBufferString(tc.body))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")

			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)

			assert.Equal(t, http.StatusBadRequest, rr.Code, "%s: %s", tc.desc, rr.Body.String())
		})
	}
}

// TestPatchService_NotFound_Returns404 verifies that patching a nonexistent service returns 404.
func TestPatchService_NotFound_Returns404(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
		Docker:    &testutil.MockDockerClient{},
	})

	body := bytes.NewBufferString(`{"name":"new-name"}`)
	req := httptest.NewRequest(http.MethodPatch, "/v1/services/does-not-exist", body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code)
}

// TestPatchService_EmptyBody_Returns400 verifies that an empty or missing body returns 400.
func TestPatchService_EmptyBody_Returns400(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	_, err := stateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, dns_label, status, image, port, container_id, url, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-empty", "tenant-1", "web", "", "running", "nginx:latest", 8080, "", "", 1000, 1000,
	)
	require.NoError(t, err)

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
		Docker:    &testutil.MockDockerClient{},
	})

	tests := []struct {
		name string
		body string
	}{
		{"empty json object", `{}`},
		{"name field missing", `{"other":"field"}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPatch, "/v1/services/svc-empty", bytes.NewBufferString(tc.body))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")

			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)

			assert.Equal(t, http.StatusBadRequest, rr.Code, "body %q should return 400: %s", tc.body, rr.Body.String())
		})
	}
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
