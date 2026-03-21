package deployments

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedTestData inserts the tenant and service rows required by the deployments
// foreign key on service_id. Returns the tenant ID and service ID.
func seedTestData(t *testing.T, db *sql.DB, tenantID, serviceID string) {
	t.Helper()
	now := time.Now().Unix()

	_, err := db.Exec(
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at) VALUES (?, ?, ?, 'active', ?, ?)`,
		tenantID, "Test Tenant", tenantID+"@example.com", now, now,
	)
	require.NoError(t, err)

	_, err = db.Exec(
		`INSERT INTO tenant_quotas (tenant_id, max_services) VALUES (?, ?)`,
		tenantID, 10,
	)
	require.NoError(t, err)

	_, err = db.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, created_at, updated_at) VALUES (?, ?, ?, 'running', 'nginx:latest', 8080, ?, ?)`,
		serviceID, tenantID, "test-svc", now, now,
	)
	require.NoError(t, err)
}

func TestCreate_InsertsRecord(t *testing.T) {
	db := testutil.NewStateDB(t)
	seedTestData(t, db, "tenant-1", "svc-1")
	store := NewStore(db)
	ctx := context.Background()

	now := time.Now().Unix()
	completedAt := now + 5
	d := &Deployment{
		ServiceID:    "svc-1",
		TenantID:     "tenant-1",
		BuildID:      "build-abc",
		Image:        "nginx:latest",
		Status:       StatusDeploying,
		Trigger:      TriggerManual,
		ContainerID:  "container-xyz",
		ErrorMessage: "",
		StartedAt:    now,
		CompletedAt:  &completedAt,
	}

	err := store.Create(ctx, d)
	require.NoError(t, err)
	assert.NotEmpty(t, d.ID, "ID should be auto-generated")
	assert.NotZero(t, d.CreatedAt, "CreatedAt should be auto-set")

	// Read back via Get.
	got, err := store.Get(ctx, "tenant-1", d.ID)
	require.NoError(t, err)

	assert.Equal(t, d.ID, got.ID)
	assert.Equal(t, "svc-1", got.ServiceID)
	assert.Equal(t, "tenant-1", got.TenantID)
	assert.Equal(t, "build-abc", got.BuildID)
	assert.Equal(t, "nginx:latest", got.Image)
	assert.Equal(t, StatusDeploying, got.Status)
	assert.Equal(t, TriggerManual, got.Trigger)
	assert.Equal(t, "container-xyz", got.ContainerID)
	assert.Equal(t, "", got.ErrorMessage)
	assert.Equal(t, now, got.StartedAt)
	require.NotNil(t, got.CompletedAt)
	assert.Equal(t, completedAt, *got.CompletedAt)
	assert.Equal(t, d.CreatedAt, got.CreatedAt)
}

func TestCreate_DuplicateID_Fails(t *testing.T) {
	db := testutil.NewStateDB(t)
	seedTestData(t, db, "tenant-1", "svc-1")
	store := NewStore(db)
	ctx := context.Background()

	now := time.Now().Unix()
	d := &Deployment{
		ID:        "fixed-id",
		ServiceID: "svc-1",
		TenantID:  "tenant-1",
		Image:     "nginx:latest",
		Status:    StatusPending,
		Trigger:   TriggerManual,
		StartedAt: now,
		CreatedAt: now,
	}

	err := store.Create(ctx, d)
	require.NoError(t, err)

	// Second insert with same ID should fail.
	d2 := &Deployment{
		ID:        "fixed-id",
		ServiceID: "svc-1",
		TenantID:  "tenant-1",
		Image:     "nginx:latest",
		Status:    StatusPending,
		Trigger:   TriggerManual,
		StartedAt: now,
		CreatedAt: now,
	}
	err = store.Create(ctx, d2)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UNIQUE constraint failed")
}

