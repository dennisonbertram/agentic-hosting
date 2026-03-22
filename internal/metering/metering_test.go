package metering

import (
	"database/sql"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testDBCounter atomic.Int64

func newMeteringDB(t *testing.T) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("metering_test_%d", testDBCounter.Add(1))
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared&_foreign_keys=on&_busy_timeout=5000", name)
	sqlDB, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open metering db: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.ApplyMeteringMigrations(sqlDB); err != nil {
		t.Fatalf("apply metering migrations: %v", err)
	}
	return sqlDB
}

func insertEvent(t *testing.T, sqlDB *sql.DB, id, tenantID, serviceID string, cpu float64, memMBSec float64, rxBytes, txBytes int64, recordedAt int64) {
	t.Helper()
	_, err := sqlDB.Exec(
		`INSERT INTO usage_events (id, tenant_id, service_id, event_type, cpu_seconds, memory_mb_seconds, network_ingress_bytes, network_egress_bytes, recorded_at)
		 VALUES (?, ?, ?, 'sample', ?, ?, ?, ?, ?)`,
		id, tenantID, serviceID, cpu, memMBSec, rxBytes, txBytes, recordedAt,
	)
	require.NoError(t, err)
}

func TestQueryTenantMetrics_EmptyDatabase(t *testing.T) {
	sqlDB := newMeteringDB(t)
	store := NewStore(sqlDB)

	now := time.Now().UTC()
	metrics, err := store.QueryTenantMetrics("tenant-1", "daily", now.Add(-24*time.Hour), now, "")
	require.NoError(t, err)
	assert.Empty(t, metrics, "empty database should return empty slice")
	assert.NotNil(t, metrics, "should return non-nil empty slice")
}

func TestQueryTenantMetrics_InvalidPeriod(t *testing.T) {
	sqlDB := newMeteringDB(t)
	store := NewStore(sqlDB)

	now := time.Now().UTC()
	_, err := store.QueryTenantMetrics("tenant-1", "weekly", now.Add(-24*time.Hour), now, "")
	assert.Error(t, err, "invalid period should return error")
	assert.Contains(t, err.Error(), "invalid period")
}

func TestQueryTenantMetrics_DailyAggregation(t *testing.T) {
	sqlDB := newMeteringDB(t)
	store := NewStore(sqlDB)

	// Insert events at two different days
	day1 := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC).Unix()
	day2 := time.Date(2026, 3, 21, 14, 0, 0, 0, time.UTC).Unix()

	insertEvent(t, sqlDB, "e1", "tenant-1", "svc-1", 10.5, 100.0, 1000, 2000, day1)
	insertEvent(t, sqlDB, "e2", "tenant-1", "svc-1", 5.5, 50.0, 500, 1000, day1)
	insertEvent(t, sqlDB, "e3", "tenant-1", "svc-1", 20.0, 200.0, 3000, 4000, day2)

	since := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 3, 22, 0, 0, 0, 0, time.UTC)

	metrics, err := store.QueryTenantMetrics("tenant-1", "daily", since, until, "")
	require.NoError(t, err)
	require.Len(t, metrics, 2, "should have two daily buckets")

	// Day 1: aggregated
	assert.Equal(t, "2026-03-20T00:00:00Z", metrics[0].Timestamp)
	assert.InDelta(t, 16.0, metrics[0].CPUSeconds, 0.01, "day 1 CPU should sum to 16.0")
	assert.InDelta(t, 150.0, metrics[0].MemoryMBAvg, 0.01)
	assert.Equal(t, int64(1500), metrics[0].NetworkRxBytes)
	assert.Equal(t, int64(3000), metrics[0].NetworkTxBytes)

	// Day 2
	assert.Equal(t, "2026-03-21T00:00:00Z", metrics[1].Timestamp)
	assert.InDelta(t, 20.0, metrics[1].CPUSeconds, 0.01)
}

