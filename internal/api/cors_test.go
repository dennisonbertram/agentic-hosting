package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/middleware"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServerCORS_PreflightOnAuthenticatedRouteSkipsAuth(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: []byte("0123456789abcdef0123456789abcdef"),
		DevMode:   true,
	})

	req := httptest.NewRequest(http.MethodOptions, "/v1/services", nil)
	req.Header.Set("Origin", "https://agentic.hosting")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type,idempotency-key")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code, "preflight must not return 401")
	assert.Equal(t, "https://agentic.hosting", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Contains(t, rec.Header().Get("Access-Control-Allow-Headers"), "Authorization")
	assert.Contains(t, rec.Header().Get("Access-Control-Allow-Headers"), "Content-Type")
	assert.Contains(t, rec.Header().Get("Access-Control-Allow-Headers"), "Idempotency-Key")
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Credentials"))
	assert.NotEqual(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Contains(t, rec.Header().Values("Vary"), "Origin")
}

func TestServerCORS_PublicGETReflectsAllowedOrigin(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: []byte("0123456789abcdef0123456789abcdef"),
		DevMode:   true,
	})

	for _, origin := range middleware.DefaultCORSOrigins {
		req := httptest.NewRequest(http.MethodGet, "/v1/system/health", nil)
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, origin)
		assert.Equal(t, origin, rec.Header().Get("Access-Control-Allow-Origin"), origin)
		assert.Contains(t, rec.Header().Values("Vary"), "Origin")
	}
}

func TestServerCORS_UnknownOriginHasNoAllowOrigin(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: []byte("0123456789abcdef0123456789abcdef"),
		DevMode:   true,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/system/health", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Contains(t, rec.Header().Values("Vary"), "Origin")
}

func TestServerCORS_CustomAllowlist(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	srv := NewServer(ServerConfig{
		Store:       &db.Store{StateDB: stateDB},
		MasterKey:   []byte("0123456789abcdef0123456789abcdef"),
		DevMode:     true,
		CORSOrigins: []string{"https://dash.example.com"},
	})

	allowed := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/system/health", nil)
	req.Header.Set("Origin", "https://dash.example.com")
	srv.ServeHTTP(allowed, req)
	assert.Equal(t, "https://dash.example.com", allowed.Header().Get("Access-Control-Allow-Origin"))

	denied := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/system/health", nil)
	req.Header.Set("Origin", "https://agentic.hosting")
	srv.ServeHTTP(denied, req)
	assert.Empty(t, denied.Header().Get("Access-Control-Allow-Origin"))
}

func TestServerCORS_EmptyAllowlistDisables(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	srv := NewServer(ServerConfig{
		Store:       &db.Store{StateDB: stateDB},
		MasterKey:   []byte("0123456789abcdef0123456789abcdef"),
		DevMode:     true,
		CORSOrigins: []string{},
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/system/health", nil)
	req.Header.Set("Origin", "https://agentic.hosting")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
}
