package reconciler_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/reconciler"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedService(t *testing.T, db *sql.DB, id, status, containerID string) {
	t.Helper()
	now := time.Now().Unix()
	_, err := db.Exec(`INSERT OR IGNORE INTO tenants (id, name, email, status, created_at, updated_at) VALUES ('tenant-1','T','t@example.com','active',?,?)`, now, now)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO services (id, tenant_id, name, status, container_id, created_at, updated_at)
		VALUES (?, 'tenant-1', ?, ?, ?, ?, ?)`, id, id, status, containerID, now, now)
	require.NoError(t, err)
}

type svcRow struct {
	status      string
	containerID string
	crashCount  int
	circuitOpen int
}

func getSvc(t *testing.T, db *sql.DB, id string) svcRow {
	t.Helper()
	var r svcRow
	require.NoError(t, db.QueryRow(`SELECT status, container_id, crash_count, circuit_open FROM services WHERE id = ?`, id).
		Scan(&r.status, &r.containerID, &r.crashCount, &r.circuitOpen))
	return r
}

func mockWith(cid string, info func() *docker.ContainerInfo) *testutil.MockDockerClient {
	m := &testutil.MockDockerClient{}
	m.ListContainersByLabelFn = func(ctx context.Context, label, value string) ([]string, error) {
		if label == "ah.tenant" {
			return []string{cid}, nil
		}
		return nil, nil
	}
	m.InspectContainerFn = func(ctx context.Context, id string) (*docker.ContainerInfo, error) {
		return info(), nil
	}
	return m
}

// A crash loop seen as Docker state "restarting" with a growing RestartCount must
// trip the breaker across several ticks, and the service must stay under watch
// once it is marked crashed.
func TestReconciler_RestartingCrashLoopTripsBreaker(t *testing.T) {
	db := testutil.NewStateDB(t)
	ctx := context.Background()
	seedService(t, db, "svc-loop", "running", "cid-loop")

	restarts := 0
	mock := mockWith("cid-loop", func() *docker.ContainerInfo {
		return &docker.ContainerInfo{Status: "restarting", RestartCount: restarts}
	})
	r := reconciler.New(db, mock, time.Minute, nil)

	require.NoError(t, r.ReconcileOnce(ctx)) // baseline observation, no crash yet
	assert.Equal(t, 0, getSvc(t, db, "svc-loop").crashCount)

	for i := 1; i <= 4; i++ {
		restarts++
		require.NoError(t, r.ReconcileOnce(ctx))
		s := getSvc(t, db, "svc-loop")
		assert.Equal(t, "crashed", s.status)
		assert.Equal(t, i, s.crashCount, "tick %d", i)
		assert.Equal(t, 0, s.circuitOpen)
	}
	restarts++
	require.NoError(t, r.ReconcileOnce(ctx))
	s := getSvc(t, db, "svc-loop")
	assert.Equal(t, 5, s.crashCount)
	assert.Equal(t, 1, s.circuitOpen, "5 restarts within the window must open the circuit")
	assert.Contains(t, mock.RemoveContainerCalls, "cid-loop")
}

// Several restarts between two ticks count as several crashes.
func TestReconciler_RestartDeltaCountsMultiple(t *testing.T) {
	db := testutil.NewStateDB(t)
	ctx := context.Background()
	seedService(t, db, "svc-d", "running", "cid-d")
	restarts := 0
	mock := mockWith("cid-d", func() *docker.ContainerInfo {
		return &docker.ContainerInfo{Status: "running", RestartCount: restarts}
	})
	r := reconciler.New(db, mock, time.Minute, nil)
	require.NoError(t, r.ReconcileOnce(ctx))
	restarts = 5
	require.NoError(t, r.ReconcileOnce(ctx))
	s := getSvc(t, db, "svc-d")
	assert.Equal(t, 5, s.crashCount)
	assert.Equal(t, 1, s.circuitOpen)
}

// A crashed service that Docker has brought back, healthy and with no new
// restarts, returns to running.
func TestReconciler_CrashedRecoversToRunning(t *testing.T) {
	db := testutil.NewStateDB(t)
	ctx := context.Background()
	seedService(t, db, "svc-r", "running", "cid-r")
	restarts := 0
	state := "running"
	mock := mockWith("cid-r", func() *docker.ContainerInfo {
		return &docker.ContainerInfo{Status: state, RestartCount: restarts, HealthStatus: "healthy"}
	})
	r := reconciler.New(db, mock, time.Minute, nil)
	require.NoError(t, r.ReconcileOnce(ctx))
	restarts = 1
	require.NoError(t, r.ReconcileOnce(ctx))
	assert.Equal(t, "crashed", getSvc(t, db, "svc-r").status)
	require.NoError(t, r.ReconcileOnce(ctx)) // no new restarts
	assert.Equal(t, "running", getSvc(t, db, "svc-r").status)
}

// A deploy that completes after the reconciler's scan must not be overwritten.
func TestReconciler_ConcurrentDeployNotClobbered(t *testing.T) {
	db := testutil.NewStateDB(t)
	ctx := context.Background()
	seedService(t, db, "svc-c", "running", "cid-old")
	mock := mockWith("cid-old", func() *docker.ContainerInfo {
		return &docker.ContainerInfo{Status: "exited", ExitCode: 1}
	})
	mock.InspectContainerFn = func(ctx context.Context, id string) (*docker.ContainerInfo, error) {
		// The deploy finishes while the reconciler is looking at the old container.
		_, err := db.Exec(`UPDATE services SET container_id = 'cid-new', status = 'running' WHERE id = 'svc-c'`)
		require.NoError(t, err)
		return &docker.ContainerInfo{Status: "exited", ExitCode: 1}, nil
	}
	r := reconciler.New(db, mock, time.Minute, nil)
	require.NoError(t, r.ReconcileOnce(ctx))
	s := getSvc(t, db, "svc-c")
	assert.Equal(t, "running", s.status)
	assert.Equal(t, "cid-new", s.containerID)
	assert.Equal(t, 0, s.crashCount)
	assert.Empty(t, mock.StopContainerCalls)
	assert.Empty(t, mock.RemoveContainerCalls)
}

// A timeout error that happens to contain "404" is not a not-found error.
func TestReconciler_DeadlineErrorWith404IsNotNotFound(t *testing.T) {
	db := testutil.NewStateDB(t)
	ctx := context.Background()
	seedService(t, db, "svc-t", "running", "cid-t")
	mock := &testutil.MockDockerClient{}
	mock.ListContainersByLabelFn = func(ctx context.Context, label, value string) ([]string, error) { return nil, nil }
	mock.InspectContainerFn = func(ctx context.Context, id string) (*docker.ContainerInfo, error) {
		return nil, fmt.Errorf("inspect container: Get http://docker/containers/404abc/json: %w", context.DeadlineExceeded)
	}
	r := reconciler.New(db, mock, time.Minute, nil)
	require.NoError(t, r.ReconcileOnce(ctx))
	assert.Equal(t, "running", getSvc(t, db, "svc-t").status)
}

// A real not-found error still marks the service crashed.
func TestReconciler_NotFoundMarksCrashed(t *testing.T) {
	db := testutil.NewStateDB(t)
	ctx := context.Background()
	seedService(t, db, "svc-n", "running", "cid-n")
	mock := &testutil.MockDockerClient{}
	mock.ListContainersByLabelFn = func(ctx context.Context, label, value string) ([]string, error) { return nil, nil }
	mock.InspectContainerFn = func(ctx context.Context, id string) (*docker.ContainerInfo, error) {
		return nil, testutil.NewNotFoundError(id)
	}
	r := reconciler.New(db, mock, time.Minute, nil)
	require.NoError(t, r.ReconcileOnce(ctx))
	s := getSvc(t, db, "svc-n")
	assert.Equal(t, "crashed", s.status)
	assert.Equal(t, 1, s.crashCount)
	// A gone container is not re-counted on every later tick.
	require.NoError(t, r.ReconcileOnce(ctx))
	assert.Equal(t, 1, getSvc(t, db, "svc-n").crashCount)
}

// An unhealthy container is restarted (not stopped, which unless-stopped never undoes),
// and counted once even though it was also scanned by the crash check.
func TestReconciler_UnhealthyIsRestartedAndCountedOnce(t *testing.T) {
	db := testutil.NewStateDB(t)
	ctx := context.Background()
	seedService(t, db, "svc-u", "running", "cid-u")
	mock := mockWith("cid-u", func() *docker.ContainerInfo {
		return &docker.ContainerInfo{Status: "running", HealthStatus: "unhealthy"}
	})
	r := reconciler.New(db, mock, time.Minute, nil)
	require.NoError(t, r.ReconcileOnce(ctx))
	assert.Equal(t, []string{"cid-u"}, mock.RestartContainerCalls)
	assert.Empty(t, mock.StopContainerCalls)
	assert.Equal(t, 1, getSvc(t, db, "svc-u").crashCount)
}

// A lease whose container fails to stop is retried on the next tick.
func TestReconciler_LeaseStopRetried(t *testing.T) {
	db := testutil.NewStateDB(t)
	ctx := context.Background()
	now := time.Now().Unix()
	seedService(t, db, "svc-x", "running", "cid-x") // unrelated service keeps the tenant row
	_, err := db.Exec(`INSERT INTO environments (id, tenant_id, name, status, container_id, lease_expires_at, created_at, updated_at)
		VALUES ('env-1','tenant-1','e','running','cid-env',?,?,?)`, now-10, now, now)
	require.NoError(t, err)

	mock := &testutil.MockDockerClient{}
	mock.ListContainersByLabelFn = func(ctx context.Context, label, value string) ([]string, error) {
		return []string{"cid-x", "cid-env"}, nil
	}
	mock.InspectContainerFn = func(ctx context.Context, id string) (*docker.ContainerInfo, error) {
		return &docker.ContainerInfo{Status: "running"}, nil
	}
	fail := true
	mock.StopContainerFn = func(ctx context.Context, id string) error {
		if fail {
			return errors.New("docker busy")
		}
		return nil
	}
	r := reconciler.New(db, mock, time.Minute, nil)
	require.NoError(t, r.ReconcileOnce(ctx))

	stopCalls := func() int {
		n := 0
		for _, c := range mock.StopContainerCalls {
			if c == "cid-env" {
				n++
			}
		}
		return n
	}
	require.Equal(t, 1, stopCalls())
	fail = false
	require.NoError(t, r.ReconcileOnce(ctx))
	assert.Equal(t, 2, stopCalls(), "failed stop must be retried")
	var st string
	require.NoError(t, db.QueryRow(`SELECT status FROM environments WHERE id='env-1'`).Scan(&st))
	assert.Equal(t, "stopped", st)
}

// A stopped environment whose container is still running (earlier stop failed) gets stopped.
func TestReconciler_StoppedExpiredEnvStillRunningIsStopped(t *testing.T) {
	db := testutil.NewStateDB(t)
	ctx := context.Background()
	now := time.Now().Unix()
	seedService(t, db, "svc-y", "stopped", "")
	_, err := db.Exec(`INSERT INTO environments (id, tenant_id, name, status, container_id, lease_expires_at, created_at, updated_at)
		VALUES ('env-2','tenant-1','e','stopped','cid-env2',?,?,?)`, now-10, now, now)
	require.NoError(t, err)
	mock := mockWith("cid-env2", func() *docker.ContainerInfo { return &docker.ContainerInfo{Status: "running"} })
	r := reconciler.New(db, mock, time.Minute, nil)
	require.NoError(t, r.ReconcileOnce(ctx))
	assert.Contains(t, mock.StopContainerCalls, "cid-env2")
}
