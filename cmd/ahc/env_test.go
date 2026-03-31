package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- BT-ENV-001: env list table has correct columns ----

func TestEnvList_TableColumns(t *testing.T) {
	envs := []ahclient.Environment{
		{
			ID:         "aabbccdd11223344aabbccdd11223344",
			Name:       "my-env",
			Status:     "running",
			TemplateID: "tmpl-golang",
			CreatedAt:  1743200000,
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(envs)
	}))
	defer srv.Close()
	c := newTestClient(srv)

	var buf bytes.Buffer
	err := runEnvList(c, &buf)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "NAME", "env table must have NAME column")
	assert.Contains(t, output, "STATUS", "env table must have STATUS column")
	assert.Contains(t, output, "TEMPLATE", "env table must have TEMPLATE column")
	assert.Contains(t, output, "ID", "env table must have ID column")
	assert.Contains(t, output, "my-env", "env table must show environment name")
	assert.Contains(t, output, "running", "env table must show environment status")
}

// ---- BT-ENV-002: env list truncates ID ----

func TestEnvList_TruncatesID(t *testing.T) {
	envs := []ahclient.Environment{
		{ID: "aabbccdd11223344aabbccdd11223344", Name: "env1", Status: "running", CreatedAt: 1743200000},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(envs)
	}))
	defer srv.Close()
	c := newTestClient(srv)

	var buf bytes.Buffer
	err := runEnvList(c, &buf)
	require.NoError(t, err)
	output := buf.String()

	assert.NotContains(t, output, "aabbccdd11223344aabbccdd11223344", "full 32-char ID must not appear")
	assert.Contains(t, output, "aabbccdd", "truncated ID must appear")
}

// ---- BT-ENV-003: env list shows "No environments found" for empty list ----

func TestEnvList_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]ahclient.Environment{})
	}))
	defer srv.Close()
	c := newTestClient(srv)

	var buf bytes.Buffer
	err := runEnvList(c, &buf)
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "No environments", "empty list must say 'No environments'")
}

// ---- BT-ENV-004: env create sends correct request ----