func TestQueryTenantMetrics_HourlyAggregation(t *testing.T) {
	sqlDB := newMeteringDB(t)
	store := NewStore(sqlDB)

	// Two events in the same hour, one in a different hour
	hour1 := time.Date(2026, 3, 20, 10, 15, 0, 0, time.UTC).Unix()
	hour1b := time.Date(2026, 3, 20, 10, 45, 0, 0, time.UTC).Unix()
	hour2 := time.Date(2026, 3, 20, 11, 30, 0, 0, time.UTC).Unix()

	insertEvent(t, sqlDB, "e1", "tenant-1", "svc-1", 5.0, 50.0, 100, 200, hour1)
	insertEvent(t, sqlDB, "e2", "tenant-1", "svc-1", 3.0, 30.0, 50, 100, hour1b)
	insertEvent(t, sqlDB, "e3", "tenant-1", "svc-1", 10.0, 100.0, 500, 600, hour2)

	since := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
	until := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)

	metrics, err := store.QueryTenantMetrics("tenant-1", "hourly", since, until, "")
	require.NoError(t, err)
	require.Len(t, metrics, 2, "should have two hourly buckets")

	assert.Equal(t, "2026-03-20T10:00:00Z", metrics[0].Timestamp)
	assert.InDelta(t, 8.0, metrics[0].CPUSeconds, 0.01, "hour 10 CPU should sum to 8.0")

	assert.Equal(t, "2026-03-20T11:00:00Z", metrics[1].Timestamp)
	assert.InDelta(t, 10.0, metrics[1].CPUSeconds, 0.01)
}

func TestQueryTenantMetrics_TenantIsolation(t *testing.T) {
	sqlDB := newMeteringDB(t)
	store := NewStore(sqlDB)

	ts := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC).Unix()
	insertEvent(t, sqlDB, "e1", "tenant-1", "svc-1", 10.0, 100.0, 1000, 2000, ts)
	insertEvent(t, sqlDB, "e2", "tenant-2", "svc-2", 20.0, 200.0, 3000, 4000, ts)

	since := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 3, 21, 0, 0, 0, 0, time.UTC)

	metrics, err := store.QueryTenantMetrics("tenant-1", "daily", since, until, "")
	require.NoError(t, err)
	require.Len(t, metrics, 1, "should only see tenant-1's data")
	assert.InDelta(t, 10.0, metrics[0].CPUSeconds, 0.01)
}

func TestQueryTenantMetrics_ServiceIDFilter(t *testing.T) {
	sqlDB := newMeteringDB(t)
	store := NewStore(sqlDB)

	ts := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC).Unix()
	insertEvent(t, sqlDB, "e1", "tenant-1", "svc-1", 10.0, 100.0, 1000, 2000, ts)
	insertEvent(t, sqlDB, "e2", "tenant-1", "svc-2", 20.0, 200.0, 3000, 4000, ts)

	since := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 3, 21, 0, 0, 0, 0, time.UTC)

	metrics, err := store.QueryTenantMetrics("tenant-1", "daily", since, until, "svc-1")
	require.NoError(t, err)
	require.Len(t, metrics, 1, "should only see svc-1's data")
	assert.InDelta(t, 10.0, metrics[0].CPUSeconds, 0.01)
	assert.Equal(t, "svc-1", metrics[0].ServiceID)
}

func TestQueryServiceMetrics_EmptyDatabase(t *testing.T) {
	sqlDB := newMeteringDB(t)
	store := NewStore(sqlDB)

	now := time.Now().UTC()
	metrics, err := store.QueryServiceMetrics("tenant-1", "svc-1", "daily", now.Add(-24*time.Hour), now)
	require.NoError(t, err)
	assert.Empty(t, metrics)
	assert.NotNil(t, metrics)
}

func TestQueryServiceMetrics_InvalidPeriod(t *testing.T) {
	sqlDB := newMeteringDB(t)
	store := NewStore(sqlDB)

	now := time.Now().UTC()
	_, err := store.QueryServiceMetrics("tenant-1", "svc-1", "monthly", now.Add(-24*time.Hour), now)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid period")
}

