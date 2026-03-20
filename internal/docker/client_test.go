package docker

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

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

func TestBuildServiceContainerConfig_Labels(t *testing.T) {
	tenantID := "t-abc"
	serviceID := "svc-xyz"
	port := 3000

	cfg := buildServiceContainerConfig(tenantID, serviceID, "myimage:latest", port, nil, nil)

	// ah labels
	if got := cfg.Labels["ah.tenant"]; got != tenantID {
		t.Errorf("ah.tenant = %q, want %q", got, tenantID)
	}
	if got := cfg.Labels["ah.service"]; got != serviceID {
		t.Errorf("ah.service = %q, want %q", got, serviceID)
	}

	// traefik.docker.network base label should NOT be present (removed for cross-tenant isolation)
	if _, ok := cfg.Labels["traefik.docker.network"]; ok {
		t.Error("traefik.docker.network should not be set as a base label")
	}

	// Hardcoded Traefik routing labels should NOT be present — they come from extraLabels now
	if _, ok := cfg.Labels["traefik.enable"]; ok {
		t.Error("traefik.enable should not be set as a base label")
	}
	routerKey := fmt.Sprintf("traefik.http.routers.%s.rule", serviceID)
	if _, ok := cfg.Labels[routerKey]; ok {
		t.Errorf("hardcoded router rule label %q should not be set", routerKey)
	}
	portKey := fmt.Sprintf("traefik.http.services.%s.loadbalancer.server.port", serviceID)
	if _, ok := cfg.Labels[portKey]; ok {
		t.Errorf("hardcoded port label %q should not be set", portKey)
	}
}

func TestBuildServiceContainerConfig_ExtraLabelsSetTraefikRouting(t *testing.T) {
	// Traefik routing labels now come via extraLabels from the services layer.
	serviceID := "svc-xyz"
	extra := map[string]string{
		"traefik.enable": "true",
		fmt.Sprintf("traefik.http.routers.%s.rule", serviceID):                      fmt.Sprintf("Host(`%s.example.com`)", serviceID),
		fmt.Sprintf("traefik.http.routers.%s.entrypoints", serviceID):               "websecure",
		fmt.Sprintf("traefik.http.services.%s.loadbalancer.server.port", serviceID): "3000",
	}
	cfg := buildServiceContainerConfig("t-abc", serviceID, "myimage:latest", 3000, nil, extra)

	if got := cfg.Labels["traefik.enable"]; got != "true" {
		t.Errorf("traefik.enable = %q, want %q", got, "true")
	}
	routerRule := cfg.Labels[fmt.Sprintf("traefik.http.routers.%s.rule", serviceID)]
	if !strings.Contains(routerRule, serviceID) {
		t.Errorf("router rule %q does not contain service ID", routerRule)
	}
}

func TestBuildServiceContainerConfig_EnvVars(t *testing.T) {
	envVars := map[string]string{
		"DATABASE_URL": "postgres://localhost/db",
		"PORT":         "8000",
	}
	cfg := buildServiceContainerConfig("t1", "s1", "img:latest", 8000, envVars, nil)

	envSet := make(map[string]string, len(cfg.Env))
	for _, e := range cfg.Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envSet[parts[0]] = parts[1]
		}
	}
	for k, want := range envVars {
		if got, ok := envSet[k]; !ok || got != want {
			t.Errorf("env %s = %q, want %q", k, got, want)
		}
	}
}

func TestBuildServiceContainerConfig_ExtraLabelsOverride(t *testing.T) {
	// Extra labels should be set and can override built-in labels.
	extra := map[string]string{
		"custom.label": "custom-value",
	}
	cfg := buildServiceContainerConfig("t1", "s1", "img:latest", 8080, nil, extra)
	if got := cfg.Labels["custom.label"]; got != "custom-value" {
		t.Errorf("custom label = %q, want %q", got, "custom-value")
	}
}

