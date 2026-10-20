package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/environments"
	"github.com/dennisonbertram/agentic-hosting/internal/middleware"
	"github.com/go-chi/chi/v5"
)

// Per-tenant sandbox create limit: every create and delete costs several
// Docker calls, so creates are limited to about 20 a minute per tenant.
const (
	sandboxCreateRPS   = 20.0 / 60.0
	sandboxCreateBurst = 10
)

// sandboxCreateRateLimit applies the per-tenant create limiter. The tenant's
// api_rate_limit override is dropped from the context for this check so the
// create limit cannot be raised by a generous general API limit.
func (s *Server) sandboxCreateRateLimit(next http.Handler) http.Handler {
	limited := s.sandboxCreateLimiter.Middleware(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limited.ServeHTTP(w, r.WithContext(middleware.ContextWithRateLimit(r.Context(), 0)))
	})
}

// POST /v1/sandboxes
func (s *Server) handleSandboxCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnvironmentManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())

	var req environments.SandboxCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}
	sbx, err := s.envManager.CreateSandbox(r.Context(), tenantID, req)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sbx)
}

// GET /v1/sandboxes?limit&offset&status
func (s *Server) handleSandboxList(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnvironmentManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	limit, offset, err := parsePagination(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	status := r.URL.Query().Get("status")
	switch status {
	case "", "creating", "running", "stopped", "failed", "deleting":
	default:
		writeError(w, http.StatusBadRequest, "status must be one of creating, running, stopped, failed, deleting")
		return
	}
	list, err := s.envManager.ListSandboxes(r.Context(), tenantID, limit, offset, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list sandboxes")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// GET /v1/sandboxes/{sandboxID}
func (s *Server) handleSandboxGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnvironmentManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	sbx, err := s.envManager.GetSandbox(r.Context(), tenantID, chi.URLParam(r, "sandboxID"))
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sbx)
}

// DELETE /v1/sandboxes/{sandboxID}
func (s *Server) handleSandboxDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnvironmentManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	if err := s.envManager.DeleteSandbox(r.Context(), tenantID, chi.URLParam(r, "sandboxID")); err != nil {
		apierr.WriteAPIError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /v1/sandboxes/{sandboxID}/exec
func (s *Server) handleSandboxExec(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnvironmentManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())

	var req environments.ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}
	// Same deadline handling as environment exec: the response may take up to
	// the exec timeout plus a margin.
	_ = http.NewResponseController(w).SetWriteDeadline(environmentExecResponseDeadline(time.Now(), req.Timeout))

	resp, err := s.envManager.ExecSandbox(r.Context(), tenantID, chi.URLParam(r, "sandboxID"), req)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// GET /v1/sandboxes/{sandboxID}/logs?tail=100&follow=false
func (s *Server) handleSandboxLogs(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnvironmentManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	follow := r.URL.Query().Get("follow") == "true"
	tail := 0
	if t := r.URL.Query().Get("tail"); t != "" {
		v, err := strconv.Atoi(t)
		if err != nil || v <= 0 {
			writeError(w, http.StatusBadRequest, "tail must be a positive integer")
			return
		}
		tail = v
	}
	reader, err := s.envManager.SandboxLogs(r.Context(), tenantID, chi.URLParam(r, "sandboxID"), follow, environments.ClampLogTail(tail))
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	if f, ok := w.(http.Flusher); ok {
		buf := make([]byte, 4096)
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				// Stop once the client is gone or its write deadline passed.
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				f.Flush()
			}
			if err != nil {
				return
			}
		}
	}
	_, _ = io.Copy(w, reader)
}

// POST /v1/sandboxes/{sandboxID}/extend {"ttl_seconds": N}
func (s *Server) handleSandboxExtend(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnvironmentManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	var body struct {
		TTLSeconds int `json:"ttl_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeError(w, err)
		return
	}
	if body.TTLSeconds <= 0 {
		writeError(w, http.StatusBadRequest, "ttl_seconds must be positive")
		return
	}
	sbx, err := s.envManager.ExtendSandbox(r.Context(), tenantID, chi.URLParam(r, "sandboxID"), body.TTLSeconds)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sbx)
}
