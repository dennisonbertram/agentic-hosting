package environments

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedTemplates inserts the default set of environment templates into the test DB.
func seedTemplates(t *testing.T, db interface{ ExecContext(ctx context.Context, query string, args ...any) (interface{ RowsAffected() (int64, error) }, error) }) {
	t.Helper()
	// Use the raw *sql.DB from testutil since ExecContext returns (sql.Result, error).
}

func seedDefaultTemplate(t *testing.T, stateDB interface {
	Exec(query string, args ...any) (interface{ RowsAffected() (int64, error) }, error)
}) {
	t.Helper()
}

func TestPoolAcquire_Hit(t *testing.T) {
	db := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{}

	pool := NewPoolManager(db, mock, PoolConfig{
		Enabled:  true,
		PoolSize: 2,
		MaxTotal: 6,
	})

	// Insert a ready warm pool entry directly.
	_, err := db.Exec(
		`INSERT INTO warm_pool (id, template_id, container_id, volume_name, status, created_at)
		 VALUES ('wp1', 'tmpl_default', 'container-abc', 'ah-pool-wp1', 'ready', ?)`,
		time.Now().Unix())
	require.NoError(t, err)

	containerID, volumeName, err := pool.Acquire(context.Background(), "tmpl_default")
	require.NoError(t, err)
	assert.Equal(t, "container-abc", containerID)
	assert.Equal(t, "ah-pool-wp1", volumeName)

	// Verify the row was deleted.
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM warm_pool WHERE id = 'wp1'`).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func TestPoolAcquire_Miss(t *testing.T) {
	db := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{}

	pool := NewPoolManager(db, mock, PoolConfig{
		Enabled:  true,
		PoolSize: 2,
		MaxTotal: 6,
	})

	_, _, err := pool.Acquire(context.Background(), "tmpl_default")
	assert.ErrorIs(t, err, ErrPoolEmpty)
}

func TestPoolAcquire_ConcurrentSafe(t *testing.T) {
	db := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{}

	pool := NewPoolManager(db, mock, PoolConfig{
		Enabled:  true,
		PoolSize: 2,
		MaxTotal: 6,
	})

	// Insert exactly one ready entry.
	_, err := db.Exec(
		`INSERT INTO warm_pool (id, template_id, container_id, volume_name, status, created_at)
		 VALUES ('wp_race', 'tmpl_default', 'container-race', 'ah-pool-wp_race', 'ready', ?)`,
		time.Now().Unix())
	require.NoError(t, err)

	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, acqErr := pool.Acquire(context.Background(), "tmpl_default")
			if acqErr == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()

	// Exactly one goroutine should win.
	assert.Equal(t, int32(1), wins.Load(), "exactly one goroutine should acquire the container")
}

func TestPoolRefill_CreatesContainers(t *testing.T) {
	db := testutil.NewStateDB(t)

	var containerCount atomic.Int32
	mock := &testutil.MockDockerClient{
		RunEnvironmentFn: func(ctx context.Context, cfg docker.RunEnvironmentConfig) (string, error) {
			n := containerCount.Add(1)
			return fmt.Sprintf("env-container-%s-%d", cfg.EnvID, n), nil
		},
	}

	pool := NewPoolManager(db, mock, PoolConfig{
		Enabled:      true,
		PoolSize:     2,
		MaxTotal:     6,
		RefillPeriod: time.Hour, // won't tick in this test
	})

	// Run one refill cycle directly.
	err := pool.refill(context.Background())
	require.NoError(t, err)

	// Check that containers were created for each template.
	// The DB has 4 templates (default + node + python + go), each should get 2 containers.
	var totalReady int
	err = db.QueryRow(`SELECT COUNT(*) FROM warm_pool WHERE status = 'ready'`).Scan(&totalReady)
	require.NoError(t, err)
	assert.Equal(t, 6, totalReady, "should create up to MaxTotal containers")

	// Verify the mock was called.
	assert.Equal(t, 6, int(containerCount.Load()), "should have created 6 containers via docker")
}

func TestPoolRefill_RespectsMaxTotal(t *testing.T) {
	db := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{}

	pool := NewPoolManager(db, mock, PoolConfig{
		Enabled:      true,
		PoolSize:     5, // wants 5 per template
		MaxTotal:     3, // but max 3 total
		RefillPeriod: time.Hour,
	})

	err := pool.refill(context.Background())
	require.NoError(t, err)

	var totalReady int
	err = db.QueryRow(`SELECT COUNT(*) FROM warm_pool WHERE status = 'ready'`).Scan(&totalReady)
	require.NoError(t, err)
	assert.Equal(t, 3, totalReady, "should not exceed MaxTotal")
}

func TestPoolStats(t *testing.T) {
	db := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{}

	pool := NewPoolManager(db, mock, PoolConfig{
		Enabled:  true,
		PoolSize: 2,
		MaxTotal: 6,
	})

	now := time.Now().Unix()
	// Insert entries for two templates.
	_, err := db.Exec(
		`INSERT INTO warm_pool (id, template_id, container_id, volume_name, status, created_at)
		 VALUES ('s1', 'tmpl_node', 'c1', 'v1', 'ready', ?),
		        ('s2', 'tmpl_node', 'c2', 'v2', 'ready', ?),
		        ('s3', 'tmpl_python', 'c3', 'v3', 'ready', ?)`,
		now, now, now)
	require.NoError(t, err)

	stats, err := pool.Stats(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, stats["tmpl_node"])
	assert.Equal(t, 1, stats["tmpl_python"])
	assert.Equal(t, 0, stats["tmpl_go"]) // not in map, defaults to zero
}

// TestPoolRefill_NoTemplates verifies that when there are no templates in the DB,
// refill is a no-op and no Docker calls are made.
func TestPoolRefill_NoTemplates(t *testing.T) {
	db := testutil.NewStateDB(t)

	var containerCount atomic.Int32
	mock := &testutil.MockDockerClient{
		RunEnvironmentFn: func(ctx context.Context, cfg docker.RunEnvironmentConfig) (string, error) {
			containerCount.Add(1)
			return "container-" + cfg.EnvID, nil
		},
	}

	pool := NewPoolManager(db, mock, PoolConfig{
		Enabled:      true,
		PoolSize:     2,
		MaxTotal:     6,
		RefillPeriod: time.Hour,
	})

	// Remove all templates so the pool has nothing to fill.
	_, err := db.Exec(`DELETE FROM environment_templates`)
	require.NoError(t, err)

	err = pool.refill(context.Background())
	require.NoError(t, err)

	// No containers should be created when there are no templates.
	assert.Equal(t, int32(0), containerCount.Load(), "no containers should be created with no templates")

	// warm_pool table should remain empty.
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM warm_pool`).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "warm_pool should be empty when no templates exist")
}

