package services

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetEnv_EnforcesMaxEnvVarsPerService(t *testing.T) {
	mgr, _, svc := newTestMgr(t, &testutil.MockDockerClient{}, "")
	ctx := context.Background()
	_, err := mgr.db.Exec(`UPDATE tenant_quotas SET max_env_vars_per_service = 2 WHERE tenant_id = 'tenant-1'`)
	require.NoError(t, err)

	require.NoError(t, mgr.SetEnv(ctx, "tenant-1", svc.ID, map[string]string{"A": "1", "B": "2"}))
	// Overwriting existing keys does not count against the quota.
	require.NoError(t, mgr.SetEnv(ctx, "tenant-1", svc.ID, map[string]string{"A": "x", "B": "y"}))

	err = mgr.SetEnv(ctx, "tenant-1", svc.ID, map[string]string{"C": "3"})
	require.Error(t, err)
	assert.ErrorIs(t, err, apierr.ErrQuotaExceeded)

	keys, err := mgr.GetEnv(ctx, "tenant-1", svc.ID, false)
	require.NoError(t, err)
	assert.Len(t, keys, 2, "rejected request must not insert any key")
}

func TestCreate_EnforcesMaxEnvVarsPerService(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedTenant(t, stateDB)
	_, err := stateDB.Exec(`UPDATE tenant_quotas SET max_env_vars_per_service = 1 WHERE tenant_id = 'tenant-1'`)
	require.NoError(t, err)
	mgr := NewManager(stateDB, &testutil.MockDockerClient{}, []byte("0123456789abcdef0123456789abcdef"), "", "", nil)

	_, err = mgr.Create(context.Background(), "tenant-1", CreateRequest{Name: "svc", Image: "nginx:latest", Port: 8080, Env: map[string]string{"A": "1", "B": "2"}})
	assert.ErrorIs(t, err, apierr.ErrQuotaExceeded)
	var n int
	require.NoError(t, stateDB.QueryRow(`SELECT COUNT(*) FROM services`).Scan(&n))
	assert.Equal(t, 0, n, "service insert must roll back with the env quota failure")
}

// A container-less Restart runs the full Deploy; it must not inherit the
// caller's short request deadline (a slow pull would fail and leave the rows
// stuck in 'deploying').
func TestRestart_ContainerlessDeployOutlivesRequestDeadline(t *testing.T) {
	mock := &testutil.MockDockerClient{PullImageFn: func(ctx context.Context, img string) error {
		select {
		case <-time.After(300 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	mgr, _, svc := newTestMgr(t, mock, "")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	require.NoError(t, mgr.Restart(ctx, "tenant-1", svc.ID))
	got, err := mgr.Get(context.Background(), "tenant-1", svc.ID)
	require.NoError(t, err)
	assert.Equal(t, "running", got.Status)
}

func TestDeploy_TenantSuspendedDuringPull_DoesNotRunContainer(t *testing.T) {
	var mgr *Manager
	mock := &testutil.MockDockerClient{PullImageFn: func(ctx context.Context, img string) error {
		_, err := mgr.db.Exec(`UPDATE tenants SET status = 'suspended' WHERE id = 'tenant-1'`)
		return err
	}}
	mgr, _, svc := newTestMgr(t, mock, "")

	err := mgr.Deploy(context.Background(), "tenant-1", svc.ID)
	require.Error(t, err)
	assert.ErrorIs(t, err, apierr.ErrForbidden)
	assert.Equal(t, 0, mock.RunContainerCalls)
}

func TestDeploy_TenantSuspendedAfterRunContainer_RemovesContainer(t *testing.T) {
	var mgr *Manager
	mock := &testutil.MockDockerClient{RunContainerFn: func(ctx context.Context, tenantID, serviceID, img string, port int, env, labels map[string]string, l *docker.ResourceLimits) (string, error) {
		_, err := mgr.db.Exec(`UPDATE tenants SET status = 'suspended' WHERE id = 'tenant-1'`)
		return "new-ctr", err
	}}
	mgr, _, svc := newTestMgr(t, mock, "")

	require.Error(t, mgr.Deploy(context.Background(), "tenant-1", svc.ID))
	assert.Contains(t, mock.RemoveContainerCalls, "new-ctr")
	var status, cid string
	require.NoError(t, mgr.db.QueryRow(`SELECT status, container_id FROM services WHERE id = ?`, svc.ID).Scan(&status, &cid))
	assert.NotEqual(t, "running", status)
	assert.Empty(t, cid)
}

func TestDeployImage_TenantSuspendedAfterRunContainer_RemovesContainer(t *testing.T) {
	var mgr *Manager
	mock := &testutil.MockDockerClient{RunContainerFn: func(ctx context.Context, tenantID, serviceID, img string, port int, env, labels map[string]string, l *docker.ResourceLimits) (string, error) {
		_, err := mgr.db.Exec(`UPDATE tenants SET status = 'suspended' WHERE id = 'tenant-1'`)
		return "built-ctr", err
	}}
	mgr, _, svc := newTestMgr(t, mock, "")

	require.Error(t, mgr.DeployImage(context.Background(), "tenant-1", svc.ID, "127.0.0.1:5000/ah/x:1", ""))
	assert.Contains(t, mock.RemoveContainerCalls, "built-ctr")
}

func TestDiscardContainer_UsesFreshContext(t *testing.T) {
	var sawCancelled bool
	mock := &testutil.MockDockerClient{
		StopContainerFn: func(ctx context.Context, id string) error {
			sawCancelled = sawCancelled || ctx.Err() != nil
			return ctx.Err()
		},
		RemoveContainerFn: func(ctx context.Context, id string) error {
			sawCancelled = sawCancelled || ctx.Err() != nil
			return ctx.Err()
		},
	}
	mgr := NewManager(testutil.NewStateDB(t), mock, []byte("0123456789abcdef0123456789abcdef"), "", "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mgr.discardContainer(ctx, "svc", "ctr")
	assert.False(t, sawCancelled)
	assert.Contains(t, mock.RemoveContainerCalls, "ctr")
}

func TestErrDeployQueueFull_Is503(t *testing.T) {
	var apiErr *apierr.APIError
	require.True(t, errors.As(errDeployQueueFull(), &apiErr))
	assert.Equal(t, http.StatusServiceUnavailable, apiErr.Code)
	assert.ErrorIs(t, errDiskLow(errors.New("disk 95% full")), apierr.ErrUnavailable)
}

func TestDelete_KeepsSnapshotImageUsedByRestoredService(t *testing.T) {
	mock := &testutil.MockDockerClient{}
	mgr, _, svc := newTestMgr(t, mock, "")
	ref := "127.0.0.1:5000/snapshots/tenant-1:snap1"
	_, err := mgr.db.Exec(`INSERT INTO snapshots (id, tenant_id, service_id, name, image_ref, created_at) VALUES ('snap1', 'tenant-1', ?, 's', ?, 1)`, svc.ID, ref)
	require.NoError(t, err)
	_, err = mgr.db.Exec(`INSERT INTO services (id, tenant_id, name, status, image, port, created_at, updated_at) VALUES ('restored', 'tenant-1', 'restored', 'running', ?, 8080, 1, 1)`, ref)
	require.NoError(t, err)

	require.NoError(t, mgr.Delete(context.Background(), "tenant-1", svc.ID))
	assert.NotContains(t, mock.RemoveImageCalls, ref)
}
