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

// seedActivityData populates a tenant with services, builds, databases, and
// API keys so that the activity endpoint returns a variety of event types.
// Returns the auth token and server.
func seedActivityData(t *testing.T) (string, *Server) {
	t.Helper()
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	// Service: svc-1 created at 10, updated (circuit_open) at 40
	_, err := stateDB.Exec(`INSERT INTO services (id, tenant_id, name, status, image, port, last_error, crash_count, circuit_open, created_at, updated_at) VALUES ('svc-1', 'tenant-1', 'web', 'failed', 'nginx:latest', 8080, 'deploy queue full', 2, 1, 10, 40)`)
	require.NoError(t, err)
	// Service: svc-2 created at 50, running at 60
	_, err = stateDB.Exec(`INSERT INTO services (id, tenant_id, name, status, image, port, last_error, crash_count, circuit_open, created_at, updated_at) VALUES ('svc-2', 'tenant-1', 'api', 'running', 'node:latest', 3000, '', 0, 0, 50, 60)`)
	require.NoError(t, err)
	// Build for svc-1: created at 20, started at 21, finished (failed) at 30
	_, err = stateDB.Exec(`INSERT INTO builds (id, service_id, tenant_id, status, source_type, source_url, source_ref, image, created_at, started_at, finished_at) VALUES ('b-1', 'svc-1', 'tenant-1', 'failed', 'git', 'https://github.com/example/repo', 'main', '127.0.0.1:5000/ah/image', 20, 21, 30)`)
	require.NoError(t, err)
	// Database: created at 12, ready at 35
	_, err = stateDB.Exec(`INSERT INTO databases (id, tenant_id, name, type, status, host, port, db_name, username, password_encrypted, connection_string_encrypted, volume_name, created_at, updated_at) VALUES ('db-1', 'tenant-1', 'cache', 'redis', 'ready', '127.0.0.1', 6379, '', '', 'enc', 'enc', 'vol-1', 12, 35)`)
	require.NoError(t, err)
	// Extra API key: created at 15, revoked at 25
	_, err = stateDB.Exec(`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at, revoked_at) VALUES ('key-extra', 'tenant-1', 'dashboard', 'key-extr', 'hash', 15, 25)`)
	require.NoError(t, err)

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
	})
	return token, srv
}

func TestActivityList_FilterByResourceType(t *testing.T) {
	token, srv := seedActivityData(t)

	tests := []struct {
		name          string
		resourceType  string
		wantTypes     []string // all events should have one of these types
		forbidTypes   []string // no event should have these types
		wantNonEmpty  bool
	}{
		{
			name:         "service only",
			resourceType: "service",
			wantTypes:    []string{"service"},
			forbidTypes:  []string{"build", "database", "api_key", "tenant"},
			wantNonEmpty: true,
		},
		{
			name:         "build only",
			resourceType: "build",
			wantTypes:    []string{"build"},
			forbidTypes:  []string{"service", "database", "api_key", "tenant"},
			wantNonEmpty: true,
		},
		{
			name:         "database only",
			resourceType: "database",
			wantTypes:    []string{"database"},
			forbidTypes:  []string{"service", "build", "api_key", "tenant"},
			wantNonEmpty: true,
		},
		{
			name:         "api_key only",
			resourceType: "api_key",
			wantTypes:    []string{"api_key"},
			forbidTypes:  []string{"service", "build", "database", "tenant"},
			wantNonEmpty: true,
		},
		{
			name:         "tenant only",
			resourceType: "tenant",
			wantTypes:    []string{"tenant"},
			forbidTypes:  []string{"service", "build", "database", "api_key"},
			wantNonEmpty: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/activity?resource_type="+tt.resourceType, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code)

			var events []ActivityEvent
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &events))
			if tt.wantNonEmpty {
				require.NotEmpty(t, events)
			}
			for _, e := range events {
				assert.Contains(t, tt.wantTypes, e.ResourceType, "unexpected resource_type %q", e.ResourceType)
				for _, ft := range tt.forbidTypes {
					assert.NotEqual(t, ft, e.ResourceType, "should not contain resource_type %q", ft)
				}
			}
		})
	}
}

func TestActivityList_FilterByAction(t *testing.T) {
	token, srv := seedActivityData(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/activity?action=build.failed", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var events []ActivityEvent
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &events))
	require.Len(t, events, 1)
	assert.Equal(t, "build.failed", events[0].Action)
}