func TestEnvCreate_SendsCorrectRequest(t *testing.T) {
	createCalled := false
	var capturedReq ahclient.CreateEnvironmentRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/v1/environments" {
			createCalled = true
			json.NewDecoder(r.Body).Decode(&capturedReq)
			json.NewEncoder(w).Encode(ahclient.Environment{
				ID: "aabbccdd11223344aabbccdd11223344", Name: capturedReq.Name, Status: "starting",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := newTestClient(srv)

	var buf bytes.Buffer
	lease := 3600
	err := runEnvCreate(c, &buf, "my-env", "tmpl-golang", &lease)
	require.NoError(t, err)
	assert.True(t, createCalled, "CreateEnvironment must be called")
	assert.Equal(t, "my-env", capturedReq.Name, "environment name must be set in request")
	assert.Equal(t, "tmpl-golang", capturedReq.TemplateID, "template ID must be set in request")
	require.NotNil(t, capturedReq.LeaseDurationSeconds, "lease duration must be set in request")
	assert.Equal(t, 3600, *capturedReq.LeaseDurationSeconds, "lease duration must be 3600")
}

// ---- BT-ENV-005: env create without template uses empty template ----

func TestEnvCreate_NoTemplate(t *testing.T) {
	var capturedReq ahclient.CreateEnvironmentRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			json.NewDecoder(r.Body).Decode(&capturedReq)
			json.NewEncoder(w).Encode(ahclient.Environment{
				ID: "aabbccdd11223344aabbccdd11223344", Name: "my-env", Status: "starting",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := newTestClient(srv)

	var buf bytes.Buffer
	err := runEnvCreate(c, &buf, "my-env", "", nil)
	require.NoError(t, err)
	assert.Equal(t, "", capturedReq.TemplateID, "template ID must be empty when not specified")
	assert.Nil(t, capturedReq.LeaseDurationSeconds, "lease duration must be nil when not specified")
}

// ---- BT-ENV-006: env exec runs command and prints output ----

func TestEnvExec_PrintsOutput(t *testing.T) {
	execCalled := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/environments" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode([]ahclient.Environment{
				{ID: "aabbccdd11223344aabbccdd11223344", Name: "my-env", Status: "running"},
			})
		case strings.Contains(r.URL.Path, "/exec") && r.Method == http.MethodPost:
			execCalled = true
			json.NewEncoder(w).Encode(ahclient.ExecResult{
				ExitCode: 0,
				Stdout:   "hello from exec\n",
				Stderr:   "",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := newTestClient(srv)

	var buf bytes.Buffer
	err := runEnvExec(c, &buf, "my-env", []string{"echo", "hello"})
	require.NoError(t, err)
	assert.True(t, execCalled, "EnvironmentExec must be called")
	assert.Contains(t, buf.String(), "hello from exec", "exec output must be printed")
}

// ---- BT-ENV-007: env exec returns error when exit code is non-zero ----

func TestEnvExec_NonZeroExitCode_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/environments" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode([]ahclient.Environment{
				{ID: "aabbccdd11223344aabbccdd11223344", Name: "my-env", Status: "running"},
			})
		case strings.Contains(r.URL.Path, "/exec") && r.Method == http.MethodPost:
			json.NewEncoder(w).Encode(ahclient.ExecResult{
				ExitCode: 1,
				Stdout:   "",
				Stderr:   "command not found\n",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := newTestClient(srv)

	var buf bytes.Buffer
	err := runEnvExec(c, &buf, "my-env", []string{"badcmd"})
	require.Error(t, err, "non-zero exit code must return an error")
	assert.Contains(t, err.Error(), "exit code 1", "error must mention exit code")
}

// ---- BT-ENV-008: env stop calls StopEnvironment ----

func TestEnvStop_CallsStopEndpoint(t *testing.T) {
	stopCalled := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/environments" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode([]ahclient.Environment{
				{ID: "aabbccdd11223344aabbccdd11223344", Name: "my-env", Status: "running"},
			})
		case strings.Contains(r.URL.Path, "/stop") && r.Method == http.MethodPost:
			stopCalled = true
			json.NewEncoder(w).Encode(ahclient.StatusResponse{Status: "stopped"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := newTestClient(srv)

	var buf bytes.Buffer
	err := runEnvStop(c, &buf, "my-env")
	require.NoError(t, err)
	assert.True(t, stopCalled, "StopEnvironment must be called")
}

// ---- BT-ENV-009: env start calls StartEnvironment ----

func TestEnvStart_CallsStartEndpoint(t *testing.T) {
	startCalled := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/environments" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode([]ahclient.Environment{
				{ID: "aabbccdd11223344aabbccdd11223344", Name: "my-env", Status: "stopped"},
			})
		case strings.Contains(r.URL.Path, "/start") && r.Method == http.MethodPost:
			startCalled = true
			json.NewEncoder(w).Encode(ahclient.StatusResponse{Status: "running"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := newTestClient(srv)

	var buf bytes.Buffer
	err := runEnvStart(c, &buf, "my-env")
	require.NoError(t, err)
	assert.True(t, startCalled, "StartEnvironment must be called")
}

// ---- BT-ENV-010: env delete requires --confirm ----

func TestEnvDelete_RequiresConfirm(t *testing.T) {
	deleteCalled := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleteCalled = true
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]ahclient.Environment{
			{ID: "aabbccdd11223344aabbccdd11223344", Name: "my-env", Status: "running"},
		})
	}))
	defer srv.Close()
	c := newTestClient(srv)

	err := runEnvDelete(c, "my-env", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--confirm", "error must mention --confirm")
	assert.False(t, deleteCalled, "DELETE must not be called without --confirm")
}

