package middleware

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/cache"
	"github.com/dennisonbertram/agentic-hosting/internal/crypto"
)

type contextKey string

const TenantIDKey contextKey = "tenant_id"

const (
	lastUsedInterval = 5 * time.Minute
	lastUsedMaxKeys  = 10000
	authCacheTTL     = 30 * time.Second
	authCacheMaxKeys = 5000
)

// authCacheEntry caches a validated key's DB result to reduce SQLite load.
// The HMAC verification still runs on every request (fast, in-memory).
type authCacheEntry struct {
	tenantID  string
	keyHash   string
	status    string
	expiresAt *int64 // key expiration time (nil = no expiry)
	cachedAt  time.Time
}

type authCache struct {
	lru *cache.LRU[string, *authCacheEntry]
}

func newAuthCache() *authCache {
	c := &authCache{
		lru: cache.New[string, *authCacheEntry](authCacheMaxKeys),
	}
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		for range ticker.C {
			now := time.Now()
			c.lru.DeleteFunc(func(_ string, v *authCacheEntry) bool {
				return now.Sub(v.cachedAt) > authCacheTTL
			})
		}
	}()
	return c
}

func (c *authCache) get(keyID string) (*authCacheEntry, bool) {
	entry, ok := c.lru.Get(keyID)
	if !ok || time.Since(entry.cachedAt) > authCacheTTL {
		return nil, false
	}
	return entry, true
}

func (c *authCache) set(keyID string, entry *authCacheEntry) {
	c.lru.Set(keyID, entry)
}

func (c *authCache) invalidate(keyID string) {
	c.lru.Delete(keyID)
}

// AuthCacheInvalidator allows callers to evict entries from the auth cache
// when keys are revoked or tenants are suspended.
type AuthCacheInvalidator struct {
	cache *authCache
}

// InvalidateKey removes a single key from the auth cache.
func (a *AuthCacheInvalidator) InvalidateKey(keyID string) {
	a.cache.invalidate(keyID)
}

// InvalidateTenant removes all cached keys belonging to the given tenant.
func (a *AuthCacheInvalidator) InvalidateTenant(tenantID string) {
	a.cache.lru.DeleteFunc(func(_ string, v *authCacheEntry) bool {
		return v.tenantID == tenantID
	})
}

// lastUsedTracker samples last_used_at updates with bounded LRU cache.
type lastUsedTracker struct {
	lru *cache.LRU[string, time.Time]
	db  *sql.DB
}

func newLastUsedTracker(db *sql.DB) *lastUsedTracker {
	t := &lastUsedTracker{
		lru: cache.New[string, time.Time](lastUsedMaxKeys),
		db:  db,
	}
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		for range ticker.C {
			cutoff := time.Now().Add(-24 * time.Hour)
			t.lru.DeleteFunc(func(_ string, v time.Time) bool {
				return v.Before(cutoff)
			})
		}
	}()
	return t
}

func (t *lastUsedTracker) maybeUpdate(keyID string) {
	now := time.Now()
	if last, ok := t.lru.Get(keyID); ok && now.Sub(last) < lastUsedInterval {
		return
	}
	t.lru.Set(keyID, now)

	if _, err := t.db.Exec("UPDATE api_keys SET last_used_at = ? WHERE id = ?", now.Unix(), keyID); err != nil {
		log.Printf("auth: failed to update last_used_at for key %s: %v", keyID, err)
	}
}

func Auth(db *sql.DB, masterKey []byte) (func(http.Handler) http.Handler, *AuthCacheInvalidator) {
	tracker := newLastUsedTracker(db)
	cache := newAuthCache()
	invalidator := &AuthCacheInvalidator{cache: cache}

	mw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				writeJSONError(w, http.StatusUnauthorized, "missing authorization header")
				return
			}

			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
				writeJSONError(w, http.StatusUnauthorized, "invalid authorization format, expected: Bearer <token>")
				return
			}
			token := strings.TrimSpace(parts[1])

			// Token format: "keyID.secret" for O(1) lookup
			dotIdx := strings.IndexByte(token, '.')
			if dotIdx < 1 || dotIdx >= len(token)-1 {
				writeJSONError(w, http.StatusUnauthorized, "invalid api key")
				return
			}
			keyID := token[:dotIdx]
			secret := token[dotIdx+1:]

			if len(keyID) > 64 || len(secret) > 256 {
				writeJSONError(w, http.StatusUnauthorized, "invalid api key")
				return
			}

			var tenantID, keyHash, status string

			// Check auth cache first to reduce DB load
			if cached, ok := cache.get(keyID); ok {
				// Verify expiry locally even on cache hit
				if cached.expiresAt != nil && time.Now().Unix() > *cached.expiresAt {
					cache.invalidate(keyID) // force DB re-check on next request
					writeJSONError(w, http.StatusUnauthorized, "invalid api key")
					return
				}
				tenantID = cached.tenantID
				keyHash = cached.keyHash
				status = cached.status
			} else {
				now := time.Now().Unix()
				var expiresAt *int64
				err := db.QueryRowContext(r.Context(),
					`SELECT ak.tenant_id, ak.key_hash, t.status, ak.expires_at
					 FROM api_keys ak
					 JOIN tenants t ON t.id = ak.tenant_id
					 WHERE ak.id = ?
					   AND ak.revoked_at IS NULL
					   AND (ak.expires_at IS NULL OR ak.expires_at > ?)`,
					keyID, now,
				).Scan(&tenantID, &keyHash, &status, &expiresAt)
				if err == sql.ErrNoRows {
					writeJSONError(w, http.StatusUnauthorized, "invalid api key")
					return
				}
				if err != nil {
					writeJSONError(w, http.StatusServiceUnavailable, "service temporarily unavailable")
					return
				}

				cache.set(keyID, &authCacheEntry{
					tenantID:  tenantID,
					keyHash:   keyHash,
					status:    status,
					expiresAt: expiresAt,
					cachedAt:  time.Now(),
				})
			}

			if status != "active" {
				writeJSONError(w, http.StatusUnauthorized, "invalid api key")
				return
			}

			if !crypto.VerifyAPIKey(keyHash, secret, masterKey) {
				writeJSONError(w, http.StatusUnauthorized, "invalid api key")
				return
			}

			tracker.maybeUpdate(keyID)

			ctx := context.WithValue(r.Context(), TenantIDKey, tenantID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}

	return mw, invalidator
}

func GetTenantID(ctx context.Context) string {
	v, _ := ctx.Value(TenantIDKey).(string)
	return v
}
