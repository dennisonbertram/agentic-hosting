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