func TestUpdateStatus_Running(t *testing.T) {
	db := testutil.NewStateDB(t)
	seedTestData(t, db, "tenant-1", "svc-1")
	store := NewStore(db)
	ctx := context.Background()

	now := time.Now().Unix()
	d := &Deployment{
		ServiceID: "svc-1",
		TenantID:  "tenant-1",
		Image:     "nginx:latest",
		Status:    StatusDeploying,
		Trigger:   TriggerManual,
		StartedAt: now,
	}
	require.NoError(t, store.Create(ctx, d))

	completedAt := now + 10
	err := store.UpdateStatus(ctx, d.ID, StatusRunning,
		WithContainerID("container-123"),
		WithCompletedAt(completedAt),
	)
	require.NoError(t, err)

	got, err := store.Get(ctx, "tenant-1", d.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusRunning, got.Status)
	assert.Equal(t, "container-123", got.ContainerID)
	require.NotNil(t, got.CompletedAt)
	assert.Equal(t, completedAt, *got.CompletedAt)
}

func TestUpdateStatus_Failed(t *testing.T) {
	db := testutil.NewStateDB(t)
	seedTestData(t, db, "tenant-1", "svc-1")
	store := NewStore(db)
	ctx := context.Background()

	now := time.Now().Unix()
	d := &Deployment{
		ServiceID: "svc-1",
		TenantID:  "tenant-1",
		Image:     "nginx:latest",
		Status:    StatusDeploying,
		Trigger:   TriggerManual,
		StartedAt: now,
	}
	require.NoError(t, store.Create(ctx, d))

	completedAt := now + 5
	err := store.UpdateStatus(ctx, d.ID, StatusFailed,
		WithError("image pull failed: timeout"),
		WithCompletedAt(completedAt),
	)
	require.NoError(t, err)

	got, err := store.Get(ctx, "tenant-1", d.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusFailed, got.Status)
	assert.Equal(t, "image pull failed: timeout", got.ErrorMessage)
	require.NotNil(t, got.CompletedAt)
	assert.Equal(t, completedAt, *got.CompletedAt)
}

func TestUpdateStatus_NotFound(t *testing.T) {
	db := testutil.NewStateDB(t)
	store := NewStore(db)
	ctx := context.Background()

	err := store.UpdateStatus(ctx, "nonexistent-id", StatusRunning)
	require.Error(t, err)
	assert.True(t, errors.Is(err, apierr.ErrNotFound))
}

func TestListByService_Pagination(t *testing.T) {
	db := testutil.NewStateDB(t)
	seedTestData(t, db, "tenant-1", "svc-1")
	store := NewStore(db)
	ctx := context.Background()

	// Create 5 deployments with increasing created_at.
	var ids []string
	for i := 0; i < 5; i++ {
		d := &Deployment{
			ServiceID: "svc-1",
			TenantID:  "tenant-1",
			Image:     "nginx:latest",
			Status:    StatusRunning,
			Trigger:   TriggerManual,
			StartedAt: int64(1000 + i),
			CreatedAt: int64(1000 + i),
		}
		require.NoError(t, store.Create(ctx, d))
		ids = append(ids, d.ID)
	}

	// First page: limit 2, offset 0.
	page1, err := store.ListByService(ctx, "tenant-1", "svc-1", 2, 0)
	require.NoError(t, err)
	require.Len(t, page1, 2)

	// Should be newest first (id[4], id[3]).
	assert.Equal(t, ids[4], page1[0].ID)
	assert.Equal(t, ids[3], page1[1].ID)

	// Second page: limit 2, offset 2.
	page2, err := store.ListByService(ctx, "tenant-1", "svc-1", 2, 2)
	require.NoError(t, err)
	require.Len(t, page2, 2)
	assert.Equal(t, ids[2], page2[0].ID)
	assert.Equal(t, ids[1], page2[1].ID)

	// Third page: limit 2, offset 4.
	page3, err := store.ListByService(ctx, "tenant-1", "svc-1", 2, 4)
	require.NoError(t, err)
	require.Len(t, page3, 1)
	assert.Equal(t, ids[0], page3[0].ID)

	// Past end: offset 5.
	page4, err := store.ListByService(ctx, "tenant-1", "svc-1", 2, 5)
	require.NoError(t, err)
	assert.Len(t, page4, 0)
}

