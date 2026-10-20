package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Environment credentials must never be written into the saved config.
func TestConfigure_DoesNotPersistEnvCredentials(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`{"url":"https://old.example.com","key":"saved-user-key"}`), 0600))
	t.Setenv("AH_URL", "https://ci.example.com")
	t.Setenv("AH_KEY", "ci-secret-key")

	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{"configure", "--url", "https://new.example.com", "--config-path", cfgPath})
	require.NoError(t, cmd.Execute())

	data, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	var cfg map[string]string
	require.NoError(t, json.Unmarshal(data, &cfg))
	assert.Equal(t, "https://new.example.com", cfg["url"])
	assert.Equal(t, "saved-user-key", cfg["key"], "the user's saved key must be kept, not replaced by AH_KEY")
}
