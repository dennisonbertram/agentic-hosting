package environments

import (
	"context"
	"errors"
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

// Git URL and ref are user input; they must reach the shell only as
// positional parameters so metacharacters cannot alter the script.
func TestSyncWorkspace_PassesURLAndRefAsArguments(t *testing.T) {
	mgr, mock := newTestManager(t, 5)
	var gotCmd []string
	mock.ExecCreateFn = func(_ context.Context, _ string, cmd []string, _ string) (string, error) {
		gotCmd = cmd
		return "e1", nil
	}
	mock.ExecRunFn = func(context.Context, string, time.Duration) ([]byte, []byte, int, error) {
		return nil, nil, 0, nil
	}
	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "sw"})
	require.NoError(t, err)

	hostileRef := "main; touch /tmp/pwned"
	hostileURL := "https://example.com/r.git'$(id)'"
	require.NoError(t, mgr.SyncWorkspace(context.Background(), testTenantID, env.ID,
		SyncRequest{GitURL: hostileURL, GitRef: hostileRef}))

	require.Len(t, gotCmd, 10)
	script := gotCmd[6]
	assert.NotContains(t, script, hostileRef)
	assert.NotContains(t, script, hostileURL)
	assert.Contains(t, script, `"$1"`)
	assert.Contains(t, script, `"$2"`)
	assert.Contains(t, script, "FETCH_HEAD", "a requested ref must be checked out after a fresh clone")
	assert.Equal(t, []string{"sh", hostileURL, hostileRef}, gotCmd[7:])
}

func TestSyncWorkspace_NonZeroExitIsError(t *testing.T) {
	mgr, mock := newTestManager(t, 5)
	mock.ExecRunFn = func(context.Context, string, time.Duration) ([]byte, []byte, int, error) {
		return nil, nil, 128, nil
	}
	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "sw"})
	require.NoError(t, err)
	err = mgr.SyncWorkspace(context.Background(), testTenantID, env.ID, SyncRequest{GitURL: "https://example.com/r.git"})
	assert.ErrorContains(t, err, "exit code 128")
}

// Stop must succeed when the container is already gone, so the environment is
// not wedged in 'running'; Start later recreates it from the volume.
func TestStop_MissingContainerStillStops(t *testing.T) {
	mgr, mock := newTestManager(t, 5)
	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "gone"})
	require.NoError(t, err)
	mock.StopContainerFn = func(_ context.Context, id string) error { return testutil.NewNotFoundError(id) }

	require.NoError(t, mgr.Stop(context.Background(), testTenantID, env.ID))
	got, err := mgr.Get(context.Background(), testTenantID, env.ID)
	require.NoError(t, err)
	assert.Equal(t, "stopped", got.Status)

	err = mgr.Stop(context.Background(), testTenantID, env.ID)
	assert.ErrorContains(t, err, "must be running")
}

func TestStop_DockerFailureKeepsRunning(t *testing.T) {
	mgr, mock := newTestManager(t, 5)
	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "busy"})
	require.NoError(t, err)
	mock.StopContainerFn = func(context.Context, string) error { return errors.New("daemon timeout") }

	assert.ErrorContains(t, mgr.Stop(context.Background(), testTenantID, env.ID), "daemon timeout")
	got, err := mgr.Get(context.Background(), testTenantID, env.ID)
	require.NoError(t, err)
	assert.Equal(t, "running", got.Status)
}

func TestExtendLease_Validation(t *testing.T) {
	mgr, _ := newTestManager(t, 5)
	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "lease"})
	require.NoError(t, err)

	assert.ErrorContains(t, mgr.ExtendLease(context.Background(), testTenantID, env.ID, 10), "between 300 and 86400")
	assert.ErrorContains(t, mgr.ExtendLease(context.Background(), "other-tenant", env.ID, 600), "not found")
	require.NoError(t, mgr.Stop(context.Background(), testTenantID, env.ID))
	assert.ErrorContains(t, mgr.ExtendLease(context.Background(), testTenantID, env.ID, 600), "must be running")
}

func TestList_PaginationAndTenantScope(t *testing.T) {
	mgr, _ := newTestManager(t, 5)
	seedTenant(t, mgr.db, "t_other", 5)
	for i := 0; i < 3; i++ {
		_, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: fmt.Sprintf("e%d", i)})
		require.NoError(t, err)
	}
	_, err := mgr.Create(context.Background(), "t_other", CreateRequest{Name: "theirs"})
	require.NoError(t, err)

	all, err := mgr.List(context.Background(), testTenantID, 0, -1)
	require.NoError(t, err)
	assert.Len(t, all, 3)
	for _, e := range all {
		assert.Equal(t, testTenantID, e.TenantID)
	}
	page, err := mgr.List(context.Background(), testTenantID, 2, 2)
	require.NoError(t, err)
	assert.Len(t, page, 1)
}

