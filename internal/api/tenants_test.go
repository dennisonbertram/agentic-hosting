package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/cache"
	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegistrationLimiter_AllowsWithinPerIPLimit verifies that an IP can
// register up to regMaxPerIPPerHour (5) times before being rate-limited.
func TestRegistrationLimiter_AllowsWithinPerIPLimit(t *testing.T) {
	rl := &registrationLimiter{
		entries: cache.New[string, *regEntry](regMaxEntries),
	}

	for i := 0; i < regMaxPerIPPerHour; i++ {
		ok, _ := rl.allow("192.168.1.1")
		assert.True(t, ok, "registration %d should be allowed", i+1)
	}

	// The 6th should be rejected
	ok, wait := rl.allow("192.168.1.1")
	assert.False(t, ok, "registration beyond per-IP limit should be rejected")
	assert.Greater(t, wait.Seconds(), float64(0),
		"Retry-After wait duration should be positive when rate-limited")
}

// TestRegistrationLimiter_PerIPIsolation verifies that different IPs have
// independent rate limit buckets. Exhausting one IP's quota does not affect
// another IP.
func TestRegistrationLimiter_PerIPIsolation(t *testing.T) {
	rl := &registrationLimiter{
		entries: cache.New[string, *regEntry](regMaxEntries),
	}

	// Exhaust IP-A's per-IP quota
	for i := 0; i < regMaxPerIPPerHour; i++ {
		ok, _ := rl.allow("10.0.0.1")
		require.True(t, ok)
	}

	// IP-A should now be blocked
	ok, _ := rl.allow("10.0.0.1")
	assert.False(t, ok, "IP-A should be rate-limited after 5 registrations")

	// IP-B should still be allowed
	ok, _ = rl.allow("10.0.0.2")
	assert.True(t, ok, "IP-B should be unaffected by IP-A's exhaustion")
}

// TestRegistrationLimiter_GlobalLimitEnforced verifies the global per-hour
// cap (20 registrations across all IPs). Even if individual IPs haven't hit
// their per-IP limits, the global cap stops all registrations.
func TestRegistrationLimiter_GlobalLimitEnforced(t *testing.T) {
	rl := &registrationLimiter{
		entries: cache.New[string, *regEntry](regMaxEntries),
	}

	// Use distinct IPs to avoid per-IP limits. Each IP registers up to its
	// per-IP limit (5), and we need 20 total to hit the global limit.
	for i := 0; i < regGlobalPerHour; i++ {
		ip := "10.0." + strconv.Itoa(i/regMaxPerIPPerHour) + "." + strconv.Itoa(i%regMaxPerIPPerHour+1)
		ok, _ := rl.allow(ip)
		require.True(t, ok, "registration %d (ip=%s) should pass under global limit", i+1, ip)
	}

	// 21st registration from a fresh IP should be rejected by global limit
	ok, wait := rl.allow("172.16.0.1")
	assert.False(t, ok, "registration beyond global limit should be rejected")
	assert.Greater(t, wait.Seconds(), float64(0),
		"wait duration should be positive when global limit is hit")
}

// TestRegistrationLimiter_RetryAfterDuration verifies that the wait duration
// returned by allow() is positive and bounded by the 1-hour window.
func TestRegistrationLimiter_RetryAfterDuration(t *testing.T) {
	rl := &registrationLimiter{
		entries: cache.New[string, *regEntry](regMaxEntries),
	}

	// Exhaust the per-IP limit
	for i := 0; i < regMaxPerIPPerHour; i++ {
		ok, _ := rl.allow("10.10.10.10")
		require.True(t, ok)
	}

	ok, wait := rl.allow("10.10.10.10")
	require.False(t, ok)

	// Wait should be between 0 and 1 hour (regWindow)
	assert.Greater(t, wait.Seconds(), float64(0))
	assert.LessOrEqual(t, wait.Seconds(), regWindow.Seconds(),
		"wait duration should not exceed the rate limit window")
}

