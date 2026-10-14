package docker

import "testing"

func TestEnvironmentLabels_KeepsWarmPoolType(t *testing.T) {
	l := environmentContainerLabels(RunEnvironmentConfig{TenantID: "pool", EnvID: "wp_1", Labels: map[string]string{"ah.type": "warm-pool"}})
	if l["ah.type"] != "warm-pool" {
		t.Fatalf("ah.type = %q, want warm-pool", l["ah.type"])
	}
	l = environmentContainerLabels(RunEnvironmentConfig{TenantID: "t", EnvID: "e"})
	if l["ah.type"] != "environment" || l["ah.tenant"] != "t" || l["traefik.enable"] != "false" {
		t.Fatalf("bad default labels: %v", l)
	}
}
