package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/crypto"
	"github.com/dennisonbertram/agentic-hosting/internal/middleware"
	"github.com/go-chi/chi/v5"
)

const maxKeysPerTenant = 20

// liveAPIKeySQL is the WHERE fragment for keys that occupy a quota slot:
// not revoked and not past expires_at. Bind one unix-seconds argument.
const liveAPIKeySQL = `revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`

type CreateKeyRequest struct {
	Name      string `json:"name"`
	ExpiresIn *int64 `json:"expires_in,omitempty"`
}

type CreateKeyResponse struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	APIKey  string `json:"api_key"`
	Prefix  string `json:"prefix"`
	Expires *int64 `json:"expires_at,omitempty"`
}

type KeyInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Prefix     string `json:"prefix"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt *int64 `json:"last_used_at,omitempty"`
	ExpiresAt  *int64 `json:"expires_at,omitempty"`
}

func (s *Server) handleKeyCreate(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req CreateKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}

	if req.Name == "" {
		req.Name = "unnamed"
	}

	if req.ExpiresIn != nil {
		if *req.ExpiresIn <= 0 {
			writeError(w, http.StatusBadRequest, "expires_in must be positive (seconds)")
			return
		}
		// Cap at 10 years to prevent effectively never-expiring keys
		const maxExpiresIn = 10 * 365 * 24 * 3600 // ~10 years in seconds
		if *req.ExpiresIn > maxExpiresIn {
			writeError(w, http.StatusBadRequest, "expires_in exceeds maximum (10 years)")
			return
		}
	}

	key, err := s.mintAPIKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	now := time.Now().Unix()

	var expiresAt *int64
	if req.ExpiresIn != nil {
		exp := now + *req.ExpiresIn
		expiresAt = &exp
	}

	// The quota check is part of the INSERT so concurrent creates cannot both
	// pass a separate COUNT and push the tenant past maxKeysPerTenant.
	res, err := s.store.StateDB.Exec(
		`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at, expires_at)
		 SELECT ?, ?, ?, ?, ?, ?, ?
		 WHERE (SELECT COUNT(*) FROM api_keys WHERE tenant_id = ? AND `+liveAPIKeySQL+`) < ?`,
		key.id, tenantID, req.Name, key.prefix, key.hash, now, expiresAt,
		tenantID, now, maxKeysPerTenant,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create key")
		return
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		writeError(w, http.StatusConflict, "quota exceeded: maximum API keys reached, revoke unused keys first")
		return
	}

	writeJSON(w, http.StatusCreated, CreateKeyResponse{
		ID:      key.id,
		Name:    req.Name,
		APIKey:  key.token,
		Prefix:  key.prefix,
		Expires: expiresAt,
	})
}

// newAPIKey is a freshly generated API key that has not been stored yet.
type newAPIKey struct {
	id     string // api_keys primary key
	prefix string // display hint: first 8 chars of the key ID, never of the secret
	hash   string // HMAC of the secret, stored as api_keys.key_hash
	token  string // "keyID.secret" (O(1) lookup by auth); shown to the caller once
}

// mintAPIKey generates a key ID and secret and derives the stored hash and the
// one-time token.
func (s *Server) mintAPIKey() (newAPIKey, error) {
	secret, keyID, err := crypto.GenerateAPIKeyWithID()
	if err != nil {
		return newAPIKey{}, err
	}
	return newAPIKey{
		id:     keyID,
		prefix: keyID[:8], // GenerateAPIKeyWithID always returns 32 hex chars
		hash:   crypto.HashAPIKey(secret, s.apiKeyHashKey),
		token:  keyID + "." + secret,
	}, nil
}

func (s *Server) handleKeyList(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	now := time.Now().Unix()
	rows, err := s.store.StateDB.Query(
		`SELECT id, name, key_prefix, created_at, last_used_at, expires_at
		 FROM api_keys
		 WHERE tenant_id = ? AND `+liveAPIKeySQL+`
		 ORDER BY created_at DESC
		 LIMIT 100`,
		tenantID, now,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list keys")
		return
	}
	defer rows.Close()

	keys := []KeyInfo{}
	for rows.Next() {
		var k KeyInfo
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.CreatedAt, &k.LastUsedAt, &k.ExpiresAt); err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list keys")
		return
	}

	writeJSON(w, http.StatusOK, keys)
}

func (s *Server) handleKeyRevoke(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	keyID := chi.URLParam(r, "keyID")

	result, err := s.store.StateDB.Exec(
		`UPDATE api_keys SET revoked_at = ? WHERE id = ? AND tenant_id = ? AND revoked_at IS NULL`,
		time.Now().Unix(), keyID, tenantID,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to revoke key")
		return
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		writeError(w, http.StatusNotFound, "key not found")
		return
	}

	// Evict from auth cache so revocation takes effect immediately
	if s.authInvalidator != nil {
		s.authInvalidator.InvalidateKey(keyID)
	}

	w.WriteHeader(http.StatusNoContent)
}
