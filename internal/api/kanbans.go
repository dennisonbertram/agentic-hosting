package api

import (
	"log"
	"net/http"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/middleware"
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

	kb, err := s.kanbanManager.Create(r.Context(), tenantID)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, kb)
}

func (s *Server) handleKanbanGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireKanbanManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())

	kb, err := s.kanbanManager.Get(r.Context(), tenantID)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, kb)
}

func (s *Server) handleKanbanAdminToken(w http.ResponseWriter, r *http.Request) {
	if !s.requireKanbanManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())

	token, err := s.kanbanManager.GetAdminToken(r.Context(), tenantID)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	keyID := middleware.GetKeyID(r.Context())
	log.Printf("AUDIT: action=kanban.admin_token_accessed tenant=%s api_key=%s", tenantID, keyID)

	writeJSON(w, http.StatusOK, map[string]string{"admin_token": token})
}

func (s *Server) handleKanbanDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireKanbanManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())

	if err := s.kanbanManager.Delete(r.Context(), tenantID); err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
