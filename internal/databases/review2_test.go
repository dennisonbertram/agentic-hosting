package databases

import (
	"context"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// If recording container_id fails while the row still exists, Create must mark
// the row failed (so it stops counting toward quota), clean up the container
// and volume, and return the real error. The row still owns its per-database
// network, so that network stays until Delete.
func TestCreate_ContainerIDWriteFailureMarksFailed(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedDatabaseTestData(t, stateDB, 5)
	_, err := stateDB.Exec(`CREATE TRIGGER fail_container_id BEFORE UPDATE OF container_id ON databases
		WHEN NEW.container_id = 'c-unrecorded'
		BEGIN SELECT RAISE(ABORT, 'injected container_id failure'); END`)
	require.NoError(t, err)
	mock := &testutil.MockDockerClient{
		RunDatabaseFn: func(context.Context, docker.RunDatabaseConfig) (string, error) { return "c-unrecorded", nil },
	}
	m := NewManager(stateDB, mock, testKey)

	_, err = m.Create(context.Background(), "tenant-1", CreateRequest{Name: "unrec", Type: "postgres"})
	require.ErrorContains(t, err, "record database container")
	assert.ErrorContains(t, err, "injected container_id failure")
	assert.NotContains(t, err.Error(), "deleted during provisioning")
	assert.Contains(t, mock.RemoveContainerCalls, "c-unrecorded")
	assert.Len(t, mock.RemoveVolumeCalls, 1)
	assert.Empty(t, mock.RemoveNetworkCalls, "the surviving row still owns its network")

	var status string
	require.NoError(t, stateDB.QueryRow(`SELECT status FROM databases WHERE name = 'unrec'`).Scan(&status))
	assert.Equal(t, "failed", status)
}

// A database error while marking the row failed is not proof the row is gone:
// the row may still be 'provisioning' and own its network, so Create must not
// remove that network.
func TestCreate_MarkFailedErrorKeepsNetwork(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedDatabaseTestData(t, stateDB, 5)
	_, err := stateDB.Exec(`CREATE TRIGGER fail_mark_failed BEFORE UPDATE OF status ON databases
		WHEN NEW.status = 'failed'
		BEGIN SELECT RAISE(ABORT, 'injected status failure'); END`)
	require.NoError(t, err)
	mock := &testutil.MockDockerClient{
		RunDatabaseFn: func(context.Context, docker.RunDatabaseConfig) (string, error) { return "c-sick", nil },
	}
	m := NewManager(stateDB, mock, testKey)
	m.healthTimeout = 100 * time.Millisecond // no engine listens

	_, err = m.Create(context.Background(), "tenant-1", CreateRequest{Name: "sick", Type: "redis"})
	require.ErrorContains(t, err, "health check failed")
	assert.Contains(t, mock.RemoveContainerCalls, "c-sick")
	require.Len(t, mock.EnsureDatabaseNetworkCalls, 1)
	assert.Empty(t, mock.RemoveNetworkCalls, "a DB error must not be treated as 'row gone'")
}

// If the provisioning -> ready transition fails with a database error after a
// healthy probe, the row may still exist and own its per-database network.
// Create must mark it failed (freeing quota), remove the container and volume,
// and keep the network for Delete rather than treating the row as gone.
func TestCreate_ReadyTransitionErrorMarksFailedAndKeepsNetwork(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedDatabaseTestData(t, stateDB, 5)
	_, err := stateDB.Exec(`CREATE TRIGGER fail_ready BEFORE UPDATE OF status ON databases
		WHEN NEW.status = 'ready'
		BEGIN SELECT RAISE(ABORT, 'injected ready failure'); END`)
	require.NoError(t, err)
	mock := &testutil.MockDockerClient{
		RunDatabaseFn: func(_ context.Context, cfg docker.RunDatabaseConfig) (string, error) {
			startFakeRedisOnPort(t, cfg.HostPort)
			return "c-unready", nil
		},
	}
	m := NewManager(stateDB, mock, testKey)

	_, err = m.Create(context.Background(), "tenant-1", CreateRequest{Name: "unready", Type: "redis"})
	require.ErrorContains(t, err, "mark database ready failed")
	assert.NotContains(t, err.Error(), "deleted during provisioning")
	assert.Contains(t, mock.RemoveContainerCalls, "c-unready")
	assert.Len(t, mock.RemoveVolumeCalls, 1)
	require.Len(t, mock.EnsureDatabaseNetworkCalls, 1)
	assert.Empty(t, mock.RemoveNetworkCalls, "the surviving row still owns its network")

	var status string
	require.NoError(t, stateDB.QueryRow(`SELECT status FROM databases WHERE name = 'unready'`).Scan(&status))
	assert.Equal(t, "failed", status)
}

func TestMarkFailed_ReportsRowGoneOnlyWhenMissing(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedDatabaseTestData(t, stateDB, 5)
	insertNullContainerDB(t, stateDB, "dbm", "provisioning")
	m := &Manager{db: stateDB, docker: &testutil.MockDockerClient{}, masterKey: testKey}

	assert.False(t, m.markFailed(context.Background(), "dbm"), "existing row is not gone")
	var status string
	require.NoError(t, stateDB.QueryRow(`SELECT status FROM databases WHERE id = 'dbm'`).Scan(&status))
	assert.Equal(t, "failed", status)

	assert.True(t, m.markFailed(context.Background(), "missing"))

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	insertNullContainerDB(t, stateDB, "dbc", "provisioning")
	assert.False(t, m.markFailed(cancelled, "dbc"), "the write is detached from a cancelled request")
	require.NoError(t, stateDB.QueryRow(`SELECT status FROM databases WHERE id = 'dbc'`).Scan(&status))
	assert.Equal(t, "failed", status)
}

// findFreePort must surface a corrupt port value instead of silently treating
// that port as free (which could hand out a port a live database holds).
func TestFindFreePort_ScanErrorIsReturned(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedDatabaseTestData(t, stateDB, 5)
	_, err := stateDB.Exec(`INSERT INTO databases (id, tenant_id, name, type, status, port, password_encrypted, created_at, updated_at)
		VALUES ('dbbad', 'tenant-1', 'bad', 'postgres', 'ready', 'not-a-port', 'x', 1, 1)`)
	require.NoError(t, err)
	m := &Manager{db: stateDB, docker: &testutil.MockDockerClient{}, masterKey: testKey}

	_, err = m.findFreePort("postgres")
	assert.ErrorContains(t, err, "scan port")
}
