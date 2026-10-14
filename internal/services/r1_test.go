package services

import (
	"context"
	"errors"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (m *Manager) rowState(t *testing.T, id string) (status, cid string, circuit int, lrc *int) {
	t.Helper()
	require.NoError(t, m.db.QueryRow(`SELECT status, container_id, circuit_open, last_restart_count FROM services WHERE id = ?`, id).
		Scan(&status, &cid, &circuit, &lrc))
	return
}

// Restart must mark the row 'restarting' before the old container is stopped, so
// the reconciler (which only watches running/crashed rows) cannot count the stop as a crash.
func TestRestart_RowRestartingBeforeStop(t *testing.T) {
	mock := &testutil.MockDockerClient{}
	mgr, _, svc := newTestMgr(t, mock, "")
	ctx := context.Background()
	require.NoError(t, mgr.Deploy(ctx, "tenant-1", svc.ID))

	var statusAtStop string
	mock.StopContainerFn = func(ctx context.Context, id string) error {
		_ = mgr.db.QueryRow(`SELECT status FROM services WHERE id = ?`, svc.ID).Scan(&statusAtStop)
		return nil
	}
	require.NoError(t, mgr.Restart(ctx, "tenant-1", svc.ID))
	assert.Equal(t, "restarting", statusAtStop)
}

// If the breaker opens while a restart is creating the new container, the restart
// must not overwrite it with running.
func TestRestart_DoesNotOverrideOpenCircuit(t *testing.T) {
	mock := &testutil.MockDockerClient{}
	mgr, _, svc := newTestMgr(t, mock, "")
	ctx := context.Background()
	require.NoError(t, mgr.Deploy(ctx, "tenant-1", svc.ID))
	mock.RunContainerFn = func(ctx context.Context, tenantID, serviceID, img string, port int, env map[string]string, labels map[string]string, l *docker.ResourceLimits) (string, error) {
		_, err := mgr.db.Exec(`UPDATE services SET circuit_open = 1, status = 'crashed' WHERE id = ?`, serviceID)
		require.NoError(t, err)
		return "new-cid", nil
	}
	require.Error(t, mgr.Restart(ctx, "tenant-1", svc.ID))
	status, cid, circuit, _ := mgr.rowState(t, svc.ID)
	assert.Equal(t, 1, circuit)
	assert.NotEqual(t, "running", status)
	assert.NotEqual(t, "new-cid", cid)
	assert.Contains(t, mock.RemoveContainerCalls, "new-cid")
}

func TestDeploy_DoesNotOverrideOpenCircuit(t *testing.T) {
	mock := &testutil.MockDockerClient{}
	mgr, _, svc := newTestMgr(t, mock, "")
	mock.RunContainerFn = func(ctx context.Context, tenantID, serviceID, img string, port int, env map[string]string, labels map[string]string, l *docker.ResourceLimits) (string, error) {
		_, err := mgr.db.Exec(`UPDATE services SET circuit_open = 1, status = 'crashed' WHERE id = ?`, serviceID)
		require.NoError(t, err)
		return "new-cid", nil
	}
	require.Error(t, mgr.Deploy(context.Background(), "tenant-1", svc.ID))
	status, _, circuit, _ := mgr.rowState(t, svc.ID)
	assert.Equal(t, 1, circuit)
	assert.NotEqual(t, "running", status)
	assert.Contains(t, mock.RemoveContainerCalls, "new-cid")
}

func TestDeployImage_DoesNotOverrideOpenCircuit(t *testing.T) {
	mock := &testutil.MockDockerClient{}
	mgr, _, svc := newTestMgr(t, mock, "")
	mock.RunContainerFn = func(ctx context.Context, tenantID, serviceID, img string, port int, env map[string]string, labels map[string]string, l *docker.ResourceLimits) (string, error) {
		_, err := mgr.db.Exec(`UPDATE services SET circuit_open = 1, status = 'crashed' WHERE id = ?`, serviceID)
		require.NoError(t, err)
		return "new-cid", nil
	}
	require.Error(t, mgr.DeployImage(context.Background(), "tenant-1", svc.ID, "img:2", ""))
	status, _, circuit, _ := mgr.rowState(t, svc.ID)
	assert.Equal(t, 1, circuit)
	assert.NotEqual(t, "running", status)
	assert.Contains(t, mock.RemoveContainerCalls, "new-cid")
}

// A new container_id must not inherit the old container's restart-count baseline.
func TestNewContainerResetsLastRestartCount(t *testing.T) {
	mock := &testutil.MockDockerClient{}
	mgr, _, svc := newTestMgr(t, mock, "")
	ctx := context.Background()
	set := func() {
		_, err := mgr.db.Exec(`UPDATE services SET last_restart_count = 7 WHERE id = ?`, svc.ID)
		require.NoError(t, err)
	}
	for name, fn := range map[string]func() error{
		"deploy":      func() error { return mgr.Deploy(ctx, "tenant-1", svc.ID) },
		"restart":     func() error { return mgr.Restart(ctx, "tenant-1", svc.ID) },
		"deployimage": func() error { return mgr.DeployImage(ctx, "tenant-1", svc.ID, "img:2", "") },
	} {
		if name == "restart" {
			require.NoError(t, mgr.Deploy(ctx, "tenant-1", svc.ID))
		}
		set()
		require.NoError(t, fn(), name)
		_, _, _, lrc := mgr.rowState(t, svc.ID)
		assert.Nil(t, lrc, name)
	}
}

// Delete must not remove the row while the container may still exist.
func TestDelete_ContainerRemovalFailureKeepsRow(t *testing.T) {
	mock := &testutil.MockDockerClient{}
	mgr, _, svc := newTestMgr(t, mock, "")
	ctx := context.Background()
	require.NoError(t, mgr.Deploy(ctx, "tenant-1", svc.ID))
	mock.RemoveContainerFn = func(ctx context.Context, id string) error { return errors.New("device busy") }
	mock.StopAndRemoveByNameFn = func(ctx context.Context, name string) error { return errors.New("device busy") }

	require.Error(t, mgr.Delete(ctx, "tenant-1", svc.ID))
	_, err := mgr.Get(ctx, "tenant-1", svc.ID)
	assert.NoError(t, err, "row must remain so the delete can be retried")

	// Once the container is really gone, delete succeeds.
	mock.InspectContainerFn = func(ctx context.Context, id string) (*docker.ContainerInfo, error) {
		return nil, testutil.NewNotFoundError(id)
	}
	require.NoError(t, mgr.Delete(ctx, "tenant-1", svc.ID))
}
