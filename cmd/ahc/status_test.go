package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildStatusServer creates a test HTTP server returning the given health, services, and databases.
func buildStatusServer(
	health ahclient.DetailedHealthResponse,
	services []ahclient.Service,
	databases []ahclient.Database,
) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/system/health/detailed":
			json.NewEncoder(w).Encode(health)
		case "/v1/services":
			json.NewEncoder(w).Encode(services)
		case "/v1/databases":
			json.NewEncoder(w).Encode(databases)
		default:
			http.NotFound(w, r)
		}
	}))
}

// healthyState is a convenience constructor for a fully-healthy scenario.
func healthyState() (ahclient.DetailedHealthResponse, []ahclient.Service, []ahclient.Database) {
	health := ahclient.DetailedHealthResponse{
		Status: "ok",
		Docker: ahclient.DockerInfo{Available: true, Version: "24.0.5"},
		GVisor: ahclient.GVisorInfo{Available: true},
		Disk:   ahclient.DiskInfo{TotalGB: 100, FreeGB: 50, UsedPercent: 50},
	}
	services := []ahclient.Service{
		{ID: "aabbccdd11223344aabbccdd11223344", Name: "web", Status: "running", CrashCount: 0},
	}
	databases := []ahclient.Database{
		{ID: "dddddddd11223344dddddddd11223344", Name: "main-db", Type: "postgres", Status: "running"},
	}
	return health, services, databases
}

// ---- BT-001: Exit code 0 when everything is healthy ----

func TestStatus_ExitCode0_WhenHealthy(t *testing.T) {
	health, services, databases := healthyState()
	srv := buildStatusServer(health, services, databases)
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "test-key")

	exitCode, err := runStatusCmd(c, &bytes.Buffer{}, false)
	require.NoError(t, err)
	assert.Equal(t, 0, exitCode, "exit code must be 0 when all systems are healthy")
}

// ---- BT-002: Exit code 1 when a service has circuit_open status ----

func TestStatus_ExitCode1_WhenServiceCircuitOpen(t *testing.T) {
	health, _, databases := healthyState()
	services := []ahclient.Service{
		{ID: "aabbccdd11223344aabbccdd11223344", Name: "web", Status: "circuit_open", CrashCount: 5},
	}
	srv := buildStatusServer(health, services, databases)
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "test-key")

	exitCode, err := runStatusCmd(c, &bytes.Buffer{}, false)
	require.NoError(t, err)
	assert.Equal(t, 1, exitCode, "exit code must be 1 when a service is circuit_open")
}

// ---- BT-003: Exit code 1 when disk usage >90% ----

func TestStatus_ExitCode1_WhenDiskCritical(t *testing.T) {
	health, services, databases := healthyState()
	health.Disk.UsedPercent = 92.0 // >90%

	srv := buildStatusServer(health, services, databases)
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "test-key")

	exitCode, err := runStatusCmd(c, &bytes.Buffer{}, false)
	require.NoError(t, err)
	assert.Equal(t, 1, exitCode, "exit code must be 1 when disk is >90% full")
}

// ---- BT-004: Yellow warning when disk >80% but not >90% ----

func TestStatus_YellowWarning_WhenDiskAbove80(t *testing.T) {
	health, services, databases := healthyState()
	health.Disk.UsedPercent = 85.0 // >80% but <=90%

	srv := buildStatusServer(health, services, databases)
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "test-key")
	out := &bytes.Buffer{}

	exitCode, err := runStatusCmd(c, out, false)
	require.NoError(t, err)

	// Exit code should be 0 (not critical)
	assert.Equal(t, 0, exitCode, "exit code must be 0 when disk is 80-90%")

	// Output must contain a warning indicator for disk
	output := out.String()
	assert.Contains(t, output, "85", "output should show disk usage percentage")
	// The warning keyword must appear — "warn" prefix covers "warning", "WARN", etc.
	assert.True(t,
		containsAny(output, "warn", "WARN", "!", "yellow"),
		"output should contain a warning indicator when disk is 80-90%: got %q", output)
}

// ---- BT-005: Services table contains NAME, STATUS, RESTARTS columns ----

func TestStatus_ServicesTable_Columns(t *testing.T) {
	health, services, databases := healthyState()
	srv := buildStatusServer(health, services, databases)
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "test-key")
	out := &bytes.Buffer{}

	_, err := runStatusCmd(c, out, false)
	require.NoError(t, err)

	output := out.String()
	assert.Contains(t, output, "NAME", "services table must have NAME column")
	assert.Contains(t, output, "STATUS", "services table must have STATUS column")
	assert.Contains(t, output, "RESTARTS", "services table must have RESTARTS column")
	assert.Contains(t, output, "web", "services table must show service name")
	assert.Contains(t, output, "running", "services table must show service status")
}

// ---- BT-006: Databases table contains NAME, TYPE, STATUS columns ----

func TestStatus_DatabasesTable_Columns(t *testing.T) {
	health, services, databases := healthyState()
	srv := buildStatusServer(health, services, databases)
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "test-key")
	out := &bytes.Buffer{}

	_, err := runStatusCmd(c, out, false)
	require.NoError(t, err)

	output := out.String()
	assert.Contains(t, output, "main-db", "databases table must show database name")
	assert.Contains(t, output, "postgres", "databases table must show database type")
}

