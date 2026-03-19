package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
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
}

func TestFromEnv_Defaults(t *testing.T) {
	// Clear all env vars
	for _, k := range []string{
		"AH_DATA_DIR", "AH_DB_PATH", "AH_METER_DB_PATH",
		"AH_MASTER_KEY_PATH", "AH_BUILD_DIR", "AH_NIXPACKS_PATH",
		"AH_DOCKER_DATA_DIR",
	} {
		t.Setenv(k, "")
	}

	cfg := FromEnv()
	assert.Equal(t, Default(), cfg)
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

func TestDiskCheckPaths(t *testing.T) {
	cfg := Default()
	paths := cfg.DiskCheckPaths()
	assert.Equal(t, []string{"/var/lib/ah", "/var/lib/docker"}, paths)

	cfg.DataDir = "/opt/ah"
	cfg.DockerDataDir = "/mnt/docker"
	paths = cfg.DiskCheckPaths()
	assert.Equal(t, []string{"/opt/ah", "/mnt/docker"}, paths)
}
