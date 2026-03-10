package middleware

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"

	"github.com/paasd/paasd/internal/crypto"
)

type contextKey string

const TenantIDKey contextKey = "tenant_id"

// lastUsedUpdater is a bounded worker pool for async last_used_at updates.
type lastUsedUpdater struct {
	ch chan lastUsedUpdate
}

type lastUsedUpdate struct {
	db    *sql.DB
	keyID string
	ts    int64
}

var updater = newLastUsedUpdater(10)

func newLastUsedUpdater(workers int) *lastUsedUpdater {
	u := &lastUsedUpdater{
		ch: make(chan lastUsedUpdate, 100),
	}
	for i := 0; i < workers; i++ {
		go u.worker()
	}
	return u
}

func (u *lastUsedUpdater) worker() {
	for upd := range u.ch {
		upd.db.Exec("UPDATE api_keys SET last_used_at = ? WHERE id = ?", upd.ts, upd.keyID)
	}
}

func (u *lastUsedUpdater) submit(db *sql.DB, keyID string) {
	select {
	case u.ch <- lastUsedUpdate{db: db, keyID: keyID, ts: time.Now().Unix()}:
	default:
		// Drop update if buffer full — non-critical
	}
}

func Auth(db *sql.DB) func(http.Handler) http.Handler {
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

			// Bounded async update of last_used_at
			updater.submit(db, matchedKeyID)

			ctx := context.WithValue(r.Context(), TenantIDKey, tenantID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func GetTenantID(ctx context.Context) string {
	v, _ := ctx.Value(TenantIDKey).(string)
	return v
}
