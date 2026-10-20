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

const (
	fakeSvcID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	fakeDbID  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	fakeEnvID = "cccccccccccccccccccccccccccccccc"
	fakeSnpID = "dddddddddddddddddddddddddddddddd"
)

// fakeAPI is a minimal in-memory agentic-hosting API used to drive whole
// ahc commands through newRootCmd. It records "METHOD path" for each call.
type fakeAPI struct {
	mu    sync.Mutex
	calls []string
	srv   *httptest.Server
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	mux := http.NewServeMux()
	js := func(v any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(v)
		}
	}
	svc := ahclient.Service{ID: fakeSvcID, Name: "web", Status: "running", Image: "nginx", Port: 80, URL: "https://web.example", LastError: "boom"}
	db := ahclient.Database{ID: fakeDbID, Name: "pg", Type: "postgres", Status: "ready", Host: "h", Port: 5432, DBName: "d", Username: "u",
		ConnectionString: "postgres://u:secretpw@h:5432/d"}
	ok := ahclient.StatusResponse{Status: "ok"}

	mux.HandleFunc("GET /v1/services", js([]ahclient.Service{svc}))
	mux.HandleFunc("POST /v1/services", js(svc))
	mux.HandleFunc("GET /v1/services/{id}", js(svc))
	mux.HandleFunc("PATCH /v1/services/{id}", js(ahclient.Service{ID: fakeSvcID, Name: "renamed"}))
	mux.HandleFunc("DELETE /v1/services/{id}", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, a := range []string{"start", "stop", "restart", "reset"} {
		mux.HandleFunc("POST /v1/services/{id}/"+a, js(ok))
	}
	mux.HandleFunc("POST /v1/services/{id}/redeploy", js(svc))
	mux.HandleFunc("GET /v1/services/{id}/deployments", js([]ahclient.Deployment{{ID: "dep-0123456789", Status: "succeeded", Trigger: "api", Image: strings.Repeat("i", 50), StartedAt: 1}}))
	mux.HandleFunc("GET /v1/services/{id}/env", js(map[string]string{"B": "2", "A": "1"}))
	mux.HandleFunc("POST /v1/services/{id}/env", js(ok))
	mux.HandleFunc("DELETE /v1/services/{id}/env/{key}", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /v1/services/{id}/logs", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "log line\n") })
	mux.HandleFunc("GET /v1/services/{id}/builds", js([]ahclient.Build{{ID: "build-newest", Status: "succeeded"}}))
	mux.HandleFunc("GET /v1/services/{id}/builds/{bid}/logs", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "build log for "+r.PathValue("bid")+"\n")
	})
	mux.HandleFunc("DELETE /v1/services/{id}/builds/{bid}", js(ok))
	mux.HandleFunc("GET /v1/databases", js([]ahclient.Database{db}))
	mux.HandleFunc("GET /v1/databases/{id}", js(db))
	mux.HandleFunc("DELETE /v1/databases/{id}", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /v1/environments", js([]ahclient.Environment{{ID: fakeEnvID, Name: "dev", Status: "running"}}))
	mux.HandleFunc("GET /v1/environments/{id}", js(ahclient.Environment{ID: fakeEnvID, Name: "dev", Status: "running"}))
	mux.HandleFunc("GET /v1/environments/templates", js([]ahclient.EnvironmentTemplate{{ID: "tmpl-go", Name: "Go"}}))
	mux.HandleFunc("POST /v1/environments/{id}/start", js(ok))
	mux.HandleFunc("POST /v1/environments/{id}/stop", js(ok))
	mux.HandleFunc("GET /v1/snapshots", js([]ahclient.Snapshot{{ID: fakeSnpID, Name: "snap", ServiceID: fakeSvcID}}))
	mux.HandleFunc("POST /v1/services/{id}/snapshots", js(ahclient.Snapshot{ID: "s1", Name: "x"})) // short ID must not panic
	mux.HandleFunc("GET /v1/system/health/detailed", js(ahclient.DetailedHealthResponse{Status: "ok",
		Docker: ahclient.DockerInfo{Available: true}, GVisor: ahclient.GVisorInfo{Available: true}}))

	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)

	isolateHome(t)
	t.Setenv("AH_URL", f.srv.URL)
	t.Setenv("AH_KEY", "k")
	return f
}

