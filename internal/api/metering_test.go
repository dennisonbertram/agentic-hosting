package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/metering"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTenantMetrics_EmptyData(t *testing.T) {
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

	req := httptest.NewRequest(http.MethodGet, "/v1/tenant/usage/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "unexpected body: %s", rr.Body.String())

	var metrics []metering.Metric
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&metrics))
	assert.Empty(t, metrics, "empty metering database should return empty array")
}

func TestTenantMetrics_DefaultParameters(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	meteringDB := testutil.NewMeteringDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	// Insert a recent event (within last 24h)
	now := time.Now().UTC()
	_, err := meteringDB.Exec(
		`INSERT INTO usage_events (id, tenant_id, service_id, event_type, cpu_seconds, memory_mb_seconds, network_ingress_bytes, network_egress_bytes, recorded_at)
		 VALUES (?, ?, ?, 'sample', 10.0, 100.0, 1000, 2000, ?)`,
		"evt-1", "tenant-1", "svc-1", now.Add(-1*time.Hour).Unix(),
	)
	require.NoError(t, err)

	srv := NewServer(ServerConfig{
		Store:         &db.Store{StateDB: stateDB, MeteringDB: meteringDB},
		MasterKey:     masterKey,
		DevMode:       true,
		MeteringStore: metering.NewStore(meteringDB),
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/tenant/usage/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "unexpected body: %s", rr.Body.String())

	var metrics []metering.Metric
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&metrics))
	assert.NotEmpty(t, metrics, "should return data for events within the default 24h window")
}

func TestTenantMetrics_InvalidPeriod(t *testing.T) {
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

	req := httptest.NewRequest(http.MethodGet, "/v1/tenant/usage/metrics?period=weekly", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "period must be")
}

func TestTenantMetrics_InvalidSince(t *testing.T) {
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

	req := httptest.NewRequest(http.MethodGet, "/v1/tenant/usage/metrics?since=not-a-date", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "since must be a valid RFC3339")
}

func TestTenantMetrics_SinceAfterUntil(t *testing.T) {
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

	req := httptest.NewRequest(http.MethodGet,
		"/v1/tenant/usage/metrics?since=2026-03-22T00:00:00Z&until=2026-03-20T00:00:00Z", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "since must be before until")
}

func TestTenantMetrics_HourlyPeriod(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	meteringDB := testutil.NewMeteringDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	hour1 := time.Date(2026, 3, 20, 10, 15, 0, 0, time.UTC).Unix()
	hour2 := time.Date(2026, 3, 20, 11, 30, 0, 0, time.UTC).Unix()
	for i, ts := range []int64{hour1, hour2} {
		_, err := meteringDB.Exec(
			`INSERT INTO usage_events (id, tenant_id, service_id, event_type, cpu_seconds, memory_mb_seconds, network_ingress_bytes, network_egress_bytes, recorded_at)
			 VALUES (?, ?, ?, 'sample', 5.0, 50.0, 100, 200, ?)`,
			"evt-"+string(rune('a'+i)), "tenant-1", "svc-1", ts,
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

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "unexpected body: %s", rr.Body.String())

	var metrics []metering.Metric
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&metrics))
	assert.Len(t, metrics, 2, "should have two hourly buckets")
}

func TestTenantMetrics_ServiceIDFilter(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	meteringDB := testutil.NewMeteringDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	ts := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC).Unix()
	_, err := meteringDB.Exec(
		`INSERT INTO usage_events (id, tenant_id, service_id, event_type, cpu_seconds, memory_mb_seconds, network_ingress_bytes, network_egress_bytes, recorded_at)
		 VALUES (?, ?, ?, 'sample', 10.0, 100.0, 1000, 2000, ?)`,
		"evt-1", "tenant-1", "svc-1", ts,
	)
	require.NoError(t, err)
	_, err = meteringDB.Exec(
		`INSERT INTO usage_events (id, tenant_id, service_id, event_type, cpu_seconds, memory_mb_seconds, network_ingress_bytes, network_egress_bytes, recorded_at)
		 VALUES (?, ?, ?, 'sample', 99.0, 999.0, 9999, 9999, ?)`,
		"evt-2", "tenant-1", "svc-other", ts,
	)
	require.NoError(t, err)

	srv := NewServer(ServerConfig{
		Store:         &db.Store{StateDB: stateDB, MeteringDB: meteringDB},
		MasterKey:     masterKey,
		DevMode:       true,
		MeteringStore: metering.NewStore(meteringDB),
	})

	req := httptest.NewRequest(http.MethodGet,
		"/v1/tenant/usage/metrics?service_id=svc-1&since=2026-03-20T00:00:00Z&until=2026-03-21T00:00:00Z", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)

	var metrics []metering.Metric
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&metrics))
	require.Len(t, metrics, 1)
	assert.InDelta(t, 10.0, metrics[0].CPUSeconds, 0.01)
}

