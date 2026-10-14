package environments

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
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
	mgr, mock := newTestManagerWithTraefik(t, 5, "example.com", dir)

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
	ingressNetwork := "ingress-" + docker.EnvironmentIngressNetworkName(testTenantID, env.ID)
	assert.Contains(t, mock.ConnectNetworkCalls,
		[2]string{ingressNetwork, "paas-traefik"},
		"preview creation must attach Traefik to the environment's dedicated ingress network")
	assert.Contains(t, mock.ConnectNetworkAliasCalls,
		[3]string{ingressNetwork, env.ContainerID, docker.EnvironmentIngressAlias(testTenantID, env.ID)},
		"preview creation must attach the environment to its dedicated ingress network")
	route, err := os.ReadFile(filepath.Join(dir, "env-preview-"+preview.ID+".yml"))
	require.NoError(t, err)
	assert.Contains(t, string(route), docker.EnvironmentIngressAlias(testTenantID, env.ID),
		"preview routes must use the ingress-only DNS alias")

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

// ---------------------------------------------------------------------------
// BT-001: Create() uses warm pool when available
// ---------------------------------------------------------------------------

// newTestManagerWithPool creates a Manager with a seeded PoolManager attached.
// Returns the shared DB so callers can seed warm_pool entries.
func newTestManagerWithPool(t *testing.T, maxEnvs int) (*Manager, *testutil.MockDockerClient, *PoolManager, *sql.DB) {
	t.Helper()
	sharedDB := testutil.NewStateDB(t)
	seedTenant(t, sharedDB, testTenantID, maxEnvs)

	mock := &testutil.MockDockerClient{}
	mgr := NewManager(sharedDB, mock, "", "")
	pool := NewPoolManager(sharedDB, mock, PoolConfig{
		Enabled:  true,
		PoolSize: 2,
		MaxTotal: 6,
	})
	mgr.SetPool(pool)
	return mgr, mock, pool, sharedDB
}

// TestCreate_UsesPoolWhenAvailable verifies BT-001:
// When Create() is called and the pool has a ready container for the template,
// RunEnvironment is NOT called — the pool container is used instead.
func TestCreate_UsesPoolWhenAvailable(t *testing.T) {
	mgr, mock, _, sharedDB := newTestManagerWithPool(t, 5)

	// Pre-seed a warm pool entry for the "default" template (DB id = "tmpl_default").
	insertWarmPoolEntry(t, sharedDB, "wp_test_001", "tmpl_default", "pool-container-abc", "ah-pool-wp_test_001")

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{
		Name:       "warm-env",
		TemplateID: "default",
	})
	require.NoError(t, err)
	assert.Equal(t, "running", env.Status)

	// RunEnvironment must NOT have been called — we used the warm container.
	assert.Equal(t, 0, mock.RunEnvironmentCalls,
		"RunEnvironment must not be called when pool has a ready container")

	// The environment's container ID should be the pool container.
	assert.Equal(t, "pool-container-abc", env.ContainerID,
		"environment should use the pool container ID")
	assert.Equal(t, [][2]string{{"pool-container-abc", "ah-env-" + testTenantID + "-" + env.ID}}, mock.RenameContainerArgs,
		"a claimed pool container must be renamed to the environment name used by preview routing")
}

func TestCreateCleansUpWarmContainerWhenNetworkTransferFails(t *testing.T) {
	mgr, mock, _, sharedDB := newTestManagerWithPool(t, 5)
	insertWarmPoolEntry(t, sharedDB, "wp_fail", "tmpl_default", "pool-fails", "ah-pool-wp_fail")
	mock.NetworkDisconnectFn = func(context.Context, string, string) error { return fmt.Errorf("disconnect failed") }
	mock.RunEnvironmentFn = func(context.Context, docker.RunEnvironmentConfig) (string, error) {
		return "cold-fallback", nil
	}
	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "fallback-env", TemplateID: "default"})
	require.NoError(t, err)
	assert.Equal(t, "cold-fallback", env.ContainerID)
	assert.Contains(t, mock.RemoveContainerCalls, "pool-fails", "failed transfer must not orphan the claimed pool container")
	assert.Contains(t, mock.RemoveVolumeCalls, "ah-pool-wp_fail", "failed transfer must clean the unclaimed pool volume")
}

