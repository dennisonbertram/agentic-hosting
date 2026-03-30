package environments

import (
	"context"
	"database/sql"
	"os"
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
	mgr := NewManager(db, mock, "", "")
	return mgr, mock
}

func newTestManagerWithTraefik(t *testing.T, maxEnvs int, baseDomain, traefikDir string) (*Manager, *testutil.MockDockerClient) {
	t.Helper()
	db := testutil.NewStateDB(t)
	seedTenant(t, db, testTenantID, maxEnvs)

	mock := &testutil.MockDockerClient{}
	mgr := NewManager(db, mock, baseDomain, traefikDir)
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
	assert.GreaterOrEqual(t, len(templates), 1, "should have at least 1 template")
	// Find the default template
	var foundDefault bool
	for _, tmpl := range templates {
		if tmpl.Name == "default" {
			foundDefault = true
		}
	}
	assert.True(t, foundDefault, "should include default template")
}

// ---------------------------------------------------------------------------
// SyncWorkspace
// ---------------------------------------------------------------------------

func TestSyncWorkspace_Success(t *testing.T) {
	mgr, mock := newTestManager(t, 5)

	mock.ExecCreateFn = func(ctx context.Context, containerID string, cmd []string, workDir string) (string, error) {
		// Verify git clone command is constructed correctly
		assert.Equal(t, "/workspace", workDir)
		assert.Equal(t, "sh", cmd[0])
		assert.Equal(t, "-c", cmd[1])
		assert.Contains(t, cmd[2], "https://github.com/test/repo.git")
		return "exec-sync", nil
	}
	mock.ExecRunFn = func(ctx context.Context, execID string, timeout time.Duration) ([]byte, []byte, int, error) {
		assert.Equal(t, "exec-sync", execID)
		assert.Equal(t, 5*time.Minute, timeout)
		return nil, nil, 0, nil
	}

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "sync-env"})
	require.NoError(t, err)

	err = mgr.SyncWorkspace(context.Background(), testTenantID, env.ID, SyncRequest{
		GitURL: "https://github.com/test/repo.git",
	})
	require.NoError(t, err)
	assert.Len(t, mock.ExecCreateCalls, 1)
	assert.Len(t, mock.ExecRunCalls, 1)
}

func TestSyncWorkspace_NotRunning(t *testing.T) {
	mgr, _ := newTestManager(t, 5)

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "sync-env"})
	require.NoError(t, err)

	// Stop the environment
	err = mgr.Stop(context.Background(), testTenantID, env.ID)
	require.NoError(t, err)

	err = mgr.SyncWorkspace(context.Background(), testTenantID, env.ID, SyncRequest{
		GitURL: "https://github.com/test/repo.git",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be running")
}

func TestSyncWorkspace_InvalidURL(t *testing.T) {
	mgr, _ := newTestManager(t, 5)

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "sync-env"})
	require.NoError(t, err)

	// http:// should be rejected
	err = mgr.SyncWorkspace(context.Background(), testTenantID, env.ID, SyncRequest{
		GitURL: "http://github.com/test/repo.git",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "https://")

	// git:// should be rejected
	err = mgr.SyncWorkspace(context.Background(), testTenantID, env.ID, SyncRequest{
		GitURL: "git://github.com/test/repo.git",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "https://")

	// Empty should be rejected
	err = mgr.SyncWorkspace(context.Background(), testTenantID, env.ID, SyncRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "git_url is required")
}

// ---------------------------------------------------------------------------
// Previews
// ---------------------------------------------------------------------------

func TestCreatePreview_Success(t *testing.T) {
	dir := t.TempDir()
	mgr, _ := newTestManagerWithTraefik(t, 5, "example.com", dir)

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "preview-env"})
	require.NoError(t, err)

	preview, err := mgr.CreatePreview(context.Background(), testTenantID, env.ID, CreatePreviewRequest{
		Name: "api",
		Port: 8080,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, preview.ID)
	assert.Equal(t, "api", preview.Name)
	assert.Equal(t, 8080, preview.Port)
	assert.Contains(t, preview.DNSLabel, "env-")
	assert.Contains(t, preview.DNSLabel, "-api")
	assert.Contains(t, preview.URL, "example.com")

	// Verify Traefik route file was written
	files, _ := os.ReadDir(dir)
	assert.Len(t, files, 1)
	assert.Contains(t, files[0].Name(), "env-preview-")
}

