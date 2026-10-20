package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/metering"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var reviewMasterKey = []byte("0123456789abcdef0123456789abcdef")

// doReq sends an authenticated request through srv and returns the recorder.
func doReq(t *testing.T, srv http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// ---------------------------------------------------------------------------
// API keys
// ---------------------------------------------------------------------------

func TestAPIKeys_CreateListRevoke(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	token := seedAuthenticatedTenant(t, stateDB, reviewMasterKey)
	srv := NewServer(ServerConfig{Store: &db.Store{StateDB: stateDB}, MasterKey: reviewMasterKey, DevMode: true})

	rr := doReq(t, srv, http.MethodPost, "/v1/auth/keys", token, `{"name":"ci","expires_in":3600}`)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	var created CreateKeyResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&created))
	assert.Equal(t, "ci", created.Name)
	assert.Equal(t, created.ID[:8], created.Prefix, "prefix comes from the key ID, not the secret")
	assert.True(t, strings.HasPrefix(created.APIKey, created.ID+"."))
	require.NotNil(t, created.Expires)
	assert.InDelta(t, time.Now().Unix()+3600, *created.Expires, 5)

	// The new key authenticates.
	rr = doReq(t, srv, http.MethodGet, "/v1/auth/keys", created.APIKey, "")
	require.Equal(t, http.StatusOK, rr.Code)
	var keys []KeyInfo
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&keys))
	assert.Len(t, keys, 2)

	// Revoke it; it stops working immediately (auth cache is invalidated).
	rr = doReq(t, srv, http.MethodDelete, "/v1/auth/keys/"+created.ID, token, "")
	assert.Equal(t, http.StatusNoContent, rr.Code)
	rr = doReq(t, srv, http.MethodGet, "/v1/auth/keys", created.APIKey, "")
	assert.Equal(t, http.StatusUnauthorized, rr.Code)

	// Revoking again is a 404.
	rr = doReq(t, srv, http.MethodDelete, "/v1/auth/keys/"+created.ID, token, "")
	assert.Equal(t, http.StatusNotFound, rr.Code)

	// Unnamed keys default to "unnamed".
	rr = doReq(t, srv, http.MethodPost, "/v1/auth/keys", token, `{}`)
	require.Equal(t, http.StatusCreated, rr.Code)
	assert.Contains(t, rr.Body.String(), `"name":"unnamed"`)
}

func TestAPIKeys_CannotRevokeOtherTenantsKey(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	token := seedAuthenticatedTenant(t, stateDB, reviewMasterKey)
	otherToken := seedTenantWithKey(t, stateDB, reviewMasterKey, "tenant-other", 0)
	otherKeyID := otherToken[:strings.IndexByte(otherToken, '.')]
	srv := NewServer(ServerConfig{Store: &db.Store{StateDB: stateDB}, MasterKey: reviewMasterKey, DevMode: true})

	rr := doReq(t, srv, http.MethodDelete, "/v1/auth/keys/"+otherKeyID, token, "")
	assert.Equal(t, http.StatusNotFound, rr.Code)
	rr = doReq(t, srv, http.MethodGet, "/v1/tenant", otherToken, "")
	assert.Equal(t, http.StatusOK, rr.Code, "other tenant's key must still work")
}

func TestAPIKeys_CreateValidation(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	token := seedAuthenticatedTenant(t, stateDB, reviewMasterKey)
	srv := NewServer(ServerConfig{Store: &db.Store{StateDB: stateDB}, MasterKey: reviewMasterKey, DevMode: true})

	for _, tc := range []struct{ body, want string }{
		{`{"expires_in":0}`, "must be positive"},
		{`{"expires_in":-5}`, "must be positive"},
		{`{"expires_in":` + strconv.Itoa(11*365*24*3600) + `}`, "exceeds maximum"},
		{`{"name":`, "invalid request body"},
	} {
		rr := doReq(t, srv, http.MethodPost, "/v1/auth/keys", token, tc.body)
		assert.Equal(t, http.StatusBadRequest, rr.Code, tc.body)
		assert.Contains(t, rr.Body.String(), tc.want, tc.body)
	}
}

