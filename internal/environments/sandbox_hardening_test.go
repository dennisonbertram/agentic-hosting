package environments

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testEgressNet = "ah-sbx-egress"

func allowEgress(t *testing.T, mgr *Manager, tenantID string) {
	t.Helper()
	_, err := mgr.db.Exec(`UPDATE tenant_quotas SET sandbox_egress_allowed = 1 WHERE tenant_id = ?`, tenantID)
	require.NoError(t, err)
}

func TestSandboxEgress_FlagUnsetIs403EvenForAllowedTenant(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	allowEgress(t, mgr, testTenantID)
	_, err := mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{
		Source: SandboxSource{Type: "template"}, Egress: "internet",
	})
	assertAPICode(t, err, 403)
	assert.Contains(t, err.Error(), "not available on this server")
	assert.Zero(t, mock.RunEnvironmentCalls)
}

func TestSandboxEgress_TenantNotAllowedIs403(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	mgr.SetSandboxEgressNetwork(testEgressNet)
	_, err := mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{
		Source: SandboxSource{Type: "template"}, Egress: "internet",
	})
	assertAPICode(t, err, 403)
	assert.Contains(t, err.Error(), "not enabled for this tenant")
	assert.Zero(t, mock.RunEnvironmentCalls)
	var n int
	require.NoError(t, mgr.db.QueryRow(`SELECT COUNT(*) FROM environments`).Scan(&n))
	assert.Zero(t, n, "a refused create leaves no row")
}

func TestSandboxEgress_InternetAttachesOnlyToEgressNetwork(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	mgr.SetSandboxEgressNetwork(testEgressNet)
	allowEgress(t, mgr, testTenantID)

	s, err := mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{
		Source: SandboxSource{Type: "image", Image: "alpine"}, Egress: "internet",
	})
	require.NoError(t, err)
	assert.Equal(t, "internet", s.Egress)

	cfg := lastRunCfg(t, mock)
	assert.Equal(t, testEgressNet, cfg.NetworkName)
	assert.True(t, cfg.EgressNetwork)
	assert.NotContains(t, mock.EnsureNetworkCalls, docker.TenantNetworkName(testTenantID),
		"an egress sandbox never touches the tenant network")
	assert.Empty(t, mock.ConnectNetworkCalls, "and is not connected to any other network")

	got, err := mgr.GetSandbox(context.Background(), testTenantID, s.ID)
	require.NoError(t, err)
	assert.Equal(t, "internet", got.Egress, "egress is persisted and reported")

	// The same server still defaults to no egress.
	s2, err := mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "image", Image: "alpine"}})
	require.NoError(t, err)
	assert.Equal(t, "none", s2.Egress)
	cfg = lastRunCfg(t, mock)
	assert.Equal(t, docker.TenantNetworkName(testTenantID), cfg.NetworkName)
	assert.False(t, cfg.EgressNetwork)
}

func TestSandboxEgress_SkipsWarmPool(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	mgr.SetSandboxEgressNetwork(testEgressNet)
	allowEgress(t, mgr, testTenantID)
	mgr.SetPool(NewPoolManager(mgr.db, mock, PoolConfig{Enabled: true, PoolSize: 2, MaxTotal: 6}))
	_, err := mgr.db.Exec(`INSERT INTO warm_pool (id, template_id, container_id, volume_name, status, created_at)
		VALUES ('wp1', 'tmpl_default', 'warm-1', 'ah-pool-wp1', 'ready', ?)`, time.Now().Unix())
	require.NoError(t, err)

	_, err = mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{
		Source: SandboxSource{Type: "template"}, Egress: "internet",
	})
	require.NoError(t, err)
	var ready int
	require.NoError(t, mgr.db.QueryRow(`SELECT COUNT(*) FROM warm_pool WHERE status = 'ready'`).Scan(&ready))
	assert.Equal(t, 1, ready, "pool containers sit on the tenant network; egress sandboxes cold start")
	assert.Equal(t, 1, mock.RunEnvironmentCalls)
}

func TestSandboxRuntime_UsedOnlyWhenSet(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	_, err := mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "image", Image: "alpine"}})
	require.NoError(t, err)
	assert.Empty(t, lastRunCfg(t, mock).Runtime, "default: the client's runtime")

	mgr.SetSandboxRuntime("runsc-sbx")
	_, err = mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "image", Image: "alpine"}})
	require.NoError(t, err)
	assert.Equal(t, "runsc-sbx", lastRunCfg(t, mock).Runtime)

	// Environments never use the sandbox runtime.
	_, err = mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "dev"})
	require.NoError(t, err)
	assert.Empty(t, lastRunCfg(t, mock).Runtime)
}

func TestSandboxDisk_RejectedWhenTmpfsOff(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	_, err := mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{
		Source: SandboxSource{Type: "template"}, DiskMB: intp(64),
	})
	assertAPICode(t, err, 400)
	assert.Contains(t, err.Error(), "disk_mb is not supported")
	assert.Zero(t, mock.RunEnvironmentCalls)

	// Default: a named volume, no disk_mb.
	s, err := mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "template"}})
	require.NoError(t, err)
	assert.Zero(t, s.DiskMB)
	cfg := lastRunCfg(t, mock)
	assert.Zero(t, cfg.WorkspaceTmpfsMB)
	assert.Equal(t, "ah-env-"+s.ID, cfg.VolumeName)
	assert.Equal(t, []string{"ah-env-" + s.ID}, mock.CreateVolumeCalls)
}

