package environments

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testTenantID = "tenant-1"

// seedTestData inserts a tenant and quota row needed by every test.
func seedTestData(t *testing.T, db *sql.DB, maxEnvironments int) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at) VALUES (?, ?, ?, 'active', 1, 1)`,
		testTenantID, "Tenant", "tenant@example.com",
	)
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO tenant_quotas (tenant_id, max_environments) VALUES (?, ?)`,
		testTenantID, maxEnvironments,
	)
	require.NoError(t, err)
}

// newTestManager creates a Manager backed by an in-memory DB and mock Docker client.
func newTestManager(t *testing.T, maxEnvs int) (*Manager, *testutil.MockDockerClient) {
	t.Helper()
	stateDB := testutil.NewStateDB(t)
	seedTestData(t, stateDB, maxEnvs)
	mock := &testutil.MockDockerClient{}
	mgr := NewManager(stateDB, mock)
	return mgr, mock
}

func validRequest() CreateRequest {
	return CreateRequest{Name: "my-env", BaseImage: "node:20"}
}

// ---- Tests ----

func TestCreateEnvironment(t *testing.T) {
	mgr, mock := newTestManager(t, 5)

	env, err := mgr.Create(context.Background(), testTenantID, validRequest())
	require.NoError(t, err)
	require.NotNil(t, env)

	assert.Equal(t, "running", env.Status)
	assert.Equal(t, testTenantID, env.TenantID)
	assert.Equal(t, "my-env", env.Name)
	assert.Equal(t, "node:20", env.BaseImage)
	assert.NotEmpty(t, env.ID)
	assert.NotEmpty(t, env.ContainerID)
	assert.NotEmpty(t, env.VolumeName)
	assert.Equal(t, 1800, env.IdleTimeoutSec) // default
	assert.NotNil(t, env.LastActivityAt)

	// Verify Docker interactions happened.
	assert.Len(t, mock.CreateVolumeCalls, 1)
	assert.Equal(t, 1, mock.RunDevEnvironmentCalls)
}

func TestCreateEnvironment_InvalidImage(t *testing.T) {
	mgr, _ := newTestManager(t, 5)

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{
		Name:      "my-env",
		BaseImage: "alpine:latest",
	})
	require.Error(t, err)
	assert.Nil(t, env)
	assert.True(t, errors.Is(err, apierr.ErrValidation), "expected validation error, got: %v", err)
	assert.ErrorContains(t, err, "invalid base_image")
}

func TestCreateEnvironment_InvalidName(t *testing.T) {
	mgr, _ := newTestManager(t, 5)

	// Empty name
	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{
		Name:      "",
		BaseImage: "node:20",
	})
	require.Error(t, err)
	assert.Nil(t, env)
	assert.True(t, errors.Is(err, apierr.ErrValidation))

	// Name too long (>128 chars)
	longName := "a"
	for len(longName) <= 128 {
		longName += "abcdefghij"
	}
	env, err = mgr.Create(context.Background(), testTenantID, CreateRequest{
		Name:      longName,
		BaseImage: "node:20",
	})
	require.Error(t, err)
	assert.Nil(t, env)
	assert.True(t, errors.Is(err, apierr.ErrValidation))
}

func TestCreateEnvironment_QuotaExceeded(t *testing.T) {
	mgr, _ := newTestManager(t, 1)

	// First creation should succeed.
	env1, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "env-1", BaseImage: "node:20"})
	require.NoError(t, err)
	require.NotNil(t, env1)

	// Second should fail with quota exceeded.
	env2, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "env-2", BaseImage: "node:20"})
	require.Error(t, err)
	assert.Nil(t, env2)
	assert.True(t, errors.Is(err, apierr.ErrQuotaExceeded), "expected quota exceeded error, got: %v", err)
	assert.ErrorContains(t, err, "environment quota exceeded (max 1)")
}

func TestGetEnvironment(t *testing.T) {
	mgr, _ := newTestManager(t, 5)

	created, err := mgr.Create(context.Background(), testTenantID, validRequest())
	require.NoError(t, err)

	got, err := mgr.Get(context.Background(), testTenantID, created.ID)
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, created.ID, got.ID)
	assert.Equal(t, created.Name, got.Name)
	assert.Equal(t, created.Status, got.Status)
	assert.Equal(t, created.BaseImage, got.BaseImage)
	assert.Equal(t, created.TenantID, got.TenantID)
}

func TestGetEnvironment_NotFound(t *testing.T) {
	mgr, _ := newTestManager(t, 5)

	got, err := mgr.Get(context.Background(), testTenantID, "nonexistent-id")
	require.Error(t, err)
	assert.Nil(t, got)
	assert.True(t, errors.Is(err, apierr.ErrNotFound), "expected not found error, got: %v", err)
}

