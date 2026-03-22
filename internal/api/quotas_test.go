package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// quotaRequest builds a PATCH /v1/tenants/{tenantID}/quotas request
// with a distinct source IP so tests do not share the same rate-limit bucket.
func quotaRequest(ip, tenantID, bootstrapToken string, body []byte) *http.Request {
	req := httptest.NewRequest(http.MethodPatch, "/v1/tenants/"+tenantID+"/quotas", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Bootstrap-Token", bootstrapToken)
	// Set loopback RemoteAddr so trustedRealIP uses X-Real-Ip for rate limiting.
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Real-Ip", ip)
	return req
}

func TestHandleQuotaUpdate(t *testing.T) {
	const bootstrapToken = "test-bootstrap-token-for-quotas"
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	t.Run("successful partial update of max_services only", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		_, err := stateDB.Exec(
			`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
			 VALUES (?, ?, ?, 'active', 1, 1)`,
			"tenant-quota-1", "Quota Tenant", "quota@example.com",
		)
		require.NoError(t, err)
		_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`, "tenant-quota-1")
		require.NoError(t, err)

		srv := NewServer(ServerConfig{
			Store:           &db.Store{StateDB: stateDB},
			MasterKey:       masterKey,
			DevMode:         true,
			BootstrapTokens: []string{bootstrapToken},
		})

		body := []byte(`{"max_services": 20}`)
		req := quotaRequest("10.2.0.1", "tenant-quota-1", bootstrapToken, body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code, "expected 200, got: %s", rr.Body.String())

		var resp QuotaUpdateResponse
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
		assert.Equal(t, "tenant-quota-1", resp.TenantID)
		assert.Equal(t, 20, resp.MaxServices, "max_services should be updated to 20")
		assert.Equal(t, 3, resp.MaxDatabases, "max_databases should remain at default (3)")
		assert.Equal(t, 3, resp.MaxBuildsConcurrent, "max_builds_concurrent should remain at default (3)")
		assert.Equal(t, 50, resp.MaxEnvVarsPerService, "max_env_vars_per_service should remain at default (50)")

		// Verify persisted in DB.
		var dbMax int
		err = stateDB.QueryRow(`SELECT max_services FROM tenant_quotas WHERE tenant_id = ?`, "tenant-quota-1").Scan(&dbMax)
		require.NoError(t, err)
		assert.Equal(t, 20, dbMax)
	})

	t.Run("successful update of all fields", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		_, err := stateDB.Exec(
			`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
			 VALUES (?, ?, ?, 'active', 1, 1)`,
			"tenant-quota-all", "Full Quota Tenant", "fullquota@example.com",
		)
		require.NoError(t, err)
		_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`, "tenant-quota-all")
		require.NoError(t, err)

		srv := NewServer(ServerConfig{
			Store:           &db.Store{StateDB: stateDB},
			MasterKey:       masterKey,
			DevMode:         true,
			BootstrapTokens: []string{bootstrapToken},
		})

		body := []byte(`{"max_services": 50, "max_databases": 25, "max_builds_concurrent": 8, "max_env_vars_per_service": 200}`)
		req := quotaRequest("10.2.0.2", "tenant-quota-all", bootstrapToken, body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code, "expected 200, got: %s", rr.Body.String())

		var resp QuotaUpdateResponse
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
		assert.Equal(t, 50, resp.MaxServices)
		assert.Equal(t, 25, resp.MaxDatabases)
		assert.Equal(t, 8, resp.MaxBuildsConcurrent)
		assert.Equal(t, 200, resp.MaxEnvVarsPerService)
	})

	t.Run("negative max_services returns 400", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		_, err := stateDB.Exec(
			`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
			 VALUES (?, ?, ?, 'active', 1, 1)`,
			"tenant-neg", "Neg Tenant", "neg@example.com",
		)
		require.NoError(t, err)
		_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`, "tenant-neg")
		require.NoError(t, err)

		srv := NewServer(ServerConfig{
			Store:           &db.Store{StateDB: stateDB},
			MasterKey:       masterKey,
			DevMode:         true,
			BootstrapTokens: []string{bootstrapToken},
		})

		body := []byte(`{"max_services": -5}`)
		req := quotaRequest("10.2.0.3", "tenant-neg", bootstrapToken, body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusBadRequest, rr.Code, "negative value should return 400")
		assert.Contains(t, rr.Body.String(), "max_services must be at least 1")
	})

	t.Run("zero max_databases returns 400", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		_, err := stateDB.Exec(
			`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
			 VALUES (?, ?, ?, 'active', 1, 1)`,
			"tenant-zero", "Zero Tenant", "zero@example.com",
		)
		require.NoError(t, err)
		_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`, "tenant-zero")
		require.NoError(t, err)

		srv := NewServer(ServerConfig{
			Store:           &db.Store{StateDB: stateDB},
			MasterKey:       masterKey,
			DevMode:         true,
			BootstrapTokens: []string{bootstrapToken},
		})

		body := []byte(`{"max_databases": 0}`)
		req := quotaRequest("10.2.0.4", "tenant-zero", bootstrapToken, body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusBadRequest, rr.Code, "zero value should return 400")
		assert.Contains(t, rr.Body.String(), "max_databases must be at least 1")
	})

	t.Run("exceeding max_services cap returns 400", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		_, err := stateDB.Exec(
			`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
			 VALUES (?, ?, ?, 'active', 1, 1)`,
			"tenant-cap", "Cap Tenant", "cap@example.com",
		)
		require.NoError(t, err)
		_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`, "tenant-cap")
		require.NoError(t, err)

		srv := NewServer(ServerConfig{
			Store:           &db.Store{StateDB: stateDB},
			MasterKey:       masterKey,
			DevMode:         true,
			BootstrapTokens: []string{bootstrapToken},
		})

		body := []byte(`{"max_services": 101}`)
		req := quotaRequest("10.2.0.5", "tenant-cap", bootstrapToken, body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusBadRequest, rr.Code, "exceeding cap should return 400")
		assert.Contains(t, rr.Body.String(), "must not exceed 100")
	})

	t.Run("exceeding max_databases cap returns 400", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		_, err := stateDB.Exec(
			`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
			 VALUES (?, ?, ?, 'active', 1, 1)`,
			"tenant-dbcap", "DBCap Tenant", "dbcap@example.com",
		)
		require.NoError(t, err)
		_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`, "tenant-dbcap")
		require.NoError(t, err)

		srv := NewServer(ServerConfig{
			Store:           &db.Store{StateDB: stateDB},
			MasterKey:       masterKey,
			DevMode:         true,
			BootstrapTokens: []string{bootstrapToken},
		})

		body := []byte(`{"max_databases": 51}`)
		req := quotaRequest("10.2.0.6", "tenant-dbcap", bootstrapToken, body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusBadRequest, rr.Code)
		assert.Contains(t, rr.Body.String(), "must not exceed 50")
	})

	t.Run("exceeding max_builds_concurrent cap returns 400", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		_, err := stateDB.Exec(
			`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
			 VALUES (?, ?, ?, 'active', 1, 1)`,
			"tenant-bldcap", "BldCap Tenant", "bldcap@example.com",
		)
		require.NoError(t, err)
		_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`, "tenant-bldcap")
		require.NoError(t, err)

		srv := NewServer(ServerConfig{
			Store:           &db.Store{StateDB: stateDB},
			MasterKey:       masterKey,
			DevMode:         true,
			BootstrapTokens: []string{bootstrapToken},
		})

		body := []byte(`{"max_builds_concurrent": 11}`)
		req := quotaRequest("10.2.0.7", "tenant-bldcap", bootstrapToken, body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusBadRequest, rr.Code)
		assert.Contains(t, rr.Body.String(), "must not exceed 10")
	})

	t.Run("exceeding max_env_vars_per_service cap returns 400", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		_, err := stateDB.Exec(
			`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
			 VALUES (?, ?, ?, 'active', 1, 1)`,
			"tenant-envcap", "EnvCap Tenant", "envcap@example.com",
		)
		require.NoError(t, err)
		_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`, "tenant-envcap")
		require.NoError(t, err)

		srv := NewServer(ServerConfig{
			Store:           &db.Store{StateDB: stateDB},
			MasterKey:       masterKey,
			DevMode:         true,
			BootstrapTokens: []string{bootstrapToken},
		})

		body := []byte(`{"max_env_vars_per_service": 501}`)
		req := quotaRequest("10.2.0.8", "tenant-envcap", bootstrapToken, body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusBadRequest, rr.Code)
		assert.Contains(t, rr.Body.String(), "must not exceed 500")
	})

	t.Run("bootstrap token required - no token returns 401", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)

		srv := NewServer(ServerConfig{
			Store:           &db.Store{StateDB: stateDB},
			MasterKey:       masterKey,
			DevMode:         true,
			BootstrapTokens: []string{bootstrapToken},
		})

		req := httptest.NewRequest(http.MethodPatch, "/v1/tenants/some-tenant/quotas",
			bytes.NewReader([]byte(`{"max_services": 10}`)))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("X-Real-Ip", "10.2.0.9")
		// No X-Bootstrap-Token header set.

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code, "missing bootstrap token should return 401")
	})

	t.Run("wrong bootstrap token returns 401", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)

		srv := NewServer(ServerConfig{
			Store:           &db.Store{StateDB: stateDB},
			MasterKey:       masterKey,
			DevMode:         true,
			BootstrapTokens: []string{bootstrapToken},
		})

		body := []byte(`{"max_services": 10}`)
		req := quotaRequest("10.2.0.10", "some-tenant", "wrong-token", body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code, "wrong bootstrap token should return 401")
	})

	t.Run("invalid tenant ID returns 404", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)

		srv := NewServer(ServerConfig{
			Store:           &db.Store{StateDB: stateDB},
			MasterKey:       masterKey,
			DevMode:         true,
			BootstrapTokens: []string{bootstrapToken},
		})

		body := []byte(`{"max_services": 10}`)
		req := quotaRequest("10.2.0.11", "does-not-exist", bootstrapToken, body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusNotFound, rr.Code, "non-existent tenant should return 404")
	})

	t.Run("AUDIT log is produced", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		_, err := stateDB.Exec(
			`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
			 VALUES (?, ?, ?, 'active', 1, 1)`,
			"tenant-audit", "Audit Tenant", "audit@example.com",
		)
		require.NoError(t, err)
		_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`, "tenant-audit")
		require.NoError(t, err)

		srv := NewServer(ServerConfig{
			Store:           &db.Store{StateDB: stateDB},
			MasterKey:       masterKey,
			DevMode:         true,
			BootstrapTokens: []string{bootstrapToken},
		})

		logBuf := captureLog(t)

		body := []byte(`{"max_services": 42}`)
		req := quotaRequest("10.2.0.12", "tenant-audit", bootstrapToken, body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code)

		logOutput := logBuf.String()
		assert.Contains(t, logOutput, "AUDIT:")
		assert.Contains(t, logOutput, "action=tenant.quotas_updated")
		assert.Contains(t, logOutput, "tenant=tenant-audit")
	})

	t.Run("empty body updates nothing and returns current quotas", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		_, err := stateDB.Exec(
			`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
			 VALUES (?, ?, ?, 'active', 1, 1)`,
			"tenant-noop", "Noop Tenant", "noop@example.com",
		)
		require.NoError(t, err)
		_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`, "tenant-noop")
		require.NoError(t, err)

		srv := NewServer(ServerConfig{
			Store:           &db.Store{StateDB: stateDB},
			MasterKey:       masterKey,
			DevMode:         true,
			BootstrapTokens: []string{bootstrapToken},
		})

		body := []byte(`{}`)
		req := quotaRequest("10.2.0.13", "tenant-noop", bootstrapToken, body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code, "empty body should return 200, got: %s", rr.Body.String())

		var resp QuotaUpdateResponse
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
		assert.Equal(t, 5, resp.MaxServices, "should return default max_services")
		assert.Equal(t, 3, resp.MaxDatabases, "should return default max_databases")
	})

	t.Run("no bootstrap tokens configured returns 503", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)

		srv := NewServer(ServerConfig{
			Store:     &db.Store{StateDB: stateDB},
			MasterKey: masterKey,
			DevMode:   true,
			// No BootstrapTokens
		})

		body := []byte(`{"max_services": 10}`)
		req := quotaRequest("10.2.0.14", "some-tenant", "anything", body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	})

	t.Run("boundary values at cap are accepted", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		_, err := stateDB.Exec(
			`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
			 VALUES (?, ?, ?, 'active', 1, 1)`,
			"tenant-boundary", "Boundary Tenant", "boundary@example.com",
		)
		require.NoError(t, err)
		_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`, "tenant-boundary")
		require.NoError(t, err)

		srv := NewServer(ServerConfig{
			Store:           &db.Store{StateDB: stateDB},
			MasterKey:       masterKey,
			DevMode:         true,
			BootstrapTokens: []string{bootstrapToken},
		})

		// Exact cap values should be accepted.
		body := []byte(`{"max_services": 100, "max_databases": 50, "max_builds_concurrent": 10, "max_env_vars_per_service": 500}`)
		req := quotaRequest("10.2.0.15", "tenant-boundary", bootstrapToken, body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code, "cap values should be accepted, got: %s", rr.Body.String())

		var resp QuotaUpdateResponse
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
		assert.Equal(t, 100, resp.MaxServices)
		assert.Equal(t, 50, resp.MaxDatabases)
		assert.Equal(t, 10, resp.MaxBuildsConcurrent)
		assert.Equal(t, 500, resp.MaxEnvVarsPerService)
	})

}
