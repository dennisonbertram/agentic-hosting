package main

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/dennisonbertram/agentic-hosting/internal/config"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
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

// isLoopbackListen reports whether the --listen-addr host binds loopback
// only. Empty means the 127.0.0.1 default.
func isLoopbackListen(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if host == "" || strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkListenIsolation refuses a non-loopback listen address while tenant
// workloads or sandboxes run without gVisor (runc and friends are dev-only
// fallbacks that share the host kernel). Combined with --dev and
// --open-registration such an instance would let anyone who can reach the
// port run code on the host. The sandbox runtime is looked up with Docker;
// a lookup that fails counts as not gVisor (fail closed).
func checkListenIsolation(ctx context.Context, listenAddr, containerRuntime, sandboxRuntime string, d sandboxHardeningDocker) error {
	if isLoopbackListen(listenAddr) {
		return nil
	}
	refuse := func(what string) error {
		return fmt.Errorf("%s is not gVisor (runsc), so ah may only listen on loopback; got --listen-addr %s. Bind to 127.0.0.1, or use runsc to serve a network", what, listenAddr)
	}
	if containerRuntime != config.RuntimeRunsc {
		return refuse("AH_CONTAINER_RUNTIME=" + containerRuntime)
	}
	if sandboxRuntime == "" {
		return nil
	}
	rt, found, err := d.LookupRuntime(ctx, sandboxRuntime)
	if err != nil || !found || !docker.IsGVisorRuntime(sandboxRuntime, rt) {
		return refuse("AH_SANDBOX_RUNTIME=" + sandboxRuntime)
	}
	return nil
}