func TestListEnvironments(t *testing.T) {
	mgr, _ := newTestManager(t, 5)

	_, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "env-a", BaseImage: "node:20"})
	require.NoError(t, err)
	_, err = mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "env-b", BaseImage: "python:3.12"})
	require.NoError(t, err)

	list, err := mgr.List(context.Background(), testTenantID)
	require.NoError(t, err)
	assert.Len(t, list, 2)

	// List returns newest first (ORDER BY created_at DESC).
	names := []string{list[0].Name, list[1].Name}
	assert.Contains(t, names, "env-a")
	assert.Contains(t, names, "env-b")
}

func TestDeleteEnvironment(t *testing.T) {
	mgr, mock := newTestManager(t, 5)

	created, err := mgr.Create(context.Background(), testTenantID, validRequest())
	require.NoError(t, err)

	err = mgr.Delete(context.Background(), testTenantID, created.ID)
	require.NoError(t, err)

	// Verify Docker cleanup happened.
	assert.NotEmpty(t, mock.StopContainerCalls)
	assert.NotEmpty(t, mock.RemoveContainerCalls)
	assert.NotEmpty(t, mock.RemoveVolumeCalls)

	// Get should now return not found.
	got, err := mgr.Get(context.Background(), testTenantID, created.ID)
	require.Error(t, err)
	assert.Nil(t, got)
	assert.True(t, errors.Is(err, apierr.ErrNotFound))
}

func TestStopEnvironment(t *testing.T) {
	mgr, mock := newTestManager(t, 5)

	created, err := mgr.Create(context.Background(), testTenantID, validRequest())
	require.NoError(t, err)
	assert.Equal(t, "running", created.Status)

	err = mgr.Stop(context.Background(), testTenantID, created.ID)
	require.NoError(t, err)

	got, err := mgr.Get(context.Background(), testTenantID, created.ID)
	require.NoError(t, err)
	assert.Equal(t, "stopped", got.Status)
	assert.NotEmpty(t, mock.StopContainerCalls)
}

func TestStartEnvironment(t *testing.T) {
	mgr, mock := newTestManager(t, 5)

	created, err := mgr.Create(context.Background(), testTenantID, validRequest())
	require.NoError(t, err)

	err = mgr.Stop(context.Background(), testTenantID, created.ID)
	require.NoError(t, err)

	err = mgr.Start(context.Background(), testTenantID, created.ID)
	require.NoError(t, err)

	got, err := mgr.Get(context.Background(), testTenantID, created.ID)
	require.NoError(t, err)
	assert.Equal(t, "running", got.Status)
	assert.NotEmpty(t, mock.StartContainerCalls)
}

func TestStopEnvironment_NotRunning(t *testing.T) {
	mgr, _ := newTestManager(t, 5)

	created, err := mgr.Create(context.Background(), testTenantID, validRequest())
	require.NoError(t, err)

	// Stop it first.
	err = mgr.Stop(context.Background(), testTenantID, created.ID)
	require.NoError(t, err)

	// Stopping again should return conflict.
	err = mgr.Stop(context.Background(), testTenantID, created.ID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, apierr.ErrConflict), "expected conflict error, got: %v", err)
	assert.ErrorContains(t, err, "not running")
}

func TestGetContainerID(t *testing.T) {
	mgr, _ := newTestManager(t, 5)

	created, err := mgr.Create(context.Background(), testTenantID, validRequest())
	require.NoError(t, err)

	containerID, err := mgr.GetContainerID(context.Background(), testTenantID, created.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, containerID)
	assert.Equal(t, created.ContainerID, containerID)
}

func TestTouchActivity(t *testing.T) {
	mgr, _ := newTestManager(t, 5)

	created, err := mgr.Create(context.Background(), testTenantID, validRequest())
	require.NoError(t, err)

	// Get original activity timestamp.
	before, err := mgr.Get(context.Background(), testTenantID, created.ID)
	require.NoError(t, err)
	require.NotNil(t, before.LastActivityAt)
	originalActivity := *before.LastActivityAt

	// Touch activity (timestamps have second granularity so the value
	// will be >= the original).
	mgr.TouchActivity(context.Background(), created.ID)

	after, err := mgr.Get(context.Background(), testTenantID, created.ID)
	require.NoError(t, err)
	require.NotNil(t, after.LastActivityAt)
	assert.GreaterOrEqual(t, *after.LastActivityAt, originalActivity)
}

// TestCreateEnvironment_RunDevEnvConfig verifies the Docker config passed.
func TestCreateEnvironment_RunDevEnvConfig(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedTestData(t, stateDB, 5)

	var captured docker.RunDevEnvConfig
	mock := &testutil.MockDockerClient{
		RunDevEnvironmentFn: func(ctx context.Context, cfg docker.RunDevEnvConfig) (string, error) {
			captured = cfg
			return "container-abc", nil
		},
	}
	mgr := NewManager(stateDB, mock)

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{
		Name:      "my-env",
		BaseImage: "golang:1.25",
	})
	require.NoError(t, err)

	assert.Equal(t, testTenantID, captured.TenantID)
	assert.Equal(t, env.ID, captured.EnvID)
	assert.Equal(t, "golang:1.25-bookworm", captured.Image)
	assert.Contains(t, captured.VolumeName, "ah-env-")
}
