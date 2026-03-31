package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- Helpers ----

// makeActivityServer returns a test HTTP server that records the last request URL
// and returns the given activity events as JSON.
func makeActivityServer(t *testing.T, events []map[string]interface{}) (*httptest.Server, *string) {
	t.Helper()
	var lastQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/activity" {
			lastQuery = r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(events)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &lastQuery
}

// ---- BT-ACT-001: --limit flag sets limit query parameter ----

func TestActivity_LimitFlag_SetsQueryParam(t *testing.T) {
	events := []map[string]interface{}{
		{
			"id":            "evt-001",
			"resource_type": "service",
			"resource_id":   "svc-abc",
			"resource_name": "my-service",
			"action":        "start",
			"status":        "success",
			"message":       "Service started",
			"created_at":    time.Now().Unix(),
		},
	}

	srv, lastQuery := makeActivityServer(t, events)

	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--url", srv.URL, "--key", "test-key", "activity", "--limit", "25"})

	err := cmd.Execute()
	require.NoError(t, err)

	assert.Contains(t, *lastQuery, "limit=25", "limit flag should be passed as query param")
}

// ---- BT-ACT-002: --since flag sets since query parameter with unix timestamp ----

func TestActivity_SinceFlag_SetsQueryParam(t *testing.T) {
	srv, lastQuery := makeActivityServer(t, []map[string]interface{}{})

	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	// Pass a valid RFC3339 timestamp
	cmd.SetArgs([]string{"--url", srv.URL, "--key", "test-key", "activity", "--since", "2026-01-01T00:00:00Z"})

	err := cmd.Execute()
	require.NoError(t, err)

	assert.Contains(t, *lastQuery, "since=", "since flag should be passed as query param with unix timestamp")
}

// ---- BT-ACT-003: --type flag sets resource_type query parameter ----

func TestActivity_TypeFlag_SetsQueryParam(t *testing.T) {
	srv, lastQuery := makeActivityServer(t, []map[string]interface{}{})

	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--url", srv.URL, "--key", "test-key", "activity", "--type", "service"})

	err := cmd.Execute()
	require.NoError(t, err)

	assert.Contains(t, *lastQuery, "resource_type=service", "--type flag should set resource_type query param")
}

// ---- BT-ACT-004: Table output contains expected columns ----

func TestActivity_TableOutput_ContainsExpectedColumns(t *testing.T) {
	ts := time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC)
	events := []map[string]interface{}{
		{
			"id":            "evt-001",
			"resource_type": "service",
			"resource_id":   "svc-abc",
			"resource_name": "my-service",
			"action":        "start",
			"status":        "success",
			"message":       "Service started successfully",
			"created_at":    ts.Unix(),
		},
	}

	srv, _ := makeActivityServer(t, events)

	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--url", srv.URL, "--key", "test-key", "activity"})

	err := cmd.Execute()
	require.NoError(t, err)

	output := out.String()
	// Must have TIME, TYPE, ACTION, RESOURCE, MESSAGE headers
	assert.Contains(t, output, "TIME", "output should have TIME column header")
	assert.Contains(t, output, "TYPE", "output should have TYPE column header")
	assert.Contains(t, output, "ACTION", "output should have ACTION column header")
	assert.Contains(t, output, "RESOURCE", "output should have RESOURCE column header")
	assert.Contains(t, output, "MESSAGE", "output should have MESSAGE column header")
}

// ---- BT-ACT-005: Table output contains event data ----

func TestActivity_TableOutput_ContainsEventData(t *testing.T) {
	ts := time.Date(2026, 3, 30, 12, 0, 0, 0, time.UTC)
	events := []map[string]interface{}{
		{
			"id":            "evt-001",
			"resource_type": "service",
			"resource_id":   "svc-abc",
			"resource_name": "my-svc",
			"action":        "deploy",
			"status":        "success",
			"message":       "Deployment completed",
			"created_at":    ts.Unix(),
		},
	}

	srv, _ := makeActivityServer(t, events)

	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--url", srv.URL, "--key", "test-key", "activity"})

	err := cmd.Execute()
	require.NoError(t, err)

	output := out.String()
	assert.Contains(t, output, "service", "output should contain resource type")
	assert.Contains(t, output, "deploy", "output should contain action")
	assert.Contains(t, output, "my-svc", "output should contain resource name")
	assert.Contains(t, output, "Deployment completed", "output should contain message")
}

