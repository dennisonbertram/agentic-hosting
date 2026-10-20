package builds

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/builder"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newBuildMgr(t *testing.T, b buildExecutor, deploy DeployFunc) *Manager {
	t.Helper()
	stateDB := testutil.NewStateDB(t)
	seedBuildTestData(t, stateDB)
	return NewManager(stateDB, b, deploy)
}

func gitReq(url, ref string) StartBuildRequest {
	return StartBuildRequest{SourceType: "git", SourceURL: url, SourceRef: ref}
}

func TestStartBuild_ValidatesInput(t *testing.T) {
	mgr := newBuildMgr(t, &stubBuilder{}, nil)
	ctx := context.Background()
	tests := map[string]StartBuildRequest{
		"non-git source":     {SourceType: "zip", SourceURL: "https://github.com/a/b"},
		"missing url":        {SourceType: "git"},
		"http url":           gitReq("http://github.com/a/b", "main"),
		"credentials in url": gitReq("https://tok@github.com/a/b", "main"),
		"untrusted host":     gitReq("https://evil.example/a/b", "main"),
		"overlong url":       gitReq("https://github.com/"+strings.Repeat("a", 2048), "main"),
		"bad ref chars":      gitReq("https://github.com/a/b", "main;rm"),
		"overlong ref":       gitReq("https://github.com/a/b", strings.Repeat("a", 257)),
		"empty host":         gitReq("https:///a/b", "main"),
		"unparseable url":    gitReq("https://github.com/%zz", "main"),
	}
	for name, req := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := mgr.StartBuild(ctx, "tenant-1", "svc-1", req)
			assert.True(t, errors.Is(err, apierr.ErrValidation), "got %v", err)
		})
	}

	_, err := mgr.StartBuild(ctx, "tenant-2", "svc-1", gitReq("https://github.com/a/b", "main"))
	assert.True(t, errors.Is(err, apierr.ErrNotFound), "another tenant's service is not found")
}

func TestStartBuild_DefaultsRefAndHostCase(t *testing.T) {
	started := make(chan builder.BuildRequest, 1)
	mgr := newBuildMgr(t, &stubBuilder{buildFn: func(_ context.Context, req builder.BuildRequest, _ func(string)) error {
		started <- req
		return nil
	}}, nil)
	b, err := mgr.StartBuild(context.Background(), "tenant-1", "svc-1", gitReq("https://GitHub.com/a/b", ""))
	require.NoError(t, err)
	assert.Equal(t, "main", b.SourceRef)
	assert.Equal(t, ImageTag("tenant-1", "svc-1", b.ID), b.Image)
	req := <-started
	assert.Equal(t, b.Image, req.ImageTag)
	waitForBuildStatus(t, mgr.db, b.ID, "succeeded")
}

func TestStartBuild_QueueFullFailsBuild(t *testing.T) {
	mgr := newBuildMgr(t, &stubBuilder{}, nil)
	for i := 0; i < cap(mgr.buildQueue); i++ {
		mgr.buildQueue <- struct{}{}
	}
	_, err := mgr.StartBuild(context.Background(), "tenant-1", "svc-1", gitReq("https://github.com/a/b", "main"))
	assert.True(t, errors.Is(err, apierr.ErrConflict))
	var n int
	require.NoError(t, mgr.db.QueryRow(`SELECT COUNT(*) FROM builds WHERE status = 'failed'`).Scan(&n))
	assert.Equal(t, 1, n, "the rejected build is not left pending")
}

func TestRunBuild_BuilderFailureMarksFailedAndClosesStreams(t *testing.T) {
	release := make(chan struct{})
	mgr := newBuildMgr(t, &stubBuilder{buildFn: func(_ context.Context, _ builder.BuildRequest, logCb func(string)) error {
		logCb("compiling")
		<-release
		return errors.New("nixpacks exploded")
	}}, func(context.Context, string, string, string, string) error {
		t.Error("a failed build must not deploy")
		return nil
	})
	b, err := mgr.StartBuild(context.Background(), "tenant-1", "svc-1", gitReq("https://github.com/a/b", "main"))
	require.NoError(t, err)
	waitForBuildStatus(t, mgr.db, b.ID, "running")

	var out syncBuf
	done := make(chan error, 1)
	go func() { done <- mgr.StreamBuildLogs(context.Background(), "tenant-1", b.ID, &out) }()
	close(release)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not end when the build failed")
	}
	waitForBuildStatus(t, mgr.db, b.ID, "failed")
	logs, err := mgr.GetBuildLogs(context.Background(), "tenant-1", b.ID)
	require.NoError(t, err)
	assert.Contains(t, logs, "compiling")
	assert.Contains(t, logs, "BUILD FAILED: nixpacks exploded")
}

