package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const svcID = "aabbccdd11223344aabbccdd11223344"

func runRoot(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := newRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

func isolateHome(t *testing.T) string {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AH_URL", "")
	t.Setenv("AH_KEY", "")
	return home
}

// Finding 1: server marks databases "ready", not "running".
func TestPollDb_AcceptsReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(ahclient.Database{ID: "d", Status: "ready"})
	}))
	defer srv.Close()
	db, err := pollDbUntilRunning(newTestClient(srv), "d", &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, "ready", db.Status)
}

// Finding 2: "failed" is the server's failure status; last_error is shown.
func TestPollUntilRunning_FailedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(ahclient.Service{ID: svcID, Name: "x", Status: "failed", LastError: "image pull failed: nope"})
	}))
	defer srv.Close()
	err := pollUntilRunning(newTestClient(srv), &bytes.Buffer{}, svcID, 3)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "image pull failed: nope")
}

func TestRunDeployGit_FailedBuildFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/services":
			json.NewEncoder(w).Encode(ahclient.Service{ID: svcID, Name: "app", Status: "created"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/builds"):
			json.NewEncoder(w).Encode(ahclient.StartBuildResponse{BuildID: "b1", Status: "pending"})
		case strings.HasSuffix(r.URL.Path, "/logs"):
			w.Write([]byte("boom\n"))
		case strings.HasSuffix(r.URL.Path, "/builds/b1"):
			json.NewEncoder(w).Encode(ahclient.Build{ID: "b1", Status: "failed"})
		default:
			// A failed build must not fall through to polling the service as if it worked.
			json.NewEncoder(w).Encode(ahclient.Service{ID: svcID, Name: "app", Status: "running"})
		}
	}))
	defer srv.Close()
	err := runDeployGit(newTestClient(srv), &bytes.Buffer{}, "https://github.com/o/r", "app", 80)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "build")
}

// Finding 3: --save uses the resolved URL (from config file) and works with --json.
func TestRegisterSave_URLFromConfigFile_AndJSON(t *testing.T) {
	home := isolateHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"tenant_id": "t", "api_key": "newkey"})
	}))
	defer srv.Close()
	cfgPath := filepath.Join(home, ".ah", "config.json")
	require.NoError(t, (&ahclient.Config{URL: srv.URL, Key: "old"}).Save(cfgPath))

	_, _, err := runRoot(t, "--json", "register", "A", "a@b.co", "--bootstrap-token", "tok", "--save")
	require.NoError(t, err)
	cfg, err := ahclient.LoadConfig(ahclient.LoadOptions{})
	require.NoError(t, err)
	assert.Equal(t, srv.URL, cfg.URL)
	assert.Equal(t, "newkey", cfg.Key)
}

func TestRecoverSave_URLFromEnv(t *testing.T) {
	isolateHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"id": "k", "name": "n", "key": "recovered"})
	}))
	defer srv.Close()
	t.Setenv("AH_URL", srv.URL)

	_, _, err := runRoot(t, "recover", "--email", "a@b.co", "--bootstrap-token", "tok", "--save")
	require.NoError(t, err)
	t.Setenv("AH_URL", "")
	cfg, err := ahclient.LoadConfig(ahclient.LoadOptions{})
	require.NoError(t, err)
	assert.Equal(t, srv.URL, cfg.URL)
	assert.Equal(t, "recovered", cfg.Key)
}

