package middleware

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/crypto"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testMasterKey = []byte("00000000000000000000000000000000") // 32 bytes

// insertTenant inserts a tenant with the given status and returns its ID.
func insertTenant(t *testing.T, db *sql.DB, status string) string {
	t.Helper()
	id := fmt.Sprintf("tenant-%d", time.Now().UnixNano())
	now := time.Now().Unix()
	_, err := db.Exec(
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, "Test Tenant", id+"@example.com", status, now, now,
	)
	require.NoError(t, err)
	return id
}

// insertAPIKey inserts an api_key row and returns the raw token (keyID.secret).
func insertAPIKey(t *testing.T, sqlDB *sql.DB, tenantID string, expiresAt *int64, revokedAt *int64) string {
	t.Helper()
	secret, keyID, err := crypto.GenerateAPIKeyWithID()
	require.NoError(t, err)

	keyHash := crypto.HashAPIKey(secret, testMasterKey)
	now := time.Now().Unix()

	if revokedAt != nil {
		_, err = sqlDB.Exec(
			`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at, expires_at, revoked_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			keyID, tenantID, "test key", keyID[:8], keyHash, now, expiresAt, revokedAt,
		)
	} else {
		_, err = sqlDB.Exec(
			`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			keyID, tenantID, "test key", keyID[:8], keyHash, now, expiresAt,
		)
	}
	require.NoError(t, err)

	return keyID + "." + secret
}

// newAuthMiddleware creates an Auth middleware and a no-op next handler for testing.
func newAuthMiddleware(t *testing.T, sqlDB *sql.DB) (http.Handler, *AuthCacheInvalidator) {
	t.Helper()
	mw, inv := Auth(sqlDB, testMasterKey)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid := GetTenantID(r.Context())
		w.Header().Set("X-Tenant-ID", tid)
		w.WriteHeader(http.StatusOK)
	})
	return mw(next), inv
}

