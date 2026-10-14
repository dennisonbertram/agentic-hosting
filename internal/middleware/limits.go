package middleware

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// InflightLimiter caps concurrent requests per tenant. It is meant for
// long-lived routes (exec, log follow, provisioning) so one tenant cannot
// occupy every slot of a shared pool.
type InflightLimiter struct {
	mu  sync.Mutex
	max int
	n   map[string]int
}

func NewInflightLimiter(max int) *InflightLimiter {
	return &InflightLimiter{max: max, n: map[string]int{}}
}

func (il *InflightLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenantID := GetTenantID(r.Context())
		if tenantID == "" {
			writeJSONError(w, http.StatusInternalServerError, "internal error")
			return
		}
		il.mu.Lock()
		if il.n[tenantID] >= il.max {
			il.mu.Unlock()
			w.Header().Set("Retry-After", "5")
			writeJSONError(w, http.StatusTooManyRequests,
				fmt.Sprintf("too many concurrent long-running requests (max %d per tenant)", il.max))
			return
		}
		il.n[tenantID]++
		il.mu.Unlock()
		defer func() {
			il.mu.Lock()
			if il.n[tenantID]--; il.n[tenantID] <= 0 {
				delete(il.n, tenantID)
			}
			il.mu.Unlock()
		}()
		next.ServeHTTP(w, r)
	})
}

// WriteDeadline bounds the whole response of a normal route: a client that
// stops reading cannot hold the connection past d. (chimw.Timeout only cancels
// a context; it cannot interrupt a blocked write.)
func WriteDeadline(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Error ignored: unsupported writers (e.g. test recorders) simply get no deadline.
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(d))
			next.ServeHTTP(w, r)
		})
	}
}

// IdleWriteDeadline is for long/streaming routes. No deadline applies while the
// handler is working (exec may run minutes before writing); once it writes,
// each Write/Flush gets a fresh deadline of d, so a reader that makes no
// progress for d is cut off while a steadily progressing stream is not.
func IdleWriteDeadline(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rc := http.NewResponseController(w)
			// Drop any deadline a prior request left on this keep-alive connection
			// (older net/http versions do not reset it when WriteTimeout is 0).
			_ = rc.SetWriteDeadline(time.Time{})
			next.ServeHTTP(&idleWriter{ResponseWriter: w, rc: rc, d: d}, r)
		})
	}
}

type idleWriter struct {
	http.ResponseWriter
	rc *http.ResponseController
	d  time.Duration
}

func (w *idleWriter) extend() { _ = w.rc.SetWriteDeadline(time.Now().Add(w.d)) }

func (w *idleWriter) WriteHeader(code int) { w.extend(); w.ResponseWriter.WriteHeader(code) }

func (w *idleWriter) Write(p []byte) (int, error) { w.extend(); return w.ResponseWriter.Write(p) }

func (w *idleWriter) Flush() { w.extend(); _ = w.rc.Flush() }

func (w *idleWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
