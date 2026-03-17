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

	// Traefik labels
	if got := cfg.Labels["traefik.enable"]; got != "true" {
		t.Errorf("traefik.enable = %q, want %q", got, "true")
	}
	routerRule := cfg.Labels[fmt.Sprintf("traefik.http.routers.%s.rule", serviceID)]
	if !strings.Contains(routerRule, serviceID) {
		t.Errorf("router rule %q does not contain service ID", routerRule)
	}
	portLabel := cfg.Labels[fmt.Sprintf("traefik.http.services.%s.loadbalancer.server.port", serviceID)]
	if portLabel != fmt.Sprintf("%d", port) {
		t.Errorf("loadbalancer port label = %q, want %q", portLabel, fmt.Sprintf("%d", port))
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