// Concurrent creates at the limit must not overshoot maxKeysPerTenant. The
// quota check now lives in the INSERT itself, so there is no window between
// check and write (this is a smoke test; the old window was too narrow to hit
// reliably here).
func TestAPIKeys_ConcurrentCreatesRespectQuota(t *testing.T) {
	stateDB := testutil.NewFileStateDB(t)
	token := seedAuthenticatedTenant(t, stateDB, reviewMasterKey)
	for i := 2; i < maxKeysPerTenant; i++ { // seed key + these = maxKeysPerTenant-1
		_, err := stateDB.Exec(
			`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at) VALUES (?, 'tenant-1', 'fill', 'fill', 'x', 1)`,
			"fill-"+strconv.Itoa(i))
		require.NoError(t, err)
	}
	srv := NewServer(ServerConfig{Store: &db.Store{StateDB: stateDB}, MasterKey: reviewMasterKey, DevMode: true})

	const n = 8
	var created atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rr := doReq(t, srv, http.MethodPost, "/v1/auth/keys", token, `{"name":"race"}`)
			switch rr.Code {
			case http.StatusCreated:
				created.Add(1)
			case http.StatusConflict:
			default:
				t.Errorf("unexpected status %d: %s", rr.Code, rr.Body.String())
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(1), created.Load(), "exactly one create may take the last slot")
	var active int
	require.NoError(t, stateDB.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE tenant_id = 'tenant-1' AND revoked_at IS NULL`).Scan(&active))
	assert.Equal(t, maxKeysPerTenant, active)
}

// mintAPIKey output must verify through the auth middleware's hash key.
func TestMintAPIKey(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	srv := NewServer(ServerConfig{Store: &db.Store{StateDB: stateDB}, MasterKey: reviewMasterKey, DevMode: true})
	k1, err := srv.mintAPIKey()
	require.NoError(t, err)
	k2, err := srv.mintAPIKey()
	require.NoError(t, err)
	assert.NotEqual(t, k1.id, k2.id)
	assert.Len(t, k1.id, 32)
	assert.Equal(t, k1.id[:8], k1.prefix)
	secret := strings.TrimPrefix(k1.token, k1.id+".")
	assert.NotEqual(t, k1.token, secret)
	assert.NotContains(t, k1.hash, secret, "hash must not contain the secret")
}

// ---------------------------------------------------------------------------
// Tenants
// ---------------------------------------------------------------------------

func TestTenantUpdate(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	token := seedAuthenticatedTenant(t, stateDB, reviewMasterKey)
	srv := NewServer(ServerConfig{Store: &db.Store{StateDB: stateDB}, MasterKey: reviewMasterKey, DevMode: true})

	rr := doReq(t, srv, http.MethodPatch, "/v1/tenant", token, `{"name":"Renamed"}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var tr TenantResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&tr))
	assert.Equal(t, "Renamed", tr.Name)
	assert.Equal(t, "tenant-1", tr.ID)

	rr = doReq(t, srv, http.MethodPatch, "/v1/tenant", token, `{"name":"x"}`)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	rr = doReq(t, srv, http.MethodPatch, "/v1/tenant", token, `{"name":"`+strings.Repeat("n", 129)+`"}`)
	assert.Equal(t, http.StatusBadRequest, rr.Code)

	// An empty patch is a no-op that returns the current tenant.
	rr = doReq(t, srv, http.MethodPatch, "/v1/tenant", token, `{}`)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), `"name":"Renamed"`)
}

