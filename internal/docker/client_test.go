package docker

import "testing"

func TestBuildServiceContainerConfig_DoesNotInjectHealthcheck(t *testing.T) {
	cfg := buildServiceContainerConfig(
		"tenant-1",
		"svc-1",
		"nginx:latest",
		8080,
		map[string]string{"PORT": "8080"},
		map[string]string{"custom": "value"},
	)

	if cfg.Healthcheck != nil {
		t.Fatalf("expected no injected healthcheck, got %#v", cfg.Healthcheck)
	}
	if got := cfg.Labels["ah.tenant"]; got != "tenant-1" {
		t.Fatalf("expected tenant label to be preserved, got %q", got)
	}
	if got := cfg.Labels["custom"]; got != "value" {
		t.Fatalf("expected extra label to be preserved, got %q", got)
	}
}
