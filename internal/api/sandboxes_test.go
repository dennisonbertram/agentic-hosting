package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/environments"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newSandboxTestHelper(t *testing.T) *envTestHelper {
	t.Helper()
	h := newEnvTestHelper(t)
	h.envMgr.SetSandboxDiskCheck(func() error { return nil })
	return h
}

func (h *envTestHelper) createSandbox(t *testing.T, body string) environments.Sandbox {
	t.Helper()
	rr := h.doRequest(t, http.MethodPost, "/v1/sandboxes", []byte(body))
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	var s environments.Sandbox
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &s))
	return s
}

func TestSandboxAPI_CreateGetListDelete(t *testing.T) {
	h := newSandboxTestHelper(t)
	s := h.createSandbox(t, `{"source":{"type":"template","template":"default"},"ttl_seconds":600}`)
	assert.Equal(t, "running", s.Status)
	assert.Equal(t, 600, s.TTLSeconds)
	assert.NotZero(t, s.ExpiresAt)

	rr := h.doRequest(t, http.MethodGet, "/v1/sandboxes/"+s.ID, nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &raw))
	for _, k := range []string{"id", "name", "status", "source", "image", "run", "egress", "memory_mb", "cpu_millicores", "created_at", "expires_at", "max_expires_at", "last_activity_at"} {
		assert.Contains(t, raw, k)
	}
	assert.NotContains(t, rr.Body.String(), "container")
	assert.NotContains(t, rr.Body.String(), "volume")
	assert.NotContains(t, rr.Body.String(), "tenant")

	rr = h.doRequest(t, http.MethodGet, "/v1/sandboxes", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var list []environments.Sandbox
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &list))
	require.Len(t, list, 1)

	rr = h.doRequest(t, http.MethodGet, "/v1/sandboxes?status=bogus", nil)
	assert.Equal(t, http.StatusBadRequest, rr.Code)

	// Kind scoping on the environment routes.
	rr = h.doRequest(t, http.MethodGet, "/v1/environments/"+s.ID, nil)
	assert.Equal(t, http.StatusNotFound, rr.Code)
	rr = h.doRequest(t, http.MethodGet, "/v1/environments", nil)
	assert.JSONEq(t, `[]`, rr.Body.String())

	rr = h.doRequest(t, http.MethodDelete, "/v1/sandboxes/"+s.ID, nil)
	assert.Equal(t, http.StatusNoContent, rr.Code)
	rr = h.doRequest(t, http.MethodGet, "/v1/sandboxes/"+s.ID, nil)
	assert.Equal(t, http.StatusNotFound, rr.Code)
	rr = h.doRequest(t, http.MethodGet, "/v1/sandboxes", nil)
	assert.JSONEq(t, `[]`, rr.Body.String(), "empty list is an array, not null")
}

func TestSandboxAPI_EnvironmentIDNotFoundOnSandboxRoutes(t *testing.T) {
	h := newSandboxTestHelper(t)
	envID := seedRunningEnv(t, h.stateDB, "tenant-1", "dev")
	for _, rq := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/sandboxes/" + envID, ""},
		{http.MethodDelete, "/v1/sandboxes/" + envID, ""},
		{http.MethodPost, "/v1/sandboxes/" + envID + "/exec", `{"command":["ls"]}`},
		{http.MethodPost, "/v1/sandboxes/" + envID + "/extend", `{"ttl_seconds":600}`},
		{http.MethodGet, "/v1/sandboxes/" + envID + "/logs", ""},
	} {
		var body []byte
		if rq.body != "" {
			body = []byte(rq.body)
		}
		rr := h.doRequest(t, rq.method, rq.path, body)
		assert.Equal(t, http.StatusNotFound, rr.Code, rq.method+" "+rq.path)
	}
}

func TestSandboxAPI_CreateErrors(t *testing.T) {
	h := newSandboxTestHelper(t)
	cases := []struct {
		body string
		code int
	}{
		{`{"source":{"type":"template"},"egress":"internet"}`, http.StatusForbidden},
		{`{"source":{"type":"image","image":"ghcr.io/x/y"}}`, http.StatusBadRequest},
		{`{"source":{"type":"snapshot","snapshot_id":"x"}}`, http.StatusServiceUnavailable},
		{`{"source":{"type":"template"},"ttl_seconds":5}`, http.StatusBadRequest},
		{`{"source":`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		rr := h.doRequest(t, http.MethodPost, "/v1/sandboxes", []byte(tc.body))
		assert.Equal(t, tc.code, rr.Code, tc.body+" -> "+rr.Body.String())
	}
}

func TestSandboxAPI_CreateIsIdempotent(t *testing.T) {
	h := newSandboxTestHelper(t)
	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", strings.NewReader(`{"source":{"type":"template"}}`))
		req.Header.Set("Authorization", "Bearer "+h.token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "sbx-key-1")
		rr := httptest.NewRecorder()
		h.srv.ServeHTTP(rr, req)
		return rr
	}
	first, second := do(), do()
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	require.Equal(t, http.StatusCreated, second.Code)
	assert.Equal(t, first.Body.String(), second.Body.String())
	assert.Equal(t, "true", second.Header().Get("Idempotency-Replayed"))
	assert.Equal(t, 1, h.mockDocker.RunEnvironmentCalls)
}

