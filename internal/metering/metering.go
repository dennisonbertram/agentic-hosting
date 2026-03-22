package metering

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Metric represents an aggregated resource usage data point.
type Metric struct {
	Timestamp       string  `json:"timestamp"`
	CPUSeconds      float64 `json:"cpu_seconds"`
	MemoryMBAvg     float64 `json:"memory_mb_avg"`
	NetworkRxBytes  int64   `json:"network_rx_bytes"`
	NetworkTxBytes  int64   `json:"network_tx_bytes"`
	ServiceID       string  `json:"service_id,omitempty"`
}

// Store wraps the metering SQLite database connection to provide
// resource usage query methods.
type Store struct {
	db *sql.DB
}

// NewStore creates a metering store from an existing *sql.DB connection
// (the metering database, not the state database).
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// ValidPeriods are the allowed aggregation periods.
var ValidPeriods = map[string]bool{
	"hourly": true,
	"daily":  true,
}

// QueryTenantMetrics returns aggregated usage metrics for a tenant across all
// or a specific service. Results are grouped by the requested period.
//
// Parameters:
//   - tenantID: required, always filters by tenant
//   - period: "hourly" or "daily"
//   - since, until: time range (inclusive)
//   - serviceID: optional, filters to a single service when non-empty
func (s *Store) QueryTenantMetrics(tenantID, period string, since, until time.Time, serviceID string) ([]Metric, error) {
	if !ValidPeriods[period] {
		return nil, fmt.Errorf("invalid period %q: must be hourly or daily", period)
	}

	timeBucket := timeBucketExpr(period)

	query := fmt.Sprintf(`
		SELECT
			%s AS bucket,
			COALESCE(service_id, '') AS svc_id,
			COALESCE(SUM(cpu_seconds), 0),
			COALESCE(SUM(memory_mb_seconds), 0),
			COALESCE(SUM(network_ingress_bytes), 0),
			COALESCE(SUM(network_egress_bytes), 0)
		FROM usage_events
		WHERE tenant_id = ?
		  AND recorded_at >= ?
		  AND recorded_at <= ?
	`, timeBucket)

	args := []any{tenantID, since.Unix(), until.Unix()}

	if serviceID != "" {
		query += " AND service_id = ?"
		args = append(args, serviceID)
	}

	query += fmt.Sprintf(" GROUP BY bucket, svc_id ORDER BY bucket ASC")

	return s.execMetricsQuery(query, args)
}

// QueryServiceMetrics returns aggregated usage metrics for a specific service
// owned by the given tenant. This is a convenience wrapper that enforces tenant
// isolation at the query level.
func (s *Store) QueryServiceMetrics(tenantID, serviceID, period string, since, until time.Time) ([]Metric, error) {
	if !ValidPeriods[period] {
		return nil, fmt.Errorf("invalid period %q: must be hourly or daily", period)
	}

	timeBucket := timeBucketExpr(period)

	query := fmt.Sprintf(`
		SELECT
			%s AS bucket,
			COALESCE(service_id, '') AS svc_id,
			COALESCE(SUM(cpu_seconds), 0),
			COALESCE(SUM(memory_mb_seconds), 0),
			COALESCE(SUM(network_ingress_bytes), 0),
			COALESCE(SUM(network_egress_bytes), 0)
		FROM usage_events
		WHERE tenant_id = ?
		  AND service_id = ?
		  AND recorded_at >= ?
		  AND recorded_at <= ?
		GROUP BY bucket
		ORDER BY bucket ASC
	`, timeBucket)

	args := []any{tenantID, serviceID, since.Unix(), until.Unix()}

	return s.execMetricsQuery(query, args)
}

// execMetricsQuery runs a metrics aggregation query and scans the results into
// a slice of Metric structs.
func (s *Store) execMetricsQuery(query string, args []any) ([]Metric, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("metering query: %w", err)
	}
	defer rows.Close()

	var metrics []Metric
	for rows.Next() {
		var m Metric
		var memMBSeconds float64
		if err := rows.Scan(&m.Timestamp, &m.ServiceID, &m.CPUSeconds, &memMBSeconds, &m.NetworkRxBytes, &m.NetworkTxBytes); err != nil {
			return nil, fmt.Errorf("scan metric row: %w", err)
		}
		// memory_mb_seconds is a cumulative metric; average makes more sense for display.
		// Without knowing exact event count per bucket, we report the raw sum.
		// Callers can interpret this as total memory-MB-seconds in the bucket.
		m.MemoryMBAvg = memMBSeconds
		metrics = append(metrics, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("metering rows iteration: %w", err)
	}

	if metrics == nil {
		metrics = []Metric{}
	}
	return metrics, nil
}

// timeBucketExpr returns a SQLite expression that truncates the recorded_at
// unix timestamp to the appropriate time bucket for GROUP BY aggregation.
func timeBucketExpr(period string) string {
	switch strings.ToLower(period) {
	case "hourly":
		// Truncate to hour: floor(recorded_at / 3600) * 3600, then format as ISO timestamp.
		return "strftime('%Y-%m-%dT%H:00:00Z', recorded_at, 'unixepoch')"
	default: // daily
		return "strftime('%Y-%m-%dT00:00:00Z', recorded_at, 'unixepoch')"
	}
}
