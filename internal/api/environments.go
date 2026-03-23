package api

import (
	"encoding/json"
	"net/http"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/environments"
	"github.com/dennisonbertram/agentic-hosting/internal/middleware"
	"github.com/go-chi/chi/v5"
)

// requireEnvironmentManager is a guard that returns 503 if envManager is nil.
func (s *Server) requireEnvironmentManager(w http.ResponseWriter) bool {
	if s.envManager == nil {
		writeError(w, http.StatusServiceUnavailable, "environment management is not available")
		return false
	}
	return true
}

func (s *Server) handleEnvironmentCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnvironmentManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())

	var req environments.CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}

	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	env, err := s.envManager.Create(r.Context(), tenantID, req)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, env)
}

func (s *Server) handleEnvironmentList(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnvironmentManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())

	limit, offset, err := parsePagination(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	envs, err := s.envManager.List(r.Context(), tenantID, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list environments")
		return
	}
	if envs == nil {
		envs = []*environments.Environment{}
	}
	writeJSON(w, http.StatusOK, envs)
}

func (s *Server) handleEnvironmentGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnvironmentManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	envID := chi.URLParam(r, "envID")

	env, err := s.envManager.Get(r.Context(), tenantID, envID)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, env)
}

func (s *Server) handleEnvironmentDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnvironmentManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	envID := chi.URLParam(r, "envID")

	if err := s.envManager.Delete(r.Context(), tenantID, envID); err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleEnvironmentStart(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnvironmentManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	envID := chi.URLParam(r, "envID")

	if err := s.envManager.Start(r.Context(), tenantID, envID); err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "running"})
}

func (s *Server) handleEnvironmentStop(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnvironmentManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	envID := chi.URLParam(r, "envID")

	if err := s.envManager.Stop(r.Context(), tenantID, envID); err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

func (s *Server) handleEnvironmentExec(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnvironmentManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	envID := chi.URLParam(r, "envID")

	var req environments.ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}

	resp, err := s.envManager.Exec(r.Context(), tenantID, envID, req)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleEnvironmentLeaseExtend(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnvironmentManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	envID := chi.URLParam(r, "envID")

	var body struct {
		DurationSeconds int `json:"duration_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeError(w, err)
		return
	}

	if body.DurationSeconds <= 0 {
		writeError(w, http.StatusBadRequest, "duration_seconds must be positive")
		return
	}

	if err := s.envManager.ExtendLease(r.Context(), tenantID, envID, body.DurationSeconds); err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	// Re-fetch to get the updated lease_expires_at
	env, err := s.envManager.Get(r.Context(), tenantID, envID)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, env)
}

func (s *Server) handleEnvironmentTemplateList(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnvironmentManager(w) {
		return
	}

	templates, err := s.envManager.ListTemplates(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list templates")
		return
	}
	if templates == nil {
		templates = []*environments.Template{}
	}
	writeJSON(w, http.StatusOK, templates)
}

func (s *Server) handleEnvironmentTemplateGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnvironmentManager(w) {
		return
	}
	templateID := chi.URLParam(r, "templateID")

	tmpl, err := s.envManager.GetTemplate(r.Context(), templateID)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, tmpl)
}