func TestCreatePreview_DuplicateName(t *testing.T) {
	dir := t.TempDir()
	mgr, _ := newTestManagerWithTraefik(t, 5, "example.com", dir)

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "preview-env"})
	require.NoError(t, err)

	_, err = mgr.CreatePreview(context.Background(), testTenantID, env.ID, CreatePreviewRequest{
		Name: "api",
		Port: 8080,
	})
	require.NoError(t, err)

	_, err = mgr.CreatePreview(context.Background(), testTenantID, env.ID, CreatePreviewRequest{
		Name: "api",
		Port: 3000,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

func TestCreatePreview_InvalidPort(t *testing.T) {
	mgr, _ := newTestManager(t, 5)

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "preview-env"})
	require.NoError(t, err)

	_, err = mgr.CreatePreview(context.Background(), testTenantID, env.ID, CreatePreviewRequest{
		Name: "api",
		Port: 0,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "port must be between")

	_, err = mgr.CreatePreview(context.Background(), testTenantID, env.ID, CreatePreviewRequest{
		Name: "api",
		Port: 70000,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "port must be between")
}

func TestListPreviews_Success(t *testing.T) {
	dir := t.TempDir()
	mgr, _ := newTestManagerWithTraefik(t, 5, "example.com", dir)

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "preview-env"})
	require.NoError(t, err)

	_, err = mgr.CreatePreview(context.Background(), testTenantID, env.ID, CreatePreviewRequest{
		Name: "api",
		Port: 8080,
	})
	require.NoError(t, err)

	_, err = mgr.CreatePreview(context.Background(), testTenantID, env.ID, CreatePreviewRequest{
		Name: "web",
		Port: 3000,
	})
	require.NoError(t, err)

	previews, err := mgr.ListPreviews(context.Background(), testTenantID, env.ID)
	require.NoError(t, err)
	assert.Len(t, previews, 2)
}

func TestDeletePreview_Success(t *testing.T) {
	dir := t.TempDir()
	mgr, _ := newTestManagerWithTraefik(t, 5, "example.com", dir)

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "preview-env"})
	require.NoError(t, err)

	preview, err := mgr.CreatePreview(context.Background(), testTenantID, env.ID, CreatePreviewRequest{
		Name: "api",
		Port: 8080,
	})
	require.NoError(t, err)

	// Verify file exists
	files, _ := os.ReadDir(dir)
	assert.Len(t, files, 1)

	err = mgr.DeletePreview(context.Background(), testTenantID, env.ID, preview.ID)
	require.NoError(t, err)

	// Verify file is removed
	files, _ = os.ReadDir(dir)
	assert.Len(t, files, 0)

	// Verify DB row is gone
	previews, err := mgr.ListPreviews(context.Background(), testTenantID, env.ID)
	require.NoError(t, err)
	assert.Len(t, previews, 0)
}

// ---------------------------------------------------------------------------
// Additional behavioral tests (regression coverage)
// ---------------------------------------------------------------------------

// TestSyncWorkspace_ExecCalledOnRunning verifies that SyncWorkspace calls ExecCreate
// and ExecRun exactly once when the environment is running.
func TestSyncWorkspace_ExecCalledOnRunning(t *testing.T) {
	mgr, mock := newTestManager(t, 5)

	var execCreateCount, execRunCount int
	mock.ExecCreateFn = func(ctx context.Context, containerID string, cmd []string, workDir string) (string, error) {
		execCreateCount++
		return "exec-sync-id", nil
	}
	mock.ExecRunFn = func(ctx context.Context, execID string, timeout time.Duration) ([]byte, []byte, int, error) {
		execRunCount++
		return nil, nil, 0, nil
	}

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "running-sync-env"})
	require.NoError(t, err)
	require.Equal(t, "running", env.Status)

	err = mgr.SyncWorkspace(context.Background(), testTenantID, env.ID, SyncRequest{
		GitURL: "https://github.com/example/repo.git",
	})
	require.NoError(t, err)

	// Both ExecCreate and ExecRun must be called exactly once.
	assert.Equal(t, 1, execCreateCount, "ExecCreate must be called once for running environment")
	assert.Equal(t, 1, execRunCount, "ExecRun must be called once for running environment")
}

