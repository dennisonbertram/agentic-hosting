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

func TestTenantMetrics_KindFilter(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	meteringDB := testutil.NewMeteringDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)
	at := time.Now().Add(-time.Hour).Unix()
	_, err := meteringDB.Exec(`INSERT INTO usage_events (id, tenant_id, service_id, event_type, cpu_seconds, memory_mb_seconds, recorded_at, resource_kind)
		VALUES ('e1', 'tenant-1', 'svc-1', 'sample', 1, 60, ?, 'service'), ('e2', 'tenant-1', 'sbx-1', 'sample', 2, 60, ?, 'sandbox')`, at, at)
	require.NoError(t, err)
	srv := NewServer(ServerConfig{
		Store:         &db.Store{StateDB: stateDB, MeteringDB: meteringDB},
		MasterKey:     masterKey,
		DevMode:       true,
		MeteringStore: metering.NewStore(meteringDB),
	})
	get := func(q string) (int, []metering.Metric) {
		req := httptest.NewRequest(http.MethodGet, "/v1/tenant/usage/metrics"+q, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		var m []metering.Metric
		if rr.Code == http.StatusOK {
			require.NoError(t, json.NewDecoder(rr.Body).Decode(&m))
		}
		return rr.Code, m
	}
	code, m := get("")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, m, 1, "default kind is service: unchanged output")
	assert.Equal(t, "svc-1", m[0].ServiceID)
	code, m = get("?kind=sandbox")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, m, 1)
	assert.Equal(t, "sbx-1", m[0].ServiceID)
	code, _ = get("?kind=database")
	assert.Equal(t, http.StatusBadRequest, code)
}

func TestTenantUsage_ReportsSandboxes(t *testing.T) {
	h := newSandboxTestHelper(t)
	h.createSandbox(t, `{"source":{"type":"template"}}`)
	rr := h.doRequest(t, http.MethodGet, "/v1/tenant/usage", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var resp TenantUsageResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Sandboxes.Used)
	assert.Equal(t, 5, resp.Sandboxes.Max)
}
