package kanbans

import (
	"context"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression: a suspension that lands before Create records container_id
// removes nothing and stops the row; Create must remove the container itself
// (keeping the volume) instead of provisioning it for a suspended tenant.
func TestCreate_SuspendedBeforeContainerIDRemovesContainer(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)
	mock := &testutil.MockDockerClient{}
	mgr := newTestManager(t, stateDB, mock)
	mock.RunDatabaseFn = func(ctx context.Context, cfg docker.RunDatabaseConfig) (string, error) {
		mgr.StopForTenant(context.Background(), "tenant-1")
		return "c-orphan", nil
	}

	_, err := mgr.Create(context.Background(), "tenant-1")
	require.Error(t, err)
	assert.ErrorIs(t, err, apierr.ErrConflict)
	assert.Contains(t, mock.RemoveContainerCalls, "c-orphan")
	var status string
	require.NoError(t, stateDB.QueryRow(`SELECT status FROM kanbans WHERE tenant_id = 'tenant-1'`).Scan(&status))
	assert.Equal(t, "failed", status, "a board that was never set up is not kept for reactivation")
}

// Regression: a board suspended before container_id was recorded never ran
// setup (no admin user or credentials). Reactivation must not start it and
// report it ready; the next Create must replace it.
func TestCreate_SuspendedBeforeSetupNotRevivedAsReady(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)
	mock := &testutil.MockDockerClient{}
	mgr := newTestManager(t, stateDB, mock)
	suspend := true
	mock.RunDatabaseFn = func(ctx context.Context, cfg docker.RunDatabaseConfig) (string, error) {
		if suspend {
			mgr.StopForTenant(context.Background(), "tenant-1")
		}
		return "c-1", nil
	}

	_, err := mgr.Create(context.Background(), "tenant-1")
	require.ErrorIs(t, err, apierr.ErrConflict)
	var firstID string
	require.NoError(t, stateDB.QueryRow(`SELECT id FROM kanbans WHERE tenant_id = 'tenant-1'`).Scan(&firstID))

	runs := mock.RunDatabaseCalls
	mgr.StartForTenant(context.Background(), "tenant-1")
	mgr.waitBackground()
	assert.Equal(t, runs, mock.RunDatabaseCalls, "reactivation must not start a board that was never set up")
	var status string
	require.NoError(t, stateDB.QueryRow(`SELECT status FROM kanbans WHERE id = ?`, firstID).Scan(&status))
	assert.NotEqual(t, "ready", status)

	suspend = false
	k, err := mgr.Create(context.Background(), "tenant-1")
	require.NoError(t, err, "a re-create replaces the never-set-up board")
	mgr.waitBackground()
	assert.NotEqual(t, firstID, k.ID)
	assert.Contains(t, mock.RemoveVolumeCalls, "ah-kanban-"+firstID)
}

// Regression: when another Create already reserved the failed board, the
// reserve CAS matches no row; restartFailedInPlace must stop before Docker and
// must not mark the other caller's board failed.
func TestRestartFailedInPlace_LostReserveIsConflict(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)
	_, err := stateDB.Exec(
		`INSERT INTO kanbans (id, tenant_id, status, container_id, admin_token_encrypted, host, port, volume_name, url, created_at, updated_at)
		 VALUES ('kb', 'tenant-1', 'provisioning', '', 'enc', '127.0.0.1', 7251, 'vol', 'u', 1, 1)`)
	require.NoError(t, err)
	ran := false
	mock := &testutil.MockDockerClient{
		RunDatabaseFn: func(context.Context, docker.RunDatabaseConfig) (string, error) { ran = true; return "c2", nil },
	}
	mgr := newTestManager(t, stateDB, mock)

	_, err = mgr.restartFailedInPlace(context.Background(), "tenant-1", "kb", "vol")
	require.Error(t, err)
	assert.ErrorIs(t, err, apierr.ErrConflict)
	assert.False(t, ran, "no container is started for a board another caller owns")
	var status string
	require.NoError(t, stateDB.QueryRow(`SELECT status FROM kanbans WHERE id = 'kb'`).Scan(&status))
	assert.Equal(t, "provisioning", status)
}