func TestListByService_TenantIsolation(t *testing.T) {
	db := testutil.NewStateDB(t)
	seedTestData(t, db, "tenant-1", "svc-1")
	store := NewStore(db)
	ctx := context.Background()

	// Seed a second tenant with its own service.
	seedTestData(t, db, "tenant-2", "svc-2")

	// Create a deployment for tenant-1.
	d1 := &Deployment{
		ServiceID: "svc-1",
		TenantID:  "tenant-1",
		Image:     "nginx:latest",
		Status:    StatusRunning,
		Trigger:   TriggerManual,
		StartedAt: time.Now().Unix(),
	}
	require.NoError(t, store.Create(ctx, d1))

	// Create a deployment for tenant-2.
	d2 := &Deployment{
		ServiceID: "svc-2",
		TenantID:  "tenant-2",
		Image:     "redis:latest",
		Status:    StatusRunning,
		Trigger:   TriggerBuild,
		StartedAt: time.Now().Unix(),
	}
	require.NoError(t, store.Create(ctx, d2))

	// tenant-1 should only see their own deployment.
	list, err := store.ListByService(ctx, "tenant-1", "svc-1", 50, 0)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, d1.ID, list[0].ID)

	// tenant-2 should not see tenant-1's deployments even if they know the service ID.
	list, err = store.ListByService(ctx, "tenant-2", "svc-1", 50, 0)
	require.NoError(t, err)
	assert.Len(t, list, 0)
}

func TestListByTenant_CrossService(t *testing.T) {
	db := testutil.NewStateDB(t)
	seedTestData(t, db, "tenant-1", "svc-1")
	store := NewStore(db)
	ctx := context.Background()

	// Add a second service for the same tenant.
	now := time.Now().Unix()
	_, err := db.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, created_at, updated_at) VALUES (?, ?, ?, 'running', 'redis:latest', 6379, ?, ?)`,
		"svc-2", "tenant-1", "test-svc-2", now, now,
	)
	require.NoError(t, err)

	// Create deployments across both services.
	d1 := &Deployment{
		ServiceID: "svc-1",
		TenantID:  "tenant-1",
		Image:     "nginx:latest",
		Status:    StatusRunning,
		Trigger:   TriggerManual,
		StartedAt: 1000,
		CreatedAt: 1000,
	}
	require.NoError(t, store.Create(ctx, d1))

	d2 := &Deployment{
		ServiceID: "svc-2",
		TenantID:  "tenant-1",
		Image:     "redis:latest",
		Status:    StatusRunning,
		Trigger:   TriggerBuild,
		StartedAt: 2000,
		CreatedAt: 2000,
	}
	require.NoError(t, store.Create(ctx, d2))

	list, err := store.ListByTenant(ctx, "tenant-1", 50, 0)
	require.NoError(t, err)
	require.Len(t, list, 2)

	// Newest first.
	assert.Equal(t, d2.ID, list[0].ID)
	assert.Equal(t, "svc-2", list[0].ServiceID)
	assert.Equal(t, d1.ID, list[1].ID)
	assert.Equal(t, "svc-1", list[1].ServiceID)
}

func TestLatestForService(t *testing.T) {
	db := testutil.NewStateDB(t)
	seedTestData(t, db, "tenant-1", "svc-1")
	store := NewStore(db)
	ctx := context.Background()

	// Create multiple deployments with different timestamps.
	d1 := &Deployment{
		ServiceID: "svc-1",
		TenantID:  "tenant-1",
		Image:     "nginx:1.0",
		Status:    StatusFailed,
		Trigger:   TriggerManual,
		StartedAt: 1000,
		CreatedAt: 1000,
	}
	require.NoError(t, store.Create(ctx, d1))

	d2 := &Deployment{
		ServiceID: "svc-1",
		TenantID:  "tenant-1",
		Image:     "nginx:2.0",
		Status:    StatusRunning,
		Trigger:   TriggerBuild,
		StartedAt: 2000,
		CreatedAt: 2000,
	}
	require.NoError(t, store.Create(ctx, d2))

	latest, err := store.LatestForService(ctx, "svc-1")
	require.NoError(t, err)
	assert.Equal(t, d2.ID, latest.ID)
	assert.Equal(t, "nginx:2.0", latest.Image)
	assert.Equal(t, StatusRunning, latest.Status)
}

func TestLatestForService_NotFound(t *testing.T) {
	db := testutil.NewStateDB(t)
	store := NewStore(db)
	ctx := context.Background()

	_, err := store.LatestForService(ctx, "nonexistent-svc")
	require.Error(t, err)
	assert.True(t, errors.Is(err, apierr.ErrNotFound))
}

func TestCascadeDelete(t *testing.T) {
	db := testutil.NewStateDB(t)
	seedTestData(t, db, "tenant-1", "svc-1")
	store := NewStore(db)
	ctx := context.Background()

	// Create a deployment.
	d := &Deployment{
		ServiceID: "svc-1",
		TenantID:  "tenant-1",
		Image:     "nginx:latest",
		Status:    StatusRunning,
		Trigger:   TriggerManual,
		StartedAt: time.Now().Unix(),
	}
	require.NoError(t, store.Create(ctx, d))

	// Verify deployment exists.
	got, err := store.Get(ctx, "tenant-1", d.ID)
	require.NoError(t, err)
	assert.Equal(t, d.ID, got.ID)

	// Delete the service row — should CASCADE to deployments.
	_, err = db.ExecContext(ctx, `DELETE FROM services WHERE id = ?`, "svc-1")
	require.NoError(t, err)

	// Verify deployment is also deleted.
	_, err = store.Get(ctx, "tenant-1", d.ID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, apierr.ErrNotFound))

	// Also verify via direct count.
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM deployments WHERE service_id = ?`, "svc-1").Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func TestGet_WrongTenant(t *testing.T) {
	db := testutil.NewStateDB(t)
	seedTestData(t, db, "tenant-1", "svc-1")
	seedTestData(t, db, "tenant-2", "svc-2")
	store := NewStore(db)
	ctx := context.Background()

	d := &Deployment{
		ServiceID: "svc-1",
		TenantID:  "tenant-1",
		Image:     "nginx:latest",
		Status:    StatusRunning,
		Trigger:   TriggerManual,
		StartedAt: time.Now().Unix(),
	}
	require.NoError(t, store.Create(ctx, d))

	// tenant-2 should not be able to retrieve tenant-1's deployment.
	_, err := store.Get(ctx, "tenant-2", d.ID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, apierr.ErrNotFound))
}

