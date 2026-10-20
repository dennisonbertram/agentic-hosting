package metering

import (
	"context"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQueryMetrics_DBErrorsPropagate(t *testing.T) {
	sqlDB := newMeteringDB(t)
	store := NewStore(sqlDB)
	_, err := sqlDB.Exec(`DROP TABLE usage_events`)
	require.NoError(t, err)

	now := time.Now()
	_, err = store.QueryTenantMetrics("t1", "daily", now.Add(-time.Hour), now, "")
	assert.ErrorContains(t, err, "metering query")
	_, err = store.QueryServiceMetrics("t1", "s1", "hourly", now.Add(-time.Hour), now)
	assert.ErrorContains(t, err, "metering query")
}

// The service query and the tenant query filtered to one service must agree.
func TestQueryServiceMetrics_MatchesFilteredTenantQuery(t *testing.T) {
	sqlDB := newMeteringDB(t)
	store := NewStore(sqlDB)
	base := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC).Unix()
	insertEvent(t, sqlDB, "e1", "t1", "s1", 1, 60, 0, 0, base)
	insertEvent(t, sqlDB, "e2", "t1", "s1", 2, 120, 0, 0, base+60)
	insertEvent(t, sqlDB, "e3", "t1", "s2", 5, 60, 0, 0, base+60)
	insertEvent(t, sqlDB, "e4", "t2", "s1", 9, 60, 0, 0, base+60)

	since, until := time.Unix(base-1, 0), time.Unix(base+3600, 0)
	svc, err := store.QueryServiceMetrics("t1", "s1", "hourly", since, until)
	require.NoError(t, err)
	ten, err := store.QueryTenantMetrics("t1", "hourly", since, until, "s1")
	require.NoError(t, err)
	assert.Equal(t, ten, svc)
	require.Len(t, svc, 1)
	assert.Equal(t, "s1", svc[0].ServiceID)
	assert.InDelta(t, 3.0, svc[0].CPUSeconds, 0.001)
	assert.InDelta(t, 1.5, svc[0].MemoryMBAvg, 0.001)
}

func TestCollectorTick_StateQueryFailureRecordsNothing(t *testing.T) {
	store := newCollectorStore(t)
	mock := &testutil.MockDockerClient{}
	c := NewCollector(store.StateDB, store.MeteringDB, mock)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Tick(ctx) // cancelled context: the services query fails

	var n int
	require.NoError(t, store.MeteringDB.QueryRow(`SELECT COUNT(*) FROM usage_events`).Scan(&n))
	assert.Zero(t, n)
}

// A container whose CPU counter goes backwards (restarted in place) records
// the new cumulative total instead of a negative or wrapped delta.
func TestCollectorTick_CounterResetRecordsNewTotal(t *testing.T) {
	store := newCollectorStore(t)
	cur := uint64(5e9)
	mock := &testutil.MockDockerClient{ContainerStatsFn: func(_ context.Context, id string) (*docker.ContainerStats, error) {
		return &docker.ContainerStats{CPUTotalNanos: cur, MemoryBytes: 1024 * 1024}, nil
	}}
	c := NewCollector(store.StateDB, store.MeteringDB, mock)
	c.Tick(context.Background()) // baseline, records 0 CPU
	cur = 2e9
	c.Tick(context.Background())

	var cpu, mem float64
	require.NoError(t, store.MeteringDB.QueryRow(`SELECT cpu_seconds, memory_mb_seconds FROM usage_daily WHERE service_id='s1'`).Scan(&cpu, &mem))
	assert.InDelta(t, 2.0, cpu, 0.001)
	assert.InDelta(t, 2*SampleSeconds, mem, 0.001, "1 MB for two samples")
}