func TestActivityList_FilterByServiceID(t *testing.T) {
	token, srv := seedActivityData(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/activity?service_id=svc-1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var events []ActivityEvent
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &events))
	require.NotEmpty(t, events)

	// All events should relate to svc-1 (either service events or build events for svc-1).
	for _, e := range events {
		assert.True(t, e.ResourceType == "service" || e.ResourceType == "build",
			"service_id filter should only return service/build events, got %q", e.ResourceType)
		if e.ResourceType == "service" {
			assert.Equal(t, "svc-1", e.ResourceID)
		}
		// Build events have ServiceID set.
		if e.ResourceType == "build" {
			assert.Equal(t, "svc-1", e.ServiceID)
		}
	}

	// Must not include events for svc-2.
	for _, e := range events {
		if e.ResourceType == "service" {
			assert.NotEqual(t, "svc-2", e.ResourceID)
		}
	}
}

func TestActivityList_FilterCombined(t *testing.T) {
	token, srv := seedActivityData(t)

	// Combine resource_type=service with action=service.circuit_open.
	req := httptest.NewRequest(http.MethodGet, "/v1/activity?resource_type=service&action=service.circuit_open", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var events []ActivityEvent
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &events))
	require.Len(t, events, 1)
	assert.Equal(t, "service", events[0].ResourceType)
	assert.Equal(t, "service.circuit_open", events[0].Action)
}

func TestActivityList_FilterSince(t *testing.T) {
	token, srv := seedActivityData(t)

	// Only events at or after timestamp 30.
	req := httptest.NewRequest(http.MethodGet, "/v1/activity?since=30", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var events []ActivityEvent
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &events))
	require.NotEmpty(t, events)

	for _, e := range events {
		assert.GreaterOrEqual(t, e.CreatedAt, int64(30), "event %s at %d should be >= 30", e.Action, e.CreatedAt)
	}
}

func TestActivityList_FilterOffset(t *testing.T) {
	token, srv := seedActivityData(t)

	// Get all events first.
	req := httptest.NewRequest(http.MethodGet, "/v1/activity?limit=200", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var allEvents []ActivityEvent
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &allEvents))
	total := len(allEvents)
	require.Greater(t, total, 2, "need at least 3 events for offset test")

	// Get events with offset=2.
	req = httptest.NewRequest(http.MethodGet, "/v1/activity?limit=200&offset=2", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var offsetEvents []ActivityEvent
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &offsetEvents))
	assert.Len(t, offsetEvents, total-2)

	// The offset events should match the tail of all events.
	for i, e := range offsetEvents {
		assert.Equal(t, allEvents[i+2].ID, e.ID)
	}
}

func TestActivityList_NoFilters_UnchangedBehavior(t *testing.T) {
	token, srv := seedActivityData(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/activity?limit=200", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var events []ActivityEvent
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &events))
	require.NotEmpty(t, events)

	// Should contain events from all resource types.
	types := map[string]bool{}
	for _, e := range events {
		types[e.ResourceType] = true
	}
	assert.True(t, types["service"], "should have service events")
	assert.True(t, types["build"], "should have build events")
	assert.True(t, types["database"], "should have database events")
	assert.True(t, types["api_key"], "should have api_key events")
	assert.True(t, types["tenant"], "should have tenant events")

	// Verify descending timestamp order.
	for i := 1; i < len(events); i++ {
		assert.GreaterOrEqual(t, events[i-1].CreatedAt, events[i].CreatedAt, "events should be in descending order")
	}
}

func TestActivityList_InvalidParams(t *testing.T) {
	token, srv := seedActivityData(t)

	tests := []struct {
		name string
		url  string
		want string
	}{
		{"bad since", "/v1/activity?since=abc", "since must be a non-negative unix timestamp"},
		{"negative since", "/v1/activity?since=-1", "since must be a non-negative unix timestamp"},
		{"bad offset", "/v1/activity?offset=abc", "offset must be a non-negative integer"},
		{"negative offset", "/v1/activity?offset=-1", "offset must be a non-negative integer"},
		{"bad limit", "/v1/activity?limit=0", "limit must be between 1 and 200"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.url, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), tt.want)
		})
	}
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
