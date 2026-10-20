package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A pending entry whose handler has run longer than the TTL must still block a
// retry with the same key; otherwise the retry runs the handler a second time
// concurrently (long routes such as tenant delete can exceed the TTL).
func TestIdempotency_PendingEntryPastTTLStillBlocksRetry(t *testing.T) {
	store := NewIdempotencyStore()
	body := `{"name":"slow"}`
	fullKey := testIdempotencyTenantID + ":POST:/v1/databases:key-slow"

	done := make(chan struct{})
	store.mu.Lock()
	store.entries[fullKey] = &idempotencyEntry{
		bodyHash:  sha256Hex([]byte(body)),
		createdAt: time.Now().Add(-20 * time.Minute),
		expiresAt: time.Now().Add(-10 * time.Minute), // reserved long ago
		done:      done,
		pending:   true,
	}
	store.mu.Unlock()

	var calls atomic.Int32
	handler := store.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req := idemRequestWithTenant(http.MethodPost, "/v1/databases", "key-slow", testIdempotencyTenantID, body).WithContext(
		context.WithValue(ctx, TenantIDKey, testIdempotencyTenantID))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, int32(0), calls.Load(), "retry must not run while the first request is in flight")
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "still in progress")
	close(done)
}

// The replay window starts when the response is stored, not when the key was
// reserved, so a slow handler's response is still replayable afterwards.
func TestIdempotency_ExpiryCountsFromCompletion(t *testing.T) {
	store := NewIdempotencyStore()
	handler := store.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate a handler that took most of the TTL by back-dating the
		// reservation while the handler runs.
		store.mu.Lock()
		for _, e := range store.entries {
			e.expiresAt = time.Now().Add(-time.Second)
		}
		store.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))

	req := idemRequestWithTenant(http.MethodPost, "/v1/services", "key-ttl", testIdempotencyTenantID, `{}`)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	store.mu.Lock()
	var entry *idempotencyEntry
	for _, e := range store.entries {
		entry = e
	}
	store.mu.Unlock()
	require.NotNil(t, entry)
	assert.False(t, entry.pending)
	assert.WithinDuration(t, time.Now().Add(idempotencyTTL), entry.expiresAt, 5*time.Second)

	req2 := idemRequestWithTenant(http.MethodPost, "/v1/services", "key-ttl", testIdempotencyTenantID, `{}`)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	assert.Equal(t, "true", rec2.Header().Get("Idempotency-Replayed"))
	assert.Equal(t, http.StatusCreated, rec2.Code)
}

func TestIdempotency_RecorderFlushAndUnwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	rr := &responseRecorder{ResponseWriter: rec}

	// WriteHeader is recorded once; later calls are ignored.
	rr.WriteHeader(http.StatusAccepted)
	rr.WriteHeader(http.StatusTeapot)
	assert.Equal(t, http.StatusAccepted, rr.statusCode)
	assert.Equal(t, http.StatusAccepted, rec.Code)

	rr.Flush()
	assert.True(t, rec.Flushed)
	assert.Same(t, http.ResponseWriter(rec), rr.Unwrap())
}

func TestIdempotency_OversizedBodyRejected(t *testing.T) {
	store := NewIdempotencyStore()
	called := false
	handler := store.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))

	big := make([]byte, maxHashBodySize+1)
	req := idemRequestWithTenant(http.MethodPost, "/v1/services", "key-big", testIdempotencyTenantID, string(big))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.False(t, called)
}

// InvalidateTenant must evict every cached key of that tenant (suspension) and
// leave other tenants' keys cached.
func TestAuth_InvalidateTenantEvictsOnlyThatTenant(t *testing.T) {
	sqlDB := testutil.NewStateDB(t)
	tenantA := insertTenant(t, sqlDB, "active")
	tokenA := insertAPIKey(t, sqlDB, tenantA, nil, nil)
	tenantB := insertTenant(t, sqlDB, "active")
	tokenB := insertAPIKey(t, sqlDB, tenantB, nil, nil)

	handler, inv := newAuthMiddleware(t, sqlDB)
	do := func(token string) int {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}
	require.Equal(t, http.StatusOK, do(tokenA))
	require.Equal(t, http.StatusOK, do(tokenB))

	// Suspend both tenants in the DB, but only invalidate A's cache entries.
	_, err := sqlDB.Exec(`UPDATE tenants SET status = 'suspended'`)
	require.NoError(t, err)
	inv.InvalidateTenant(tenantA)

	assert.Equal(t, http.StatusUnauthorized, do(tokenA), "A's cached key must be evicted")
	assert.Equal(t, http.StatusOK, do(tokenB), "B's cache entry is untouched until its own invalidation/TTL")
}

func TestAuth_SetsKeyIDInContext(t *testing.T) {
	sqlDB := testutil.NewStateDB(t)
	tenantID := insertTenant(t, sqlDB, "active")
	token := insertAPIKey(t, sqlDB, tenantID, nil, nil)

	mw, _ := Auth(sqlDB, testMasterKey)
	var gotKeyID string
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKeyID = GetKeyID(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, token[:32], gotKeyID)
	assert.Empty(t, GetKeyID(context.Background()))
}

func TestAuth_OversizedTokenPartsRejected(t *testing.T) {
	sqlDB := testutil.NewStateDB(t)
	handler, _ := newAuthMiddleware(t, sqlDB)
	for _, token := range []string{
		string(make([]byte, 65)) + ".secret",
		"keyid." + string(make([]byte, 257)),
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer k"+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	}
}

// idleWriter: a handler's explicit SetWriteDeadline becomes the floor for later
// per-write extensions, and Unwrap exposes the underlying writer.
func TestIdleWriter_FloorAndUnwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	iw := &idleWriter{ResponseWriter: rec, rc: http.NewResponseController(rec), d: time.Second}
	floor := time.Now().Add(time.Hour)
	_ = iw.SetWriteDeadline(floor) // recorder does not support deadlines; floor is still recorded
	assert.Equal(t, floor, iw.floor)

	iw.WriteHeader(http.StatusAccepted)
	_, err := iw.Write([]byte("x"))
	require.NoError(t, err)
	iw.Flush()
	assert.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, "x", rec.Body.String())
	assert.Same(t, http.ResponseWriter(rec), iw.Unwrap())
}

func TestInflightLimiter_MissingTenantFailsClosed(t *testing.T) {
	il := NewInflightLimiter(1)
	called := false
	h := il.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.False(t, called)
}
