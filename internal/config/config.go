// Package config provides injectable configuration with environment variable overrides.
package config

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Config holds all configurable paths and addresses for the ah server.
type Config struct {
	DataDir               string // root data directory (default: /var/lib/ah)
	DBPath                string // state database path (default: {DataDir}/ah.db)
	MeterDBPath           string // metering database path (default: {DataDir}/meter.db)
	MasterKeyPath         string // master encryption key (default: {DataDir}/master.key)
	BuildDir              string // nixpacks build work directory (default: {DataDir}/builds)
	NixpacksPath          string // path to nixpacks binary (default: /usr/local/bin/nixpacks)
	DockerDataDir         string // Docker data root for disk checks (default: /var/lib/docker)
	BaseDomain            string // base domain for public service URLs (default: "" = localhost fallback)
	ExternalTLSTerminated bool   // public TLS terminates before the local proxy (default: false)
	TraefikConfigDir      string // Traefik file provider dynamic config directory (default: /etc/traefik/dynamic)
	KanbanPortStart       int    // inclusive start of kanban port range (default: 7100)
	KanbanPortEnd         int    // inclusive end of kanban port range (default: 9100)

	AdminToken string // AH_ADMIN_TOKEN: operator token for recover/reactivate/quota (comma-separated for rotation; empty = fall back to bootstrap tokens)

	// CORSOrigins is the browser Origin allowlist for the dashboard.
	// Empty AH_CORS_ORIGINS uses DefaultCORSOrigins. "*" is ignored.
	CORSOrigins []string

	// Snapshot retention
	SnapshotMaxPerService int           // max snapshots kept per service (default: 10, 0 = unlimited)
	SnapshotMaxAge        time.Duration // max age before a snapshot is eligible for cleanup (default: 30d, 0 = unlimited)

	// ContainerRuntime is the OCI runtime for tenant workloads (AH_CONTAINER_RUNTIME).
	// Default runsc (gVisor). RuntimeRunc is a dev-only fallback for hosts
	// that cannot run gVisor (macOS); cmd/ah refuses it without --dev.
	ContainerRuntime string

	// WarmPoolEnabled keeps pre-started environment containers (AH_WARM_POOL_ENABLED,
	// default true). Local sandboxes turn it off to avoid pulling every template image.
	WarmPoolEnabled bool

	// GCPruneDocker lets GC run daemon-wide dangling-image and anonymous-volume
	// prunes (AH_GC_PRUNE_DOCKER, default true). Set false when the Docker daemon
	// is shared with unrelated work, such as a developer laptop.
	GCPruneDocker bool

	// MaxSandboxesTotal caps non-failed disposable sandboxes across all
	// tenants on this host (AH_MAX_SANDBOXES_TOTAL, default 20, 0 = no host cap).
	MaxSandboxesTotal int

	// Sandbox hardening (phase 2). Egress and the sandbox runtime are off by
	// default (no egress, the container runtime); the tmpfs workspace is on
	// by default so a sandbox cannot fill the host disk.
	//
	// SandboxEgressNetwork (AH_SANDBOX_EGRESS_NETWORK) names the bridge that
	// sandboxes with egress "internet" attach to. Empty disables egress.
	SandboxEgressNetwork string
	// SandboxEgressSubnet (AH_SANDBOX_EGRESS_SUBNET) is that bridge's IPv4
	// subnet (default DefaultSandboxEgressSubnet).
	SandboxEgressSubnet string
	// SandboxEgressSkipFirewallCheck (AH_SANDBOX_EGRESS_SKIP_FIREWALL_CHECK)
	// enables egress without verifying the host firewall rules. For hosts
	// where the rules are enforced outside iptables; never the default.
	SandboxEgressSkipFirewallCheck bool
	// SandboxRuntime (AH_SANDBOX_RUNTIME) is the OCI runtime sandboxes run
	// under, e.g. runsc-sbx. Empty uses ContainerRuntime.
	SandboxRuntime string
	// SandboxWorkspaceTmpfs (AH_SANDBOX_WORKSPACE_TMPFS, default true) makes
	// each sandbox's /workspace a tmpfs of its disk_mb instead of a named
	// volume with no size limit. Set false to keep the volume.
	SandboxWorkspaceTmpfs bool
	// SandboxMaxConcurrentPulls (AH_SANDBOX_MAX_CONCURRENT_PULLS, default
	// DefaultSandboxMaxConcurrentPulls) caps sandbox image pulls in flight
	// across all tenants.
	SandboxMaxConcurrentPulls int
	// SandboxMaxImageMB (AH_SANDBOX_MAX_IMAGE_MB, default
	// DefaultSandboxMaxImageMB) is the largest image a sandbox create may
	// pull from a public registry; larger images are removed and refused.
	SandboxMaxImageMB int
}

