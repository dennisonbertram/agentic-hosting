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

// Internet-egress sandboxes record network bytes as deltas of the cumulative
// container counters; services and egress-none sandboxes keep recording 0,
// exactly as before.
func TestCollectorTick_MetersEgressSandboxNetworkBytes(t *testing.T) {
	store := newCollectorStore(t)
	now := time.Now().Unix()
	for _, r := range []struct{ id, cid, egress string }{
		{"sbx-net", "cn", "internet"},
		{"sbx-none", "cx", "none"},
	} {
		_, err := store.StateDB.Exec(`INSERT INTO environments (id, tenant_id, name, kind, status, container_id, egress, created_at, updated_at)
			VALUES (?, 't1', ?, 'sandbox', 'running', ?, ?, ?, ?)`, r.id, r.id, r.cid, r.egress, now, now)
		require.NoError(t, err)
	}
	var rx, tx uint64 = 1000, 500
	mock := &testutil.MockDockerClient{ContainerStatsFn: func(_ context.Context, id string) (*docker.ContainerStats, error) {
		return &docker.ContainerStats{CPUTotalNanos: 1e9, MemoryBytes: 1 << 20, NetRxBytes: rx, NetTxBytes: tx}, nil
	}}
	c := NewCollector(store.StateDB, store.MeteringDB, mock)

	daily := func(id string) (int64, int64) {
		var in, out int64
		require.NoError(t, store.MeteringDB.QueryRow(
			`SELECT network_ingress_bytes, network_egress_bytes FROM usage_daily WHERE service_id = ?`, id).Scan(&in, &out))
		return in, out
	}

	c.Tick(context.Background()) // baseline
	in, out := daily("sbx-net")
	assert.Zero(t, in)
	assert.Zero(t, out)

	rx, tx = 5000, 2500
	c.Tick(context.Background())
	in, out = daily("sbx-net")
	assert.EqualValues(t, 4000, in)
	assert.EqualValues(t, 2000, out)

	// Counter reset (container restarted): the new total is recorded.
	rx, tx = 100, 50
	c.Tick(context.Background())
	in, out = daily("sbx-net")
	assert.EqualValues(t, 4100, in)
	assert.EqualValues(t, 2050, out)

	for _, id := range []string{"sbx-none", "s1", "s2"} {
		in, out = daily(id)
		assert.Zero(t, in, id)
		assert.Zero(t, out, id)
	}

	// The tenant's sandbox metrics expose the egress bytes.
	ms := NewStore(store.MeteringDB)
	m, err := ms.QueryTenantMetricsKind("t1", ResourceKindSandbox, "daily", time.Now().Add(-time.Hour), time.Now().Add(time.Hour), "sbx-net")
	require.NoError(t, err)
	require.Len(t, m, 1)
	assert.EqualValues(t, 2050, m[0].NetworkTxBytes)
	assert.EqualValues(t, 4100, m[0].NetworkRxBytes)
}

func TestCounterDelta(t *testing.T) {
	assert.EqualValues(t, 5, counterDelta(10, 5))
	assert.EqualValues(t, 3, counterDelta(3, 10), "reset")
	assert.EqualValues(t, 0, counterDelta(7, 7))
}

// Regression: a sandbox created after the collector started is sampled from
// zero, so its first sample records the whole cumulative counters. Otherwise a
// sandbox that lives less than one tick records no CPU or egress at all.
// Sandboxes predating the collector keep the baseline-only first sample.
func TestCollectorTick_FreshSandboxFirstSampleRecordsTotals(t *testing.T) {
	store := newCollectorStore(t)
	mock := &testutil.MockDockerClient{ContainerStatsFn: func(_ context.Context, id string) (*docker.ContainerStats, error) {
		return &docker.ContainerStats{CPUTotalNanos: 3e9, MemoryBytes: 1 << 20, NetRxBytes: 7000, NetTxBytes: 9000}, nil
	}}
	c := NewCollector(store.StateDB, store.MeteringDB, mock)
	for _, r := range []struct {
		id, cid string
		created int64
	}{
		{"sbx-fresh", "cf", c.startedAt + 5},
		{"sbx-old", "co", c.startedAt - 3600},
	} {
		_, err := store.StateDB.Exec(`INSERT INTO environments (id, tenant_id, name, kind, status, container_id, egress, created_at, updated_at)
			VALUES (?, 't1', ?, 'sandbox', 'running', ?, 'internet', ?, ?)`, r.id, r.id, r.cid, r.created, r.created)
		require.NoError(t, err)
	}

	c.Tick(context.Background())

	row := func(id string) (cpu float64, in, out int64) {
		require.NoError(t, store.MeteringDB.QueryRow(
			`SELECT cpu_seconds, network_ingress_bytes, network_egress_bytes FROM usage_daily WHERE service_id = ?`, id).Scan(&cpu, &in, &out))
		return
	}
	cpu, in, out := row("sbx-fresh")
	assert.InDelta(t, 3.0, cpu, 1e-9)
	assert.EqualValues(t, 7000, in)
	assert.EqualValues(t, 9000, out)

	cpu, in, out = row("sbx-old")
	assert.Zero(t, cpu, "a sandbox from before this process keeps the baseline-only first sample")
	assert.Zero(t, in)
	assert.Zero(t, out)
}