// TestCreate_FallsBackToColdStartWhenPoolEmpty verifies that when the pool
// has no ready container, Create() falls back to the normal cold-start path
// and calls RunEnvironment.
func TestCreate_FallsBackToColdStartWhenPoolEmpty(t *testing.T) {
	mgr, mock, _, _ := newTestManagerWithPool(t, 5)

	mock.RunEnvironmentFn = func(ctx context.Context, cfg docker.RunEnvironmentConfig) (string, error) {
		return "cold-container-xyz", nil
	}

	// Pool is empty — no pre-seeded rows.
	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{
		Name:       "cold-env",
		TemplateID: "default",
	})
	require.NoError(t, err)
	assert.Equal(t, "running", env.Status)

	// RunEnvironment MUST be called since pool was empty.
	assert.Equal(t, 1, mock.RunEnvironmentCalls,
		"RunEnvironment must be called when pool is empty (cold start)")
	assert.Equal(t, "cold-container-xyz", env.ContainerID)
}

func TestStartRecreatesMissingContainerWithExistingWorkspaceVolume(t *testing.T) {
	mgr, mock, _, _ := newTestManagerWithPool(t, 5)
	var runs int
	mock.RunEnvironmentFn = func(context.Context, docker.RunEnvironmentConfig) (string, error) {
		runs++
		if runs == 1 {
			return "original-container", nil
		}
		return "replacement-container", nil
	}
	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "recover-env", TemplateID: "default"})
	require.NoError(t, err)
	require.NoError(t, mgr.Stop(context.Background(), testTenantID, env.ID))
	mock.StartContainerFn = func(context.Context, string) error { return fmt.Errorf("No such container: original-container") }
	mock.ListVolumesFn = func(_ context.Context, prefix string) ([]string, error) { return []string{prefix}, nil }
	require.NoError(t, mgr.Start(context.Background(), testTenantID, env.ID))
	recovered, err := mgr.Get(context.Background(), testTenantID, env.ID)
	require.NoError(t, err)
	assert.Equal(t, "replacement-container", recovered.ContainerID)
	assert.Equal(t, env.VolumeName, recovered.VolumeName, "recovery must preserve existing workspace contents")
}

func TestStartDoesNotRecreateMissingWorkspaceVolume(t *testing.T) {
	mgr, mock, _, _ := newTestManagerWithPool(t, 5)
	mock.RunEnvironmentFn = func(context.Context, docker.RunEnvironmentConfig) (string, error) { return "original-container", nil }
	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "missing-volume", TemplateID: "default"})
	require.NoError(t, err)
	require.NoError(t, mgr.Stop(context.Background(), testTenantID, env.ID))
	runsBeforeStart := mock.RunEnvironmentCalls
	mock.StartContainerFn = func(context.Context, string) error { return fmt.Errorf("No such container: original-container") }
	mock.ListVolumesFn = func(context.Context, string) ([]string, error) { return nil, nil }
	err = mgr.Start(context.Background(), testTenantID, env.ID)
	require.ErrorContains(t, err, "workspace volume")
	assert.Equal(t, runsBeforeStart, mock.RunEnvironmentCalls,
		"Start must not create a fresh empty volume when the workspace volume is missing")
}

func TestCreateCannotResurrectEnvironmentAfterTenantSuspension(t *testing.T) {
	mgr, mock, _, stateDB := newTestManagerWithPool(t, 5)
	entered := make(chan struct{})
	release := make(chan struct{})
	mock.RunEnvironmentFn = func(context.Context, docker.RunEnvironmentConfig) (string, error) {
		close(entered)
		<-release
		return "late-container", nil
	}
	type createResult struct {
		env *Environment
		err error
	}
	resultCh := make(chan createResult, 1)
	go func() {
		env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "suspend-race", TemplateID: "default"})
		resultCh <- createResult{env: env, err: err}
	}()
	<-entered
	mgr.StopAllForTenant(context.Background(), testTenantID)
	close(release)
	result := <-resultCh
	require.Error(t, result.err, "creation must fail after suspension wins the status race")
	assert.Contains(t, mock.RemoveContainerCalls, "late-container", "the late container must be cleaned up")
	var count int
	require.NoError(t, stateDB.QueryRow(`SELECT COUNT(*) FROM environments WHERE tenant_id = ? AND name = 'suspend-race'`, testTenantID).Scan(&count))
	assert.Equal(t, 0, count, "a failed in-flight creation must not leave a stopped row with deleted storage")
}

