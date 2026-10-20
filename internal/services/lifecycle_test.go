package services

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/deployments"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deployedSvc creates a service and deploys it, returning the manager, store and
// the deployed service row.
func deployedSvc(t *testing.T, mock *testutil.MockDockerClient, traefikDir string) (*Manager, *deployments.Store, *Service) {
	t.Helper()
	mgr, store, svc := newTestMgr(t, mock, traefikDir)
	require.NoError(t, mgr.Deploy(context.Background(), "tenant-1", svc.ID))
	got, err := mgr.getOwned(context.Background(), "tenant-1", svc.ID)
	require.NoError(t, err)
	require.NotEmpty(t, got.ContainerID)
	return mgr, store, got
}

func rowState(t *testing.T, mgr *Manager, id string) (status, containerID, lastError string) {
	t.Helper()
	require.NoError(t, mgr.db.QueryRow(`SELECT status, COALESCE(container_id, ''), COALESCE(last_error, '') FROM services WHERE id = ?`, id).
		Scan(&status, &containerID, &lastError))
	return
}

// latestDeployment returns the most recently inserted deployment row. Rows
// created within the same second tie on created_at, so order by rowid.
func latestDeployment(t *testing.T, mgr *Manager, svcID string) *deployments.Deployment {
	t.Helper()
	var id string
	require.NoError(t, mgr.db.QueryRow(`SELECT id FROM deployments WHERE service_id = ? ORDER BY rowid DESC LIMIT 1`, svcID).Scan(&id))
	d, err := mgr.deployStore.Get(context.Background(), "tenant-1", id)
	require.NoError(t, err)
	return d
}

func fillQueue(mgr *Manager) {
	for i := 0; i < cap(mgr.deployQueue); i++ {
		mgr.deployQueue <- struct{}{}
	}
}

func TestStop(t *testing.T) {
	ctx := context.Background()

	t.Run("no container is a conflict", func(t *testing.T) {
		mgr, _, svc := newTestMgr(t, &testutil.MockDockerClient{}, "")
		err := mgr.Stop(ctx, "tenant-1", svc.ID)
		assert.True(t, errors.Is(err, apierr.ErrConflict))
	})

	t.Run("success marks stopped and records it", func(t *testing.T) {
		mgr, _, svc := deployedSvc(t, &testutil.MockDockerClient{}, "")
		require.NoError(t, mgr.Stop(ctx, "tenant-1", svc.ID))
		status, cid, _ := rowState(t, mgr, svc.ID)
		assert.Equal(t, "stopped", status)
		assert.Equal(t, svc.ContainerID, cid, "a stopped container is kept for Start")
		d := latestDeployment(t, mgr, svc.ID)
		assert.Equal(t, deployments.StatusStopped, d.Status)
		assert.NotNil(t, d.CompletedAt)
	})

	t.Run("already stopped is success", func(t *testing.T) {
		mock := &testutil.MockDockerClient{}
		mgr, _, svc := deployedSvc(t, mock, "")
		mock.StopContainerFn = func(context.Context, string) error { return errors.New("container already stopped") }
		require.NoError(t, mgr.Stop(ctx, "tenant-1", svc.ID))
		status, _, _ := rowState(t, mgr, svc.ID)
		assert.Equal(t, "stopped", status)
	})

	t.Run("missing container self-heals", func(t *testing.T) {
		mock := &testutil.MockDockerClient{}
		mgr, _, svc := deployedSvc(t, mock, "")
		mock.StopContainerFn = func(_ context.Context, id string) error { return testutil.NewNotFoundError(id) }
		require.NoError(t, mgr.Stop(ctx, "tenant-1", svc.ID))
		status, cid, _ := rowState(t, mgr, svc.ID)
		assert.Equal(t, "stopped", status)
		assert.Empty(t, cid)
	})

	t.Run("other docker errors are returned", func(t *testing.T) {
		mock := &testutil.MockDockerClient{}
		mgr, _, svc := deployedSvc(t, mock, "")
		mock.StopContainerFn = func(context.Context, string) error { return errors.New("daemon down") }
		assert.Error(t, mgr.Stop(ctx, "tenant-1", svc.ID))
		status, _, _ := rowState(t, mgr, svc.ID)
		assert.Equal(t, "running", status)
	})

	t.Run("other tenant cannot stop", func(t *testing.T) {
		mgr, _, svc := deployedSvc(t, &testutil.MockDockerClient{}, "")
		err := mgr.Stop(ctx, "tenant-2", svc.ID)
		assert.True(t, errors.Is(err, apierr.ErrNotFound))
	})
}

