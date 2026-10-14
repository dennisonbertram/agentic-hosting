package builds

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/builder"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func insertBuild(t *testing.T, mgr *Manager, id, status string) {
	t.Helper()
	_, err := mgr.db.Exec(
		`INSERT INTO builds (id, service_id, tenant_id, status, source_type, source_url, source_ref, image, log, created_at)
		 VALUES (?, 'svc-1', 'tenant-1', ?, 'git', 'https://github.com/example/repo.git', 'main', 'img:latest', '', 1)`, id, status)
	require.NoError(t, err)
}

// A cancel that arrives while the builder has no systemd unit tracked (builder.CancelBuild
// errors) must still stop the build and keep it from deploying.
func TestCancelBuild_StopsBuildAndSkipsDeploy(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedBuildTestData(t, stateDB)

	started := make(chan struct{})
	finished := make(chan struct{})
	var deployed atomic.Bool
	sb := &stubBuilder{
		cancelFn: func(string) error { return errors.New("build not found or not running") },
		buildFn: func(ctx context.Context, req builder.BuildRequest, logCb func(string)) error {
			close(started)
			defer close(finished)
			select {
			case <-ctx.Done():
			case <-time.After(3 * time.Second):
			}
			return nil // a builder that finishes "successfully" despite the cancel
		},
	}
	mgr := NewManager(stateDB, sb, func(ctx context.Context, tenantID, serviceID, imageTag, buildID string) error {
		deployed.Store(true)
		return nil
	})

	build, err := mgr.StartBuild(context.Background(), "tenant-1", "svc-1", StartBuildRequest{
		SourceType: "git", SourceURL: "https://github.com/example/repo.git", SourceRef: "main",
	})
	require.NoError(t, err)
	<-started

	require.NoError(t, mgr.CancelBuild(context.Background(), "tenant-1", build.ID))

	select {
	case <-finished:
	case <-time.After(1 * time.Second):
		t.Fatal("cancel did not cancel the build context")
	}
	time.Sleep(200 * time.Millisecond) // let runBuild pass the point where it would deploy
	assert.False(t, deployed.Load(), "cancelled build must not deploy")
	assert.Equal(t, "failed", getBuildStatus(t, stateDB, build.ID))
	assert.Eventually(t, func() bool {
		mgr.logMu.Lock()
		defer mgr.logMu.Unlock()
		return len(mgr.logStates) == 0 && len(mgr.cancels) == 0
	}, 2*time.Second, 10*time.Millisecond, "per-build state must be freed")
}

func TestCancelActive_DoesNotOverwriteSucceeded(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedBuildTestData(t, stateDB)
	mgr := NewManager(stateDB, &stubBuilder{}, nil)
	insertBuild(t, mgr, "b-done", "succeeded")

	ok, _ := mgr.cancelActive(context.Background(), "b-done", "failed", "[ah] Build cancelled by user")
	assert.False(t, ok)
	assert.Equal(t, "succeeded", getBuildStatus(t, stateDB, "b-done"))
	logs, err := mgr.GetBuildLogs(context.Background(), "tenant-1", "b-done")
	require.NoError(t, err)
	assert.NotContains(t, logs, "cancelled")
}

func TestRunBuild_LateLogCallbackDoesNotRecreateState(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedBuildTestData(t, stateDB)

	var late func(string)
	mgr := NewManager(stateDB, &stubBuilder{
		buildFn: func(ctx context.Context, req builder.BuildRequest, logCb func(string)) error {
			late = logCb
			return nil
		},
	}, nil)
	build, err := mgr.StartBuild(context.Background(), "tenant-1", "svc-1", StartBuildRequest{
		SourceType: "git", SourceURL: "https://github.com/example/repo.git", SourceRef: "main",
	})
	require.NoError(t, err)
	waitForBuildStatus(t, stateDB, build.ID, "succeeded")
	assert.Eventually(t, func() bool {
		mgr.logMu.Lock()
		defer mgr.logMu.Unlock()
		return len(mgr.logStates) == 0
	}, 2*time.Second, 10*time.Millisecond)

	late("straggler output")
	mgr.logMu.Lock()
	n := len(mgr.logStates)
	mgr.logMu.Unlock()
	assert.Equal(t, 0, n, "late log line must not recreate log state")
}

func TestStreamBuildLogs_UnsubscribesWhenContextCancelled(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedBuildTestData(t, stateDB)
	mgr := NewManager(stateDB, &stubBuilder{}, nil)
	insertBuild(t, mgr, "b-run", "running")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.StreamBuildLogs(ctx, "tenant-1", "b-run", &strings.Builder{}) }()
	assert.Eventually(t, func() bool {
		mgr.logMu.Lock()
		defer mgr.logMu.Unlock()
		return len(mgr.logSubs["b-run"]) == 1
	}, 2*time.Second, 5*time.Millisecond)
	cancel()
	require.Error(t, <-done)

	mgr.logMu.Lock()
	defer mgr.logMu.Unlock()
	assert.Empty(t, mgr.logSubs["b-run"], "abandoned subscriber must be removed")
}

// A build that is already finished must not leave the stream waiting or a subscriber behind.
func TestStreamBuildLogs_FinishedBuildReturnsAndLeavesNoSubscriber(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedBuildTestData(t, stateDB)
	mgr := NewManager(stateDB, &stubBuilder{}, nil)
	insertBuild(t, mgr, "b-cancelled", "cancelled")

	var sb strings.Builder
	require.NoError(t, mgr.StreamBuildLogs(context.Background(), "tenant-1", "b-cancelled", &sb))
	mgr.logMu.Lock()
	defer mgr.logMu.Unlock()
	assert.Empty(t, mgr.logSubs["b-cancelled"])
}

func TestAppendLog_BuffersUntilFlushOrThreshold(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedBuildTestData(t, stateDB)
	mgr := NewManager(stateDB, &stubBuilder{}, nil)
	insertBuild(t, mgr, "b-buf", "running")
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		mgr.appendLog(ctx, "b-buf", "small line")
	}
	logs, err := mgr.GetBuildLogs(ctx, "tenant-1", "b-buf")
	require.NoError(t, err)
	assert.Empty(t, logs, "small lines should be buffered, not written per line")

	mgr.appendLog(ctx, "b-buf", strings.Repeat("Y", flushBytes))
	logs, err = mgr.GetBuildLogs(ctx, "tenant-1", "b-buf")
	require.NoError(t, err)
	assert.Equal(t, 10, strings.Count(logs, "small line"), "crossing the size threshold flushes everything in order")

	mgr.appendLog(ctx, "b-buf", "last")
	mgr.flushTailBuffer(ctx, "b-buf")
	logs, err = mgr.GetBuildLogs(ctx, "tenant-1", "b-buf")
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(logs, "last\n"))
}