// TestRegistrationEndpoint_RateLimitReturns429 exercises the full HTTP handler
// path for registration rate limiting. After 5 requests from the same IP,
// subsequent requests should receive 429 with a Retry-After header.
func TestRegistrationEndpoint_RateLimitReturns429(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	// Reset the global registration limiter to avoid pollution from other tests.
	// Save and restore afterwards.
	savedLimiter := regLimiter
	regLimiter = &registrationLimiter{
		entries: cache.New[string, *regEntry](regMaxEntries),
	}
	t.Cleanup(func() { regLimiter = savedLimiter })

	srv := NewServer(ServerConfig{
		Store:            &db.Store{StateDB: stateDB},
		MasterKey:        masterKey,
		DevMode:          true,
		OpenRegistration: true,
	})

	body := func() *bytes.Reader {
		return bytes.NewReader([]byte(`{"name":"Test User","email":"user@example.com"}`))
	}

	// First regMaxPerIPPerHour (5) requests should succeed (or fail for other
	// reasons like duplicate email, but NOT with 429). We use unique emails.
	for i := 0; i < regMaxPerIPPerHour; i++ {
		payload, _ := json.Marshal(map[string]string{
			"name":  "Test User",
			"email": "user" + strconv.Itoa(i) + "@example.com",
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/tenants/register", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.168.1.100:12345"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		// Should be 201 (success), not 429
		assert.NotEqual(t, http.StatusTooManyRequests, rec.Code,
			"registration %d should not be rate-limited", i+1)
	}

	// 6th request should be rate-limited
	req := httptest.NewRequest(http.MethodPost, "/v1/tenants/register", body())
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.168.1.100:12345"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusTooManyRequests, rec.Code,
		"6th registration from same IP should return 429")
	assert.NotEmpty(t, rec.Header().Get("Retry-After"),
		"429 response must include Retry-After header")

	// Verify the body is valid JSON with an error field
	var errResp map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Contains(t, errResp["error"], "rate limit",
		"error message should mention rate limit")
}

// TestRegistrationEndpoint_DifferentIPsNotAffected verifies that rate limiting
// one IP does not prevent a different IP from registering.
func TestRegistrationEndpoint_DifferentIPsNotAffected(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	savedLimiter := regLimiter
	regLimiter = &registrationLimiter{
		entries: cache.New[string, *regEntry](regMaxEntries),
	}
	t.Cleanup(func() { regLimiter = savedLimiter })

	srv := NewServer(ServerConfig{
		Store:            &db.Store{StateDB: stateDB},
		MasterKey:        masterKey,
		DevMode:          true,
		OpenRegistration: true,
	})

	// Exhaust IP-A's registration quota
	for i := 0; i < regMaxPerIPPerHour; i++ {
		payload, _ := json.Marshal(map[string]string{
			"name":  "User A",
			"email": "usera" + strconv.Itoa(i) + "@example.com",
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/tenants/register", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "10.0.0.1:12345"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		require.NotEqual(t, http.StatusTooManyRequests, rec.Code)
	}

	// Confirm IP-A is blocked
	payload, _ := json.Marshal(map[string]string{
		"name":  "User A Extra",
		"email": "usera-extra@example.com",
	})
	reqA := httptest.NewRequest(http.MethodPost, "/v1/tenants/register", bytes.NewReader(payload))
	reqA.Header.Set("Content-Type", "application/json")
	reqA.RemoteAddr = "10.0.0.1:12345"
	recA := httptest.NewRecorder()
	srv.ServeHTTP(recA, reqA)
	require.Equal(t, http.StatusTooManyRequests, recA.Code)

	// IP-B should still be allowed to register
	payload, _ = json.Marshal(map[string]string{
		"name":  "User B",
		"email": "userb@example.com",
	})
	reqB := httptest.NewRequest(http.MethodPost, "/v1/tenants/register", bytes.NewReader(payload))
	reqB.Header.Set("Content-Type", "application/json")
	reqB.RemoteAddr = "10.0.0.2:12345"
	recB := httptest.NewRecorder()
	srv.ServeHTTP(recB, reqB)

	assert.NotEqual(t, http.StatusTooManyRequests, recB.Code,
		"IP-B should not be rate-limited when only IP-A exhausted its quota")
}
