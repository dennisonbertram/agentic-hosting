package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- BT-001: When `ahc` is run with no args, it prints help text listing all command groups ----

func TestRootHelp_ContainsCommandGroups(t *testing.T) {
	// Execute root command with no arguments, capture output
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{})

	// Running with no args should print usage (cobra default behavior)
	_ = cmd.Execute()

	combined := out.String() + errOut.String()

	// Must contain these command groups
	assert.Contains(t, combined, "configure", "help should mention configure command")
	assert.Contains(t, combined, "version", "help should mention version command")
	assert.Contains(t, combined, "service", "help should mention service command group")
	assert.Contains(t, combined, "deploy", "help should mention deploy command")
	assert.Contains(t, combined, "env", "help should mention env command group")
	assert.Contains(t, combined, "db", "help should mention db command group")
	assert.Contains(t, combined, "ahc", "help should mention the program name")
}

func TestRootHelp_ContainsFlagDescriptions(t *testing.T) {
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{"--help"})

	_ = cmd.Execute()

	combined := out.String() + errOut.String()

	assert.Contains(t, combined, "--url", "help should show --url flag")
	assert.Contains(t, combined, "--key", "help should show --key flag")
	assert.Contains(t, combined, "--json", "help should show --json flag")
}

// ---- BT-002: When `ahc configure --url X --key Y` is run, config file is created ----

func TestConfigure_CreatesConfigFile(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")

	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{"configure", "--url", "https://api.example.com", "--key", "test-api-key-12345", "--config-path", cfgPath})

	err := cmd.Execute()
	require.NoError(t, err, "configure should not return an error")

	// File must exist
	_, err = os.Stat(cfgPath)
	require.NoError(t, err, "config file should be created")

	// File must contain the correct values
	data, err := os.ReadFile(cfgPath)
	require.NoError(t, err)

	var cfg map[string]string
	err = json.Unmarshal(data, &cfg)
	require.NoError(t, err, "config file should contain valid JSON")

	assert.Equal(t, "https://api.example.com", cfg["url"], "url should be saved")
	assert.Equal(t, "test-api-key-12345", cfg["key"], "key should be saved")
}

func TestConfigure_PrintsConfirmation(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")

	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{"configure", "--url", "https://api.example.com", "--key", "test-api-key-12345", "--config-path", cfgPath})

	err := cmd.Execute()
	require.NoError(t, err)

	assert.Contains(t, out.String(), "Configuration saved", "should print confirmation message")
}

func TestConfigure_FilePermissions(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")

	cmd := newRootCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"configure", "--url", "https://api.example.com", "--key", "test-key", "--config-path", cfgPath})

	err := cmd.Execute()
	require.NoError(t, err)

	info, err := os.Stat(cfgPath)
	require.NoError(t, err)

	perm := info.Mode().Perm()
	assert.Equal(t, os.FileMode(0600), perm, "config file should have 0600 permissions")
}

// ---- BT-003: When `ahc version` is run, it prints version info ----

func TestVersion_PrintsVersionInfo(t *testing.T) {
	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"version"})

	err := cmd.Execute()
	require.NoError(t, err)

	output := out.String()
	assert.Contains(t, output, "ahc", "version output should contain program name")
	assert.Contains(t, output, "version", "version output should contain 'version' word")
}

func TestVersion_DefaultsToDevWhenNotSet(t *testing.T) {
	// Save and reset version vars
	origVersion := Version
	origCommit := Commit
	origDate := Date
	Version = ""
	Commit = ""
	Date = ""
	defer func() {
		Version = origVersion
		Commit = origCommit
		Date = origDate
	}()

	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"version"})

	err := cmd.Execute()
	require.NoError(t, err)

	output := out.String()
	assert.Contains(t, output, "dev", "version output should default to 'dev' when Version is empty")
}

