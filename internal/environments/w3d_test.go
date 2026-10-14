package environments

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A claimed pool container must be renamed to the tenant env name so the
// preview Traefik target (http://ah-env-<tenant>-<env>) resolves.
func TestCreate_PoolClaimRenamesContainer(t *testing.T) {
	mgr, mock, _, sharedDB := newTestManagerWithPool(t, 5)
	insertWarmPoolEntry(t, sharedDB, "wp_r1", "tmpl_default", "pool-ctr-1", "ah-pool-wp_r1")

	var renamedID, renamedTo string
	mock.RenameContainerFn = func(ctx context.Context, id, name string) error {
		renamedID, renamedTo = id, name
		return nil
	}
	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "r", TemplateID: "default"})
	require.NoError(t, err)
	assert.Equal(t, "pool-ctr-1", renamedID)
	assert.Equal(t, "ah-env-"+testTenantID+"-"+env.ID, renamedTo)

	// The claimed container keeps a warm_pool row so GC's warm-pool check keeps it.
	var n int
	require.NoError(t, sharedDB.QueryRow(`SELECT COUNT(*) FROM warm_pool WHERE container_id = 'pool-ctr-1'`).Scan(&n))
	assert.Equal(t, 1, n)

	// Deleting the env removes that row.
	require.NoError(t, mgr.Delete(context.Background(), testTenantID, env.ID))
	require.NoError(t, sharedDB.QueryRow(`SELECT COUNT(*) FROM warm_pool WHERE container_id = 'pool-ctr-1'`).Scan(&n))
	assert.Equal(t, 0, n)
}

// If the claimed container cannot be moved to the tenant network it must be
// removed, not leaked, and creation falls back to a cold start.
func TestCreate_PoolConnectFailureRemovesContainer(t *testing.T) {
	mgr, mock, _, sharedDB := newTestManagerWithPool(t, 5)
	insertWarmPoolEntry(t, sharedDB, "wp_c1", "tmpl_default", "pool-ctr-2", "ah-pool-wp_c1")
	mock.ConnectNetworkFn = func(ctx context.Context, net, id string) error { return fmt.Errorf("boom") }
	mock.RunEnvironmentFn = func(ctx context.Context, cfg docker.RunEnvironmentConfig) (string, error) {
		return "cold-1", nil
	}
	var removed []string
	mock.RemoveContainerFn = func(ctx context.Context, id string) error { removed = append(removed, id); return nil }

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "c", TemplateID: "default"})
	require.NoError(t, err)
	assert.Equal(t, "cold-1", env.ContainerID)
	assert.Contains(t, removed, "pool-ctr-2")
}

func TestExec_TimeoutSetsTimedOut(t *testing.T) {
	mgr, mock := newTestManager(t, 5)
	mock.ExecRunFn = func(ctx context.Context, execID string, timeout time.Duration) ([]byte, []byte, int, error) {
		return []byte("partial"), nil, -1, fmt.Errorf("exec timed out: %w", context.DeadlineExceeded)
	}
	var gotCmd []string
	mock.ExecCreateFn = func(ctx context.Context, id string, cmd []string, wd string) (string, error) {
		gotCmd = cmd
		return "e1", nil
	}
	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "to"})
	require.NoError(t, err)
	resp, err := mgr.Exec(context.Background(), testTenantID, env.ID, ExecRequest{Command: []string{"sleep", "infinity"}, Timeout: 1})
	require.NoError(t, err)
	assert.True(t, resp.TimedOut)
	assert.Equal(t, "partial", resp.Stdout)
	// The process is also killed inside the container by coreutils timeout.
	require.GreaterOrEqual(t, len(gotCmd), 4)
	assert.Equal(t, "timeout", gotCmd[0])
	assert.Equal(t, []string{"sleep", "infinity"}, gotCmd[len(gotCmd)-2:])
}

func TestStopAllForTenant_StopsEnvsDisablesPreviewRoutesKeepsRows(t *testing.T) {
	dir := t.TempDir()
	mgr, mock := newTestManagerWithTraefik(t, 5, "example.com", dir)
	mock.RunEnvironmentFn = func(ctx context.Context, cfg docker.RunEnvironmentConfig) (string, error) {
		return "ctr-" + cfg.EnvID, nil
	}
	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "s"})
	require.NoError(t, err)
	_, err = mgr.CreatePreview(context.Background(), testTenantID, env.ID, CreatePreviewRequest{Name: "web", Port: 3000})
	require.NoError(t, err)

	var stopped []string
	mock.StopContainerFn = func(ctx context.Context, id string) error { stopped = append(stopped, id); return nil }
	mgr.StopAllForTenant(context.Background(), testTenantID)

	assert.Equal(t, []string{env.ContainerID}, stopped)
	got, err := mgr.Get(context.Background(), testTenantID, env.ID)
	require.NoError(t, err)
	assert.Equal(t, "stopped", got.Status)
	files, _ := os.ReadDir(dir)
	assert.Len(t, files, 0, "preview Traefik routes must be removed")
	ps, err := mgr.ListPreviews(context.Background(), testTenantID, env.ID)
	require.NoError(t, err)
	assert.Len(t, ps, 1, "preview rows are kept for restore on reactivation")
}

