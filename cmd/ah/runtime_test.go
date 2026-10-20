package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"

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

func TestIsLoopbackListen(t *testing.T) {
	for _, h := range []string{"", "127.0.0.1", "127.0.0.2", "::1", "[::1]", "localhost"} {
		assert.True(t, isLoopbackListen(h), h)
	}
	for _, h := range []string{"0.0.0.0", "::", "192.168.1.5", "example.com"} {
		assert.False(t, isLoopbackListen(h), h)
	}
}

// Regression: a runc (non-gVisor) workload or sandbox runtime must not be
// served on a network address, even with --dev.
func TestCheckListenIsolation(t *testing.T) {
	ctx := context.Background()
	d := &fakeHardeningDocker{runtimes: map[string]docker.RuntimeInfo{
		"runsc-sbx": {Path: "/usr/local/bin/runsc"},
		"runc":      {Path: "runc"},
	}}

	// Loopback is always fine, whatever the runtime.
	require.NoError(t, checkListenIsolation(ctx, "", "runc", "runc", d))
	require.NoError(t, checkListenIsolation(ctx, "127.0.0.1", "runc", "", d))

	// gVisor everywhere may listen on a network address.
	require.NoError(t, checkListenIsolation(ctx, "0.0.0.0", "runsc", "", d))
	require.NoError(t, checkListenIsolation(ctx, "0.0.0.0", "runsc", "runsc-sbx", d))

	err := checkListenIsolation(ctx, "0.0.0.0", "runc", "", d)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AH_CONTAINER_RUNTIME=runc is not gVisor")
	assert.Contains(t, err.Error(), "loopback")

	err = checkListenIsolation(ctx, "0.0.0.0", "runsc", "runc", d)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AH_SANDBOX_RUNTIME=runc")

	// A sandbox runtime that cannot be looked up fails closed.
	err = checkListenIsolation(ctx, "::", "runsc", "runsc-sbx", &fakeHardeningDocker{infoErr: errors.New("daemon down")})
	require.Error(t, err)
}
