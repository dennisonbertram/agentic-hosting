package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadFromEnv_MaxSandboxesTotal(t *testing.T) {
	t.Setenv("AH_MAX_SANDBOXES_TOTAL", "")
	cfg, err := LoadFromEnv()
	require.NoError(t, err)
	assert.Equal(t, DefaultMaxSandboxesTotal, cfg.MaxSandboxesTotal)
	assert.Equal(t, DefaultMaxSandboxesTotal, Default().MaxSandboxesTotal)

	t.Setenv("AH_MAX_SANDBOXES_TOTAL", "0")
	cfg, err = LoadFromEnv()
	require.NoError(t, err)
	assert.Equal(t, 0, cfg.MaxSandboxesTotal)

	t.Setenv("AH_MAX_SANDBOXES_TOTAL", "-1")
	cfg, err = LoadFromEnv()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AH_MAX_SANDBOXES_TOTAL")
	assert.Equal(t, DefaultMaxSandboxesTotal, cfg.MaxSandboxesTotal)

	t.Setenv("AH_MAX_SANDBOXES_TOTAL", "many")
	_, err = LoadFromEnv()
	require.Error(t, err)
}

func TestLoadFromEnv_SandboxHardeningDefaultsOff(t *testing.T) {
	for _, k := range []string{"AH_SANDBOX_EGRESS_NETWORK", "AH_SANDBOX_EGRESS_SUBNET", "AH_SANDBOX_EGRESS_SKIP_FIREWALL_CHECK", "AH_SANDBOX_RUNTIME", "AH_SANDBOX_WORKSPACE_TMPFS"} {
		t.Setenv(k, "")
	}
	cfg, err := LoadFromEnv()
	require.NoError(t, err)
	assert.Empty(t, cfg.SandboxEgressNetwork, "egress is off unless configured")
	assert.Equal(t, DefaultSandboxEgressSubnet, cfg.SandboxEgressSubnet)
	assert.False(t, cfg.SandboxEgressSkipFirewallCheck)
	assert.Empty(t, cfg.SandboxRuntime)
	assert.False(t, cfg.SandboxWorkspaceTmpfs)

	d := Default()
	assert.Empty(t, d.SandboxEgressNetwork)
	assert.Empty(t, d.SandboxRuntime)
	assert.False(t, d.SandboxWorkspaceTmpfs)
}

func TestLoadFromEnv_SandboxHardeningSet(t *testing.T) {
	t.Setenv("AH_SANDBOX_EGRESS_NETWORK", " ah-sbx-egress ")
	t.Setenv("AH_SANDBOX_EGRESS_SUBNET", "10.77.0.0/24")
	t.Setenv("AH_SANDBOX_EGRESS_SKIP_FIREWALL_CHECK", "1")
	t.Setenv("AH_SANDBOX_RUNTIME", "runsc-sbx")
	t.Setenv("AH_SANDBOX_WORKSPACE_TMPFS", "true")
	cfg, err := LoadFromEnv()
	require.NoError(t, err)
	assert.Equal(t, "ah-sbx-egress", cfg.SandboxEgressNetwork)
	assert.Equal(t, "10.77.0.0/24", cfg.SandboxEgressSubnet)
	assert.True(t, cfg.SandboxEgressSkipFirewallCheck)
	assert.Equal(t, "runsc-sbx", cfg.SandboxRuntime)
	assert.True(t, cfg.SandboxWorkspaceTmpfs)
}

func TestLoadFromEnv_SandboxHardeningInvalid(t *testing.T) {
	t.Setenv("AH_SANDBOX_RUNTIME", "runsc sbx; rm")
	cfg, err := LoadFromEnv()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AH_SANDBOX_RUNTIME")
	assert.Empty(t, cfg.SandboxRuntime)

	t.Setenv("AH_SANDBOX_RUNTIME", "")
	t.Setenv("AH_SANDBOX_WORKSPACE_TMPFS", "yes please")
	_, err = LoadFromEnv()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AH_SANDBOX_WORKSPACE_TMPFS")
}
