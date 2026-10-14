package ahclient

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEnvironmentExecUsesTimeoutBeyondDefaultHTTPTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(60 * time.Millisecond)
		_, _ = fmt.Fprint(w, `{"exit_code":0,"stdout":"done","stderr":"","truncated":false,"timed_out":false,"duration_ms":60}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	c.HTTPClient.Timeout = 20 * time.Millisecond
	result, err := c.EnvironmentExec("env-1", []string{"sleep", "1"}, "", 1)

	require.NoError(t, err)
	require.Equal(t, "done", result.Stdout)
}

func TestEnvironmentExecClientTimeoutUsesDefaultAndServerMaximum(t *testing.T) {
	require.Equal(t, 65*time.Second, environmentExecClientTimeout(0))
	require.Equal(t, 65*time.Second, environmentExecClientTimeout(60))
	require.Equal(t, 305*time.Second, environmentExecClientTimeout(900))
}
