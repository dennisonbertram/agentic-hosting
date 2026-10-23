package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/cache"
	"github.com/dennisonbertram/agentic-hosting/internal/environments"
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
	Sandboxes TenantUsageBucket `json:"sandboxes"`
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

// adminLimiter rate-limits the admin-token routes (recover, reactivate,
// suspend, quotas) with the same per-IP/global limits as registration but in separate
// buckets, so bootstrap-token holders exhausting the registration budget
// cannot lock the operator out of admin endpoints (and vice versa).
var adminLimiter = &registrationLimiter{
	entries: cache.New[string, *regEntry](regMaxEntries),
}

// resetLimitersForTest resets both the registration and admin limiters.
func resetLimitersForTest() {
	regLimiter.resetForTest()
	adminLimiter.resetForTest()
}

// resetForTest resets the rate limiter state. Exported only for tests within
// the api package (unexported, same-package access only).
func (rl *registrationLimiter) resetForTest() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.globalCount = 0
	rl.globalWindowAt = time.Time{}
	rl.entries = cache.New[string, *regEntry](regMaxEntries)
}

func init() {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		for range ticker.C {
			now := time.Now()
			for _, rl := range []*registrationLimiter{regLimiter, adminLimiter} {
				rl.entries.DeleteFunc(func(_ string, v *regEntry) bool {
					return now.After(v.windowAt)
				})
			}
		}
	}()
}

// allow returns whether the request is permitted and, if blocked, how long
// until the relevant window resets. It charges the per-IP bucket for every
// request but only checks (does not charge) the global bucket.
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
		return true, 0
	}
	if entry.count >= regMaxPerIPPerHour {
		return false, time.Until(entry.windowAt)
	}
	entry.count++
	return true, 0
}

