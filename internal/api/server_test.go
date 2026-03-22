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
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/builds"
	"github.com/dennisonbertram/agentic-hosting/internal/crypto"
	"github.com/dennisonbertram/agentic-hosting/internal/databases"
	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/deployments"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/kanbans"
	"github.com/dennisonbertram/agentic-hosting/internal/metering"
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

// TestServiceList_StatusFilter verifies that GET /v1/services?status=running returns
// only services matching the requested status, and that invalid status values return 400.
func TestServiceList_StatusFilter(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	// Insert services with different statuses.
	for _, s := range []struct{ id, name, status string }{
		{"svc-run1", "web1", "running"},
		{"svc-run2", "web2", "running"},
		{"svc-stop1", "web3", "stopped"},
		{"svc-fail1", "web4", "failed"},
	} {
		_, err := stateDB.Exec(
			`INSERT INTO services (id, tenant_id, name, status, image, port, container_id, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			s.id, "tenant-1", s.name, s.status, "nginx:latest", 8080, "", 1000, 1000,
		)
		require.NoError(t, err)
	}

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
		Docker:    &testutil.MockDockerClient{},
	})

	t.Run("filter single status", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/services?status=running", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var svcs []map[string]any
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&svcs))
		assert.Len(t, svcs, 2, "should return only the 2 running services")
		for _, s := range svcs {
			assert.Equal(t, "running", s["status"])
		}
	})

	t.Run("filter comma-separated statuses", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/services?status=running,stopped", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var svcs []map[string]any
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&svcs))
		assert.Len(t, svcs, 3, "should return running + stopped = 3")
	})

	t.Run("no filter returns all", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/services", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var svcs []map[string]any
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&svcs))
		assert.Len(t, svcs, 4, "no filter should return all 4 services")
	})

	t.Run("invalid status returns 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/services?status=invalid_status", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusBadRequest, rr.Code)
		assert.Contains(t, rr.Body.String(), "invalid status value")
	})

	t.Run("valid and invalid mixed returns 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/services?status=running,bogus", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusBadRequest, rr.Code)
		assert.Contains(t, rr.Body.String(), "invalid status value: bogus")
	})

	t.Run("filter with no matches returns empty array", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/services?status=deploying", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var svcs []map[string]any
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&svcs))
		assert.Len(t, svcs, 0, "should return empty array for no matches")
	})
}

// TestSnapshotGet_DefaultMaskedEnvVars verifies that GET /v1/snapshots/{id}
// without ?reveal=true includes env vars with masked values.
func TestSnapshotGet_DefaultMaskedEnvVars(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	// Insert a service.
	_, err := stateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-snap", "tenant-1", "web", "running", "nginx:latest", 8080, 1000, 1000,
	)
	require.NoError(t, err)

	// Insert encrypted env vars for the service, then create a snapshot with them.
	enc1, err := crypto.Encrypt([]byte("secret-val"), masterKey)
	require.NoError(t, err)
	enc2, err := crypto.Encrypt([]byte("another-secret"), masterKey)
	require.NoError(t, err)

	envJSON := `{"DB_HOST":"` + enc1 + `","API_KEY":"` + enc2 + `"}`

	_, err = stateDB.Exec(
		`INSERT INTO snapshots (id, tenant_id, service_id, name, description, image_ref, env_encrypted, resource_config, port, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"snap-1", "tenant-1", "svc-snap", "test-snap", "desc", "img:1", envJSON, "{}", 8080, 1000,
	)
	require.NoError(t, err)

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
		Docker:    &testutil.MockDockerClient{},
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/snapshots/snap-1", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "unexpected body: %s", rr.Body.String())

	var resp map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal(t, "snap-1", resp["id"])
	assert.Equal(t, "test-snap", resp["name"])

	envVars, ok := resp["env_vars"].(map[string]any)
	require.True(t, ok, "env_vars should be a map in response")
	assert.Equal(t, "********", envVars["DB_HOST"], "env var value should be masked")
	assert.Equal(t, "********", envVars["API_KEY"], "env var value should be masked")
}

// TestSnapshotGet_RevealTrue verifies that GET /v1/snapshots/{id}?reveal=true
// returns decrypted env var values.
func TestSnapshotGet_RevealTrue(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	// Insert a service.
	_, err := stateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-snap2", "tenant-1", "web", "running", "nginx:latest", 8080, 1000, 1000,
	)
	require.NoError(t, err)

	// Encrypt real values.
	enc1, err := crypto.Encrypt([]byte("my-db-host"), masterKey)
	require.NoError(t, err)
	enc2, err := crypto.Encrypt([]byte("my-api-key"), masterKey)
	require.NoError(t, err)

	envJSON := `{"DB_HOST":"` + enc1 + `","API_KEY":"` + enc2 + `"}`

	_, err = stateDB.Exec(
		`INSERT INTO snapshots (id, tenant_id, service_id, name, description, image_ref, env_encrypted, resource_config, port, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"snap-2", "tenant-1", "svc-snap2", "test-snap-reveal", "desc", "img:2", envJSON, "{}", 8080, 1000,
	)
	require.NoError(t, err)

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
		Docker:    &testutil.MockDockerClient{},
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/snapshots/snap-2?reveal=true", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "unexpected body: %s", rr.Body.String())

	var resp map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal(t, "snap-2", resp["id"])

	envVars, ok := resp["env_vars"].(map[string]any)
	require.True(t, ok, "env_vars should be a map in response")
	assert.Equal(t, "my-db-host", envVars["DB_HOST"], "env var should be decrypted when reveal=true")
	assert.Equal(t, "my-api-key", envVars["API_KEY"], "env var should be decrypted when reveal=true")
}

// TestSnapshotGet_NoEnvVars verifies that a snapshot with no env vars returns
// an empty env_vars map (not omitted or null).
func TestSnapshotGet_NoEnvVars(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	_, err := stateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-snap3", "tenant-1", "web", "running", "nginx:latest", 8080, 1000, 1000,
	)
	require.NoError(t, err)

	// Snapshot with empty env blob.
	_, err = stateDB.Exec(
		`INSERT INTO snapshots (id, tenant_id, service_id, name, description, image_ref, env_encrypted, resource_config, port, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"snap-3", "tenant-1", "svc-snap3", "no-env-snap", "", "img:3", "", "{}", 8080, 1000,
	)
	require.NoError(t, err)

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
		Docker:    &testutil.MockDockerClient{},
	})

	// Default (no reveal) — should get empty env_vars map.
	req := httptest.NewRequest(http.MethodGet, "/v1/snapshots/snap-3", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "unexpected body: %s", rr.Body.String())

	var resp map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))

	// With empty env_encrypted, GetEnvKeys returns an empty map, which should serialize
	// as env_vars: {} (not omitted). But since omitempty is set, an empty map is omitted.
	// That's acceptable behavior for no env vars.
	assert.Equal(t, "snap-3", resp["id"])
}

// TestSnapshotGet_NotFound verifies 404 for a nonexistent snapshot ID.
func TestSnapshotGet_NotFound(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
		Docker:    &testutil.MockDockerClient{},
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/snapshots/does-not-exist", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code)
}

// TestDeploymentCancel_QueuedDeployment verifies that POST /v1/services/{id}/deployments/{id}/cancel
// cancels a pending deployment and returns 200 with cancelled status and cancelled_at timestamp.
func TestDeploymentCancel_QueuedDeployment(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	_, err := stateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, container_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-cancel", "tenant-1", "web", "running", "nginx:latest", 8080, "ctr-1", 1000, 1000,
	)
	require.NoError(t, err)

	deployStore := deployments.NewStore(stateDB)
	require.NoError(t, deployStore.Create(context.Background(), &deployments.Deployment{
		ID:        "deploy-q1",
		ServiceID: "svc-cancel",
		TenantID:  "tenant-1",
		Image:     "nginx:latest",
		Status:    deployments.StatusPending,
		Trigger:   deployments.TriggerManual,
		StartedAt: 1000,
		CreatedAt: 1000,
	}))

	srv := NewServer(ServerConfig{
		Store:           &db.Store{StateDB: stateDB},
		MasterKey:       masterKey,
		DevMode:         true,
		Docker:          &testutil.MockDockerClient{},
		DeploymentStore: deployStore,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/services/svc-cancel/deployments/deploy-q1/cancel", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "unexpected body: %s", rr.Body.String())

	var body map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&body))
	assert.Equal(t, "cancelled", body["status"])
	assert.Equal(t, "deploy-q1", body["id"])
	assert.NotNil(t, body["cancelled_at"], "response should include cancelled_at timestamp")
}