func TestGetTemplate_ByIDAndName(t *testing.T) {
	mgr, _ := newTestManager(t, 5)
	byName, err := mgr.GetTemplate(context.Background(), "default")
	require.NoError(t, err)
	byID, err := mgr.GetTemplate(context.Background(), byName.ID)
	require.NoError(t, err)
	assert.Equal(t, byName, byID)
	_, err = mgr.GetTemplate(context.Background(), "nope")
	assert.ErrorContains(t, err, `template "nope" not found`)
}

// When the preview ingress cannot be restored, Start must undo: a container it
// recreated is removed rather than left running outside DB ownership.
func TestStart_IngressFailureRemovesRecreatedContainer(t *testing.T) {
	dir := t.TempDir()
	mgr, mock := newTestManagerWithTraefik(t, 5, "example.com", dir)
	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "pv"})
	require.NoError(t, err)
	_, err = mgr.CreatePreview(context.Background(), testTenantID, env.ID, CreatePreviewRequest{Name: "web", Port: 3000})
	require.NoError(t, err)
	require.NoError(t, mgr.Stop(context.Background(), testTenantID, env.ID))

	mock.StartContainerFn = func(_ context.Context, id string) error { return testutil.NewNotFoundError(id) }
	mock.ListVolumesFn = func(_ context.Context, prefix string) ([]string, error) { return []string{prefix}, nil }
	mock.RunEnvironmentFn = func(context.Context, docker.RunEnvironmentConfig) (string, error) { return "recreated", nil }
	mock.EnsureIngressNetworkFn = func(context.Context, string) (string, error) { return "", errors.New("no ingress") }

	err = mgr.Start(context.Background(), testTenantID, env.ID)
	require.ErrorContains(t, err, "restore preview ingress network")
	assert.Contains(t, mock.StopContainerCalls, "recreated")
	assert.Contains(t, mock.RemoveContainerCalls, "recreated")
	got, err := mgr.Get(context.Background(), testTenantID, env.ID)
	require.NoError(t, err)
	assert.Equal(t, "stopped", got.Status)
	assert.Equal(t, env.ContainerID, got.ContainerID, "ownership must not move to the removed container")
}

// Deleting the last preview tears down the dedicated ingress network.
func TestDeletePreview_LastPreviewRemovesIngressNetwork(t *testing.T) {
	dir := t.TempDir()
	mgr, mock := newTestManagerWithTraefik(t, 5, "", dir)
	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "pv"})
	require.NoError(t, err)
	p, err := mgr.CreatePreview(context.Background(), testTenantID, env.ID, CreatePreviewRequest{Name: "web", Port: 3000})
	require.NoError(t, err)
	route, err := os.ReadFile(filepath.Join(dir, "env-preview-"+p.ID+".yml"))
	require.NoError(t, err)
	assert.Contains(t, string(route), ".localhost", "no base domain means localhost routing")

	netName := docker.EnvironmentIngressNetworkName(testTenantID, env.ID)
	mock.NetworkListFn = func(context.Context) ([]docker.NetworkInfo, error) {
		return []docker.NetworkInfo{{ID: "net-1", Name: netName}}, nil
	}
	require.NoError(t, mgr.DeletePreview(context.Background(), testTenantID, env.ID, p.ID))
	assert.Contains(t, mock.NetworkDisconnectCalls, [2]string{"net-1", env.ContainerID})
	assert.Contains(t, mock.NetworkDisconnectCalls, [2]string{"net-1", "paas-traefik"})
	assert.Equal(t, []string{"net-1"}, mock.RemoveNetworkCalls)
	assert.NoFileExists(t, filepath.Join(dir, "env-preview-"+p.ID+".yml"))

	err = mgr.DeletePreview(context.Background(), testTenantID, env.ID, p.ID)
	assert.ErrorContains(t, err, "preview not found")
}

func TestIsNotFoundError(t *testing.T) {
	assert.False(t, isNotFoundError(nil))
	assert.True(t, isNotFoundError(testutil.NewNotFoundError("c")))
	assert.True(t, isNotFoundError(errors.New("Error: No such volume: v")))
	assert.False(t, isNotFoundError(errors.New("permission denied")))
}

func TestPoolRun_DisabledReturnsImmediately(t *testing.T) {
	p := NewPoolManager(testutil.NewStateDB(t), &testutil.MockDockerClient{}, PoolConfig{})
	done := make(chan struct{})
	go func() { p.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("disabled pool Run must return")
	}
	assert.Equal(t, 2, p.config.PoolSize)
	assert.Equal(t, 6, p.config.MaxTotal)
	assert.Equal(t, 60*time.Second, p.config.RefillPeriod)
}

func TestPoolRun_StopsOnCancel(t *testing.T) {
	p := NewPoolManager(testutil.NewStateDB(t), &testutil.MockDockerClient{}, PoolConfig{Enabled: true})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run must return once its context is cancelled")
	}
}
