package snapshots

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/crypto"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testMasterKey = []byte("test-master-key-32-bytes-long!!!")

// setupTest creates an in-memory DB, a mock Docker client, a snapshot Manager,
// seeds a tenant with quotas, and inserts a service with an image.
// Returns the manager, mock docker client, tenant ID, and service ID.
func setupTest(t *testing.T) (*Manager, *testutil.MockDockerClient, string, string) {
	t.Helper()

	db := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{}
	mgr := NewManager(db, mock, testMasterKey)

	tenantID := "tenant-1"
	serviceID := "svc-1"

	// Insert tenant.
	_, err := db.Exec(
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at) VALUES (?, ?, ?, 'active', 1, 1)`,
		tenantID, "Test Tenant", "test@example.com",
	)
	require.NoError(t, err)

	// Insert tenant quotas.
	_, err = db.Exec(
		`INSERT INTO tenant_quotas (tenant_id, max_services) VALUES (?, ?)`,
		tenantID, 10,
	)
	require.NoError(t, err)

	// Insert a service with an image.
	_, err = db.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, created_at, updated_at) VALUES (?, ?, ?, 'running', 'nginx:latest', 8080, ?, ?)`,
		serviceID, tenantID, "test-svc", time.Now().Unix(), time.Now().Unix(),
	)
	require.NoError(t, err)

	return mgr, mock, tenantID, serviceID
}

// setupTestWithDB is like setupTest but also returns the raw *sql.DB for direct manipulation.
func setupTestWithDB(t *testing.T) (*Manager, *testutil.MockDockerClient, *sql.DB, string, string) {
	t.Helper()

	db := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{}
	mgr := NewManager(db, mock, testMasterKey)

	tenantID := "tenant-1"
	serviceID := "svc-1"

	_, err := db.Exec(
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at) VALUES (?, ?, ?, 'active', 1, 1)`,
		tenantID, "Test Tenant", "test@example.com",
	)
	require.NoError(t, err)

	_, err = db.Exec(
		`INSERT INTO tenant_quotas (tenant_id, max_services) VALUES (?, ?)`,
		tenantID, 10,
	)
	require.NoError(t, err)

	_, err = db.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, created_at, updated_at) VALUES (?, ?, ?, 'running', 'nginx:latest', 8080, ?, ?)`,
		serviceID, tenantID, "test-svc", time.Now().Unix(), time.Now().Unix(),
	)
	require.NoError(t, err)

	return mgr, mock, db, tenantID, serviceID
}

func TestSnapshotCreate_Success(t *testing.T) {
	mgr, mock, tenantID, serviceID := setupTest(t)
	ctx := context.Background()

	snap, err := mgr.Create(ctx, tenantID, serviceID, CreateRequest{
		Name:        "my-snapshot",
		Description: "test snapshot",
	})
	require.NoError(t, err)

	assert.NotEmpty(t, snap.ID)
	assert.Equal(t, tenantID, snap.TenantID)
	assert.Equal(t, serviceID, snap.ServiceID)
	assert.Equal(t, "my-snapshot", snap.Name)
	assert.Equal(t, "test snapshot", snap.Description)
	assert.Contains(t, snap.ImageRef, "127.0.0.1:5000/snapshots/")
	assert.Equal(t, 8080, snap.Port)
	assert.NotZero(t, snap.CreatedAt)
	assert.NotEmpty(t, snap.ResourceConfig)

	// Verify TagImage was called with the correct source and target.
	require.Len(t, mock.TagImageCalls, 1)
	assert.Equal(t, "nginx:latest", mock.TagImageCalls[0][0])
	assert.Equal(t, snap.ImageRef, mock.TagImageCalls[0][1])
}

func TestSnapshotCreate_EmptyName(t *testing.T) {
	mgr, _, tenantID, serviceID := setupTest(t)
	ctx := context.Background()

	_, err := mgr.Create(ctx, tenantID, serviceID, CreateRequest{
		Name: "",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, apierr.ErrValidation))
}

func TestSnapshotCreate_ServiceNotFound(t *testing.T) {
	mgr, _, tenantID, _ := setupTest(t)
	ctx := context.Background()

	_, err := mgr.Create(ctx, tenantID, "nonexistent-svc", CreateRequest{
		Name: "snap",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, apierr.ErrNotFound))
}

