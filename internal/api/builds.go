package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/builds"
	"github.com/dennisonbertram/agentic-hosting/internal/middleware"
	"github.com/go-chi/chi/v5"
)

func (s *Server) requireBuildManager(w http.ResponseWriter) bool {
	if s.buildManager == nil {
		writeError(w, http.StatusServiceUnavailable, "build system is not available")
		return false
	}
	return true
}

func (s *Server) handleBuildCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireBuildManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	var req builds.StartBuildRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}

	build, err := s.buildManager.StartBuild(r.Context(), tenantID, serviceID, req)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{
		"build_id": build.ID,
		"status":   build.Status,
		"image":    build.Image,
	})
}

func (s *Server) handleBuildListAll(w http.ResponseWriter, r *http.Request) {
	if !s.requireBuildManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())

	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 200 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 200")
			return
		}
		limit = value
	}

	result, err := s.buildManager.ListTenantBuilds(r.Context(), tenantID, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list builds")
		return
	}
	if result == nil {
		result = []*builds.Build{}
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleBuildList(w http.ResponseWriter, r *http.Request) {
	if !s.requireBuildManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	result, err := s.buildManager.ListBuilds(r.Context(), tenantID, serviceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list builds")
		return
	}
	if result == nil {
		result = []*builds.Build{}
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleBuildGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireBuildManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	buildID := chi.URLParam(r, "buildID")

	build, err := s.buildManager.GetBuild(r.Context(), tenantID, buildID)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, build)
}

func (s *Server) handleBuildLogs(w http.ResponseWriter, r *http.Request) {
	if !s.requireBuildManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	buildID := chi.URLParam(r, "buildID")

	follow := r.URL.Query().Get("follow") == "true"

	if follow {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Transfer-Encoding", "chunked")
		w.WriteHeader(http.StatusOK)

		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}

		if err := s.buildManager.StreamBuildLogs(r.Context(), tenantID, buildID, w); err != nil {
			// Can't write error after headers sent
			return
		}
		return
	}

	logs, err := s.buildManager.GetBuildLogs(r.Context(), tenantID, buildID)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(logs))
}

func (s *Server) handleBuildCancel(w http.ResponseWriter, r *http.Request) {
	if !s.requireBuildManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	buildID := chi.URLParam(r, "buildID")

	if err := s.buildManager.CancelBuild(r.Context(), tenantID, buildID); err != nil {
		apierr.WriteAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}
