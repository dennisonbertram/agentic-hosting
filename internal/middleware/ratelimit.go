package middleware

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/cache"
	"golang.org/x/time/rate"
)

const maxRateLimitEntries = 10000

type rateLimitEntry struct {
	limiter  *rate.Limiter
	rps      float64      // guarded by RateLimiter.mu
	lastSeen atomic.Int64 // unix nanos
}

type RateLimiter struct {
	mu    sync.Mutex // makes get-or-create atomic
	lru   *cache.LRU[string, *rateLimitEntry]
	rate  rate.Limit
	burst int
	now   func() time.Time // injectable for deterministic tests
}

func NewRateLimiter(rps float64, burst int) *RateLimiter {
	rl := &RateLimiter{
		lru:   cache.New[string, *rateLimitEntry](maxRateLimitEntries),
		rate:  rate.Limit(rps),
		burst: burst,
		now:   time.Now,
	}
	go rl.cleanup()
	return rl
}

func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	for range ticker.C {
		rl.sweep(time.Now().Add(-1 * time.Hour))
	}
}

func (rl *RateLimiter) sweep(cutoff time.Time) {
	rl.lru.DeleteFunc(func(_ string, v *rateLimitEntry) bool {
		return v.lastSeen.Load() < cutoff.UnixNano()
	})
}

type rateLimitKey struct{}

// ContextWithRateLimit carries a tenant's api_rate_limit (requests/second)
// from auth to the rate limiter. Values <= 0 mean "use the default".
func ContextWithRateLimit(ctx context.Context, rps int) context.Context {
	return context.WithValue(ctx, rateLimitKey{}, rps)
}

// getLimiter returns the tenant's limiter, creating it if needed. The limiter
// runs at rps (burst 2x rps); rps <= 0 selects the default rate and burst.
func (rl *RateLimiter) getLimiter(key string, rps float64) *rate.Limiter {
	limit, burst := rl.rate, rl.burst
	if rps > 0 {
		limit, burst = rate.Limit(rps), int(math.Ceil(2*rps))
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()
	entry, ok := rl.lru.Get(key)
	if !ok {
		entry = &rateLimitEntry{limiter: rate.NewLimiter(limit, burst), rps: rps}
		rl.lru.Set(key, entry)
	} else if entry.rps != rps {
		entry.limiter.SetLimit(limit)
		entry.limiter.SetBurst(burst)
		entry.rps = rps
	}
	entry.lastSeen.Store(rl.now().UnixNano())
	return entry.limiter
}

func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenantID := GetTenantID(r.Context())
		if tenantID == "" {
			// Fail closed: reject if tenant ID is missing (should never happen
			// in authenticated routes, but prevents bypass if context is lost)
			writeJSONError(w, http.StatusInternalServerError, "internal error")
			return
		}

		tenantRPS, _ := r.Context().Value(rateLimitKey{}).(int)
		limiter := rl.getLimiter(tenantID, float64(tenantRPS))
		now := rl.now()

		// Set rate limit headers on all responses
		w.Header().Set("X-RateLimit-Limit", fmt.Sprintf("%.0f", float64(limiter.Limit())))
		w.Header().Set("X-RateLimit-Remaining", fmt.Sprintf("%.0f", math.Max(0, limiter.TokensAt(now))))

		if !limiter.AllowN(now, 1) {
			w.Header().Set("Retry-After", retryAfterFromLimiter(limiter, now))
			writeJSONError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// GlobalRateLimiter enforces a single rate limit across all tenants to prevent
// multi-tenant aggregate abuse (e.g., attacker creating many tenants to multiply throughput).
type GlobalRateLimiter struct {
	limiter *rate.Limiter
}

func NewGlobalRateLimiter(rps float64, burst int) *GlobalRateLimiter {
	return &GlobalRateLimiter{
		limiter: rate.NewLimiter(rate.Limit(rps), burst),
	}
}

func (gl *GlobalRateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !gl.limiter.Allow() {
			w.Header().Set("Retry-After", retryAfterFromLimiter(gl.limiter, time.Now()))
			writeJSONError(w, http.StatusTooManyRequests, "global rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// retryAfterFromLimiter computes a Retry-After value (in seconds) based on
// the token bucket's refill rate. Uses Reserve/Cancel to peek at the delay
// without consuming a token.
func retryAfterFromLimiter(l *rate.Limiter, now time.Time) string {
	r := l.ReserveN(now, 1)
	delay := r.Delay()
	r.CancelAt(now)
	secs := int(math.Ceil(delay.Seconds()))
	if secs < 1 {
		secs = 1
	}
	if secs > 60 {
		secs = 60 // cap at 60s to avoid unreasonable backoff
	}
	return fmt.Sprintf("%d", secs)
}