func TestCreate_NullBuildID(t *testing.T) {
	db := testutil.NewStateDB(t)
	seedTestData(t, db, "tenant-1", "svc-1")
	store := NewStore(db)
	ctx := context.Background()

	d := &Deployment{
		ServiceID: "svc-1",
		TenantID:  "tenant-1",
		Image:     "nginx:latest",
		Status:    StatusPending,
		Trigger:   TriggerManual,
		StartedAt: time.Now().Unix(),
	}
	require.NoError(t, store.Create(ctx, d))

	got, err := store.Get(ctx, "tenant-1", d.ID)
	require.NoError(t, err)
	assert.Equal(t, "", got.BuildID, "BuildID should be empty when not set")
}

func TestListByService_EmptyResult(t *testing.T) {
	db := testutil.NewStateDB(t)
	seedTestData(t, db, "tenant-1", "svc-1")
	store := NewStore(db)
	ctx := context.Background()

	list, err := store.ListByService(ctx, "tenant-1", "svc-1", 50, 0)
	require.NoError(t, err)
	assert.NotNil(t, list, "should return empty slice, not nil")
	assert.Len(t, list, 0)
}

func TestListByTenant_EmptyResult(t *testing.T) {
	db := testutil.NewStateDB(t)
	seedTestData(t, db, "tenant-1", "svc-1")
	store := NewStore(db)
	ctx := context.Background()

	list, err := store.ListByTenant(ctx, "tenant-1", 50, 0)
	require.NoError(t, err)
	assert.NotNil(t, list, "should return empty slice, not nil")
	assert.Len(t, list, 0)
}