// TestPoolRefill_MaxTotalAlreadyMet verifies that when MaxTotal containers are already
// in ready state, refill creates no new containers.
func TestPoolRefill_MaxTotalAlreadyMet(t *testing.T) {
	db := testutil.NewStateDB(t)

	var containerCount atomic.Int32
	mock := &testutil.MockDockerClient{
		RunEnvironmentFn: func(ctx context.Context, cfg docker.RunEnvironmentConfig) (string, error) {
			containerCount.Add(1)
			return "container-" + cfg.EnvID, nil
		},
	}

	pool := NewPoolManager(db, mock, PoolConfig{
		Enabled:      true,
		PoolSize:     5, // wants 5 per template
		MaxTotal:     2, // max is 2 total
		RefillPeriod: time.Hour,
	})

	now := time.Now().Unix()
	// Pre-fill exactly MaxTotal (2) ready entries so no more should be created.
	_, err := db.Exec(
		`INSERT INTO warm_pool (id, template_id, container_id, volume_name, status, created_at)
		 VALUES ('pre1', 'tmpl_default', 'c-pre1', 'v-pre1', 'ready', ?),
		        ('pre2', 'tmpl_default', 'c-pre2', 'v-pre2', 'ready', ?)`,
		now, now)
	require.NoError(t, err)

	err = pool.refill(context.Background())
	require.NoError(t, err)

	// The mock should NOT have been called because MaxTotal was already reached.
	assert.Equal(t, int32(0), containerCount.Load(),
		"no new containers should be created when MaxTotal is already met")

	// Total ready count should still be exactly 2.
	var totalReady int
	err = db.QueryRow(`SELECT COUNT(*) FROM warm_pool WHERE status = 'ready'`).Scan(&totalReady)
	require.NoError(t, err)
	assert.Equal(t, 2, totalReady, "ready count should remain at MaxTotal (2)")
}

