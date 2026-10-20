package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	atBootstrap = "bootstrap-token-0123456789-0123456789"
	atAdmin     = "admin-token-aaaaaaaaaa-0123456789abc"
)

func newAdminTestServer(t *testing.T, admin []string) *Server {
	t.Helper()
	stateDB := testutil.NewStateDB(t)
	_, err := stateDB.Exec(
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
		 VALUES ('t-admin', 'Admin Tenant', 'admin@example.com', 'active', 1, 1)`)
	require.NoError(t, err)
	_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES ('t-admin')`)
	require.NoError(t, err)
	return NewServer(ServerConfig{
		Store:           &db.Store{StateDB: stateDB},
		MasterKey:       []byte("0123456789abcdef0123456789abcdef"),
		DevMode:         true,
		BootstrapTokens: []string{atBootstrap},
		AdminTokens:     admin,
	})
}

func adminCalls(ip, token string) map[string]*http.Request {
	rec, _ := json.Marshal(KeyRecoverRequest{Email: "admin@example.com", BootstrapToken: token})
	return map[string]*http.Request{
		"recover":    recoverRequest(ip, rec),
		"reactivate": reactivateRequest(ip, "t-admin", token),
		"quota":      quotaRequest(ip, "t-admin", token, []byte(`{"max_services": 20}`)),
	}
}

func TestAdminToken_BootstrapTokenRejectedWhenAdminSet(t *testing.T) {
	resetLimitersForTest()
	srv := newAdminTestServer(t, []string{atAdmin})
	for name, req := range adminCalls("10.9.0.1", atBootstrap) {
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusForbidden, rr.Code, "%s: %s", name, rr.Body.String())
	}
}

func TestAdminToken_AdminTokenAccepted(t *testing.T) {
	resetLimitersForTest()
	srv := newAdminTestServer(t, []string{atAdmin})
	// Recover and quota succeed; reactivate of an active tenant is 409 (past auth).
	want := map[string]int{"recover": http.StatusCreated, "reactivate": http.StatusConflict, "quota": http.StatusOK}
	for name, req := range adminCalls("10.9.0.2", atAdmin) {
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		assert.Equal(t, want[name], rr.Code, "%s: %s", name, rr.Body.String())
	}
	// X-Admin-Token header also works.
	req := quotaRequest("10.9.0.2", "t-admin", "", []byte(`{"max_services": 21}`))
	req.Header.Del("X-Bootstrap-Token")
	req.Header.Set("X-Admin-Token", atAdmin)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
}

func TestAdminToken_FallbackToBootstrapWhenUnset(t *testing.T) {
	resetLimitersForTest()
	srv := newAdminTestServer(t, nil)
	want := map[string]int{"recover": http.StatusCreated, "reactivate": http.StatusConflict, "quota": http.StatusOK}
	for name, req := range adminCalls("10.9.0.3", atBootstrap) {
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		assert.Equal(t, want[name], rr.Code, "%s: %s", name, rr.Body.String())
	}
}

func TestRateLimit_InvalidTokensDoNotConsumeGlobalBudget(t *testing.T) {
	resetLimitersForTest()
	t.Cleanup(resetLimitersForTest)
	srv := newAdminTestServer(t, nil)

	// 30 invalid registrations from distinct IPs (so the per-IP cap never fires).
	for i := 0; i < 30; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/tenants/register", bytes.NewReader([]byte(`{}`)))
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("X-Real-Ip", fmt.Sprintf("10.8.0.%d", i+1))
		req.Header.Set("X-Bootstrap-Token", "wrong")
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		require.Equal(t, http.StatusUnauthorized, rr.Code)
	}

	body, _ := json.Marshal(RegisterRequest{Name: "Legit User", Email: "legit@example.com"})
	req := httptest.NewRequest(http.MethodPost, "/v1/tenants/register", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Real-Ip", "10.8.1.1")
	req.Header.Set("X-Bootstrap-Token", atBootstrap)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
}

// Bootstrap-token holders spending the registration budget (validate calls,
// registrations with bad bodies) must not lock the operator out of admin routes.
func TestRateLimit_BootstrapHolderCannotExhaustAdminBudget(t *testing.T) {
	resetLimitersForTest()
	t.Cleanup(resetLimitersForTest)
	srv := newAdminTestServer(t, []string{atAdmin})

	for i := 0; i < 2*regGlobalPerHour; i++ {
		var req *http.Request
		if i%2 == 0 {
			body, _ := json.Marshal(BootstrapTokenValidateRequest{Token: atBootstrap})
			req = httptest.NewRequest(http.MethodPost, "/v1/system/bootstrap-token/validate", bytes.NewReader(body))
		} else {
			req = httptest.NewRequest(http.MethodPost, "/v1/tenants/register", bytes.NewReader([]byte(`not json`)))
			req.Header.Set("X-Bootstrap-Token", atBootstrap)
		}
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("X-Real-Ip", fmt.Sprintf("10.7.%d.1", i))
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		require.NotEqual(t, http.StatusTooManyRequests, rr.Code, "request %d", i)
	}

	// Registration's global budget was not charged by failed/validate calls.
	body, _ := json.Marshal(RegisterRequest{Name: "Legit User", Email: "legit2@example.com"})
	req := httptest.NewRequest(http.MethodPost, "/v1/tenants/register", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Real-Ip", "10.7.200.1")
	req.Header.Set("X-Bootstrap-Token", atBootstrap)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())

	// Even with the registration global bucket full, admin routes still work.
	for i := 0; i < regGlobalPerHour; i++ {
		regLimiter.chargeGlobal()
	}
	quota := quotaRequest("10.7.201.1", "t-admin", atAdmin, []byte(`{"max_services": 20}`))
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, quota)
	assert.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
}
