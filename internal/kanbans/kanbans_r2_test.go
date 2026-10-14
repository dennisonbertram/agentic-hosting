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

const kanbanID16 = "0123456789abcdef0123"

func insertStoppedKanban(t *testing.T, mgr *Manager) {
	t.Helper()
	_, err := mgr.db.Exec(
		`INSERT INTO kanbans (id, tenant_id, status, host, port, volume_name, url, created_at, updated_at)
		 VALUES (?, 'tenant-1', 'stopped', '127.0.0.1', 7391, 'vol', 'u', 1, 1)`, kanbanID16)
	require.NoError(t, err)
}

func TestStartForTenant_HealthFailureRemovesContainer(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)
	mock := &testutil.MockDockerClient{
		RunDatabaseFn: func(ctx context.Context, cfg docker.RunDatabaseConfig) (string, error) { return "c-new", nil },
	}
	mgr := newTestManager(t, stateDB, mock)
	mgr.healthCheckTimeout = 300 * time.Millisecond // nothing listens on the port
	insertStoppedKanban(t, mgr)

	mgr.StartForTenant(context.Background(), "tenant-1")

	var status string
	require.NoError(t, stateDB.QueryRow(`SELECT status FROM kanbans WHERE id=?`, kanbanID16).Scan(&status))
	assert.Equal(t, "failed", status)
	assert.Contains(t, mock.RemoveContainerCalls, "c-new")
	assert.Empty(t, mock.RemoveVolumeCalls, "volume holds data and must be kept")
}

func TestStartForTenant_SkipsSuspendedTenant(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)
	mock := &testutil.MockDockerClient{}
	mgr := newTestManager(t, stateDB, mock)
	insertStoppedKanban(t, mgr)
	_, err := stateDB.Exec(`UPDATE tenants SET status='suspended'`)
	require.NoError(t, err)

	mgr.StartForTenant(context.Background(), "tenant-1")

	assert.Equal(t, 0, mock.RunDatabaseCalls, "must not recreate a container for a suspended tenant")
}

func TestCreate_FailedRowCleanup(t *testing.T) {
	for _, tc := range []struct {
		name       string
		adminToken string
		wantVolume bool
	}{
		{"first-time failure deletes volume", "NULL", true},
		{"previously working board keeps volume", "'enc'", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDB := testutil.NewStateDB(t)
			seedKanbanTestData(t, stateDB)
			_, err := stateDB.Exec(
				`INSERT INTO kanbans (id, tenant_id, status, container_id, admin_token_encrypted, host, port, volume_name, url, created_at, updated_at)
				 VALUES ('kfail', 'tenant-1', 'failed', 'old-c', ` + tc.adminToken + `, '127.0.0.1', 7200, 'oldvol', 'u', 1, 1)`)
			require.NoError(t, err)
			mock := &testutil.MockDockerClient{
				RunDatabaseFn: func(ctx context.Context, cfg docker.RunDatabaseConfig) (string, error) {
					return "", assert.AnError // only cleanup of the old row is under test
				},
			}
			mgr := newTestManager(t, stateDB, mock)
			_, _ = mgr.Create(context.Background(), "tenant-1")

			assert.Contains(t, mock.RemoveContainerCalls, "old-c")
			if tc.wantVolume {
				assert.Contains(t, mock.RemoveVolumeCalls, "oldvol")
			} else {
				assert.NotContains(t, mock.RemoveVolumeCalls, "oldvol")
			}
		})
	}
}