// Container runtimes accepted in AH_CONTAINER_RUNTIME.
const (
	RuntimeRunsc = "runsc" // gVisor: the only runtime allowed in production
	RuntimeRunc  = "runc"  // dev-only fallback: no syscall sandbox
)

const defaultDataDir = "/var/lib/ah"

// Default kanban port range constants.
const (
	DefaultKanbanPortStart = 7100
	DefaultKanbanPortEnd   = 9100
)

// Snapshot retention defaults.
const (
	DefaultSnapshotMaxPerService = 10
	DefaultSnapshotMaxAge        = 30 * 24 * time.Hour // 30 days
)

// DefaultMaxSandboxesTotal is the default host-wide sandbox cap.
const DefaultMaxSandboxesTotal = 20

// Sandbox image pull defaults.
const (
	DefaultSandboxMaxConcurrentPulls = 2
	DefaultSandboxMaxImageMB         = 4096
)

// DefaultSandboxEgressSubnet is the default sandbox egress bridge subnet. It
// must match docker.DefaultSandboxEgressSubnet (checked by a cmd/ah test);
// config does not import docker.
const DefaultSandboxEgressSubnet = "10.250.0.0/24"

// runtimeNamePattern is a Docker runtime name as registered in daemon.json.
var runtimeNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

// Default returns a Config with all default values.
func Default() Config {
	return Config{
		DataDir:               defaultDataDir,
		DBPath:                filepath.Join(defaultDataDir, "ah.db"),
		MeterDBPath:           filepath.Join(defaultDataDir, "meter.db"),
		MasterKeyPath:         filepath.Join(defaultDataDir, "master.key"),
		BuildDir:              filepath.Join(defaultDataDir, "builds"),
		NixpacksPath:          "/usr/local/bin/nixpacks",
		DockerDataDir:         "/var/lib/docker",
		TraefikConfigDir:      "/etc/traefik/dynamic",
		KanbanPortStart:       DefaultKanbanPortStart,
		KanbanPortEnd:         DefaultKanbanPortEnd,
		SnapshotMaxPerService: DefaultSnapshotMaxPerService,
		SnapshotMaxAge:        DefaultSnapshotMaxAge,
		CORSOrigins:           DefaultCORSOrigins(),
		ContainerRuntime:      RuntimeRunsc,
		WarmPoolEnabled:       true,
		GCPruneDocker:         true,
		MaxSandboxesTotal:     DefaultMaxSandboxesTotal,
		SandboxEgressSubnet:   DefaultSandboxEgressSubnet,

		SandboxWorkspaceTmpfs:     true,
		SandboxMaxConcurrentPulls: DefaultSandboxMaxConcurrentPulls,
		SandboxMaxImageMB:         DefaultSandboxMaxImageMB,
	}
}

// FromEnv returns a Config populated from environment variables, falling back
// to defaults. Setting AH_DATA_DIR changes all derived paths unless individually
// overridden. Invalid numeric/duration values fall back to the default and are
// logged; use LoadFromEnv to treat them as fatal.
func FromEnv() Config {
	cfg, err := LoadFromEnv()
	if err != nil {
		log.Printf("WARNING: config: %v (using defaults for invalid values)", err)
	}
	return cfg
}