func TestStart(t *testing.T) {
	ctx := context.Background()

	t.Run("success restores route and records running", func(t *testing.T) {
		dir := t.TempDir()
		mgr, _, svc := deployedSvc(t, &testutil.MockDockerClient{}, dir)
		require.NoError(t, mgr.Stop(ctx, "tenant-1", svc.ID))
		require.NoError(t, os.Remove(filepath.Join(dir, svc.ID+".yml")))

		require.NoError(t, mgr.Start(ctx, "tenant-1", svc.ID))
		status, _, _ := rowState(t, mgr, svc.ID)
		assert.Equal(t, "running", status)
		_, err := os.Stat(filepath.Join(dir, svc.ID+".yml"))
		assert.NoError(t, err, "route is rewritten on start")
		assert.Equal(t, deployments.StatusRunning, latestDeployment(t, mgr, svc.ID).Status)
	})

	t.Run("restore failure stops the container again", func(t *testing.T) {
		mock := &testutil.MockDockerClient{}
		mgr, _, svc := deployedSvc(t, mock, "")
		require.NoError(t, mgr.Stop(ctx, "tenant-1", svc.ID))
		mock.EnsureIngressNetworkFn = func(context.Context, string) (string, error) { return "", errors.New("no network") }
		stopsBefore := len(mock.StopContainerCalls)

		err := mgr.Start(ctx, "tenant-1", svc.ID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "restore service ingress network")
		assert.Len(t, mock.StopContainerCalls, stopsBefore+1, "an unreachable container must not keep running")
		status, _, _ := rowState(t, mgr, svc.ID)
		assert.Equal(t, "stopped", status)
	})

	t.Run("inspect failure stops the container again", func(t *testing.T) {
		mock := &testutil.MockDockerClient{}
		mgr, _, svc := deployedSvc(t, mock, "")
		mock.InspectContainerFn = func(context.Context, string) (*docker.ContainerInfo, error) { return nil, errors.New("boom") }
		err := mgr.Start(ctx, "tenant-1", svc.ID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "inspect started container")
	})

	t.Run("missing container clears row", func(t *testing.T) {
		mock := &testutil.MockDockerClient{}
		mgr, _, svc := deployedSvc(t, mock, "")
		mock.StartContainerFn = func(_ context.Context, id string) error { return testutil.NewNotFoundError(id) }
		err := mgr.Start(ctx, "tenant-1", svc.ID)
		assert.True(t, errors.Is(err, apierr.ErrConflict))
		_, cid, _ := rowState(t, mgr, svc.ID)
		assert.Empty(t, cid)
	})

	t.Run("start error is returned", func(t *testing.T) {
		mock := &testutil.MockDockerClient{}
		mgr, _, svc := deployedSvc(t, mock, "")
		mock.StartContainerFn = func(context.Context, string) error { return errors.New("boom") }
		assert.Error(t, mgr.Start(ctx, "tenant-1", svc.ID))
	})

	t.Run("no container and open circuit are conflicts", func(t *testing.T) {
		mgr, _, svc := newTestMgr(t, &testutil.MockDockerClient{}, "")
		assert.True(t, errors.Is(mgr.Start(ctx, "tenant-1", svc.ID), apierr.ErrConflict))
		_, err := mgr.db.Exec(`UPDATE services SET circuit_open = 1 WHERE id = ?`, svc.ID)
		require.NoError(t, err)
		assert.True(t, errors.Is(mgr.Start(ctx, "tenant-1", svc.ID), apierr.ErrConflict))
	})

	t.Run("suspended tenant is forbidden", func(t *testing.T) {
		mgr, _, svc := newTestMgr(t, &testutil.MockDockerClient{}, "")
		_, err := mgr.db.Exec(`UPDATE tenants SET status = 'suspended' WHERE id = 'tenant-1'`)
		require.NoError(t, err)
		assert.True(t, errors.Is(mgr.Start(ctx, "tenant-1", svc.ID), apierr.ErrForbidden))
	})
}

