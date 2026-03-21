package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reactivateRequest builds a POST /v1/tenants/{tenantID}/reactivate request
// with a distinct source IP so tests do not share the same rate-limit bucket.
func reactivateRequest(ip, tenantID, bootstrapToken string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/tenants/"+tenantID+"/reactivate", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Bootstrap-Token", bootstrapToken)
	// Set loopback RemoteAddr so trustedRealIP uses X-Real-Ip for rate limiting.
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Real-Ip", ip)
	return req
}

func TestHandleTenantReactivate(t *testing.T) {
	const bootstrapToken = "test-bootstrap-token-for-reactivation"
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	t.Run("successful reactivation of suspended tenant", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		// Seed a suspended tenant with revoked keys (simulating post-DELETE state).
		_, err := stateDB.Exec(
			`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
			 VALUES (?, ?, ?, 'suspended', 1, 1)`,
			"tenant-react", "Suspended Tenant", "suspended@example.com",
		)
		require.NoError(t, err)
		_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`, "tenant-react")
		require.NoError(t, err)

		srv := NewServer(ServerConfig{
			Store:          &db.Store{StateDB: stateDB},
			MasterKey:      masterKey,
			DevMode:        true,
			BootstrapTokens: []string{bootstrapToken},
		})

		req := reactivateRequest("10.1.0.1", "tenant-react", bootstrapToken)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code, "expected 200, got: %s", rr.Body.String())

		var resp ReactivateResponse
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
		assert.Equal(t, "tenant-react", resp.TenantID, "response tenant_id should match")
		assert.Equal(t, "active", resp.Status, "response status should be active")
		assert.NotEmpty(t, resp.APIKey, "response should include a new API key")
		assert.Contains(t, resp.APIKey, ".", "API key should be in keyID.secret format")

		// Verify the tenant status was updated in the database.
		var status string
		err = stateDB.QueryRow(`SELECT status FROM tenants WHERE id = ?`, "tenant-react").Scan(&status)
		require.NoError(t, err)
		assert.Equal(t, "active", status, "tenant status should be active in DB")
	})

	t.Run("reactivation of already-active tenant returns 409", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		_, err := stateDB.Exec(
			`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
			 VALUES (?, ?, ?, 'active', 1, 1)`,
			"tenant-active", "Active Tenant", "active@example.com",
		)
		require.NoError(t, err)
		_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`, "tenant-active")
		require.NoError(t, err)

		srv := NewServer(ServerConfig{
			Store:          &db.Store{StateDB: stateDB},
			MasterKey:      masterKey,
			DevMode:        true,
			BootstrapTokens: []string{bootstrapToken},
		})

		req := reactivateRequest("10.1.0.2", "tenant-active", bootstrapToken)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusConflict, rr.Code, "already-active tenant should return 409")
	})

	t.Run("reactivation of non-existent tenant returns 404", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)

		srv := NewServer(ServerConfig{
			Store:          &db.Store{StateDB: stateDB},
			MasterKey:      masterKey,
			DevMode:        true,
			BootstrapTokens: []string{bootstrapToken},
		})

		req := reactivateRequest("10.1.0.3", "does-not-exist", bootstrapToken)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusNotFound, rr.Code, "non-existent tenant should return 404")
	})

	t.Run("reactivation without bootstrap token returns 401", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		_, err := stateDB.Exec(
			`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
			 VALUES (?, ?, ?, 'suspended', 1, 1)`,
			"tenant-notoken", "Suspended", "notoken@example.com",
		)
		require.NoError(t, err)

		srv := NewServer(ServerConfig{
			Store:          &db.Store{StateDB: stateDB},
			MasterKey:      masterKey,
			DevMode:        true,
			BootstrapTokens: []string{bootstrapToken},
		})

		req := reactivateRequest("10.1.0.4", "tenant-notoken", "wrong-token")
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code, "wrong bootstrap token should return 401")
	})

	t.Run("reactivation with missing bootstrap token header returns 401", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)

		srv := NewServer(ServerConfig{
			Store:          &db.Store{StateDB: stateDB},
			MasterKey:      masterKey,
			DevMode:        true,
			BootstrapTokens: []string{bootstrapToken},
		})

		req := httptest.NewRequest(http.MethodPost, "/v1/tenants/some-tenant/reactivate", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("X-Real-Ip", "10.1.0.5")
		// No X-Bootstrap-Token header set.
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code, "missing bootstrap token should return 401")
	})

	t.Run("reactivation when bootstrap token not configured returns 503", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)

		srv := NewServer(ServerConfig{
			Store:     &db.Store{StateDB: stateDB},
			MasterKey: masterKey,
			DevMode:   true,
			// BootstrapToken intentionally omitted
		})

		req := reactivateRequest("10.1.0.6", "some-tenant", "anything")
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusServiceUnavailable, rr.Code, "unconfigured bootstrap token should return 503")
	})

	t.Run("reactivated tenant key is usable for authenticated requests", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		_, err := stateDB.Exec(
			`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
			 VALUES (?, ?, ?, 'suspended', 1, 1)`,
			"tenant-usekey", "Suspended Tenant", "usekey@example.com",
		)
		require.NoError(t, err)
		_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`, "tenant-usekey")
		require.NoError(t, err)

		srv := NewServer(ServerConfig{
			Store:          &db.Store{StateDB: stateDB},
			MasterKey:      masterKey,
			DevMode:        true,
			BootstrapTokens: []string{bootstrapToken},
		})

		// Step 1: reactivate the tenant.
		reactReq := reactivateRequest("10.1.0.7", "tenant-usekey", bootstrapToken)
		reactRR := httptest.NewRecorder()
		srv.ServeHTTP(reactRR, reactReq)
		require.Equal(t, http.StatusOK, reactRR.Code, "reactivation should return 200, got: %s", reactRR.Body.String())

		var resp ReactivateResponse
		require.NoError(t, json.NewDecoder(reactRR.Body).Decode(&resp))

		// Step 2: use the new key to call an authenticated endpoint.
		tenantReq := httptest.NewRequest(http.MethodGet, "/v1/tenant", nil)
		tenantReq.Header.Set("Authorization", "Bearer "+resp.APIKey)
		tenantRR := httptest.NewRecorder()
		srv.ServeHTTP(tenantRR, tenantReq)

		assert.Equal(t, http.StatusOK, tenantRR.Code,
			"reactivated key should authenticate successfully: %s", tenantRR.Body.String())

		// Verify the returned tenant is actually active.
		var tenant TenantResponse
		require.NoError(t, json.NewDecoder(tenantRR.Body).Decode(&tenant))
		assert.Equal(t, "active", tenant.Status, "tenant should show active status")
		assert.Equal(t, "tenant-usekey", tenant.ID)
	})
}