func TestEnvDelete_WithConfirm(t *testing.T) {
	deleteCalled := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/environments/") {
			deleteCalled = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path == "/v1/environments" {
			json.NewEncoder(w).Encode([]ahclient.Environment{
				{ID: "aabbccdd11223344aabbccdd11223344", Name: "my-env", Status: "running"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := newTestClient(srv)

	err := runEnvDelete(c, "my-env", true)
	require.NoError(t, err)
	assert.True(t, deleteCalled, "DELETE must be called when --confirm is set")
}

// ---- BT-ENV-011: env templates lists available templates ----

func TestEnvTemplates_ListsTemplates(t *testing.T) {
	templates := []ahclient.EnvironmentTemplate{
		{ID: "tmpl-golang", Name: "Go 1.22", Description: "Go development environment", BaseImage: "golang:1.22"},
		{ID: "tmpl-node", Name: "Node 20", Description: "Node.js development environment", BaseImage: "node:20"},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(templates)
	}))
	defer srv.Close()
	c := newTestClient(srv)

	var buf bytes.Buffer
	err := runEnvTemplates(c, &buf)
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "tmpl-golang", "templates list must show template ID")
	assert.Contains(t, output, "Go 1.22", "templates list must show template name")
	assert.Contains(t, output, "tmpl-node", "templates list must show second template")
}

// ---- BT-ENV-012: env get shows environment details ----

func TestEnvGet_ShowsDetails(t *testing.T) {
	env := ahclient.Environment{
		ID:         "aabbccdd11223344aabbccdd11223344",
		Name:       "my-env",
		Status:     "running",
		TemplateID: "tmpl-golang",
		CreatedAt:  1743200000,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/environments" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode([]ahclient.Environment{env})
		case strings.HasPrefix(r.URL.Path, "/v1/environments/") && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(env)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := newTestClient(srv)

	var buf bytes.Buffer
	err := runEnvGet(c, &buf, "my-env")
	require.NoError(t, err)
	output := buf.String()

	assert.Contains(t, output, "my-env", "env get must show environment name")
	assert.Contains(t, output, "running", "env get must show environment status")
}

// ---- Regression tests ----

// REG-ENV-001: runEnvDelete with hex ID and confirm=false must error before calling DELETE
func TestEnvDelete_HexIDNoConfirm(t *testing.T) {
	deleteCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleteCalled = true
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := newTestClient(srv)

	err := runEnvDelete(c, "aabbccdd11223344aabbccdd11223344", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--confirm")
	assert.False(t, deleteCalled)
}

// REG-ENV-002: env exec stderr content is printed even on success (exit code 0)
func TestEnvExec_PrintsStderr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/environments" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode([]ahclient.Environment{
				{ID: "aabbccdd11223344aabbccdd11223344", Name: "my-env", Status: "running"},
			})
		case strings.Contains(r.URL.Path, "/exec") && r.Method == http.MethodPost:
			json.NewEncoder(w).Encode(ahclient.ExecResult{
				ExitCode: 0,
				Stdout:   "stdout content\n",
				Stderr:   "warning: deprecated flag\n",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := newTestClient(srv)

	var buf bytes.Buffer
	err := runEnvExec(c, &buf, "my-env", []string{"cmd"})
	require.NoError(t, err)
	output := buf.String()
	assert.Contains(t, output, "stdout content", "stdout must be printed")
	// stderr warning should also be present
	assert.Contains(t, output, "warning: deprecated flag", "stderr must be printed")
}

// REG-ENV-003: env list EXPIRES column shows lease info when present
func TestEnvList_ShowsLeaseInfo(t *testing.T) {
	expires := int64(1743286400)
	envs := []ahclient.Environment{
		{
			ID:             "aabbccdd11223344aabbccdd11223344",
			Name:           "leased-env",
			Status:         "running",
			TemplateID:     "tmpl-golang",
			LeaseExpiresAt: &expires,
			CreatedAt:      1743200000,
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(envs)
	}))
	defer srv.Close()
	c := newTestClient(srv)

	var buf bytes.Buffer
	err := runEnvList(c, &buf)
	require.NoError(t, err)

	output := buf.String()
	// The table should have an EXPIRES column
	assert.Contains(t, output, "EXPIRES", "env list must have EXPIRES column")
}

// REG-ENV-004: env cmd help text contains examples
func TestEnvCmd_HelpTextContainsExamples(t *testing.T) {
	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"env", "--help"})

	_ = cmd.Execute()
	output := out.String()

	// Help text should contain key subcommands
	assert.Contains(t, output, "create", "env help must mention 'create'")
	assert.Contains(t, output, "exec", "env help must mention 'exec'")
	assert.Contains(t, output, "delete", "env help must mention 'delete'")
}

// ---- helper: stub for io.Writer on exec output ----
var _ io.Writer = &bytes.Buffer{}