// A full deploy queue is retryable backpressure: Restart, ResetCircuitBreaker
// and DeployImage answer 503 + Retry-After, while Deploy keeps the 409 it has
// always returned (existing API contract). None of them may surface as a 500.
func TestQueueFullStatusCodes(t *testing.T) {
	ctx := context.Background()
	mgr, _, svc := deployedSvc(t, &testutil.MockDockerClient{}, "")
	fillQueue(mgr)

	for name, fn := range map[string]func() error{
		"restart":     func() error { return mgr.Restart(ctx, "tenant-1", svc.ID) },
		"reset":       func() error { return mgr.ResetCircuitBreaker(ctx, "tenant-1", svc.ID) },
		"deployImage": func() error { return mgr.DeployImage(ctx, "tenant-1", svc.ID, "img", "") },
	} {
		err := fn()
		var apiErr *apierr.APIError
		require.True(t, errors.As(err, &apiErr), "%s: queue full must be an API error, not a 500: %v", name, err)
		assert.Equal(t, http.StatusServiceUnavailable, apiErr.Code, name)
		assert.ErrorIs(t, err, apierr.ErrUnavailable, name)
	}
	status, _, _ := rowState(t, mgr, svc.ID)
	assert.Equal(t, "running", status, "503 backpressure must not touch the service row")

	err := mgr.Deploy(ctx, "tenant-1", svc.ID)
	assert.True(t, errors.Is(err, apierr.ErrConflict), "deploy: queue full stays 409: %v", err)
	_, _, lastErr := rowState(t, mgr, svc.ID)
	assert.Contains(t, lastErr, "deploy queue full")
}

func TestAcquireDeploySlot_CancelledWhileWaitingReleasesQueue(t *testing.T) {
	mgr, _, svc := newTestMgr(t, &testutil.MockDockerClient{}, "")
	for i := 0; i < cap(mgr.deploySem); i++ {
		mgr.deploySem <- struct{}{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Deploy(ctx, "tenant-1", svc.ID) }()
	require.Eventually(t, func() bool { return len(mgr.deployQueue) == 1 }, 2*time.Second, time.Millisecond, "deploy is queued waiting for a slot")
	cancel()

	err := <-done
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 0, len(mgr.deployQueue), "queue slot is released when the wait is abandoned")
	status, _, _ := rowState(t, mgr, svc.ID)
	assert.Equal(t, "created", status, "a cancelled caller does not mark the service failed")
}

func TestDeploy_FailureRecordedEvenAfterDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mock := &testutil.MockDockerClient{
		RunContainerFn: func(context.Context, string, string, string, int, map[string]string, map[string]string, *docker.ResourceLimits) (string, error) {
			cancel() // the deploy deadline passes while the container is being created
			return "", context.Canceled
		},
	}
	mgr, _, svc := newTestMgr(t, mock, "")

	require.Error(t, mgr.Deploy(ctx, "tenant-1", svc.ID))
	status, _, lastErr := rowState(t, mgr, svc.ID)
	assert.Equal(t, "failed", status, "service must not be left 'deploying'")
	assert.Contains(t, lastErr, "container start failed")
	assert.Equal(t, deployments.StatusFailed, latestDeployment(t, mgr, svc.ID).Status)
}

func TestDeploy_OrphanDiscardedWithLiveContextWhenCircuitOpens(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var removedWithLiveCtx []string
	mock := &testutil.MockDockerClient{
		RemoveContainerFn: func(c context.Context, id string) error {
			mu.Lock()
			defer mu.Unlock()
			if c.Err() == nil {
				removedWithLiveCtx = append(removedWithLiveCtx, id)
			}
			return nil
		},
	}
	mgr, _, svc := newTestMgr(t, mock, "")
	mock.RunContainerFn = func(context.Context, string, string, string, int, map[string]string, map[string]string, *docker.ResourceLimits) (string, error) {
		_, err := mgr.db.Exec(`UPDATE services SET circuit_open = 1 WHERE id = ?`, svc.ID)
		require.NoError(t, err)
		cancel()
		return "new-cid", nil
	}

	err := mgr.Deploy(ctx, "tenant-1", svc.ID)
	require.Error(t, err)
	assert.Equal(t, []string{"new-cid"}, removedWithLiveCtx, "orphan cleanup must not reuse the expired deploy context")
	assert.Equal(t, deployments.StatusFailed, latestDeployment(t, mgr, svc.ID).Status)
}

