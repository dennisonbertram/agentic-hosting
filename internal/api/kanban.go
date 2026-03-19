package api

import (
	"encoding/json"
	"net/http"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/kanban"
	"github.com/dennisonbertram/agentic-hosting/internal/middleware"
	"github.com/go-chi/chi/v5"
)

// requireKanbanManager is a guard that returns 503 if kanbanManager is nil.
func (s *Server) requireKanbanManager(w http.ResponseWriter) bool {
	if s.kanbanManager == nil {
		writeError(w, http.StatusServiceUnavailable, "kanban management is not available")
		return false
	}
	return true
}

func (s *Server) handleKanbanCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireKanbanManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())

	var req kanban.CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}

	inst, err := s.kanbanManager.Create(r.Context(), tenantID, req)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, inst)
}

func (s *Server) handleKanbanGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireKanbanManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())

	// If kanbanID is provided, get by ID; otherwise get by tenant
	kanbanID := chi.URLParam(r, "kanbanID")
	if kanbanID != "" {
		inst, err := s.kanbanManager.Get(r.Context(), tenantID, kanbanID)
		if err != nil {
			apierr.WriteAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, inst)
		return
	}

	inst, err := s.kanbanManager.GetByTenant(r.Context(), tenantID)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, inst)
}

func (s *Server) handleKanbanDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireKanbanManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	kanbanID := chi.URLParam(r, "kanbanID")

	if err := s.kanbanManager.Delete(r.Context(), tenantID, kanbanID); err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleKanbanAPIToken(w http.ResponseWriter, r *http.Request) {
	if !s.requireKanbanManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	kanbanID := chi.URLParam(r, "kanbanID")

	token, err := s.kanbanManager.GetAPIToken(r.Context(), tenantID, kanbanID)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"api_token": token})
}