// Two reactivations racing past the status check: the one that loses the
// guarded UPDATE gets 409 and mints no key (previously both succeeded and the
// tenant got two keys plus two rounds of container restarts).
func TestTenantReactivate_RaceLoserGets409(t *testing.T) {
	resetLimitersForTest()
	t.Cleanup(resetLimitersForTest)
	const adminToken = "admin-token-for-racing-reactivate-0123456789"
	stateDB := testutil.NewStateDB(t)
	_, err := stateDB.Exec(`INSERT INTO tenants (id, name, email, status, created_at, updated_at) VALUES ('t-r', 'R', 'r@example.com', 'suspended', 1, 1)`)
	require.NoError(t, err)
	srv := NewServer(ServerConfig{Store: &db.Store{StateDB: stateDB}, MasterKey: reviewMasterKey, DevMode: true, AdminTokens: []string{adminToken}})

	reactivate := func(ip string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/tenants/t-r/reactivate", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("X-Real-Ip", ip)
		req.Header.Set("X-Admin-Token", adminToken)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr
	}

	// After the first request passes its status check, a second request runs
	// to completion before the first reaches its UPDATE.
	var winner *httptest.ResponseRecorder
	reactivateAfterStatusCheck = func() {
		reactivateAfterStatusCheck = nil
		winner = reactivate("10.77.0.2")
	}
	t.Cleanup(func() { reactivateAfterStatusCheck = nil })

	loser := reactivate("10.77.0.1")
	require.NotNil(t, winner)
	assert.Equal(t, http.StatusOK, winner.Code, winner.Body.String())
	assert.Equal(t, http.StatusConflict, loser.Code, loser.Body.String())

	var keys int
	require.NoError(t, stateDB.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE tenant_id = 't-r'`).Scan(&keys))
	assert.Equal(t, 1, keys, "only the winning reactivation may mint a key")
}

func TestQuotaUpdate_PartialUpdateAndCaps(t *testing.T) {
	resetLimitersForTest()
	t.Cleanup(resetLimitersForTest)
	const adminToken = "admin-token-for-quota-review-tests-0123456789"
	stateDB := testutil.NewStateDB(t)
	seedAuthenticatedTenant(t, stateDB, reviewMasterKey)
	srv := NewServer(ServerConfig{Store: &db.Store{StateDB: stateDB}, MasterKey: reviewMasterKey, DevMode: true, AdminTokens: []string{adminToken}})

	patch := func(ip, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, "/v1/tenants/tenant-1/quotas", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("X-Real-Ip", ip)
		req.Header.Set("X-Admin-Token", adminToken)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr
	}

	var before QuotaUpdateResponse
	require.NoError(t, stateDB.QueryRow(`SELECT max_services, max_databases FROM tenant_quotas WHERE tenant_id = 'tenant-1'`).Scan(&before.MaxServices, &before.MaxDatabases))

	rr := patch("10.88.0.1", `{"max_builds_concurrent":3,"max_env_vars_per_service":200}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var resp QuotaUpdateResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal(t, 3, resp.MaxBuildsConcurrent)
	assert.Equal(t, 200, resp.MaxEnvVarsPerService)
	assert.Equal(t, before.MaxServices, resp.MaxServices, "omitted fields are unchanged")
	assert.Equal(t, before.MaxDatabases, resp.MaxDatabases, "omitted fields are unchanged")

	for _, tc := range []struct{ body, want string }{
		{`{"max_databases":0}`, "max_databases must be at least 1"},
		{`{"max_builds_concurrent":11}`, "max_builds_concurrent must not exceed 10"},
		{`{"max_env_vars_per_service":501}`, "max_env_vars_per_service must not exceed 500"},
		// The first invalid field aborts the whole update.
		{`{"max_services":5,"max_databases":51}`, "max_databases must not exceed 50"},
	} {
		rr := patch("10.88.0.2", tc.body)
		assert.Equal(t, http.StatusBadRequest, rr.Code, tc.body)
		assert.Contains(t, rr.Body.String(), tc.want, tc.body)
	}
	var services int
	require.NoError(t, stateDB.QueryRow(`SELECT max_services FROM tenant_quotas WHERE tenant_id = 'tenant-1'`).Scan(&services))
	assert.Equal(t, before.MaxServices, services, "a rejected request must not apply its valid fields")
}

func TestAllowRequest_RetryAfter(t *testing.T) {
	rl := &registrationLimiter{}
	rl.resetForTest()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "192.0.2.10:5555"
	for i := 0; i < regMaxPerIPPerHour; i++ {
		require.True(t, rl.allowRequest(httptest.NewRecorder(), req))
	}
	rr := httptest.NewRecorder()
	assert.False(t, rl.allowRequest(rr, req))
	assert.Equal(t, http.StatusTooManyRequests, rr.Code)
	secs, err := strconv.Atoi(rr.Header().Get("Retry-After"))
	require.NoError(t, err)
	assert.Greater(t, secs, 0)
	assert.LessOrEqual(t, secs, int(regWindow.Seconds()))
}

// ---------------------------------------------------------------------------
// Metering
// ---------------------------------------------------------------------------

// A failing ownership lookup is a server error, not "service not found".
func TestServiceMetrics_DBErrorIs500(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	token := seedAuthenticatedTenant(t, stateDB, reviewMasterKey)
	srv := NewServer(ServerConfig{
		Store:         &db.Store{StateDB: stateDB},
		MasterKey:     reviewMasterKey,
		DevMode:       true,
		MeteringStore: metering.NewStore(testutil.NewMeteringDB(t)),
	})
	_, err := stateDB.Exec(`ALTER TABLE services RENAME TO services_gone`)
	require.NoError(t, err)

	rr := doReq(t, srv, http.MethodGet, "/v1/services/svc-1/metrics", token, "")
	assert.Equal(t, http.StatusInternalServerError, rr.Code, rr.Body.String())
}

func TestParseMetricsQuery(t *testing.T) {
	get := func(q string) (metricsQuery, error) {
		return parseMetricsQuery(httptest.NewRequest(http.MethodGet, "/?"+q, nil))
	}
	mq, err := get("")
	require.NoError(t, err)
	assert.Equal(t, "daily", mq.period)
	assert.InDelta(t, 24*time.Hour, mq.until.Sub(mq.since), float64(time.Second))

	mq, err = get("period=hourly&since=2026-01-01T00:00:00Z&until=2026-01-02T00:00:00%2B02:00")
	require.NoError(t, err)
	assert.Equal(t, "hourly", mq.period)
	assert.Equal(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), mq.since)
	assert.Equal(t, time.Date(2026, 1, 1, 22, 0, 0, 0, time.UTC), mq.until, "until is normalized to UTC")

	for q, want := range map[string]string{
		"period=weekly":   "period must be",
		"since=yesterday": "since must be a valid",
		"until=2026":      "until must be a valid",
		"since=2026-02-01T00:00:00Z&until=2026-01-01T00:00:00Z": "since must be before until",
	} {
		_, err := get(q)
		require.Error(t, err, q)
		assert.Contains(t, err.Error(), want, q)
	}
}

