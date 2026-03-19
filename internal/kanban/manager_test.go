package kanban_test

import (
	"context"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/kanban"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// masterKey is a 32-byte test key.
var masterKey = []byte("0123456789abcdef0123456789abcdef")

func setupManager(t *testing.T) (*kanban.Manager, *testutil.MockDockerClient) {
	t.Helper()
	db := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{}

	// Insert a test tenant so FK constraints pass
	_, err := db.Exec(`INSERT INTO tenants (id, name, email, created_at, updated_at) VALUES ('tenant-12345678', 'test', 'test@test.com', 1, 1)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO tenant_quotas (tenant_id, max_services, max_databases) VALUES ('tenant-12345678', 5, 5)`)
	require.NoError(t, err)

	mgr := kanban.NewManager(db, mock, masterKey, "kanban.test.local")
	// Skip real TCP health check in tests
	mgr.SetHealthCheck(func(port int, timeout time.Duration) bool { return true })
	return mgr, mock
}

func TestCreateAndGet(t *testing.T) {
	mgr, mock := setupManager(t)

	// Mock RunDatabase to return a container ID without actually running Docker
	mock.RunDatabaseFn = func(ctx context.Context, cfg docker.RunDatabaseConfig) (string, error) {
		return "ctr-vikunja-123", nil
	}

	inst, err := mgr.Create(context.Background(), "tenant-12345678", kanban.CreateRequest{Name: "myboard"})
	require.NoError(t, err)
	assert.Equal(t, "ready", inst.Status)
	assert.NotEmpty(t, inst.APIToken)
	assert.Contains(t, inst.URL, "myboard-tenant-1")
	assert.Contains(t, inst.URL, "kanban.test.local")

	// Get by tenant should return same instance
	got, err := mgr.GetByTenant(context.Background(), "tenant-12345678")
	require.NoError(t, err)
	assert.Equal(t, inst.ID, got.ID)
	assert.Empty(t, got.APIToken) // not returned on Get

	// Get by ID
	got2, err := mgr.Get(context.Background(), "tenant-12345678", inst.ID)
	require.NoError(t, err)
	assert.Equal(t, inst.ID, got2.ID)

	// GetAPIToken
	token, err := mgr.GetAPIToken(context.Background(), "tenant-12345678", inst.ID)
	require.NoError(t, err)
	assert.Equal(t, inst.APIToken, token)
}

func TestCreateDuplicate(t *testing.T) {
	mgr, _ := setupManager(t)

	_, err := mgr.Create(context.Background(), "tenant-12345678", kanban.CreateRequest{Name: "board1"})
	require.NoError(t, err)

	// Second create should fail
	_, err = mgr.Create(context.Background(), "tenant-12345678", kanban.CreateRequest{Name: "board2"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already has an active kanban instance")
}

func TestCreateValidation(t *testing.T) {
	mgr, _ := setupManager(t)

	// Empty name
	_, err := mgr.Create(context.Background(), "tenant-12345678", kanban.CreateRequest{Name: ""})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "name is required")

	// Name too long
	longName := make([]byte, 129)
	for i := range longName {
		longName[i] = 'a'
	}
	_, err = mgr.Create(context.Background(), "tenant-12345678", kanban.CreateRequest{Name: string(longName)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "name is required")
}

func TestDelete(t *testing.T) {
	mgr, mock := setupManager(t)

	inst, err := mgr.Create(context.Background(), "tenant-12345678", kanban.CreateRequest{Name: "todelete"})
	require.NoError(t, err)

	err = mgr.Delete(context.Background(), "tenant-12345678", inst.ID)
	require.NoError(t, err)

	// Verify Docker cleanup was called
	assert.NotEmpty(t, mock.RemoveVolumeCalls)

	// Should not find it anymore
	_, err = mgr.Get(context.Background(), "tenant-12345678", inst.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestDeleteNotFound(t *testing.T) {
	mgr, _ := setupManager(t)

	err := mgr.Delete(context.Background(), "tenant-12345678", "nonexistent-id")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestGetByTenantNotFound(t *testing.T) {
	mgr, _ := setupManager(t)

	_, err := mgr.GetByTenant(context.Background(), "tenant-12345678")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no kanban instance")
}