func TestDeploy_ReplacesExistingContainer(t *testing.T) {
	mock := &testutil.MockDockerClient{}
	mgr, _, svc := deployedSvc(t, mock, "")
	n := 0
	mock.RunContainerFn = func(context.Context, string, string, string, int, map[string]string, map[string]string, *docker.ResourceLimits) (string, error) {
		n++
		return "second-cid", nil
	}
	require.NoError(t, mgr.Deploy(context.Background(), "tenant-1", svc.ID))
	assert.Contains(t, mock.RemoveContainerCalls, svc.ContainerID)
	_, cid, _ := rowState(t, mgr, svc.ID)
	assert.Equal(t, "second-cid", cid)
	d := latestDeployment(t, mgr, svc.ID)
	assert.Equal(t, deployments.StatusRunning, d.Status)
	assert.Equal(t, "second-cid", d.ContainerID)
}

func TestDeploy_OldContainerThatCannotBeRemovedFailsDeploy(t *testing.T) {
	mock := &testutil.MockDockerClient{}
	mgr, _, svc := deployedSvc(t, mock, "")
	mock.RemoveContainerFn = func(context.Context, string) error { return errors.New("device busy") }
	mock.StopAndRemoveByNameFn = func(context.Context, string) error { return errors.New("device busy") }
	runs := mock.RunContainerCalls

	err := mgr.Deploy(context.Background(), "tenant-1", svc.ID)
	require.Error(t, err)
	assert.Equal(t, runs, mock.RunContainerCalls, "no second container is started next to the old one")
	status, cid, _ := rowState(t, mgr, svc.ID)
	assert.Equal(t, "failed", status)
	assert.Equal(t, svc.ContainerID, cid, "the surviving container stays tracked")
	assert.Equal(t, deployments.StatusFailed, latestDeployment(t, mgr, svc.ID).Status)
}

func TestDeployImage(t *testing.T) {
	ctx := context.Background()

	t.Run("success switches image and records build", func(t *testing.T) {
		dir := t.TempDir()
		mgr, _, svc := deployedSvc(t, &testutil.MockDockerClient{}, dir)
		img := "127.0.0.1:5000/ah/tenant-1-" + svc.ID + ":b1"
		require.NoError(t, mgr.DeployImage(ctx, "tenant-1", svc.ID, img, ""))
		got, err := mgr.getOwned(ctx, "tenant-1", svc.ID)
		require.NoError(t, err)
		assert.Equal(t, img, got.Image)
		assert.Equal(t, "running", got.Status)
		d := latestDeployment(t, mgr, svc.ID)
		assert.Equal(t, deployments.TriggerBuild, d.Trigger)
		assert.Equal(t, deployments.StatusRunning, d.Status)
		_, err = os.Stat(filepath.Join(dir, svc.ID+".yml"))
		assert.NoError(t, err)
	})

	t.Run("run failure after removing old container clears container_id", func(t *testing.T) {
		mock := &testutil.MockDockerClient{}
		mgr, _, svc := deployedSvc(t, mock, "")
		mock.RunContainerFn = func(context.Context, string, string, string, int, map[string]string, map[string]string, *docker.ResourceLimits) (string, error) {
			return "", errors.New("runsc failed")
		}
		require.Error(t, mgr.DeployImage(ctx, "tenant-1", svc.ID, "img", ""))
		status, cid, lastErr := rowState(t, mgr, svc.ID)
		assert.Equal(t, "failed", status)
		assert.Empty(t, cid, "row must not point at the container that was just removed")
		assert.Contains(t, lastErr, "container start failed")
		assert.Equal(t, deployments.StatusFailed, latestDeployment(t, mgr, svc.ID).Status)
	})

	t.Run("network failure marks failed", func(t *testing.T) {
		mock := &testutil.MockDockerClient{}
		mgr, _, svc := newTestMgr(t, mock, "")
		mock.EnsureNetworkFn = func(context.Context, string) (string, error) { return "", errors.New("no net") }
		require.Error(t, mgr.DeployImage(ctx, "tenant-1", svc.ID, "img", ""))
		status, _, lastErr := rowState(t, mgr, svc.ID)
		assert.Equal(t, "failed", status)
		assert.Contains(t, lastErr, "network setup failed")
	})

	t.Run("database attach failure discards new container", func(t *testing.T) {
		mock := &testutil.MockDockerClient{}
		mgr, _, svc := newTestMgr(t, mock, "")
		_, err := mgr.db.Exec(`INSERT INTO databases (id, tenant_id, name, type, status, password_encrypted, created_at, updated_at)
			VALUES ('db1', 'tenant-1', 'db', 'postgres', 'running', 'p', 1, 1)`)
		require.NoError(t, err)
		require.NoError(t, mgr.SetEnv(ctx, "tenant-1", svc.ID, map[string]string{"DATABASE_URL": "postgres://" + docker.DatabaseAlias("db1") + "/d"}))
		mock.ConnectNetworkFn = func(context.Context, string, string) error { return errors.New("attach failed") }

		require.Error(t, mgr.DeployImage(ctx, "tenant-1", svc.ID, "img", ""))
		assert.Contains(t, mock.RemoveContainerCalls, "mock-container-"+svc.ID)
		status, cid, _ := rowState(t, mgr, svc.ID)
		assert.Equal(t, "failed", status)
		assert.Empty(t, cid)
	})

	t.Run("missing service and inactive tenant", func(t *testing.T) {
		mgr, _, _ := newTestMgr(t, &testutil.MockDockerClient{}, "")
		assert.True(t, errors.Is(mgr.DeployImage(ctx, "tenant-1", "nope", "img", ""), apierr.ErrNotFound))
		assert.True(t, errors.Is(mgr.DeployImage(ctx, "tenant-2", "nope", "img", ""), apierr.ErrForbidden))
	})
}