func TestSnapshotCreate_NoImage(t *testing.T) {
	mgr, _, db, tenantID, _ := setupTestWithDB(t)
	ctx := context.Background()

	// Insert a service with no image.
	noImageSvcID := "svc-no-image"
	_, err := db.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, created_at, updated_at) VALUES (?, ?, ?, 'created', '', 8080, ?, ?)`,
		noImageSvcID, tenantID, "no-image-svc", time.Now().Unix(), time.Now().Unix(),
	)
	require.NoError(t, err)

	_, err = mgr.Create(ctx, tenantID, noImageSvcID, CreateRequest{
		Name: "snap",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, apierr.ErrConflict))
}

func TestSnapshotCreate_WithEnvVars(t *testing.T) {
	mgr, mock, db, tenantID, serviceID := setupTestWithDB(t)
	ctx := context.Background()

	// Encrypt and insert env vars for the service.
	envVars := map[string]string{
		"DB_HOST":     "localhost",
		"DB_PASSWORD": "secret123",
	}
	now := time.Now().Unix()
	for k, v := range envVars {
		encrypted, err := crypto.Encrypt([]byte(v), testMasterKey)
		require.NoError(t, err)
		_, err = db.Exec(
			`INSERT INTO service_env (service_id, key, value_encrypted, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
			serviceID, k, encrypted, now, now,
		)
		require.NoError(t, err)
	}

	snap, err := mgr.Create(ctx, tenantID, serviceID, CreateRequest{
		Name: "env-snapshot",
	})
	require.NoError(t, err)
	assert.NotEmpty(t, snap.ID)

	// Verify TagImage was called.
	require.Len(t, mock.TagImageCalls, 1)
	assert.Equal(t, "nginx:latest", mock.TagImageCalls[0][0])

	// Verify env vars are captured by restoring them.
	restored, err := mgr.RestoreEnvVars(ctx, tenantID, snap.ID)
	require.NoError(t, err)
	assert.Equal(t, "localhost", restored["DB_HOST"])
	assert.Equal(t, "secret123", restored["DB_PASSWORD"])
}

func TestSnapshotList_Empty(t *testing.T) {
	mgr, _, tenantID, _ := setupTest(t)
	ctx := context.Background()

	snapshots, err := mgr.List(ctx, tenantID, 100, 0)
	require.NoError(t, err)
	assert.NotNil(t, snapshots, "should return empty slice, not nil")
	assert.Len(t, snapshots, 0)
}

func TestSnapshotList_WithSnapshots(t *testing.T) {
	mgr, _, tenantID, serviceID := setupTest(t)
	ctx := context.Background()

	// Create 2 snapshots.
	snap1, err := mgr.Create(ctx, tenantID, serviceID, CreateRequest{Name: "snap-1"})
	require.NoError(t, err)

	// Small delay to ensure different created_at timestamps.
	time.Sleep(1100 * time.Millisecond)

	snap2, err := mgr.Create(ctx, tenantID, serviceID, CreateRequest{Name: "snap-2"})
	require.NoError(t, err)

	snapshots, err := mgr.List(ctx, tenantID, 100, 0)
	require.NoError(t, err)
	require.Len(t, snapshots, 2)

	// Should be ordered by created_at DESC (snap2 first).
	assert.Equal(t, snap2.ID, snapshots[0].ID)
	assert.Equal(t, snap1.ID, snapshots[1].ID)
}

func TestSnapshotList_Pagination(t *testing.T) {
	mgr, _, tenantID, serviceID := setupTest(t)
	ctx := context.Background()

	// Create 3 snapshots.
	for i := 0; i < 3; i++ {
		_, err := mgr.Create(ctx, tenantID, serviceID, CreateRequest{
			Name: "snap-" + string(rune('a'+i)),
		})
		require.NoError(t, err)
	}

	// Limit 2, offset 0.
	snapshots, err := mgr.List(ctx, tenantID, 2, 0)
	require.NoError(t, err)
	assert.Len(t, snapshots, 2)

	// Limit 10, offset 2.
	snapshots, err = mgr.List(ctx, tenantID, 10, 2)
	require.NoError(t, err)
	assert.Len(t, snapshots, 1)

	// Limit 10, offset 3 (past end).
	snapshots, err = mgr.List(ctx, tenantID, 10, 3)
	require.NoError(t, err)
	assert.Len(t, snapshots, 0)
}

func TestSnapshotGet_Success(t *testing.T) {
	mgr, _, tenantID, serviceID := setupTest(t)
	ctx := context.Background()

	created, err := mgr.Create(ctx, tenantID, serviceID, CreateRequest{
		Name:        "get-me",
		Description: "to be retrieved",
	})
	require.NoError(t, err)

	got, err := mgr.Get(ctx, tenantID, created.ID)
	require.NoError(t, err)

	assert.Equal(t, created.ID, got.ID)
	assert.Equal(t, created.TenantID, got.TenantID)
	assert.Equal(t, created.ServiceID, got.ServiceID)
	assert.Equal(t, "get-me", got.Name)
	assert.Equal(t, "to be retrieved", got.Description)
	assert.Equal(t, created.ImageRef, got.ImageRef)
	assert.Equal(t, created.Port, got.Port)
	assert.Equal(t, created.CreatedAt, got.CreatedAt)
}

