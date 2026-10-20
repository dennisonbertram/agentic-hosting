package databases

import (
	"context"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression: a tenant suspension that lands before Create records the
// container_id stops the row without removing anything. Create must then remove
// the container itself (keeping the volume) instead of leaving it running.
func TestCreate_SuspendedBeforeContainerIDRemovesContainer(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedDatabaseTestData(t, stateDB, 5)
	mock := &testutil.MockDockerClient{}
	m := NewManager(stateDB, mock, testKey)
	mock.RunDatabaseFn = func(ctx context.Context, cfg docker.RunDatabaseConfig) (string, error) {
		// StopAllForTenant runs during the image pull: no container_id yet.
		m.StopAllForTenant(context.Background(), "tenant-1")
		return "c-orphan", nil
	}

	_, err := m.Create(context.Background(), "tenant-1", CreateRequest{Name: "susp", Type: "postgres"})
	require.Error(t, err)
	assert.ErrorIs(t, err, apierr.ErrConflict)
	assert.Contains(t, mock.RemoveContainerCalls, "c-orphan")
	assert.Empty(t, mock.RemoveVolumeCalls, "the volume is kept for reactivation")

	var status, cid string
	require.NoError(t, stateDB.QueryRow(`SELECT status, COALESCE(container_id, '') FROM databases WHERE name = 'susp'`).Scan(&status, &cid))
	assert.Equal(t, "stopped", status)
	assert.Empty(t, cid)
}
