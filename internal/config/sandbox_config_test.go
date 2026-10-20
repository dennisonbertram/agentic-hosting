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
