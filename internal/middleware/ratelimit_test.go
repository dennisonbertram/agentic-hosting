package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// contextWithTenant returns a context with the given tenant ID set.
func contextWithTenant(tenantID string) context.Context {
	return context.WithValue(context.Background(), TenantIDKey, tenantID)
}

// newRateLimitHandler wraps a trivial OK handler with the RateLimiter middleware.
func newRateLimitHandler(rl *RateLimiter) http.Handler {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return rl.Middleware(next)
}

func TestRateLimit_AllowsRequestsWithinBurst(t *testing.T) {
	// rps=10, burst=5 — first 5 requests should all pass
	rl := NewRateLimiter(10, 5)
	handler := newRateLimitHandler(rl)

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req = req.WithContext(contextWithTenant("tenant-a"))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code, "request %d should pass", i+1)
	}
}

func TestRateLimit_EnforcedPerTenant(t *testing.T) {
	// rps=10, burst=1 — each tenant gets their own bucket
	rl := NewRateLimiter(10, 1)
	handler := newRateLimitHandler(rl)

	// Drain tenant-a's single token
	req1 := httptest.NewRequest(http.MethodGet, "/", nil)
	req1 = req1.WithContext(contextWithTenant("tenant-a"))
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	assert.Equal(t, http.StatusOK, rec1.Code)

	// Second request for tenant-a should be rate-limited
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2 = req2.WithContext(contextWithTenant("tenant-a"))
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	assert.Equal(t, http.StatusTooManyRequests, rec2.Code)

	// First request for tenant-b should still pass (separate bucket)
	req3 := httptest.NewRequest(http.MethodGet, "/", nil)
	req3 = req3.WithContext(contextWithTenant("tenant-b"))
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)
	assert.Equal(t, http.StatusOK, rec3.Code)
}

func TestRateLimit_RetryAfterHeaderOnExhaustion(t *testing.T) {
	// burst=1, low rps — exhaust the bucket
	rl := NewRateLimiter(0.1, 1) // 1 req per 10 seconds, burst=1
	handler := newRateLimitHandler(rl)

	// First request consumes the burst
	req1 := httptest.NewRequest(http.MethodGet, "/", nil)
	req1 = req1.WithContext(contextWithTenant("tenant-x"))
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusOK, rec1.Code)

	// Second request should be rate-limited with Retry-After header
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2 = req2.WithContext(contextWithTenant("tenant-x"))
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	assert.Equal(t, http.StatusTooManyRequests, rec2.Code)

	retryAfter := rec2.Header().Get("Retry-After")
	require.NotEmpty(t, retryAfter, "Retry-After header must be set on 429")

	secs, err := strconv.Atoi(retryAfter)
	require.NoError(t, err, "Retry-After must be a valid integer")
	assert.GreaterOrEqual(t, secs, 1, "Retry-After must be at least 1 second")
	assert.LessOrEqual(t, secs, 60, "Retry-After must be capped at 60 seconds")
}

func TestRateLimit_MissingTenantIDFailsClosed(t *testing.T) {
	rl := NewRateLimiter(100, 100)
	handler := newRateLimitHandler(rl)

	// No tenant ID in context
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestGlobalRateLimit_AllowsWithinBurst(t *testing.T) {
	gl := NewGlobalRateLimiter(10, 5)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := gl.Middleware(next)

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code, "request %d should pass", i+1)
	}
}

func TestGlobalRateLimit_ExhaustReturns429WithRetryAfter(t *testing.T) {
	gl := NewGlobalRateLimiter(0.1, 1) // burst=1, very slow refill
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := gl.Middleware(next)

	// Consume the burst
	req1 := httptest.NewRequest(http.MethodGet, "/", nil)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusOK, rec1.Code)

	// Should now be limited
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	assert.Equal(t, http.StatusTooManyRequests, rec2.Code)

	retryAfter := rec2.Header().Get("Retry-After")
	require.NotEmpty(t, retryAfter)
	secs, err := strconv.Atoi(retryAfter)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, secs, 1)
}
