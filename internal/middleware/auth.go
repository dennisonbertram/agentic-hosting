package middleware

import (
	"context"
	"database/sql"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/paasd/paasd/internal/crypto"
)

type contextKey string

const TenantIDKey contextKey = "tenant_id"

const (
	lastUsedInterval  = 5 * time.Minute
	lastUsedMaxKeys   = 10000
	authFailMaxPerIP  = 60  // per minute
	authFailWindowSec = 60
)

// lastUsedTracker samples last_used_at updates with bounded map.
type lastUsedTracker struct {
	mu       sync.Mutex
	lastSeen map[string]time.Time
	db       *sql.DB
}

func newLastUsedTracker(db *sql.DB) *lastUsedTracker {
	t := &lastUsedTracker{
		lastSeen: make(map[string]time.Time),
		db:       db,
	}
	// Periodic cleanup of stale entries
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		for range ticker.C {
			t.mu.Lock()
			cutoff := time.Now().Add(-24 * time.Hour)
			for k, v := range t.lastSeen {
				if v.Before(cutoff) {
					delete(t.lastSeen, k)
				}
			}
			t.mu.Unlock()
		}
	}()
	return t
}

func (t *lastUsedTracker) maybeUpdate(keyID string) {
	t.mu.Lock()
	last, exists := t.lastSeen[keyID]
	now := time.Now()
	if exists && now.Sub(last) < lastUsedInterval {
		t.mu.Unlock()
		return
	}
	// Evict oldest if at capacity
	if !exists && len(t.lastSeen) >= lastUsedMaxKeys {
		var oldestKey string
		var oldestTime time.Time
		for k, v := range t.lastSeen {
			if oldestKey == "" || v.Before(oldestTime) {
				oldestKey = k
				oldestTime = v
			}
		}
		if oldestKey != "" {
			delete(t.lastSeen, oldestKey)
		}
	}
	t.lastSeen[keyID] = now
	t.mu.Unlock()

	t.db.Exec("UPDATE api_keys SET last_used_at = ? WHERE id = ?", now.Unix(), keyID)
}

// authFailLimiter tracks auth failures per IP to prevent brute-force.
type authFailLimiter struct {
	mu      sync.Mutex
	entries map[string]*authFailEntry
}

type authFailEntry struct {
	count    int
	windowAt time.Time
}

func newAuthFailLimiter() *authFailLimiter {
	l := &authFailLimiter{entries: make(map[string]*authFailEntry)}
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		for range ticker.C {
			l.mu.Lock()
			now := time.Now()
			for k, v := range l.entries {
				if now.After(v.windowAt) {
					delete(l.entries, k)
				}
			}
			l.mu.Unlock()
		}
	}()
	return l
}

func (l *authFailLimiter) check(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	entry, exists := l.entries[ip]
	if !exists || now.After(entry.windowAt) {
		return true // allowed
	}
	return entry.count < authFailMaxPerIP
}

func (l *authFailLimiter) record(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	entry, exists := l.entries[ip]
	if !exists || now.After(entry.windowAt) {
		l.entries[ip] = &authFailEntry{count: 1, windowAt: now.Add(time.Duration(authFailWindowSec) * time.Second)}
		return
	}
	entry.count++
}

func Auth(db *sql.DB, masterKey []byte) func(http.Handler) http.Handler {
	tracker := newLastUsedTracker(db)
	failLimiter := newAuthFailLimiter()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Pre-auth IP rate limit on failures
			ip, _, _ := net.SplitHostPort(r.RemoteAddr)
			if ip == "" {
				ip = r.RemoteAddr
			}
			if !failLimiter.check(ip) {
				http.Error(w, `{"error":"too many auth failures"}`, http.StatusTooManyRequests)
				return
			}

			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				failLimiter.record(ip)
				http.Error(w, `{"error":"missing authorization header"}`, http.StatusUnauthorized)
				return
			}

			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) != 2 || parts[0] != "Bearer" {
				failLimiter.record(ip)
				http.Error(w, `{"error":"invalid authorization format"}`, http.StatusUnauthorized)
				return
			}
			token := parts[1]

			if len(token) < 8 || len(token) > 256 {
				failLimiter.record(ip)
				http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
				return
			}
			prefix := token[:8]

			now := time.Now().Unix()
			rows, err := db.QueryContext(r.Context(),
				`SELECT ak.id, ak.tenant_id, ak.key_hash, t.status
				 FROM api_keys ak
				 JOIN tenants t ON t.id = ak.tenant_id
				 WHERE ak.key_prefix = ?
				   AND ak.revoked_at IS NULL
				   AND (ak.expires_at IS NULL OR ak.expires_at > ?)`,
				prefix, now,
			)
			if err != nil {
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			defer rows.Close()

			var matched bool
			var tenantID string
			var matchedKeyID string
			for rows.Next() {
				var keyID, tid, keyHash, status string
				if err := rows.Scan(&keyID, &tid, &keyHash, &status); err != nil {
					continue
				}
				if status != "active" {
					continue
				}
				if crypto.VerifyAPIKey(keyHash, token, masterKey) {
					matched = true
					tenantID = tid
					matchedKeyID = keyID
					break
				}
			}

			if !matched {
				failLimiter.record(ip)
				http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
				return
			}

			tracker.maybeUpdate(matchedKeyID)

			ctx := context.WithValue(r.Context(), TenantIDKey, tenantID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func GetTenantID(ctx context.Context) string {
	v, _ := ctx.Value(TenantIDKey).(string)
	return v
}
