package api

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/databases"
	"github.com/dennisonbertram/agentic-hosting/internal/middleware"
	"github.com/go-chi/chi/v5"
)

// requireDBManager is a guard that returns 503 if dbManager is nil.
func (s *Server) requireDBManager(w http.ResponseWriter) bool {
	if s.dbManager == nil {
		writeError(w, http.StatusServiceUnavailable, "database management is not available")
		return false
	}
	return true
}

func (s *Server) handleDatabaseCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireDBManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())

	var req databases.CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}

	db, err := s.dbManager.Create(r.Context(), tenantID, req)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, db)
}

func (s *Server) handleDatabaseList(w http.ResponseWriter, r *http.Request) {
	if !s.requireDBManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())

	limit, offset, err := parsePagination(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	dbs, err := s.dbManager.ListPaginated(r.Context(), tenantID, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list databases")
		return
	}
	if dbs == nil {
		dbs = []*databases.Database{}
	}
	writeJSON(w, http.StatusOK, dbs)
}

func (s *Server) handleDatabaseGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireDBManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	dbID := chi.URLParam(r, "dbID")

	db, err := s.dbManager.Get(r.Context(), tenantID, dbID)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, db)
}

func (s *Server) handleDatabaseConnectionString(w http.ResponseWriter, r *http.Request) {
	if !s.requireDBManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	dbID := chi.URLParam(r, "dbID")

	connStr, err := s.dbManager.GetConnectionString(r.Context(), tenantID, dbID)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	keyID := middleware.GetKeyID(r.Context())
	log.Printf("AUDIT: action=database.connection_string_accessed tenant=%s database=%s api_key=%s", tenantID, dbID, keyID)

	writeJSON(w, http.StatusOK, map[string]string{"connection_string": connStr})
}

func (s *Server) handleDatabaseDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireDBManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	dbID := chi.URLParam(r, "dbID")

	if err := s.dbManager.Delete(r.Context(), tenantID, dbID); err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
