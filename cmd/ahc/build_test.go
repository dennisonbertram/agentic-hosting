package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- helpers ----

func makeBuild(id, status, gitURL, branch string, createdAt int64) ahclient.Build {
	return ahclient.Build{
		ID:        id,
		ServiceID: "aabbccdd11223344aabbccdd11223344",
		Status:    status,
		SourceURL: gitURL,
		SourceRef: branch,
		CreatedAt: createdAt,
	}
}

// ---- BT-BUILD-001: build list with no builds returns "No builds found" ----

func TestBuildList_NoBuilds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/builds") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]ahclient.Build{})
			return
		}
		if r.URL.Path == "/v1/services" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]ahclient.Service{
				{ID: "aabbccdd11223344aabbccdd11223344", Name: "my-svc"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := ahclient.NewClient(srv.URL, "test-key")

	var buf bytes.Buffer
	err := runBuildList(c, "my-svc", false, &buf)
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "No builds found", "empty build list must say 'No builds found'")
}

// ---- BT-BUILD-002: build list shows table columns ----

func TestBuildList_TableColumns(t *testing.T) {
	now := time.Now().Unix()
	builds := []ahclient.Build{
		makeBuild("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "succeeded", "https://github.com/org/repo", "main", now),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/builds") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(builds)
			return
		}
		if r.URL.Path == "/v1/services" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]ahclient.Service{
				{ID: "aabbccdd11223344aabbccdd11223344", Name: "my-svc"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := ahclient.NewClient(srv.URL, "test-key")

	var buf bytes.Buffer
	err := runBuildList(c, "my-svc", false, &buf)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "ID", "table must have ID column")
	assert.Contains(t, output, "STATUS", "table must have STATUS column")
	assert.Contains(t, output, "GIT_URL", "table must have GIT_URL column")
	assert.Contains(t, output, "BRANCH", "table must have BRANCH column")
	assert.Contains(t, output, "CREATED", "table must have CREATED column")
}

// ---- BT-BUILD-003: build list shows truncated ID ----

func TestBuildList_TruncatedID(t *testing.T) {
	now := time.Now().Unix()
	builds := []ahclient.Build{
		makeBuild("cccccccccccccccccccccccccccccccc", "building", "https://github.com/org/repo", "main", now),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/builds") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(builds)
			return
		}
		if r.URL.Path == "/v1/services" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]ahclient.Service{
				{ID: "aabbccdd11223344aabbccdd11223344", Name: "my-svc"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := ahclient.NewClient(srv.URL, "test-key")

	var buf bytes.Buffer
	err := runBuildList(c, "my-svc", false, &buf)
	require.NoError(t, err)
	output := buf.String()

	// Full 32-char ID should NOT appear; truncated (8-char) prefix should.
	assert.NotContains(t, output, "cccccccccccccccccccccccccccccccc",
		"full 32-char build ID must not appear in table output")
	assert.Contains(t, output, "cccccccc", "truncated build ID (first 8 chars) must appear")
}

// ---- BT-BUILD-004: latest build selection uses first build from ordered list ----

func TestBuildLatest_UsesFirstFromList(t *testing.T) {
	// The API returns builds newest-first. latestBuildID must return builds[0].ID.
	builds := []ahclient.Build{
		makeBuild("11111111111111111111111111111111", "succeeded", "", "main", 2000),
		makeBuild("22222222222222222222222222222222", "failed", "", "main", 1000),
	}

	id, err := latestBuildID(builds)
	require.NoError(t, err)
	assert.Equal(t, "11111111111111111111111111111111", id,
		"latestBuildID must return the first build in the list (newest)")
}

// ---- BT-BUILD-005: latestBuildID returns error when list is empty ----

func TestBuildLatest_EmptyListReturnsError(t *testing.T) {
	_, err := latestBuildID([]ahclient.Build{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no builds", "error must mention 'no builds'")
}

// ---- BT-BUILD-006: status color mapping ----

// enableColorsForTest forces colorsEnabled=true for the duration of the test.
func enableColorsForTest(t *testing.T) {
	t.Helper()
	orig := colorsEnabled
	colorsEnabled = true
	t.Cleanup(func() { colorsEnabled = orig })
}

func TestBuildStatusColor_Succeeded(t *testing.T) {
	enableColorsForTest(t)
	// succeeded → green ANSI sequence
	out := coloredBuildStatus("succeeded")
	assert.Contains(t, out, "succeeded", "colored status must contain the status text")
	assert.Contains(t, out, colorGreen, "succeeded must use green color code")
}

func TestBuildStatusColor_Failed(t *testing.T) {
	enableColorsForTest(t)
	out := coloredBuildStatus("failed")
	assert.Contains(t, out, "failed", "colored status must contain the status text")
	assert.Contains(t, out, colorRed, "failed must use red color code")
}

func TestBuildStatusColor_Building(t *testing.T) {
	enableColorsForTest(t)
	out := coloredBuildStatus("building")
	assert.Contains(t, out, "building", "colored status must contain the status text")
	assert.Contains(t, out, colorYellow, "building must use yellow color code")
}

func TestBuildStatusColor_Unknown(t *testing.T) {
	// unknown status must not panic and must return the text unchanged (no color wrapping with wrong code)
	// We do NOT enable colors here — we want the default/passthrough behavior.
	out := coloredBuildStatus("queued")
	assert.Contains(t, out, "queued", "unknown status must still contain the status text")
}

// ---- BT-BUILD-007: build list --all uses tenant-wide endpoint ----

func TestBuildList_All_UsesTenantWideEndpoint(t *testing.T) {
	tenantEndpointCalled := false
	serviceEndpointCalled := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/builds" {
			tenantEndpointCalled = true
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]ahclient.Build{})
			return
		}
		if strings.Contains(r.URL.Path, "/services/") && strings.HasSuffix(r.URL.Path, "/builds") {
			serviceEndpointCalled = true
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]ahclient.Build{})
			return
		}
		if r.URL.Path == "/v1/services" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]ahclient.Service{
				{ID: "aabbccdd11223344aabbccdd11223344", Name: "my-svc"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := ahclient.NewClient(srv.URL, "test-key")

	var buf bytes.Buffer
	err := runBuildList(c, "", true /* allFlag */, &buf)
	require.NoError(t, err)
	assert.True(t, tenantEndpointCalled, "--all must call tenant-wide /v1/builds endpoint")
	assert.False(t, serviceEndpointCalled, "--all must NOT call per-service builds endpoint")
}

// ---- BT-BUILD-008: build cancel sends DELETE to correct endpoint ----

func TestBuildCancel_CallsCorrectEndpoint(t *testing.T) {
	cancelCalled := false
	cancelPath := ""

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/builds/") {
			cancelCalled = true
			cancelPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ahclient.StatusResponse{Status: "cancelled"})
			return
		}
		if r.URL.Path == "/v1/services" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]ahclient.Service{
				{ID: "aabbccdd11223344aabbccdd11223344", Name: "my-svc"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := ahclient.NewClient(srv.URL, "test-key")

	err := runBuildCancel(c, "my-svc", "dddddddddddddddddddddddddddddddd")
	require.NoError(t, err)
	assert.True(t, cancelCalled, "cancel must send a DELETE request")
	assert.Contains(t, cancelPath, "dddddddddddddddddddddddddddddddd", "cancel must include build ID in path")
}

// ---- BT-LOGS-001: logs command streams runtime logs from service ----

func TestLogsCmd_StreamsFromServiceEndpoint(t *testing.T) {
	logsCalled := false
	var logsPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/logs") {
			logsCalled = true
			logsPath = r.URL.Path
			w.Header().Set("Content-Type", "text/plain")
			io.WriteString(w, "2026-03-30 app started\n")
			return
		}
		if r.URL.Path == "/v1/services" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]ahclient.Service{
				{ID: "aabbccdd11223344aabbccdd11223344", Name: "my-svc"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := ahclient.NewClient(srv.URL, "test-key")

	var buf bytes.Buffer
	err := runServiceRuntimeLogs(c, "my-svc", 100, false, &buf)
	require.NoError(t, err)
	assert.True(t, logsCalled, "logs command must call the /logs endpoint")
	assert.Contains(t, logsPath, "aabbccdd11223344aabbccdd11223344", "logs path must include resolved service ID")
	assert.Contains(t, buf.String(), "app started", "logs command must stream log content to output")
}

// ---- BT-LOGS-002: logs command passes tail parameter ----

func TestLogsCmd_PassesTailParam(t *testing.T) {
	var capturedQuery string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/logs") {
			capturedQuery = r.URL.RawQuery
			w.Header().Set("Content-Type", "text/plain")
			return
		}
		if r.URL.Path == "/v1/services" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]ahclient.Service{
				{ID: "aabbccdd11223344aabbccdd11223344", Name: "my-svc"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := ahclient.NewClient(srv.URL, "test-key")

	var buf bytes.Buffer
	err := runServiceRuntimeLogs(c, "my-svc", 42, false, &buf)
	require.NoError(t, err)
	assert.Contains(t, capturedQuery, "42", "logs request must include tail=42 in query")
}

// ---- BT-LOGS-003: logs command passes follow parameter ----

func TestLogsCmd_PassesFollowParam(t *testing.T) {
	var capturedQuery string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/logs") {
			capturedQuery = r.URL.RawQuery
			w.Header().Set("Content-Type", "text/plain")
			return
		}
		if r.URL.Path == "/v1/services" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]ahclient.Service{
				{ID: "aabbccdd11223344aabbccdd11223344", Name: "my-svc"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := ahclient.NewClient(srv.URL, "test-key")

	var buf bytes.Buffer
	err := runServiceRuntimeLogs(c, "my-svc", 100, true /* follow */, &buf)
	require.NoError(t, err)
	assert.Contains(t, capturedQuery, "follow=true", "logs request must include follow=true in query when --follow is set")
}

// ---- Regression tests ----

// Regression: build list with multiple builds must show all of them (not just one).
func TestBuildList_ShowsAllBuilds(t *testing.T) {
	now := time.Now().Unix()
	builds := []ahclient.Build{
		makeBuild("aaaaaaaaaaaaaaaaaaaaaaaaaaaa0001", "succeeded", "https://github.com/org/repo", "main", now),
		makeBuild("aaaaaaaaaaaaaaaaaaaaaaaaaaaa0002", "failed", "https://github.com/org/repo", "dev", now-100),
		makeBuild("aaaaaaaaaaaaaaaaaaaaaaaaaaaa0003", "building", "https://github.com/org/repo", "feat/x", now-200),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/builds") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(builds)
			return
		}
		if r.URL.Path == "/v1/services" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]ahclient.Service{
				{ID: "aabbccdd11223344aabbccdd11223344", Name: "my-svc"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := ahclient.NewClient(srv.URL, "test-key")

	var buf bytes.Buffer
	err := runBuildList(c, "my-svc", false, &buf)
	require.NoError(t, err)
	output := buf.String()

	// All three truncated IDs must appear.
	assert.Contains(t, output, "aaaaaaaa", "first build ID prefix must appear")
	// Branches must all appear.
	assert.Contains(t, output, "main", "main branch must appear")
	assert.Contains(t, output, "dev", "dev branch must appear")
	assert.Contains(t, output, "feat/x", "feature branch must appear")
}

// Regression: coloredBuildStatus must not embed color codes when colorsEnabled is false
// (this tests the raw function logic with force-disabled color).
func TestBuildStatusColor_NoColorMode(t *testing.T) {
	// Save and restore the global colorsEnabled state.
	orig := colorsEnabled
	colorsEnabled = false
	defer func() { colorsEnabled = orig }()

	out := coloredBuildStatus("succeeded")
	assert.Equal(t, "succeeded", out, "when colors are disabled, status must be returned as plain text")
}

// Regression: latestBuildID must return the FIRST element (index 0), not last.
func TestBuildLatest_FirstNotLast(t *testing.T) {
	builds := []ahclient.Build{
		makeBuild("first111111111111111111111111111", "succeeded", "", "main", 9999),
		makeBuild("last2222222222222222222222222222", "failed", "", "main", 1111),
	}
	id, err := latestBuildID(builds)
	require.NoError(t, err)
	assert.Equal(t, "first111111111111111111111111111", id,
		"latestBuildID must return builds[0].ID (first=newest), not builds[last].ID")
}
