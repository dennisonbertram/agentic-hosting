package reconciler_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/reconciler"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// If the container cannot be stopped when the breaker trips, the row must not
// be committed as circuit-open: it has to stay watched and be retried.
func TestReconciler_BreakerTripStopFailureIsRetried(t *testing.T) {
	db := testutil.NewStateDB(t)
	ctx := context.Background()
	seedService(t, db, "svc-b", "running", "cid-b")
	restarts := 0
	mock := mockWith("cid-b", func() *docker.ContainerInfo {
		return &docker.ContainerInfo{Status: "running", RestartCount: restarts}
	})
	fail := true
	mock.StopContainerFn = func(ctx context.Context, id string) error {
		if fail {
			return errors.New("docker busy")
		}
		return nil
	}
	r := reconciler.New(db, mock, time.Minute, nil)
	require.NoError(t, r.ReconcileOnce(ctx))
	restarts = 5
	require.NoError(t, r.ReconcileOnce(ctx))
	s := getSvc(t, db, "svc-b")
	assert.Equal(t, 0, s.circuitOpen, "stop failed: circuit must not be committed")
	assert.Equal(t, "cid-b", s.containerID)

	fail = false
	require.NoError(t, r.ReconcileOnce(ctx))
	s = getSvc(t, db, "svc-b")
	assert.Equal(t, 1, s.circuitOpen, "retry must open the circuit once stop works")
	assert.Equal(t, "", s.containerID)
	assert.Contains(t, mock.RemoveContainerCalls, "cid-b")
}

// If Stop marks the row stopped while the reconciler is restarting an unhealthy
// container, the reconciler must stop the container again.
func TestReconciler_UnhealthyRestartRacingStop(t *testing.T) {
	db := testutil.NewStateDB(t)
	ctx := context.Background()
	seedService(t, db, "svc-s", "running", "cid-s")
	mock := mockWith("cid-s", func() *docker.ContainerInfo {
		return &docker.ContainerInfo{Status: "running", HealthStatus: "unhealthy"}
	})
	mock.RestartContainerFn = func(ctx context.Context, id string) error {
		_, err := db.Exec(`UPDATE services SET status='stopped' WHERE id='svc-s'`)
		return err
	}
	r := reconciler.New(db, mock, time.Minute, nil)
	require.NoError(t, r.ReconcileOnce(ctx))
	assert.Contains(t, mock.StopContainerCalls, "cid-s")
}

// An expired lease is claimed in the database before the container is stopped, so
// a lease renewal cannot interleave with the stop.
func TestReconciler_LeaseClaimedBeforeStop(t *testing.T) {
	db := testutil.NewStateDB(t)
	ctx := context.Background()
	now := time.Now().Unix()
	seedService(t, db, "svc-x", "running", "cid-x")
	_, err := db.Exec(`INSERT INTO environments (id, tenant_id, name, status, container_id, lease_expires_at, created_at, updated_at)
		VALUES ('env-1','tenant-1','e','running','cid-env',?,?,?)`, now-10, now, now)
	require.NoError(t, err)
	mock := mockWith("cid-x", func() *docker.ContainerInfo { return &docker.ContainerInfo{Status: "running"} })
	var statusAtStop string
	mock.StopContainerFn = func(ctx context.Context, id string) error {
		if id == "cid-env" {
			_ = db.QueryRow(`SELECT status FROM environments WHERE id='env-1'`).Scan(&statusAtStop)
		}
		return nil
	}
	r := reconciler.New(db, mock, time.Minute, nil)
	require.NoError(t, r.ReconcileOnce(ctx))
	assert.Equal(t, "stopped", statusAtStop)
}