func TestRunBuild_NotPendingAbortsWithoutBuilding(t *testing.T) {
	built := false
	mgr := newBuildMgr(t, &stubBuilder{buildFn: func(context.Context, builder.BuildRequest, func(string)) error {
		built = true
		return nil
	}}, nil)
	insertBuild(t, mgr, "b-cancelled", "failed")
	mgr.buildQueue <- struct{}{}
	mgr.runBuild("b-cancelled", "tenant-1", "svc-1", builder.BuildRequest{BuildID: "b-cancelled"})
	assert.False(t, built)
	assert.Empty(t, mgr.logSubs)
	assert.Equal(t, 0, len(mgr.buildQueue), "queue slot released")
}

func TestRunBuild_DBErrorDoesNotLeaveSubscribersHanging(t *testing.T) {
	mgr := newBuildMgr(t, &stubBuilder{}, nil)
	insertBuild(t, mgr, "b-1", "pending")
	ch := mgr.subscribeLogs("b-1")
	require.NoError(t, mgr.db.Close())
	mgr.buildQueue <- struct{}{}
	mgr.runBuild("b-1", "tenant-1", "svc-1", builder.BuildRequest{BuildID: "b-1"})
	_, open := <-ch
	assert.False(t, open, "log streams are closed even when the build never started")
}

func TestListBuilds(t *testing.T) {
	mgr := newBuildMgr(t, &stubBuilder{}, nil)
	ctx := context.Background()
	insertBuild(t, mgr, "b-1", "succeeded")
	insertBuild(t, mgr, "b-2", "failed")
	_, err := mgr.db.Exec(`UPDATE builds SET created_at = 2 WHERE id = 'b-2'`)
	require.NoError(t, err)

	list, err := mgr.ListBuilds(ctx, "tenant-1", "svc-1")
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "b-2", list[0].ID, "newest first")
	assert.Equal(t, "Service", list[0].ServiceName)
	assert.Empty(t, list[0].Log, "list responses omit logs")

	list, err = mgr.ListTenantBuilds(ctx, "tenant-1", 1)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "b-2", list[0].ID)

	list, err = mgr.ListTenantBuilds(ctx, "tenant-1", 0)
	require.NoError(t, err)
	assert.Len(t, list, 2)

	list, err = mgr.ListBuilds(ctx, "tenant-2", "svc-1")
	require.NoError(t, err)
	assert.Empty(t, list, "builds are tenant-scoped")
	_, err = mgr.GetBuild(ctx, "tenant-2", "b-1")
	assert.True(t, errors.Is(err, apierr.ErrNotFound))
	_, err = mgr.GetBuildLogs(ctx, "tenant-2", "b-1")
	assert.True(t, errors.Is(err, apierr.ErrNotFound))
}

func TestCancelBuild_Errors(t *testing.T) {
	mgr := newBuildMgr(t, &stubBuilder{}, nil)
	ctx := context.Background()
	insertBuild(t, mgr, "b-done", "succeeded")
	insertBuild(t, mgr, "b-pending", "pending")

	assert.True(t, errors.Is(mgr.CancelBuild(ctx, "tenant-1", "missing"), apierr.ErrNotFound))
	assert.True(t, errors.Is(mgr.CancelBuild(ctx, "tenant-1", "b-done"), apierr.ErrConflict))
	assert.True(t, errors.Is(mgr.CancelBuild(ctx, "tenant-2", "b-pending"), apierr.ErrNotFound), "another tenant cannot cancel")
	require.NoError(t, mgr.CancelBuild(ctx, "tenant-1", "b-pending"))
	assert.Equal(t, "failed", getBuildStatus(t, mgr.db, "b-pending"))
	logs, err := mgr.GetBuildLogs(ctx, "tenant-1", "b-pending")
	require.NoError(t, err)
	assert.Contains(t, logs, "cancelled by user")
}

func TestReconcileStaleBuildsOnStartup(t *testing.T) {
	mgr := newBuildMgr(t, &stubBuilder{}, nil)
	insertBuild(t, mgr, "b-run", "running")
	insertBuild(t, mgr, "b-ok", "succeeded")
	NewManager(mgr.db, &stubBuilder{}, nil)
	assert.Equal(t, "failed", getBuildStatus(t, mgr.db, "b-run"))
	assert.Equal(t, "succeeded", getBuildStatus(t, mgr.db, "b-ok"))
}
