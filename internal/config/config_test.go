package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefault(t *testing.T) {
	cfg := Default()

	assert.Equal(t, "/var/lib/ah", cfg.DataDir)
	assert.Equal(t, "/var/lib/ah/ah.db", cfg.DBPath)
	assert.Equal(t, "/var/lib/ah/meter.db", cfg.MeterDBPath)
	assert.Equal(t, "/var/lib/ah/master.key", cfg.MasterKeyPath)
	assert.Equal(t, "/var/lib/ah/builds", cfg.BuildDir)
	assert.Equal(t, "/usr/local/bin/nixpacks", cfg.NixpacksPath)
	assert.Equal(t, "/var/lib/docker", cfg.DockerDataDir)
	assert.Equal(t, 7100, cfg.KanbanPortStart)
	assert.Equal(t, 9100, cfg.KanbanPortEnd)
}

func TestFromEnv_Defaults(t *testing.T) {
	// Clear all env vars
	for _, k := range []string{
		"AH_DATA_DIR", "AH_DB_PATH", "AH_METER_DB_PATH",
		"AH_MASTER_KEY_PATH", "AH_BUILD_DIR", "AH_NIXPACKS_PATH",
		"AH_DOCKER_DATA_DIR", "AH_KANBAN_PORT_START", "AH_KANBAN_PORT_END",
		"AH_EXTERNAL_TLS_TERMINATED", "AH_CORS_ORIGINS",
	} {
		t.Setenv(k, "")
	}

	cfg := FromEnv()
	assert.Equal(t, Default(), cfg)
}

func TestFromEnv_ExternalTLSTerminated(t *testing.T) {
	t.Setenv("AH_EXTERNAL_TLS_TERMINATED", "true")
	assert.True(t, FromEnv().ExternalTLSTerminated)

	t.Setenv("AH_EXTERNAL_TLS_TERMINATED", "false")
	assert.False(t, FromEnv().ExternalTLSTerminated)

	for _, value := range []string{"", "TRUE", "1", "yes", "invalid"} {
		t.Setenv("AH_EXTERNAL_TLS_TERMINATED", value)
		assert.Falsef(t, FromEnv().ExternalTLSTerminated, "value %q must not enable external TLS mode", value)
	}
}

func TestFromEnv_DataDirDerivesPaths(t *testing.T) {
	t.Setenv("AH_DATA_DIR", "/opt/ah")
	// Clear individual overrides so they derive from AH_DATA_DIR
	for _, k := range []string{"AH_DB_PATH", "AH_METER_DB_PATH", "AH_MASTER_KEY_PATH", "AH_BUILD_DIR"} {
		t.Setenv(k, "")
	}

	cfg := FromEnv()

	assert.Equal(t, "/opt/ah", cfg.DataDir)
	assert.Equal(t, filepath.Join("/opt/ah", "ah.db"), cfg.DBPath)
	assert.Equal(t, filepath.Join("/opt/ah", "meter.db"), cfg.MeterDBPath)
	assert.Equal(t, filepath.Join("/opt/ah", "master.key"), cfg.MasterKeyPath)
	assert.Equal(t, filepath.Join("/opt/ah", "builds"), cfg.BuildDir)
}

func TestFromEnv_IndividualOverrides(t *testing.T) {
	t.Setenv("AH_DATA_DIR", "/opt/ah")
	t.Setenv("AH_DB_PATH", "/custom/path/state.db")
	t.Setenv("AH_MASTER_KEY_PATH", "/etc/ah/key.pem")
	t.Setenv("AH_METER_DB_PATH", "")
	t.Setenv("AH_BUILD_DIR", "")

	cfg := FromEnv()

	assert.Equal(t, "/opt/ah", cfg.DataDir)
	assert.Equal(t, "/custom/path/state.db", cfg.DBPath)
	assert.Equal(t, "/etc/ah/key.pem", cfg.MasterKeyPath)
	// These derive from AH_DATA_DIR
	assert.Equal(t, filepath.Join("/opt/ah", "meter.db"), cfg.MeterDBPath)
	assert.Equal(t, filepath.Join("/opt/ah", "builds"), cfg.BuildDir)
}

func TestFromEnv_AllOverrides(t *testing.T) {
	t.Setenv("AH_DATA_DIR", "/opt/ah")
	t.Setenv("AH_DB_PATH", "/a/db.sqlite")
	t.Setenv("AH_METER_DB_PATH", "/b/meter.sqlite")
	t.Setenv("AH_MASTER_KEY_PATH", "/c/master.key")
	t.Setenv("AH_BUILD_DIR", "/d/builds")
	t.Setenv("AH_NIXPACKS_PATH", "/e/nixpacks")
	t.Setenv("AH_DOCKER_DATA_DIR", "/mnt/docker")

	cfg := FromEnv()

	assert.Equal(t, "/opt/ah", cfg.DataDir)
	assert.Equal(t, "/a/db.sqlite", cfg.DBPath)
	assert.Equal(t, "/b/meter.sqlite", cfg.MeterDBPath)
	assert.Equal(t, "/c/master.key", cfg.MasterKeyPath)
	assert.Equal(t, "/d/builds", cfg.BuildDir)
	assert.Equal(t, "/e/nixpacks", cfg.NixpacksPath)
	assert.Equal(t, "/mnt/docker", cfg.DockerDataDir)
}

func TestDefaultCORSOriginsMatchMiddleware(t *testing.T) {
	assert.Equal(t, middleware.DefaultCORSOrigins, DefaultCORSOrigins())
}

