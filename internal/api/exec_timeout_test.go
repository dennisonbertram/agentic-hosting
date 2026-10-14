package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type deadlineRecordingWriter struct {
	http.ResponseWriter
	deadline time.Time
}

func (w *deadlineRecordingWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}

func TestEnvironmentExecExtendsOnlyItsResponseDeadline(t *testing.T) {
	h := newEnvTestHelper(t)
	envID := seedRunningEnv(t, h.stateDB, "tenant-1", "exec-timeout")
	h.mockDocker.ExecCreateFn = func(context.Context, string, []string, string) (string, error) {
		return "exec-timeout-id", nil
	}
	h.mockDocker.ExecRunFn = func(context.Context, string, time.Duration) ([]byte, []byte, int, error) {
		return []byte("ok"), nil, 0, nil
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/environments/"+envID+"/exec", strings.NewReader(`{"command":["true"],"timeout":300}`))
	request.Header.Set("Authorization", "Bearer "+h.token)
	w := &deadlineRecordingWriter{ResponseWriter: httptest.NewRecorder()}
	started := time.Now()
	h.srv.ServeHTTP(w, request)

	require.Equal(t, http.StatusOK, w.ResponseWriter.(*httptest.ResponseRecorder).Code)
	require.NotZero(t, w.deadline, "exec handler should set a response deadline")
	require.GreaterOrEqual(t, w.deadline.Sub(started), 300*time.Second)
}

func TestEnvironmentExecResponseDeadlineUsesDefaultAndMaximum(t *testing.T) {
	start := time.Now()
	require.Equal(t, 65*time.Second, environmentExecResponseDeadline(start, 0).Sub(start))
	require.Equal(t, 65*time.Second, environmentExecResponseDeadline(start, 60).Sub(start))
	require.Equal(t, 305*time.Second, environmentExecResponseDeadline(start, 900).Sub(start))
}

func TestEnvironmentExecResponseSurvivesShortServerWriteTimeout(t *testing.T) {
	h := newEnvTestHelper(t)
	envID := seedRunningEnv(t, h.stateDB, "tenant-1", "exec-server-deadline")
	h.mockDocker.ExecCreateFn = func(context.Context, string, []string, string) (string, error) {
		return "exec-server-deadline-id", nil
	}
	h.mockDocker.ExecRunFn = func(context.Context, string, time.Duration) ([]byte, []byte, int, error) {
		time.Sleep(60 * time.Millisecond)
		return []byte("completed"), nil, 0, nil
	}

	ts := httptest.NewUnstartedServer(h.srv)
	ts.Config.WriteTimeout = 20 * time.Millisecond
	ts.Start()
	defer ts.Close()

	request, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/environments/"+envID+"/exec", strings.NewReader(`{"command":["true"],"timeout":1}`))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+h.token)
	response, err := (&http.Client{Timeout: time.Second}).Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "completed")
}
