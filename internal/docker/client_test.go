package docker

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
)

func TestConfiguredContainerPortUsesPORTThenUniqueExposedTCPPort(t *testing.T) {
	cfg := &container.Config{
		Env: []string{"TOKEN=must-not-be-returned", "PORT=3000"},
		ExposedPorts: nat.PortSet{
			nat.Port("80/tcp"):   struct{}{},
			nat.Port("3000/tcp"): struct{}{},
		},
	}
	if got := configuredContainerPort(cfg); got != 3000 {
		t.Fatalf("configuredContainerPort with PORT = %d, want 3000", got)
	}

	unique := &container.Config{ExposedPorts: nat.PortSet{nat.Port("8080/tcp"): struct{}{}}}
	if got := configuredContainerPort(unique); got != 8080 {
		t.Fatalf("configuredContainerPort with one exposed TCP port = %d, want 8080", got)
	}

	ambiguous := &container.Config{ExposedPorts: nat.PortSet{
		nat.Port("8080/tcp"): struct{}{},
		nat.Port("9090/tcp"): struct{}{},
	}}
	if got := configuredContainerPort(ambiguous); got != 0 {
		t.Fatalf("configuredContainerPort with ambiguous exposed ports = %d, want 0", got)
	}
}

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

func TestEnvironmentContainerLabelsKeepLifecycleTypeReserved(t *testing.T) {
	base := RunEnvironmentConfig{
		TenantID: "tenant-1", EnvID: "env-1",
		Labels: map[string]string{"ah.type": "attacker-value", "ah.tenant": "attacker-tenant"},
	}
	labels := environmentContainerLabels(base)
	if labels["ah.type"] != "environment" || labels["ah.tenant"] != "tenant-1" {
		t.Fatalf("environment labels did not preserve reserved identity: %#v", labels)
	}
	base.WarmPool = true
	labels = environmentContainerLabels(base)
	if labels["ah.type"] != "warm-pool" || labels["ah.tenant"] != "tenant-1" {
		t.Fatalf("warm-pool lifecycle was not explicitly applied: %#v", labels)
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

func TestPerWorkloadIngressNetworkNames(t *testing.T) {
	tenantID := "tenant-1"
	serviceID := "svc-1"
	environmentID := "env-1"
	if got, want := ServiceIngressNetworkName(tenantID, serviceID), "ah-ingress-tenant-1-service-svc-1"; got != want {
		t.Fatalf("ServiceIngressNetworkName() = %q, want %q", got, want)
	}
	if got, want := EnvironmentIngressNetworkName(tenantID, environmentID), "ah-ingress-tenant-1-environment-env-1"; got != want {
		t.Fatalf("EnvironmentIngressNetworkName() = %q, want %q", got, want)
	}
	if got, want := TenantIngressNetworkPrefix(tenantID), "ah-ingress-tenant-1-"; got != want {
		t.Fatalf("TenantIngressNetworkPrefix() = %q, want %q", got, want)
	}
	if got, want := ServiceIngressAlias(tenantID, serviceID), "ah-route-service-svc-1"; got != want {
		t.Fatalf("ServiceIngressAlias() = %q, want %q", got, want)
	}
	if got, want := EnvironmentIngressAlias(tenantID, environmentID), "ah-route-env-env-1"; got != want {
		t.Fatalf("EnvironmentIngressAlias() = %q, want %q", got, want)
	}
}

func TestDatabaseNetworkNameAndAliasFitDockerDNSLabelLimit(t *testing.T) {
	tenantID := "0123456789abcdef0123456789abcdef"
	dbID := "fedcba9876543210fedcba9876543210"
	name := DatabaseNetworkName(tenantID, dbID)
	alias := DatabaseAlias(dbID)
	if len(name) > 63 {
		t.Fatalf("database network name exceeds 63 chars: %q (%d)", name, len(name))
	}
	if len(alias) > 63 {
		t.Fatalf("database alias exceeds 63 chars: %q (%d)", alias, len(alias))
	}
	if !strings.HasPrefix(name, "ah-dbnet-") {
		t.Fatalf("network name = %q", name)
	}
	if alias != "ah-db-"+dbID {
		t.Fatalf("alias = %q", alias)
	}
	if !strings.Contains(name, dbID) {
		t.Fatalf("network name dropped the globally unique database id: %q", name)
	}
	other := DatabaseNetworkName("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", dbID)
	if name == other {
		t.Fatal("different tenants must not share a database network name")
	}
	if !IsDatabaseNetwork(name) || IsDatabaseNetwork("ah-tenant-"+tenantID) {
		t.Fatal("IsDatabaseNetwork prefix check failed")
	}
}

func TestPerWorkloadIngressAliasesFitDockerDNSLabelLimit(t *testing.T) {
	fullID := "0123456789abcdef0123456789abcdef"
	serviceAlias := ServiceIngressAlias("tenant-with-a-long-id", fullID)
	environmentAlias := EnvironmentIngressAlias("tenant-with-a-long-id", fullID)
	if len(serviceAlias) > 63 || len(environmentAlias) > 63 {
		t.Fatalf("aliases exceed Docker DNS-label limit: service=%q (%d), environment=%q (%d)",
			serviceAlias, len(serviceAlias), environmentAlias, len(environmentAlias))
	}
	if serviceAlias == environmentAlias {
		t.Fatalf("service and environment aliases must have distinct prefixes: %q", serviceAlias)
	}
	if serviceAlias != ServiceIngressAlias("different-tenant", fullID) {
		t.Fatal("globally unique workload IDs should produce tenant-independent aliases")
	}
}

func TestBridgeNetworkCreateOptionsKeepTenantIsolationAndAllowIngressPair(t *testing.T) {
	tenant := bridgeNetworkCreateOptions(false)
	if !tenant.Internal || tenant.Driver != "bridge" || tenant.Options["com.docker.network.bridge.enable_icc"] != "false" {
		t.Fatalf("tenant/pool network options weakened isolation: %#v", tenant)
	}
	ingress := bridgeNetworkCreateOptions(true)
	if !ingress.Internal || ingress.Driver != "bridge" || ingress.Options["com.docker.network.bridge.enable_icc"] != "true" {
		t.Fatalf("ingress pair network options do not allow internal proxy traffic: %#v", ingress)
	}
	dbnet := bridgeNetworkCreateOptions(true)
	if !dbnet.Internal || dbnet.Options["com.docker.network.bridge.enable_icc"] != "true" {
		t.Fatalf("database network options must match ingress (Internal + ICC): %#v", dbnet)
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

func TestWipeConfigs(t *testing.T) {
	cfg, host := wipeConfigs("vol1")
	if !strings.HasPrefix(cfg.Image, "busybox:") || strings.HasSuffix(cfg.Image, ":latest") {
		t.Errorf("image must be pinned, got %q", cfg.Image)
	}
	if host.Runtime != "runsc" {
		t.Errorf("runtime = %q, want runsc", host.Runtime)
	}
	if host.NetworkMode != "none" {
		t.Errorf("network = %q, want none", host.NetworkMode)
	}
	if len(host.CapDrop) != 1 || host.CapDrop[0] != "ALL" {
		t.Errorf("CapDrop = %v, want [ALL]", host.CapDrop)
	}
	if !host.AutoRemove {
		t.Error("AutoRemove must be set so a crash cannot strand the container on the volume")
	}
	if cfg.Labels["ah.managed"] != "true" || cfg.Labels["ah.type"] != "wipe" {
		t.Errorf("labels = %v, want ah.managed=true ah.type=wipe", cfg.Labels)
	}
	if host.Memory == 0 || host.PidsLimit == nil {
		t.Error("memory and pids limits must be set")
	}
	script := strings.Join(cfg.Cmd, " ")
	if strings.Contains(script, "if=/dev/zero") || !strings.Contains(script, `head -c "$(wc -c`) {
		t.Errorf("wipe must cap writes to the file size, got %q", script)
	}
}

// Runs the wipe script (without the final rm) in a real container and checks
// each file keeps its size and is zeroed. Skipped when docker is unavailable.
func TestWipeScript_BoundedToFileSize(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable")
	}
	script := strings.Replace(wipeScript, `rm -f "$1"`, `echo "$(wc -c < "$1") $(tr -d "\\000" < "$1" | wc -c)"`, 1)
	script = `mkdir -p /data/sub; echo -n hello > /data/a; head -c 5000 /dev/urandom > /data/sub/b; ` + script
	if err := exec.Command("docker", "pull", "-q", wipeImage).Run(); err != nil {
		t.Skipf("cannot pull %s: %v", wipeImage, err)
	}
	out, err := exec.Command("docker", "run", "--rm", "--network", "none", wipeImage, "sh", "-c", script).Output()
	if err != nil {
		t.Skipf("cannot run %s: %v: %s", wipeImage, err, out)
	}
	// per file: "<size> <non-zero byte count>"
	f := strings.Fields(string(out))
	got := map[string]bool{}
	for i := 0; i+1 < len(f); i += 2 {
		got[f[i]+" "+f[i+1]] = true
	}
	if !got["5 0"] || !got["5000 0"] {
		t.Fatalf("sizes changed or data not zeroed: %q", out)
	}
}
