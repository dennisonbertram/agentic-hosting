package environments

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStopAllForTenant_StopFailureLeavesRowForRetry(t *testing.T) {
	mgr, mock := newTestManager(t, 5)
	mock.RunEnvironmentFn = func(ctx context.Context, cfg docker.RunEnvironmentConfig) (string, error) {
		return "ctr-" + cfg.EnvID, nil
	}
	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "s"})
	require.NoError(t, err)

	mock.StopContainerFn = func(ctx context.Context, id string) error { return fmt.Errorf("docker down") }
	mgr.StopAllForTenant(context.Background(), testTenantID)
	got, err := mgr.Get(context.Background(), testTenantID, env.ID)
	require.NoError(t, err)
	assert.Equal(t, "running", got.Status, "a failed stop must not be recorded as stopped")

	// Retry succeeds on the next suspend.
	mock.StopContainerFn = nil
	mgr.StopAllForTenant(context.Background(), testTenantID)
	got, _ = mgr.Get(context.Background(), testTenantID, env.ID)
	assert.Equal(t, "stopped", got.Status)
}

func TestStopAllForTenant_NotFoundCountsAsStopped(t *testing.T) {
	mgr, mock := newTestManager(t, 5)
	mock.RunEnvironmentFn = func(ctx context.Context, cfg docker.RunEnvironmentConfig) (string, error) { return "c1", nil }
	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "s"})
	require.NoError(t, err)
	mock.StopContainerFn = func(ctx context.Context, id string) error { return fmt.Errorf("No such container: %s", id) }
	mgr.StopAllForTenant(context.Background(), testTenantID)
	got, _ := mgr.Get(context.Background(), testTenantID, env.ID)
	assert.Equal(t, "stopped", got.Status)
}

// Adoption cleanup must still work when the request context is cancelled.
func TestCreate_PoolAdoptionCleanupSurvivesCancelledContext(t *testing.T) {
	mgr, mock, _, sharedDB := newTestManagerWithPool(t, 5)
	insertWarmPoolEntry(t, sharedDB, "wp_x", "tmpl_default", "pool-x", "ah-pool-wp_x")
	ctx, cancel := context.WithCancel(context.Background())
	mock.ConnectNetworkFn = func(c context.Context, net, id string) error {
		cancel()
		return fmt.Errorf("boom")
	}
	var ctxErrs []error
	mock.RemoveContainerFn = func(c context.Context, id string) error { ctxErrs = append(ctxErrs, c.Err()); return nil }
	mock.RemoveVolumeFn = func(c context.Context, name string) error { ctxErrs = append(ctxErrs, c.Err()); return nil }

	_, _ = mgr.Create(ctx, testTenantID, CreateRequest{Name: "x", TemplateID: "default"})

	require.NotEmpty(t, ctxErrs)
	for _, e := range ctxErrs {
		assert.NoError(t, e, "cleanup must use a detached context")
	}
	var n int
	require.NoError(t, sharedDB.QueryRow(`SELECT COUNT(*) FROM warm_pool WHERE container_id = 'pool-x'`).Scan(&n))
	assert.Equal(t, 0, n, "warm_pool row must be deleted")
}

// A failed disconnect from the pool network is an adoption failure.
func TestCreate_PoolDisconnectFailureFallsBackToCold(t *testing.T) {
	mgr, mock, _, sharedDB := newTestManagerWithPool(t, 5)
	insertWarmPoolEntry(t, sharedDB, "wp_d", "tmpl_default", "pool-d", "ah-pool-wp_d")
	mock.NetworkDisconnectFn = func(c context.Context, net, id string) error { return fmt.Errorf("boom") }
	mock.RunEnvironmentFn = func(c context.Context, cfg docker.RunEnvironmentConfig) (string, error) { return "cold-d", nil }
	var connected []string
	mock.ConnectNetworkFn = func(c context.Context, net, id string) error { connected = append(connected, id); return nil }
	var removed []string
	mock.RemoveContainerFn = func(c context.Context, id string) error { removed = append(removed, id); return nil }

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "d", TemplateID: "default"})
	require.NoError(t, err)
	assert.Equal(t, "cold-d", env.ContainerID)
	assert.NotContains(t, connected, "pool-d")
	assert.Contains(t, removed, "pool-d")
}

// A tenant suspended while Create is starting the container must not end up
// with a running environment.
func TestCreate_TenantSuspendedDuringCreate(t *testing.T) {
	mgr, mock := newTestManager(t, 5)
	mock.RunEnvironmentFn = func(c context.Context, cfg docker.RunEnvironmentConfig) (string, error) {
		_, err := mgr.db.Exec(`UPDATE tenants SET status = 'suspended' WHERE id = ?`, testTenantID)
		require.NoError(t, err)
		return "late-ctr", nil
	}
	_, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "late"})
	require.Error(t, err)
	assert.Contains(t, mock.StopContainerCalls, "late-ctr")
	var status string
	require.NoError(t, mgr.db.QueryRow(`SELECT status FROM environments WHERE name = 'late'`).Scan(&status))
	assert.Equal(t, "failed", status)
}

func TestSyncWorkspace_CommandHasKillTimeout(t *testing.T) {
	mgr, mock := newTestManager(t, 5)
	var gotCmd []string
	mock.ExecCreateFn = func(c context.Context, id string, cmd []string, wd string) (string, error) {
		gotCmd = cmd
		return "e1", nil
	}
	mock.ExecRunFn = func(c context.Context, id string, to time.Duration) ([]byte, []byte, int, error) {
		return nil, nil, 0, nil
	}
	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "sw"})
	require.NoError(t, err)
	require.NoError(t, mgr.SyncWorkspace(context.Background(), testTenantID, env.ID, SyncRequest{GitURL: "https://example.com/r.git"}))
	require.GreaterOrEqual(t, len(gotCmd), 6)
	assert.Equal(t, []string{"timeout", "-s", "KILL", "305", "sh", "-c"}, gotCmd[:6])
}