// ---- BT-ACT-006: Long messages are truncated in table output ----

func TestActivity_LongMessage_IsTruncated(t *testing.T) {
	longMsg := strings.Repeat("x", 200)
	ts := time.Now()
	events := []map[string]interface{}{
		{
			"id":            "evt-001",
			"resource_type": "service",
			"resource_id":   "svc-abc",
			"resource_name": "my-svc",
			"action":        "deploy",
			"status":        "success",
			"message":       longMsg,
			"created_at":    ts.Unix(),
		},
	}

	srv, _ := makeActivityServer(t, events)

	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--url", srv.URL, "--key", "test-key", "activity"})

	err := cmd.Execute()
	require.NoError(t, err)

	output := out.String()
	// The full 200-char message should NOT appear verbatim — it must be truncated
	assert.NotContains(t, output, longMsg, "output should not contain full long message")
	// But output should contain a truncation indicator
	assert.Contains(t, output, "...", "output should contain truncation ellipsis for long messages")
}

// ---- BT-ACT-007: Empty result shows appropriate message ----

func TestActivity_EmptyResults_ShowsEmptyMessage(t *testing.T) {
	srv, _ := makeActivityServer(t, []map[string]interface{}{})

	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--url", srv.URL, "--key", "test-key", "activity"})

	err := cmd.Execute()
	require.NoError(t, err)

	output := out.String()
	assert.Contains(t, output, "No activity", "empty result should display a 'no activity' message")
}

// ---- BT-ACT-008: --json flag outputs valid JSON array ----

func TestActivity_JSONFlag_OutputsValidJSON(t *testing.T) {
	ts := time.Now()
	events := []map[string]interface{}{
		{
			"id":            "evt-001",
			"resource_type": "service",
			"resource_id":   "svc-abc",
			"resource_name": "my-svc",
			"action":        "start",
			"status":        "success",
			"message":       "Started",
			"created_at":    ts.Unix(),
		},
	}

	srv, _ := makeActivityServer(t, events)

	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--url", srv.URL, "--key", "test-key", "--json", "activity"})

	err := cmd.Execute()
	require.NoError(t, err)

	var result []map[string]interface{}
	err = json.Unmarshal(out.Bytes(), &result)
	require.NoError(t, err, "JSON output should be a valid JSON array")
	require.Len(t, result, 1, "JSON output should contain one event")
	assert.Equal(t, "service", result[0]["resource_type"])
}

// ---- BT-ACT-009: activity --help contains flag descriptions and examples ----

func TestActivity_Help_ContainsFlagsAndExamples(t *testing.T) {
	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{"activity", "--help"})

	_ = cmd.Execute()
	output := out.String()

	assert.Contains(t, output, "--limit", "help should mention --limit flag")
	assert.Contains(t, output, "--since", "help should mention --since flag")
	assert.Contains(t, output, "--type", "help should mention --type flag")
}

// ---- BT-ACT-010: invalid --since value returns error ----

func TestActivity_InvalidSince_ReturnsError(t *testing.T) {
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{"--url", "http://localhost:9999", "--key", "test", "activity", "--since", "not-a-date"})

	err := cmd.Execute()
	assert.Error(t, err, "invalid --since value should return an error")
}

// ---- BT-TEN-001: `ahc tenant` shows tenant info ----