// ---------------------------------------------------------------------------
// Pagination helpers
// ---------------------------------------------------------------------------

func TestParseLimit(t *testing.T) {
	get := func(q string, def int) (int, error) {
		return parseLimit(httptest.NewRequest(http.MethodGet, "/?"+q, nil), def)
	}
	n, err := get("", 50)
	require.NoError(t, err)
	assert.Equal(t, 50, n)
	n, err = get("limit=7", 50)
	require.NoError(t, err)
	assert.Equal(t, 7, n)
	n, err = get("limit=999", 50)
	require.NoError(t, err)
	assert.Equal(t, maxPageLimit, n)
	for _, q := range []string{"limit=0", "limit=-1", "limit=abc"} {
		_, err := get(q, 50)
		require.Error(t, err, q)
		assert.Equal(t, "limit must be between 1 and 200", err.Error())
	}
}

func TestListLimitValidation_ActivityAndBuilds(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	token := seedAuthenticatedTenant(t, stateDB, reviewMasterKey)
	srv := NewServer(ServerConfig{Store: &db.Store{StateDB: stateDB}, MasterKey: reviewMasterKey, DevMode: true})

	rr := doReq(t, srv, http.MethodGet, "/v1/activity?limit=0", token, "")
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	rr = doReq(t, srv, http.MethodGet, "/v1/activity?limit=500", token, "")
	assert.Equal(t, http.StatusOK, rr.Code)
}

// ---------------------------------------------------------------------------
// Unconfigured managers return 503 on every route they back
// ---------------------------------------------------------------------------

