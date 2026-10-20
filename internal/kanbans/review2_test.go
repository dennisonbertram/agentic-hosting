package kanbans

import (
	"context"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// If recording container_id fails while the row still exists, Create must mark
// it failed (so a re-create can replace it instead of being blocked by a
// 'provisioning' row), clean up the container and volume, and return the real
// error rather than claiming the row was deleted.
func TestCreate_ContainerIDWriteFailureMarksFailed(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)
	_, err := stateDB.Exec(`CREATE TRIGGER fail_container_id BEFORE UPDATE OF container_id ON kanbans
		WHEN NEW.container_id = 'c-unrecorded'
		BEGIN SELECT RAISE(ABORT, 'injected container_id failure'); END`)
	require.NoError(t, err)
	mock := &testutil.MockDockerClient{
		RunDatabaseFn: func(context.Context, docker.RunDatabaseConfig) (string, error) { return "c-unrecorded", nil },
	}
	mgr := newIsolatedManager(t, stateDB, mock)

	_, err = mgr.Create(context.Background(), "tenant-1")
	require.ErrorContains(t, err, "record kanban container")
	assert.ErrorContains(t, err, "injected container_id failure")
	assert.NotContains(t, err.Error(), "deleted during provisioning")
	mgr.waitBackground()
	assert.Contains(t, mock.RemoveContainerCalls, "c-unrecorded")
	assert.Len(t, mock.RemoveVolumeCalls, 1)

	k, err := mgr.Get(context.Background(), "tenant-1")
	require.NoError(t, err)
	assert.Equal(t, "failed", k.Status)
}

// The in-place restart's failure cleanup must run even when the request that
// triggered it was cancelled, or the restarted container keeps running.
func TestRestartFailedInPlace_ContainerIDWriteFailureCleansUpDetached(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)
	_, err := stateDB.Exec(`INSERT INTO kanbans (id, tenant_id, status, container_id, host, port, volume_name, url,
		admin_token_encrypted, created_at, updated_at)
		VALUES ('kr0123456789abcdef', 'tenant-1', 'failed', '', '127.0.0.1', 7350, 'vol-keep', 'u', 'enc', 1, 1)`)
	require.NoError(t, err)
	_, err = stateDB.Exec(`CREATE TRIGGER fail_container_id BEFORE UPDATE OF container_id ON kanbans
		WHEN NEW.container_id = 'c-restart'
		BEGIN SELECT RAISE(ABORT, 'injected container_id failure'); END`)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	mock := &testutil.MockDockerClient{
		RunDatabaseFn: func(context.Context, docker.RunDatabaseConfig) (string, error) {
			cancel() // the client goes away once the container is started
			return "c-restart", nil
		},
	}
	var removedLive []string
	mock.RemoveContainerFn = func(c context.Context, id string) error {
		if c.Err() == nil {
			removedLive = append(removedLive, id)
		}
		return c.Err()
	}
	mgr := newIsolatedManager(t, stateDB, mock)

	_, err = mgr.Create(ctx, "tenant-1")
	require.ErrorContains(t, err, "store container id")
	mgr.waitBackground()
	assert.Equal(t, []string{"c-restart"}, removedLive, "removal must run on a live context")
	assert.Empty(t, mock.RemoveVolumeCalls, "the set-up volume holds board data")

	var status string
	require.NoError(t, stateDB.QueryRow(`SELECT status FROM kanbans WHERE id = 'kr0123456789abcdef'`).Scan(&status))
	assert.Equal(t, "failed", status)
}