func makeTenantServer(t *testing.T, tenant map[string]interface{}, usage map[string]interface{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/tenant":
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(tenant)
			} else if r.Method == http.MethodPatch {
				// Echo back merged tenant
				var update map[string]interface{}
				_ = json.NewDecoder(r.Body).Decode(&update)
				merged := make(map[string]interface{})
				for k, v := range tenant {
					merged[k] = v
				}
				for k, v := range update {
					if v != nil {
						merged[k] = v
					}
				}
				_ = json.NewEncoder(w).Encode(merged)
			} else {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
		case "/v1/tenant/usage":
			_ = json.NewEncoder(w).Encode(usage)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTenant_ShowsInfo(t *testing.T) {
	tenant := map[string]interface{}{
		"id":         "ten-abc",
		"name":       "Acme Corp",
		"email":      "admin@acme.com",
		"status":     "active",
		"created_at": time.Now().Unix(),
		"updated_at": time.Now().Unix(),
	}
	usage := map[string]interface{}{
		"services":   map[string]interface{}{"used": 3, "max": 10},
		"databases":  map[string]interface{}{"used": 1, "max": 5},
		"api_keys":   map[string]interface{}{"used": 2, "max": 20},
		"memory_mb":  2048,
		"cpu_cores":  2.0,
		"disk_gb":    20,
		"rate_limit": 100,
	}
	srv := makeTenantServer(t, tenant, usage)

	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--url", srv.URL, "--key", "test-key", "tenant"})

	err := cmd.Execute()
	require.NoError(t, err)

	output := out.String()
	assert.Contains(t, output, "Acme Corp", "output should show tenant name")
	assert.Contains(t, output, "admin@acme.com", "output should show tenant email")
	assert.Contains(t, output, "active", "output should show tenant status")
}

// ---- BT-TEN-002: `ahc tenant` shows quota info from usage endpoint ----

func TestTenant_ShowsQuotas(t *testing.T) {
	tenant := map[string]interface{}{
		"id":         "ten-abc",
		"name":       "Acme Corp",
		"email":      "admin@acme.com",
		"status":     "active",
		"created_at": time.Now().Unix(),
		"updated_at": time.Now().Unix(),
	}
	usage := map[string]interface{}{
		"services":   map[string]interface{}{"used": 3, "max": 10},
		"databases":  map[string]interface{}{"used": 1, "max": 5},
		"api_keys":   map[string]interface{}{"used": 2, "max": 20},
		"memory_mb":  2048,
		"cpu_cores":  2.0,
		"disk_gb":    20,
		"rate_limit": 100,
	}
	srv := makeTenantServer(t, tenant, usage)

	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--url", srv.URL, "--key", "test-key", "tenant"})

	err := cmd.Execute()
	require.NoError(t, err)

	output := out.String()
	// Should show services quota
	assert.Contains(t, output, "3", "output should show services used count")
	assert.Contains(t, output, "10", "output should show services max count")
}

// ---- BT-TEN-003: `ahc tenant update --name` updates tenant name ----

func TestTenantUpdate_Name_SendsPATCH(t *testing.T) {
	var capturedBody []byte
	tenant := map[string]interface{}{
		"id":         "ten-abc",
		"name":       "New Name",
		"email":      "admin@acme.com",
		"status":     "active",
		"created_at": time.Now().Unix(),
		"updated_at": time.Now().Unix(),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/tenant" && r.Method == http.MethodPatch {
			capturedBody, _ = io.ReadAll(r.Body)
			_ = json.NewEncoder(w).Encode(tenant)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--url", srv.URL, "--key", "test-key", "tenant", "update", "--name", "New Name"})

	err := cmd.Execute()
	require.NoError(t, err)

	// The PATCH body must contain the name
	assert.Contains(t, string(capturedBody), "New Name", "PATCH body should contain the new name")
	// Output should confirm the update
	assert.Contains(t, out.String(), "New Name", "output should show updated tenant name")
}

// ---- BT-TEN-004: `ahc tenant update --email` is not supported (field not in UpdateTenantRequest) ----
// The UpdateTenantRequest in types.go only has Name. Email is not updateable.
// So --email flag should either not exist or return an error about unsupported field.

func TestTenantUpdate_NoFlags_ShowsGuidance(t *testing.T) {
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{"--url", "http://localhost:9999", "--key", "test", "tenant", "update"})

	err := cmd.Execute()
	// Should error since no flags were provided for update
	assert.Error(t, err, "tenant update with no flags should return error")
}

// ---- BT-TEN-005: `ahc tenant --json` outputs valid JSON ----

func TestTenant_JSONFlag_OutputsValidJSON(t *testing.T) {
	tenant := map[string]interface{}{
		"id":         "ten-abc",
		"name":       "Acme Corp",
		"email":      "admin@acme.com",
		"status":     "active",
		"created_at": time.Now().Unix(),
		"updated_at": time.Now().Unix(),
	}
	usage := map[string]interface{}{
		"services":   map[string]interface{}{"used": 3, "max": 10},
		"databases":  map[string]interface{}{"used": 1, "max": 5},
		"api_keys":   map[string]interface{}{"used": 2, "max": 20},
		"memory_mb":  2048,
		"cpu_cores":  2.0,
		"disk_gb":    20,
		"rate_limit": 100,
	}
	srv := makeTenantServer(t, tenant, usage)

	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--url", srv.URL, "--key", "test-key", "--json", "tenant"})

	err := cmd.Execute()
	require.NoError(t, err)

	var result map[string]interface{}
	err = json.Unmarshal(out.Bytes(), &result)
	require.NoError(t, err, "JSON output should be valid JSON object")
	assert.Equal(t, "Acme Corp", result["name"], "JSON should include tenant name")
}

// ---- BT-TEN-006: `ahc tenant` help contains examples ----

func TestTenant_Help_ContainsExamples(t *testing.T) {
	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{"tenant", "--help"})

	_ = cmd.Execute()
	output := out.String()

	assert.Contains(t, output, "ahc tenant", "help should show usage example with 'ahc tenant'")
}

func TestTenantUpdate_Help_ContainsFlags(t *testing.T) {
	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{"tenant", "update", "--help"})

	_ = cmd.Execute()
	output := out.String()

	assert.Contains(t, output, "--name", "tenant update help should show --name flag")
}

