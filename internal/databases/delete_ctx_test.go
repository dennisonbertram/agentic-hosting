package databases

import (
	"context"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A slow wipe must not starve volume removal: when the request context is
// cancelled mid-wipe, Delete still removes the volume and the record.
func TestDelete_SurvivesRequestContextCancel(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedDatabaseTestData(t, stateDB, 5)
	dockerClient := &testutil.MockDockerClient{}
	mgr := NewManager(stateDB, dockerClient, []byte("0123456789abcdef0123456789abcdef"))
	_, err := stateDB.Exec(`INSERT INTO databases
		(id, tenant_id, name, type, status, container_id, port, db_name, username, password_encrypted, volume_name, created_at, updated_at)
		VALUES ('slow', 'tenant-1', 'slow', 'redis', 'ready', NULL, 16380, '', '', 'encrypted', 'volume-slow', 1, 1)`)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	dockerClient.WipeVolumeFn = func(wctx context.Context, _ string) error {
		cancel() // the request is gone while the wipe runs
		time.Sleep(20 * time.Millisecond)
		return wctx.Err()
	}
	dockerClient.RemoveVolumeFn = func(rctx context.Context, _ string) error { return rctx.Err() }

	require.NoError(t, mgr.Delete(ctx, "tenant-1", "slow"))
	assert.Equal(t, []string{"volume-slow"}, dockerClient.RemoveVolumeCalls)
	var count int
	require.NoError(t, stateDB.QueryRow(`SELECT COUNT(*) FROM databases WHERE id = 'slow'`).Scan(&count))
	assert.Zero(t, count)
}
