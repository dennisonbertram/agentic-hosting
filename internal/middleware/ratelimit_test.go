package middleware

import (
	"context"
	"encoding/json"
	"fmt"
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

// TestRateLimit_ProductionConfig_200BurstThenThrottled verifies the production
// configuration (100 req/s, burst=200) allows all 200 burst requests and then
// returns 429 for the next request. This exercises the exact values used in
// server.go NewRateLimiter(100, 200).
func TestRateLimit_ProductionConfig_200BurstThenThrottled(t *testing.T) {
	rl := NewRateLimiter(100, 200)
	handler := newRateLimitHandler(rl)

	// All 200 burst requests should pass
	for i := 0; i < 200; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req = req.WithContext(contextWithTenant("tenant-burst"))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code, "burst request %d should pass", i+1)
	}

	// Request 201 should be throttled (burst exhausted, not enough time for refill)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(contextWithTenant("tenant-burst"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusTooManyRequests, rec.Code,
		"request beyond burst capacity should return 429")
}

// TestRateLimit_XRateLimitHeaders verifies that X-RateLimit-Limit and
// X-RateLimit-Remaining headers are set on every response, both for allowed
// and throttled requests.
func TestRateLimit_XRateLimitHeaders(t *testing.T) {
	rl := NewRateLimiter(100, 2) // burst=2 for fast exhaustion
	handler := newRateLimitHandler(rl)

	// First request — should pass with rate limit headers
	req1 := httptest.NewRequest(http.MethodGet, "/", nil)
	req1 = req1.WithContext(contextWithTenant("tenant-headers"))
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)

	require.Equal(t, http.StatusOK, rec1.Code)
	assert.Equal(t, "100", rec1.Header().Get("X-RateLimit-Limit"),
		"X-RateLimit-Limit should reflect the configured rate")
	remaining := rec1.Header().Get("X-RateLimit-Remaining")
	require.NotEmpty(t, remaining, "X-RateLimit-Remaining must be set on 200")
	remVal, err := strconv.Atoi(remaining)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, remVal, 0, "remaining should be non-negative")

	// Exhaust the burst
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2 = req2.WithContext(contextWithTenant("tenant-headers"))
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	req3 := httptest.NewRequest(http.MethodGet, "/", nil)
	req3 = req3.WithContext(contextWithTenant("tenant-headers"))
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)

	// Even on 429 the rate limit headers should still be set
	assert.Equal(t, http.StatusTooManyRequests, rec3.Code)
	assert.Equal(t, "100", rec3.Header().Get("X-RateLimit-Limit"),
		"X-RateLimit-Limit should be present on 429 responses too")
	assert.NotEmpty(t, rec3.Header().Get("X-RateLimit-Remaining"),
		"X-RateLimit-Remaining should be present on 429 responses too")
}

// TestRateLimit_429ResponseIsJSON verifies the 429 response body is valid JSON
// with an "error" field matching the expected rate limit message.
func TestRateLimit_429ResponseIsJSON(t *testing.T) {
	rl := NewRateLimiter(10, 1)
	handler := newRateLimitHandler(rl)

	// Consume the burst
	req1 := httptest.NewRequest(http.MethodGet, "/", nil)
	req1 = req1.WithContext(contextWithTenant("tenant-json"))
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusOK, rec1.Code)

	// Trigger 429
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2 = req2.WithContext(contextWithTenant("tenant-json"))
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	require.Equal(t, http.StatusTooManyRequests, rec2.Code)
	assert.Equal(t, "application/json", rec2.Header().Get("Content-Type"))

	var errResp map[string]string
	require.NoError(t, json.NewDecoder(rec2.Body).Decode(&errResp),
		"429 body must be valid JSON")
	assert.Equal(t, "rate limit exceeded", errResp["error"])
}

// TestRateLimit_PerTenantIsolationUnderLoad verifies that exhausting one
// tenant's rate limit bucket does not affect another tenant, even when both
// are being processed by the same RateLimiter instance. This is the key
// multi-tenant isolation guarantee.
func TestRateLimit_PerTenantIsolationUnderLoad(t *testing.T) {
	rl := NewRateLimiter(100, 10) // burst=10
	handler := newRateLimitHandler(rl)

	// Exhaust tenant-A's burst completely
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req = req.WithContext(contextWithTenant("tenant-A"))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
	}

	// Confirm tenant-A is now rate-limited
	reqA := httptest.NewRequest(http.MethodGet, "/", nil)
	reqA = reqA.WithContext(contextWithTenant("tenant-A"))
	recA := httptest.NewRecorder()
	handler.ServeHTTP(recA, reqA)
	require.Equal(t, http.StatusTooManyRequests, recA.Code,
		"tenant-A should be rate-limited after exhausting burst")

	// tenant-B should still be fully unaffected — all 10 burst requests pass
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req = req.WithContext(contextWithTenant("tenant-B"))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code,
			"tenant-B request %d should pass (independent bucket)", i+1)
	}

	// tenant-C should also be fully independent
	reqC := httptest.NewRequest(http.MethodGet, "/", nil)
	reqC = reqC.WithContext(contextWithTenant("tenant-C"))
	recC := httptest.NewRecorder()
	handler.ServeHTTP(recC, reqC)
	assert.Equal(t, http.StatusOK, recC.Code,
		"tenant-C should be unaffected by tenant-A being rate-limited")
}

// TestRateLimit_MultipleTenantsConcurrentBuckets verifies that the LRU-based
// rate limiter correctly maintains separate buckets for many distinct tenants
// (not just 2-3).
func TestRateLimit_MultipleTenantsConcurrentBuckets(t *testing.T) {
	rl := NewRateLimiter(100, 1) // burst=1 per tenant
	handler := newRateLimitHandler(rl)

	// 50 distinct tenants each send 1 request — all should pass
	for i := 0; i < 50; i++ {
		tid := fmt.Sprintf("tenant-%03d", i)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req = req.WithContext(contextWithTenant(tid))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code,
			"first request for %s should pass", tid)
	}

	// Second request from each of the first 5 tenants should be rate-limited
	for i := 0; i < 5; i++ {
		tid := fmt.Sprintf("tenant-%03d", i)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req = req.WithContext(contextWithTenant(tid))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusTooManyRequests, rec.Code,
			"second request for %s should be rate-limited", tid)
	}
}

// TestRateLimit_GlobalRateLimiter429IsJSON verifies the global rate limiter
// also returns a well-formed JSON error body on 429.
func TestRateLimit_GlobalRateLimiter429IsJSON(t *testing.T) {
	gl := NewGlobalRateLimiter(0.1, 1)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := gl.Middleware(next)

	// Consume the burst
	req1 := httptest.NewRequest(http.MethodGet, "/", nil)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusOK, rec1.Code)

	// Trigger 429
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	require.Equal(t, http.StatusTooManyRequests, rec2.Code)
	assert.Equal(t, "application/json", rec2.Header().Get("Content-Type"))

	var errResp map[string]string
	require.NoError(t, json.NewDecoder(rec2.Body).Decode(&errResp))
	assert.Equal(t, "global rate limit exceeded", errResp["error"])
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