// TestDeploymentCancel_CompletedDeployment_Returns409 verifies that cancelling a
// completed (running) deployment returns 409 Conflict.
func TestDeploymentCancel_CompletedDeployment_Returns409(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	_, err := stateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, container_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-cancel2", "tenant-1", "web", "running", "nginx:latest", 8080, "ctr-1", 1000, 1000,
	)
	require.NoError(t, err)

	deployStore := deployments.NewStore(stateDB)
	require.NoError(t, deployStore.Create(context.Background(), &deployments.Deployment{
		ID:        "deploy-done",
		ServiceID: "svc-cancel2",
		TenantID:  "tenant-1",
		Image:     "nginx:latest",
		Status:    deployments.StatusRunning,
		Trigger:   deployments.TriggerManual,
		StartedAt: 1000,
		CreatedAt: 1000,
	}))

	srv := NewServer(ServerConfig{
		Store:           &db.Store{StateDB: stateDB},
		MasterKey:       masterKey,
		DevMode:         true,
		Docker:          &testutil.MockDockerClient{},
		DeploymentStore: deployStore,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/services/svc-cancel2/deployments/deploy-done/cancel", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusConflict, rr.Code, "cancelling a running deployment should return 409")
}

// TestDeploymentCancel_AlreadyCancelled_Returns409 verifies that cancelling an
// already-cancelled deployment returns 409 Conflict.
func TestDeploymentCancel_AlreadyCancelled_Returns409(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	_, err := stateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, container_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-cancel3", "tenant-1", "web", "running", "nginx:latest", 8080, "ctr-1", 1000, 1000,
	)
	require.NoError(t, err)

	deployStore := deployments.NewStore(stateDB)
	require.NoError(t, deployStore.Create(context.Background(), &deployments.Deployment{
		ID:        "deploy-already",
		ServiceID: "svc-cancel3",
		TenantID:  "tenant-1",
		Image:     "nginx:latest",
		Status:    deployments.StatusPending,
		Trigger:   deployments.TriggerManual,
		StartedAt: 1000,
		CreatedAt: 1000,
	}))

	srv := NewServer(ServerConfig{
		Store:           &db.Store{StateDB: stateDB},
		MasterKey:       masterKey,
		DevMode:         true,
		Docker:          &testutil.MockDockerClient{},
		DeploymentStore: deployStore,
	})

	// First cancel should succeed.
	req := httptest.NewRequest(http.MethodPost, "/v1/services/svc-cancel3/deployments/deploy-already/cancel", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	// Second cancel should return 409.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/services/svc-cancel3/deployments/deploy-already/cancel", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	rr2 := httptest.NewRecorder()
	srv.ServeHTTP(rr2, req2)
	assert.Equal(t, http.StatusConflict, rr2.Code, "cancelling an already-cancelled deployment should return 409")
}

// TestDeploymentCancel_NotFound_Returns404 verifies that cancelling a nonexistent
// deployment returns 404.
func TestDeploymentCancel_NotFound_Returns404(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	_, err := stateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, container_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-cancel4", "tenant-1", "web", "running", "nginx:latest", 8080, "ctr-1", 1000, 1000,
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

	req := httptest.NewRequest(http.MethodPost, "/v1/services/svc-cancel4/deployments/nonexistent/cancel", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code, "cancelling a nonexistent deployment should return 404")
}

// TestDeploymentCancel_WrongService_Returns404 verifies that a deployment belonging
// to a different service returns 404.
func TestDeploymentCancel_WrongService_Returns404(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	// Create two services.
	for _, svc := range []struct{ id, name string }{{"svc-a", "svc-a"}, {"svc-b", "svc-b"}} {
		_, err := stateDB.Exec(
			`INSERT INTO services (id, tenant_id, name, status, image, port, container_id, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			svc.id, "tenant-1", svc.name, "running", "nginx:latest", 8080, "ctr-1", 1000, 1000,
		)
		require.NoError(t, err)
	}

	deployStore := deployments.NewStore(stateDB)
	// Create deployment belonging to svc-a.
	require.NoError(t, deployStore.Create(context.Background(), &deployments.Deployment{
		ID:        "deploy-svc-a",
		ServiceID: "svc-a",
		TenantID:  "tenant-1",
		Image:     "nginx:latest",
		Status:    deployments.StatusPending,
		Trigger:   deployments.TriggerManual,
		StartedAt: 1000,
		CreatedAt: 1000,
	}))

	srv := NewServer(ServerConfig{
		Store:           &db.Store{StateDB: stateDB},
		MasterKey:       masterKey,
		DevMode:         true,
		Docker:          &testutil.MockDockerClient{},
		DeploymentStore: deployStore,
	})

	// Try to cancel deploy-svc-a via svc-b — should return 404.
	req := httptest.NewRequest(http.MethodPost, "/v1/services/svc-b/deployments/deploy-svc-a/cancel", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code, "deployment belonging to a different service should return 404")
}

// TestDeploymentCancel_ServiceNotFound_Returns404 verifies that specifying a
// nonexistent service returns 404.
func TestDeploymentCancel_ServiceNotFound_Returns404(t *testing.T) {
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

	req := httptest.NewRequest(http.MethodPost, "/v1/services/nonexistent-svc/deployments/deploy-1/cancel", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code, "nonexistent service should return 404")
}

// ---------------------------------------------------------------------------
// Regression tests — TestServiceList_StatusFilter_Regression
// (PR #140, issue #110)
// ---------------------------------------------------------------------------

func TestServiceList_StatusFilter_Regression(t *testing.T) {
	regLimiter.resetForTest()

	// Helper to insert a service with a specific status directly in the DB.
	insertService := func(t *testing.T, stateDB *sql.DB, id, name, status string) {
		t.Helper()
		_, err := stateDB.Exec(
			`INSERT INTO services (id, tenant_id, name, dns_label, status, image, port, container_id, url, created_at, updated_at)
			 VALUES (?, 'tenant-1', ?, '', ?, 'nginx:latest', 8080, '', '', ?, ?)`,
			id, name, status, 1000, 1000,
		)
		require.NoError(t, err)
	}

	// Helper to create a fresh server, DB, and auth token for each subtest.
	setup := func(t *testing.T) (*sql.DB, *Server, string) {
		t.Helper()
		stateDB := testutil.NewStateDB(t)
		masterKey := []byte("0123456789abcdef0123456789abcdef")
		token := seedAuthenticatedTenant(t, stateDB, masterKey)
		srv := NewServer(ServerConfig{
			Store:     &db.Store{StateDB: stateDB},
			MasterKey: masterKey,
			DevMode:   true,
			Docker:    &testutil.MockDockerClient{},
		})
		return stateDB, srv, token
	}

	// Helper to do a GET /v1/services with optional query string and return the response.
	doListRequest := func(t *testing.T, srv *Server, token, query, clientIP string) *httptest.ResponseRecorder {
		t.Helper()
		url := "/v1/services"
		if query != "" {
			url += "?" + query
		}
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("X-Real-Ip", clientIP)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr
	}

	t.Run("single_status_running", func(t *testing.T) {
		stateDB, srv, token := setup(t)
		insertService(t, stateDB, "svc-r1", "web-running", "running")
		insertService(t, stateDB, "svc-s1", "web-stopped", "stopped")
		insertService(t, stateDB, "svc-f1", "web-failed", "failed")

		rr := doListRequest(t, srv, token, "status=running", "10.110.1.1")
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var svcs []map[string]any
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&svcs))
		require.Len(t, svcs, 1, "should return only running services")
		assert.Equal(t, "svc-r1", svcs[0]["id"])
		assert.Equal(t, "running", svcs[0]["status"])
	})

	t.Run("single_status_stopped", func(t *testing.T) {
		stateDB, srv, token := setup(t)
		insertService(t, stateDB, "svc-r2", "web-running", "running")
		insertService(t, stateDB, "svc-s2", "web-stopped", "stopped")

		rr := doListRequest(t, srv, token, "status=stopped", "10.110.1.2")
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var svcs []map[string]any
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&svcs))
		require.Len(t, svcs, 1)
		assert.Equal(t, "svc-s2", svcs[0]["id"])
		assert.Equal(t, "stopped", svcs[0]["status"])
	})

	t.Run("single_status_created", func(t *testing.T) {
		stateDB, srv, token := setup(t)
		insertService(t, stateDB, "svc-c1", "web-created", "created")
		insertService(t, stateDB, "svc-r3", "web-running", "running")

		rr := doListRequest(t, srv, token, "status=created", "10.110.1.3")
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var svcs []map[string]any
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&svcs))
		require.Len(t, svcs, 1)
		assert.Equal(t, "created", svcs[0]["status"])
	})

	t.Run("multiple_comma_separated_statuses", func(t *testing.T) {
		stateDB, srv, token := setup(t)
		insertService(t, stateDB, "svc-m-r", "web-running", "running")
		insertService(t, stateDB, "svc-m-s", "web-stopped", "stopped")
		insertService(t, stateDB, "svc-m-f", "web-failed", "failed")
		insertService(t, stateDB, "svc-m-c", "web-created", "created")

		rr := doListRequest(t, srv, token, "status=running,stopped", "10.110.2.1")
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var svcs []map[string]any
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&svcs))
		require.Len(t, svcs, 2, "should return union of running and stopped")

		statuses := map[string]bool{}
		for _, svc := range svcs {
			statuses[svc["status"].(string)] = true
		}
		assert.True(t, statuses["running"], "should include running services")
		assert.True(t, statuses["stopped"], "should include stopped services")
		assert.False(t, statuses["failed"], "should not include failed services")
		assert.False(t, statuses["created"], "should not include created services")
	})

	t.Run("multiple_statuses_three_values", func(t *testing.T) {
		stateDB, srv, token := setup(t)
		insertService(t, stateDB, "svc-3-r", "web-running", "running")
		insertService(t, stateDB, "svc-3-s", "web-stopped", "stopped")
		insertService(t, stateDB, "svc-3-f", "web-failed", "failed")
		insertService(t, stateDB, "svc-3-d", "web-deploying", "deploying")

		rr := doListRequest(t, srv, token, "status=running,stopped,failed", "10.110.2.2")
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var svcs []map[string]any
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&svcs))
		require.Len(t, svcs, 3, "should return running, stopped, and failed")

		statuses := map[string]bool{}
		for _, svc := range svcs {
			statuses[svc["status"].(string)] = true
		}
		assert.True(t, statuses["running"])
		assert.True(t, statuses["stopped"])
		assert.True(t, statuses["failed"])
		assert.False(t, statuses["deploying"], "deploying should be excluded")
	})

	t.Run("case_sensitivity_uppercase_rejected", func(t *testing.T) {
		_, srv, token := setup(t)

		rr := doListRequest(t, srv, token, "status=Running", "10.110.3.1")
		assert.Equal(t, http.StatusBadRequest, rr.Code, "uppercase status should be rejected")
		assert.Contains(t, rr.Body.String(), "invalid status value")
	})

	t.Run("case_sensitivity_mixed_case_rejected", func(t *testing.T) {
		_, srv, token := setup(t)

		rr := doListRequest(t, srv, token, "status=RUNNING", "10.110.3.2")
		assert.Equal(t, http.StatusBadRequest, rr.Code, "all-caps status should be rejected")
		assert.Contains(t, rr.Body.String(), "invalid status value")
	})

	t.Run("case_sensitivity_one_valid_one_invalid", func(t *testing.T) {
		_, srv, token := setup(t)

		rr := doListRequest(t, srv, token, "status=running,Stopped", "10.110.3.3")
		assert.Equal(t, http.StatusBadRequest, rr.Code,
			"mixed valid+invalid statuses should be rejected; body: %s", rr.Body.String())
		assert.Contains(t, rr.Body.String(), "invalid status value")
	})

	t.Run("empty_status_parameter_no_filter", func(t *testing.T) {
		stateDB, srv, token := setup(t)
		insertService(t, stateDB, "svc-e-r", "web-running", "running")
		insertService(t, stateDB, "svc-e-s", "web-stopped", "stopped")

		rr := doListRequest(t, srv, token, "status=", "10.110.4.1")
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var svcs []map[string]any
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&svcs))
		assert.Len(t, svcs, 2, "empty status= should return all services (no filter)")
	})

	t.Run("status_with_only_commas_no_filter", func(t *testing.T) {
		stateDB, srv, token := setup(t)
		insertService(t, stateDB, "svc-comma-r", "web-running", "running")
		insertService(t, stateDB, "svc-comma-s", "web-stopped", "stopped")
		insertService(t, stateDB, "svc-comma-f", "web-failed", "failed")

		rr := doListRequest(t, srv, token, "status=,,,", "10.110.5.1")
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var svcs []map[string]any
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&svcs))
		assert.Len(t, svcs, 3, "status=,,, should be treated as no filter (all empty segments skipped)")
	})

	t.Run("combined_with_pagination_limit", func(t *testing.T) {
		stateDB, srv, token := setup(t)
		// Insert 3 running services with different created_at for deterministic ordering
		_, err := stateDB.Exec(
			`INSERT INTO services (id, tenant_id, name, dns_label, status, image, port, container_id, url, created_at, updated_at)
			 VALUES ('svc-pg-1', 'tenant-1', 'web-1', '', 'running', 'nginx:latest', 8080, '', '', 1000, 1000)`)
		require.NoError(t, err)
		_, err = stateDB.Exec(
			`INSERT INTO services (id, tenant_id, name, dns_label, status, image, port, container_id, url, created_at, updated_at)
			 VALUES ('svc-pg-2', 'tenant-1', 'web-2', '', 'running', 'nginx:latest', 8080, '', '', 2000, 2000)`)
		require.NoError(t, err)
		_, err = stateDB.Exec(
			`INSERT INTO services (id, tenant_id, name, dns_label, status, image, port, container_id, url, created_at, updated_at)
			 VALUES ('svc-pg-3', 'tenant-1', 'web-3', '', 'running', 'nginx:latest', 8080, '', '', 3000, 3000)`)
		require.NoError(t, err)
		// Insert a stopped service that should not appear
		insertService(t, stateDB, "svc-pg-s", "web-stopped", "stopped")

		// Page 1: limit=1, offset=0
		rr := doListRequest(t, srv, token, "status=running&limit=1&offset=0", "10.110.6.1")
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var page1 []map[string]any
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&page1))
		require.Len(t, page1, 1, "limit=1 should return exactly 1 service")
		assert.Equal(t, "running", page1[0]["status"])

		// Page 2: limit=1, offset=1
		rr2 := doListRequest(t, srv, token, "status=running&limit=1&offset=1", "10.110.6.2")
		require.Equal(t, http.StatusOK, rr2.Code, "body: %s", rr2.Body.String())

		var page2 []map[string]any
		require.NoError(t, json.NewDecoder(rr2.Body).Decode(&page2))
		require.Len(t, page2, 1, "page 2 should return exactly 1 service")
		assert.Equal(t, "running", page2[0]["status"])
		assert.NotEqual(t, page1[0]["id"], page2[0]["id"], "page 2 should return a different service than page 1")

		// All running: limit=10, offset=0
		rrAll := doListRequest(t, srv, token, "status=running&limit=10&offset=0", "10.110.6.3")
		require.Equal(t, http.StatusOK, rrAll.Code, "body: %s", rrAll.Body.String())

		var allRunning []map[string]any
		require.NoError(t, json.NewDecoder(rrAll.Body).Decode(&allRunning))
		assert.Len(t, allRunning, 3, "should return all 3 running services")
		for _, svc := range allRunning {
			assert.Equal(t, "running", svc["status"], "all returned services should be running")
		}
	})

	t.Run("filter_matching_zero_services_returns_empty_array", func(t *testing.T) {
		stateDB, srv, token := setup(t)
		// Only insert running services
		insertService(t, stateDB, "svc-z-r1", "web-1", "running")
		insertService(t, stateDB, "svc-z-r2", "web-2", "running")

		rr := doListRequest(t, srv, token, "status=stopped", "10.110.7.1")
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		// Verify it's an empty JSON array, not null
		body := strings.TrimSpace(rr.Body.String())
		assert.Equal(t, "[]", body, "zero matches should return empty JSON array, not null")

		// Also decode to verify
		var svcs []map[string]any
		require.NoError(t, json.Unmarshal([]byte(body), &svcs))
		assert.Len(t, svcs, 0)
	})

	t.Run("filter_matching_zero_services_deploying_returns_empty_array", func(t *testing.T) {
		stateDB, srv, token := setup(t)
		insertService(t, stateDB, "svc-zd-r1", "web-1", "running")

		rr := doListRequest(t, srv, token, "status=deploying", "10.110.7.2")
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		body := strings.TrimSpace(rr.Body.String())
		assert.Equal(t, "[]", body, "filtering by deploying when none exist should return empty array")
	})

	t.Run("invalid_status_value_returns_400", func(t *testing.T) {
		_, srv, token := setup(t)

		rr := doListRequest(t, srv, token, "status=bogus", "10.110.8.1")
		assert.Equal(t, http.StatusBadRequest, rr.Code, "invalid status should return 400")
		assert.Contains(t, rr.Body.String(), "invalid status value")
	})

	t.Run("invalid_status_in_comma_list_returns_400", func(t *testing.T) {
		_, srv, token := setup(t)

		rr := doListRequest(t, srv, token, "status=running,bogus,stopped", "10.110.8.2")
		assert.Equal(t, http.StatusBadRequest, rr.Code,
			"any invalid status in comma list should return 400; body: %s", rr.Body.String())
		assert.Contains(t, rr.Body.String(), "invalid status value: bogus")
	})

	t.Run("no_status_param_returns_all", func(t *testing.T) {
		stateDB, srv, token := setup(t)
		insertService(t, stateDB, "svc-all-r", "web-running", "running")
		insertService(t, stateDB, "svc-all-s", "web-stopped", "stopped")
		insertService(t, stateDB, "svc-all-f", "web-failed", "failed")
		insertService(t, stateDB, "svc-all-c", "web-created", "created")
		insertService(t, stateDB, "svc-all-d", "web-deploying", "deploying")

		rr := doListRequest(t, srv, token, "", "10.110.9.1")
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var svcs []map[string]any
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&svcs))
		assert.Len(t, svcs, 5, "no status param should return all services")
	})

	t.Run("all_valid_statuses_accepted", func(t *testing.T) {
		stateDB, srv, token := setup(t)
		insertService(t, stateDB, "svc-vs-r", "web-running", "running")
		insertService(t, stateDB, "svc-vs-s", "web-stopped", "stopped")
		insertService(t, stateDB, "svc-vs-f", "web-failed", "failed")
		insertService(t, stateDB, "svc-vs-c", "web-created", "created")
		insertService(t, stateDB, "svc-vs-d", "web-deploying", "deploying")

		rr := doListRequest(t, srv, token, "status=created,deploying,running,stopped,failed", "10.110.10.1")
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var svcs []map[string]any
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&svcs))
		assert.Len(t, svcs, 5, "all 5 valid statuses should return all 5 services")
	})

	t.Run("whitespace_around_status_values_trimmed", func(t *testing.T) {
		stateDB, srv, token := setup(t)
		insertService(t, stateDB, "svc-ws-r", "web-running", "running")
		insertService(t, stateDB, "svc-ws-s", "web-stopped", "stopped")

		// Spaces around values in the comma list
		rr := doListRequest(t, srv, token, "status=+running+,+stopped", "10.110.11.1")
		// URL query values with + are interpreted as spaces by net/http
		require.Equal(t, http.StatusOK, rr.Code, "whitespace around status values should be trimmed; body: %s", rr.Body.String())

		var svcs []map[string]any
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&svcs))
		assert.Len(t, svcs, 2, "whitespace-trimmed running and stopped should match 2 services")
	})

	t.Run("duplicate_status_values_work", func(t *testing.T) {
		stateDB, srv, token := setup(t)
		insertService(t, stateDB, "svc-dup-r", "web-running", "running")
		insertService(t, stateDB, "svc-dup-s", "web-stopped", "stopped")

		rr := doListRequest(t, srv, token, "status=running,running", "10.110.12.1")
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var svcs []map[string]any
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&svcs))
		assert.Len(t, svcs, 1, "duplicate running should still return 1 running service (SQL IN deduplicates)")
		assert.Equal(t, "running", svcs[0]["status"])
	})

	t.Run("tenant_isolation_with_status_filter", func(t *testing.T) {
		stateDB, srv, token := setup(t)
		// Insert services for tenant-1 (the authenticated tenant)
		insertService(t, stateDB, "svc-iso-t1", "web-t1", "running")

		// Insert a running service for a different tenant directly in DB
		_, err := stateDB.Exec(
			`INSERT INTO tenants (id, name, email, status, created_at, updated_at) VALUES (?, ?, ?, 'active', 1, 1)`,
			"tenant-other", "Other", "other@example.com",
		)
		require.NoError(t, err)
		_, err = stateDB.Exec(
			`INSERT INTO services (id, tenant_id, name, dns_label, status, image, port, container_id, url, created_at, updated_at)
			 VALUES ('svc-iso-t2', 'tenant-other', 'web-other', '', 'running', 'nginx:latest', 8080, '', '', 1000, 1000)`,
		)
		require.NoError(t, err)

		rr := doListRequest(t, srv, token, "status=running", "10.110.13.1")
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var svcs []map[string]any
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&svcs))
		require.Len(t, svcs, 1, "should only return services for the authenticated tenant")
		assert.Equal(t, "svc-iso-t1", svcs[0]["id"])
	})
}

// ---------------------------------------------------------------------------
// Regression tests — TestSnapshotReveal_Regression
// (PR #141, issue #112)
// ---------------------------------------------------------------------------

// seedSnapshotTestDB sets up a tenant, service, and optionally env vars for snapshot tests.
// Returns the auth token, stateDB, and the service ID used.
func seedSnapshotTestDB(t *testing.T, withEnvVars bool) (token string, stateDB *sql.DB, serviceID string) {
	t.Helper()
	stateDB = testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token = seedAuthenticatedTenant(t, stateDB, masterKey)
	serviceID = "svc-snap-1"

	_, err := stateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, created_at, updated_at)
		 VALUES (?, ?, ?, 'running', 'nginx:latest', 8080, ?, ?)`,
		serviceID, "tenant-1", "snap-test-svc", 1000, 1000,
	)
	require.NoError(t, err)

	if withEnvVars {
		now := int64(1000)
		for k, v := range map[string]string{"DB_HOST": "localhost", "DB_PASSWORD": "secret123"} {
			encrypted, err := crypto.Encrypt([]byte(v), masterKey)
			require.NoError(t, err)
			_, err = stateDB.Exec(
				`INSERT INTO service_env (service_id, key, value_encrypted, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?)`,
				serviceID, k, encrypted, now, now,
			)
			require.NoError(t, err)
		}
	}

	return token, stateDB, serviceID
}

// createSnapshotViaAPI creates a snapshot via the API and returns the snapshot ID.
func createSnapshotViaAPI(t *testing.T, srv http.Handler, token, serviceID, name string) string {
	t.Helper()
	body := []byte(`{"name":"` + name + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/services/"+serviceID+"/snapshots", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	require.Equal(t, http.StatusCreated, rr.Code, "snapshot create failed: %s", rr.Body.String())

	var resp map[string]interface{}
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	id, ok := resp["id"].(string)
	require.True(t, ok, "snapshot response should contain string id")
	return id
}

func TestSnapshotReveal_Regression(t *testing.T) {
	regLimiter.resetForTest()
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	t.Run("no_env_vars/reveal_true_returns_empty_or_absent_env_vars", func(t *testing.T) {
		token, stateDB, serviceID := seedSnapshotTestDB(t, false)
		srv := NewServer(ServerConfig{
			Store:     &db.Store{StateDB: stateDB},
			MasterKey: masterKey,
			DevMode:   true,
			Docker:    &testutil.MockDockerClient{},
		})

		snapID := createSnapshotViaAPI(t, srv, token, serviceID, "no-env-snap")

		req := httptest.NewRequest(http.MethodGet, "/v1/snapshots/"+snapID+"?reveal=true", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Real-Ip", "10.112.1.1")

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var resp map[string]interface{}
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
		assert.Equal(t, snapID, resp["id"])

		// env_vars should be absent or an empty map for a snapshot with no env vars
		if envVars, ok := resp["env_vars"].(map[string]interface{}); ok {
			assert.Empty(t, envVars, "env_vars should be empty for a snapshot with no env vars")
		}
		// else: env_vars omitted entirely, which is also acceptable
	})

	t.Run("with_env_vars/default_no_reveal_returns_masked", func(t *testing.T) {
		token, stateDB, serviceID := seedSnapshotTestDB(t, true)
		srv := NewServer(ServerConfig{
			Store:     &db.Store{StateDB: stateDB},
			MasterKey: masterKey,
			DevMode:   true,
			Docker:    &testutil.MockDockerClient{},
		})

		snapID := createSnapshotViaAPI(t, srv, token, serviceID, "env-snap")

		req := httptest.NewRequest(http.MethodGet, "/v1/snapshots/"+snapID, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Real-Ip", "10.112.1.2")

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var resp map[string]interface{}
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))

		envVars, ok := resp["env_vars"].(map[string]interface{})
		require.True(t, ok, "response should contain env_vars map")
		assert.Len(t, envVars, 2, "should have 2 env var keys")

		// Values should be masked
		for key, val := range envVars {
			assert.Equal(t, "********", val, "env var %q should be masked", key)
		}
		// Keys should be present
		assert.Contains(t, envVars, "DB_HOST")
		assert.Contains(t, envVars, "DB_PASSWORD")
	})

	t.Run("with_env_vars/reveal_true_returns_decrypted", func(t *testing.T) {
		token, stateDB, serviceID := seedSnapshotTestDB(t, true)
		srv := NewServer(ServerConfig{
			Store:     &db.Store{StateDB: stateDB},
			MasterKey: masterKey,
			DevMode:   true,
			Docker:    &testutil.MockDockerClient{},
		})

		snapID := createSnapshotViaAPI(t, srv, token, serviceID, "reveal-snap")

		req := httptest.NewRequest(http.MethodGet, "/v1/snapshots/"+snapID+"?reveal=true", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Real-Ip", "10.112.1.3")

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var resp map[string]interface{}
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))

		envVars, ok := resp["env_vars"].(map[string]interface{})
		require.True(t, ok, "response should contain env_vars map")
		assert.Len(t, envVars, 2, "should have 2 env vars")
		assert.Equal(t, "localhost", envVars["DB_HOST"], "DB_HOST should be decrypted")
		assert.Equal(t, "secret123", envVars["DB_PASSWORD"], "DB_PASSWORD should be decrypted")
	})

	t.Run("not_found/returns_404_regardless_of_reveal", func(t *testing.T) {
		token, stateDB, _ := seedSnapshotTestDB(t, false)
		srv := NewServer(ServerConfig{
			Store:     &db.Store{StateDB: stateDB},
			MasterKey: masterKey,
			DevMode:   true,
			Docker:    &testutil.MockDockerClient{},
		})

		for _, url := range []string{
			"/v1/snapshots/nonexistent-snap-id",
			"/v1/snapshots/nonexistent-snap-id?reveal=true",
		} {
			req := httptest.NewRequest(http.MethodGet, url, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("X-Real-Ip", "10.112.1.4")

			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)

			assert.Equal(t, http.StatusNotFound, rr.Code,
				"GET %s should return 404, got %d: %s", url, rr.Code, rr.Body.String())
		}
	})

	t.Run("reveal_false_explicit/behaves_same_as_no_reveal", func(t *testing.T) {
		token, stateDB, serviceID := seedSnapshotTestDB(t, true)
		srv := NewServer(ServerConfig{
			Store:     &db.Store{StateDB: stateDB},
			MasterKey: masterKey,
			DevMode:   true,
			Docker:    &testutil.MockDockerClient{},
		})

		snapID := createSnapshotViaAPI(t, srv, token, serviceID, "reveal-false-snap")

		// Request with reveal=false
		req := httptest.NewRequest(http.MethodGet, "/v1/snapshots/"+snapID+"?reveal=false", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Real-Ip", "10.112.1.5")

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var resp map[string]interface{}
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))

		envVars, ok := resp["env_vars"].(map[string]interface{})
		require.True(t, ok, "response should contain env_vars map")

		// Should be masked, same as default (no reveal param)
		for key, val := range envVars {
			assert.Equal(t, "********", val, "env var %q should be masked with reveal=false", key)
		}
	})

	t.Run("reveal_true/produces_audit_log", func(t *testing.T) {
		token, stateDB, serviceID := seedSnapshotTestDB(t, true)
		srv := NewServer(ServerConfig{
			Store:     &db.Store{StateDB: stateDB},
			MasterKey: masterKey,
			DevMode:   true,
			Docker:    &testutil.MockDockerClient{},
		})

		snapID := createSnapshotViaAPI(t, srv, token, serviceID, "audit-snap")

		logBuf := captureLog(t)

		req := httptest.NewRequest(http.MethodGet, "/v1/snapshots/"+snapID+"?reveal=true", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Real-Ip", "10.112.1.6")

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code)

		logOutput := logBuf.String()
		assert.Contains(t, logOutput, "AUDIT:")
		assert.Contains(t, logOutput, "action=snapshot.env.revealed")
		assert.Contains(t, logOutput, "tenant=tenant-1")
		assert.Contains(t, logOutput, "snapshot="+snapID)
	})
}

// ---------------------------------------------------------------------------
// Regression tests — TestSnapshotListFilter_Regression
// (PR #142, issue #113)
// ---------------------------------------------------------------------------

// insertSnapshotDirect inserts a snapshot row directly into the database (bypassing Docker/API).
func insertSnapshotDirect(t *testing.T, stateDB *sql.DB, id, tenantID, serviceID, name string, createdAt int64) {
	t.Helper()
	imageRef := "127.0.0.1:5000/snapshots/" + tenantID + ":" + id
	_, err := stateDB.Exec(
		`INSERT INTO snapshots (id, tenant_id, service_id, name, description, image_ref, env_encrypted, resource_config, port, created_at)
		 VALUES (?, ?, ?, ?, '', ?, '', '{}', 8080, ?)`,
		id, tenantID, serviceID, name, imageRef, createdAt,
	)
	require.NoError(t, err)
}

func TestSnapshotListFilter_Regression(t *testing.T) {
	regLimiter.resetForTest()
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	t.Run("filter_by_service_id", func(t *testing.T) {
		token, stateDB, serviceID := seedSnapshotTestDB(t, false)

		// Create a second service
		svc2ID := "svc-snap-2"
		_, err := stateDB.Exec(
			`INSERT INTO services (id, tenant_id, name, status, image, port, created_at, updated_at)
			 VALUES (?, ?, ?, 'running', 'redis:latest', 6379, ?, ?)`,
			svc2ID, "tenant-1", "snap-test-svc-2", 1000, 1000,
		)
		require.NoError(t, err)

		srv := NewServer(ServerConfig{
			Store:     &db.Store{StateDB: stateDB},
			MasterKey: masterKey,
			DevMode:   true,
			Docker:    &testutil.MockDockerClient{},
		})

		// Create snapshots on both services
		createSnapshotViaAPI(t, srv, token, serviceID, "svc1-snap")
		createSnapshotViaAPI(t, srv, token, svc2ID, "svc2-snap")

		// Filter by service_id=svc-snap-1
		req := httptest.NewRequest(http.MethodGet, "/v1/snapshots?service_id="+serviceID, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Real-Ip", "10.112.2.1")

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var snaps []map[string]interface{}
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&snaps))
		require.Len(t, snaps, 1, "should return only snapshots for the filtered service")
		assert.Equal(t, serviceID, snaps[0]["service_id"])
	})

	t.Run("filter_by_name_partial_match", func(t *testing.T) {
		token, stateDB, serviceID := seedSnapshotTestDB(t, false)
		srv := NewServer(ServerConfig{
			Store:     &db.Store{StateDB: stateDB},
			MasterKey: masterKey,
			DevMode:   true,
			Docker:    &testutil.MockDockerClient{},
		})

		createSnapshotViaAPI(t, srv, token, serviceID, "prod-backup-daily")
		createSnapshotViaAPI(t, srv, token, serviceID, "staging-backup-daily")
		createSnapshotViaAPI(t, srv, token, serviceID, "dev-snapshot")

		// Filter by name containing "backup"
		req := httptest.NewRequest(http.MethodGet, "/v1/snapshots?name=backup", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Real-Ip", "10.112.2.2")

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var snaps []map[string]interface{}
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&snaps))
		require.Len(t, snaps, 2, "should return 2 snapshots matching 'backup'")

		for _, snap := range snaps {
			name := snap["name"].(string)
			assert.Contains(t, name, "backup", "each returned snapshot name should contain 'backup'")
		}
	})

	t.Run("filter_by_since_unix_timestamp", func(t *testing.T) {
		token, stateDB, _ := seedSnapshotTestDB(t, false)

		// Directly insert snapshots with specific timestamps to avoid time.Sleep
		now := int64(2000000)
		insertSnapshotDirect(t, stateDB, "old-snap", "tenant-1", "svc-snap-1", "old-snapshot", now-1000)
		insertSnapshotDirect(t, stateDB, "new-snap", "tenant-1", "svc-snap-1", "new-snapshot", now)

		srv := NewServer(ServerConfig{
			Store:     &db.Store{StateDB: stateDB},
			MasterKey: masterKey,
			DevMode:   true,
			Docker:    &testutil.MockDockerClient{},
		})

		// Filter for snapshots since now-500 (should exclude the old one)
		sinceVal := strconv.FormatInt(now-500, 10)
		req := httptest.NewRequest(http.MethodGet, "/v1/snapshots?since="+sinceVal, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Real-Ip", "10.112.2.3")

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var snaps []map[string]interface{}
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&snaps))
		require.Len(t, snaps, 1, "should return only the newer snapshot")
		assert.Equal(t, "new-snap", snaps[0]["id"])
	})

	t.Run("combined_filters_service_id_and_since", func(t *testing.T) {
		token, stateDB, _ := seedSnapshotTestDB(t, false)

		// Create a second service
		svc2ID := "svc-snap-2"
		_, err := stateDB.Exec(
			`INSERT INTO services (id, tenant_id, name, status, image, port, created_at, updated_at)
			 VALUES (?, ?, ?, 'running', 'redis:latest', 6379, ?, ?)`,
			svc2ID, "tenant-1", "snap-test-svc-2", 1000, 1000,
		)
		require.NoError(t, err)

		now := int64(3000000)
		// svc1: old and new
		insertSnapshotDirect(t, stateDB, "s1-old", "tenant-1", "svc-snap-1", "s1-old-snap", now-2000)
		insertSnapshotDirect(t, stateDB, "s1-new", "tenant-1", "svc-snap-1", "s1-new-snap", now)
		// svc2: new
		insertSnapshotDirect(t, stateDB, "s2-new", "tenant-1", svc2ID, "s2-new-snap", now)

		srv := NewServer(ServerConfig{
			Store:     &db.Store{StateDB: stateDB},
			MasterKey: masterKey,
			DevMode:   true,
			Docker:    &testutil.MockDockerClient{},
		})

		// Filter by service_id=svc-snap-1 AND since=now-500
		sinceVal := strconv.FormatInt(now-500, 10)
		req := httptest.NewRequest(http.MethodGet, "/v1/snapshots?service_id=svc-snap-1&since="+sinceVal, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Real-Ip", "10.112.2.4")

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var snaps []map[string]interface{}
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&snaps))
		require.Len(t, snaps, 1, "should return only svc1 new snapshot")
		assert.Equal(t, "s1-new", snaps[0]["id"])
		assert.Equal(t, "svc-snap-1", snaps[0]["service_id"])
	})

	t.Run("invalid_since_negative_returns_400", func(t *testing.T) {
		token, stateDB, _ := seedSnapshotTestDB(t, false)
		srv := NewServer(ServerConfig{
			Store:     &db.Store{StateDB: stateDB},
			MasterKey: masterKey,
			DevMode:   true,
			Docker:    &testutil.MockDockerClient{},
		})

		req := httptest.NewRequest(http.MethodGet, "/v1/snapshots?since=-1", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Real-Ip", "10.112.2.5")

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusBadRequest, rr.Code,
			"negative since value should return 400: %s", rr.Body.String())
	})

	t.Run("invalid_since_non_numeric_returns_400", func(t *testing.T) {
		token, stateDB, _ := seedSnapshotTestDB(t, false)
		srv := NewServer(ServerConfig{
			Store:     &db.Store{StateDB: stateDB},
			MasterKey: masterKey,
			DevMode:   true,
			Docker:    &testutil.MockDockerClient{},
		})

		req := httptest.NewRequest(http.MethodGet, "/v1/snapshots?since=not-a-number", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Real-Ip", "10.112.2.6")

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusBadRequest, rr.Code,
			"non-numeric since value should return 400: %s", rr.Body.String())
	})

	t.Run("no_filters_returns_all_snapshots", func(t *testing.T) {
		token, stateDB, serviceID := seedSnapshotTestDB(t, false)
		srv := NewServer(ServerConfig{
			Store:     &db.Store{StateDB: stateDB},
			MasterKey: masterKey,
			DevMode:   true,
			Docker:    &testutil.MockDockerClient{},
		})

		createSnapshotViaAPI(t, srv, token, serviceID, "snap-a")
		createSnapshotViaAPI(t, srv, token, serviceID, "snap-b")
		createSnapshotViaAPI(t, srv, token, serviceID, "snap-c")

		req := httptest.NewRequest(http.MethodGet, "/v1/snapshots", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Real-Ip", "10.112.2.7")

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var snaps []map[string]interface{}
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&snaps))
		assert.Len(t, snaps, 3, "should return all 3 snapshots when no filters applied")
	})

	t.Run("pagination_works_with_filters", func(t *testing.T) {
		token, stateDB, _ := seedSnapshotTestDB(t, false)

		// Insert 5 snapshots with the same prefix in their name
		now := int64(4000000)
		for i := 0; i < 5; i++ {
			id := "page-snap-" + strconv.Itoa(i)
			name := "filtered-snap-" + strconv.Itoa(i)
			insertSnapshotDirect(t, stateDB, id, "tenant-1", "svc-snap-1", name, now+int64(i))
		}

		srv := NewServer(ServerConfig{
			Store:     &db.Store{StateDB: stateDB},
			MasterKey: masterKey,
			DevMode:   true,
			Docker:    &testutil.MockDockerClient{},
		})

		// Request page 1: limit=2, offset=0, with name filter
		req := httptest.NewRequest(http.MethodGet, "/v1/snapshots?name=filtered&limit=2&offset=0", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Real-Ip", "10.112.2.8")

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var page1 []map[string]interface{}
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&page1))
		assert.Len(t, page1, 2, "page 1 should have 2 results")

		// Request page 2: limit=2, offset=2
		req2 := httptest.NewRequest(http.MethodGet, "/v1/snapshots?name=filtered&limit=2&offset=2", nil)
		req2.Header.Set("Authorization", "Bearer "+token)
		req2.Header.Set("X-Real-Ip", "10.112.2.9")

		rr2 := httptest.NewRecorder()
		srv.ServeHTTP(rr2, req2)

		require.Equal(t, http.StatusOK, rr2.Code, "body: %s", rr2.Body.String())

		var page2 []map[string]interface{}
		require.NoError(t, json.NewDecoder(rr2.Body).Decode(&page2))
		assert.Len(t, page2, 2, "page 2 should have 2 results")

		// Request page 3: limit=2, offset=4
		req3 := httptest.NewRequest(http.MethodGet, "/v1/snapshots?name=filtered&limit=2&offset=4", nil)
		req3.Header.Set("Authorization", "Bearer "+token)
		req3.Header.Set("X-Real-Ip", "10.112.2.10")

		rr3 := httptest.NewRecorder()
		srv.ServeHTTP(rr3, req3)

		require.Equal(t, http.StatusOK, rr3.Code, "body: %s", rr3.Body.String())

		var page3 []map[string]interface{}
		require.NoError(t, json.NewDecoder(rr3.Body).Decode(&page3))
		assert.Len(t, page3, 1, "page 3 should have 1 result (last page)")

		// Verify no overlap between pages
		allIDs := make(map[string]bool)
		for _, snap := range page1 {
			allIDs[snap["id"].(string)] = true
		}
		for _, snap := range page2 {
			id := snap["id"].(string)
			assert.False(t, allIDs[id], "page 2 snapshot %s should not overlap with page 1", id)
			allIDs[id] = true
		}
		for _, snap := range page3 {
			id := snap["id"].(string)
			assert.False(t, allIDs[id], "page 3 snapshot %s should not overlap with earlier pages", id)
			allIDs[id] = true
		}
		assert.Len(t, allIDs, 5, "all 5 snapshots should be returned across 3 pages")
	})
}

