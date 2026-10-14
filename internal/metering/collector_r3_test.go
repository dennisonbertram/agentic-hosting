package metering

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCollectorTick_ContainerReplacementDoesNotMixCounters(t *testing.T) {
	store := newCollectorStore(t)
	total := map[string]uint64{"c1": 10e9, "c2": 1e9, "c9": 50e9}
	mock := &testutil.MockDockerClient{ContainerStatsFn: func(_ context.Context, id string) (*docker.ContainerStats, error) {
		return &docker.ContainerStats{CPUTotalNanos: total[id]}, nil
	}}
	c := NewCollector(store.StateDB, store.MeteringDB, mock)
	c.Tick(context.Background())

	// s1 is redeployed onto a new container whose counter starts elsewhere.
	_, err := store.StateDB.Exec(`UPDATE services SET container_id='c9' WHERE id='s1'`)
	require.NoError(t, err)
	c.Tick(context.Background())

	var cpu float64
	require.NoError(t, store.MeteringDB.QueryRow(`SELECT cpu_seconds FROM usage_daily WHERE service_id='s1'`).Scan(&cpu))
	assert.InDelta(t, 0.0, cpu, 0.001, "new container's first sample is a baseline, not a delta against the old container")
}

func TestCollectorTick_BaselineAdvancesOnlyAfterSuccessfulWrite(t *testing.T) {
	store := newCollectorStore(t)
	cur := uint64(1e9)
	mock := &testutil.MockDockerClient{ContainerStatsFn: func(_ context.Context, id string) (*docker.ContainerStats, error) {
		return &docker.ContainerStats{CPUTotalNanos: cur}, nil
	}}
	c := NewCollector(store.StateDB, store.MeteringDB, mock)
	c.Tick(context.Background()) // baseline 1s

	_, err := store.MeteringDB.Exec(`ALTER TABLE usage_events RENAME TO usage_events_x`)
	require.NoError(t, err)
	cur = 3e9
	c.Tick(context.Background()) // write fails
	_, err = store.MeteringDB.Exec(`ALTER TABLE usage_events_x RENAME TO usage_events`)
	require.NoError(t, err)

	cur = 4e9
	c.Tick(context.Background())
	var cpu float64
	require.NoError(t, store.MeteringDB.QueryRow(`SELECT cpu_seconds FROM usage_daily WHERE service_id='s1'`).Scan(&cpu))
	assert.InDelta(t, 3.0, cpu, 0.001, "delta since the last recorded sample (1s -> 4s)")
}

func TestCollectorTick_StaleBaselinesDropped(t *testing.T) {
	store := newCollectorStore(t)
	mock := &testutil.MockDockerClient{ContainerStatsFn: func(_ context.Context, id string) (*docker.ContainerStats, error) {
		return &docker.ContainerStats{CPUTotalNanos: 1e9}, nil
	}}
	c := NewCollector(store.StateDB, store.MeteringDB, mock)
	c.Tick(context.Background())
	require.Len(t, c.lastCPU, 2)
	_, err := store.StateDB.Exec(`UPDATE services SET status='stopped' WHERE id='s2'`)
	require.NoError(t, err)
	c.Tick(context.Background())
	assert.Len(t, c.lastCPU, 1)
	assert.Contains(t, c.lastCPU, "c1")
}

func TestCollectorTick_StatsCallHasTimeout(t *testing.T) {
	store := newCollectorStore(t)
	mock := &testutil.MockDockerClient{ContainerStatsFn: func(ctx context.Context, id string) (*docker.ContainerStats, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	c := NewCollector(store.StateDB, store.MeteringDB, mock)
	c.statsTimeout = 50 * time.Millisecond
	done := make(chan struct{})
	go func() { c.Tick(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a hung ContainerStats call blocked the tick")
	}
}

func TestCollectorTick_MissingContainerLoggedOncePerContainer(t *testing.T) {
	store := newCollectorStore(t)
	mock := &testutil.MockDockerClient{ContainerStatsFn: func(_ context.Context, id string) (*docker.ContainerStats, error) {
		return nil, errors.New("no such container")
	}}
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	c := NewCollector(store.StateDB, store.MeteringDB, mock)
	for i := 0; i < 3; i++ {
		c.Tick(context.Background())
	}
	assert.Equal(t, 1, strings.Count(buf.String(), "container c1"), "logged: %s", buf.String())
}