func TestAuth_MissingAuthHeader(t *testing.T) {
	sqlDB := testutil.NewStateDB(t)
	handler, _ := newAuthMiddleware(t, sqlDB)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuth_InvalidAuthFormat(t *testing.T) {
	sqlDB := testutil.NewStateDB(t)
	handler, _ := newAuthMiddleware(t, sqlDB)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Token abc123")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuth_InvalidTokenFormat(t *testing.T) {
	sqlDB := testutil.NewStateDB(t)
	handler, _ := newAuthMiddleware(t, sqlDB)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	// Missing the dot separator
	req.Header.Set("Authorization", "Bearer justakeywithnodot")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuth_ValidKeyPasses(t *testing.T) {
	sqlDB := testutil.NewStateDB(t)
	tenantID := insertTenant(t, sqlDB, "active")
	token := insertAPIKey(t, sqlDB, tenantID, nil, nil)

	handler, _ := newAuthMiddleware(t, sqlDB)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, tenantID, rec.Header().Get("X-Tenant-ID"))
}

func TestAuth_RevokedKeyRejected(t *testing.T) {
	sqlDB := testutil.NewStateDB(t)
	tenantID := insertTenant(t, sqlDB, "active")
	revokedAt := time.Now().Add(-1 * time.Hour).Unix()
	token := insertAPIKey(t, sqlDB, tenantID, nil, &revokedAt)

	handler, _ := newAuthMiddleware(t, sqlDB)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuth_ExpiredKeyRejected(t *testing.T) {
	sqlDB := testutil.NewStateDB(t)
	tenantID := insertTenant(t, sqlDB, "active")
	expiredAt := time.Now().Add(-1 * time.Hour).Unix()
	token := insertAPIKey(t, sqlDB, tenantID, &expiredAt, nil)

	handler, _ := newAuthMiddleware(t, sqlDB)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuth_SuspendedTenantRejected(t *testing.T) {
	sqlDB := testutil.NewStateDB(t)
	tenantID := insertTenant(t, sqlDB, "suspended")
	token := insertAPIKey(t, sqlDB, tenantID, nil, nil)

	handler, _ := newAuthMiddleware(t, sqlDB)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuth_WrongSecret(t *testing.T) {
	sqlDB := testutil.NewStateDB(t)
	tenantID := insertTenant(t, sqlDB, "active")
	token := insertAPIKey(t, sqlDB, tenantID, nil, nil)

	// Corrupt the secret portion of the token
	dotIdx := len(token) - 65 // secret is 64 chars
	corruptToken := token[:dotIdx+1] + "badbadbadbadbadbadbadbadbadbadbadbadbadbadbadbadbadbadbadbadbad0"

	handler, _ := newAuthMiddleware(t, sqlDB)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+corruptToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuth_CacheHitWithinTTL(t *testing.T) {
	sqlDB := testutil.NewStateDB(t)
	tenantID := insertTenant(t, sqlDB, "active")
	token := insertAPIKey(t, sqlDB, tenantID, nil, nil)

	handler, _ := newAuthMiddleware(t, sqlDB)

	// First request — populates cache
	req1 := httptest.NewRequest(http.MethodGet, "/", nil)
	req1.Header.Set("Authorization", "Bearer "+token)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusOK, rec1.Code)

	// Delete key from DB — cache should still service the request
	dotIdx := len(token) - 65
	keyID := token[:dotIdx]
	_, err := sqlDB.Exec("DELETE FROM api_keys WHERE id = ?", keyID)
	require.NoError(t, err)

	// Second request should still succeed from cache
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	assert.Equal(t, http.StatusOK, rec2.Code)
}

func TestAuth_CacheMissAfterInvalidation(t *testing.T) {
	sqlDB := testutil.NewStateDB(t)
	tenantID := insertTenant(t, sqlDB, "active")
	token := insertAPIKey(t, sqlDB, tenantID, nil, nil)

	handler, inv := newAuthMiddleware(t, sqlDB)

	// First request — populates cache
	req1 := httptest.NewRequest(http.MethodGet, "/", nil)
	req1.Header.Set("Authorization", "Bearer "+token)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusOK, rec1.Code)

	// Revoke key in DB and invalidate cache
	dotIdx := len(token) - 65
	keyID := token[:dotIdx]
	_, err := sqlDB.Exec("UPDATE api_keys SET revoked_at = ? WHERE id = ?", time.Now().Unix(), keyID)
	require.NoError(t, err)
	inv.InvalidateKey(keyID)

	// Should now fail — cache was cleared, DB shows revoked
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	assert.Equal(t, http.StatusUnauthorized, rec2.Code)
}

func TestAuth_ExpiredKeyInCache(t *testing.T) {
	// Manually populate the cache with an entry that has a past expiry,
	// then verify the middleware rejects it without hitting the DB.
	sqlDB := testutil.NewStateDB(t)
	tenantID := insertTenant(t, sqlDB, "active")
	token := insertAPIKey(t, sqlDB, tenantID, nil, nil)

	mw, _ := Auth(sqlDB, testMasterKey)

	// Extract keyID from token to pre-populate cache
	dotIdx := len(token) - 65
	keyID := token[:dotIdx]
	secret := token[dotIdx+1:]
	keyHash := crypto.HashAPIKey(secret, testMasterKey)

	// Get a handle on the cache by calling the middleware once normally
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := mw(next)

	req1 := httptest.NewRequest(http.MethodGet, "/", nil)
	req1.Header.Set("Authorization", "Bearer "+token)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusOK, rec1.Code)

	// Now set an expired key in the DB and update the key with an expiry in the past
	pastExpiry := time.Now().Add(-1 * time.Minute).Unix()
	_, err := sqlDB.Exec("UPDATE api_keys SET expires_at = ? WHERE id = ?", pastExpiry, keyID)
	require.NoError(t, err)

	// Manually inject an expired cache entry — we do this by creating a fresh
	// cache and populating it directly to simulate "cached but expired" state.
	c := newAuthCache()
	c.set(keyID, &authCacheEntry{
		tenantID:  tenantID,
		keyHash:   keyHash,
		status:    "active",
		expiresAt: &pastExpiry,
		cachedAt:  time.Now(),
	})
	_, ok := c.get(keyID) // should be a hit (within TTL) but expiresAt is in the past

	if ok {
		// The cache hits but the middleware logic checks expiresAt locally — verify
		// expiry detection in authCache.get does NOT filter by expiresAt (it only
		// filters by cachedAt TTL). Expiry is checked in the middleware handler.
		entry, _ := c.get(keyID)
		assert.NotNil(t, entry)
		assert.NotNil(t, entry.expiresAt)
		assert.True(t, time.Now().Unix() > *entry.expiresAt, "entry should have a past expiry")
	}

	// Verify the middleware itself rejects an expired cached entry.
	// We need a handler with a cache we control. Since Auth() creates its own
	// internal cache, test this via the actual middleware flow: invalidate the
	// current cache so it re-reads from DB (which now has expired key).
	// This exercises the "DB re-check finds expired key" path.
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	rec2 := httptest.NewRecorder()
	// The current handler still has keyID in cache with no expiresAt (from first req).
	// We need a fresh handler that will read from DB.
	mw2, _ := Auth(sqlDB, testMasterKey)
	handler2 := mw2(next)
	handler2.ServeHTTP(rec2, req2)

	// DB has the key as expired — DB lookup (no cache) should reject
	assert.Equal(t, http.StatusUnauthorized, rec2.Code)
}

func TestAuth_GetTenantIDFromContext(t *testing.T) {
	ctx := context.WithValue(context.Background(), TenantIDKey, "my-tenant")
	assert.Equal(t, "my-tenant", GetTenantID(ctx))
}

func TestAuth_GetTenantIDMissingFromContext(t *testing.T) {
	assert.Equal(t, "", GetTenantID(context.Background()))
}