func TestResetCircuitBreaker(t *testing.T) {
	ctx := context.Background()
	openCircuit := func(t *testing.T, mgr *Manager, id string) {
		_, err := mgr.db.Exec(`UPDATE services SET circuit_open = 1, crash_count = 5, status = 'crashed' WHERE id = ?`, id)
		require.NoError(t, err)
	}

	t.Run("success clears breaker and container", func(t *testing.T) {
		dir := t.TempDir()
		mgr, _, svc := deployedSvc(t, &testutil.MockDockerClient{}, dir)
		openCircuit(t, mgr, svc.ID)
		require.NoError(t, mgr.ResetCircuitBreaker(ctx, "tenant-1", svc.ID))
		got, err := mgr.getOwned(ctx, "tenant-1", svc.ID)
		require.NoError(t, err)
		assert.False(t, got.CircuitOpen)
		assert.Equal(t, 0, got.CrashCount)
		assert.Equal(t, "stopped", got.Status)
		assert.Empty(t, got.ContainerID)
		_, err = os.Stat(filepath.Join(dir, svc.ID+".yml"))
		assert.True(t, os.IsNotExist(err), "route to the removed container is deleted")
	})

	t.Run("container that cannot be stopped keeps breaker open", func(t *testing.T) {
		mock := &testutil.MockDockerClient{}
		mgr, _, svc := deployedSvc(t, mock, "")
		openCircuit(t, mgr, svc.ID)
		mock.StopContainerFn = func(context.Context, string) error { return errors.New("timeout") }
		mock.InspectContainerFn = func(context.Context, string) (*docker.ContainerInfo, error) {
			return &docker.ContainerInfo{Status: "running"}, nil
		}
		require.Error(t, mgr.ResetCircuitBreaker(ctx, "tenant-1", svc.ID))
		got, _ := mgr.getOwned(ctx, "tenant-1", svc.ID)
		assert.True(t, got.CircuitOpen)
	})

	t.Run("unverifiable state after stop failure fails closed", func(t *testing.T) {
		mock := &testutil.MockDockerClient{}
		mgr, _, svc := deployedSvc(t, mock, "")
		mock.StopContainerFn = func(context.Context, string) error { return errors.New("timeout") }
		mock.InspectContainerFn = func(context.Context, string) (*docker.ContainerInfo, error) { return nil, errors.New("daemon down") }
		err := mgr.ResetCircuitBreaker(ctx, "tenant-1", svc.ID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot verify container state")
	})

	t.Run("container that survives removal keeps breaker open", func(t *testing.T) {
		mock := &testutil.MockDockerClient{}
		mgr, _, svc := deployedSvc(t, mock, "")
		openCircuit(t, mgr, svc.ID)
		mock.RemoveContainerFn = func(context.Context, string) error { return errors.New("busy") }
		err := mgr.ResetCircuitBreaker(ctx, "tenant-1", svc.ID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to remove container during reset")
	})

	t.Run("stopped-but-present container and gone-after-remove-error both proceed", func(t *testing.T) {
		mock := &testutil.MockDockerClient{}
		mgr, _, svc := deployedSvc(t, mock, "")
		mock.StopContainerFn = func(context.Context, string) error { return errors.New("timeout") }
		inspects := 0
		mock.InspectContainerFn = func(_ context.Context, id string) (*docker.ContainerInfo, error) {
			inspects++
			if inspects == 1 {
				return &docker.ContainerInfo{Status: "exited"}, nil
			}
			return nil, testutil.NewNotFoundError(id)
		}
		mock.RemoveContainerFn = func(context.Context, string) error { return errors.New("racing removal") }
		require.NoError(t, mgr.ResetCircuitBreaker(ctx, "tenant-1", svc.ID))
	})

	t.Run("orphan found by name must be confirmed gone", func(t *testing.T) {
		mock := &testutil.MockDockerClient{}
		mgr, _, svc := newTestMgr(t, mock, "")
		mock.StopAndRemoveByNameFn = func(context.Context, string) error { return errors.New("busy") }
		err := mgr.ResetCircuitBreaker(ctx, "tenant-1", svc.ID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "container still exists")

		mock.InspectContainerFn = func(context.Context, string) (*docker.ContainerInfo, error) { return nil, errors.New("daemon down") }
		err = mgr.ResetCircuitBreaker(ctx, "tenant-1", svc.ID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot confirm container absence")

		mock.InspectContainerFn = func(_ context.Context, id string) (*docker.ContainerInfo, error) {
			return nil, testutil.NewNotFoundError(id)
		}
		assert.NoError(t, mgr.ResetCircuitBreaker(ctx, "tenant-1", svc.ID))
	})

	t.Run("missing container is fine", func(t *testing.T) {
		mock := &testutil.MockDockerClient{}
		mgr, _, svc := deployedSvc(t, mock, "")
		mock.StopContainerFn = func(_ context.Context, id string) error { return testutil.NewNotFoundError(id) }
		assert.NoError(t, mgr.ResetCircuitBreaker(ctx, "tenant-1", svc.ID))
	})
}

func TestStopAllForTenant_FailedCleanupMarksFailed(t *testing.T) {
	mock := &testutil.MockDockerClient{}
	mgr, _, svc := deployedSvc(t, mock, "")
	other, err := mgr.Create(context.Background(), "tenant-1", CreateRequest{Name: "idle", Image: "nginx:latest"})
	require.NoError(t, err)
	mock.RemoveContainerFn = func(context.Context, string) error { return errors.New("busy") }

	mgr.StopAllForTenant(context.Background(), "tenant-1")
	status, cid, lastErr := rowState(t, mgr, svc.ID)
	assert.Equal(t, "failed", status)
	assert.Equal(t, svc.ContainerID, cid)
	assert.Contains(t, lastErr, "tenant suspension")
	status, _, _ = rowState(t, mgr, other.ID)
	assert.Equal(t, "stopped", status)
}

func TestLogsListAndEnv(t *testing.T) {
	ctx := context.Background()
	mock := &testutil.MockDockerClient{LogsContainerFn: func(context.Context, string, bool, int) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("hello")), nil
	}}
	mgr, _, svc := newTestMgr(t, mock, "")

	_, err := mgr.Logs(ctx, "tenant-1", svc.ID, false, 10)
	assert.True(t, errors.Is(err, apierr.ErrConflict), "no container, no logs")
	require.NoError(t, mgr.Deploy(ctx, "tenant-1", svc.ID))
	rc, err := mgr.Logs(ctx, "tenant-1", svc.ID, false, 10)
	require.NoError(t, err)
	b, _ := io.ReadAll(rc)
	assert.Equal(t, "hello", string(b))
	_, err = mgr.Logs(ctx, "tenant-2", svc.ID, false, 10)
	assert.True(t, errors.Is(err, apierr.ErrNotFound))

	list, err := mgr.List(ctx, "tenant-1")
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, svc.ID, list[0].ID)

	require.NoError(t, mgr.SetEnv(ctx, "tenant-1", svc.ID, map[string]string{"B": "2", "A": "1"}))
	assert.Error(t, mgr.SetEnv(ctx, "tenant-1", svc.ID, map[string]string{"PATH": "/x"}))
	masked, err := mgr.GetEnv(ctx, "tenant-1", svc.ID, false)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"A": "********", "B": "********"}, masked)
	revealed, err := mgr.GetEnv(ctx, "tenant-1", svc.ID, true)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"A": "1", "B": "2"}, revealed)
	_, err = mgr.GetEnv(ctx, "tenant-2", svc.ID, true)
	assert.True(t, errors.Is(err, apierr.ErrNotFound), "env is tenant-scoped")

	require.NoError(t, mgr.DeleteEnv(ctx, "tenant-1", svc.ID, "A"))
	assert.True(t, errors.Is(mgr.DeleteEnv(ctx, "tenant-1", svc.ID, "A"), apierr.ErrNotFound))
	assert.True(t, errors.Is(mgr.DeleteEnv(ctx, "tenant-2", svc.ID, "B"), apierr.ErrNotFound))
}