func TestParseCORSOrigins_DefaultsAndAllowlist(t *testing.T) {
	assert.Equal(t, DefaultCORSOrigins(), ParseCORSOrigins(""))
	assert.Equal(t, DefaultCORSOrigins(), ParseCORSOrigins("   "))
	assert.Equal(t, []string{"https://dash.example.com"}, ParseCORSOrigins("https://dash.example.com"))
	assert.Equal(t, []string{"https://a.example", "https://b.example"}, ParseCORSOrigins(" https://a.example, https://b.example "))
	assert.Empty(t, ParseCORSOrigins("*"))
	assert.Equal(t, []string{"https://dash.example.com"}, ParseCORSOrigins("*,https://dash.example.com,*"))
}

func TestFromEnv_CORSOrigins(t *testing.T) {
	t.Setenv("AH_CORS_ORIGINS", "")
	assert.Equal(t, DefaultCORSOrigins(), FromEnv().CORSOrigins)

	t.Setenv("AH_CORS_ORIGINS", "https://dash.example.com")
	assert.Equal(t, []string{"https://dash.example.com"}, FromEnv().CORSOrigins)
}

func TestDiskCheckPaths(t *testing.T) {
	cfg := Default()
	paths := cfg.DiskCheckPaths()
	assert.Equal(t, []string{"/var/lib/ah", "/var/lib/docker"}, paths)

	cfg.DataDir = "/opt/ah"
	cfg.DockerDataDir = "/mnt/docker"
	paths = cfg.DiskCheckPaths()
	assert.Equal(t, []string{"/opt/ah", "/mnt/docker"}, paths)
}

func TestFromEnv_KanbanPortRangeOverride(t *testing.T) {
	t.Setenv("AH_KANBAN_PORT_START", "8000")
	t.Setenv("AH_KANBAN_PORT_END", "8500")

	cfg := FromEnv()
	assert.Equal(t, 8000, cfg.KanbanPortStart)
	assert.Equal(t, 8500, cfg.KanbanPortEnd)
}

func TestFromEnv_KanbanPortRangeInvalidFallsBack(t *testing.T) {
	t.Setenv("AH_KANBAN_PORT_START", "not-a-number")
	t.Setenv("AH_KANBAN_PORT_END", "also-bad")

	cfg := FromEnv()
	assert.Equal(t, DefaultKanbanPortStart, cfg.KanbanPortStart)
	assert.Equal(t, DefaultKanbanPortEnd, cfg.KanbanPortEnd)
}

func TestFromEnv_KanbanPortRangePartialOverride(t *testing.T) {
	// Override only start, end stays default
	t.Setenv("AH_KANBAN_PORT_START", "8000")
	os.Unsetenv("AH_KANBAN_PORT_END")

	cfg := FromEnv()
	assert.Equal(t, 8000, cfg.KanbanPortStart)
	assert.Equal(t, DefaultKanbanPortEnd, cfg.KanbanPortEnd)
}

func TestParseCORSOrigins_NoValidOriginsDisablesCORS(t *testing.T) {
	for _, raw := range []string{"*", ",", "none", " NONE ", "*,none"} {
		got := ParseCORSOrigins(raw)
		assert.NotNil(t, got, "%q must return a non-nil empty slice (nil means defaults)", raw)
		assert.Empty(t, got, raw)
	}
}

func TestLoadFromEnv_ReportsInvalidValues(t *testing.T) {
	t.Setenv("AH_SNAPSHOT_MAX_AGE", "ninety")
	t.Setenv("AH_SNAPSHOT_MAX_PER_SERVICE", "unlimited")
	_, err := LoadFromEnv()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AH_SNAPSHOT_MAX_AGE")
	assert.Contains(t, err.Error(), "AH_SNAPSHOT_MAX_PER_SERVICE")
}

func TestLoadFromEnv_AcceptsDaySuffix(t *testing.T) {
	t.Setenv("AH_SNAPSHOT_MAX_AGE", "90d")
	cfg, err := LoadFromEnv()
	require.NoError(t, err)
	assert.Equal(t, 90*24*time.Hour, cfg.SnapshotMaxAge)
}

func TestLoadFromEnv_RejectsInvertedKanbanRange(t *testing.T) {
	t.Setenv("AH_KANBAN_PORT_START", "9000")
	t.Setenv("AH_KANBAN_PORT_END", "8000")
	_, err := LoadFromEnv()
	require.Error(t, err)
}

func TestFromEnv_SnapshotRetention(t *testing.T) {
	t.Setenv("AH_SNAPSHOT_MAX_PER_SERVICE", "3")
	t.Setenv("AH_SNAPSHOT_MAX_AGE", "48h")
	cfg := FromEnv()
	assert.Equal(t, 3, cfg.SnapshotMaxPerService)
	assert.Equal(t, 48*time.Hour, cfg.SnapshotMaxAge)

	t.Setenv("AH_SNAPSHOT_MAX_AGE", "two days")
	assert.Equal(t, DefaultSnapshotMaxAge, FromEnv().SnapshotMaxAge, "invalid duration falls back to default")
}

func TestParseCORSOrigins_DedupesAndDropsWildcard(t *testing.T) {
	got := ParseCORSOrigins(" https://a.example , *, https://a.example,,https://b.example")
	assert.Equal(t, []string{"https://a.example", "https://b.example"}, got)
}