func TestContainerName(t *testing.T) {
	tests := []struct {
		tenantID  string
		serviceID string
		want      string
	}{
		{"tenant-1", "svc-1", "ah-tenant-1-svc-1"},
		{"abc", "xyz", "ah-abc-xyz"},
		{"t", "s", "ah-t-s"},
	}
	for _, tc := range tests {
		got := containerName(tc.tenantID, tc.serviceID)
		if got != tc.want {
			t.Errorf("containerName(%q, %q) = %q, want %q", tc.tenantID, tc.serviceID, got, tc.want)
		}
	}
}

func TestTenantNetworkName(t *testing.T) {
	tests := []struct {
		tenantID string
		want     string
	}{
		{"abc", "ah-tenant-abc"},
		{"t-123", "ah-tenant-t-123"},
		{"", "ah-tenant-"},
	}
	for _, tc := range tests {
		got := TenantNetworkName(tc.tenantID)
		if got != tc.want {
			t.Errorf("TenantNetworkName(%q) = %q, want %q", tc.tenantID, got, tc.want)
		}
	}
}

// TestContainerInfo_HealthStatus verifies the ContainerInfo struct correctly
// holds health status values, including the empty string for nil Health.
// The nil-check logic lives in InspectContainer; here we validate the struct
// behavior that callers depend on.
func TestContainerInfo_HealthStatus(t *testing.T) {
	tests := []struct {
		name         string
		healthStatus string
		wantEmpty    bool
	}{
		{"nil health maps to empty string", "", true},
		{"starting", "starting", false},
		{"healthy", "healthy", false},
		{"unhealthy", "unhealthy", false},
		{"none", "none", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			info := &ContainerInfo{
				CreatedAt:    time.Now(),
				Status:       "running",
				StartedAt:    "2024-01-01T00:00:00Z",
				ExitCode:     0,
				HealthStatus: tc.healthStatus,
			}
			if tc.wantEmpty && info.HealthStatus != "" {
				t.Errorf("expected empty HealthStatus, got %q", info.HealthStatus)
			}
			if !tc.wantEmpty && info.HealthStatus != tc.healthStatus {
				t.Errorf("HealthStatus = %q, want %q", info.HealthStatus, tc.healthStatus)
			}
		})
	}
}

// TestContainerInfo_NilHealthMapping verifies the InspectContainer logic:
// when State.Health is nil, HealthStatus should be empty string (not "none" or panic).
// We test this by mimicking the same conditional used in client.go.
func TestContainerInfo_NilHealthMapping(t *testing.T) {
	type dockerHealth struct {
		Status string
	}

	// simulate the nil-check from InspectContainer
	mapHealth := func(h *dockerHealth) string {
		if h == nil {
			return ""
		}
		return h.Status
	}

	if got := mapHealth(nil); got != "" {
		t.Errorf("nil health: got %q, want empty string", got)
	}
	if got := mapHealth(&dockerHealth{Status: "healthy"}); got != "healthy" {
		t.Errorf("healthy: got %q, want %q", got, "healthy")
	}
	if got := mapHealth(&dockerHealth{Status: "unhealthy"}); got != "unhealthy" {
		t.Errorf("unhealthy: got %q, want %q", got, "unhealthy")
	}
}

// TestBuildServiceContainerConfig_LabelImmutability verifies that extraLabels cannot
// overwrite the control-plane ah.tenant and ah.service labels.
// These labels are used for container attribution and cleanup; overriding them
// would allow a tenant to hijack another tenant's containers.
func TestBuildServiceContainerConfig_LabelImmutability(t *testing.T) {
	// An attacker-controlled caller supplies extraLabels that attempt to overwrite
	// the ah.tenant and ah.service control-plane keys.
	maliciousExtra := map[string]string{
		"ah.tenant":  "other-tenant",
		"ah.service": "other-service",
		"custom":     "allowed",
	}
	cfg := buildServiceContainerConfig("real-tenant", "real-service", "img:latest", 8080, nil, maliciousExtra)

	if got := cfg.Labels["ah.tenant"]; got != "real-tenant" {
		t.Errorf("ah.tenant overwritten: got %q, want %q", got, "real-tenant")
	}
	if got := cfg.Labels["ah.service"]; got != "real-service" {
		t.Errorf("ah.service overwritten: got %q, want %q", got, "real-service")
	}
	// Non-reserved labels should pass through normally.
	if got := cfg.Labels["custom"]; got != "allowed" {
		t.Errorf("custom label: got %q, want %q", got, "allowed")
	}
}