// LoadFromEnv is FromEnv but also returns every invalid setting it found
// (unparseable ints/durations, kanban start > end) instead of silently
// substituting defaults. A silently-defaulted retention value such as
// AH_SNAPSHOT_MAX_AGE=90d would otherwise make GC delete snapshots the
// operator meant to keep. The returned Config still holds defaults for the
// invalid values.
func LoadFromEnv() (Config, error) {
	dataDir := envOr("AH_DATA_DIR", defaultDataDir)
	var errs []error

	cfg := Config{
		DataDir:               dataDir,
		DBPath:                envOr("AH_DB_PATH", filepath.Join(dataDir, "ah.db")),
		MeterDBPath:           envOr("AH_METER_DB_PATH", filepath.Join(dataDir, "meter.db")),
		MasterKeyPath:         envOr("AH_MASTER_KEY_PATH", filepath.Join(dataDir, "master.key")),
		BuildDir:              envOr("AH_BUILD_DIR", filepath.Join(dataDir, "builds")),
		NixpacksPath:          envOr("AH_NIXPACKS_PATH", "/usr/local/bin/nixpacks"),
		DockerDataDir:         envOr("AH_DOCKER_DATA_DIR", "/var/lib/docker"),
		BaseDomain:            strings.TrimSpace(os.Getenv("AH_BASE_DOMAIN")),
		AdminToken:            strings.TrimSpace(os.Getenv("AH_ADMIN_TOKEN")),
		ExternalTLSTerminated: os.Getenv("AH_EXTERNAL_TLS_TERMINATED") == "true",
		TraefikConfigDir:      envOr("AH_TRAEFIK_CONFIG_DIR", "/etc/traefik/dynamic"),
		KanbanPortStart:       envOrInt("AH_KANBAN_PORT_START", DefaultKanbanPortStart, &errs),
		KanbanPortEnd:         envOrInt("AH_KANBAN_PORT_END", DefaultKanbanPortEnd, &errs),
		SnapshotMaxPerService: envOrInt("AH_SNAPSHOT_MAX_PER_SERVICE", DefaultSnapshotMaxPerService, &errs),
		SnapshotMaxAge:        envOrDuration("AH_SNAPSHOT_MAX_AGE", DefaultSnapshotMaxAge, &errs),
		CORSOrigins:           ParseCORSOrigins(os.Getenv("AH_CORS_ORIGINS")),
		ContainerRuntime:      strings.TrimSpace(envOr("AH_CONTAINER_RUNTIME", RuntimeRunsc)),
		WarmPoolEnabled:       envOrBool("AH_WARM_POOL_ENABLED", true, &errs),
		GCPruneDocker:         envOrBool("AH_GC_PRUNE_DOCKER", true, &errs),
		MaxSandboxesTotal:     envOrInt("AH_MAX_SANDBOXES_TOTAL", DefaultMaxSandboxesTotal, &errs),

		SandboxEgressNetwork:           strings.TrimSpace(os.Getenv("AH_SANDBOX_EGRESS_NETWORK")),
		SandboxEgressSubnet:            strings.TrimSpace(envOr("AH_SANDBOX_EGRESS_SUBNET", DefaultSandboxEgressSubnet)),
		SandboxEgressSkipFirewallCheck: envOrBool("AH_SANDBOX_EGRESS_SKIP_FIREWALL_CHECK", false, &errs),
		SandboxRuntime:                 strings.TrimSpace(os.Getenv("AH_SANDBOX_RUNTIME")),
		SandboxWorkspaceTmpfs:          envOrBool("AH_SANDBOX_WORKSPACE_TMPFS", true, &errs),
		SandboxMaxConcurrentPulls:      envOrInt("AH_SANDBOX_MAX_CONCURRENT_PULLS", DefaultSandboxMaxConcurrentPulls, &errs),
		SandboxMaxImageMB:              envOrInt("AH_SANDBOX_MAX_IMAGE_MB", DefaultSandboxMaxImageMB, &errs),
	}
	if cfg.SandboxMaxConcurrentPulls < 1 {
		errs = append(errs, fmt.Errorf("AH_SANDBOX_MAX_CONCURRENT_PULLS must be >= 1, got %d", cfg.SandboxMaxConcurrentPulls))
		cfg.SandboxMaxConcurrentPulls = DefaultSandboxMaxConcurrentPulls
	}
	if cfg.SandboxMaxImageMB < 1 {
		errs = append(errs, fmt.Errorf("AH_SANDBOX_MAX_IMAGE_MB must be >= 1, got %d", cfg.SandboxMaxImageMB))
		cfg.SandboxMaxImageMB = DefaultSandboxMaxImageMB
	}
	if cfg.SandboxRuntime != "" && !runtimeNamePattern.MatchString(cfg.SandboxRuntime) {
		errs = append(errs, fmt.Errorf("AH_SANDBOX_RUNTIME=%q is not a valid Docker runtime name", cfg.SandboxRuntime))
		cfg.SandboxRuntime = ""
	}
	if cfg.MaxSandboxesTotal < 0 {
		errs = append(errs, fmt.Errorf("AH_MAX_SANDBOXES_TOTAL must be >= 0 (0 = no host cap), got %d", cfg.MaxSandboxesTotal))
		cfg.MaxSandboxesTotal = DefaultMaxSandboxesTotal
	}
	if cfg.ContainerRuntime != RuntimeRunsc && cfg.ContainerRuntime != RuntimeRunc {
		errs = append(errs, fmt.Errorf("AH_CONTAINER_RUNTIME must be %q or %q, got %q", RuntimeRunsc, RuntimeRunc, cfg.ContainerRuntime))
		cfg.ContainerRuntime = RuntimeRunsc
	}
	if cfg.KanbanPortStart > cfg.KanbanPortEnd {
		errs = append(errs, fmt.Errorf("AH_KANBAN_PORT_START (%d) must not exceed AH_KANBAN_PORT_END (%d)", cfg.KanbanPortStart, cfg.KanbanPortEnd))
	}
	if cfg.SnapshotMaxPerService < 0 {
		errs = append(errs, fmt.Errorf("AH_SNAPSHOT_MAX_PER_SERVICE must be >= 0 (0 = unlimited), got %d", cfg.SnapshotMaxPerService))
	}
	if cfg.SnapshotMaxAge < 0 {
		errs = append(errs, fmt.Errorf("AH_SNAPSHOT_MAX_AGE must be >= 0 (0 = unlimited), got %s", cfg.SnapshotMaxAge))
	}
	return cfg, errors.Join(errs...)
}

