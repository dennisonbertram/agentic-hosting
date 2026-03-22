package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recoverRequest builds a POST /v1/auth/recover request with a distinct
// source IP so tests do not share the same rate-limit bucket.  The limiter
// is a package-level singleton that persists across all tests in the package,
// so each test must use its own IP to avoid being erroneously rate-limited.
func recoverRequest(ip string, body []byte) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/recover", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// httptest.NewRequest sets RemoteAddr to "192.0.2.1:1234" (loopback-adjacent
	// test address).  We override X-Real-Ip from a loopback RemoteAddr so
	// trustedRealIP picks up the per-test IP.
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Real-Ip", ip)
	return req
}

func TestHandleKeyRecover(t *testing.T) {
	regLimiter.resetForTest()
	const bootstrapToken = "test-bootstrap-token-for-recovery"
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	// newServer creates a fresh in-memory DB with a single active tenant that
	// has NO API keys — the scenario described in issue #12.
	newServer := func(t *testing.T) *Server {
		t.Helper()
		stateDB := testutil.NewStateDB(t)
		_, err := stateDB.Exec(
			`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
			 VALUES (?, ?, ?, 'active', 1, 1)`,
			"tenant-recover", "Recovery Tenant", "recover@example.com",
		)
		require.NoError(t, err)
		_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`, "tenant-recover")
		require.NoError(t, err)

		return NewServer(ServerConfig{
			Store:           &db.Store{StateDB: stateDB},
			MasterKey:       masterKey,
			DevMode:         true,
			BootstrapTokens: []string{bootstrapToken},
		})
	}

	t.Run("valid bootstrap token and email creates a new key", func(t *testing.T) {
		srv := newServer(t)

		body, _ := json.Marshal(KeyRecoverRequest{
			Email:          "recover@example.com",
			BootstrapToken: bootstrapToken,
		})
		req := recoverRequest("10.0.1.1", body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusCreated, rr.Code, "expected 201, got: %s", rr.Body.String())

		var resp KeyRecoverResponse
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
		assert.NotEmpty(t, resp.ID, "key ID should be set")
		assert.NotEmpty(t, resp.Key, "raw key should be present")
		assert.Contains(t, resp.Name, "recovery-", "name should be prefixed with recovery-")
		assert.Greater(t, resp.CreatedAt, int64(0), "created_at should be positive unix timestamp")
		// Key must be in "keyID.secret" format
		assert.Contains(t, resp.Key, ".", "key should be in keyID.secret format")
	})

	t.Run("invalid bootstrap token returns 401", func(t *testing.T) {
		srv := newServer(t)

		body, _ := json.Marshal(KeyRecoverRequest{
			Email:          "recover@example.com",
			BootstrapToken: "wrong-token",
		})
		req := recoverRequest("10.0.1.2", body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code)
	})

	t.Run("unknown email with valid bootstrap token returns 401 (no email enumeration)", func(t *testing.T) {
		srv := newServer(t)

		body, _ := json.Marshal(KeyRecoverRequest{
			Email:          "nobody@example.com",
			BootstrapToken: bootstrapToken,
		})
		req := recoverRequest("10.0.1.3", body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		// Must return 401 (not 404) to prevent tenant email enumeration.
		assert.Equal(t, http.StatusUnauthorized, rr.Code)
	})

	t.Run("invalid email format returns 400", func(t *testing.T) {
		srv := newServer(t)

		body, _ := json.Marshal(KeyRecoverRequest{
			Email:          "not-an-email",
			BootstrapToken: bootstrapToken,
		})
		req := recoverRequest("10.0.1.4", body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusBadRequest, rr.Code)
	})

	t.Run("suspended tenant returns 403", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		_, err := stateDB.Exec(
			`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
			 VALUES (?, ?, ?, 'suspended', 1, 1)`,
			"tenant-suspended", "Suspended", "suspended@example.com",
		)
		require.NoError(t, err)
		_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`, "tenant-suspended")
		require.NoError(t, err)

		srv := NewServer(ServerConfig{
			Store:           &db.Store{StateDB: stateDB},
			MasterKey:       masterKey,
			DevMode:         true,
			BootstrapTokens: []string{bootstrapToken},
		})

		body, _ := json.Marshal(KeyRecoverRequest{
			Email:          "suspended@example.com",
			BootstrapToken: bootstrapToken,
		})
		req := recoverRequest("10.0.1.5", body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusForbidden, rr.Code)
	})

	t.Run("empty bootstrap token in server config returns 503", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		srv := NewServer(ServerConfig{
			Store:     &db.Store{StateDB: stateDB},
			MasterKey: masterKey,
			DevMode:   true,
			// BootstrapToken intentionally omitted — recovery must be unavailable
		})

		body, _ := json.Marshal(KeyRecoverRequest{
			Email:          "anyone@example.com",
			BootstrapToken: "anything",
		})
		req := recoverRequest("10.0.1.6", body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	})

	t.Run("recovered key is usable for authenticated requests", func(t *testing.T) {
		srv := newServer(t)

		// Step 1 — recover a key
		body, _ := json.Marshal(KeyRecoverRequest{
			Email:          "recover@example.com",
			BootstrapToken: bootstrapToken,
		})
		recoverReq := recoverRequest("10.0.1.7", body)
		recoverRR := httptest.NewRecorder()
		srv.ServeHTTP(recoverRR, recoverReq)
		require.Equal(t, http.StatusCreated, recoverRR.Code, "recovery should return 201, got: %s", recoverRR.Body.String())

		var resp KeyRecoverResponse
		require.NoError(t, json.NewDecoder(recoverRR.Body).Decode(&resp))

		// Step 2 — use the recovered key to call an authenticated endpoint
		tenantReq := httptest.NewRequest(http.MethodGet, "/v1/tenant", nil)
		tenantReq.Header.Set("Authorization", "Bearer "+resp.Key)
		tenantRR := httptest.NewRecorder()
		srv.ServeHTTP(tenantRR, tenantReq)

		assert.Equal(t, http.StatusOK, tenantRR.Code,
			"recovered key should authenticate successfully: %s", tenantRR.Body.String())
	})
}

