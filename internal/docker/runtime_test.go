package docker

import "testing"

func TestDefaultRuntimeIsGVisor(t *testing.T) {
	if DefaultRuntime != "runsc" {
		t.Fatalf("DefaultRuntime = %q, want runsc", DefaultRuntime)
	}
	// A zero-value client (e.g. constructed in tests) must never fall back to
	// Docker's default runc.
	if got := (&DockerClient{}).Runtime(); got != "runsc" {
		t.Errorf("zero-value Runtime() = %q, want runsc", got)
	}
}

func TestNewClientUsesRunsc(t *testing.T) {
	c, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()
	if c.Runtime() != "runsc" {
		t.Errorf("NewClient runtime = %q, want runsc", c.Runtime())
	}
}

func TestNewClientWithRuntime(t *testing.T) {
	c, err := NewClientWithRuntime("runc")
	if err != nil {
		t.Fatalf("NewClientWithRuntime: %v", err)
	}
	defer c.Close()
	if c.Runtime() != "runc" {
		t.Errorf("runtime = %q, want runc", c.Runtime())
	}
	if _, err := NewClientWithRuntime("  "); err == nil {
		t.Error("empty runtime accepted")
	}
}

func TestHostConfigsHonorRuntime(t *testing.T) {
	_, wipe := wipeConfigs("v", "runc")
	if wipe.Runtime != "runc" {
		t.Errorf("wipe runtime = %q, want runc", wipe.Runtime)
	}
	cfg := RunDatabaseConfig{HostPort: 5432, ContainerPort: 5432, VolumeName: "v", MountPath: "/data"}
	if h := databaseHostConfig(cfg, "5432/tcp", "runc"); h.Runtime != "runc" {
		t.Errorf("database runtime = %q, want runc", h.Runtime)
	}
	if h := databaseHostConfig(cfg, "5432/tcp", DefaultRuntime); h.Runtime != "runsc" {
		t.Errorf("database runtime = %q, want runsc", h.Runtime)
	}
}