func (f *fakeAPI) called(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == call {
			return true
		}
	}
	return false
}

func TestCommands_AgainstFakeAPI(t *testing.T) {
	cases := []struct {
		args     []string
		call     string
		contains []string
		absent   []string
	}{
		{[]string{"service", "list"}, "GET /v1/services", []string{"web", "running", "aaaaaaaa"}, nil},
		{[]string{"service", "list", "--json"}, "GET /v1/services", []string{`"name": "web"`}, nil},
		{[]string{"service", "get", "web"}, "GET /v1/services/" + fakeSvcID, []string{"Image:", "nginx", "Last Error:", "boom", "URL:"}, nil},
		{[]string{"service", "get", fakeSvcID, "--json"}, "GET /v1/services/" + fakeSvcID, []string{`"id": "` + fakeSvcID}, nil},
		{[]string{"service", "create", "--name", "web", "--image", "nginx"}, "POST /v1/services", []string{`Service "web" created`}, nil},
		{[]string{"service", "create", "--name", "web", "--image", "nginx", "--json"}, "POST /v1/services", []string{`"status": "running"`}, nil},
		{[]string{"service", "delete", "web", "--confirm"}, "DELETE /v1/services/" + fakeSvcID, nil, nil},
		{[]string{"service", "start", "web"}, "POST /v1/services/" + fakeSvcID + "/start", []string{"Service started."}, nil},
		{[]string{"service", "stop", "web"}, "POST /v1/services/" + fakeSvcID + "/stop", []string{"Service stopped."}, nil},
		{[]string{"service", "restart", "web"}, "POST /v1/services/" + fakeSvcID + "/restart", []string{"Service restarted."}, nil},
		{[]string{"service", "reset", "web"}, "POST /v1/services/" + fakeSvcID + "/reset", []string{"Circuit breaker reset."}, nil},
		{[]string{"service", "redeploy", "web"}, "POST /v1/services/" + fakeSvcID + "/redeploy", []string{"redeployed"}, nil},
		{[]string{"service", "redeploy", "web", "--json"}, "POST /v1/services/" + fakeSvcID + "/redeploy", []string{`"name": "web"`}, nil},
		{[]string{"service", "rename", "web", "renamed"}, "PATCH /v1/services/" + fakeSvcID, []string{`renamed to "renamed"`}, nil},
		{[]string{"service", "rename", "web", "renamed", "--json"}, "PATCH /v1/services/" + fakeSvcID, []string{`"name": "renamed"`}, nil},
		{[]string{"service", "logs", "web"}, "GET /v1/services/" + fakeSvcID + "/logs", []string{"log line"}, nil},
		{[]string{"logs", "web", "--tail", "5"}, "GET /v1/services/" + fakeSvcID + "/logs", []string{"log line"}, nil},
		{[]string{"service", "deployments", "web"}, "GET /v1/services/" + fakeSvcID + "/deployments", []string{"dep-0123", "succeeded", "iii..."}, []string{strings.Repeat("i", 50)}},
		{[]string{"service", "deployments", "web", "--json"}, "GET /v1/services/" + fakeSvcID + "/deployments", []string{`"trigger": "api"`}, nil},
		{[]string{"service", "env", "get", "web"}, "GET /v1/services/" + fakeSvcID + "/env", []string{"A", "***"}, []string{"  1\n"}},
		{[]string{"service", "env", "get", "web", "--reveal"}, "GET /v1/services/" + fakeSvcID + "/env", []string{"A", "1"}, nil},
		{[]string{"service", "env", "get", "web", "--json"}, "GET /v1/services/" + fakeSvcID + "/env", []string{`"A": "1"`}, nil},
		{[]string{"service", "env", "set", "web", "A=1", "B=x=y"}, "POST /v1/services/" + fakeSvcID + "/env", []string{"2 variables updated"}, nil},
		{[]string{"service", "env", "set", "web", "A=1"}, "POST /v1/services/" + fakeSvcID + "/env", []string{"1 variable updated"}, nil},
		{[]string{"service", "env", "delete", "web", "A"}, "DELETE /v1/services/" + fakeSvcID + "/env/A", []string{`Variable "A" deleted`}, nil},
		{[]string{"build", "logs", "web"}, "GET /v1/services/" + fakeSvcID + "/builds/build-newest/logs", []string{"build log for build-newest"}, nil},
		{[]string{"build", "logs", "web", "b2"}, "GET /v1/services/" + fakeSvcID + "/builds/b2/logs", []string{"build log for b2"}, nil},
		{[]string{"build", "cancel", "web", "b2"}, "DELETE /v1/services/" + fakeSvcID + "/builds/b2", nil, nil},
		{[]string{"db", "list"}, "GET /v1/databases", []string{"pg", "postgres", "bbbbbbbb"}, nil},
		{[]string{"db", "list", "--json"}, "GET /v1/databases", []string{`"type": "postgres"`}, nil},
		{[]string{"db", "get", "pg"}, "GET /v1/databases/" + fakeDbID, []string{"postgres://h:5432", "Username:", "DB Name:"}, []string{"secretpw"}},
		{[]string{"db", "get", "pg", "--json"}, "GET /v1/databases/" + fakeDbID, []string{`"name": "pg"`}, nil},
		{[]string{"db", "delete", "pg", "--confirm"}, "DELETE /v1/databases/" + fakeDbID, nil, nil},
		{[]string{"env", "list"}, "GET /v1/environments", []string{"dev", "cccccccc"}, nil},
		{[]string{"env", "list", "--json"}, "GET /v1/environments", []string{`"name": "dev"`}, nil},
		{[]string{"env", "get", "dev"}, "GET /v1/environments/" + fakeEnvID, []string{"Status:", "running"}, nil},
		{[]string{"env", "get", "dev", "--json"}, "GET /v1/environments/" + fakeEnvID, []string{`"status": "running"`}, nil},
		{[]string{"env", "start", "dev"}, "POST /v1/environments/" + fakeEnvID + "/start", []string{"started"}, nil},
		{[]string{"env", "stop", "dev"}, "POST /v1/environments/" + fakeEnvID + "/stop", []string{"stopped"}, nil},
		{[]string{"env", "templates"}, "GET /v1/environments/templates", []string{"tmpl-go"}, nil},
		{[]string{"env", "templates", "--json"}, "GET /v1/environments/templates", []string{`"id": "tmpl-go"`}, nil},
		{[]string{"snapshot", "list"}, "GET /v1/snapshots", []string{"snap", "dddddddd"}, nil},
		{[]string{"snapshot", "list", "--json"}, "GET /v1/snapshots", []string{`"name": "snap"`}, nil},
		{[]string{"snapshot", "take", "web", "--name", "x"}, "POST /v1/services/" + fakeSvcID + "/snapshots", []string{"(id: s1)"}, nil},
		{[]string{"status"}, "GET /v1/system/health/detailed", []string{"System Health", "web"}, nil},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			f := newFakeAPI(t)
			out, _, err := runRoot(t, tc.args...)
			require.NoError(t, err)
			assert.True(t, f.called(tc.call), "expected call %q, got %v", tc.call, f.calls)
			for _, s := range tc.contains {
				assert.Contains(t, out, s)
			}
			for _, s := range tc.absent {
				assert.NotContains(t, out, s)
			}
		})
	}
}

