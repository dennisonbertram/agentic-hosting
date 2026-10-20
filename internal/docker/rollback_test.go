package docker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/docker/docker/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A Run* rollback must still remove the half-created container when the
// caller's context is already cancelled (the usual reason the run failed).
func TestRollbackRemove_UsesFreshContextWhenCallerCancelled(t *testing.T) {
	var removed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/containers/abc123") {
			assert.Equal(t, "1", r.URL.Query().Get("force"))
			removed.Store(true)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cli, err := client.NewClientWithOpts(client.WithHost(server.URL), client.WithVersion("1.45"))
	require.NoError(t, err)
	defer cli.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	(&DockerClient{cli: cli}).rollbackRemove(ctx, "abc123")
	assert.True(t, removed.Load(), "rollback must reach the daemon despite the cancelled caller context")
}
