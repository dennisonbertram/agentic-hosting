package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/builder"
	"github.com/dennisonbertram/agentic-hosting/internal/builds"
	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type apiStubBuilder struct{}

func (apiStubBuilder) Build(ctx context.Context, req builder.BuildRequest, logCb func(string)) error {
	return nil
}

func (apiStubBuilder) CancelBuild(buildID string) error {
	return nil
}

func TestTenantUsage_ReturnsQuotaCounts(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	_, err := stateDB.Exec(`UPDATE tenant_quotas SET max_services = 9, max_databases = 4, max_memory_mb = 4096, max_cpu_cores = 3.5, max_disk_gb = 80, api_rate_limit = 250 WHERE tenant_id = ?`, "tenant-1")
	require.NoError(t, err)
	_, err = stateDB.Exec(`INSERT INTO services (id, tenant_id, name, status, image, port, created_at, updated_at) VALUES ('svc-1', 'tenant-1', 'web', 'running', 'nginx:latest', 8080, 10, 20)`)
	require.NoError(t, err)
	_, err = stateDB.Exec(`INSERT INTO databases (id, tenant_id, name, type, status, host, port, db_name, username, password_encrypted, connection_string_encrypted, volume_name, created_at, updated_at) VALUES ('db-1', 'tenant-1', 'main', 'postgres', 'ready', '127.0.0.1', 5432, 'ah', 'ah', 'enc', 'enc', 'vol-1', 10, 20)`)
	require.NoError(t, err)

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/tenant/usage", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp TenantUsageResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, TenantUsageBucket{Used: 1, Max: 9}, resp.Services)
	assert.Equal(t, TenantUsageBucket{Used: 1, Max: 4}, resp.Databases)
	assert.Equal(t, TenantUsageBucket{Used: 1, Max: maxKeysPerTenant}, resp.APIKeys)
	assert.Equal(t, 4096, resp.MemoryMB)
	assert.Equal(t, 3.5, resp.CPUCores)
	assert.Equal(t, 80, resp.DiskGB)
	assert.Equal(t, 250, resp.RateLimit)
}

func TestBuildListAll_ReturnsTenantBuilds(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	_, err := stateDB.Exec(`INSERT INTO services (id, tenant_id, name, status, image, port, created_at, updated_at) VALUES ('svc-1', 'tenant-1', 'web', 'running', 'nginx:latest', 8080, 10, 20)`)
	require.NoError(t, err)
	_, err = stateDB.Exec(`INSERT INTO builds (id, service_id, tenant_id, status, source_type, source_url, source_ref, image, created_at, started_at, finished_at) VALUES ('b-1', 'svc-1', 'tenant-1', 'succeeded', 'git', 'https://github.com/example/repo', 'main', '127.0.0.1:5000/ah/image', 10, 11, 20)`)
	require.NoError(t, err)

	buildMgr := builds.NewManager(stateDB, apiStubBuilder{}, nil)
	srv := NewServer(ServerConfig{
		Store:        &db.Store{StateDB: stateDB},
		MasterKey:    masterKey,
		DevMode:      true,
		BuildManager: buildMgr,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/builds?limit=20", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var result []builds.Build
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	require.Len(t, result, 1)
	assert.Equal(t, "b-1", result[0].ID)
	assert.Equal(t, "web", result[0].ServiceName)
}

func TestActivityList_ReturnsRecentEvents(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	_, err := stateDB.Exec(`INSERT INTO services (id, tenant_id, name, status, image, port, last_error, crash_count, circuit_open, created_at, updated_at) VALUES ('svc-1', 'tenant-1', 'web', 'failed', 'nginx:latest', 8080, 'deploy queue full', 2, 1, 10, 40)`)
	require.NoError(t, err)
	_, err = stateDB.Exec(`INSERT INTO builds (id, service_id, tenant_id, status, source_type, source_url, source_ref, image, created_at, started_at, finished_at) VALUES ('b-1', 'svc-1', 'tenant-1', 'failed', 'git', 'https://github.com/example/repo', 'main', '127.0.0.1:5000/ah/image', 20, 21, 30)`)
	require.NoError(t, err)
	_, err = stateDB.Exec(`INSERT INTO databases (id, tenant_id, name, type, status, host, port, db_name, username, password_encrypted, connection_string_encrypted, volume_name, created_at, updated_at) VALUES ('db-1', 'tenant-1', 'cache', 'redis', 'ready', '127.0.0.1', 6379, '', '', 'enc', 'enc', 'vol-1', 12, 35)`)
	require.NoError(t, err)
	_, err = stateDB.Exec(`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at, revoked_at) VALUES ('key-extra', 'tenant-1', 'dashboard', 'key-extr', 'hash', 15, 25)`)
	require.NoError(t, err)

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/activity?limit=20", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var events []ActivityEvent
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &events))
	require.NotEmpty(t, events)

	actions := make([]string, 0, len(events))
	for _, event := range events {
		actions = append(actions, event.Action)
	}
	assert.Contains(t, actions, "service.circuit_open")
	assert.Contains(t, actions, "build.failed")
	assert.Contains(t, actions, "database.ready")
	assert.Contains(t, actions, "api_key.revoked")
}

func TestBuildLogsStream_Returns404WhenBuildNotFound(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	buildMgr := builds.NewManager(stateDB, apiStubBuilder{}, nil)
	srv := NewServer(ServerConfig{
		Store:        &db.Store{StateDB: stateDB},
		MasterKey:    masterKey,
		DevMode:      true,
		BuildManager: buildMgr,
	})

	// Request streaming logs for a build that does not exist.
	req := httptest.NewRequest(http.MethodGet, "/v1/services/svc-1/builds/nonexistent/logs?follow=true", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	// Must receive 404 with a JSON error body, NOT a 200 with a silent failure.
	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "build not found")
}

func TestBuildLogsStream_Returns404WhenBuildBelongsToDifferentTenant(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	// Seed a second tenant with a service and build that belongs to it.
	_, err := stateDB.Exec(`INSERT INTO tenants (id, name, email, status, created_at, updated_at) VALUES ('tenant-other', 'Other', 'other@example.com', 'active', 1, 1)`)
	require.NoError(t, err)
	_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES ('tenant-other')`)
	require.NoError(t, err)
	_, err = stateDB.Exec(`INSERT INTO services (id, tenant_id, name, status, image, port, created_at, updated_at) VALUES ('svc-other', 'tenant-other', 'web', 'running', 'nginx:latest', 8080, 10, 20)`)
	require.NoError(t, err)
	_, err = stateDB.Exec(`INSERT INTO builds (id, service_id, tenant_id, status, source_type, source_url, source_ref, image, created_at) VALUES ('b-other', 'svc-other', 'tenant-other', 'running', 'git', 'https://github.com/example/repo', 'main', 'img', 10)`)
	require.NoError(t, err)

	buildMgr := builds.NewManager(stateDB, apiStubBuilder{}, nil)
	srv := NewServer(ServerConfig{
		Store:        &db.Store{StateDB: stateDB},
		MasterKey:    masterKey,
		DevMode:      true,
		BuildManager: buildMgr,
	})

	// tenant-1 requests logs for a build owned by tenant-other.
	req := httptest.NewRequest(http.MethodGet, "/v1/services/svc-other/builds/b-other/logs?follow=true", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	// GetBuild scopes by tenantID, so this should return 404 (not a cross-tenant leak).
	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "build not found")
}

func TestServiceLogsRoute_IsRegistered(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/services/svc-1/logs", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}
