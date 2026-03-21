package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/middleware"
	"github.com/dennisonbertram/agentic-hosting/internal/services"
	"github.com/go-chi/chi/v5"
)

// requireSvcManager is a guard that returns 503 if svcManager is nil.
// All service handlers must call this before proceeding.
func (s *Server) requireSvcManager(w http.ResponseWriter) bool {
	if s.svcManager == nil {
		writeError(w, http.StatusServiceUnavailable, "service management is not available (Docker not configured)")
		return false
	}
	return true
}

func (s *Server) handleServiceCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireSvcManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())

	// Support creating a service from a snapshot.
	if fromSnapshot := r.URL.Query().Get("from_snapshot"); fromSnapshot != "" {
		s.handleServiceCreateFromSnapshot(w, r, tenantID, fromSnapshot)
		return
	}

	var req services.CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}

	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if len(req.Name) > 128 {
		writeError(w, http.StatusBadRequest, "name must be at most 128 characters")
		return
	}

	// Validate image format and registry allowlist
	if err := services.ValidateImage(req.Image); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Validate env vars if provided inline
	if len(req.Env) > 0 {
		if err := services.ValidateEnvVars(req.Env); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	svc, err := s.svcManager.Create(r.Context(), tenantID, req)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	// Deploy asynchronously — return immediately with status "deploying".
	// Use a bounded context (10 min) to prevent goroutine leaks from stuck deploys.
	go func(tid, sid string) {
		deployCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := s.svcManager.Deploy(deployCtx, tid, sid); err != nil {
			log.Printf("deploy failed for service %s: %v", sid, err)
			return
		}
	}(tenantID, svc.ID)

	svc.Status = "deploying"
	writeJSON(w, http.StatusCreated, svc)
}

// handleServiceCreateFromSnapshot creates a new service from an existing snapshot.
// The snapshot's image, env vars, and port are used as the template for the new service.
func (s *Server) handleServiceCreateFromSnapshot(w http.ResponseWriter, r *http.Request, tenantID, snapshotID string) {
	if !s.requireSnapshotManager(w) {
		return
	}

	// Decode request body for the new service name (and optional overrides).
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeError(w, err)
		return
	}
	if body.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if len(body.Name) > 128 {
		writeError(w, http.StatusBadRequest, "name must be at most 128 characters")
		return
	}

	// Load the snapshot.
	snap, err := s.snapshotManager.Get(r.Context(), tenantID, snapshotID)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	// Restore env vars from the snapshot (tenant-scoped for isolation).
	envVars, err := s.snapshotManager.RestoreEnvVars(r.Context(), tenantID, snapshotID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to restore snapshot env vars")
		log.Printf("snapshot restore env vars error: %v", err)
		return
	}

	// Validate snapshot data before creating (defense-in-depth against corrupted snapshots).
	if err := services.ValidateImage(snap.ImageRef); err != nil {
		writeError(w, http.StatusBadRequest, "snapshot contains invalid image: "+err.Error())
		return
	}
	if len(envVars) > 0 {
		if err := services.ValidateEnvVars(envVars); err != nil {
			writeError(w, http.StatusBadRequest, "snapshot contains invalid env vars: "+err.Error())
			return
		}
	}
	if snap.Port < 1 || snap.Port > 65535 {
		writeError(w, http.StatusBadRequest, "snapshot contains invalid port")
		return
	}

	// Create the service using the snapshot's image and port.
	req := services.CreateRequest{
		Name:  body.Name,
		Image: snap.ImageRef,
		Port:  snap.Port,
		Env:   envVars,
	}

	svc, err := s.svcManager.Create(r.Context(), tenantID, req)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	// Deploy asynchronously. The image is already in the local registry so this
	// should be nearly instant (<5s) since no Nixpacks build or remote pull is needed.
	go func(tid, sid string) {
		deployCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := s.svcManager.Deploy(deployCtx, tid, sid); err != nil {
			log.Printf("deploy from snapshot failed for service %s: %v", sid, err)
			return
		}
	}(tenantID, svc.ID)

	svc.Status = "deploying"
	writeJSON(w, http.StatusCreated, svc)
}

func (s *Server) handleServiceList(w http.ResponseWriter, r *http.Request) {
	if !s.requireSvcManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())

	limit, offset, err := parsePagination(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	svcs, err := s.svcManager.ListPaginated(r.Context(), tenantID, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list services")
		return
	}
	if svcs == nil {
		svcs = []*services.Service{}
	}
	writeJSON(w, http.StatusOK, svcs)
}

func (s *Server) handleServiceGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireSvcManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	svc, err := s.svcManager.Get(r.Context(), tenantID, serviceID)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, svc)
}