// TestSyncWorkspace_StoppedEnvReturnsError verifies that SyncWorkspace returns an
// error (not nil) when the environment is not in running state.
func TestSyncWorkspace_StoppedEnvReturnsError(t *testing.T) {
	mgr, mock := newTestManager(t, 5)

	var execCreateCalled bool
	mock.ExecCreateFn = func(ctx context.Context, containerID string, cmd []string, workDir string) (string, error) {
		execCreateCalled = true
		return "should-not-be-called", nil
	}

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "stopped-sync-env"})
	require.NoError(t, err)

	// Explicitly stop the environment so it is not running.
	err = mgr.Stop(context.Background(), testTenantID, env.ID)
	require.NoError(t, err)

	err = mgr.SyncWorkspace(context.Background(), testTenantID, env.ID, SyncRequest{
		GitURL: "https://github.com/example/repo.git",
	})
	require.Error(t, err, "SyncWorkspace must return an error for a stopped environment")
	assert.Contains(t, err.Error(), "must be running",
		"error message should mention running requirement")
	assert.False(t, execCreateCalled,
		"ExecCreate must NOT be called when environment is not running")
}

// TestCreatePreview_TraefikFileWritten verifies that CreatePreview writes a
// Traefik dynamic configuration file to the configured directory.
func TestCreatePreview_TraefikFileWritten(t *testing.T) {
	dir := t.TempDir()
	mgr, _ := newTestManagerWithTraefik(t, 5, "preview.example.com", dir)

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "traefik-env"})
	require.NoError(t, err)

	preview, err := mgr.CreatePreview(context.Background(), testTenantID, env.ID, CreatePreviewRequest{
		Name: "backend",
		Port: 9090,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, preview.ID)
	assert.Equal(t, "backend", preview.Name)
	assert.Equal(t, 9090, preview.Port)
	assert.Contains(t, preview.URL, "preview.example.com",
		"preview URL should include the configured base domain")

	// Exactly one Traefik config file must be written to the configured directory.
	files, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, files, 1, "exactly one Traefik config file should be written per preview")
	assert.Contains(t, files[0].Name(), ".yml",
		"Traefik config file should have a .yml extension")
}

// TestListPreviews_EmptySlice verifies that ListPreviews returns an empty,
// non-nil slice when the environment has no previews.
func TestListPreviews_EmptySlice(t *testing.T) {
	mgr, _ := newTestManager(t, 5)

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "no-preview-env"})
	require.NoError(t, err)

	previews, err := mgr.ListPreviews(context.Background(), testTenantID, env.ID)
	require.NoError(t, err)

	// Result must be a non-nil empty slice — callers that range over it must not panic.
	assert.NotNil(t, previews, "ListPreviews must return a non-nil slice even when empty")
	assert.Len(t, previews, 0, "ListPreviews must return zero previews for a fresh environment")
}

// TestDeletePreview_RemovesRowAndFile verifies that DeletePreview removes the DB
// row AND the corresponding Traefik config file in a single operation.
func TestDeletePreview_RemovesRowAndFile(t *testing.T) {
	dir := t.TempDir()
	mgr, _ := newTestManagerWithTraefik(t, 5, "del.example.com", dir)

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "del-preview-env"})
	require.NoError(t, err)

	preview, err := mgr.CreatePreview(context.Background(), testTenantID, env.ID, CreatePreviewRequest{
		Name: "myroute",
		Port: 4000,
	})
	require.NoError(t, err)

	// Precondition: one config file must exist before delete.
	files, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, files, 1, "precondition: Traefik file must exist before DeletePreview")

	err = mgr.DeletePreview(context.Background(), testTenantID, env.ID, preview.ID)
	require.NoError(t, err)

	// The Traefik config file must be gone.
	files, err = os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, files, 0, "Traefik config file must be removed by DeletePreview")

	// The DB row must be gone — ListPreviews should return empty.
	previews, err := mgr.ListPreviews(context.Background(), testTenantID, env.ID)
	require.NoError(t, err)
	assert.Len(t, previews, 0, "DB row must be removed by DeletePreview")
}
