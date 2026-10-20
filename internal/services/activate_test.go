package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

// withDatabaseEnv gives svc a database reference so attachDatabaseNetworks
// calls ConnectNetwork (the mock hook the tests below use).
func withDatabaseEnv(t *testing.T, mgr *Manager, svcID string) {
	t.Helper()
	_, err := mgr.db.Exec(`INSERT INTO databases (id, tenant_id, name, type, status, password_encrypted, created_at, updated_at)
		VALUES ('db1', 'tenant-1', 'db', 'postgres', 'running', 'p', 1, 1)`)
	require.NoError(t, err)
	require.NoError(t, mgr.SetEnv(context.Background(), "tenant-1", svcID,
		map[string]string{"DATABASE_URL": "postgres://" + docker.DatabaseAlias("db1") + "/d"}))
}

// The deploy's context expires between starting the container and the final
// "running" write. The write fails, and the failure writes must not reuse the
// dead context, or both rows stay 'deploying' forever.
func TestActivateContainer_ContextExpiresBeforeFinalWrite_MarksRowsFailed(t *testing.T) {
	mock := &testutil.MockDockerClient{}
	mgr, _, svc := newTestMgr(t, mock, "")
	withDatabaseEnv(t, mgr, svc.ID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mock.ConnectNetworkFn = func(context.Context, string, string) error {
		cancel() // the request deadline passes right after the network attach
		return nil
	}

	require.Error(t, mgr.Deploy(ctx, "tenant-1", svc.ID))

	status, cid, lastErr := rowState(t, mgr, svc.ID)
	assert.Equal(t, "failed", status, "service row must not stay 'deploying'")
	assert.Empty(t, cid)
	assert.Contains(t, lastErr, "db update failed")
	assert.Equal(t, deployments.StatusFailed, latestDeployment(t, mgr, svc.ID).Status, "deployment row must not stay 'deploying'")
	assert.Contains(t, mock.RemoveContainerCalls, "mock-container-"+svc.ID, "the new container is discarded")
}

// A DB error on the final write (not a context problem) also marks the
// service failed, not only the deployment row.
func TestActivateContainer_DBError_MarksServiceFailed(t *testing.T) {
	mock := &testutil.MockDockerClient{}
	mgr, _, svc := newTestMgr(t, mock, "")
	_, err := mgr.db.Exec(`CREATE TRIGGER fail_running BEFORE UPDATE OF container_id ON services
		WHEN NEW.status = 'running' AND NEW.container_id != '' BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`)
	require.NoError(t, err)

	err = mgr.DeployImage(context.Background(), "tenant-1", svc.ID, "img", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "update service")

	status, cid, lastErr := rowState(t, mgr, svc.ID)
	assert.Equal(t, "failed", status)
	assert.Empty(t, cid)
	assert.Contains(t, lastErr, "db update failed")
	assert.Equal(t, deployments.StatusFailed, latestDeployment(t, mgr, svc.ID).Status)
	assert.Contains(t, mock.RemoveContainerCalls, "mock-container-"+svc.ID)
}

// When the final write matches no row (circuit opened meanwhile), the
// deployment is failed but the service row is left to whoever changed it.
func TestActivateContainer_CircuitOpenedDuringDeploy_LeavesServiceRow(t *testing.T) {
	var mgr *Manager
	mock := &testutil.MockDockerClient{RunContainerFn: func(ctx context.Context, tenantID, serviceID, img string, port int, env, labels map[string]string, l *docker.ResourceLimits) (string, error) {
		_, err := mgr.db.Exec(`UPDATE services SET circuit_open = 1, status = 'crashed' WHERE id = ?`, serviceID)
		return "new-ctr", err
	}}
	mgr, _, svc := newTestMgr(t, mock, "")

	err := mgr.Deploy(context.Background(), "tenant-1", svc.ID)
	require.Error(t, err)
	assert.ErrorIs(t, err, errInterrupted)

	status, cid, _ := rowState(t, mgr, svc.ID)
	assert.Equal(t, "crashed", status, "the circuit breaker's status is kept")
	assert.Empty(t, cid)
	assert.Contains(t, mock.RemoveContainerCalls, "new-ctr")
	d := latestDeployment(t, mgr, svc.ID)
	assert.Equal(t, deployments.StatusFailed, d.Status)
	assert.Equal(t, errDeployInterrupted, d.ErrorMessage)
}

// A tenant suspended while Restart's new container starts: the container is
// discarded, its route dropped, and the 'stopped' status written by
// StopAllForTenant is not overwritten with 'failed'.
func TestRestart_TenantSuspendedDuringRun_KeepsSuspensionStatus(t *testing.T) {
	dir := t.TempDir()
	mock := &testutil.MockDockerClient{}
	mgr, _, svc := deployedSvc(t, mock, dir)
	mock.RunContainerFn = func(ctx context.Context, tenantID, serviceID, img string, port int, env, labels map[string]string, l *docker.ResourceLimits) (string, error) {
		// Simulate the suspension (and StopAllForTenant's row write) landing now.
		if _, err := mgr.db.Exec(`UPDATE tenants SET status = 'suspended' WHERE id = 'tenant-1'`); err != nil {
			return "", err
		}
		_, err := mgr.db.Exec(`UPDATE services SET status = 'stopped' WHERE id = ?`, serviceID)
		return "restarted-ctr", err
	}

	err := mgr.Restart(context.Background(), "tenant-1", svc.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "during restart")

	status, cid, _ := rowState(t, mgr, svc.ID)
	assert.Equal(t, "stopped", status)
	assert.Empty(t, cid)
	assert.Contains(t, mock.RemoveContainerCalls, "restarted-ctr")
	_, statErr := os.Stat(filepath.Join(dir, svc.ID+".yml"))
	assert.True(t, os.IsNotExist(statErr), "route to the discarded container is removed")
	assert.Equal(t, deployments.StatusFailed, latestDeployment(t, mgr, svc.ID).Status)
}

// DeployImage re-checks the tenant right before creating the container.
func TestDeployImage_TenantSuspendedBeforeRun_NoContainer(t *testing.T) {
	var mgr *Manager
	mock := &testutil.MockDockerClient{EnsureNetworkFn: func(context.Context, string) (string, error) {
		_, err := mgr.db.Exec(`UPDATE tenants SET status = 'suspended' WHERE id = 'tenant-1'`)
		return "net", err
	}}
	mgr, _, svc := newTestMgr(t, mock, "")

	err := mgr.DeployImage(context.Background(), "tenant-1", svc.ID, "img", "")
	assert.ErrorIs(t, err, apierr.ErrForbidden)
	assert.Equal(t, 0, mock.RunContainerCalls)
	assert.Equal(t, deployments.StatusFailed, latestDeployment(t, mgr, svc.ID).Status)
}

// Start's rollback stop must be bounded even though it ignores the caller's
// cancellation, so a hung Docker API cannot pin the request goroutine.
func TestStart_RollbackStopHasDeadline(t *testing.T) {
	mock := &testutil.MockDockerClient{}
	mgr, _, svc := deployedSvc(t, mock, "")
	require.NoError(t, mgr.Stop(context.Background(), "tenant-1", svc.ID))

	var mu sync.Mutex
	var deadline time.Time
	var hasDeadline bool
	mock.EnsureIngressNetworkFn = func(context.Context, string) (string, error) { return "", errors.New("no network") }
	mock.StopContainerFn = func(ctx context.Context, id string) error {
		mu.Lock()
		defer mu.Unlock()
		deadline, hasDeadline = ctx.Deadline()
		return nil
	}

	require.Error(t, mgr.Start(context.Background(), "tenant-1", svc.ID))
	mu.Lock()
	defer mu.Unlock()
	require.True(t, hasDeadline, "rollback stop must carry a deadline")
	assert.WithinDuration(t, time.Now().Add(discardTimeout), deadline, 5*time.Second)
}