func (s *Server) handleServiceDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireSvcManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	if err := s.svcManager.Delete(r.Context(), tenantID, serviceID); err != nil {
		apierr.WriteAPIError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleServiceStart(w http.ResponseWriter, r *http.Request) {
	if !s.requireSvcManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	if err := s.svcManager.Start(r.Context(), tenantID, serviceID); err != nil {
		apierr.WriteAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "running"})
}

func (s *Server) handleServiceStop(w http.ResponseWriter, r *http.Request) {
	if !s.requireSvcManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	if err := s.svcManager.Stop(r.Context(), tenantID, serviceID); err != nil {
		apierr.WriteAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

func (s *Server) handleServiceRestart(w http.ResponseWriter, r *http.Request) {
	if !s.requireSvcManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	if err := s.svcManager.Restart(r.Context(), tenantID, serviceID); err != nil {
		apierr.WriteAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "running"})
}

func (s *Server) handleServiceLogs(w http.ResponseWriter, r *http.Request) {
	if !s.requireSvcManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	follow := r.URL.Query().Get("follow") == "true"
	tail := 100
	if t := r.URL.Query().Get("tail"); t != "" {
		if v, err := strconv.Atoi(t); err == nil && v > 0 && v <= 10000 {
			tail = v
		}
	}

	reader, err := s.svcManager.Logs(r.Context(), tenantID, serviceID, follow, tail)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if follow {
		w.Header().Set("Transfer-Encoding", "chunked")
	}
	w.WriteHeader(http.StatusOK)

	if f, ok := w.(http.Flusher); ok {
		buf := make([]byte, 4096)
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
				f.Flush()
			}
			if err != nil {
				break
			}
		}
	} else {
		io.Copy(w, reader)
	}
}

func (s *Server) handleServiceEnvGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireSvcManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")
	reveal := r.URL.Query().Get("reveal") == "true"

	vars, err := s.svcManager.GetEnv(r.Context(), tenantID, serviceID, reveal)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, vars)
}

func (s *Server) handleServiceEnvSet(w http.ResponseWriter, r *http.Request) {
	if !s.requireSvcManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	var vars map[string]string
	if err := json.NewDecoder(r.Body).Decode(&vars); err != nil {
		writeDecodeError(w, err)
		return
	}

	if len(vars) == 0 {
		writeError(w, http.StatusBadRequest, "no environment variables provided")
		return
	}
	if len(vars) > 100 {
		writeError(w, http.StatusBadRequest, "too many environment variables (max 100)")
		return
	}

	// Validate env var keys and values
	if err := services.ValidateEnvVars(vars); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := s.svcManager.SetEnv(r.Context(), tenantID, serviceID, vars); err != nil {
		apierr.WriteAPIError(w, err)
		return
	}
	log.Printf("AUDIT: tenant=%s set env vars for service=%s keys=%v", tenantID, serviceID, envKeys(vars))
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated", "note": "restart will recreate the container with the new env vars"})
}

func (s *Server) handleServiceEnvDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireSvcManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")
	key := chi.URLParam(r, "key")

	if err := s.svcManager.DeleteEnv(r.Context(), tenantID, serviceID, key); err != nil {
		apierr.WriteAPIError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// envKeys extracts just the keys from a map for audit logging (no values).
func envKeys(vars map[string]string) []string {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	return keys
}

func (s *Server) handleServiceReset(w http.ResponseWriter, r *http.Request) {
	if !s.requireSvcManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	if err := s.svcManager.ResetCircuitBreaker(r.Context(), tenantID, serviceID); err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "circuit breaker reset"})
}

// handleServiceRedeploy is a semantically explicit alias for restart.
// It stops the existing container, loads the current env vars from the database,
// and starts a new container with the current image and env. No new build is triggered.
// Returns the updated service object so callers can inspect the new status.
func (s *Server) handleServiceRedeploy(w http.ResponseWriter, r *http.Request) {
	if !s.requireSvcManager(w) {
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	if err := s.svcManager.Restart(r.Context(), tenantID, serviceID); err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	svc, err := s.svcManager.Get(r.Context(), tenantID, serviceID)
	if err != nil {
		apierr.WriteAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, svc)
}

// handleServiceDeployments returns the paginated deployment history for a service
// from the dedicated deployments table.
func (s *Server) handleServiceDeployments(w http.ResponseWriter, r *http.Request) {
	if !s.requireSvcManager(w) {
		return
	}
	if s.deploymentStore == nil {
		writeError(w, http.StatusServiceUnavailable, "deployment history is not available")
		return
	}
	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	// Verify the service exists and belongs to the tenant.
	if _, err := s.svcManager.Get(r.Context(), tenantID, serviceID); err != nil {
		apierr.WriteAPIError(w, err)
		return
	}

	limit, offset, err := parsePagination(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	records, err := s.deploymentStore.ListByService(r.Context(), tenantID, serviceID, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list deployments")
		return
	}
	writeJSON(w, http.StatusOK, records)
}
