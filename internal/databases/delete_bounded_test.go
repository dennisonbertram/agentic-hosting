package databases

import (
	"context"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/require"
)

// A stalled Docker daemon must not hang Delete: container stop+remove have
// their own bounded context.
func TestDelete_ContainerStopIsBounded(t *testing.T) {
	old := containerCleanupTimeout
	containerCleanupTimeout = 50 * time.Millisecond
	t.Cleanup(func() { containerCleanupTimeout = old })

	stateDB := testutil.NewStateDB(t)
	seedDatabaseTestData(t, stateDB, 5)
	dockerClient := &testutil.MockDockerClient{}
	mgr := NewManager(stateDB, dockerClient, []byte("0123456789abcdef0123456789abcdef"))
	_, err := stateDB.Exec(`INSERT INTO databases
		(id, tenant_id, name, type, status, container_id, port, db_name, username, password_encrypted, volume_name, created_at, updated_at)
		VALUES ('hung', 'tenant-1', 'hung', 'redis', 'ready', 'ctr-hung', 16381, '', '', 'encrypted', 'volume-hung', 1, 1)`)
	require.NoError(t, err)
	dockerClient.StopContainerFn = func(ctx context.Context, _ string) error { <-ctx.Done(); return ctx.Err() }
	dockerClient.RemoveContainerFn = func(ctx context.Context, _ string) error { return ctx.Err() }

	done := make(chan error, 1)
	go func() { done <- mgr.Delete(context.Background(), "tenant-1", "hung") }()
	select {
	case err := <-done:
		require.Error(t, err) // container removal failed, record is kept
	case <-time.After(5 * time.Second):
		t.Fatal("Delete hung on a stalled StopContainer")
	}
}