func TestSandboxAPI_ExecIsNotIdempotentAndExtendsDeadline(t *testing.T) {
	h := newSandboxTestHelper(t)
	s := h.createSandbox(t, `{"source":{"type":"template"}}`)
	runs := 0
	h.mockDocker.ExecCreateFn = func(context.Context, string, []string, string) (string, error) { return "e1", nil }
	h.mockDocker.ExecRunFn = func(context.Context, string, time.Duration) ([]byte, []byte, int, error) {
		runs++
		return []byte("out"), nil, 2, nil
	}
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+s.ID+"/exec", strings.NewReader(`{"command":["false"],"timeout":300}`))
		req.Header.Set("Authorization", "Bearer "+h.token)
		req.Header.Set("Idempotency-Key", "same-key")
		w := &deadlineRecordingWriter{ResponseWriter: httptest.NewRecorder()}
		started := time.Now()
		h.srv.ServeHTTP(w, req)
		rec := w.ResponseWriter.(*httptest.ResponseRecorder)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp environments.ExecResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, 2, resp.ExitCode)
		assert.GreaterOrEqual(t, w.deadline.Sub(started), 300*time.Second)
	}
	assert.Equal(t, 2, runs, "exec runs every time even with the same Idempotency-Key")
}

func TestSandboxAPI_Extend(t *testing.T) {
	h := newSandboxTestHelper(t)
	s := h.createSandbox(t, `{"source":{"type":"template"},"ttl_seconds":120}`)
	rr := h.doRequest(t, http.MethodPost, "/v1/sandboxes/"+s.ID+"/extend", []byte(`{"ttl_seconds":3600}`))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var got environments.Sandbox
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	assert.Greater(t, got.ExpiresAt, s.ExpiresAt)
	rr = h.doRequest(t, http.MethodPost, "/v1/sandboxes/"+s.ID+"/extend", []byte(`{"ttl_seconds":0}`))
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestSandboxAPI_LogsClampTail(t *testing.T) {
	h := newSandboxTestHelper(t)
	s := h.createSandbox(t, `{"source":{"type":"template"}}`)
	var gotTail int
	var gotFollow bool
	h.mockDocker.LogsContainerFn = func(_ context.Context, _ string, follow bool, tail int) (io.ReadCloser, error) {
		gotTail, gotFollow = tail, follow
		return io.NopCloser(bytes.NewReader([]byte("hello logs"))), nil
	}
	rr := h.doRequest(t, http.MethodGet, "/v1/sandboxes/"+s.ID+"/logs?tail=50000&follow=true", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "hello logs", rr.Body.String())
	assert.Equal(t, 10000, gotTail)
	assert.True(t, gotFollow)
	rr = h.doRequest(t, http.MethodGet, "/v1/sandboxes/"+s.ID+"/logs", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, 100, gotTail)
	rr = h.doRequest(t, http.MethodGet, "/v1/sandboxes/"+s.ID+"/logs?tail=abc", nil)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestSandboxAPI_CreateRateLimited(t *testing.T) {
	h := newSandboxTestHelper(t)
	_, err := h.stateDB.Exec(`UPDATE tenant_quotas SET max_sandboxes = 50, api_rate_limit = 1000`)
	require.NoError(t, err)
	limited := 0
	for i := 0; i < sandboxCreateBurst+3; i++ {
		rr := h.doRequest(t, http.MethodPost, "/v1/sandboxes", []byte(`{"source":{"type":"template"}}`))
		if rr.Code == http.StatusTooManyRequests {
			limited++
			assert.NotEmpty(t, rr.Header().Get("Retry-After"))
		} else {
			require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
		}
	}
	assert.GreaterOrEqual(t, limited, 2, "creates beyond the burst are rejected despite a high tenant api_rate_limit")
	// Reads are not affected by the create limiter.
	rr := h.doRequest(t, http.MethodGet, "/v1/sandboxes", nil)
	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestSandboxAPI_RequiresAuth(t *testing.T) {
	h := newSandboxTestHelper(t)
	for _, rq := range []struct{ method, path string }{
		{http.MethodPost, "/v1/sandboxes"},
		{http.MethodGet, "/v1/sandboxes"},
		{http.MethodGet, "/v1/sandboxes/x"},
		{http.MethodDelete, "/v1/sandboxes/x"},
		{http.MethodPost, "/v1/sandboxes/x/exec"},
		{http.MethodGet, "/v1/sandboxes/x/logs"},
		{http.MethodPost, "/v1/sandboxes/x/extend"},
	} {
		req := httptest.NewRequest(rq.method, rq.path, strings.NewReader(`{}`))
		rr := httptest.NewRecorder()
		h.srv.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusUnauthorized, rr.Code, rq.method+" "+rq.path)
	}
}

func TestSandboxAPI_NilManagerReturns503(t *testing.T) {
	srv, token := newServerWithoutEnvManager(t)
	for _, rq := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/sandboxes", `{"source":{"type":"template"}}`},
		{http.MethodGet, "/v1/sandboxes", ""},
		{http.MethodGet, "/v1/sandboxes/x", ""},
		{http.MethodDelete, "/v1/sandboxes/x", ""},
		{http.MethodPost, "/v1/sandboxes/x/exec", `{"command":["ls"]}`},
		{http.MethodGet, "/v1/sandboxes/x/logs", ""},
		{http.MethodPost, "/v1/sandboxes/x/extend", `{"ttl_seconds":60}`},
	} {
		var body []byte
		if rq.body != "" {
			body = []byte(rq.body)
		}
		rr := doAuthRequest(t, srv, token, rq.method, rq.path, body)
		assert.Equal(t, http.StatusServiceUnavailable, rr.Code, rq.method+" "+rq.path)
	}
}
