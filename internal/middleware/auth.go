package middleware

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/paasd/paasd/internal/crypto"
)

type contextKey string

const TenantIDKey contextKey = "tenant_id"

// lastUsedTracker samples last_used_at updates — at most once per key per 5 minutes.
type lastUsedTracker struct {
	mu       sync.Mutex
	lastSeen map[string]time.Time
	db       *sql.DB
}

func newLastUsedTracker(db *sql.DB) *lastUsedTracker {
	return &lastUsedTracker{
		lastSeen: make(map[string]time.Time),
		db:       db,
	}
}

const lastUsedInterval = 5 * time.Minute

func (t *lastUsedTracker) maybeUpdate(keyID string) {
	t.mu.Lock()
	last, exists := t.lastSeen[keyID]
	now := time.Now()
	if exists && now.Sub(last) < lastUsedInterval {
		t.mu.Unlock()
		return
	}
	t.lastSeen[keyID] = now
	t.mu.Unlock()

	// Synchronous but sampled — runs at most once per key per 5 min
	t.db.Exec("UPDATE api_keys SET last_used_at = ? WHERE id = ?", now.Unix(), keyID)
}

func Auth(db *sql.DB) func(http.Handler) http.Handler {
	tracker := newLastUsedTracker(db)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				http.Error(w, `{"error":"missing authorization header"}`, http.StatusUnauthorized)
				return
			}

			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) != 2 || parts[0] != "Bearer" {
				http.Error(w, `{"error":"invalid authorization format"}`, http.StatusUnauthorized)
				return
			}
			token := parts[1]

			if len(token) < 8 || len(token) > 256 {
				http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
				return
			}
			prefix := token[:8]

			now := time.Now().Unix()
			rows, err := db.Query(
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
				if crypto.VerifyPassword(keyHash, token) {
					matched = true
					tenantID = tid
					matchedKeyID = keyID
					break
				}
			}

			if !matched {
				http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
				return
			}

			// Sampled last_used_at update (at most once per key per 5 min)
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
