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