// ---- Regression tests ----

// REG-ACT-001: activity command is not a placeholder — it makes a real API call
func TestActivity_IsNotPlaceholder(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/activity" {
			callCount++
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, "[]")
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{"--url", srv.URL, "--key", "test-key", "activity"})

	err := cmd.Execute()
	require.NoError(t, err)
	assert.Equal(t, 1, callCount, "activity command should call /v1/activity exactly once")
}

// REG-TEN-001: tenant command is not a placeholder — it makes a real API call
func TestTenant_IsNotPlaceholder(t *testing.T) {
	callCount := 0
	tenant := map[string]interface{}{
		"id": "ten-abc", "name": "Test", "email": "a@b.com",
		"status": "active", "created_at": int64(0), "updated_at": int64(0),
	}
	usage := map[string]interface{}{
		"services":   map[string]interface{}{"used": 0, "max": 10},
		"databases":  map[string]interface{}{"used": 0, "max": 5},
		"api_keys":   map[string]interface{}{"used": 0, "max": 20},
		"memory_mb": 0, "cpu_cores": 0.0, "disk_gb": 0, "rate_limit": 100,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/tenant":
			callCount++
			_ = json.NewEncoder(w).Encode(tenant)
		case "/v1/tenant/usage":
			callCount++
			_ = json.NewEncoder(w).Encode(usage)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--url", srv.URL, "--key", "test-key", "tenant"})

	err := cmd.Execute()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, callCount, 1, "tenant command should call tenant API endpoints")
}

// REG-ACT-002: --limit=0 does not send limit query param (zero is the zero value — no filter)
func TestActivity_LimitZero_DoesNotSetParam(t *testing.T) {
	srv, lastQuery := makeActivityServer(t, []map[string]interface{}{})

	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--url", srv.URL, "--key", "test-key", "activity"})

	err := cmd.Execute()
	require.NoError(t, err)

	// With no --limit flag, limit= should not be in query (or not with a value)
	assert.NotContains(t, *lastQuery, "limit=0", "limit=0 should not be sent as a query param when unset")
}
