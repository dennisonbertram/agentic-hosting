package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/metering"
	"github.com/dennisonbertram/agentic-hosting/internal/middleware"
	"github.com/go-chi/chi/v5"
)

// metricsQuery holds the parsed period/since/until query parameters shared by
// the metrics endpoints.
type metricsQuery struct {
	period       string
	since, until time.Time
}

// parseMetricsQuery reads period ("hourly" or "daily", default "daily"),
// since (RFC3339, default 24h ago) and until (RFC3339, default now). The
// error message is safe to return to the client as a 400.
func parseMetricsQuery(r *http.Request) (metricsQuery, error) {
	q := r.URL.Query()
	now := time.Now().UTC()
	mq := metricsQuery{period: q.Get("period"), since: now.Add(-24 * time.Hour), until: now}

	if mq.period == "" {
		mq.period = "daily"
	}
	if !metering.ValidPeriods[mq.period] {
		return mq, errors.New("period must be 'hourly' or 'daily'")
	}
	if raw := q.Get("since"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return mq, errors.New("since must be a valid RFC3339 timestamp")
		}
		mq.since = parsed.UTC()
	}
	if raw := q.Get("until"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return mq, errors.New("until must be a valid RFC3339 timestamp")
		}
		mq.until = parsed.UTC()
	}
	if mq.since.After(mq.until) {
		return mq, errors.New("since must be before until")
	}
	return mq, nil
}

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

	mq, err := parseMetricsQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	serviceID := r.URL.Query().Get("service_id")

	metrics, err := s.meteringStore.QueryTenantMetrics(tenantID, mq.period, mq.since, mq.until, serviceID)
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
	if err != nil {
		// A DB failure is not "service not found"; report it as such.
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if count == 0 {
		writeError(w, http.StatusNotFound, "service not found")
		return
	}

	mq, err := parseMetricsQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	metrics, err := s.meteringStore.QueryServiceMetrics(tenantID, serviceID, mq.period, mq.since, mq.until)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to query metrics")
		return
	}

	writeJSON(w, http.StatusOK, metrics)
}