// allowRequest charges the per-IP bucket for r's trusted client IP. When the
// request is over the limit it writes a 429 with Retry-After and returns false.
func (rl *registrationLimiter) allowRequest(w http.ResponseWriter, r *http.Request) bool {
	ok, wait := rl.allow(trustedRealIP(r))
	if ok {
		return true
	}
	secs := int(math.Ceil(wait.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeError(w, http.StatusTooManyRequests, "rate limit exceeded, try again later")
	return false
}

// chargeGlobal counts one authenticated request against the global hourly
// budget. allow only checks the global bucket; it is charged here, after the
// token validated, so invalid requests cannot exhaust it.
func (rl *registrationLimiter) chargeGlobal() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	if rl.globalWindowAt.IsZero() || now.After(rl.globalWindowAt) {
		rl.globalCount = 0
		rl.globalWindowAt = now.Add(regWindow)
	}
	rl.globalCount++
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
	if !regLimiter.allowRequest(w, r) {
		return
	}

	// Bootstrap token gate — open registration (--dev --open-registration) is
	// the only path that skips this. Without that explicit flag, bootstrapTokens
	// is always required (main.go fatals if unset outside dev+open-registration).
	if !s.openRegistration {
		// Fail closed: reject if no bootstrap tokens are configured
		if len(s.bootstrapTokens) == 0 {
			writeError(w, http.StatusServiceUnavailable, "registration temporarily unavailable")
			return
		}
		provided := strings.TrimSpace(r.Header.Get("X-Bootstrap-Token"))
		// HMAC-compare against all configured tokens to support graceful rotation.
		if !validateBootstrapToken(provided, s.bootstrapTokens, s.masterKey) {
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

	key, err := s.mintAPIKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	_, err = tx.Exec(
		`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at, role)
		 VALUES (?, ?, 'default', ?, ?, ?, 'owner')`,
		key.id, tenantID, key.prefix, key.hash, now,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Charge the global registration budget only for a tenant actually
	// created, so requests that fail validation cannot exhaust it.
	regLimiter.chargeGlobal()

	writeJSON(w, http.StatusCreated, RegisterResponse{
		TenantID: tenantID,
		APIKey:   key.token,
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

	now := time.Now().Unix()
	var resp TenantUsageResponse
	err := s.store.StateDB.QueryRow(
		`SELECT q.max_services, q.max_databases, q.max_memory_mb, q.max_cpu_cores, q.max_disk_gb, q.api_rate_limit, q.max_sandboxes,
		    (SELECT COUNT(*) FROM services WHERE tenant_id = ?),
		    (SELECT COUNT(*) FROM databases WHERE tenant_id = ? AND status != 'failed'),
		    (SELECT COUNT(*) FROM api_keys WHERE tenant_id = ? AND `+liveAPIKeySQL+`),
		    (SELECT COUNT(*) FROM environments WHERE tenant_id = ? AND kind = 'sandbox' AND status != 'failed')
		 FROM tenant_quotas q
		 WHERE q.tenant_id = ?`,
		tenantID, tenantID, tenantID, now, tenantID, tenantID,
	).Scan(&resp.Services.Max, &resp.Databases.Max, &resp.MemoryMB, &resp.CPUCores, &resp.DiskGB, &resp.RateLimit, &resp.Sandboxes.Max,
		&resp.Services.Used, &resp.Databases.Used, &resp.APIKeys.Used, &resp.Sandboxes.Used)
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
	if !requireKeyRole(w, r, middleware.KeyRoleOwner, "this key cannot suspend the tenant") {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	if !s.commitTenantSuspension(w, tenantID, false) {
		return
	}

	// Cleanup must outlive the request (30s cap, client disconnects), so run it
	// on a detached context with its own timeout.
	cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Minute)
	defer cancelCleanup()
	s.stopTenantWorkloads(cleanupCtx, tenantID)

	w.WriteHeader(http.StatusNoContent)
}

// handleTenantSuspend is the operator kill switch: POST /v1/tenants/{id}/suspend.
// Same effect as DELETE /v1/tenant (status suspended, keys revoked, workloads
// stopped, records kept) but authorized with the admin token so the operator
// does not need a leftover tenant key.
func (s *Server) handleTenantSuspend(w http.ResponseWriter, r *http.Request) {
	if !adminLimiter.allowRequest(w, r) {
		return
	}
	if !s.requireAdmin(w, adminTokenFromRequest(r, r.Header.Get("X-Bootstrap-Token")), "suspension temporarily unavailable") {
		return
	}

	tenantID := chi.URLParam(r, "tenantID")
	if tenantID == "" {
		writeError(w, http.StatusBadRequest, "tenant ID is required")
		return
	}

	if !s.commitTenantSuspension(w, tenantID, true) {
		return
	}

	log.Printf("AUDIT: action=tenant.suspended tenant=%s auth=admin ip=%s", tenantID, trustedRealIP(r))

	// Stop workloads in the background so the 30s public-route timeout cannot
	// hide a successful suspend from the operator. Auth is already dead.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		s.stopTenantWorkloads(ctx, tenantID)
	}()

	w.WriteHeader(http.StatusNoContent)
}

// commitTenantSuspension marks the tenant suspended and revokes its keys.
// requireActive is the admin path: missing tenant is 404, already suspended
// is 409. The tenant self-delete path (requireActive=false) keeps the previous
// UPDATE-by-id behavior. Returns false after writing an error response.
func (s *Server) commitTenantSuspension(w http.ResponseWriter, tenantID string, requireActive bool) bool {
	now := time.Now().Unix()

	tx, err := s.store.StateDB.Begin()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return false
	}
	defer tx.Rollback()

	if requireActive {
		var current string
		err := tx.QueryRow(`SELECT status FROM tenants WHERE id = ?`, tenantID).Scan(&current)
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "tenant not found")
			return false
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return false
		}
		if current == "suspended" {
			writeError(w, http.StatusConflict, "tenant is already suspended")
			return false
		}
	}

	res, err := tx.Exec(
		`UPDATE tenants SET status = 'suspended', updated_at = ? WHERE id = ?`,
		now, tenantID,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete tenant")
		return false
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusNotFound, "tenant not found")
		return false
	}

	_, err = tx.Exec(
		`UPDATE api_keys SET revoked_at = ? WHERE tenant_id = ? AND revoked_at IS NULL`,
		now, tenantID,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return false
	}

	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return false
	}

	if s.authInvalidator != nil {
		s.authInvalidator.InvalidateTenant(tenantID)
	}
	return true
}

