package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnvironmentContainerConfigs_DefaultIdle(t *testing.T) {
	cfg := RunEnvironmentConfig{TenantID: "t", EnvID: "e", Image: "ubuntu", MemoryMB: 256, CPUMillis: 500, VolumeName: "v", NetworkName: "n"}
	c, h := environmentContainerConfigs(cfg, "runsc")
	assert.Equal(t, []string{"sleep", "infinity"}, []string(c.Cmd))
	assert.Equal(t, "/workspace", c.WorkingDir)
	assert.Empty(t, c.Env)
	assert.Equal(t, "runsc", h.Runtime)
	assert.Equal(t, int64(256*1024*1024), h.Memory)
	assert.Equal(t, h.Memory, h.MemorySwap)
	assert.Equal(t, int64(500_000_000), h.NanoCPUs)
	assert.Equal(t, []string{"ALL"}, []string(h.CapDrop))
	assert.Contains(t, h.SecurityOpt, "no-new-privileges")
	require.NotNil(t, h.PidsLimit)
	assert.Equal(t, int64(256), *h.PidsLimit)
	assert.Equal(t, "environment", c.Labels["ah.type"])
}

func TestEnvironmentContainerConfigs_UseImageCmdAndEnv(t *testing.T) {
	env := []string{"A=1", "B=two"}
	cfg := RunEnvironmentConfig{TenantID: "t", EnvID: "e", Image: "img", UseImageCmd: true, Env: env, NetworkName: "n"}
	c, h := environmentContainerConfigs(cfg, "runc-dev")
	assert.Nil(t, c.Cmd, "UseImageCmd leaves Cmd nil so the image default applies")
	assert.Empty(t, c.WorkingDir)
	assert.Equal(t, env, c.Env)
	env[0] = "MUTATED=1"
	assert.Equal(t, "A=1", c.Env[0], "Env is copied")
	assert.Equal(t, "runc-dev", h.Runtime, "runtime comes from the client option")
	// Labels are unchanged for sandboxes: GC keys on ah.type=environment.
	assert.Equal(t, "environment", c.Labels["ah.type"])
	assert.Equal(t, "t", c.Labels["ah.tenant"])
	assert.Equal(t, "e", c.Labels["ah.environment"])
}

func TestEnsureEnvironmentImage(t *testing.T) {
	pullErr := errors.New("pull failed")
	var pulled int
	failPull := func(context.Context, string) error { pulled++; return pullErr }
	okPull := func(context.Context, string) error { pulled++; return nil }
	present := func(context.Context, string) (bool, error) { return true, nil }
	absent := func(context.Context, string) (bool, error) { return false, nil }
	ctx := context.Background()

	// Without LocalImage a failed pull is fatal even when the image exists.
	assert.ErrorIs(t, ensureEnvironmentImage(ctx, RunEnvironmentConfig{Image: "i"}, failPull, present), pullErr)
	// LocalImage: a present local image is used when the pull fails.
	assert.NoError(t, ensureEnvironmentImage(ctx, RunEnvironmentConfig{Image: "i", LocalImage: true}, failPull, present))
	// LocalImage: a missing image fails.
	assert.ErrorIs(t, ensureEnvironmentImage(ctx, RunEnvironmentConfig{Image: "i", LocalImage: true}, failPull, absent), pullErr)
	pulled = 0
	assert.NoError(t, ensureEnvironmentImage(ctx, RunEnvironmentConfig{Image: "i", LocalImage: true}, okPull, absent))
	assert.Equal(t, 1, pulled)
	// ImagePrepared: the caller already pulled it; nothing is pulled again.
	pulled = 0
	assert.NoError(t, ensureEnvironmentImage(ctx, RunEnvironmentConfig{Image: "i", ImagePrepared: true}, failPull, absent))
	assert.Zero(t, pulled)
}