func TestUnconfiguredManagersReturn503(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	token := seedAuthenticatedTenant(t, stateDB, reviewMasterKey)
	srv := NewServer(ServerConfig{Store: &db.Store{StateDB: stateDB}, MasterKey: reviewMasterKey, DevMode: true})

	for _, rt := range []struct{ method, path string }{
		{http.MethodPost, "/v1/services/s/builds"},
		{http.MethodGet, "/v1/builds"},
		{http.MethodGet, "/v1/services/s/builds"},
		{http.MethodGet, "/v1/services/s/builds/b"},
		{http.MethodGet, "/v1/services/s/builds/b/logs"},
		{http.MethodDelete, "/v1/services/s/builds/b"},
		{http.MethodGet, "/v1/databases"},
		{http.MethodGet, "/v1/databases/d"},
		{http.MethodGet, "/v1/databases/d/connection-string"},
		{http.MethodPost, "/v1/kanban"},
		{http.MethodGet, "/v1/kanban"},
		{http.MethodGet, "/v1/kanban/admin-token"},
		{http.MethodDelete, "/v1/kanban"},
		{http.MethodGet, "/v1/services"},
		{http.MethodGet, "/v1/services/s"},
		{http.MethodDelete, "/v1/services/s"},
		{http.MethodPost, "/v1/services/s/start"},
		{http.MethodPost, "/v1/services/s/stop"},
		{http.MethodPost, "/v1/services/s/restart"},
		{http.MethodPost, "/v1/services/s/reset"},
		{http.MethodGet, "/v1/services/s/env"},
		{http.MethodDelete, "/v1/services/s/env/K"},
		{http.MethodGet, "/v1/snapshots"},
		{http.MethodDelete, "/v1/snapshots/x"},
		{http.MethodGet, "/v1/environments/templates/t"},
		{http.MethodGet, "/v1/environments/e/previews"},
		{http.MethodGet, "/v1/tenant/usage/metrics"},
	} {
		rr := doReq(t, srv, rt.method, rt.path, token, `{}`)
		assert.Equal(t, http.StatusServiceUnavailable, rr.Code, "%s %s: %s", rt.method, rt.path, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Kanban and database handlers (fake managers)
// ---------------------------------------------------------------------------

func TestKanbanAndDatabaseHandlers_WithManagers(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	token := seedAuthenticatedTenant(t, stateDB, reviewMasterKey)
	srv := NewServer(ServerConfig{
		Store:           &db.Store{StateDB: stateDB},
		MasterKey:       reviewMasterKey,
		DevMode:         true,
		KanbanManager:   &fakeKanbanManager{},
		DatabaseManager: &fakeDatabaseManager{},
	})

	assert.Equal(t, http.StatusCreated, doReq(t, srv, http.MethodPost, "/v1/kanban", token, "").Code)
	assert.Equal(t, http.StatusOK, doReq(t, srv, http.MethodGet, "/v1/kanban", token, "").Code)
	assert.Equal(t, http.StatusOK, doReq(t, srv, http.MethodGet, "/v1/kanban/admin-token", token, "").Code)
	assert.Equal(t, http.StatusNoContent, doReq(t, srv, http.MethodDelete, "/v1/kanban", token, "").Code)

	rr := doReq(t, srv, http.MethodGet, "/v1/databases", token, "")
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.JSONEq(t, `[]`, rr.Body.String(), "an empty list is [] not null")
	assert.Equal(t, http.StatusOK, doReq(t, srv, http.MethodGet, "/v1/databases/db-1", token, "").Code)
	assert.Equal(t, http.StatusBadRequest, doReq(t, srv, http.MethodGet, "/v1/databases?offset=-1", token, "").Code)
}

// ---------------------------------------------------------------------------
// Service handlers (real services.Manager, mock Docker)
// ---------------------------------------------------------------------------

func newServiceTestServer(t *testing.T, dc *testutil.MockDockerClient) (*Server, *sql.DB, string) {
	t.Helper()
	stateDB := testutil.NewStateDB(t)
	token := seedAuthenticatedTenant(t, stateDB, reviewMasterKey)
	_, err := stateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, dns_label, status, image, port, container_id, created_at, updated_at)
		 VALUES ('svc-1', 'tenant-1', 'web', 'web-abc', 'running', 'nginx:latest', 8080, 'ctr-1', 1, 1)`)
	require.NoError(t, err)
	srv := NewServer(ServerConfig{
		Store:            &db.Store{StateDB: stateDB},
		MasterKey:        reviewMasterKey,
		DevMode:          true,
		Docker:           dc,
		TraefikConfigDir: t.TempDir(),
	})
	return srv, stateDB, token
}

func TestServiceHandlers_Lifecycle(t *testing.T) {
	dc := &testutil.MockDockerClient{}
	srv, stateDB, token := newServiceTestServer(t, dc)

	rr := doReq(t, srv, http.MethodGet, "/v1/services/svc-1", token, "")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), `"id":"svc-1"`)

	rr = doReq(t, srv, http.MethodPost, "/v1/services/svc-1/stop", token, "")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.JSONEq(t, `{"status":"stopped"}`, rr.Body.String())

	rr = doReq(t, srv, http.MethodPost, "/v1/services/svc-1/start", token, "")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.JSONEq(t, `{"status":"running"}`, rr.Body.String())

	rr = doReq(t, srv, http.MethodPost, "/v1/services/svc-1/restart", token, "")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	rr = doReq(t, srv, http.MethodPost, "/v1/services/svc-1/reset", token, "")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "circuit breaker reset")

	rr = doReq(t, srv, http.MethodDelete, "/v1/services/svc-1", token, "")
	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
	var n int
	require.NoError(t, stateDB.QueryRow(`SELECT COUNT(*) FROM services WHERE id = 'svc-1'`).Scan(&n))
	assert.Zero(t, n)

	rr = doReq(t, srv, http.MethodGet, "/v1/services/svc-1", token, "")
	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestServiceHandlers_OtherTenantGets404(t *testing.T) {
	srv, stateDB, _ := newServiceTestServer(t, &testutil.MockDockerClient{})
	other := seedTenantWithKey(t, stateDB, reviewMasterKey, "tenant-2", 0)

	for _, rt := range []struct{ method, path string }{
		{http.MethodGet, "/v1/services/svc-1"},
		{http.MethodPost, "/v1/services/svc-1/stop"},
		{http.MethodPost, "/v1/services/svc-1/start"},
		{http.MethodDelete, "/v1/services/svc-1"},
		{http.MethodGet, "/v1/services/svc-1/env"},
		{http.MethodGet, "/v1/services/svc-1/logs"},
	} {
		rr := doReq(t, srv, rt.method, rt.path, other, "")
		assert.Equal(t, http.StatusNotFound, rr.Code, "%s %s", rt.method, rt.path)
	}
}

func TestServiceHandlers_Env(t *testing.T) {
	srv, _, token := newServiceTestServer(t, &testutil.MockDockerClient{})

	rr := doReq(t, srv, http.MethodPost, "/v1/services/svc-1/env", token, `{"API_URL":"https://x","TOKEN":"s3cret"}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), `"status":"updated"`)

	rr = doReq(t, srv, http.MethodGet, "/v1/services/svc-1/env", token, "")
	require.Equal(t, http.StatusOK, rr.Code)
	assert.NotContains(t, rr.Body.String(), "s3cret", "values are masked unless reveal=true")

	rr = doReq(t, srv, http.MethodGet, "/v1/services/svc-1/env?reveal=true", token, "")
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), "s3cret")

	rr = doReq(t, srv, http.MethodDelete, "/v1/services/svc-1/env/TOKEN", token, "")
	assert.Equal(t, http.StatusNoContent, rr.Code)
	rr = doReq(t, srv, http.MethodDelete, "/v1/services/svc-1/env/TOKEN", token, "")
	assert.Equal(t, http.StatusNotFound, rr.Code)

	// Validation.
	assert.Equal(t, http.StatusBadRequest, doReq(t, srv, http.MethodPost, "/v1/services/svc-1/env", token, `{}`).Code)
	many := map[string]string{}
	for i := 0; i <= 100; i++ {
		many["K"+strconv.Itoa(i)] = "v"
	}
	body, _ := json.Marshal(many)
	rr = doReq(t, srv, http.MethodPost, "/v1/services/svc-1/env", token, string(body))
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "max 100")
}