// stopTenantWorkloads cancels builds and stops environments, services,
// databases and kanban for a just-suspended tenant. Records are kept for
// reactivation.
func (s *Server) stopTenantWorkloads(ctx context.Context, tenantID string) {
	if s.buildManager != nil {
		s.buildManager.CancelAllForTenant(ctx, tenantID)
	}
	if s.envManager != nil {
		s.envManager.StopAllForTenant(ctx, tenantID)
	}
	if s.svcManager != nil {
		s.svcManager.StopAllForTenant(ctx, tenantID)
	}
	if s.dbManager != nil {
		s.dbManager.StopAllForTenant(ctx, tenantID)
	}
	if s.kanbanManager != nil {
		s.kanbanManager.StopForTenant(ctx, tenantID)
	}
	if s.envManager != nil {
		s.envManager.StopAllForTenant(ctx, tenantID)
	}
}

// ReactivateResponse is the response body for POST /v1/tenants/{tenantID}/reactivate.
// It returns the new API key so the operator can hand it to the tenant.
type ReactivateResponse struct {
	TenantID string `json:"tenant_id"`
	APIKey   string `json:"api_key"`
	Status   string `json:"status"`
}

// reactivateAfterStatusCheck is a test hook run between the reactivation
// status check and the guarded UPDATE.
var reactivateAfterStatusCheck func()

// handleTenantReactivate handles POST /v1/tenants/{tenantID}/reactivate.
//
// Security model:
//   - Rate-limited by adminLimiter (5/IP/hour, 20/global/hour), separate
//     buckets from registration's regLimiter; the global bucket is charged
//     only after a successful admin-token check (requireAdmin).
//   - Requires the admin token (X-Admin-Token, or X-Bootstrap-Token carrying it); falls back to bootstrap tokens when no admin token is configured.
//   - Only suspended tenants can be reactivated; already-active returns 409.
//   - A new API key is generated and returned (one-time display).
func (s *Server) handleTenantReactivate(w http.ResponseWriter, r *http.Request) {
	// Admin-route limiter (separate from registration's) to prevent
	// brute-force of the admin/bootstrap token via this endpoint.
	if !adminLimiter.allowRequest(w, r) {
		return
	}

	if !s.requireAdmin(w, adminTokenFromRequest(r, r.Header.Get("X-Bootstrap-Token")), "reactivation temporarily unavailable") {
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
	if reactivateAfterStatusCheck != nil {
		reactivateAfterStatusCheck()
	}

	now := time.Now().Unix()

	tx, err := s.store.StateDB.Begin()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback()

	// Set tenant status back to active. The status guard makes a concurrent
	// reactivation (which passed the check above) a 409 instead of a second
	// key and a second round of container restarts.
	res, err := tx.Exec(
		`UPDATE tenants SET status = 'active', updated_at = ? WHERE id = ? AND status != 'active'`,
		now, tenantID,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reactivate tenant")
		return
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		writeError(w, http.StatusConflict, "tenant is already active")
		return
	}

	// Generate a new API key for the reactivated tenant since all keys
	// were revoked on suspension.
	key, err := s.mintAPIKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	_, err = tx.Exec(
		`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at, role)
		 VALUES (?, ?, 'reactivation', ?, ?, ?, 'owner')`,
		key.id, tenantID, key.prefix, key.hash, now,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create reactivation key")
		return
	}

	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Recreate the database and kanban containers that suspension removed.
	// They wait for health checks (up to ~30-60s each), so run in the background.
	if s.dbManager != nil || s.kanbanManager != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if s.dbManager != nil {
				s.dbManager.StartAllForTenant(ctx, tenantID)
			}
			if s.kanbanManager != nil {
				s.kanbanManager.StartForTenant(ctx, tenantID)
			}
		}()
	}

	writeJSON(w, http.StatusOK, ReactivateResponse{
		TenantID: tenantID,
		APIKey:   key.token,
		Status:   "active",
	})
}

