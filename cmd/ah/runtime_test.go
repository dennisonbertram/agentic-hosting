package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckContainerRuntime_RunscAlwaysAllowed(t *testing.T) {
	for _, dev := range []bool{false, true} {
		warnings, err := checkContainerRuntime("runsc", dev)
		require.NoError(t, err)
		assert.Empty(t, warnings)
	}
}

func TestCheckContainerRuntime_RuncRefusedInProduction(t *testing.T) {
	_, err := checkContainerRuntime("runc", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires --dev")
}

func TestCheckContainerRuntime_RuncInDevWarnsLoudly(t *testing.T) {
	warnings, err := checkContainerRuntime("runc", true)
	require.NoError(t, err)
	joined := strings.Join(warnings, "\n")
	assert.Contains(t, joined, "gVisor (runsc) is NOT in use")
	assert.Contains(t, joined, "DEV SANDBOX ONLY")
}
