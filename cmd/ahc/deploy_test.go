package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- BT-001: Image vs git URL detection ----

func TestIsGitURL_HttpsScheme(t *testing.T) {
	assert.True(t, isGitURL("https://github.com/org/repo"),
		"https:// URL should be detected as a git URL")
}

func TestIsGitURL_HttpScheme(t *testing.T) {
	assert.True(t, isGitURL("http://github.com/org/repo"),
		"http:// URL should be detected as a git URL")
}

func TestIsGitURL_DockerImage_Returnsfalse(t *testing.T) {
	assert.False(t, isGitURL("nginx:alpine"),
		"docker image tag should NOT be detected as a git URL")
}

func TestIsGitURL_BareImageName_ReturnsFalse(t *testing.T) {
	assert.False(t, isGitURL("ubuntu"),
		"bare image name should NOT be detected as a git URL")
}

func TestIsGitURL_ImageWithTag_ReturnsFalse(t *testing.T) {
	assert.False(t, isGitURL("myregistry.example.com:5000/myimage:v1"),
		"registry/image:tag should NOT be detected as a git URL")
}

// ---- BT-002: --redeploy flag behavior ----

// When --redeploy is passed with a service name, it calls RedeployService (not CreateService).
func TestRunDeploy_Redeploy_CallsRedeployEndpoint(t *testing.T) {
	redeplyCalled := false
	createCalled := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/services":
			json.NewEncoder(w).Encode([]ahclient.Service{
				{ID: "aabbccdd11223344aabbccdd11223344", Name: "my-app", Status: "running", URL: "https://my-app.example.com"},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/redeploy"):
			redeplyCalled = true
			json.NewEncoder(w).Encode(ahclient.Service{
				ID: "aabbccdd11223344aabbccdd11223344", Name: "my-app", Status: "running", URL: "https://my-app.example.com",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/services":
			createCalled = true
			json.NewEncoder(w).Encode(ahclient.Service{ID: "newid", Name: "my-app", Status: "running"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "test-key")
	var out bytes.Buffer
	err := runRedeploy(c, &out, "my-app")
	require.NoError(t, err)
	assert.True(t, redeplyCalled, "redeploy endpoint must be called when --redeploy is set")
	assert.False(t, createCalled, "CreateService must NOT be called when --redeploy is set")
}

// When --redeploy is passed with a nonexistent service name, it returns an error.
func TestRunDeploy_Redeploy_NonexistentService_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]ahclient.Service{}) // empty list
	}))
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "test-key")
	var out bytes.Buffer
	err := runRedeploy(c, &out, "ghost-service")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ghost-service", "error must name the missing service")
}

// ---- BT-003: Error on missing service name (deploy command validation) ----

func TestDeployCmd_NoArgs_ReturnsError(t *testing.T) {
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{"deploy"})

	err := cmd.Execute()
	// The deploy command should fail because we need either --redeploy with a name
	// or positional args <source> <service-name>
	assert.Error(t, err, "deploy with no args must return an error")
}

func TestDeployCmd_RedeployWithNoName_ReturnsError(t *testing.T) {
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{"deploy", "--redeploy"})

	err := cmd.Execute()
	assert.Error(t, err, "deploy --redeploy with no service name must return an error")
}

// ---- BT-004: Docker image flow — creates service, polls until running ----