// QuotaUpdateRequest is the request body for PATCH /v1/tenants/{tenantID}/quotas.
// All fields are optional pointers — only non-nil fields are updated.
type QuotaUpdateRequest struct {
	MaxServices          *int `json:"max_services,omitempty"`
	MaxDatabases         *int `json:"max_databases,omitempty"`
	MaxBuildsConcurrent  *int `json:"max_builds_concurrent,omitempty"`
	MaxEnvVarsPerService *int `json:"max_env_vars_per_service,omitempty"`
	// Sandbox quotas. max_sandboxes may be 0 to disable sandboxes for a
	// tenant; sandbox_egress_allowed is 0 or 1 and takes effect only when
	// the server also has a sandbox egress network (otherwise egress is
	// refused). max_sandbox_disk_mb caps disk_mb when sandbox workspaces are
	// tmpfs (AH_SANDBOX_WORKSPACE_TMPFS).
	MaxSandboxes              *int `json:"max_sandboxes,omitempty"`
	MaxSandboxTTLSeconds      *int `json:"max_sandbox_ttl_seconds,omitempty"`
	MaxSandboxLifetimeSeconds *int `json:"max_sandbox_lifetime_seconds,omitempty"`
	SandboxEgressAllowed      *int `json:"sandbox_egress_allowed,omitempty"`
	MaxSandboxDiskMB          *int `json:"max_sandbox_disk_mb,omitempty"`
}

// QuotaUpdateResponse is the response body for PATCH /v1/tenants/{tenantID}/quotas.
type QuotaUpdateResponse struct {
	TenantID             string  `json:"tenant_id"`
	MaxServices          int     `json:"max_services"`
	MaxDatabases         int     `json:"max_databases"`
	MaxBuildsConcurrent  int     `json:"max_builds_concurrent"`
	MaxEnvVarsPerService int     `json:"max_env_vars_per_service"`
	MaxMemoryMB          int     `json:"max_memory_mb"`
	MaxCPUCores          float64 `json:"max_cpu_cores"`
	MaxDiskGB            int     `json:"max_disk_gb"`
	APIRateLimit         int     `json:"api_rate_limit"`

	MaxSandboxes              int `json:"max_sandboxes"`
	MaxSandboxTTLSeconds      int `json:"max_sandbox_ttl_seconds"`
	MaxSandboxLifetimeSeconds int `json:"max_sandbox_lifetime_seconds"`
	SandboxEgressAllowed      int `json:"sandbox_egress_allowed"`
	MaxSandboxDiskMB          int `json:"max_sandbox_disk_mb"`
}

// Quota caps — hard upper bounds for operator-set quotas.
const (
	quotaCapMaxServices          = 100
	quotaCapMaxDatabases         = 50
	quotaCapMaxBuildsConcurrent  = 10
	quotaCapMaxEnvVarsPerService = 500

	quotaCapMaxSandboxes              = 50
	quotaCapMaxSandboxTTLSeconds      = 86400
	quotaCapMaxSandboxLifetimeSeconds = 7 * 86400
	quotaCapSandboxEgressAllowed      = 1
	quotaCapMaxSandboxDiskMB          = environments.SandboxMaxDiskMBCap
)