// Finding 4: a corrupt config file is reported, not swallowed.
func TestGetClient_CorruptConfigSurfaced(t *testing.T) {
	home := isolateHome(t)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".ah"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".ah", "config.json"), []byte("{not json"), 0600))
	_, err := getClient(&rootState{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config.json")
}

// Finding 6: status exit code covers every unhealthy signal.
func TestStatus_ExitCodeUnhealthySignals(t *testing.T) {
	cases := map[string]func(h *ahclient.DetailedHealthResponse, s []ahclient.Service){
		"circuit open":  func(h *ahclient.DetailedHealthResponse, s []ahclient.Service) { s[0].CircuitOpen = true },
		"crashed":       func(h *ahclient.DetailedHealthResponse, s []ahclient.Service) { s[0].Status = "crashed" },
		"health not ok": func(h *ahclient.DetailedHealthResponse, s []ahclient.Service) { h.Status = "degraded" },
		"docker down":   func(h *ahclient.DetailedHealthResponse, s []ahclient.Service) { h.Docker.Available = false },
		"docker disk":   func(h *ahclient.DetailedHealthResponse, s []ahclient.Service) { h.DockerDisk.UsedPercent = 95 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h, s, d := healthyState()
			mutate(&h, s)
			srv := buildStatusServer(h, s, d)
			defer srv.Close()
			code, err := runStatusCmd(ahclient.NewClient(srv.URL, "k"), &bytes.Buffer{}, false)
			require.NoError(t, err)
			assert.Equal(t, 1, code)
		})
	}
}

// Finding 7: --json output is a single JSON document after the full flow.
func TestDbProvision_JSONRunsFullFlow(t *testing.T) {
	isolateHome(t)
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/services":
			json.NewEncoder(w).Encode([]ahclient.Service{{ID: svcID, Name: "api"}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/databases":
			json.NewEncoder(w).Encode(ahclient.Database{ID: "dbid", Name: "api-postgres", Status: "ready"})
		case strings.HasSuffix(r.URL.Path, "/connection-string"):
			json.NewEncoder(w).Encode(ahclient.ConnectionStringResponse{ConnectionString: "postgres://u:p@h:5432/d"})
		case strings.HasPrefix(r.URL.Path, "/v1/databases/"):
			json.NewEncoder(w).Encode(ahclient.Database{ID: "dbid", Name: "api-postgres", Status: "ready"})
		default:
			json.NewEncoder(w).Encode(ahclient.StatusResponse{Status: "ok"})
		}
	}))
	defer srv.Close()
	out, _, err := runRoot(t, "--url", srv.URL, "--key", "k", "--json", "db", "provision", "api", "postgres")
	require.NoError(t, err)
	var db ahclient.Database
	require.NoError(t, json.Unmarshal([]byte(out), &db), "stdout must be only JSON: %q", out)
	joined := strings.Join(calls, "\n")
	assert.Contains(t, joined, "POST /v1/services/"+svcID+"/env")
	assert.Contains(t, joined, "POST /v1/services/"+svcID+"/restart")
}

// Finding 9: exec exit code, timed_out.
func TestEnvExec_ExitCodePropagates(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  ahclient.ExecResult
		want int
	}{
		{"exit 3", ahclient.ExecResult{ExitCode: 3}, 3},
		{"timed out", ahclient.ExecResult{ExitCode: 0, TimedOut: true}, 124},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(tc.res)
			}))
			defer srv.Close()
			err := runEnvExec(newTestClient(srv), &bytes.Buffer{}, svcID, []string{"x"})
			require.Error(t, err)
			var ec interface{ ExitCode() int }
			require.True(t, errors.As(err, &ec), "error must expose ExitCode()")
			assert.Equal(t, tc.want, ec.ExitCode())
		})
	}
}

func TestBuildList_HonorsJSON(t *testing.T) {
	isolateHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]ahclient.Build{{ID: "b1", Status: "succeeded"}})
	}))
	defer srv.Close()
	out, _, err := runRoot(t, "--url", srv.URL, "--key", "k", "--json", "build", "list", "--all")
	require.NoError(t, err)
	var builds []ahclient.Build
	require.NoError(t, json.Unmarshal([]byte(out), &builds), out)
}

func TestServiceCreate_ImageRequired(t *testing.T) {
	isolateHome(t)
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer srv.Close()
	_, _, err := runRoot(t, "--url", srv.URL, "--key", "k", "service", "create", "--name", "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--image")
	assert.False(t, called)
}

// Finding 8: saving over a world-readable file tightens permissions.
func TestConfigSave_TightensExistingPerms(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(p, []byte("{}"), 0644))
	require.NoError(t, (&ahclient.Config{URL: "u", Key: "k"}).Save(p))
	fi, err := os.Stat(p)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), fi.Mode().Perm())
}