func TestDNSLabelRules(t *testing.T) {
	ctx := context.Background()
	mgr, _, svc := newTestMgr(t, &testutil.MockDockerClient{}, "")

	for _, name := range []string{"api", "env-preview", "!!!", strings.Repeat("a", 64)} {
		_, err := mgr.Create(ctx, "tenant-1", CreateRequest{Name: name, Image: "nginx:latest"})
		assert.True(t, errors.Is(err, apierr.ErrValidation), "name %q must be rejected", name)
		_, err = mgr.UpdateName(ctx, "tenant-1", svc.ID, name)
		assert.True(t, errors.Is(err, apierr.ErrValidation), "rename to %q must be rejected", name)
	}

	other, err := mgr.Create(ctx, "tenant-1", CreateRequest{Name: "Other App", Image: "nginx:latest"})
	require.NoError(t, err)
	assert.Equal(t, "other-app", other.DNSLabel)
	_, err = mgr.Create(ctx, "tenant-1", CreateRequest{Name: "other_app", Image: "nginx:latest"})
	assert.True(t, errors.Is(err, apierr.ErrConflict), "same label from a different spelling conflicts")
	_, err = mgr.UpdateName(ctx, "tenant-1", svc.ID, "OTHER-APP")
	assert.True(t, errors.Is(err, apierr.ErrConflict))

	renamed, err := mgr.UpdateName(ctx, "tenant-1", svc.ID, "brand new")
	require.NoError(t, err)
	assert.Equal(t, "brand-new", renamed.DNSLabel)
	assert.Equal(t, "https://brand-new.example.com", renamed.URL)

	// Without a base domain the name is cosmetic and no label is claimed.
	local := NewManager(mgr.db, &testutil.MockDockerClient{}, mgr.masterKey, "", "", nil)
	label, err := local.dnsLabelFor("api")
	require.NoError(t, err)
	assert.Empty(t, label)
}

func TestCreate_RejectsBadInput(t *testing.T) {
	ctx := context.Background()
	mgr, _, _ := newTestMgr(t, &testutil.MockDockerClient{}, "")
	_, err := mgr.Create(ctx, "tenant-1", CreateRequest{Name: "p", Image: "nginx", Port: 70000})
	assert.True(t, errors.Is(err, apierr.ErrValidation))
	_, err = mgr.Create(ctx, "tenant-1", CreateRequest{Name: "e", Image: "nginx", Env: map[string]string{"LD_PRELOAD": "x"}})
	assert.True(t, errors.Is(err, apierr.ErrValidation))
	_, err = mgr.Create(ctx, "tenant-9", CreateRequest{Name: "x", Image: "nginx"})
	assert.True(t, errors.Is(err, apierr.ErrForbidden))

	svc, err := mgr.Create(ctx, "tenant-1", CreateRequest{Name: "withenv", Image: "nginx", Env: map[string]string{"K": "v"}})
	require.NoError(t, err)
	env, err := mgr.GetEnv(ctx, "tenant-1", svc.ID, true)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"K": "v"}, env, "inline env is stored encrypted with the service")
}