func TestServiceCreate_FromSnapshot(t *testing.T) {
	token, stateDB, serviceID := seedSnapshotTestDB(t, true)
	srv := NewServer(ServerConfig{
		Store:            &db.Store{StateDB: stateDB},
		MasterKey:        reviewMasterKey,
		DevMode:          true,
		Docker:           &testutil.MockDockerClient{},
		TraefikConfigDir: t.TempDir(),
	})
	snapID := createSnapshotViaAPI(t, srv, token, serviceID, "base")

	rr := doReq(t, srv, http.MethodPost, "/v1/services?from_snapshot="+snapID, token, `{"name":"fork"}`)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	var svc map[string]any
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&svc))
	assert.Equal(t, "deploying", svc["status"])
	assert.Equal(t, "fork", svc["name"])

	// Env vars were copied from the snapshot.
	var envCount int
	require.NoError(t, stateDB.QueryRow(`SELECT COUNT(*) FROM service_env WHERE service_id = ?`, svc["id"]).Scan(&envCount))
	assert.Equal(t, 2, envCount)

	assert.Equal(t, http.StatusBadRequest, doReq(t, srv, http.MethodPost, "/v1/services?from_snapshot="+snapID, token, `{}`).Code)
	assert.Equal(t, http.StatusNotFound, doReq(t, srv, http.MethodPost, "/v1/services?from_snapshot=nope", token, `{"name":"x"}`).Code)

	// Snapshot delete.
	assert.Equal(t, http.StatusNoContent, doReq(t, srv, http.MethodDelete, "/v1/snapshots/"+snapID, token, "").Code)
	assert.Equal(t, http.StatusNotFound, doReq(t, srv, http.MethodDelete, "/v1/snapshots/"+snapID, token, "").Code)
}

