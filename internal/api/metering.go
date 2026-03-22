package api

import (
	"net/http"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/metering"
	"github.com/dennisonbertram/agentic-hosting/internal/middleware"
	"github.com/go-chi/chi/v5"
)

// handleTenantMetrics handles GET /v1/tenant/usage/metrics.
// Returns aggregated resource usage metrics for the authenticated tenant.
//
// Query parameters:
//   - period:     "hourly" or "daily" (default: "daily")
//   - since:      RFC3339 timestamp (default: 24h ago)
//   - until:      RFC3339 timestamp (default: now)
//   - service_id: optional, filter to a specific service
func (s *Server) handleTenantMetrics(w http.ResponseWriter, r *http.Request) {
	if s.meteringStore == nil {
		writeError(w, http.StatusServiceUnavailable, "metering not available")
		return
	}

	tenantID := middleware.GetTenantID(r.Context())
	q := r.URL.Query()

	period := q.Get("period")
	if period == "" {
		period = "daily"
	}
	if !metering.ValidPeriods[period] {
		writeError(w, http.StatusBadRequest, "period must be 'hourly' or 'daily'")
		return
	}

	now := time.Now().UTC()
	since := now.Add(-24 * time.Hour)
	until := now

	if raw := q.Get("since"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "since must be a valid RFC3339 timestamp")
			return
		}
		since = parsed.UTC()
	}

	if raw := q.Get("until"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "until must be a valid RFC3339 timestamp")
			return
		}
		until = parsed.UTC()
	}

	if since.After(until) {
		writeError(w, http.StatusBadRequest, "since must be before until")
		return
	}

	serviceID := q.Get("service_id")

	metrics, err := s.meteringStore.QueryTenantMetrics(tenantID, period, since, until, serviceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to query metrics")
		return
	}

	writeJSON(w, http.StatusOK, metrics)
}

// handleServiceMetrics handles GET /v1/services/{serviceID}/metrics.
// Returns aggregated resource usage metrics for a specific service.
//
// Query parameters:
//   - period: "hourly" or "daily" (default: "daily")
//   - since:  RFC3339 timestamp (default: 24h ago)
//   - until:  RFC3339 timestamp (default: now)
func (s *Server) handleServiceMetrics(w http.ResponseWriter, r *http.Request) {
	if s.meteringStore == nil {
		writeError(w, http.StatusServiceUnavailable, "metering not available")
		return
	}

	tenantID := middleware.GetTenantID(r.Context())
	serviceID := chi.URLParam(r, "serviceID")

	// Verify the service belongs to this tenant.
	var count int
	err := s.store.StateDB.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM services WHERE id = ? AND tenant_id = ?`,
		serviceID, tenantID,
	).Scan(&count)
	if err != nil || count == 0 {
		writeError(w, http.StatusNotFound, "service not found")
		return
	}

	q := r.URL.Query()

	period := q.Get("period")
	if period == "" {
		period = "daily"
	}
	if !metering.ValidPeriods[period] {
		writeError(w, http.StatusBadRequest, "period must be 'hourly' or 'daily'")
		return
	}

	now := time.Now().UTC()
	since := now.Add(-24 * time.Hour)
	until := now

	if raw := q.Get("since"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "since must be a valid RFC3339 timestamp")
			return
		}
		since = parsed.UTC()
	}

	if raw := q.Get("until"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "until must be a valid RFC3339 timestamp")
			return
		}
		until = parsed.UTC()
	}

	if since.After(until) {
		writeError(w, http.StatusBadRequest, "since must be before until")
		return
	}

	metrics, err := s.meteringStore.QueryServiceMetrics(tenantID, serviceID, period, since, until)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to query metrics")
		return
	}

	writeJSON(w, http.StatusOK, metrics)
}
