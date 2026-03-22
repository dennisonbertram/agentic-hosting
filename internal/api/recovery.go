package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
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
// When a key slot was auto-revoked to make room, Warning and RevokedKeyID
// are populated so the caller knows what happened.
type KeyRecoverResponse struct {
	ID           string `json:"id"`
	Key          string `json:"key"`
	Name         string `json:"name"`
	CreatedAt    int64  `json:"created_at"`
	Warning      string `json:"warning,omitempty"`
	RevokedKeyID string `json:"revoked_key_id,omitempty"`
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

	// Fail closed: reject if no bootstrap tokens are configured.
	if len(s.bootstrapTokens) == 0 {
		writeError(w, http.StatusServiceUnavailable, "recovery temporarily unavailable")
		return
	}

	var req KeyRecoverRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}

	// HMAC-compare against all configured tokens to support graceful rotation.
	provided := strings.TrimSpace(req.BootstrapToken)
	if !validateBootstrapToken(provided, s.bootstrapTokens, s.masterKey) {
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

	// Check how many non-revoked keys the tenant has.
	var keyCount int
	if err := s.store.StateDB.QueryRow(
		`SELECT COUNT(*) FROM api_keys WHERE tenant_id = ? AND revoked_at IS NULL`,
		tenantID,
	).Scan(&keyCount); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// If the keyring is full, auto-revoke the oldest key to make room.
	// Recovery is a last-resort operation — it should always succeed for a
	// legitimate tenant, even if they have filled all key slots.
	var revokedKeyID string
	if keyCount >= maxKeysPerTenant {
		// Prefer revoking an expired key first (least disruptive).
		revokedID, err := s.store.RevokeOldestExpired(tenantID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if revokedID == "" {
			// No expired keys — revoke the oldest key by created_at.
			revokedID, err = s.store.RevokeOldest(tenantID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
		}
		if revokedID == "" {
			// Should be unreachable: keyCount >= 20 but nothing to revoke.
			writeError(w, http.StatusConflict, "quota exceeded: maximum API keys reached; revoke unused keys first")
			return
		}
		revokedKeyID = revokedID
		log.Printf("AUDIT: action=recovery.key_auto_revoked tenant=%s revoked_key=%s reason=keyring_full", tenantID, revokedKeyID)

		// Evict the revoked key from the auth cache so it stops working immediately.
		if s.authInvalidator != nil {
			s.authInvalidator.InvalidateKey(revokedKeyID)
		}
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

	resp := KeyRecoverResponse{
		ID:        keyID,
		Key:       token,
		Name:      keyName,
		CreatedAt: now,
	}
	if revokedKeyID != "" {
		resp.Warning = "key slot was full; revoked oldest key"
		resp.RevokedKeyID = revokedKeyID
	}

	writeJSON(w, http.StatusCreated, resp)
}
