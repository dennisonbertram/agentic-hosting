package environments

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testTenantID = "t_test_tenant_001"

// seedTenant inserts a tenant and tenant_quotas row with the given max_environments.
func seedTenant(t *testing.T, db *sql.DB, tenantID string, maxEnvs int) {
	t.Helper()
	now := time.Now().Unix()
	_, err := db.Exec(
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
		 VALUES (?, ?, ?, 'active', ?, ?)`,
		tenantID, "Test Tenant", tenantID+"@test.com", now, now,
	)
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO tenant_quotas (tenant_id, max_services, max_databases, max_memory_mb, max_cpu_cores, max_disk_gb, api_rate_limit, max_environments)
		 VALUES (?, 5, 3, 2048, 2.0, 20, 100, ?)`,
		tenantID, maxEnvs,
	)
	require.NoError(t, err)
}

func newTestManager(t *testing.T, maxEnvs int) (*Manager, *testutil.MockDockerClient) {
	t.Helper()
	db := testutil.NewStateDB(t)
	seedTenant(t, db, testTenantID, maxEnvs)

	mock := &testutil.MockDockerClient{}
	mgr := NewManager(db, mock)
	return mgr, mock
}

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

func TestCreateEnvironment_Success(t *testing.T) {
	mgr, mock := newTestManager(t, 5)

	mock.RunEnvironmentFn = func(ctx context.Context, cfg docker.RunEnvironmentConfig) (string, error) {
		return "container-123", nil
	}

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{
		Name: "my-env",
	})
	require.NoError(t, err)
	assert.Equal(t, "my-env", env.Name)
	assert.Equal(t, "running", env.Status)
	assert.Equal(t, "default", env.TemplateID)
	assert.Equal(t, "container-123", env.ContainerID)
	assert.Equal(t, 3600, env.LeaseDurationSeconds)
	assert.NotNil(t, env.LeaseExpiresAt)
	assert.NotEmpty(t, env.ID)
	assert.Equal(t, 1, mock.RunEnvironmentCalls)
	assert.Len(t, mock.EnsureNetworkCalls, 1)
	assert.Len(t, mock.CreateVolumeCalls, 1)
}

func TestCreateEnvironment_QuotaExceeded(t *testing.T) {
	mgr, mock := newTestManager(t, 1)

	mock.RunEnvironmentFn = func(ctx context.Context, cfg docker.RunEnvironmentConfig) (string, error) {
		return "container-1", nil
	}

	// Create first environment (should succeed)
	_, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "env1"})
	require.NoError(t, err)

	// Create second environment (should fail — quota is 1)
	_, err = mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "env2"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "quota exceeded")
}

func TestCreateEnvironment_DuplicateName(t *testing.T) {
	mgr, mock := newTestManager(t, 5)

	mock.RunEnvironmentFn = func(ctx context.Context, cfg docker.RunEnvironmentConfig) (string, error) {
		return "container-1", nil
	}

	_, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "my-env"})
	require.NoError(t, err)

	_, err = mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "my-env"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

func TestCreateEnvironment_InvalidName(t *testing.T) {
	mgr, _ := newTestManager(t, 5)

	tests := []struct {
		name string
		desc string
	}{
		{"", "empty name"},
		{"1starts-with-number", "starts with number"},
		{"-starts-with-dash", "starts with dash"},
		{"has spaces", "has spaces"},
		{"has.dots", "has dots"},
		{string(make([]byte, 64)), "too long"},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			_, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: tc.name})
			require.Error(t, err)
		})
	}
}

