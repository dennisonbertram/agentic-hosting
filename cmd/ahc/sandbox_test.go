package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSbxID = "aabbccdd11223344aabbccdd11223344"

// fakeSandboxAPI is a minimal /v1/sandboxes server for CLI tests.
type fakeSandboxAPI struct {
	mu        sync.Mutex
	created   []map[string]any
	execBody  map[string]any
	execCode  int  // exit code returned by exec
	execFail  bool // exec returns HTTP 500
	deleted   []string
	deleteErr bool
}

func (f *fakeSandboxAPI) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			var m map[string]any
			require.NoError(t, json.Unmarshal(body, &m))
			f.created = append(f.created, m)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"`+testSbxID+`","name":"sbx-aabbccdd","status":"running","expires_at":1}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes":
			_, _ = io.WriteString(w, `[{"id":"`+testSbxID+`","name":"scratch","status":"running","source":{"type":"template","ref":"default"}}]`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes/"+testSbxID+"/exec":
			if f.execFail {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"error":"boom"}`)
				return
			}
			_ = json.Unmarshal(body, &f.execBody)
			_ = json.NewEncoder(w).Encode(ahclient.ExecResult{ExitCode: f.execCode, Stdout: "out\n", Stderr: "err\n"})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/sandboxes/"+testSbxID:
			f.deleted = append(f.deleted, testSbxID)
			if f.deleteErr {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"error":"daemon busy"}`)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/"+testSbxID:
			_, _ = io.WriteString(w, `{"id":"`+testSbxID+`","name":"scratch","status":"running","image":"alpine","source":{"type":"image","ref":"alpine"},"memory_mb":512,"cpu_millicores":1000}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes/"+testSbxID+"/extend":
			_ = json.Unmarshal(body, &f.execBody)
			_, _ = io.WriteString(w, `{"id":"`+testSbxID+`","name":"scratch","status":"running","expires_at":2}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/"+testSbxID+"/logs":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "log line tail="+r.URL.Query().Get("tail")+"\n")
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not found"}`)
		}
	})
}

func newFakeSandboxAPI(t *testing.T) (*fakeSandboxAPI, *ahclient.Client) {
	f := &fakeSandboxAPI{}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	return f, newTestClient(srv)
}