func TestTenantMetrics_Unauthenticated(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	meteringDB := testutil.NewMeteringDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	srv := NewServer(ServerConfig{
		Store:         &db.Store{StateDB: stateDB, MeteringDB: meteringDB},
		MasterKey:     masterKey,
		DevMode:       true,
		MeteringStore: metering.NewStore(meteringDB),
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/tenant/usage/metrics", nil)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusUnauthorized, rr.Code, "unauthenticated request should return 401")
}

func TestServiceMetrics_EmptyData(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	meteringDB := testutil.NewMeteringDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	// Create a service so the ownership check passes
	_, err := stateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, container_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-1", "tenant-1", "web", "running", "nginx:latest", 8080, "", 1000, 1000,
	)
	require.NoError(t, err)

	srv := NewServer(ServerConfig{
		Store:         &db.Store{StateDB: stateDB, MeteringDB: meteringDB},
		MasterKey:     masterKey,
		DevMode:       true,
		MeteringStore: metering.NewStore(meteringDB),
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/services/svc-1/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "unexpected body: %s", rr.Body.String())

	var metrics []metering.Metric
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&metrics))
	assert.Empty(t, metrics)
}

func TestServiceMetrics_NotFound(t *testing.T) {
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

	req := httptest.NewRequest(http.MethodGet, "/v1/services/nonexistent/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code, "service not found should return 404")
}

func TestServiceMetrics_TenantIsolation(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	meteringDB := testutil.NewMeteringDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	// Create a service owned by a different tenant
	_, err := stateDB.Exec(
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at) VALUES (?, ?, ?, 'active', 1, 1)`,
		"tenant-2", "Other", "other@example.com",
	)
	require.NoError(t, err)
	_, err = stateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, container_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-other", "tenant-2", "web", "running", "nginx:latest", 8080, "", 1000, 1000,
	)
	require.NoError(t, err)

	srv := NewServer(ServerConfig{
		Store:         &db.Store{StateDB: stateDB, MeteringDB: meteringDB},
		MasterKey:     masterKey,
		DevMode:       true,
		MeteringStore: metering.NewStore(meteringDB),
	})

	// Tenant-1 tries to access tenant-2's service metrics
	req := httptest.NewRequest(http.MethodGet, "/v1/services/svc-other/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusNotFound, rr.Code,
		"accessing another tenant's service metrics should return 404")
}

func TestServiceMetrics_InvalidPeriod(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	meteringDB := testutil.NewMeteringDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	_, err := stateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, container_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-1", "tenant-1", "web", "running", "nginx:latest", 8080, "", 1000, 1000,
	)
	require.NoError(t, err)

	srv := NewServer(ServerConfig{
		Store:         &db.Store{StateDB: stateDB, MeteringDB: meteringDB},
		MasterKey:     masterKey,
		DevMode:       true,
		MeteringStore: metering.NewStore(meteringDB),
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/services/svc-1/metrics?period=yearly", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "period must be")
}

func TestServiceMetrics_WithData(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	meteringDB := testutil.NewMeteringDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	_, err := stateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, container_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-1", "tenant-1", "web", "running", "nginx:latest", 8080, "", 1000, 1000,
	)
	require.NoError(t, err)

	ts := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC).Unix()
	_, err = meteringDB.Exec(
		`INSERT INTO usage_events (id, tenant_id, service_id, event_type, cpu_seconds, memory_mb_seconds, network_ingress_bytes, network_egress_bytes, recorded_at)
		 VALUES (?, ?, ?, 'sample', 15.5, 128.0, 5000, 3000, ?)`,
		"evt-1", "tenant-1", "svc-1", ts,
	)
	require.NoError(t, err)

	srv := NewServer(ServerConfig{
		Store:         &db.Store{StateDB: stateDB, MeteringDB: meteringDB},
		MasterKey:     masterKey,
		DevMode:       true,
		MeteringStore: metering.NewStore(meteringDB),
	})

	req := httptest.NewRequest(http.MethodGet,
		"/v1/services/svc-1/metrics?since=2026-03-20T00:00:00Z&until=2026-03-21T00:00:00Z", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "unexpected body: %s", rr.Body.String())

	var metrics []metering.Metric
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&metrics))
	require.Len(t, metrics, 1)
	assert.InDelta(t, 15.5, metrics[0].CPUSeconds, 0.01)
	assert.InDelta(t, 128.0, metrics[0].MemoryMBAvg, 0.01)
	assert.Equal(t, int64(5000), metrics[0].NetworkRxBytes)
	assert.Equal(t, int64(3000), metrics[0].NetworkTxBytes)
}

func TestTenantMetrics_NoMeteringStore(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	// No MeteringStore configured
	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/tenant/usage/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	assert.Contains(t, rr.Body.String(), "metering not available")
}