func TestQueryServiceMetrics_ReturnsData(t *testing.T) {
	sqlDB := newMeteringDB(t)
	store := NewStore(sqlDB)

	ts := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC).Unix()
	insertEvent(t, sqlDB, "e1", "tenant-1", "svc-1", 5.0, 50.0, 100, 200, ts)
	insertEvent(t, sqlDB, "e2", "tenant-1", "svc-1", 3.0, 30.0, 50, 100, ts)
	// Different service, same tenant — should not appear
	insertEvent(t, sqlDB, "e3", "tenant-1", "svc-2", 99.0, 999.0, 9999, 9999, ts)

	since := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 3, 21, 0, 0, 0, 0, time.UTC)

	metrics, err := store.QueryServiceMetrics("tenant-1", "svc-1", "daily", since, until)
	require.NoError(t, err)
	require.Len(t, metrics, 1)
	assert.InDelta(t, 8.0, metrics[0].CPUSeconds, 0.01)
	assert.InDelta(t, 80.0, metrics[0].MemoryMBAvg, 0.01)
	assert.Equal(t, int64(150), metrics[0].NetworkRxBytes)
	assert.Equal(t, int64(300), metrics[0].NetworkTxBytes)
}

func TestQueryServiceMetrics_TenantIsolation(t *testing.T) {
	sqlDB := newMeteringDB(t)
	store := NewStore(sqlDB)

	ts := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC).Unix()
	// Same service_id but different tenants
	insertEvent(t, sqlDB, "e1", "tenant-1", "svc-1", 5.0, 50.0, 100, 200, ts)
	insertEvent(t, sqlDB, "e2", "tenant-2", "svc-1", 99.0, 999.0, 9999, 9999, ts)

	since := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 3, 21, 0, 0, 0, 0, time.UTC)

	metrics, err := store.QueryServiceMetrics("tenant-1", "svc-1", "daily", since, until)
	require.NoError(t, err)
	require.Len(t, metrics, 1)
	assert.InDelta(t, 5.0, metrics[0].CPUSeconds, 0.01)
}

// ---------------------------------------------------------------------------
// Regression tests — TestMeteringStore_Regression
// ---------------------------------------------------------------------------

// Regression: QueryTenantMetrics with no data must return an empty non-nil
// slice so that JSON serialization produces "[]" instead of "null".
func TestMeteringStore_Regression_EmptySliceNotNil(t *testing.T) {
	sqlDB := newMeteringDB(t)
	store := NewStore(sqlDB)

	now := time.Now().UTC()
	metrics, err := store.QueryTenantMetrics("nonexistent-tenant", "daily", now.Add(-24*time.Hour), now, "")
	require.NoError(t, err)
	require.NotNil(t, metrics, "must return non-nil slice for JSON []")
	assert.Len(t, metrics, 0)
}

// Regression: hourly period groups events within the same clock hour together,
// even when they fall at different minutes within that hour.
func TestMeteringStore_Regression_HourlyGrouping(t *testing.T) {
	sqlDB := newMeteringDB(t)
	store := NewStore(sqlDB)

	// Three events across two hours: minute 0, minute 30, and next hour minute 15.
	h1a := time.Date(2026, 3, 20, 14, 0, 0, 0, time.UTC).Unix()
	h1b := time.Date(2026, 3, 20, 14, 30, 0, 0, time.UTC).Unix()
	h2a := time.Date(2026, 3, 20, 15, 15, 0, 0, time.UTC).Unix()

	insertEvent(t, sqlDB, "r1", "t-reg", "svc-a", 1.0, 10.0, 100, 200, h1a)
	insertEvent(t, sqlDB, "r2", "t-reg", "svc-a", 2.0, 20.0, 300, 400, h1b)
	insertEvent(t, sqlDB, "r3", "t-reg", "svc-a", 4.0, 40.0, 500, 600, h2a)

	since := time.Date(2026, 3, 20, 14, 0, 0, 0, time.UTC)
	until := time.Date(2026, 3, 20, 16, 0, 0, 0, time.UTC)

	metrics, err := store.QueryTenantMetrics("t-reg", "hourly", since, until, "")
	require.NoError(t, err)
	require.Len(t, metrics, 2, "events at minute-0 and minute-30 should collapse into one hourly bucket")

	assert.Equal(t, "2026-03-20T14:00:00Z", metrics[0].Timestamp)
	assert.InDelta(t, 3.0, metrics[0].CPUSeconds, 0.01, "hour-14 should sum 1.0+2.0")
	assert.InDelta(t, 30.0, metrics[0].MemoryMBAvg, 0.01)
	assert.Equal(t, int64(400), metrics[0].NetworkRxBytes)
	assert.Equal(t, int64(600), metrics[0].NetworkTxBytes)

	assert.Equal(t, "2026-03-20T15:00:00Z", metrics[1].Timestamp)
	assert.InDelta(t, 4.0, metrics[1].CPUSeconds, 0.01)
}