// handleQuotaUpdate handles PATCH /v1/tenants/{tenantID}/quotas.
//
// Security model:
//   - Rate-limited by adminLimiter (5/IP/hour, 20/global/hour), separate
//     buckets from registration's regLimiter; the global bucket is charged
//     only after a successful admin-token check (requireAdmin).
//   - Requires the admin token (X-Admin-Token, or X-Bootstrap-Token carrying it); falls back to bootstrap tokens when no admin token is configured.
//   - Only updates fields that are present in the request body (partial update).
//   - Validates that all values are positive and within defined caps.
func (s *Server) handleQuotaUpdate(w http.ResponseWriter, r *http.Request) {
	// Admin-route limiter (separate from registration's) to prevent
	// brute-force of the admin/bootstrap token via this endpoint.
	if !adminLimiter.allowRequest(w, r) {
		return
	}

	if !s.requireAdmin(w, adminTokenFromRequest(r, r.Header.Get("X-Bootstrap-Token")), "quota update temporarily unavailable") {
		return
	}

	tenantID := chi.URLParam(r, "tenantID")
	if tenantID == "" {
		writeError(w, http.StatusBadRequest, "tenant ID is required")
		return
	}

	// Verify the tenant exists.
	var tenantExists int
	err := s.store.StateDB.QueryRow(
		`SELECT COUNT(*) FROM tenants WHERE id = ?`, tenantID,
	).Scan(&tenantExists)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if tenantExists == 0 {
		writeError(w, http.StatusNotFound, "tenant not found")
		return
	}

	var req QuotaUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}

	// Each field's JSON name is also its tenant_quotas column name (a constant,
	// never user input, so it is safe to build into the UPDATE).
	fields := []struct {
		column string
		value  *int
		min    int
		cap    int
	}{
		{"max_services", req.MaxServices, 1, quotaCapMaxServices},
		{"max_databases", req.MaxDatabases, 1, quotaCapMaxDatabases},
		{"max_builds_concurrent", req.MaxBuildsConcurrent, 1, quotaCapMaxBuildsConcurrent},
		{"max_env_vars_per_service", req.MaxEnvVarsPerService, 1, quotaCapMaxEnvVarsPerService},
		{"max_sandboxes", req.MaxSandboxes, 0, quotaCapMaxSandboxes},
		{"max_sandbox_ttl_seconds", req.MaxSandboxTTLSeconds, 60, quotaCapMaxSandboxTTLSeconds},
		{"max_sandbox_lifetime_seconds", req.MaxSandboxLifetimeSeconds, 60, quotaCapMaxSandboxLifetimeSeconds},
		{"sandbox_egress_allowed", req.SandboxEgressAllowed, 0, quotaCapSandboxEgressAllowed},
		{"max_sandbox_disk_mb", req.MaxSandboxDiskMB, 16, quotaCapMaxSandboxDiskMB},
	}

	// Validate: all provided values must be within [min, cap].
	// Build the partial UPDATE from the fields that are present.
	var setClauses []string
	var args []any
	for _, f := range fields {
		if f.value == nil {
			continue
		}
		if *f.value < f.min {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("%s must be at least %d", f.column, f.min))
			return
		}
		if *f.value > f.cap {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("%s must not exceed %d", f.column, f.cap))
			return
		}
		setClauses = append(setClauses, f.column+" = ?")
		args = append(args, *f.value)
	}

	if len(setClauses) > 0 {
		query := "UPDATE tenant_quotas SET " + strings.Join(setClauses, ", ") + " WHERE tenant_id = ?"
		args = append(args, tenantID)
		if _, err := s.store.StateDB.Exec(query, args...); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to update quotas")
			return
		}
	}

	// AUDIT log the quota change. Use helper to dereference pointers for clean output.
	ptrIntStr := func(p *int) string {
		if p == nil {
			return "<unchanged>"
		}
		return strconv.Itoa(*p)
	}
	log.Printf("AUDIT: action=tenant.quotas_updated tenant=%s max_services=%s max_databases=%s max_builds_concurrent=%s max_env_vars_per_service=%s max_sandboxes=%s max_sandbox_ttl_seconds=%s max_sandbox_lifetime_seconds=%s sandbox_egress_allowed=%s max_sandbox_disk_mb=%s",
		tenantID, ptrIntStr(req.MaxServices), ptrIntStr(req.MaxDatabases), ptrIntStr(req.MaxBuildsConcurrent), ptrIntStr(req.MaxEnvVarsPerService),
		ptrIntStr(req.MaxSandboxes), ptrIntStr(req.MaxSandboxTTLSeconds), ptrIntStr(req.MaxSandboxLifetimeSeconds), ptrIntStr(req.SandboxEgressAllowed),
		ptrIntStr(req.MaxSandboxDiskMB))

	// Read back the updated quotas and return them.
	var resp QuotaUpdateResponse
	resp.TenantID = tenantID
	err = s.store.StateDB.QueryRow(
		`SELECT max_services, max_databases, max_builds_concurrent, max_env_vars_per_service,
		        max_memory_mb, max_cpu_cores, max_disk_gb, api_rate_limit,
		        max_sandboxes, max_sandbox_ttl_seconds, max_sandbox_lifetime_seconds, sandbox_egress_allowed,
		        max_sandbox_disk_mb
		 FROM tenant_quotas WHERE tenant_id = ?`, tenantID,
	).Scan(&resp.MaxServices, &resp.MaxDatabases, &resp.MaxBuildsConcurrent, &resp.MaxEnvVarsPerService,
		&resp.MaxMemoryMB, &resp.MaxCPUCores, &resp.MaxDiskGB, &resp.APIRateLimit,
		&resp.MaxSandboxes, &resp.MaxSandboxTTLSeconds, &resp.MaxSandboxLifetimeSeconds, &resp.SandboxEgressAllowed,
		&resp.MaxSandboxDiskMB)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, resp)
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