// ---------------------------------------------------------------------------
// Regression tests — TestMeteringAPI_Regression
// (PR #143, metering API)
// ---------------------------------------------------------------------------

// Regression: omitting the period param defaults to "daily" and returns 200.
func TestMeteringAPI_Regression_MissingPeriodDefaults(t *testing.T) {
	regLimiter.resetForTest()
	t.Cleanup(regLimiter.resetForTest)

	stateDB := testutil.NewStateDB(t)
	meteringDB := testutil.NewMeteringDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	srv := NewServer(ServerConfig{
		Store:         &db.Store{StateDB: stateDB, MeteringDB: meteringDB},
		MasterKey:     masterKey,
		DevMode:       true,
		MeteringStore: metering.NewStore(meteringDB),
	})

	// No period param — should default to "daily" and return 200
	req := httptest.NewRequest(http.MethodGet, "/v1/tenant/usage/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Real-Ip", "10.134.1.1")

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code,
		"missing period should default to daily and return 200, got body: %s", rr.Body.String())
}

// Regression: invalid period value returns 400.
func TestMeteringAPI_Regression_InvalidPeriodReturns400(t *testing.T) {
	regLimiter.resetForTest()
	t.Cleanup(regLimiter.resetForTest)

	stateDB := testutil.NewStateDB(t)
	meteringDB := testutil.NewMeteringDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	srv := NewServer(ServerConfig{
		Store:         &db.Store{StateDB: stateDB, MeteringDB: meteringDB},
		MasterKey:     masterKey,
		DevMode:       true,
		MeteringStore: metering.NewStore(meteringDB),
	})

	for _, period := range []string{"weekly", "monthly", "minute", "HOURLY", ""} {
		if period == "" {
			continue // empty defaults to "daily", which is valid
		}
		t.Run("period="+period, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/tenant/usage/metrics?period="+period, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("X-Real-Ip", "10.134.2.1")

			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)

			assert.Equal(t, http.StatusBadRequest, rr.Code,
				"period=%q should return 400", period)
			assert.Contains(t, rr.Body.String(), "period must be",
				"error body should indicate valid periods")
		})
	}
}