// TestHandleKeyRecover_FullKeyring tests the auto-revocation behavior when the
// tenant's keyring is full (issue #138).
func TestHandleKeyRecover_FullKeyring(t *testing.T) {
	regLimiter.resetForTest()
	const bootstrapToken = "test-bootstrap-token-for-full-keyring"
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	// fillKeyring inserts exactly maxKeysPerTenant keys for the given tenant.
	// If expiredIdx >= 0, that key index will have an expires_at in the past
	// (expired but not yet revoked), making it a candidate for preferential
	// revocation.  Returns the ID of the oldest key.
	fillKeyring := func(t *testing.T, stateDB *db.Store, tenantID string, expiredIdx int) string {
		t.Helper()
		now := time.Now().Unix()
		var oldestID string
		for i := 0; i < maxKeysPerTenant; i++ {
			keyID := fmt.Sprintf("key-%s-%03d", tenantID, i)
			createdAt := now - int64(maxKeysPerTenant-i) // oldest first
			var expiresAt *int64
			if i == expiredIdx {
				past := now - 3600 // expired 1 hour ago
				expiresAt = &past
			}
			_, err := stateDB.StateDB.Exec(
				`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at, expires_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				keyID, tenantID, fmt.Sprintf("key-%d", i), keyID[:8], "hash", createdAt, expiresAt,
			)
			require.NoError(t, err)
			if i == 0 {
				oldestID = keyID
			}
		}
		return oldestID
	}

	// newServerWithStore creates a server with the given store.
	newServerWithStore := func(t *testing.T, store *db.Store) *Server {
		t.Helper()
		return NewServer(ServerConfig{
			Store:           store,
			MasterKey:       masterKey,
			DevMode:         true,
			BootstrapTokens: []string{bootstrapToken},
		})
	}

	t.Run("full keyring auto-revokes expired key", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		_, err := stateDB.Exec(
			`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
			 VALUES (?, ?, ?, 'active', 1, 1)`,
			"tenant-full-exp", "Full Expired", "full-exp@example.com",
		)
		require.NoError(t, err)
		_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`, "tenant-full-exp")
		require.NoError(t, err)

		store := &db.Store{StateDB: stateDB}
		// Fill keyring with key index 5 as expired.
		fillKeyring(t, store, "tenant-full-exp", 5)
		expiredKeyID := "key-tenant-full-exp-005"

		logBuf := captureLog(t)
		srv := newServerWithStore(t, store)

		body, _ := json.Marshal(KeyRecoverRequest{
			Email:          "full-exp@example.com",
			BootstrapToken: bootstrapToken,
		})
		req := recoverRequest("10.0.2.1", body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusCreated, rr.Code, "should succeed: %s", rr.Body.String())

		var resp KeyRecoverResponse
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
		assert.NotEmpty(t, resp.ID)
		assert.NotEmpty(t, resp.Key)
		assert.Equal(t, "key slot was full; revoked oldest key", resp.Warning)
		assert.Equal(t, expiredKeyID, resp.RevokedKeyID,
			"should preferentially revoke the expired key")

		// Verify AUDIT log
		logOutput := logBuf.String()
		assert.Contains(t, logOutput, "AUDIT:")
		assert.Contains(t, logOutput, "action=recovery.key_auto_revoked")
		assert.Contains(t, logOutput, "tenant=tenant-full-exp")
		assert.Contains(t, logOutput, "revoked_key="+expiredKeyID)
	})

	t.Run("full keyring with no expired keys auto-revokes oldest", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		_, err := stateDB.Exec(
			`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
			 VALUES (?, ?, ?, 'active', 1, 1)`,
			"tenant-full-old", "Full Oldest", "full-old@example.com",
		)
		require.NoError(t, err)
		_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`, "tenant-full-old")
		require.NoError(t, err)

		store := &db.Store{StateDB: stateDB}
		// Fill keyring with no expired keys (expiredIdx = -1).
		oldestKeyID := fillKeyring(t, store, "tenant-full-old", -1)

		logBuf := captureLog(t)
		srv := newServerWithStore(t, store)

		body, _ := json.Marshal(KeyRecoverRequest{
			Email:          "full-old@example.com",
			BootstrapToken: bootstrapToken,
		})
		req := recoverRequest("10.0.2.2", body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusCreated, rr.Code, "should succeed: %s", rr.Body.String())

		var resp KeyRecoverResponse
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
		assert.NotEmpty(t, resp.ID)
		assert.NotEmpty(t, resp.Key)
		assert.Equal(t, "key slot was full; revoked oldest key", resp.Warning)
		assert.Equal(t, oldestKeyID, resp.RevokedKeyID,
			"should revoke the oldest key when no expired keys exist")

		// Verify AUDIT log
		logOutput := logBuf.String()
		assert.Contains(t, logOutput, "AUDIT:")
		assert.Contains(t, logOutput, "action=recovery.key_auto_revoked")
		assert.Contains(t, logOutput, "revoked_key="+oldestKeyID)
	})

	t.Run("normal recovery with space has no warning", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		_, err := stateDB.Exec(
			`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
			 VALUES (?, ?, ?, 'active', 1, 1)`,
			"tenant-space", "Has Space", "has-space@example.com",
		)
		require.NoError(t, err)
		_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`, "tenant-space")
		require.NoError(t, err)

		store := &db.Store{StateDB: stateDB}
		// Insert only 5 keys — well under the limit.
		for i := 0; i < 5; i++ {
			_, err := stateDB.Exec(
				`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at)
				 VALUES (?, ?, ?, ?, ?, ?)`,
				fmt.Sprintf("key-space-%d", i), "tenant-space", "key", "prefix"+fmt.Sprintf("%02d", i), "hash", 1,
			)
			require.NoError(t, err)
		}

		srv := newServerWithStore(t, store)

		body, _ := json.Marshal(KeyRecoverRequest{
			Email:          "has-space@example.com",
			BootstrapToken: bootstrapToken,
		})
		req := recoverRequest("10.0.2.3", body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusCreated, rr.Code, "should succeed: %s", rr.Body.String())

		var resp KeyRecoverResponse
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
		assert.NotEmpty(t, resp.ID)
		assert.NotEmpty(t, resp.Key)
		assert.Empty(t, resp.Warning, "no warning when keyring has space")
		assert.Empty(t, resp.RevokedKeyID, "no revoked key when keyring has space")
	})
}
