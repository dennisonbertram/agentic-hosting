package builds

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Once the build is deploying, a cancel must be refused: the deploy cannot be
// stopped, so reporting "cancelled" would be false.
func TestCancelBuild_RefusedWhileDeploying(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedBuildTestData(t, stateDB)

	deployStarted := make(chan struct{})
	release := make(chan struct{})
	mgr := NewManager(stateDB, &stubBuilder{}, func(ctx context.Context, tenantID, serviceID, imageTag, buildID string) error {
		close(deployStarted)
		<-release
		return nil
	})
	build, err := mgr.StartBuild(context.Background(), "tenant-1", "svc-1", StartBuildRequest{
		SourceType: "git", SourceURL: "https://github.com/example/repo.git", SourceRef: "main",
	})
	require.NoError(t, err)
	<-deployStarted

	err = mgr.CancelBuild(context.Background(), "tenant-1", build.ID)
	require.Error(t, err)
	var ae *apierr.APIError
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, 409, ae.Code)
	assert.Contains(t, err.Error(), "deploying")
	assert.Equal(t, "running", getBuildStatus(t, stateDB, build.ID), "refused cancel leaves the build untouched")

	close(release)
	waitForBuildStatus(t, stateDB, build.ID, "succeeded")
}

type syncBuf struct {
	mu sync.Mutex
	b  []byte
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b = append(s.b, p...)
	return len(p), nil
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.b)
}

// A line accepted before the stream's snapshot read, but whose live notification
// arrives afterwards, must not be shown twice.
func TestStreamBuildLogs_NoDuplicateForLineInSnapshotAndChannel(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedBuildTestData(t, stateDB)
	mgr := NewManager(stateDB, &stubBuilder{}, nil)
	insertBuild(t, mgr, "b-dup", "running")
	ctx := context.Background()

	mgr.appendLog(ctx, "b-dup", "X")
	out := &syncBuf{}
	done := make(chan error, 1)
	go func() { done <- mgr.StreamBuildLogs(ctx, "tenant-1", "b-dup", out) }()
	require.Eventually(t, func() bool { return out.String() == "X\n" }, 2*time.Second, 5*time.Millisecond)

	mgr.notifyLogSubs("b-dup", "X") // late notification for the line already in the snapshot
	mgr.appendLog(ctx, "b-dup", "Y")
	mgr.notifyLogSubs("b-dup", "Y")
	mgr.closeLogSubs("b-dup")
	require.NoError(t, <-done)
	assert.Equal(t, "X\nY\n", out.String())
}
