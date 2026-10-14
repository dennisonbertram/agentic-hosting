package docker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/client"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errSyntheticStream = errors.New("synthetic stream failure")

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errSyntheticStream }

func TestConsumeImagePullResponse(t *testing.T) {
	t.Run("progress stream succeeds", func(t *testing.T) {
		err := consumeImagePullResponse(context.Background(), strings.NewReader("{\"status\":\"pulling\"}\n{\"status\":\"complete\"}\n"))
		require.NoError(t, err)
	})
	t.Run("daemon error is returned without response text", func(t *testing.T) {
		const marker = "synthetic-private-registry-token"
		err := consumeImagePullResponse(context.Background(), strings.NewReader("{\"error\":\"denied\",\"errorDetail\":{\"message\":\""+marker+"\"}}\n"))
		require.Error(t, err)
		require.NotContains(t, err.Error(), marker)
	})
	t.Run("reader failure propagates", func(t *testing.T) {
		err := consumeImagePullResponse(context.Background(), failingReader{})
		require.Error(t, err)
		require.ErrorIs(t, err, errSyntheticStream)
	})
	t.Run("canceled context aborts stream processing", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := consumeImagePullResponse(ctx, strings.NewReader("{\"status\":\"pulling\"}\n"))
		require.ErrorIs(t, err, context.Canceled)
	})
	t.Run("empty stream is rejected", func(t *testing.T) {
		err := consumeImagePullResponse(context.Background(), io.LimitReader(strings.NewReader(""), 1))
		require.Error(t, err)
	})
}

func TestPullImageHonorsCallerCancellationDuringStream(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{\"status\":\"pulling\"}\n")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()

	cli, err := client.NewClientWithOpts(client.WithHost(server.URL), client.WithVersion("1.45"))
	require.NoError(t, err)
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- (&DockerClient{cli: cli}).PullImage(ctx, "example.invalid/test:latest") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("pull request did not reach the test server")
	}
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(time.Second):
		t.Fatal("PullImage did not stop after caller deadline")
	}
}

func TestDrainPullStream_ReturnsStreamError(t *testing.T) {
	stream := `{"status":"Pulling from library/x"}` + "\n" +
		`{"errorDetail":{"message":"manifest unknown"},"error":"manifest unknown"}` + "\n"
	err := drainPullStream(strings.NewReader(stream))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "manifest unknown")
}

func TestDrainPullStream_OKStream(t *testing.T) {
	assert.NoError(t, drainPullStream(strings.NewReader(`{"status":"Download complete"}`+"\n")))
}

func TestDrainPullStream_ReturnsReadError(t *testing.T) {
	assert.Error(t, drainPullStream(io.MultiReader(strings.NewReader(`{"status":"x"}`+"\n"), errReader{})))
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, context.DeadlineExceeded }

// Integration: skips without a reachable Docker daemon.
func TestPullImage_NonexistentImageErrors(t *testing.T) {
	c, err := NewClient()
	if err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := c.cli.Ping(ctx); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	assert.Error(t, c.PullImage(ctx, "127.0.0.1:1/ah-nonexistent/none:none"))
}