func TestRunDeployImage_CreatesServiceAndPollsStatus(t *testing.T) {
	createCalled := false
	pollCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/services":
			createCalled = true
			json.NewEncoder(w).Encode(ahclient.Service{
				ID: "aabbccdd11223344aabbccdd11223344", Name: "my-site", Status: "pending",
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/services/"):
			pollCount++
			status := "pending"
			if pollCount >= 2 {
				status = "running"
			}
			json.NewEncoder(w).Encode(ahclient.Service{
				ID: "aabbccdd11223344aabbccdd11223344", Name: "my-site", Status: status,
				URL: "https://my-site.example.com",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "test-key")
	var out bytes.Buffer
	err := runDeployImage(c, &out, "nginx:alpine", "my-site", 80)
	require.NoError(t, err)
	assert.True(t, createCalled, "CreateService must be called for image deploy")
	assert.GreaterOrEqual(t, pollCount, 2, "must poll at least twice before detecting running status")
	assert.Contains(t, out.String(), "https://my-site.example.com", "output must contain the service URL")
}

// ---- BT-005: Git URL flow — creates service, starts build, streams build logs, polls service ----

func TestRunDeployGit_CreatesBuildAndStreamsLogs(t *testing.T) {
	createCalled := false
	buildStarted := false
	buildLogsFetched := false
	pollCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/services":
			createCalled = true
			json.NewEncoder(w).Encode(ahclient.Service{
				ID: "aabbccdd11223344aabbccdd11223344", Name: "my-app", Status: "pending",
			})
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/builds"):
			buildStarted = true
			json.NewEncoder(w).Encode(ahclient.StartBuildResponse{
				BuildID: "build-001",
				Status:  "queued",
			})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/builds/") && strings.HasSuffix(r.URL.Path, "/logs"):
			buildLogsFetched = true
			w.Header().Set("Content-Type", "text/plain")
			// Simulate build log lines
			w.Write([]byte("Step 1: cloning repository\nStep 2: running nixpacks\nBuild complete\n"))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/services/") && !strings.Contains(r.URL.Path, "/builds"):
			pollCount++
			status := "pending"
			if pollCount >= 2 {
				status = "running"
			}
			json.NewEncoder(w).Encode(ahclient.Service{
				ID: "aabbccdd11223344aabbccdd11223344", Name: "my-app", Status: status,
				URL: "https://my-app.example.com",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "test-key")
	var out bytes.Buffer
	err := runDeployGit(c, &out, "https://github.com/org/repo", "my-app", 3000)
	require.NoError(t, err)
	assert.True(t, createCalled, "CreateService must be called for git deploy")
	assert.True(t, buildStarted, "StartBuild must be called for git deploy")
	assert.True(t, buildLogsFetched, "build logs must be fetched for git deploy")
	// Build log lines should appear with [build] prefix
	assert.Contains(t, out.String(), "[build]", "build log lines must be prefixed with [build]")
	assert.Contains(t, out.String(), "https://my-app.example.com", "output must contain the service URL")
}

// ---- BT-006: Deploy command help text contains examples ----

func TestDeployCmd_HelpText_ContainsExamples(t *testing.T) {
	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"deploy", "--help"})

	_ = cmd.Execute()
	output := out.String()

	assert.Contains(t, output, "nginx:alpine", "help must contain Docker image example")
	assert.Contains(t, output, "github.com", "help must contain git URL example")
	assert.Contains(t, output, "--redeploy", "help must describe --redeploy flag")
}

// ---- Regression tests ----

// REG-001: isGitURL returns false for empty string (no panic).
func TestIsGitURL_EmptyString_ReturnsFalse(t *testing.T) {
	assert.False(t, isGitURL(""), "empty string should not be treated as git URL")
}

// REG-002: runRedeploy outputs the service URL after redeployment.
func TestRunRedeploy_OutputsURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/services":
			json.NewEncoder(w).Encode([]ahclient.Service{
				{ID: "aabbccdd11223344aabbccdd11223344", Name: "my-app", Status: "running", URL: "https://my-app.example.com"},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/redeploy"):
			json.NewEncoder(w).Encode(ahclient.Service{
				ID: "aabbccdd11223344aabbccdd11223344", Name: "my-app", Status: "running", URL: "https://my-app.example.com",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "test-key")
	var out bytes.Buffer
	err := runRedeploy(c, &out, "my-app")
	require.NoError(t, err)
	assert.Contains(t, out.String(), "https://my-app.example.com",
		"runRedeploy must print the service URL after successful redeployment")
}

// REG-003: runDeployImage returns error when service never reaches running within attempts.
func TestRunDeployImage_ServiceNeverRunning_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/services":
			json.NewEncoder(w).Encode(ahclient.Service{
				ID: "aabbccdd11223344aabbccdd11223344", Name: "stuck-svc", Status: "pending",
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/services/"):
			// Always return "pending" — never transitions to running
			json.NewEncoder(w).Encode(ahclient.Service{
				ID: "aabbccdd11223344aabbccdd11223344", Name: "stuck-svc", Status: "pending",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "test-key")
	var out bytes.Buffer
	// Use a very low maxAttempts to make the test fast.
	err := pollUntilRunning(c, &out, "aabbccdd11223344aabbccdd11223344", 3)
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "timeout",
		"error message must mention timeout when service never starts")
}

// REG-004: runDeployImage returns error when service enters "error" state.
func TestRunDeployImage_ServiceEntersErrorState_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/services":
			json.NewEncoder(w).Encode(ahclient.Service{
				ID: "aabbccdd11223344aabbccdd11223344", Name: "bad-svc", Status: "pending",
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/services/"):
			json.NewEncoder(w).Encode(ahclient.Service{
				ID: "aabbccdd11223344aabbccdd11223344", Name: "bad-svc", Status: "error",
				LastError: "container exited with code 1",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := ahclient.NewClient(srv.URL, "test-key")
	var out bytes.Buffer
	err := pollUntilRunning(c, &out, "aabbccdd11223344aabbccdd11223344", 5)
	require.Error(t, err)
	// Should mention the error state or last_error
	assert.True(t,
		strings.Contains(strings.ToLower(err.Error()), "error") || strings.Contains(err.Error(), "container exited"),
		"error message must indicate the service failed, got: %s", err.Error())
}
