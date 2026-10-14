package docker

import "testing"

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
