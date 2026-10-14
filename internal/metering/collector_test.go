package metering

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newCollectorStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "ah.db"))
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })

	now := time.Now().Unix()
	for _, tn := range []string{"t1", "t2"} {
		_, err = store.StateDB.Exec(`INSERT INTO tenants (id, name, email, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`, tn, tn, tn+"@x.test", now, now)
		require.NoError(t, err)
	}
	svc := func(id, tenant, status, cid string) {
		_, err := store.StateDB.Exec(`INSERT INTO services (id, tenant_id, name, status, container_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, tenant, id, status, cid, now, now)
		require.NoError(t, err)
	}
	svc("s1", "t1", "running", "c1")
	svc("s2", "t2", "running", "c2")
	svc("s3", "t1", "stopped", "c3")
	return store
}

func TestCollectorTick_RecordsRunningServices(t *testing.T) {
	store := newCollectorStore(t)
	mock := &testutil.MockDockerClient{ContainerStatsFn: func(_ context.Context, id string) (*docker.ContainerStats, error) {
		return &docker.ContainerStats{CPUTotalNanos: 2e9, MemoryBytes: 100 * 1024 * 1024}, nil
	}}
	c := NewCollector(store.StateDB, store.MeteringDB, mock)
	c.Tick(context.Background())

	rows, err := store.MeteringDB.Query(`SELECT tenant_id, service_id, memory_mb_seconds FROM usage_events ORDER BY service_id`)
	require.NoError(t, err)
	defer rows.Close()
	var got [][2]string
	for rows.Next() {
		var tn, sv string
		var mem float64
		require.NoError(t, rows.Scan(&tn, &sv, &mem))
		assert.InDelta(t, 100.0*SampleSeconds, mem, 0.01)
		got = append(got, [2]string{tn, sv})
	}
	assert.Equal(t, [][2]string{{"t1", "s1"}, {"t2", "s2"}}, got, "stopped service must not be sampled")

	var n int
	require.NoError(t, store.MeteringDB.QueryRow(`SELECT COUNT(*) FROM usage_daily`).Scan(&n))
	assert.Equal(t, 2, n)

	// Second tick: CPU is a delta of the cumulative counter, daily row accumulates.
	mock.ContainerStatsFn = func(_ context.Context, id string) (*docker.ContainerStats, error) {
		return &docker.ContainerStats{CPUTotalNanos: 5e9, MemoryBytes: 100 * 1024 * 1024}, nil
	}
	c.Tick(context.Background())
	var cpu float64
	require.NoError(t, store.MeteringDB.QueryRow(`SELECT cpu_seconds FROM usage_daily WHERE service_id='s1'`).Scan(&cpu))
	assert.InDelta(t, 3.0, cpu, 0.001)
}

func TestCollectorTick_FailingContainerDoesNotBlockOthers(t *testing.T) {
	store := newCollectorStore(t)
	mock := &testutil.MockDockerClient{ContainerStatsFn: func(_ context.Context, id string) (*docker.ContainerStats, error) {
		if id == "c1" {
			return nil, errors.New("boom")
		}
		return &docker.ContainerStats{MemoryBytes: 1 << 20}, nil
	}}
	NewCollector(store.StateDB, store.MeteringDB, mock).Tick(context.Background())

	var n int
	require.NoError(t, store.MeteringDB.QueryRow(`SELECT COUNT(*) FROM usage_events WHERE service_id='s2' AND tenant_id='t2'`).Scan(&n))
	assert.Equal(t, 1, n)
	require.NoError(t, store.MeteringDB.QueryRow(`SELECT COUNT(*) FROM usage_events`).Scan(&n))
	assert.Equal(t, 1, n)
}

func TestCollectorRun_StopsOnCancel(t *testing.T) {
	store := newCollectorStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		NewCollector(store.StateDB, store.MeteringDB, &testutil.MockDockerClient{}).Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on context cancel")
	}
}
