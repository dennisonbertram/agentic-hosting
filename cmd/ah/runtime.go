package main

import (
	"fmt"

	"github.com/dennisonbertram/agentic-hosting/internal/config"
)

// checkContainerRuntime decides whether the configured workload runtime may be
// used. runsc (gVisor) is always allowed. Anything else is a dev-only fallback
// for hosts that cannot run gVisor (macOS laptops): it is refused without
// --dev, and with --dev it returns warning lines the caller must log loudly so
// nobody mistakes a sandbox for an isolated deployment.
func checkContainerRuntime(runtime string, devMode bool) ([]string, error) {
	if runtime == config.RuntimeRunsc {
		return nil, nil
	}
	if !devMode {
		return nil, fmt.Errorf("AH_CONTAINER_RUNTIME=%s is a dev-only fallback and requires --dev; production must use %s (gVisor)", runtime, config.RuntimeRunsc)
	}
	return []string{
		"################################################################",
		fmt.Sprintf("WARNING: AH_CONTAINER_RUNTIME=%s -- gVisor (runsc) is NOT in use.", runtime),
		"WARNING: tenant containers share the host kernel with no syscall sandbox.",
		"WARNING: this is a LOCAL DEV SANDBOX ONLY. Never run untrusted code or",
		"WARNING: expose this instance to a network. Production requires runsc.",
		"################################################################",
	}, nil
}
