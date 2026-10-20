package ahclient_test

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func recordingServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.EscapedPath())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), paths...) }
}

// Dot-segment arguments must never reach the server: a path-cleaning proxy
// would turn ".../builds/../../../tenant" into DELETE /v1/tenant.
func TestClient_RejectsDotSegmentIDs(t *testing.T) {
	srv, got := recordingServer(t)
	c := ahclient.NewClient(srv.URL, "k")

	require.Error(t, c.RevokeKey("../../tenant"))
	_, err := c.CancelBuild("svc", "../../../tenant")
	require.Error(t, err)
	_, err = c.GetBuild("svc", "..")
	require.Error(t, err)
	require.Error(t, c.DeleteSnapshot(""))
	assert.Empty(t, got())
}

func TestClient_EscapesPathSegments(t *testing.T) {
	srv, got := recordingServer(t)
	c := ahclient.NewClient(srv.URL, "k")

	require.NoError(t, c.DeleteServiceEnv("svc", "A?b#c"))
	require.Error(t, c.RevokeKey("a/b"), "escaped slashes are rejected too")
	assert.Equal(t, []string{"DELETE /v1/services/svc/env/A%3Fb%23c"}, got())
}