func TestSnapshotGet_NotFound(t *testing.T) {
	mgr, _, tenantID, _ := setupTest(t)
	ctx := context.Background()

	_, err := mgr.Get(ctx, tenantID, "nonexistent-snap")
	require.Error(t, err)
	assert.True(t, errors.Is(err, apierr.ErrNotFound))
}

func TestSnapshotGet_WrongTenant(t *testing.T) {
	mgr, _, db, tenantID, serviceID := setupTestWithDB(t)
	ctx := context.Background()

	// Create a snapshot under tenant-1.
	snap, err := mgr.Create(ctx, tenantID, serviceID, CreateRequest{Name: "isolated"})
	require.NoError(t, err)

	// Insert a second tenant.
	_, err = db.Exec(
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at) VALUES (?, ?, ?, 'active', 1, 1)`,
		"tenant-2", "Other Tenant", "other@example.com",
	)
	require.NoError(t, err)

	// Attempt to get the snapshot as tenant-2 — should not be found.
	_, err = mgr.Get(ctx, "tenant-2", snap.ID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, apierr.ErrNotFound))
}

func TestSnapshotDelete_Success(t *testing.T) {
	mgr, _, tenantID, serviceID := setupTest(t)
	ctx := context.Background()

	snap, err := mgr.Create(ctx, tenantID, serviceID, CreateRequest{Name: "delete-me"})
	require.NoError(t, err)

	err = mgr.Delete(ctx, tenantID, snap.ID)
	require.NoError(t, err)

	// Verify it is gone.
	_, err = mgr.Get(ctx, tenantID, snap.ID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, apierr.ErrNotFound))
}

func TestSnapshotDelete_NotFound(t *testing.T) {
	mgr, _, tenantID, _ := setupTest(t)
	ctx := context.Background()

	err := mgr.Delete(ctx, tenantID, "nonexistent-snap")
	require.Error(t, err)
	assert.True(t, errors.Is(err, apierr.ErrNotFound))
}

func TestSnapshotRestoreEnvVars_Success(t *testing.T) {
	mgr, _, db, tenantID, serviceID := setupTestWithDB(t)
	ctx := context.Background()

	// Insert encrypted env vars.
	envVars := map[string]string{
		"API_KEY":    "abc123",
		"SECRET_KEY": "xyz789",
	}
	now := time.Now().Unix()
	for k, v := range envVars {
		encrypted, err := crypto.Encrypt([]byte(v), testMasterKey)
		require.NoError(t, err)
		_, err = db.Exec(
			`INSERT INTO service_env (service_id, key, value_encrypted, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
			serviceID, k, encrypted, now, now,
		)
		require.NoError(t, err)
	}

	// Create a snapshot that captures the env vars.
	snap, err := mgr.Create(ctx, tenantID, serviceID, CreateRequest{Name: "env-snap"})
	require.NoError(t, err)

	// Restore and verify decrypted values.
	restored, err := mgr.RestoreEnvVars(ctx, tenantID, snap.ID)
	require.NoError(t, err)
	assert.Len(t, restored, 2)
	assert.Equal(t, "abc123", restored["API_KEY"])
	assert.Equal(t, "xyz789", restored["SECRET_KEY"])
}

func TestSnapshotRestoreEnvVars_WrongTenant(t *testing.T) {
	mgr, _, db, tenantID, serviceID := setupTestWithDB(t)
	ctx := context.Background()

	snap, err := mgr.Create(ctx, tenantID, serviceID, CreateRequest{Name: "isolated-env"})
	require.NoError(t, err)

	// Insert a second tenant.
	_, err = db.Exec(
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at) VALUES (?, ?, ?, 'active', 1, 1)`,
		"tenant-2", "Other Tenant", "other@example.com",
	)
	require.NoError(t, err)

	// Attempt to restore as wrong tenant — should not be found.
	_, err = mgr.RestoreEnvVars(ctx, "tenant-2", snap.ID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, apierr.ErrNotFound))
}

func TestSnapshotRestoreEnvVars_Empty(t *testing.T) {
	mgr, _, tenantID, serviceID := setupTest(t)
	ctx := context.Background()

	// Create a snapshot with no env vars on the service.
	snap, err := mgr.Create(ctx, tenantID, serviceID, CreateRequest{Name: "no-env"})
	require.NoError(t, err)

	restored, err := mgr.RestoreEnvVars(ctx, tenantID, snap.ID)
	require.NoError(t, err)
	assert.NotNil(t, restored)
	assert.Empty(t, restored)
}