// Regression: daily period groups events on the same calendar day together,
// regardless of their hour-of-day.
func TestMeteringStore_Regression_DailyGrouping(t *testing.T) {
	sqlDB := newMeteringDB(t)
	store := NewStore(sqlDB)

	d1am := time.Date(2026, 3, 20, 2, 0, 0, 0, time.UTC).Unix()
	d1pm := time.Date(2026, 3, 20, 22, 0, 0, 0, time.UTC).Unix()
	d2 := time.Date(2026, 3, 21, 12, 0, 0, 0, time.UTC).Unix()

	insertEvent(t, sqlDB, "r1", "t-day", "svc-b", 3.0, 30.0, 300, 400, d1am)
	insertEvent(t, sqlDB, "r2", "t-day", "svc-b", 7.0, 70.0, 700, 800, d1pm)
	insertEvent(t, sqlDB, "r3", "t-day", "svc-b", 5.0, 50.0, 500, 600, d2)

	since := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 3, 22, 0, 0, 0, 0, time.UTC)

	metrics, err := store.QueryTenantMetrics("t-day", "daily", since, until, "")
	require.NoError(t, err)
	require.Len(t, metrics, 2, "events at 02:00 and 22:00 on same day should share one daily bucket")

	assert.Equal(t, "2026-03-20T00:00:00Z", metrics[0].Timestamp)
	assert.InDelta(t, 10.0, metrics[0].CPUSeconds, 0.01, "day 20 should sum 3.0+7.0")

	assert.Equal(t, "2026-03-21T00:00:00Z", metrics[1].Timestamp)
	assert.InDelta(t, 5.0, metrics[1].CPUSeconds, 0.01)
}

// Regression: QueryServiceMetrics filters to only the specified service.
// Events for other services under the same tenant must not appear.
func TestMeteringStore_Regression_ServiceFilterStrict(t *testing.T) {
	sqlDB := newMeteringDB(t)
	store := NewStore(sqlDB)

	ts := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC).Unix()
	insertEvent(t, sqlDB, "r1", "t-sf", "svc-target", 10.0, 100.0, 1000, 2000, ts)
	insertEvent(t, sqlDB, "r2", "t-sf", "svc-other-1", 20.0, 200.0, 2000, 3000, ts)
	insertEvent(t, sqlDB, "r3", "t-sf", "svc-other-2", 30.0, 300.0, 3000, 4000, ts)

	since := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 3, 21, 0, 0, 0, 0, time.UTC)

	metrics, err := store.QueryServiceMetrics("t-sf", "svc-target", "daily", since, until)
	require.NoError(t, err)
	require.Len(t, metrics, 1, "should return only the target service")
	assert.InDelta(t, 10.0, metrics[0].CPUSeconds, 0.01)
	assert.Equal(t, int64(1000), metrics[0].NetworkRxBytes)
}