// validateBootstrapToken checks whether the provided token matches any of the
// configured bootstrap tokens. All comparisons use HMAC-compare to prevent
// timing side-channels. This supports graceful token rotation: operators can
// set AH_BOOTSTRAP_TOKEN=new,old, deploy, then remove the old token.
//
// Important: this function always iterates over ALL tokens regardless of whether
// an early match is found. This prevents leaking which position in the list
// matched (or whether a match occurred at all) via timing analysis.
func validateBootstrapToken(provided string, tokens []string, masterKey []byte) bool {
	matched := false
	for _, tok := range tokens {
		if hmacEqual(provided, tok, masterKey) {
			matched = true
		}
	}
	return matched
}

// requireAdmin authorizes an operator-only request. When admin tokens are
// configured only they are accepted (a valid bootstrap token gets 403);
// otherwise it falls back to bootstrap tokens (deprecated, warned at startup).
// On success it charges the global rate-limit bucket. On failure it writes the
// response and returns false.
func (s *Server) requireAdmin(w http.ResponseWriter, provided, unavailableMsg string) bool {
	provided = strings.TrimSpace(provided)
	if len(s.adminTokens) > 0 {
		if !validateBootstrapToken(provided, s.adminTokens, s.masterKey) {
			if validateBootstrapToken(provided, s.bootstrapTokens, s.masterKey) {
				writeError(w, http.StatusForbidden, "this operation requires the admin token")
			} else {
				writeError(w, http.StatusUnauthorized, "missing or invalid admin token")
			}
			return false
		}
	} else {
		// Fail closed: reject if no bootstrap tokens are configured.
		if len(s.bootstrapTokens) == 0 {
			writeError(w, http.StatusServiceUnavailable, unavailableMsg)
			return false
		}
		if !validateBootstrapToken(provided, s.bootstrapTokens, s.masterKey) {
			writeError(w, http.StatusUnauthorized, "missing or invalid bootstrap token")
			return false
		}
	}
	adminLimiter.chargeGlobal()
	return true
}

// adminTokenFromRequest prefers the X-Admin-Token header and otherwise uses
// fallback (the legacy X-Bootstrap-Token header or bootstrap_token body field).
func adminTokenFromRequest(r *http.Request, fallback string) string {
	if v := r.Header.Get("X-Admin-Token"); v != "" {
		return v
	}
	return fallback
}