func TestCreateEnvironment_InvalidTemplate(t *testing.T) {
	mgr, _ := newTestManager(t, 5)

	_, err := mgr.Create(context.Background(), testTenantID, CreateRequest{
		Name:       "my-env",
		TemplateID: "nonexistent",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestCreateEnvironment_InvalidLeaseDuration(t *testing.T) {
	mgr, _ := newTestManager(t, 5)

	// Too short
	tooShort := 100
	_, err := mgr.Create(context.Background(), testTenantID, CreateRequest{
		Name:                 "my-env",
		LeaseDurationSeconds: &tooShort,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "lease_duration_seconds")

	// Too long
	tooLong := 100000
	_, err = mgr.Create(context.Background(), testTenantID, CreateRequest{
		Name:                 "my-env2",
		LeaseDurationSeconds: &tooLong,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "lease_duration_seconds")
}

// ---------------------------------------------------------------------------
// Get
// ---------------------------------------------------------------------------

func TestGetEnvironment_Success(t *testing.T) {
	mgr, _ := newTestManager(t, 5)

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "my-env"})
	require.NoError(t, err)

	got, err := mgr.Get(context.Background(), testTenantID, env.ID)
	require.NoError(t, err)
	assert.Equal(t, env.ID, got.ID)
	assert.Equal(t, "my-env", got.Name)
	assert.Equal(t, "running", got.Status)
}

func TestGetEnvironment_NotFound(t *testing.T) {
	mgr, _ := newTestManager(t, 5)

	_, err := mgr.Get(context.Background(), testTenantID, "nonexistent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestGetEnvironment_WrongTenant(t *testing.T) {
	mgr, _ := newTestManager(t, 5)

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "my-env"})
	require.NoError(t, err)

	_, err = mgr.Get(context.Background(), "other-tenant", env.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

func TestListEnvironments_Empty(t *testing.T) {
	mgr, _ := newTestManager(t, 5)

	list, err := mgr.List(context.Background(), testTenantID, 50, 0)
	require.NoError(t, err)
	assert.NotNil(t, list)
	assert.Len(t, list, 0)
}

// ---------------------------------------------------------------------------
// Delete
// ---------------------------------------------------------------------------

func TestDeleteEnvironment_Success(t *testing.T) {
	mgr, mock := newTestManager(t, 5)

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "my-env"})
	require.NoError(t, err)

	err = mgr.Delete(context.Background(), testTenantID, env.ID)
	require.NoError(t, err)

	// Verify container and volume cleanup
	assert.Contains(t, mock.StopContainerCalls, env.ContainerID)
	assert.Contains(t, mock.RemoveContainerCalls, env.ContainerID)
	assert.NotEmpty(t, mock.RemoveVolumeCalls)

	// Verify DB record is gone
	_, err = mgr.Get(context.Background(), testTenantID, env.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestDeleteEnvironment_NotFound(t *testing.T) {
	mgr, _ := newTestManager(t, 5)

	err := mgr.Delete(context.Background(), testTenantID, "nonexistent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// ---------------------------------------------------------------------------
// Start / Stop
// ---------------------------------------------------------------------------

func TestStartEnvironment_Success(t *testing.T) {
	mgr, _ := newTestManager(t, 5)

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "my-env"})
	require.NoError(t, err)

	// Stop first
	err = mgr.Stop(context.Background(), testTenantID, env.ID)
	require.NoError(t, err)

	got, err := mgr.Get(context.Background(), testTenantID, env.ID)
	require.NoError(t, err)
	assert.Equal(t, "stopped", got.Status)

	// Start
	err = mgr.Start(context.Background(), testTenantID, env.ID)
	require.NoError(t, err)

	got, err = mgr.Get(context.Background(), testTenantID, env.ID)
	require.NoError(t, err)
	assert.Equal(t, "running", got.Status)
}

func TestStopEnvironment_Success(t *testing.T) {
	mgr, mock := newTestManager(t, 5)

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "my-env"})
	require.NoError(t, err)

	err = mgr.Stop(context.Background(), testTenantID, env.ID)
	require.NoError(t, err)

	got, err := mgr.Get(context.Background(), testTenantID, env.ID)
	require.NoError(t, err)
	assert.Equal(t, "stopped", got.Status)
	assert.Contains(t, mock.StopContainerCalls, env.ContainerID)
}

// ---------------------------------------------------------------------------
// Exec
// ---------------------------------------------------------------------------

func TestExec_Success(t *testing.T) {
	mgr, mock := newTestManager(t, 5)

	mock.ExecCreateFn = func(ctx context.Context, containerID string, cmd []string, workDir string) (string, error) {
		return "exec-123", nil
	}
	mock.ExecRunFn = func(ctx context.Context, execID string, timeout time.Duration) ([]byte, []byte, int, error) {
		return []byte("hello world\n"), []byte(""), 0, nil
	}

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "my-env"})
	require.NoError(t, err)

	resp, err := mgr.Exec(context.Background(), testTenantID, env.ID, ExecRequest{
		Command: []string{"echo", "hello world"},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, resp.ExitCode)
	assert.Equal(t, "hello world\n", resp.Stdout)
	assert.False(t, resp.TimedOut)
	assert.False(t, resp.Truncated)
	assert.Greater(t, resp.DurationMs, int64(-1))
}

func TestExec_NotRunning(t *testing.T) {
	mgr, _ := newTestManager(t, 5)

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "my-env"})
	require.NoError(t, err)

	// Stop it
	err = mgr.Stop(context.Background(), testTenantID, env.ID)
	require.NoError(t, err)

	_, err = mgr.Exec(context.Background(), testTenantID, env.ID, ExecRequest{
		Command: []string{"echo", "hi"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be running")
}

func TestExec_EmptyCommand(t *testing.T) {
	mgr, _ := newTestManager(t, 5)

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "my-env"})
	require.NoError(t, err)

	_, err = mgr.Exec(context.Background(), testTenantID, env.ID, ExecRequest{
		Command: []string{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "command is required")
}

// ---------------------------------------------------------------------------
// ExtendLease
// ---------------------------------------------------------------------------

func TestExtendLease_Success(t *testing.T) {
	mgr, _ := newTestManager(t, 5)

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "my-env"})
	require.NoError(t, err)

	err = mgr.ExtendLease(context.Background(), testTenantID, env.ID, 7200)
	require.NoError(t, err)

	got, err := mgr.Get(context.Background(), testTenantID, env.ID)
	require.NoError(t, err)
	assert.NotNil(t, got.LeaseExpiresAt)
	// The new lease should be at least now + 7200 - 5 (allowing some test timing slack)
	now := time.Now().Unix()
	assert.Greater(t, *got.LeaseExpiresAt, now+7190)
}

// ---------------------------------------------------------------------------
// Templates
// ---------------------------------------------------------------------------

func TestGetTemplate_Success(t *testing.T) {
	mgr, _ := newTestManager(t, 5)

	tmpl, err := mgr.GetTemplate(context.Background(), "default")
	require.NoError(t, err)
	assert.Equal(t, "tmpl_default", tmpl.ID)
	assert.Equal(t, "default", tmpl.Name)
	assert.Equal(t, "ubuntu:24.04", tmpl.BaseImage)
	assert.Equal(t, 512, tmpl.MemoryMB)
}

func TestListTemplates(t *testing.T) {
	mgr, _ := newTestManager(t, 5)

	templates, err := mgr.ListTemplates(context.Background())
	require.NoError(t, err)
	assert.Len(t, templates, 1)
	assert.Equal(t, "default", templates[0].Name)
}
