package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/middleware"
	"github.com/dennisonbertram/agentic-hosting/internal/snapshots"
	"github.com/go-chi/chi/v5"
)

// requireSnapshotManager is a guard that returns 503 if snapshotManager is nil.
// All snapshot handlers must call this before proceeding.
func (s *Server) requireSnapshotManager(w http.ResponseWriter) bool {
	if s.snapshotManager == nil {
		writeError(w, http.StatusServiceUnavailable, "snapshot management is not available")
		return false
	}
	return true
}

func (s *Server) handleSnapshotCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireSnapshotManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	var req snapshots.CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}

	snap, err := s.snapshotManager.Create(r.Context(), tenantID, serviceID, req)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, snap)
}

func (s *Server) handleSnapshotList(w http.ResponseWriter, r *http.Request) {
	if !s.requireSnapshotManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())

	limit, offset, err := parsePagination(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Parse optional filter query parameters.
	var filter snapshots.ListFilter
	filter.ServiceID = r.URL.Query().Get("service_id")
	filter.Name = r.URL.Query().Get("name")
	if raw := r.URL.Query().Get("since"); raw != "" {
		since, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || since < 0 {
			writeError(w, http.StatusBadRequest, "since must be a non-negative unix timestamp")
			return
		}
		filter.Since = since
	}

	snaps, err := s.snapshotManager.List(r.Context(), tenantID, limit, offset, &filter)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, snaps)
}

func (s *Server) handleSnapshotGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireSnapshotManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	snapshotID := chi.URLParam(r, "snapshotID")

	snap, err := s.snapshotManager.Get(r.Context(), tenantID, snapshotID)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleSnapshotDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireSnapshotManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	snapshotID := chi.URLParam(r, "snapshotID")

	if err := s.snapshotManager.Delete(r.Context(), tenantID, snapshotID); err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
