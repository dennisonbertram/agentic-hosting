package docker

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
