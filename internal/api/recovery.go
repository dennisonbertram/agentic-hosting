package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/crypto"
)

// KeyRecoverRequest is the body accepted by POST /v1/auth/recover.
// Authentication is via bootstrap token, not an API key, so this endpoint
// can be called even when the tenant has no valid keys remaining.
type KeyRecoverRequest struct {
	Email          string `json:"email"`
	BootstrapToken string `json:"bootstrap_token"`
}

// KeyRecoverResponse mirrors CreateKeyResponse so callers can treat both
// responses the same way.  The raw key is shown exactly once.
type KeyRecoverResponse struct {
	ID        string `json:"id"`
	Key       string `json:"key"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"created_at"`
}

// handleKeyRecover handles POST /v1/auth/recover.
//
// Security model:
//   - Rate-limited by the same per-IP / global limiter used for tenant
//     registration (5/IP/hour, 20/global/hour) — limits brute-force of the
//     bootstrap token.
//   - Bootstrap token is verified via HMAC-compare (no length leak).
//   - Unknown-email and bad-token responses are deliberately identical (401)
//     to prevent email enumeration.
func (s *Server) handleKeyRecover(w http.ResponseWriter, r *http.Request) {
	// Apply the same rate limiter as tenant registration to prevent
	// brute-force of the bootstrap token via this endpoint.
	ip := trustedRealIP(r)
	if ok, wait := regLimiter.allow(ip); !ok {
		secs := int(math.Ceil(wait.Seconds()))
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded, try again later")
		return
	}

	// Fail closed: reject if bootstrap token is not configured.
	if s.bootstrapToken == "" {
		writeError(w, http.StatusServiceUnavailable, "recovery temporarily unavailable")
		return
	}

	var req KeyRecoverRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}

	// HMAC-compare the provided token to prevent length-leak attacks.
	provided := strings.TrimSpace(req.BootstrapToken)
	if !hmacEqual(provided, s.bootstrapToken, s.masterKey) {
		writeError(w, http.StatusUnauthorized, "invalid bootstrap token or email")
		return
	}

	if !emailRegex.MatchString(req.Email) {
		writeError(w, http.StatusBadRequest, "invalid email format")
		return
	}

	// Look up the tenant by email. Intentionally return the same 401 as a bad
	// token so the response cannot be used to enumerate registered emails.
	var tenantID, tenantStatus string
	err := s.store.StateDB.QueryRow(
		`SELECT id, status FROM tenants WHERE email = ?`,
		req.Email,
	).Scan(&tenantID, &tenantStatus)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusUnauthorized, "invalid bootstrap token or email")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if tenantStatus != "active" {
		writeError(w, http.StatusForbidden, "tenant account is not active")
		return
	}

	// Enforce the same per-tenant key cap that handleKeyCreate enforces.
	var keyCount int
	if err := s.store.StateDB.QueryRow(
		`SELECT COUNT(*) FROM api_keys WHERE tenant_id = ? AND revoked_at IS NULL`,
		tenantID,
	).Scan(&keyCount); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if keyCount >= maxKeysPerTenant {
		writeError(w, http.StatusForbidden, "maximum API keys reached; revoke unused keys first")
		return
	}

	apiKey, keyID, err := crypto.GenerateAPIKeyWithID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	keyHash := crypto.HashAPIKey(apiKey, s.masterKey)
	if len(keyID) < 8 {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	prefix := keyID[:8]

	now := time.Now().Unix()
	keyName := fmt.Sprintf("recovery-%s", time.Now().UTC().Format("20060102"))

	_, err = s.store.StateDB.Exec(
		`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		keyID, tenantID, keyName, prefix, keyHash, now,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create recovery key")
		return
	}

	// Return token in format "keyID.secret" for O(1) lookup.
	// This is the only time the raw key is shown.
	token := keyID + "." + apiKey

	writeJSON(w, http.StatusCreated, KeyRecoverResponse{
		ID:        keyID,
		Key:       token,
		Name:      keyName,
		CreatedAt: now,
	})
}