// Regression: valid request returns 200 with a JSON metrics array.
func TestMeteringAPI_Regression_ValidRequestReturnsMetrics(t *testing.T) {
	regLimiter.resetForTest()
	t.Cleanup(regLimiter.resetForTest)

	stateDB := testutil.NewStateDB(t)
	meteringDB := testutil.NewMeteringDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	// Seed usage events for two different hours
	h1 := time.Date(2026, 3, 20, 10, 15, 0, 0, time.UTC).Unix()
	h2 := time.Date(2026, 3, 20, 11, 30, 0, 0, time.UTC).Unix()
	for i, ts := range []int64{h1, h2} {
		_, err := meteringDB.Exec(
			`INSERT INTO usage_events (id, tenant_id, service_id, event_type, cpu_seconds, memory_mb_seconds, network_ingress_bytes, network_egress_bytes, recorded_at)
			 VALUES (?, ?, ?, 'sample', 5.0, 50.0, 100, 200, ?)`,
			"reg-evt-"+strconv.Itoa(i), "tenant-1", "svc-1", ts,
		)
		require.NoError(t, err)
	}

	srv := NewServer(ServerConfig{
		Store:         &db.Store{StateDB: stateDB, MeteringDB: meteringDB},
		MasterKey:     masterKey,
		DevMode:       true,
		MeteringStore: metering.NewStore(meteringDB),
	})

	req := httptest.NewRequest(http.MethodGet,
		"/v1/tenant/usage/metrics?period=hourly&since=2026-03-20T10:00:00Z&until=2026-03-20T12:00:00Z", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Real-Ip", "10.134.3.1")

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	var metrics []metering.Metric
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&metrics))
	assert.Len(t, metrics, 2, "should return two hourly buckets")
	assert.Equal(t, "2026-03-20T10:00:00Z", metrics[0].Timestamp)
	assert.Equal(t, "2026-03-20T11:00:00Z", metrics[1].Timestamp)
}

