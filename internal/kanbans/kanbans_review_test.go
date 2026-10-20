package kanbans

import (
	"context"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A failed board that had completed setup must be restarted on its own volume
// and row; deleting the row would let GC remove the "orphaned" volume.
func TestCreate_RestartsSetUpFailedBoardInPlace(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)
	_, err := stateDB.Exec(
		`INSERT INTO kanbans (id, tenant_id, status, container_id, admin_token_encrypted, host, port, volume_name, url, created_at, updated_at)
		 VALUES ('kfail', 'tenant-1', 'failed', 'old-c', 'enc', '127.0.0.1', 7250, 'oldvol', 'u', 1, 1)`)
	require.NoError(t, err)
	var gotVolume string
	mock := &testutil.MockDockerClient{
		RunDatabaseFn: func(ctx context.Context, cfg docker.RunDatabaseConfig) (string, error) {
			gotVolume = cfg.VolumeName
			startFakeVikunja(t, cfg.HostPort)
			return "c-new", nil
		},
	}
	mgr := newTestManager(t, stateDB, mock)

	kb, err := mgr.Create(context.Background(), "tenant-1")
	require.NoError(t, err)
	assert.Equal(t, "kfail", kb.ID, "the existing row is reused")
	assert.Equal(t, "oldvol", gotVolume)
	waitForStatus(t, stateDB, "tenant-1", "ready", 10*time.Second)
	assert.Empty(t, mock.RemoveVolumeCalls)
	assert.Contains(t, mock.RemoveContainerCalls, "old-c")
	var tok string
	require.NoError(t, stateDB.QueryRow(`SELECT admin_token_encrypted FROM kanbans WHERE id='kfail'`).Scan(&tok))
	assert.Equal(t, "enc", tok, "stored admin credentials are kept")
}

func TestCasReady_DoesNotOverwriteStopped(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)
	_, err := stateDB.Exec(
		`INSERT INTO kanbans (id, tenant_id, status, host, port, volume_name, url, created_at, updated_at)
		 VALUES ('k1', 'tenant-1', 'stopped', '127.0.0.1', 7260, 'vol', 'u', 1, 1)`)
	require.NoError(t, err)
	mgr := newTestManager(t, stateDB, &testutil.MockDockerClient{})

	assert.False(t, mgr.casReady(context.Background(), "k1"))
	var status string
	require.NoError(t, stateDB.QueryRow(`SELECT status FROM kanbans WHERE id='k1'`).Scan(&status))
	assert.Equal(t, "stopped", status)
}
