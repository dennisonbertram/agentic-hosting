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

// tokenRequest builds a POST /v1/system/bootstrap-token/validate request with
// a per-test source IP to avoid sharing rate-limit buckets across tests.
func tokenRequest(ip string, body []byte) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/system/bootstrap-token/validate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Real-Ip", ip)
	return req
}

// registerRequest builds a POST /v1/tenants/register request with a per-test
// source IP and the given bootstrap token header.
func registerRequest(ip, bootstrapToken string, body []byte) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/tenants/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Real-Ip", ip)
	if bootstrapToken != "" {
		req.Header.Set("X-Bootstrap-Token", bootstrapToken)
	}
	return req
}

func TestValidateBootstrapToken(t *testing.T) {
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	t.Run("single token matches", func(t *testing.T) {
		assert.True(t, validateBootstrapToken("my-token", []string{"my-token"}, masterKey))
	})

	t.Run("single token does not match", func(t *testing.T) {
		assert.False(t, validateBootstrapToken("wrong-token", []string{"my-token"}, masterKey))
	})

	t.Run("multiple tokens first matches", func(t *testing.T) {
		tokens := []string{"token-a", "token-b", "token-c"}
		assert.True(t, validateBootstrapToken("token-a", tokens, masterKey))
	})

	t.Run("multiple tokens last matches", func(t *testing.T) {
		tokens := []string{"token-a", "token-b", "token-c"}
		assert.True(t, validateBootstrapToken("token-c", tokens, masterKey))
	})

	t.Run("multiple tokens middle matches", func(t *testing.T) {
		tokens := []string{"token-a", "token-b", "token-c"}
		assert.True(t, validateBootstrapToken("token-b", tokens, masterKey))
	})

	t.Run("multiple tokens none match", func(t *testing.T) {
		tokens := []string{"token-a", "token-b", "token-c"}
		assert.False(t, validateBootstrapToken("token-d", tokens, masterKey))
	})

	t.Run("empty token list rejects everything", func(t *testing.T) {
		assert.False(t, validateBootstrapToken("any-token", []string{}, masterKey))
	})

	t.Run("empty provided token", func(t *testing.T) {
		assert.False(t, validateBootstrapToken("", []string{"token-a"}, masterKey))
	})

	t.Run("empty provided against empty list", func(t *testing.T) {
		assert.False(t, validateBootstrapToken("", []string{}, masterKey))
	})
}

func TestHandleBootstrapTokenValidate(t *testing.T) {
	regLimiter.resetForTest()
	const tokenA = "test-bootstrap-token-aaaaaaa-32chars"
	const tokenB = "test-bootstrap-token-bbbbbbb-32chars"
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	newServer := func(t *testing.T, tokens []string) *Server {
		t.Helper()
		stateDB := testutil.NewStateDB(t)
		return NewServer(ServerConfig{
			Store:           &db.Store{StateDB: stateDB},
			MasterKey:       masterKey,
			DevMode:         true,
			BootstrapTokens: tokens,
		})
	}

	t.Run("valid single token returns valid=true", func(t *testing.T) {
		srv := newServer(t, []string{tokenA})

		body, _ := json.Marshal(BootstrapTokenValidateRequest{Token: tokenA})
		req := tokenRequest("10.1.1.1", body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code)
		var resp BootstrapTokenValidateResponse
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
		assert.True(t, resp.Valid)
	})

	t.Run("invalid token returns valid=false", func(t *testing.T) {
		srv := newServer(t, []string{tokenA})

		body, _ := json.Marshal(BootstrapTokenValidateRequest{Token: "wrong-token"})
		req := tokenRequest("10.1.1.2", body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code)
		var resp BootstrapTokenValidateResponse
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
		assert.False(t, resp.Valid)
	})

	t.Run("both tokens valid during rotation", func(t *testing.T) {
		srv := newServer(t, []string{tokenA, tokenB})

		// Token A should be valid
		bodyA, _ := json.Marshal(BootstrapTokenValidateRequest{Token: tokenA})
		reqA := tokenRequest("10.1.1.3", bodyA)
		rrA := httptest.NewRecorder()
		srv.ServeHTTP(rrA, reqA)

		require.Equal(t, http.StatusOK, rrA.Code)
		var respA BootstrapTokenValidateResponse
		require.NoError(t, json.NewDecoder(rrA.Body).Decode(&respA))
		assert.True(t, respA.Valid)

		// Token B should also be valid
		bodyB, _ := json.Marshal(BootstrapTokenValidateRequest{Token: tokenB})
		reqB := tokenRequest("10.1.1.4", bodyB)
		rrB := httptest.NewRecorder()
		srv.ServeHTTP(rrB, reqB)

		require.Equal(t, http.StatusOK, rrB.Code)
		var respB BootstrapTokenValidateResponse
		require.NoError(t, json.NewDecoder(rrB.Body).Decode(&respB))
		assert.True(t, respB.Valid)
	})

	t.Run("no tokens configured returns valid=false", func(t *testing.T) {
		srv := newServer(t, nil)

		body, _ := json.Marshal(BootstrapTokenValidateRequest{Token: "any-token"})
		req := tokenRequest("10.1.1.5", body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code)
		var resp BootstrapTokenValidateResponse
		require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
		assert.False(t, resp.Valid)
	})

	t.Run("invalid request body returns 400", func(t *testing.T) {
		srv := newServer(t, []string{tokenA})

		req := tokenRequest("10.1.1.6", []byte("not json"))
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusBadRequest, rr.Code)
	})
}