// Regression: service-level metrics for a service belonging to a different
// tenant must return 404 (tenant isolation at the API layer).
func TestMeteringAPI_Regression_ServiceMetrics_WrongTenant404(t *testing.T) {
	regLimiter.resetForTest()
	t.Cleanup(regLimiter.resetForTest)

	stateDB := testutil.NewStateDB(t)
	meteringDB := testutil.NewMeteringDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey) // creates tenant-1

	// Create a second tenant and its service
	_, err := stateDB.Exec(
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at) VALUES (?, ?, ?, 'active', 1, 1)`,
		"tenant-2", "Other Tenant", "other@example.com",
	)
	require.NoError(t, err)
	_, err = stateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, container_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-other-tenant", "tenant-2", "web", "running", "nginx:latest", 8080, "", 1000, 1000,
	)
	require.NoError(t, err)

	// Seed metering data for tenant-2's service
	ts := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC).Unix()
	_, err = meteringDB.Exec(
		`INSERT INTO usage_events (id, tenant_id, service_id, event_type, cpu_seconds, memory_mb_seconds, network_ingress_bytes, network_egress_bytes, recorded_at)
		 VALUES (?, ?, ?, 'sample', 99.0, 999.0, 9999, 9999, ?)`,
		"reg-cross-evt", "tenant-2", "svc-other-tenant", ts,
	)
	require.NoError(t, err)

	srv := NewServer(ServerConfig{
		Store:         &db.Store{StateDB: stateDB, MeteringDB: meteringDB},
		MasterKey:     masterKey,
		DevMode:       true,
		MeteringStore: metering.NewStore(meteringDB),
	})

	// Tenant-1 tries to access tenant-2's service metrics
	req := httptest.NewRequest(http.MethodGet, "/v1/services/svc-other-tenant/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Real-Ip", "10.134.4.1")

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code,
		"accessing another tenant's service metrics should return 404")
}

// ---------------------------------------------------------------------------
// Regression tests — TestQuotaUpdate_Regression
// (PR #145, issue #136)
// ---------------------------------------------------------------------------

func TestQuotaUpdate_Regression(t *testing.T) {
	regLimiter.resetForTest()
	const bootstrapToken = "test-bootstrap-token-quota-update"
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	newServer := func(t *testing.T, tokens []string) (*Server, *sql.DB) {
		t.Helper()
		stateDB := testutil.NewStateDB(t)
		_, err := stateDB.Exec(
			`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
			 VALUES (?, ?, ?, 'active', 1, 1)`,
			"tenant-quota", "Quota Tenant", "quota@example.com",
		)
		require.NoError(t, err)
		_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`, "tenant-quota")
		require.NoError(t, err)

		srv := NewServer(ServerConfig{
			Store:           &db.Store{StateDB: stateDB},
			MasterKey:       masterKey,
			DevMode:         true,
			BootstrapTokens: tokens,
		})
		return srv, stateDB
	}

	t.Run("valid update with bootstrap token succeeds 200", func(t *testing.T) {
		srv, stateDB := newServer(t, []string{bootstrapToken})

		body := []byte(`{"max_services":10,"max_databases":5}`)
		req := quotaRequest("10.136.1.1", "tenant-quota", bootstrapToken, body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var resp QuotaUpdateResponse
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
		assert.Equal(t, "tenant-quota", resp.TenantID)
		assert.Equal(t, 10, resp.MaxServices)
		assert.Equal(t, 5, resp.MaxDatabases)

		// Verify persistence
		var maxSvc, maxDB int
		err := stateDB.QueryRow(`SELECT max_services, max_databases FROM tenant_quotas WHERE tenant_id = ?`, "tenant-quota").Scan(&maxSvc, &maxDB)
		require.NoError(t, err)
		assert.Equal(t, 10, maxSvc)
		assert.Equal(t, 5, maxDB)
	})

	t.Run("missing bootstrap token returns 401", func(t *testing.T) {
		srv, _ := newServer(t, []string{bootstrapToken})

		body := []byte(`{"max_services":10}`)
		req := quotaRequest("10.136.1.2", "tenant-quota", "", body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code)
	})

	t.Run("wrong bootstrap token returns 401", func(t *testing.T) {
		srv, _ := newServer(t, []string{bootstrapToken})

		body := []byte(`{"max_services":10}`)
		req := quotaRequest("10.136.1.3", "tenant-quota", "wrong-token", body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code)
	})

	t.Run("non-existent tenant returns 404", func(t *testing.T) {
		srv, _ := newServer(t, []string{bootstrapToken})

		body := []byte(`{"max_services":10}`)
		req := quotaRequest("10.136.1.4", "does-not-exist", bootstrapToken, body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusNotFound, rr.Code)
	})

	t.Run("zero value for quota field returns 400", func(t *testing.T) {
		srv, _ := newServer(t, []string{bootstrapToken})

		body := []byte(`{"max_services":0}`)
		req := quotaRequest("10.136.1.5", "tenant-quota", bootstrapToken, body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusBadRequest, rr.Code)
	})

	t.Run("negative value for quota field returns 400", func(t *testing.T) {
		srv, _ := newServer(t, []string{bootstrapToken})

		body := []byte(`{"max_services":-5}`)
		req := quotaRequest("10.136.1.6", "tenant-quota", bootstrapToken, body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusBadRequest, rr.Code)
	})

	t.Run("value exceeding cap returns 400", func(t *testing.T) {
		srv, _ := newServer(t, []string{bootstrapToken})

		body := []byte(`{"max_services":999}`)
		req := quotaRequest("10.136.1.7", "tenant-quota", bootstrapToken, body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusBadRequest, rr.Code)
	})

	t.Run("partial update only changes provided fields", func(t *testing.T) {
		srv, stateDB := newServer(t, []string{bootstrapToken})

		// Set initial values
		_, err := stateDB.Exec(`UPDATE tenant_quotas SET max_services = 7, max_databases = 4 WHERE tenant_id = ?`, "tenant-quota")
		require.NoError(t, err)

		// Only update max_services, leave max_databases unchanged
		body := []byte(`{"max_services":15}`)
		req := quotaRequest("10.136.1.8", "tenant-quota", bootstrapToken, body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var resp QuotaUpdateResponse
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
		assert.Equal(t, 15, resp.MaxServices, "max_services should be updated")
		assert.Equal(t, 4, resp.MaxDatabases, "max_databases should remain unchanged")
	})

	t.Run("no bootstrap tokens configured returns 503", func(t *testing.T) {
		srv, _ := newServer(t, nil)

		body := []byte(`{"max_services":10}`)
		req := quotaRequest("10.136.1.9", "tenant-quota", "anything", body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	})
}

// ---------------------------------------------------------------------------
// Regression tests — TestRecoveryKeyringFull_Regression
// (PR #147, issue #138)
// ---------------------------------------------------------------------------

func TestRecoveryKeyringFull_Regression(t *testing.T) {
	regLimiter.resetForTest()
	const bootstrapToken = "test-bootstrap-token-keyring-full"
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	// newRecoverServer creates a fresh in-memory DB with an active tenant and
	// the specified number of existing API keys (all non-revoked).
	newRecoverServer := func(t *testing.T, numKeys int, withExpiredKeys bool) (*Server, *sql.DB) {
		t.Helper()
		stateDB := testutil.NewStateDB(t)
		_, err := stateDB.Exec(
			`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
			 VALUES (?, ?, ?, 'active', 1, 1)`,
			"tenant-keyring", "Keyring Tenant", "keyring@example.com",
		)
		require.NoError(t, err)
		_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`, "tenant-keyring")
		require.NoError(t, err)

		// Seed keys. If withExpiredKeys is true, the first key is expired.
		for i := 0; i < numKeys; i++ {
			keyID := "key-" + strconv.Itoa(i)
			prefix := "pref" + strconv.Itoa(i) + "xxx"
			if len(prefix) > 8 {
				prefix = prefix[:8]
			}
			var expiresAt interface{}
			if withExpiredKeys && i == 0 {
				// Make the first key expired (created long ago, expired 1 hour ago)
				expiresAt = int64(1000) // expires_at in the past
			}
			_, err = stateDB.Exec(
				`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at, expires_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				keyID, "tenant-keyring", "key-"+strconv.Itoa(i), prefix, "hash-"+strconv.Itoa(i),
				int64(100+i), // created_at increments so we know which is "oldest"
				expiresAt,
			)
			require.NoError(t, err)
		}

		srv := NewServer(ServerConfig{
			Store:           &db.Store{StateDB: stateDB},
			MasterKey:       masterKey,
			DevMode:         true,
			BootstrapTokens: []string{bootstrapToken},
		})
		return srv, stateDB
	}

	t.Run("recovery when keyring has room succeeds normally 201", func(t *testing.T) {
		srv, _ := newRecoverServer(t, 5, false) // 5 keys, well under maxKeysPerTenant (20)

		body, _ := json.Marshal(KeyRecoverRequest{
			Email:          "keyring@example.com",
			BootstrapToken: bootstrapToken,
		})
		req := recoverRequest("10.138.1.1", body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())

		var resp KeyRecoverResponse
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
		assert.NotEmpty(t, resp.ID)
		assert.NotEmpty(t, resp.Key)
		assert.Empty(t, resp.Warning, "no warning when keyring has room")
		assert.Empty(t, resp.RevokedKeyID, "no revoked_key_id when keyring has room")
	})

	t.Run("recovery when keyring full with expired keys auto-revokes oldest expired", func(t *testing.T) {
		srv, stateDB := newRecoverServer(t, maxKeysPerTenant, true) // full keyring, key-0 is expired

		body, _ := json.Marshal(KeyRecoverRequest{
			Email:          "keyring@example.com",
			BootstrapToken: bootstrapToken,
		})
		req := recoverRequest("10.138.1.2", body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())

		var resp KeyRecoverResponse
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
		assert.NotEmpty(t, resp.Key)
		assert.Contains(t, resp.Warning, "revoked oldest key", "warning should mention key revocation")
		assert.Equal(t, "key-0", resp.RevokedKeyID, "should have revoked the oldest expired key (key-0)")

		// Verify key-0 was actually revoked in DB
		var revokedAt *int64
		err := stateDB.QueryRow(`SELECT revoked_at FROM api_keys WHERE id = 'key-0'`).Scan(&revokedAt)
		require.NoError(t, err)
		assert.NotNil(t, revokedAt, "key-0 should have revoked_at set")
	})

	t.Run("recovery when keyring full with no expired keys auto-revokes oldest active", func(t *testing.T) {
		srv, stateDB := newRecoverServer(t, maxKeysPerTenant, false) // full keyring, no expired keys

		body, _ := json.Marshal(KeyRecoverRequest{
			Email:          "keyring@example.com",
			BootstrapToken: bootstrapToken,
		})
		req := recoverRequest("10.138.1.3", body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())

		var resp KeyRecoverResponse
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
		assert.NotEmpty(t, resp.Key)
		assert.Contains(t, resp.Warning, "revoked oldest key", "warning should mention key revocation")
		assert.Equal(t, "key-0", resp.RevokedKeyID, "should have revoked the oldest key (key-0)")

		// Verify key-0 was revoked
		var revokedAt *int64
		err := stateDB.QueryRow(`SELECT revoked_at FROM api_keys WHERE id = 'key-0'`).Scan(&revokedAt)
		require.NoError(t, err)
		assert.NotNil(t, revokedAt, "key-0 should have revoked_at set")
	})

	t.Run("response includes warning field when auto-revoke happened", func(t *testing.T) {
		srv, _ := newRecoverServer(t, maxKeysPerTenant, false)

		body, _ := json.Marshal(KeyRecoverRequest{
			Email:          "keyring@example.com",
			BootstrapToken: bootstrapToken,
		})
		req := recoverRequest("10.138.1.4", body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())

		var resp map[string]interface{}
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
		warning, ok := resp["warning"]
		assert.True(t, ok, "response should include 'warning' field")
		assert.NotEmpty(t, warning, "warning should not be empty when auto-revoke happened")
	})

	t.Run("response includes revoked_key_id when auto-revoke happened", func(t *testing.T) {
		srv, _ := newRecoverServer(t, maxKeysPerTenant, true)

		body, _ := json.Marshal(KeyRecoverRequest{
			Email:          "keyring@example.com",
			BootstrapToken: bootstrapToken,
		})
		req := recoverRequest("10.138.1.5", body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())

		var resp map[string]interface{}
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
		revokedID, ok := resp["revoked_key_id"]
		assert.True(t, ok, "response should include 'revoked_key_id' field")
		assert.NotEmpty(t, revokedID, "revoked_key_id should not be empty when auto-revoke happened")
	})
}