func TestParseTTL(t *testing.T) {
	for in, want := range map[string]int{"": 0, "90": 90, "30m": 1800, "2h": 7200, "1h30m": 5400} {
		got, err := parseTTL(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, bad := range []string{"-5", "0", "soon", "10ms"} {
		_, err := parseTTL(bad)
		assert.Error(t, err, bad)
	}
}

func TestBuildSandboxRequest_FlagMapping(t *testing.T) {
	req, err := buildSandboxRequest(sandboxCreateOpts{})
	require.NoError(t, err)
	assert.Equal(t, ahclient.SandboxSource{Type: "template", Template: "default"}, req.Source)
	assert.Nil(t, req.TTLSeconds)
	assert.Nil(t, req.MemoryMB)

	req, err = buildSandboxRequest(sandboxCreateOpts{name: "x", image: "alpine", ttl: "15m", run: "app", memoryMB: 256, cpuMilli: 500})
	require.NoError(t, err)
	assert.Equal(t, "x", req.Name)
	assert.Equal(t, ahclient.SandboxSource{Type: "image", Image: "alpine"}, req.Source)
	require.NotNil(t, req.TTLSeconds)
	assert.Equal(t, 900, *req.TTLSeconds)
	assert.Equal(t, "app", req.Run)
	assert.Equal(t, 256, *req.MemoryMB)
	assert.Equal(t, 500, *req.CPUMillicores)

	req, err = buildSandboxRequest(sandboxCreateOpts{snapshot: "sn1", includeEnv: true})
	require.NoError(t, err)
	assert.Equal(t, ahclient.SandboxSource{Type: "snapshot", SnapshotID: "sn1"}, req.Source)
	assert.True(t, req.IncludeEnv)

	req, err = buildSandboxRequest(sandboxCreateOpts{service: "s1"})
	require.NoError(t, err)
	assert.Equal(t, ahclient.SandboxSource{Type: "service", ServiceID: "s1"}, req.Source)

	_, err = buildSandboxRequest(sandboxCreateOpts{image: "a", template: "b"})
	assert.ErrorContains(t, err, "only one")
	_, err = buildSandboxRequest(sandboxCreateOpts{ttl: "later"})
	assert.Error(t, err)
}

func TestSandboxRun_DeletesOnSuccess(t *testing.T) {
	f, c := newFakeSandboxAPI(t)
	var out, errOut bytes.Buffer
	ttl := 600
	err := runSandboxRun(c, &out, &errOut, ahclient.CreateSandboxRequest{Source: ahclient.SandboxSource{Type: "template"}, TTLSeconds: &ttl},
		[]string{"echo", "hi"}, "/app", 30)
	require.NoError(t, err)
	assert.Equal(t, "out\n", out.String())
	assert.Equal(t, "err\n", errOut.String())
	assert.Equal(t, []string{testSbxID}, f.deleted)
	assert.Equal(t, []any{"echo", "hi"}, f.execBody["command"])
	assert.Equal(t, "/app", f.execBody["work_dir"])
	assert.EqualValues(t, 600, f.created[0]["ttl_seconds"])
}

func TestSandboxRun_DeletesOnNonZeroExitAndPropagatesCode(t *testing.T) {
	f, c := newFakeSandboxAPI(t)
	f.execCode = 3
	err := runSandboxRun(c, io.Discard, io.Discard, ahclient.CreateSandboxRequest{Source: ahclient.SandboxSource{Type: "template"}}, []string{"false"}, "", 0)
	var ec *exitCodeError
	require.True(t, errors.As(err, &ec), "got %v", err)
	assert.Equal(t, 3, ec.ExitCode())
	assert.Equal(t, []string{testSbxID}, f.deleted)
}

func TestSandboxRun_DeletesOnExecError(t *testing.T) {
	f, c := newFakeSandboxAPI(t)
	f.execFail = true
	err := runSandboxRun(c, io.Discard, io.Discard, ahclient.CreateSandboxRequest{Source: ahclient.SandboxSource{Type: "template"}}, []string{"ls"}, "", 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exec")
	assert.Equal(t, []string{testSbxID}, f.deleted)
}

func TestSandboxRun_DeleteFailureIsReported(t *testing.T) {
	f, c := newFakeSandboxAPI(t)
	f.deleteErr = true
	var errOut bytes.Buffer
	err := runSandboxRun(c, io.Discard, &errOut, ahclient.CreateSandboxRequest{Source: ahclient.SandboxSource{Type: "template"}}, []string{"ls"}, "", 0)
	require.Error(t, err, "a successful command still reports the failed delete")
	assert.Contains(t, errOut.String(), "could not delete sandbox")

	// A failing command keeps its own exit code.
	f.execCode = 2
	err = runSandboxRun(c, io.Discard, io.Discard, ahclient.CreateSandboxRequest{Source: ahclient.SandboxSource{Type: "template"}}, []string{"ls"}, "", 0)
	var ec *exitCodeError
	require.True(t, errors.As(err, &ec))
	assert.Equal(t, 2, ec.ExitCode())
}

func TestResolveSandboxID(t *testing.T) {
	_, c := newFakeSandboxAPI(t)
	id, err := resolveSandboxID(c, testSbxID)
	require.NoError(t, err)
	assert.Equal(t, testSbxID, id)
	id, err = resolveSandboxID(c, "scratch")
	require.NoError(t, err)
	assert.Equal(t, testSbxID, id)
	id, err = resolveSandboxID(c, "aabbcc")
	require.NoError(t, err)
	assert.Equal(t, testSbxID, id)
	_, err = resolveSandboxID(c, "nope")
	assert.Error(t, err)
}

func TestSandboxCmd_ThroughRoot(t *testing.T) {
	f := &fakeSandboxAPI{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	run := func(args ...string) (string, error) {
		out := &bytes.Buffer{}
		cmd := newRootCmd()
		cmd.SetOut(out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(append([]string{"--url", srv.URL, "--key", "k"}, args...))
		err := cmd.Execute()
		return out.String(), err
	}

	out, err := run("sbx", "create", "scratch", "--image", "alpine", "--ttl", "5m", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, testSbxID)
	assert.Equal(t, "scratch", f.created[0]["name"])
	assert.EqualValues(t, 300, f.created[0]["ttl_seconds"])

	out, err = run("sandbox", "list")
	require.NoError(t, err)
	assert.Contains(t, out, "scratch")
	assert.Contains(t, out, "template:default")

	_, err = run("sandbox", "exec", "scratch", "--timeout", "9", "--", "ls", "-la")
	require.NoError(t, err)
	assert.EqualValues(t, 9, f.execBody["timeout"])
	assert.Equal(t, []any{"ls", "-la"}, f.execBody["command"])

	out, err = run("sandbox", "get", "scratch")
	require.NoError(t, err)
	assert.Contains(t, out, "Image:     alpine")
	assert.Contains(t, out, "512 MB")

	out, err = run("sandbox", "logs", "scratch", "--tail", "7")
	require.NoError(t, err)
	assert.Equal(t, "log line tail=7\n", out)

	out, err = run("sandbox", "extend", "scratch", "--ttl", "1h")
	require.NoError(t, err)
	assert.EqualValues(t, 3600, f.execBody["ttl_seconds"])
	assert.Contains(t, out, "now expires")
	_, err = run("sandbox", "extend", "scratch")
	assert.ErrorContains(t, err, "--ttl is required")

	_, err = run("sandbox", "delete", "scratch")
	require.NoError(t, err)
	assert.Len(t, f.deleted, 1)

	_, err = run("sandbox", "run", "--image", "alpine", "--", "true")
	require.NoError(t, err)
	assert.Len(t, f.deleted, 2)
	assert.EqualValues(t, 600, f.created[1]["ttl_seconds"], "run defaults to a 10m TTL")

	_, err = run("sandbox", "run", "--image", "alpine")
	assert.ErrorContains(t, err, "no command")

	out, err = run("sandbox", "--help")
	require.NoError(t, err)
	assert.True(t, strings.Contains(out, "ahc sandbox run"))
}