func TestVersion_ShowsCommitAndDate(t *testing.T) {
	origVersion := Version
	origCommit := Commit
	origDate := Date
	Version = "1.2.3"
	Commit = "abc1234"
	Date = "2026-03-31"
	defer func() {
		Version = origVersion
		Commit = origCommit
		Date = origDate
	}()

	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"version"})

	err := cmd.Execute()
	require.NoError(t, err)

	output := out.String()
	assert.Contains(t, output, "1.2.3", "version output should contain version number")
	assert.Contains(t, output, "abc1234", "version output should contain commit hash")
	assert.Contains(t, output, "2026-03-31", "version output should contain build date")
}

// ---- BT-004: When --json is passed, output functions use JSON format ----

func TestPrintJSON_OutputsValidJSON(t *testing.T) {
	out := &bytes.Buffer{}
	err := printJSON(out, map[string]string{"status": "ok", "name": "test"})
	require.NoError(t, err)

	var parsed map[string]string
	err = json.Unmarshal(out.Bytes(), &parsed)
	require.NoError(t, err, "printJSON should output valid JSON")
	assert.Equal(t, "ok", parsed["status"])
	assert.Equal(t, "test", parsed["name"])
}

func TestPrintTable_OutputsAlignedColumns(t *testing.T) {
	out := &bytes.Buffer{}
	headers := []string{"NAME", "STATUS", "URL"}
	rows := [][]string{
		{"my-service", "running", "https://example.com"},
		{"other", "stopped", "https://other.com"},
	}
	printTable(out, headers, rows)

	output := out.String()
	assert.Contains(t, output, "NAME", "table should contain header")
	assert.Contains(t, output, "STATUS", "table should contain header")
	assert.Contains(t, output, "my-service", "table should contain row data")
	assert.Contains(t, output, "running", "table should contain row data")
}

func TestPrintSuccess_ContainsMessage(t *testing.T) {
	out := &bytes.Buffer{}
	printSuccess(out, "Operation completed successfully")
	assert.Contains(t, out.String(), "Operation completed successfully")
}

func TestPrintError_ContainsMessage(t *testing.T) {
	out := &bytes.Buffer{}
	printError(out, "Something went wrong")
	assert.Contains(t, out.String(), "Something went wrong")
}

func TestPrintWarning_ContainsMessage(t *testing.T) {
	out := &bytes.Buffer{}
	printWarning(out, "This is a warning")
	assert.Contains(t, out.String(), "This is a warning")
}

// ---- Regression tests ----

// REG-001: configure with no flags shows current config or usage guidance, not an error
func TestConfigure_NoFlags_ShowsGuidance(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "nonexistent.json")

	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{"configure", "--config-path", cfgPath})

	// Should not error — should show guidance
	err := cmd.Execute()
	require.NoError(t, err)

	output := out.String()
	// Should tell the user how to use flags
	assert.True(t,
		strings.Contains(output, "--url") || strings.Contains(output, "not configured") || strings.Contains(output, "No configuration"),
		"should show guidance when no flags provided, got: %s", output)
}

// REG-002: version command still works when cobra flags are passed
func TestVersion_WithGlobalFlags(t *testing.T) {
	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--json", "version"})

	err := cmd.Execute()
	require.NoError(t, err)

	// With --json, version should output JSON
	output := out.String()
	assert.NotEmpty(t, output, "version with --json should still produce output")
}

// REG-003: printJSON returns error for non-marshallable values, not a panic
func TestPrintJSON_NonMarshalableValue_ReturnsError(t *testing.T) {
	out := &bytes.Buffer{}
	// channels cannot be marshaled to JSON
	ch := make(chan int)
	err := printJSON(out, ch)
	assert.Error(t, err, "printJSON should return error for non-marshallable values")
}

// REG-004: configure creates parent directories if they don't exist
func TestConfigure_CreatesParentDirs(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "nested", "dir", "config.json")

	cmd := newRootCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"configure", "--url", "https://api.example.com", "--key", "test-key", "--config-path", cfgPath})

	err := cmd.Execute()
	require.NoError(t, err)

	_, statErr := os.Stat(cfgPath)
	require.NoError(t, statErr, "config file should be created even when parent dirs don't exist")
}