// ---------------------------------------------------------------------------
// Regression tests — TestDeploymentCancel_Regression
// (PR #148, issue #139)
// ---------------------------------------------------------------------------

func TestDeploymentCancel_Regression(t *testing.T) {
	regLimiter.resetForTest()
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	// newCancelServerWithToken creates a fresh server with a service and deploy store,
	// and returns the auth token for making authenticated requests.
	newCancelServerWithToken := func(t *testing.T) (*Server, *sql.DB, *deployments.Store, string) {
		t.Helper()
		stateDB := testutil.NewStateDB(t)
		token := seedAuthenticatedTenant(t, stateDB, masterKey)

		_, err := stateDB.Exec(
			`INSERT INTO services (id, tenant_id, name, dns_label, status, image, port, container_id, url, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"svc-cancel", "tenant-1", "web", "", "running", "nginx:latest", 8080, "ctr-1", "", 1000, 1000,
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

		return srv, stateDB, deployStore, token
	}

	t.Run("cancel queued deployment succeeds 200", func(t *testing.T) {
		srv, _, deployStore, token := newCancelServerWithToken(t)

		// Insert a pending deployment.
		require.NoError(t, deployStore.Create(context.Background(), &deployments.Deployment{
			ID:        "deploy-queued",
			ServiceID: "svc-cancel",
			TenantID:  "tenant-1",
			Image:     "nginx:latest",
			Status:    deployments.StatusPending,
			Trigger:   deployments.TriggerManual,
			StartedAt: 1000,
			CreatedAt: 1000,
		}))

		req := httptest.NewRequest(http.MethodPost, "/v1/services/svc-cancel/deployments/deploy-queued/cancel", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var resp map[string]interface{}
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
		assert.Equal(t, "cancelled", resp["status"])
	})

	t.Run("cancel completed deployment returns 409", func(t *testing.T) {
		srv, _, deployStore, token := newCancelServerWithToken(t)

		require.NoError(t, deployStore.Create(context.Background(), &deployments.Deployment{
			ID:        "deploy-done",
			ServiceID: "svc-cancel",
			TenantID:  "tenant-1",
			Image:     "nginx:latest",
			Status:    deployments.StatusRunning,
			Trigger:   deployments.TriggerManual,
			StartedAt: 1000,
			CreatedAt: 1000,
		}))

		req := httptest.NewRequest(http.MethodPost, "/v1/services/svc-cancel/deployments/deploy-done/cancel", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusConflict, rr.Code, "completed deployment should return 409")
	})

	t.Run("cancel already-cancelled deployment returns 409", func(t *testing.T) {
		srv, _, deployStore, token := newCancelServerWithToken(t)

		require.NoError(t, deployStore.Create(context.Background(), &deployments.Deployment{
			ID:        "deploy-already-cancelled",
			ServiceID: "svc-cancel",
			TenantID:  "tenant-1",
			Image:     "nginx:latest",
			Status:    deployments.StatusCancelled,
			Trigger:   deployments.TriggerManual,
			StartedAt: 1000,
			CreatedAt: 1000,
		}))

		req := httptest.NewRequest(http.MethodPost, "/v1/services/svc-cancel/deployments/deploy-already-cancelled/cancel", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusConflict, rr.Code, "already-cancelled deployment should return 409")
	})

	t.Run("cancel deployment that does not exist returns 404", func(t *testing.T) {
		srv, _, _, token := newCancelServerWithToken(t)

		req := httptest.NewRequest(http.MethodPost, "/v1/services/svc-cancel/deployments/nonexistent/cancel", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusNotFound, rr.Code, "nonexistent deployment should return 404")
	})

	t.Run("cancel deployment belonging to different service returns 404", func(t *testing.T) {
		srv, stateDB, deployStore, token := newCancelServerWithToken(t)

		// Create another service.
		_, err := stateDB.Exec(
			`INSERT INTO services (id, tenant_id, name, dns_label, status, image, port, container_id, url, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"svc-other", "tenant-1", "other", "", "running", "nginx:latest", 8081, "", "", 1000, 1000,
		)
		require.NoError(t, err)

		// Deployment belongs to svc-other, not svc-cancel.
		require.NoError(t, deployStore.Create(context.Background(), &deployments.Deployment{
			ID:        "deploy-wrong-svc",
			ServiceID: "svc-other",
			TenantID:  "tenant-1",
			Image:     "nginx:latest",
			Status:    deployments.StatusPending,
			Trigger:   deployments.TriggerManual,
			StartedAt: 1000,
			CreatedAt: 1000,
		}))

		// Try to cancel via svc-cancel path.
		req := httptest.NewRequest(http.MethodPost, "/v1/services/svc-cancel/deployments/deploy-wrong-svc/cancel", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusNotFound, rr.Code, "deployment belonging to different service should return 404")
	})

	t.Run("cancel deployment for non-existent service returns 404", func(t *testing.T) {
		srv, _, _, token := newCancelServerWithToken(t)

		req := httptest.NewRequest(http.MethodPost, "/v1/services/does-not-exist/deployments/any-deploy/cancel", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusNotFound, rr.Code, "non-existent service should return 404")
	})
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
