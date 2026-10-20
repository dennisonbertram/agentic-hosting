package middleware

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
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
	// headerPrefix names the limit headers this limiter sets
	// (<prefix>-Limit, <prefix>-Remaining). Default "X-RateLimit".
	headerPrefix string
}

// DefaultRateLimitHeaderPrefix is the header prefix of the general API limiter.
const DefaultRateLimitHeaderPrefix = "X-RateLimit"

func NewRateLimiter(rps float64, burst int) *RateLimiter {
	rl := &RateLimiter{
		lru:   cache.New[string, *rateLimitEntry](maxRateLimitEntries),
		rate:  rate.Limit(rps),
		burst: burst,
		now:   time.Now,

		headerPrefix: DefaultRateLimitHeaderPrefix,
	}
	go rl.cleanup()
	return rl
}

// NewRateLimiterWithHeaderPrefix is NewRateLimiter for a secondary limiter
// stacked on routes that already pass the general API limiter: it reports its
// state under <prefix>-Limit and <prefix>-Remaining, so the general
// X-RateLimit-* headers keep describing the general limit.
func NewRateLimiterWithHeaderPrefix(rps float64, burst int, prefix string) *RateLimiter {
	rl := NewRateLimiter(rps, burst)
	if prefix != "" {
		rl.headerPrefix = prefix
	}
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
		prefix := rl.headerPrefix
		if prefix == "" {
			prefix = DefaultRateLimitHeaderPrefix
		}
		w.Header().Set(prefix+"-Limit", formatLimit(limiter.Limit()))
		w.Header().Set(prefix+"-Remaining", fmt.Sprintf("%.0f", math.Max(0, limiter.TokensAt(now))))

		if !limiter.AllowN(now, 1) {
			w.Header().Set("Retry-After", retryAfterFromLimiter(limiter, now))
			writeJSONError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// formatLimit renders a per-second limit. Whole rates keep the historical
// integer format; sub-1/s rates (e.g. 20/min) keep up to 3 decimals instead
// of rounding to "0".
func formatLimit(l rate.Limit) string {
	v := float64(l)
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return strconv.FormatFloat(math.Round(v*1000)/1000, 'f', -1, 64)
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