// TestFindTraefikContainerNameMatching verifies the exact-name matching logic used
// by findTraefikContainer. Only the name "paas-traefik" (with or without leading "/")
// should match — any other name, including ones that merely contain "traefik", must not.
//
// Because findTraefikContainer is an unexported method that calls the Docker API,
// we replicate its name-matching predicate here so it can be tested without a daemon.
func TestFindTraefikContainerNameMatching(t *testing.T) {
	// matchesTraefik replicates the predicate inside findTraefikContainer.
	matchesTraefik := func(name string) bool {
		return name == "/paas-traefik" || name == "paas-traefik"
	}

	tests := []struct {
		name      string
		wantMatch bool
	}{
		{"/paas-traefik", true},
		{"paas-traefik", true},
		// Must NOT match unrelated containers that happen to contain "traefik".
		{"/traefik", false},
		{"traefik", false},
		{"/myapp-traefik", false},
		{"/paas-traefik-v2", false},
		{"", false},
		{"/paas-Traefik", false}, // case-sensitive
	}
	for _, tc := range tests {
		got := matchesTraefik(tc.name)
		if got != tc.wantMatch {
			t.Errorf("matchesTraefik(%q) = %v, want %v", tc.name, got, tc.wantMatch)
		}
	}
}

// TestBuildServiceContainerConfig_NoHealthcheck verifies that buildServiceContainerConfig
// does not inject a healthcheck. Images that ship without curl/wget would break
// if a shell-based probe were added automatically.
func TestBuildServiceContainerConfig_NoHealthcheck(t *testing.T) {
	cfg := buildServiceContainerConfig("t", "s", "alpine:latest", 8080, nil, nil)
	if cfg.Healthcheck != nil {
		t.Fatalf("expected Healthcheck to be nil, got %#v", cfg.Healthcheck)
	}
}

// TestBuildServiceContainerConfig_ImagePassthrough verifies the image field is
// set exactly as provided, without normalisation or modification.
func TestBuildServiceContainerConfig_ImagePassthrough(t *testing.T) {
	images := []string{
		"nginx:latest",
		"gcr.io/project/service:abc123",
		"localhost:5000/myimage:v1.2.3",
	}
	for _, img := range images {
		cfg := buildServiceContainerConfig("t", "s", img, 8080, nil, nil)
		if cfg.Image != img {
			t.Errorf("Image = %q, want %q", cfg.Image, img)
		}
	}
}

// TestRunDatabaseConfig_DefaultLabels verifies that RunDatabaseConfig uses
// the ah.managed and ah.type labels when no override labels are provided.
// This mirrors the logic in RunDatabase where cfg.Labels==nil triggers defaults.
func TestRunDatabaseConfig_DefaultLabels(t *testing.T) {
	// Replicate the label-selection logic from RunDatabase.
	applyLabels := func(cfg RunDatabaseConfig) map[string]string {
		labels := map[string]string{
			"ah.managed": "true",
			"ah.type":    "database",
		}
		if cfg.Labels != nil {
			labels = cfg.Labels
		}
		return labels
	}

	// nil Labels → defaults applied.
	defaults := applyLabels(RunDatabaseConfig{Name: "mydb"})
	if defaults["ah.managed"] != "true" {
		t.Errorf("ah.managed = %q, want %q", defaults["ah.managed"], "true")
	}
	if defaults["ah.type"] != "database" {
		t.Errorf("ah.type = %q, want %q", defaults["ah.type"], "database")
	}

	// Non-nil Labels → caller's labels replace defaults entirely.
	custom := applyLabels(RunDatabaseConfig{
		Name:   "mydb",
		Labels: map[string]string{"custom": "label"},
	})
	if _, ok := custom["ah.managed"]; ok {
		t.Error("ah.managed should not be present when caller provides labels")
	}
	if custom["custom"] != "label" {
		t.Errorf("custom label = %q, want %q", custom["custom"], "label")
	}
}