// DefaultCORSOrigins is the public dashboard allowlist.
func DefaultCORSOrigins() []string {
	return []string{
		"https://agentic.hosting",
		"https://www.agentic.hosting",
		"https://dennisonbertram.github.io",
	}
}

// ParseCORSOrigins splits a comma-separated Origin allowlist. Empty input
// returns DefaultCORSOrigins. "*" is dropped (never allow-all), as is "none".
// Non-empty input that yields no origins (e.g. "none", "*", ",") returns a
// non-nil empty slice, which disables CORS; only nil means "use defaults".
func ParseCORSOrigins(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return DefaultCORSOrigins()
	}
	seen := make(map[string]struct{})
	out := []string{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" || part == "*" || strings.EqualFold(part, "none") {
			continue
		}
		if _, dup := seen[part]; dup {
			continue
		}
		seen[part] = struct{}{}
		out = append(out, part)
	}
	return out
}

// DiskCheckPaths returns the paths that should be checked for disk space.
func (c Config) DiskCheckPaths() []string {
	return []string{c.DataDir, c.DockerDataDir}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envOrInt(key string, fallback int, errs *[]error) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("invalid %s=%q: must be an integer", key, v))
		return fallback
	}
	return n
}

func envOrBool(key string, fallback bool, errs *[]error) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("invalid %s=%q: must be true or false", key, v))
		return fallback
	}
	return b
}

func envOrDuration(key string, fallback time.Duration, errs *[]error) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	d, err := ParseDuration(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("invalid %s=%q: %v", key, v, err))
		return fallback
	}
	return d
}

// ParseDuration is time.ParseDuration plus a whole-day suffix ("90d"), the
// natural unit for retention settings.
func ParseDuration(v string) (time.Duration, error) {
	if strings.HasSuffix(v, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(v, "d"))
		if err != nil {
			return 0, fmt.Errorf("must be a Go duration (e.g. 720h) or whole days (e.g. 30d)")
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("must be a Go duration (e.g. 720h) or whole days (e.g. 30d)")
	}
	return d, nil
}