func TestRegistrationWithMultipleBootstrapTokens(t *testing.T) {
	regLimiter.resetForTest()
	const tokenA = "test-bootstrap-token-aaaaaaa-32chars"
	const tokenB = "test-bootstrap-token-bbbbbbb-32chars"
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	newServer := func(t *testing.T, tokens []string) *Server {
		t.Helper()
		stateDB := testutil.NewStateDB(t)
		return NewServer(ServerConfig{
			Store:           &db.Store{StateDB: stateDB},
			MasterKey:       masterKey,
			DevMode:         true,
			BootstrapTokens: tokens,
		})
	}

	t.Run("registration with first token succeeds", func(t *testing.T) {
		srv := newServer(t, []string{tokenA, tokenB})

		body, _ := json.Marshal(RegisterRequest{Name: "TenantA", Email: "a@example.com"})
		req := registerRequest("10.2.1.1", tokenA, body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())
	})

	t.Run("registration with second token succeeds", func(t *testing.T) {
		srv := newServer(t, []string{tokenA, tokenB})

		body, _ := json.Marshal(RegisterRequest{Name: "TenantB", Email: "b@example.com"})
		req := registerRequest("10.2.1.2", tokenB, body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())
	})

	t.Run("registration with unknown token fails", func(t *testing.T) {
		srv := newServer(t, []string{tokenA, tokenB})

		body, _ := json.Marshal(RegisterRequest{Name: "TenantC", Email: "c@example.com"})
		req := registerRequest("10.2.1.3", "unknown-token", body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code)
	})

	t.Run("registration with no tokens configured returns 503", func(t *testing.T) {
		srv := newServer(t, nil)

		body, _ := json.Marshal(RegisterRequest{Name: "TenantD", Email: "d@example.com"})
		req := registerRequest("10.2.1.4", tokenA, body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	})

	t.Run("registration with single token still works (backward compatibility)", func(t *testing.T) {
		srv := newServer(t, []string{tokenA})

		body, _ := json.Marshal(RegisterRequest{Name: "TenantE", Email: "e@example.com"})
		req := registerRequest("10.2.1.5", tokenA, body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())
	})
}

func TestRecoveryWithMultipleBootstrapTokens(t *testing.T) {
	regLimiter.resetForTest()
	const tokenA = "test-bootstrap-token-aaaaaaa-32chars"
	const tokenB = "test-bootstrap-token-bbbbbbb-32chars"
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	newServer := func(t *testing.T, tokens []string) *Server {
		t.Helper()
		stateDB := testutil.NewStateDB(t)
		_, err := stateDB.Exec(
			`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
			 VALUES (?, ?, ?, 'active', 1, 1)`,
			"tenant-rot", "Rotation Tenant", "rotation@example.com",
		)
		require.NoError(t, err)
		_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`, "tenant-rot")
		require.NoError(t, err)

		return NewServer(ServerConfig{
			Store:           &db.Store{StateDB: stateDB},
			MasterKey:       masterKey,
			DevMode:         true,
			BootstrapTokens: tokens,
		})
	}

	t.Run("recovery with first token succeeds", func(t *testing.T) {
		srv := newServer(t, []string{tokenA, tokenB})

		body, _ := json.Marshal(KeyRecoverRequest{
			Email:          "rotation@example.com",
			BootstrapToken: tokenA,
		})
		req := recoverRequest("10.3.1.1", body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())
	})

	t.Run("recovery with second token succeeds", func(t *testing.T) {
		srv := newServer(t, []string{tokenA, tokenB})

		body, _ := json.Marshal(KeyRecoverRequest{
			Email:          "rotation@example.com",
			BootstrapToken: tokenB,
		})
		req := recoverRequest("10.3.1.2", body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusCreated, rr.Code, "body: %s", rr.Body.String())
	})

	t.Run("recovery with unknown token fails", func(t *testing.T) {
		srv := newServer(t, []string{tokenA, tokenB})

		body, _ := json.Marshal(KeyRecoverRequest{
			Email:          "rotation@example.com",
			BootstrapToken: "wrong-token",
		})
		req := recoverRequest("10.3.1.3", body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code)
	})

	t.Run("recovery with no tokens configured returns 503", func(t *testing.T) {
		srv := newServer(t, nil)

		body, _ := json.Marshal(KeyRecoverRequest{
			Email:          "rotation@example.com",
			BootstrapToken: tokenA,
		})
		req := recoverRequest("10.3.1.4", body)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	})
}

func TestTokenRotationWorkflow(t *testing.T) {
	regLimiter.resetForTest()
	// Simulates the full rotation workflow:
	// 1. Start with token A only
	// 2. Register with token A (works)
	// 3. Add token B alongside A (simulating deployment)
	// 4. Register with token B (works)
	// 5. Register with token A (still works)
	// 6. Remove token A, keep only B (simulating second deployment)
	// 7. Register with token A (fails)
	// 8. Register with token B (works)
	const tokenA = "rotation-test-token-aaaaaaa-32charslong"
	const tokenB = "rotation-test-token-bbbbbbb-32charslong"
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	// Phase 1: Only token A
	stateDB := testutil.NewStateDB(t)
	srv := NewServer(ServerConfig{
		Store:           &db.Store{StateDB: stateDB},
		MasterKey:       masterKey,
		DevMode:         true,
		BootstrapTokens: []string{tokenA},
	})

	body, _ := json.Marshal(RegisterRequest{Name: "Phase1", Email: "phase1@example.com"})
	req := registerRequest("10.4.1.1", tokenA, body)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	require.Equal(t, http.StatusCreated, rr.Code, "Phase 1: token A should work")

	// Phase 2: Both tokens A and B (rotation in progress)
	srv2 := NewServer(ServerConfig{
		Store:           &db.Store{StateDB: stateDB},
		MasterKey:       masterKey,
		DevMode:         true,
		BootstrapTokens: []string{tokenB, tokenA}, // new token first
	})

	body2a, _ := json.Marshal(RegisterRequest{Name: "Phase2A", Email: "phase2a@example.com"})
	req2a := registerRequest("10.4.1.2", tokenA, body2a)
	rr2a := httptest.NewRecorder()
	srv2.ServeHTTP(rr2a, req2a)
	require.Equal(t, http.StatusCreated, rr2a.Code, "Phase 2: old token A should still work")

	body2b, _ := json.Marshal(RegisterRequest{Name: "Phase2B", Email: "phase2b@example.com"})
	req2b := registerRequest("10.4.1.3", tokenB, body2b)
	rr2b := httptest.NewRecorder()
	srv2.ServeHTTP(rr2b, req2b)
	require.Equal(t, http.StatusCreated, rr2b.Code, "Phase 2: new token B should work")

	// Phase 3: Only token B (rotation complete, old token removed)
	srv3 := NewServer(ServerConfig{
		Store:           &db.Store{StateDB: stateDB},
		MasterKey:       masterKey,
		DevMode:         true,
		BootstrapTokens: []string{tokenB},
	})

	body3a, _ := json.Marshal(RegisterRequest{Name: "Phase3A", Email: "phase3a@example.com"})
	req3a := registerRequest("10.4.1.4", tokenA, body3a)
	rr3a := httptest.NewRecorder()
	srv3.ServeHTTP(rr3a, req3a)
	assert.Equal(t, http.StatusUnauthorized, rr3a.Code, "Phase 3: old token A should be rejected")

	body3b, _ := json.Marshal(RegisterRequest{Name: "Phase3B", Email: "phase3b@example.com"})
	req3b := registerRequest("10.4.1.5", tokenB, body3b)
	rr3b := httptest.NewRecorder()
	srv3.ServeHTTP(rr3b, req3b)
	require.Equal(t, http.StatusCreated, rr3b.Code, "Phase 3: new token B should work")
}
