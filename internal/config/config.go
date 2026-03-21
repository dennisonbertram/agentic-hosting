// Package config provides injectable configuration with environment variable overrides.
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds all configurable paths and addresses for the ah server.
type Config struct {
	DataDir          string // root data directory (default: /var/lib/ah)
	DBPath           string // state database path (default: {DataDir}/ah.db)
	MeterDBPath      string // metering database path (default: {DataDir}/meter.db)
	MasterKeyPath    string // master encryption key (default: {DataDir}/master.key)
	BuildDir         string // nixpacks build work directory (default: {DataDir}/builds)
	NixpacksPath     string // path to nixpacks binary (default: /usr/local/bin/nixpacks)
	DockerDataDir    string // Docker data root for disk checks (default: /var/lib/docker)
	BaseDomain       string // base domain for public service URLs (default: "" = localhost fallback)
	TraefikConfigDir string // Traefik file provider dynamic config directory (default: /etc/traefik/dynamic)

	// Snapshot retention
	SnapshotMaxPerService int           // max snapshots kept per service (default: 10, 0 = unlimited)
	SnapshotMaxAge        time.Duration // max age before a snapshot is eligible for cleanup (default: 30d, 0 = unlimited)
}

const defaultDataDir = "/var/lib/ah"

// Snapshot retention defaults.
const (
	DefaultSnapshotMaxPerService = 10
	DefaultSnapshotMaxAge        = 30 * 24 * time.Hour // 30 days
)

// Default returns a Config with all default values.
func Default() Config {
	return Config{
		DataDir:          defaultDataDir,
		DBPath:           filepath.Join(defaultDataDir, "ah.db"),
		MeterDBPath:      filepath.Join(defaultDataDir, "meter.db"),
		MasterKeyPath:    filepath.Join(defaultDataDir, "master.key"),
		BuildDir:         filepath.Join(defaultDataDir, "builds"),
		NixpacksPath:     "/usr/local/bin/nixpacks",
		DockerDataDir:         "/var/lib/docker",
		TraefikConfigDir:      "/etc/traefik/dynamic",
		SnapshotMaxPerService: DefaultSnapshotMaxPerService,
		SnapshotMaxAge:        DefaultSnapshotMaxAge,
	}
}

// FromEnv returns a Config populated from environment variables, falling back
// to defaults. Setting AH_DATA_DIR changes all derived paths unless individually
// overridden.
func FromEnv() Config {
	dataDir := envOr("AH_DATA_DIR", defaultDataDir)

	return Config{
		DataDir:               dataDir,
		DBPath:                envOr("AH_DB_PATH", filepath.Join(dataDir, "ah.db")),
		MeterDBPath:           envOr("AH_METER_DB_PATH", filepath.Join(dataDir, "meter.db")),
		MasterKeyPath:         envOr("AH_MASTER_KEY_PATH", filepath.Join(dataDir, "master.key")),
		BuildDir:              envOr("AH_BUILD_DIR", filepath.Join(dataDir, "builds")),
		NixpacksPath:          envOr("AH_NIXPACKS_PATH", "/usr/local/bin/nixpacks"),
		DockerDataDir:         envOr("AH_DOCKER_DATA_DIR", "/var/lib/docker"),
		BaseDomain:            strings.TrimSpace(os.Getenv("AH_BASE_DOMAIN")),
		TraefikConfigDir:      envOr("AH_TRAEFIK_CONFIG_DIR", "/etc/traefik/dynamic"),
		SnapshotMaxPerService: envOrInt("AH_SNAPSHOT_MAX_PER_SERVICE", DefaultSnapshotMaxPerService),
		SnapshotMaxAge:        envOrDuration("AH_SNAPSHOT_MAX_AGE", DefaultSnapshotMaxAge),
	}
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

func envOrInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func envOrDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}
