package docker

import (
	"testing"

	"github.com/docker/go-connections/nat"
)

func TestDatabaseHostConfig_Hardened(t *testing.T) {
	h := databaseHostConfig(RunDatabaseConfig{HostPort: 5432, ContainerPort: 5432, VolumeName: "v", MountPath: "/data"}, "5432/tcp", DefaultRuntime)
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

// TestIssue164_DatabaseKeepsHostLoopbackAndJoinsDatabaseNetwork is the fixed
// topology for #164: health probes still use 127.0.0.1 (never 0.0.0.0), and
// managed databases sit on ah-dbnet-* instead of the default bridge.
func TestIssue164_DatabaseKeepsHostLoopbackAndJoinsDatabaseNetwork(t *testing.T) {
	netName := DatabaseNetworkName("tenant-1", "db-1")
	h := databaseHostConfig(RunDatabaseConfig{
		HostPort: 5456, ContainerPort: 5432, VolumeName: "v", MountPath: "/data",
		NetworkName: netName, NetworkAlias: DatabaseAlias("db-1"),
	}, "5432/tcp", DefaultRuntime)
	bindings := h.PortBindings[nat.Port("5432/tcp")]
	if len(bindings) != 1 {
		t.Fatalf("PortBindings[5432/tcp] = %#v, want one loopback binding", bindings)
	}
	if bindings[0].HostIP != "127.0.0.1" {
		t.Errorf("HostIP = %q, want 127.0.0.1 for control-plane probes", bindings[0].HostIP)
	}
	if bindings[0].HostIP == "0.0.0.0" {
		t.Fatal("must not bind the engine on 0.0.0.0")
	}
	if bindings[0].HostPort != "5456" {
		t.Errorf("HostPort = %q, want 5456", bindings[0].HostPort)
	}
	if string(h.NetworkMode) != netName {
		t.Errorf("NetworkMode = %q, want %q", h.NetworkMode, netName)
	}

	kanban := databaseHostConfig(RunDatabaseConfig{HostPort: 7200, ContainerPort: 3000, VolumeName: "v", MountPath: "/data"}, "3000/tcp", DefaultRuntime)
	if kanban.NetworkMode != "" {
		t.Errorf("kanban NetworkMode = %q, want empty (loopback-only, no dbnet)", kanban.NetworkMode)
	}
	if kanban.PortBindings[nat.Port("3000/tcp")][0].HostIP != "127.0.0.1" {
		t.Fatal("kanban must stay on host loopback")
	}
}