// Regression: since/until time range filtering works correctly.
// Events outside the window must be excluded.
func TestMeteringStore_Regression_TimeRangeFiltering(t *testing.T) {
	sqlDB := newMeteringDB(t)
	store := NewStore(sqlDB)

	beforeWindow := time.Date(2026, 3, 19, 23, 59, 59, 0, time.UTC).Unix()
	insideWindow := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC).Unix()
	afterWindow := time.Date(2026, 3, 21, 0, 0, 1, 0, time.UTC).Unix()

	insertEvent(t, sqlDB, "r1", "t-tr", "svc-1", 5.0, 50.0, 100, 200, beforeWindow)
	insertEvent(t, sqlDB, "r2", "t-tr", "svc-1", 10.0, 100.0, 1000, 2000, insideWindow)
	insertEvent(t, sqlDB, "r3", "t-tr", "svc-1", 15.0, 150.0, 1500, 3000, afterWindow)

	since := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 3, 21, 0, 0, 0, 0, time.UTC)

	metrics, err := store.QueryTenantMetrics("t-tr", "daily", since, until, "")
	require.NoError(t, err)
	require.Len(t, metrics, 1, "only the event inside the window should appear")
	assert.InDelta(t, 10.0, metrics[0].CPUSeconds, 0.01)
}

// Regression: querying a non-existent service returns empty, not an error.
func TestMeteringStore_Regression_NonexistentServiceReturnsEmpty(t *testing.T) {
	sqlDB := newMeteringDB(t)
	store := NewStore(sqlDB)

	ts := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC).Unix()
	insertEvent(t, sqlDB, "r1", "t-ne", "svc-exists", 10.0, 100.0, 1000, 2000, ts)

	since := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 3, 21, 0, 0, 0, 0, time.UTC)

	metrics, err := store.QueryServiceMetrics("t-ne", "svc-does-not-exist", "daily", since, until)
	require.NoError(t, err, "non-existent service should not error")
	assert.Empty(t, metrics, "should return empty slice for non-existent service")
	assert.NotNil(t, metrics, "should be non-nil empty slice")
}

// Regression: multiple services with overlapping timestamps aggregate correctly
// by tenant. When querying without a service filter, each service+bucket pair
// should be a separate row, not collapsed into a single aggregate.
func TestMeteringStore_Regression_MultiServiceOverlappingTimestamps(t *testing.T) {
	sqlDB := newMeteringDB(t)
	store := NewStore(sqlDB)

	ts := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC).Unix()
	insertEvent(t, sqlDB, "r1", "t-ms", "svc-A", 10.0, 100.0, 1000, 2000, ts)
	insertEvent(t, sqlDB, "r2", "t-ms", "svc-A", 5.0, 50.0, 500, 1000, ts)
	insertEvent(t, sqlDB, "r3", "t-ms", "svc-B", 20.0, 200.0, 2000, 3000, ts)
	insertEvent(t, sqlDB, "r4", "t-ms", "svc-C", 7.0, 70.0, 700, 1400, ts)

	since := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 3, 21, 0, 0, 0, 0, time.UTC)

	metrics, err := store.QueryTenantMetrics("t-ms", "daily", since, until, "")
	require.NoError(t, err)
	// The query groups by (bucket, service_id), so we expect 3 rows:
	// svc-A aggregated (10+5=15), svc-B (20), svc-C (7)
	require.Len(t, metrics, 3, "each service should get its own row in the same daily bucket")

	// Verify total CPU across all services
	totalCPU := 0.0
	for _, m := range metrics {
		totalCPU += m.CPUSeconds
	}
	assert.InDelta(t, 42.0, totalCPU, 0.01, "total CPU across all services should be 10+5+20+7=42")

	// Verify svc-A is aggregated correctly
	for _, m := range metrics {
		if m.ServiceID == "svc-A" {
			assert.InDelta(t, 15.0, m.CPUSeconds, 0.01, "svc-A events should sum to 15")
			assert.InDelta(t, 150.0, m.MemoryMBAvg, 0.01)
			assert.Equal(t, int64(1500), m.NetworkRxBytes)
		}
	}
}
