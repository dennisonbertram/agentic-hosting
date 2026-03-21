package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/cache"
	"github.com/dennisonbertram/agentic-hosting/internal/crypto"
	"github.com/dennisonbertram/agentic-hosting/internal/middleware"
	"github.com/go-chi/chi/v5"
)

type RegisterRequest struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type RegisterResponse struct {
	TenantID string `json:"tenant_id"`
	APIKey   string `json:"api_key"`
}

type TenantResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Status    string `json:"status"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

type TenantUsageBucket struct {
	Used int `json:"used"`
	Max  int `json:"max"`
}

type TenantUsageResponse struct {
	Services  TenantUsageBucket `json:"services"`
	Databases TenantUsageBucket `json:"databases"`
	APIKeys   TenantUsageBucket `json:"api_keys"`
	MemoryMB  int               `json:"memory_mb"`
	CPUCores  float64           `json:"cpu_cores"`
	DiskGB    int               `json:"disk_gb"`
	RateLimit int               `json:"rate_limit"`
}

type UpdateTenantRequest struct {
	Name *string `json:"name,omitempty"`
}

var emailRegex = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)

type registrationLimiter struct {
	mu             sync.Mutex
	entries        *cache.LRU[string, *regEntry]
	globalCount    int
	globalWindowAt time.Time
}

type regEntry struct {
	count    int
	windowAt time.Time
}

const (
	regMaxPerIPPerHour = 5
	regGlobalPerHour   = 20
	regMaxEntries      = 10000
	regWindow          = 1 * time.Hour
	maxTenants         = 1000
)

var regLimiter = &registrationLimiter{
	entries: cache.New[string, *regEntry](regMaxEntries),
	// globalWindowAt zero-value: first request starts the window
}

func init() {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		for range ticker.C {
			now := time.Now()
			regLimiter.entries.DeleteFunc(func(_ string, v *regEntry) bool {
				return now.After(v.windowAt)
			})
		}
	}()
}

// allow returns whether the request is permitted and, if blocked, how long
// until the relevant window resets.
func (rl *registrationLimiter) allow(ip string) (bool, time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()

	if rl.globalWindowAt.IsZero() || now.After(rl.globalWindowAt) {
		rl.globalCount = 0
		rl.globalWindowAt = now.Add(regWindow)
	}
	if rl.globalCount >= regGlobalPerHour {
		return false, time.Until(rl.globalWindowAt)
	}

	entry, exists := rl.entries.Get(ip)
	if !exists || now.After(entry.windowAt) {
		rl.entries.Set(ip, &regEntry{count: 1, windowAt: now.Add(regWindow)})
		rl.globalCount++
		return true, 0
	}
	if entry.count >= regMaxPerIPPerHour {
		return false, time.Until(entry.windowAt)
	}
	entry.count++
	rl.globalCount++
	return true, 0
}

func generateID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func (s *Server) handleTenantRegister(w http.ResponseWriter, r *http.Request) {
	// Rate limit ALL registration attempts (including invalid tokens) to prevent
	// brute-force of the bootstrap token. Uses trusted real IP from proxy headers.
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

	// Bootstrap token gate — open registration (--dev --open-registration) is
	// the only path that skips this. Without that explicit flag, bootstrapToken
	// is always required (main.go fatals if unset outside dev+open-registration).
	if !s.openRegistration {
		// Fail closed: reject if bootstrap token is not configured
		if s.bootstrapToken == "" {
			writeError(w, http.StatusServiceUnavailable, "registration temporarily unavailable")
			return
		}
		provided := strings.TrimSpace(r.Header.Get("X-Bootstrap-Token"))
		// HMAC-compare to prevent length-leak from ConstantTimeCompare
		if !hmacEqual(provided, s.bootstrapToken, s.masterKey) {
			writeError(w, http.StatusUnauthorized, "missing or invalid bootstrap token")
			return
		}
	}

	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}

	if len(req.Name) < 2 {
		writeError(w, http.StatusBadRequest, "name must be at least 2 characters")
		return
	}
	if len(req.Name) > 128 {
		writeError(w, http.StatusBadRequest, "name must be at most 128 characters")
		return
	}

	if !emailRegex.MatchString(req.Email) {
		writeError(w, http.StatusBadRequest, "invalid email format")
		return
	}
	if len(req.Email) > 256 {
		writeError(w, http.StatusBadRequest, "email too long")
		return
	}

	// Enforce max tenant count AFTER input validation to minimize DB load
	// from malformed requests. Only counts active tenants.
	var tenantCount int
	if err := s.store.StateDB.QueryRow(`SELECT COUNT(*) FROM tenants WHERE status = 'active'`).Scan(&tenantCount); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if tenantCount >= maxTenants {
		writeError(w, http.StatusConflict, "quota exceeded: maximum tenants reached")
		return
	}

	tenantID, err := generateID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	now := time.Now().Unix()

	tx, err := s.store.StateDB.Begin()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
		 VALUES (?, ?, ?, 'active', ?, ?)`,
		tenantID, req.Name, req.Email, now, now,
	)
	if err != nil {
		// Return generic non-500 error to prevent email enumeration while still
		// signaling to the client that registration failed for a fixable reason.
		// Operators can check server logs for the actual constraint violation.
		writeError(w, http.StatusUnprocessableEntity, "registration failed, please check your details and try again")
		return
	}

	_, err = tx.Exec(
		`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`,
		tenantID,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	apiKey, keyID, err := crypto.GenerateAPIKeyWithID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	keyHash := crypto.HashAPIKey(apiKey, s.masterKey)
	// Use keyID prefix as display hint, not the secret's prefix
	if len(keyID) < 8 {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	prefix := keyID[:8]

	_, err = tx.Exec(
		`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at)
		 VALUES (?, ?, 'default', ?, ?, ?)`,
		keyID, tenantID, prefix, keyHash, now,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Return token in format "keyID.secret" for O(1) lookup
	token := keyID + "." + apiKey

	writeJSON(w, http.StatusCreated, RegisterResponse{
		TenantID: tenantID,
		APIKey:   token,
	})
}

func (s *Server) handleTenantGet(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var t TenantResponse
	err := s.store.StateDB.QueryRow(
		`SELECT id, name, email, status, created_at, updated_at FROM tenants WHERE id = ?`,
		tenantID,
	).Scan(&t.ID, &t.Name, &t.Email, &t.Status, &t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "tenant not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleTenantUsage(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var resp TenantUsageResponse
	err := s.store.StateDB.QueryRow(
		`SELECT q.max_services, q.max_databases, q.max_memory_mb, q.max_cpu_cores, q.max_disk_gb, q.api_rate_limit,
		    (SELECT COUNT(*) FROM services WHERE tenant_id = ?),
		    (SELECT COUNT(*) FROM databases WHERE tenant_id = ? AND status != 'failed'),
		    (SELECT COUNT(*) FROM api_keys WHERE tenant_id = ? AND revoked_at IS NULL)
		 FROM tenant_quotas q
		 WHERE q.tenant_id = ?`,
		tenantID, tenantID, tenantID, tenantID,
	).Scan(&resp.Services.Max, &resp.Databases.Max, &resp.MemoryMB, &resp.CPUCores, &resp.DiskGB, &resp.RateLimit,
		&resp.Services.Used, &resp.Databases.Used, &resp.APIKeys.Used)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "tenant quota not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	resp.APIKeys.Max = maxKeysPerTenant

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleTenantUpdate(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req UpdateTenantRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}

	if req.Name != nil {
		if len(*req.Name) < 2 || len(*req.Name) > 128 {
			writeError(w, http.StatusBadRequest, "name must be 2-128 characters")
			return
		}
		_, err := s.store.StateDB.Exec(
			`UPDATE tenants SET name = ?, updated_at = ? WHERE id = ?`,
			*req.Name, time.Now().Unix(), tenantID,
		)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to update tenant")
			return
		}
	}

	s.handleTenantGet(w, r)
}

func (s *Server) handleTenantDelete(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	now := time.Now().Unix()

	tx, err := s.store.StateDB.Begin()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`UPDATE tenants SET status = 'suspended', updated_at = ? WHERE id = ?`,
		now, tenantID,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete tenant")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusNotFound, "tenant not found")
		return
	}

	// Revoke all API keys for the suspended tenant
	_, err = tx.Exec(
		`UPDATE api_keys SET revoked_at = ? WHERE tenant_id = ? AND revoked_at IS NULL`,
		now, tenantID,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Evict all cached keys for this tenant so suspension takes effect immediately
	if s.authInvalidator != nil {
		s.authInvalidator.InvalidateTenant(tenantID)
	}

	// Cancel all active builds for this tenant to free build slots
	if s.buildManager != nil {
		s.buildManager.CancelAllForTenant(r.Context(), tenantID)
	}

	// Stop and remove all running containers for this tenant
	if s.svcManager != nil {
		s.svcManager.StopAllForTenant(r.Context(), tenantID)
	}

	// Stop all database containers for this tenant (records kept for potential reactivation)
	if s.dbManager != nil {
		s.dbManager.StopAllForTenant(r.Context(), tenantID)
	}

	// Stop the kanban board container for this tenant (record kept for potential reactivation)
	if s.kanbanManager != nil {
		s.kanbanManager.StopForTenant(r.Context(), tenantID)
	}

	w.WriteHeader(http.StatusNoContent)
}

// ReactivateResponse is the response body for POST /v1/tenants/{tenantID}/reactivate.
// It returns the new API key so the operator can hand it to the tenant.
type ReactivateResponse struct {
	TenantID string `json:"tenant_id"`
	APIKey   string `json:"api_key"`
	Status   string `json:"status"`
}

// handleTenantReactivate handles POST /v1/tenants/{tenantID}/reactivate.
//
// Security model:
//   - Rate-limited by the same per-IP / global limiter used for tenant
//     registration (5/IP/hour, 20/global/hour).
//   - Requires bootstrap token via X-Bootstrap-Token header.
//   - Only suspended tenants can be reactivated; already-active returns 409.
//   - A new API key is generated and returned (one-time display).
func (s *Server) handleTenantReactivate(w http.ResponseWriter, r *http.Request) {
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
		writeError(w, http.StatusServiceUnavailable, "reactivation temporarily unavailable")
		return
	}

	// Verify bootstrap token via HMAC-compare (no length leak).
	provided := strings.TrimSpace(r.Header.Get("X-Bootstrap-Token"))
	if !hmacEqual(provided, s.bootstrapToken, s.masterKey) {
		writeError(w, http.StatusUnauthorized, "missing or invalid bootstrap token")
		return
	}

	tenantID := chi.URLParam(r, "tenantID")
	if tenantID == "" {
		writeError(w, http.StatusBadRequest, "tenant ID is required")
		return
	}

	// Look up the tenant and check its current status.
	var currentStatus string
	err := s.store.StateDB.QueryRow(
		`SELECT status FROM tenants WHERE id = ?`, tenantID,
	).Scan(&currentStatus)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "tenant not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if currentStatus == "active" {
		writeError(w, http.StatusConflict, "tenant is already active")
		return
	}

	now := time.Now().Unix()

	tx, err := s.store.StateDB.Begin()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback()

	// Set tenant status back to active.
	_, err = tx.Exec(
		`UPDATE tenants SET status = 'active', updated_at = ? WHERE id = ?`,
		now, tenantID,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reactivate tenant")
		return
	}

	// Generate a new API key for the reactivated tenant since all keys
	// were revoked on suspension.
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

	_, err = tx.Exec(
		`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at)
		 VALUES (?, ?, 'reactivation', ?, ?, ?)`,
		keyID, tenantID, prefix, keyHash, now,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create reactivation key")
		return
	}

	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Return token in format "keyID.secret" for O(1) lookup.
	token := keyID + "." + apiKey

	writeJSON(w, http.StatusOK, ReactivateResponse{
		TenantID: tenantID,
		APIKey:   token,
		Status:   "active",
	})
}

// hmacEqual compares two strings in constant time regardless of length.
// Unlike subtle.ConstantTimeCompare, this does not leak the length of either input.
// Uses the server's master key as the HMAC key for domain separation.
func hmacEqual(a, b string, key []byte) bool {
	macA := hmac.New(sha256.New, key)
	macA.Write([]byte(a))
	macB := hmac.New(sha256.New, key)
	macB.Write([]byte(b))
	return hmac.Equal(macA.Sum(nil), macB.Sum(nil))
}