// TestPoolDrain_StopsAndRemovesAll verifies that Drain stops and removes all containers,
// removes their volumes, and leaves the warm_pool table empty.
func TestPoolDrain_StopsAndRemovesAll(t *testing.T) {
	db := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{}

	pool := NewPoolManager(db, mock, PoolConfig{
		Enabled:  true,
		PoolSize: 2,
		MaxTotal: 6,
	})

	now := time.Now().Unix()
	// Insert 3 pool entries.
	_, err := db.Exec(
		`INSERT INTO warm_pool (id, template_id, container_id, volume_name, status, created_at)
		 VALUES ('d1', 'tmpl_default', 'ctr-drain-1', 'vol-drain-1', 'ready', ?),
		        ('d2', 'tmpl_default', 'ctr-drain-2', 'vol-drain-2', 'ready', ?),
		        ('d3', 'tmpl_node',    'ctr-drain-3', 'vol-drain-3', 'ready', ?)`,
		now, now, now)
	require.NoError(t, err)

	err = pool.Drain(context.Background())
	require.NoError(t, err)

	// StopContainer should have been called for all 3 containers.
	assert.Len(t, mock.StopContainerCalls, 3, "should stop all 3 containers")
	assert.Contains(t, mock.StopContainerCalls, "ctr-drain-1")
	assert.Contains(t, mock.StopContainerCalls, "ctr-drain-2")
	assert.Contains(t, mock.StopContainerCalls, "ctr-drain-3")

	// RemoveContainer should have been called for all 3.
	assert.Len(t, mock.RemoveContainerCalls, 3, "should remove all 3 containers")
	assert.Contains(t, mock.RemoveContainerCalls, "ctr-drain-1")
	assert.Contains(t, mock.RemoveContainerCalls, "ctr-drain-2")
	assert.Contains(t, mock.RemoveContainerCalls, "ctr-drain-3")

	// warm_pool table should be empty after drain.
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM warm_pool`).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "warm_pool table should be empty after Drain")
}

// TestPoolStats_PerTemplate verifies that Stats returns correct per-template counts
// and excludes non-ready entries from the counts.
func TestPoolStats_PerTemplate(t *testing.T) {
	db := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{}

	pool := NewPoolManager(db, mock, PoolConfig{
		Enabled:  true,
		PoolSize: 2,
		MaxTotal: 6,
	})

	now := time.Now().Unix()
	_, err := db.Exec(
		`INSERT INTO warm_pool (id, template_id, container_id, volume_name, status, created_at)
		 VALUES ('p1', 'tmpl_default', 'c1', 'v1', 'ready',   ?),
		        ('p2', 'tmpl_default', 'c2', 'v2', 'ready',   ?),
		        ('p3', 'tmpl_default', 'c3', 'v3', 'claimed', ?),
		        ('p4', 'tmpl_node',    'c4', 'v4', 'ready',   ?)`,
		now, now, now, now)
	require.NoError(t, err)

	stats, err := pool.Stats(context.Background())
	require.NoError(t, err)

	// claimed entries should NOT count toward ready stats.
	assert.Equal(t, 2, stats["tmpl_default"],
		"tmpl_default should have 2 ready (not 3; the claimed one is excluded)")
	assert.Equal(t, 1, stats["tmpl_node"],
		"tmpl_node should have exactly 1 ready entry")
	// A template with no entries should return zero (map default).
	assert.Equal(t, 0, stats["tmpl_python"],
		"tmpl_python should not appear in stats (zero value)")
	// Stats map should only contain templates that have ready containers.
	assert.Len(t, stats, 2, "stats should only have entries for templates with ready containers")
}

// ---------------------------------------------------------------------------
// BT-002: refill() prunes stale entries (ghost pool rows)
// ---------------------------------------------------------------------------

// TestPoolRefill_PrunesStaleEntries verifies BT-002:
// When refill() runs and a DB row points to a non-existent container,
// that row is deleted from the DB.
func TestPoolRefill_PrunesStaleEntries(t *testing.T) {
	db := testutil.NewStateDB(t)

	// InspectContainer returns error for ghost containers (container gone),
	// returns success for live containers.
	mock := &testutil.MockDockerClient{
		InspectContainerFn: func(ctx context.Context, id string) (*docker.ContainerInfo, error) {
			if id == "ghost-container" {
				return nil, fmt.Errorf("no such container: %s", id)
			}
			return &docker.ContainerInfo{Status: "running"}, nil
		},
	}

	pool := NewPoolManager(db, mock, PoolConfig{
		Enabled:      true,
		PoolSize:     2,
		MaxTotal:     6,
		RefillPeriod: time.Hour,
	})

	now := time.Now().Unix()
	// Insert one ghost row (container doesn't exist in Docker).
	_, err := db.Exec(
		`INSERT INTO warm_pool (id, template_id, container_id, volume_name, status, created_at)
		 VALUES ('ghost1', 'tmpl_default', 'ghost-container', 'ah-pool-ghost1', 'ready', ?)`,
		now)
	require.NoError(t, err)

	// Run refill — it should prune the ghost row before counting/filling.
	err = pool.refill(context.Background())
	require.NoError(t, err)

	// The ghost row must be gone.
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM warm_pool WHERE id = 'ghost1'`).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "ghost row must be deleted after refill prunes stale entries")
}

// TestPoolRefill_KeepsLiveEntries verifies the inverse of BT-002:
// when a pool row's container DOES exist, refill() leaves the row intact.
func TestPoolRefill_KeepsLiveEntries(t *testing.T) {
	db := testutil.NewStateDB(t)

	mock := &testutil.MockDockerClient{
		InspectContainerFn: func(ctx context.Context, id string) (*docker.ContainerInfo, error) {
			// All containers are alive.
			return &docker.ContainerInfo{Status: "running"}, nil
		},
	}

	pool := NewPoolManager(db, mock, PoolConfig{
		Enabled:      true,
		PoolSize:     2,
		MaxTotal:     6,
		RefillPeriod: time.Hour,
	})

	now := time.Now().Unix()
	_, err := db.Exec(
		`INSERT INTO warm_pool (id, template_id, container_id, volume_name, status, created_at)
		 VALUES ('live1', 'tmpl_default', 'live-container', 'ah-pool-live1', 'ready', ?)`,
		now)
	require.NoError(t, err)

	err = pool.refill(context.Background())
	require.NoError(t, err)

	// The live row must still be there.
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM warm_pool WHERE id = 'live1'`).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "live row must not be deleted by stale-entry pruning")
}