func TestStartCannotResurrectEnvironmentAfterTenantSuspension(t *testing.T) {
	mgr, mock, _, stateDB := newTestManagerWithPool(t, 5)
	mock.RunEnvironmentFn = func(context.Context, docker.RunEnvironmentConfig) (string, error) { return "existing-container", nil }
	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "start-suspend-race", TemplateID: "default"})
	require.NoError(t, err)
	require.NoError(t, mgr.Stop(context.Background(), testTenantID, env.ID))
	entered := make(chan struct{})
	release := make(chan struct{})
	mock.StartContainerFn = func(context.Context, string) error {
		close(entered)
		<-release
		return nil
	}
	startResult := make(chan error, 1)
	go func() { startResult <- mgr.Start(context.Background(), testTenantID, env.ID) }()
	<-entered
	_, err = stateDB.Exec(`UPDATE tenants SET status = 'suspended' WHERE id = ?`, testTenantID)
	require.NoError(t, err)
	mgr.StopAllForTenant(context.Background(), testTenantID)
	close(release)
	require.Error(t, <-startResult, "start must fail after tenant suspension wins the status race")
	assert.Contains(t, mock.StopContainerCalls, "existing-container", "the container started after suspension must be stopped")
	recovered, err := mgr.Get(context.Background(), testTenantID, env.ID)
	require.NoError(t, err)
	assert.Equal(t, "stopped", recovered.Status)
}

func TestStopAllForTenantKeepsNetworksReferencedByStoppedEnvironment(t *testing.T) {
	mgr, mock, _, stateDB := newTestManagerWithPool(t, 5)
	now := time.Now().Unix()
	_, err := stateDB.Exec(`INSERT INTO environments
		(id, tenant_id, name, template_id, status, container_id, volume_name, created_at, updated_at)
		VALUES ('env-suspend-net', ?, 'suspend-net', 'tmpl_default', 'running', 'env-container', 'ah-env-suspend-net', ?, ?)`, testTenantID, now, now)
	require.NoError(t, err)
	_, err = stateDB.Exec(`INSERT INTO environment_previews
		(id, environment_id, tenant_id, name, port, dns_label, created_at, updated_at)
		VALUES ('preview-suspend-net', 'env-suspend-net', ?, 'web', 3000, 'env-suspend-net-web', ?, ?)`, testTenantID, now, now)
	require.NoError(t, err)
	tenantNetwork := docker.TenantNetworkName(testTenantID)
	previewNetwork := docker.EnvironmentIngressNetworkName(testTenantID, "env-suspend-net")
	mock.NetworkListFn = func(context.Context) ([]docker.NetworkInfo, error) {
		return []docker.NetworkInfo{
			{ID: "tenant-net", Name: tenantNetwork},
			{ID: "preview-net", Name: previewNetwork},
		}, nil
	}

	mgr.StopAllForTenant(context.Background(), testTenantID)

	assert.Contains(t, mock.StopContainerCalls, "env-container")
	assert.Empty(t, mock.RemoveNetworkCalls, "suspension must retain Docker networks referenced by stopped environment containers")
	var status string
	require.NoError(t, stateDB.QueryRow(`SELECT status FROM environments WHERE id = 'env-suspend-net'`).Scan(&status))
	assert.Equal(t, "stopped", status)
}

// insertWarmPoolEntry inserts a ready warm pool entry into the given DB.
func insertWarmPoolEntry(t *testing.T, db *sql.DB, id, templateID, containerID, volumeName string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO warm_pool (id, template_id, container_id, volume_name, status, created_at)
		 VALUES (?, ?, ?, ?, 'ready', ?)`,
		id, templateID, containerID, volumeName, time.Now().Unix(),
	)
	require.NoError(t, err)
}