func TestCreatePreview_CapAndLabelLength(t *testing.T) {
	mgr, mock := newTestManagerWithTraefik(t, 5, "example.com", t.TempDir())
	mock.RunEnvironmentFn = func(ctx context.Context, cfg docker.RunEnvironmentConfig) (string, error) { return "c", nil }
	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "p"})
	require.NoError(t, err)

	_, err = mgr.CreatePreview(context.Background(), testTenantID, env.ID,
		CreatePreviewRequest{Name: strings.Repeat("a", 51), Port: 80})
	require.Error(t, err, "label env-xxxxxxxx-<name> must fit in 63 chars")

	for i := 0; i < maxPreviewsPerEnv; i++ {
		_, err = mgr.CreatePreview(context.Background(), testTenantID, env.ID,
			CreatePreviewRequest{Name: fmt.Sprintf("p%d", i), Port: 80})
		require.NoError(t, err)
	}
	_, err = mgr.CreatePreview(context.Background(), testTenantID, env.ID,
		CreatePreviewRequest{Name: "extra", Port: 80})
	require.Error(t, err)
}

// Acquire must not wait for an in-progress refill (image pull can take minutes).
func TestPoolAcquire_NotBlockedByRefill(t *testing.T) {
	db := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{}
	pool := NewPoolManager(db, mock, PoolConfig{Enabled: true, PoolSize: 2, MaxTotal: 6})

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	mock.RunEnvironmentFn = func(ctx context.Context, cfg docker.RunEnvironmentConfig) (string, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return "c-" + cfg.EnvID, nil
	}
	_, err := db.Exec(`INSERT INTO warm_pool (id, template_id, container_id, volume_name, status, created_at)
		 VALUES ('wpx', 'tmpl_default', 'ctr-x', 'ah-pool-wpx', 'ready', ?)`, time.Now().Unix())
	require.NoError(t, err)

	go func() { _ = pool.refill(context.Background()) }()
	defer close(release)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("refill never reached RunEnvironment")
	}

	done := make(chan error, 1)
	go func() { _, _, err := pool.Acquire(context.Background(), "tmpl_default"); done <- err }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire blocked behind refill")
	}
}

// Only a not-found Inspect error may prune a pool row; transient errors keep it.
func TestPoolRefill_KeepsRowOnTransientInspectError(t *testing.T) {
	db := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{}
	pool := NewPoolManager(db, mock, PoolConfig{Enabled: true, PoolSize: 1, MaxTotal: 1})
	_, err := db.Exec(`INSERT INTO warm_pool (id, template_id, container_id, volume_name, status, created_at)
		 VALUES ('wpt', 'tmpl_default', 'ctr-t', 'ah-pool-wpt', 'ready', ?)`, time.Now().Unix())
	require.NoError(t, err)
	mock.InspectContainerFn = func(ctx context.Context, id string) (*docker.ContainerInfo, error) {
		return nil, fmt.Errorf("docker daemon unavailable")
	}
	require.NoError(t, pool.refill(context.Background()))
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM warm_pool WHERE id='wpt'`).Scan(&n))
	assert.Equal(t, 1, n)
}

// Pool containers are created with the explicit WarmPool lifecycle, which
// labels them ah.type=warm-pool so GC's warm-pool check matches.
func TestPoolRefill_KeepsWarmPoolLabelType(t *testing.T) {
	db := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{}
	pool := NewPoolManager(db, mock, PoolConfig{Enabled: true, PoolSize: 1, MaxTotal: 1})
	var got *docker.RunEnvironmentConfig
	mock.RunEnvironmentFn = func(ctx context.Context, cfg docker.RunEnvironmentConfig) (string, error) {
		got = &cfg
		return "c1", nil
	}
	require.NoError(t, pool.refill(context.Background()))
	require.NotNil(t, got)
	assert.True(t, got.WarmPool)
	// docker.environmentContainerLabels maps WarmPool to ah.type=warm-pool (see docker tests).
}
