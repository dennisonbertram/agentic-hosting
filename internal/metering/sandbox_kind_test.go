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

func TestCollectorTick_SamplesRunningSandboxesWithKind(t *testing.T) {
	store := newCollectorStore(t)
	now := time.Now().Unix()
	env := func(id, kind, status, cid string) {
		_, err := store.StateDB.Exec(`INSERT INTO environments (id, tenant_id, name, kind, status, container_id, created_at, updated_at)
			VALUES (?, 't1', ?, ?, ?, ?, ?, ?)`, id, id, kind, status, cid, now, now)
		require.NoError(t, err)
	}
	env("sbx1", "sandbox", "running", "cs1")
	env("sbx2", "sandbox", "stopped", "cs2")
	env("env1", "environment", "running", "ce1")

	mock := &testutil.MockDockerClient{ContainerStatsFn: func(_ context.Context, id string) (*docker.ContainerStats, error) {
		return &docker.ContainerStats{CPUTotalNanos: 1e9, MemoryBytes: 64 * 1024 * 1024}, nil
	}}
	c := NewCollector(store.StateDB, store.MeteringDB, mock)
	c.Tick(context.Background())

	kinds := map[string]string{}
	rows, err := store.MeteringDB.Query(`SELECT service_id, resource_kind FROM usage_events`)
	require.NoError(t, err)
	for rows.Next() {
		var id, kind string
		require.NoError(t, rows.Scan(&id, &kind))
		kinds[id] = kind
	}
	require.NoError(t, rows.Close())
	assert.Equal(t, map[string]string{"s1": "service", "s2": "service", "sbx1": "sandbox"}, kinds,
		"running sandboxes are sampled; stopped sandboxes and environments are not")

	var dailyKind string
	require.NoError(t, store.MeteringDB.QueryRow(`SELECT resource_kind FROM usage_daily WHERE service_id = 'sbx1'`).Scan(&dailyKind))
	assert.Equal(t, "sandbox", dailyKind)

	// Service metrics queries exclude sandbox samples; the kind query returns them.
	ms := NewStore(store.MeteringDB)
	since, until := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	svcMetrics, err := ms.QueryTenantMetrics("t1", "daily", since, until, "")
	require.NoError(t, err)
	for _, m := range svcMetrics {
		assert.NotEqual(t, "sbx1", m.ServiceID)
	}
	require.Len(t, svcMetrics, 1)
	sbxMetrics, err := ms.QueryTenantMetricsKind("t1", ResourceKindSandbox, "daily", since, until, "")
	require.NoError(t, err)
	require.Len(t, sbxMetrics, 1)
	assert.Equal(t, "sbx1", sbxMetrics[0].ServiceID)
	byID, err := ms.QueryTenantMetricsKind("t1", ResourceKindSandbox, "daily", since, until, "sbx1")
	require.NoError(t, err)
	assert.Len(t, byID, 1)
	_, err = ms.QueryTenantMetricsKind("t1", "database", "daily", since, until, "")
	assert.Error(t, err)

	// Per-tenant usage_daily totals cover both kinds.
	var total float64
	require.NoError(t, store.MeteringDB.QueryRow(`SELECT SUM(memory_mb_seconds) FROM usage_daily WHERE tenant_id = 't1'`).Scan(&total))
	assert.InDelta(t, 2*64.0*SampleSeconds, total, 0.01)
}