func TestCommands_DestructiveRequireConfirm(t *testing.T) {
	for _, args := range [][]string{
		{"service", "delete", "web"},
		{"db", "delete", "pg"},
		{"env", "delete", "dev"},
		{"snapshot", "delete", "snap"},
		{"key", "revoke", "k1"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			f := newFakeAPI(t)
			_, _, err := runRoot(t, args...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "--confirm")
			for _, c := range f.calls {
				assert.False(t, strings.HasPrefix(c, "DELETE"), "no delete may be sent without --confirm: %s", c)
			}
		})
	}
}

func TestEnvExecCmd_RequiresCommand(t *testing.T) {
	f := newFakeAPI(t)
	_, _, err := runRoot(t, "env", "exec", "dev")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no command given")
	assert.Empty(t, f.calls, "nothing should be sent without a command")
}

func TestStatusCmd_UnhealthyReturnsExitCodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/system/health/detailed":
			_ = json.NewEncoder(w).Encode(ahclient.DetailedHealthResponse{Status: "ok",
				Docker: ahclient.DockerInfo{Available: true}, GVisor: ahclient.GVisorInfo{Available: false}})
		default:
			_, _ = io.WriteString(w, "[]")
		}
	}))
	defer srv.Close()
	isolateHome(t)
	t.Setenv("AH_URL", srv.URL)

	out, _, err := runRoot(t, "status")
	var ec *exitCodeError
	require.True(t, errors.As(err, &ec), "unhealthy status must surface as exitCodeError, not os.Exit")
	assert.Equal(t, 1, ec.ExitCode())
	assert.Contains(t, out, "unavailable")
}

