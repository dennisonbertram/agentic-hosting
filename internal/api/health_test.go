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

// TestHealthDetailed_DefaultUsesCache verifies that the detailed health endpoint
// returns cached results within the TTL window when ?fresh is not set.
func TestHealthDetailed_DefaultUsesCache(t *testing.T) {
	resetDetailedHealthCache()
	t.Cleanup(resetDetailedHealthCache)

	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
	})

	// First request — populates the cache.
	rr1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodGet, "/v1/system/health/detailed", nil)
	req1.Header.Set("Authorization", "Bearer "+token)
	srv.ServeHTTP(rr1, req1)
	require.Equal(t, http.StatusOK, rr1.Code)

	var resp1 DetailedHealthResponse
	require.NoError(t, json.NewDecoder(rr1.Body).Decode(&resp1))
	require.Equal(t, "ok", resp1.Status, "DB is up, status should be ok")

	// Close the DB so buildDetailedHealth would return "degraded" if called.
	stateDB.Close()

	// Second request — without ?fresh, should return the cached "ok" response.
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/v1/system/health/detailed", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	srv.ServeHTTP(rr2, req2)
	require.Equal(t, http.StatusOK, rr2.Code)

	var resp2 DetailedHealthResponse
	require.NoError(t, json.NewDecoder(rr2.Body).Decode(&resp2))
	assert.Equal(t, "ok", resp2.Status,
		"cached response should still show 'ok' even though DB is now closed")
}

// TestHealthDetailed_FreshBypassesCache verifies that ?fresh=true skips the cache
// and returns live probe results, allowing operators to see real-time status
// during incident response.
func TestHealthDetailed_FreshBypassesCache(t *testing.T) {
	resetDetailedHealthCache()
	t.Cleanup(resetDetailedHealthCache)

	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
	})

	// First request — populates the cache with "ok".
	rr1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodGet, "/v1/system/health/detailed", nil)
	req1.Header.Set("Authorization", "Bearer "+token)
	srv.ServeHTTP(rr1, req1)
	require.Equal(t, http.StatusOK, rr1.Code)

	var resp1 DetailedHealthResponse
	require.NoError(t, json.NewDecoder(rr1.Body).Decode(&resp1))
	require.Equal(t, "ok", resp1.Status)

	// Close the DB so buildDetailedHealth will return "degraded".
	stateDB.Close()

	// Second request WITH ?fresh=true — should bypass cache and return "degraded".
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/v1/system/health/detailed?fresh=true", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	srv.ServeHTTP(rr2, req2)
	require.Equal(t, http.StatusOK, rr2.Code)

	var resp2 DetailedHealthResponse
	require.NoError(t, json.NewDecoder(rr2.Body).Decode(&resp2))
	assert.Equal(t, "degraded", resp2.Status,
		"fresh=true should bypass cache and detect the closed DB as degraded")
}

// TestHealthDetailed_FreshUpdatesCache verifies that after a ?fresh=true request,
// subsequent normal requests return the fresh result (i.e. the cache was updated).
func TestHealthDetailed_FreshUpdatesCache(t *testing.T) {
	resetDetailedHealthCache()
	t.Cleanup(resetDetailedHealthCache)

	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
	})

	// First request — populates cache with "ok".
	rr1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodGet, "/v1/system/health/detailed", nil)
	req1.Header.Set("Authorization", "Bearer "+token)
	srv.ServeHTTP(rr1, req1)
	require.Equal(t, http.StatusOK, rr1.Code)

	// Close DB, then do a fresh request to update cache to "degraded".
	stateDB.Close()

	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/v1/system/health/detailed?fresh=true", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	srv.ServeHTTP(rr2, req2)
	require.Equal(t, http.StatusOK, rr2.Code)

	var resp2 DetailedHealthResponse
	require.NoError(t, json.NewDecoder(rr2.Body).Decode(&resp2))
	require.Equal(t, "degraded", resp2.Status)

	// Third request — normal (no fresh), should now return cached "degraded".
	rr3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodGet, "/v1/system/health/detailed", nil)
	req3.Header.Set("Authorization", "Bearer "+token)
	srv.ServeHTTP(rr3, req3)
	require.Equal(t, http.StatusOK, rr3.Code)

	var resp3 DetailedHealthResponse
	require.NoError(t, json.NewDecoder(rr3.Body).Decode(&resp3))
	assert.Equal(t, "degraded", resp3.Status,
		"cache should have been updated by the fresh request, so normal request returns degraded")
}