func TestSandboxDisk_TmpfsWorkspaceHasNoVolume(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	mgr.SetSandboxWorkspaceTmpfs(true)
	ctx := context.Background()

	s, err := mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{
		Source: SandboxSource{Type: "image", Image: "alpine"}, MemoryMB: intp(512), DiskMB: intp(200),
	})
	require.NoError(t, err)
	assert.Equal(t, 200, s.DiskMB)
	cfg := lastRunCfg(t, mock)
	assert.Equal(t, 200, cfg.WorkspaceTmpfsMB)
	assert.Empty(t, cfg.VolumeName)
	assert.Empty(t, mock.CreateVolumeCalls, "no volume is created for a tmpfs workspace")

	got, err := mgr.GetSandbox(ctx, testTenantID, s.ID)
	require.NoError(t, err)
	assert.Equal(t, 200, got.DiskMB)
	assert.Empty(t, got.volumeName)

	b, err := json.Marshal(got)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"disk_mb":200`)

	require.NoError(t, mgr.DeleteSandbox(ctx, testTenantID, s.ID))
	assert.Empty(t, mock.RemoveVolumeCalls, "and none is removed")
}

func TestSandboxDisk_RunFailureRemovesNoVolume(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	mgr.SetSandboxWorkspaceTmpfs(true)
	mock.RunEnvironmentFn = func(context.Context, docker.RunEnvironmentConfig) (string, error) {
		return "", fmt.Errorf("runtime runsc-sbx not found")
	}
	_, err := mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "template"}})
	require.Error(t, err)
	assert.Empty(t, mock.RemoveVolumeCalls)
	var status string
	require.NoError(t, mgr.db.QueryRow(`SELECT status FROM environments`).Scan(&status))
	assert.Equal(t, "failed", status)
}

func TestSandboxDisk_Bounds(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	mgr.SetSandboxWorkspaceTmpfs(true)
	ctx := context.Background()
	img := SandboxSource{Type: "image", Image: "alpine"}

	for _, tc := range []struct {
		name string
		mem  int
		disk int
	}{
		{"below minimum", 512, 15},
		{"above memory", 512, 513},
		{"above tenant cap", 2048, 1025},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Source: img, MemoryMB: intp(tc.mem), DiskMB: intp(tc.disk)})
			assertAPICode(t, err, 400)
			assert.Contains(t, err.Error(), "disk_mb must be between")
		})
	}
	assert.Zero(t, mock.RunEnvironmentCalls)

	// Inclusive bounds.
	s, err := mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Source: img, MemoryMB: intp(512), DiskMB: intp(512)})
	require.NoError(t, err)
	assert.Equal(t, 512, s.DiskMB, "disk_mb may equal memory_mb")
	s, err = mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Source: img, DiskMB: intp(16)})
	require.NoError(t, err)
	assert.Equal(t, 16, s.DiskMB)

	// Default: half the memory, within the tenant cap.
	s, err = mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Source: img, MemoryMB: intp(512)})
	require.NoError(t, err)
	assert.Equal(t, 256, s.DiskMB)
	_, err = mgr.db.Exec(`UPDATE tenant_quotas SET max_sandbox_disk_mb = 100 WHERE tenant_id = ?`, testTenantID)
	require.NoError(t, err)
	s, err = mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Source: img, MemoryMB: intp(512)})
	require.NoError(t, err)
	assert.Equal(t, 100, s.DiskMB, "the default is clamped to max_sandbox_disk_mb")

	// A tenant cap below the minimum workspace refuses creates.
	_, err = mgr.db.Exec(`UPDATE tenant_quotas SET max_sandbox_disk_mb = 8 WHERE tenant_id = ?`, testTenantID)
	require.NoError(t, err)
	_, err = mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Source: img})
	assertAPICode(t, err, 400)
}

func TestResolveSandboxDisk(t *testing.T) {
	m := &Manager{}
	q := &sandboxQuota{maxDiskMB: 1024}
	d, err := m.resolveSandboxDisk(nil, 512, q)
	require.NoError(t, err)
	assert.Zero(t, d, "tmpfs off: no disk_mb")

	m.sandboxWorkspaceTmpfs = true
	d, err = m.resolveSandboxDisk(nil, 128, q)
	require.NoError(t, err)
	assert.Equal(t, 64, d)
	d, err = m.resolveSandboxDisk(nil, 2048, q)
	require.NoError(t, err)
	assert.Equal(t, 1024, d)
	d, err = m.resolveSandboxDisk(nil, 20, &sandboxQuota{maxDiskMB: 1024})
	require.NoError(t, err)
	assert.Equal(t, 16, d, "never below the minimum workspace")
}

func TestSandboxEgressGate(t *testing.T) {
	m := &Manager{}
	allowed := &sandboxQuota{egressAllowed: true}
	denied := &sandboxQuota{}

	net, err := m.sandboxEgressGate(SandboxEgressNone, denied)
	require.NoError(t, err)
	assert.Empty(t, net, "egress none uses the tenant network")

	_, err = m.sandboxEgressGate(SandboxEgressInternet, allowed)
	assert.Equal(t, errEgressUnavailable, err, "flag unset")

	m.SetSandboxEgressNetwork(testEgressNet)
	_, err = m.sandboxEgressGate(SandboxEgressInternet, denied)
	assert.Equal(t, errEgressNotAllowed, err, "tenant not allowed")

	net, err = m.sandboxEgressGate(SandboxEgressInternet, allowed)
	require.NoError(t, err)
	assert.Equal(t, testEgressNet, net)

	for in, want := range map[string]string{"": "none", "none": "none", "internet": "internet"} {
		got, err := parseSandboxEgress(in)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	_, err = parseSandboxEgress("INTERNET")
	assertAPICode(t, err, 400)
}
