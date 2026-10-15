package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCORS_PreflightAllowedOrigin(t *testing.T) {
	h := CORS(nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("preflight must not reach the next handler")
	}))
	for _, origin := range DefaultCORSOrigins {
		req := httptest.NewRequest(http.MethodOptions, "/v1/services", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "POST")
		req.Header.Set("Access-Control-Request-Headers", "authorization, content-type, idempotency-key")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusNoContent, rec.Code, origin)
		assert.Equal(t, origin, rec.Header().Get("Access-Control-Allow-Origin"), origin)
		assert.NotEqual(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
		assert.Empty(t, rec.Header().Get("Access-Control-Allow-Credentials"))
		assert.Contains(t, rec.Header().Get("Access-Control-Allow-Headers"), "Authorization")
		assert.Contains(t, rec.Header().Get("Access-Control-Allow-Headers"), "Content-Type")
		assert.Contains(t, rec.Header().Get("Access-Control-Allow-Headers"), "Idempotency-Key")
		assert.Contains(t, rec.Header().Get("Access-Control-Allow-Methods"), "GET")
		assert.Contains(t, rec.Header().Get("Access-Control-Allow-Methods"), "POST")
		assert.Contains(t, rec.Header().Get("Access-Control-Allow-Methods"), "PATCH")
		assert.Contains(t, rec.Header().Get("Access-Control-Allow-Methods"), "DELETE")
		assert.Contains(t, rec.Header().Values("Vary"), "Origin")
	}
}

func TestCORS_ActualRequestAllowedOrigin(t *testing.T) {
	h := CORS(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/system/health", nil)
	req.Header.Set("Origin", "https://agentic.hosting")
	req.Header.Set("Authorization", "Bearer key.secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "https://agentic.hosting", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Credentials"))
	assert.Contains(t, rec.Header().Values("Vary"), "Origin")
}

func TestCORS_DeniedOriginHasNoAllowOrigin(t *testing.T) {
	h := CORS(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/system/health", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Contains(t, rec.Header().Values("Vary"), "Origin")
}

func TestCORS_PreflightDeniedOrigin(t *testing.T) {
	h := CORS(nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("preflight must not reach the next handler")
	}))
	req := httptest.NewRequest(http.MethodOptions, "/v1/services", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Contains(t, rec.Header().Values("Vary"), "Origin")
}

func TestCORS_WildcardNeverAllowed(t *testing.T) {
	h := CORS([]string{"*"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://agentic.hosting")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
	assert.NotEqual(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
}

func TestCORS_CustomAllowlist(t *testing.T) {
	h := CORS([]string{"https://dash.example.com"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	allowed := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://dash.example.com")
	h.ServeHTTP(allowed, req)
	assert.Equal(t, "https://dash.example.com", allowed.Header().Get("Access-Control-Allow-Origin"))

	denied := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://agentic.hosting")
	h.ServeHTTP(denied, req)
	assert.Empty(t, denied.Header().Get("Access-Control-Allow-Origin"))
}

func TestCORS_EmptyAllowlistDisables(t *testing.T) {
	h := CORS([]string{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://agentic.hosting")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
}

func TestCORS_NoOriginStillVaries(t *testing.T) {
	h := CORS(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Contains(t, rec.Header().Values("Vary"), "Origin")
}

func TestNormalizeOrigin(t *testing.T) {
	ok, allowed := NormalizeOrigin("https://Agentic.Hosting")
	require.True(t, allowed)
	assert.Equal(t, "https://agentic.hosting", ok)

	_, allowed = NormalizeOrigin("*")
	assert.False(t, allowed)
	_, allowed = NormalizeOrigin("https://evil.example/path")
	assert.False(t, allowed)
	_, allowed = NormalizeOrigin("ftp://agentic.hosting")
	assert.False(t, allowed)
	_, allowed = NormalizeOrigin("https://user:pass@agentic.hosting")
	assert.False(t, allowed)
}