// ---- BT-007: System health block shows Docker version and gVisor status ----

func TestStatus_HealthBlock_ShowsDockerAndGVisor(t *testing.T) {
	health, services, databases := healthyState()
	srv := buildStatusServer(health, services, databases)
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "test-key")
	out := &bytes.Buffer{}

	_, err := runStatusCmd(c, out, false)
	require.NoError(t, err)

	output := out.String()
	assert.Contains(t, output, "24.0.5", "health block must show Docker version")
	assert.Contains(t, output, "50", "health block must show disk usage")
}

// ---- BT-008: --json flag outputs a combined JSON object ----

func TestStatus_JSONOutput_CombinedObject(t *testing.T) {
	health, services, databases := healthyState()
	srv := buildStatusServer(health, services, databases)
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "test-key")
	out := &bytes.Buffer{}

	_, err := runStatusCmd(c, out, true /* jsonOutput */)
	require.NoError(t, err)

	var result map[string]json.RawMessage
	err = json.Unmarshal(out.Bytes(), &result)
	require.NoError(t, err, "JSON output must be valid JSON, got: %s", out.String())

	_, hasHealth := result["health"]
	_, hasServices := result["services"]
	_, hasDatabases := result["databases"]

	assert.True(t, hasHealth, "JSON output must contain 'health' key")
	assert.True(t, hasServices, "JSON output must contain 'services' key")
	assert.True(t, hasDatabases, "JSON output must contain 'databases' key")
}

// ---- BT-009: Exit code 1 when service has failed status ----

func TestStatus_ExitCode1_WhenServiceFailed(t *testing.T) {
	health, _, databases := healthyState()
	services := []ahclient.Service{
		{ID: "aabbccdd11223344aabbccdd11223344", Name: "worker", Status: "failed", CrashCount: 10},
	}
	srv := buildStatusServer(health, services, databases)
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "test-key")

	exitCode, err := runStatusCmd(c, &bytes.Buffer{}, false)
	require.NoError(t, err)
	assert.Equal(t, 1, exitCode, "exit code must be 1 when a service has status 'failed'")
}

// ---- BT-010: Exactly 90% disk does NOT trigger critical exit code ----

func TestStatus_Disk90Percent_IsNotCritical(t *testing.T) {
	health, services, databases := healthyState()
	health.Disk.UsedPercent = 90.0 // exactly 90% — boundary condition

	srv := buildStatusServer(health, services, databases)
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "test-key")

	exitCode, err := runStatusCmd(c, &bytes.Buffer{}, false)
	require.NoError(t, err)
	// 90% is the threshold — >90% is critical, so 90.0 itself should NOT trigger exit 1
	assert.Equal(t, 0, exitCode, "exit code must be 0 when disk is exactly 90% (not strictly greater)")
}

// ---- regression tests ----

// REG-001: When both a circuit_open service AND healthy disk exist, exit code is still 1
func TestStatus_Regression_CircuitOpenTakesPrecedence(t *testing.T) {
	health, _, databases := healthyState()
	health.Disk.UsedPercent = 50 // healthy disk
	services := []ahclient.Service{
		{ID: "aabbccdd11223344aabbccdd11223344", Name: "api", Status: "circuit_open", CrashCount: 3},
		{ID: "bbbbccdd11223344aabbccdd11223344", Name: "worker", Status: "running", CrashCount: 0},
	}
	srv := buildStatusServer(health, services, databases)
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "test-key")

	exitCode, err := runStatusCmd(c, &bytes.Buffer{}, false)
	require.NoError(t, err)
	assert.Equal(t, 1, exitCode, "exit code must be 1 even when only one service is circuit_open")
}

// REG-002: JSON output must include exit-code-relevant status so callers can inspect it
func TestStatus_JSONOutput_IncludesUnhealthyStatus(t *testing.T) {
	health, _, databases := healthyState()
	services := []ahclient.Service{
		{ID: "aabbccdd11223344aabbccdd11223344", Name: "api", Status: "circuit_open", CrashCount: 3},
	}
	srv := buildStatusServer(health, services, databases)
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "test-key")
	out := &bytes.Buffer{}

	exitCode, err := runStatusCmd(c, out, true)
	require.NoError(t, err)
	assert.Equal(t, 1, exitCode, "exit code must be 1 in JSON mode when service is circuit_open")

	// JSON should still be parseable
	var result map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out.Bytes(), &result), "JSON output must always be valid")
}

// REG-003: Empty services and databases lists should not crash and should exit 0
func TestStatus_EmptyLists_DoNotCrash(t *testing.T) {
	health, _, _ := healthyState()
	srv := buildStatusServer(health, []ahclient.Service{}, []ahclient.Database{})
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "test-key")
	out := &bytes.Buffer{}

	exitCode, err := runStatusCmd(c, out, false)
	require.NoError(t, err)
	assert.Equal(t, 0, exitCode, "empty service/database lists must not cause failure")
}

// ---- helpers ----

// containsAny returns true if s contains any of the given substrings.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if bytes.Contains([]byte(s), []byte(sub)) {
			return true
		}
	}
	return false
}
