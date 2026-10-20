package docker

import (
	"testing"

	"github.com/docker/go-connections/nat"
)

func TestDatabaseHostConfig_Hardened(t *testing.T) {
	h := databaseHostConfig(RunDatabaseConfig{HostPort: 5432, ContainerPort: 5432, VolumeName: "v", MountPath: "/data"}, "5432/tcp")
	if h.Memory <= 0 || h.MemorySwap != h.Memory {
		t.Errorf("memory limit not set without swap: %d/%d", h.Memory, h.MemorySwap)
	}
	if h.PidsLimit == nil || *h.PidsLimit <= 0 {
		t.Error("pids limit not set")
	}
	if len(h.SecurityOpt) != 1 || h.SecurityOpt[0] != "no-new-privileges" {
		t.Errorf("SecurityOpt = %v", h.SecurityOpt)
	}
	if len(h.CapDrop) != 1 || h.CapDrop[0] != "ALL" {
		t.Errorf("CapDrop = %v", h.CapDrop)
	}
	for _, need := range []string{"CHOWN", "SETUID", "SETGID"} {
		found := false
		for _, c := range h.CapAdd {
			found = found || c == need
		}
		if !found {
			t.Errorf("CapAdd missing %s (entrypoint needs it)", need)
		}
	}
}

// TestIssue164_DatabasePublishedOnHostLoopbackOnly documents the topology that
// makes a service container unable to reach its managed database (#164).
// databaseHostConfig publishes the engine port on 127.0.0.1 and does not set
// NetworkMode, so the container lands on Docker's default bridge rather than
// the tenant's internal network. A service on ah-tenant-<id> (Internal=true,
// ICC=false) has its own loopback, so 127.0.0.1 in DATABASE_URL is itself.
func TestIssue164_DatabasePublishedOnHostLoopbackOnly(t *testing.T) {
	h := databaseHostConfig(RunDatabaseConfig{HostPort: 5456, ContainerPort: 5432, VolumeName: "v", MountPath: "/data"}, "5432/tcp")
	bindings := h.PortBindings[nat.Port("5432/tcp")]
	if len(bindings) != 1 {
		t.Fatalf("PortBindings[5432/tcp] = %#v, want one loopback binding", bindings)
	}
	if bindings[0].HostIP != "127.0.0.1" {
		t.Errorf("HostIP = %q, want 127.0.0.1 (issue #164: service containers cannot use this address)", bindings[0].HostIP)
	}
	if bindings[0].HostPort != "5456" {
		t.Errorf("HostPort = %q, want 5456", bindings[0].HostPort)
	}
	if h.NetworkMode != "" {
		t.Errorf("NetworkMode = %q, want empty (default bridge, not a tenant network)", h.NetworkMode)
	}
}