func TestShortID(t *testing.T) {
	assert.Equal(t, "", shortID(""))
	assert.Equal(t, "abc", shortID("abc"))
	assert.Equal(t, "12345678", shortID("1234567890"))
}

func TestRunDeployImage_ShortServiceIDDoesNotPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ahclient.Service{ID: "s1", Name: "web", Status: "running"})
	}))
	defer srv.Close()
	var out bytes.Buffer
	require.NoError(t, runDeployImage(newTestClient(srv), &out, "nginx", "web", 80))
	assert.Contains(t, out.String(), "(id: s1)")
}

func TestStreamBuildLogs_LongLines(t *testing.T) {
	long := strings.Repeat("x", 200*1024)
	var out bytes.Buffer
	streamBuildLogs(strings.NewReader("first\n"+long+"\nlast\n"), &out)
	assert.Contains(t, out.String(), "[build] first\n")
	assert.Contains(t, out.String(), "[build] last\n", "lines after a >64KiB line must still be shown")
}

func TestStreamBuildLogs_ReportsReadError(t *testing.T) {
	var out bytes.Buffer
	streamBuildLogs(io.MultiReader(strings.NewReader("a\n"), errReader{}), &out)
	assert.Contains(t, out.String(), "log stream interrupted")
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("conn reset") }

func TestPollBuildUntilComplete_TerminalStatuses(t *testing.T) {
	for _, st := range []string{"succeeded", "failed", "cancelled"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(ahclient.Build{ID: "b", Status: st})
		}))
		b, err := pollBuildUntilComplete(newTestClient(srv), "s", "b")
		srv.Close()
		require.NoError(t, err)
		assert.Equal(t, st, b.Status)
	}
}

func TestMaskConnString_UnparseableMasksEverything(t *testing.T) {
	for _, in := range []string{
		"host=db port=5432 user=u password=secret dbname=d",
		"postgres://u:p%zzsecret@h:5432/d", // invalid escape in password
	} {
		got := maskConnString(in)
		assert.NotContains(t, got, "secret", in)
		assert.Equal(t, "***", got)
	}
}

func TestMaskKey(t *testing.T) {
	assert.Equal(t, "***", maskKey("short"))
	assert.Equal(t, "ak_12345", maskKey("ak_123456789"))
}

func TestVersionInfoDefaults(t *testing.T) {
	v, c, d := versionInfo()
	if Version == "" {
		assert.Equal(t, "dev", v)
	}
	if Commit == "" {
		assert.Equal(t, "unknown", c)
	}
	if Date == "" {
		assert.Equal(t, "unknown", d)
	}
}