// endlessReader yields log bytes forever, like a followed container log.
type endlessReader struct {
	reads  atomic.Int32
	closed atomic.Bool
}

func (e *endlessReader) Read(p []byte) (int, error) {
	if e.reads.Add(1) > 10000 {
		return 0, errors.New("test guard: handler kept reading after the client went away")
	}
	return copy(p, "log line\n"), nil
}

func (e *endlessReader) Close() error { e.closed.Store(true); return nil }

// failingWriter accepts the header and then fails every body write, like a
// client whose write deadline expired.
type failingWriter struct {
	header http.Header
	writes int
}

func (f *failingWriter) Header() http.Header { return f.header }
func (f *failingWriter) WriteHeader(int)     {}
func (f *failingWriter) Flush()              {}
func (f *failingWriter) Write(p []byte) (int, error) {
	f.writes++
	return 0, errors.New("write: broken pipe")
}

// A followed log stream must stop reading once writes to the client fail.
func TestServiceLogs_StopsOnWriteError(t *testing.T) {
	logs := &endlessReader{}
	dc := &testutil.MockDockerClient{
		LogsContainerFn: func(ctx context.Context, containerID string, follow bool, tail int) (io.ReadCloser, error) {
			assert.True(t, follow)
			assert.Equal(t, 100, tail, "out-of-range tail falls back to the default")
			return logs, nil
		},
	}
	srv, _, token := newServiceTestServer(t, dc)

	req := httptest.NewRequest(http.MethodGet, "/v1/services/svc-1/logs?follow=true&tail=999999", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	fw := &failingWriter{header: http.Header{}}
	srv.ServeHTTP(fw, req)

	assert.Equal(t, 1, fw.writes, "handler must stop after the first failed write")
	assert.Less(t, logs.reads.Load(), int32(5))
	assert.True(t, logs.closed.Load(), "the docker log stream must be closed")
}

func TestServiceLogs_NonFollowReturnsBody(t *testing.T) {
	dc := &testutil.MockDockerClient{
		LogsContainerFn: func(ctx context.Context, containerID string, follow bool, tail int) (io.ReadCloser, error) {
			assert.Equal(t, 5, tail)
			return io.NopCloser(strings.NewReader("hello\nworld\n")), nil
		},
	}
	srv, _, token := newServiceTestServer(t, dc)
	rr := doReq(t, srv, http.MethodGet, "/v1/services/svc-1/logs?tail=5", token, "")
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "hello\nworld\n", rr.Body.String())
	assert.Equal(t, "text/plain; charset=utf-8", rr.Header().Get("Content-Type"))
}

// ---------------------------------------------------------------------------
// Transport security middleware
// ---------------------------------------------------------------------------

func TestRequireHTTPS(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	srv := NewServer(ServerConfig{Store: &db.Store{StateDB: stateDB}, MasterKey: reviewMasterKey, DevMode: false})

	health := func(remote, proto string, withTLS bool) int {
		req := httptest.NewRequest(http.MethodGet, "/v1/system/health", nil)
		req.RemoteAddr = remote
		if proto != "" {
			req.Header.Set("X-Forwarded-Proto", proto)
		}
		if withTLS {
			req.TLS = &tls.ConnectionState{}
		}
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr.Code
	}

	assert.Equal(t, http.StatusOK, health("127.0.0.1:5000", "https", false), "trusted proxy with https")
	assert.Equal(t, http.StatusForbidden, health("127.0.0.1:5000", "http", false), "trusted proxy with plain http")
	assert.Equal(t, http.StatusForbidden, health("127.0.0.1:5000", "", false), "loopback without proxy header")
	assert.Equal(t, http.StatusForbidden, health("203.0.113.9:5000", "https", false), "spoofed header from non-loopback is stripped")
	assert.Equal(t, http.StatusOK, health("203.0.113.9:5000", "", true), "direct TLS")
}

func TestWriteDecodeError_BodyTooLarge(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	token := seedAuthenticatedTenant(t, stateDB, reviewMasterKey)
	srv := NewServer(ServerConfig{Store: &db.Store{StateDB: stateDB}, MasterKey: reviewMasterKey, DevMode: true})

	big := `{"name":"` + strings.Repeat("a", 2<<20) + `"}`
	req := httptest.NewRequest(http.MethodPatch, "/v1/tenant", bytes.NewReader([]byte(big)))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rr.Code)
}
