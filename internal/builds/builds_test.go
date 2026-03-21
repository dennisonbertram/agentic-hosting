package builds

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/builder"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubBuilder struct {
	buildFn  func(ctx context.Context, req builder.BuildRequest, logCb func(string)) error
	cancelFn func(buildID string) error
}

func (s *stubBuilder) Build(ctx context.Context, req builder.BuildRequest, logCb func(string)) error {
	if s.buildFn != nil {
		return s.buildFn(ctx, req, logCb)
	}
	return nil
}

func (s *stubBuilder) CancelBuild(buildID string) error {
	if s.cancelFn != nil {
		return s.cancelFn(buildID)
	}
	return nil
}

func TestStartBuild_KeepsBuildRunningUntilDeployCompletes(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedBuildTestData(t, stateDB)

	deployStarted := make(chan struct{})
	releaseDeploy := make(chan struct{})
	mgr := NewManager(stateDB, &stubBuilder{}, func(ctx context.Context, tenantID, serviceID, imageTag string) error {
		close(deployStarted)
		<-releaseDeploy
		return nil
	})

	build, err := mgr.StartBuild(context.Background(), "tenant-1", "svc-1", StartBuildRequest{
		SourceType: "git",
		SourceURL:  "https://github.com/example/repo.git",
		SourceRef:  "main",
	})
	require.NoError(t, err)

	select {
	case <-deployStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for deploy to start")
	}

	current := getBuildStatus(t, stateDB, build.ID)
	assert.Equal(t, "running", current, "build should remain running until deploy finishes")

	close(releaseDeploy)
	waitForBuildStatus(t, stateDB, build.ID, "succeeded")
}

func TestStartBuild_MarksFailedWhenDeployFails(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedBuildTestData(t, stateDB)

	mgr := NewManager(stateDB, &stubBuilder{}, func(ctx context.Context, tenantID, serviceID, imageTag string) error {
		return errors.New("deploy boom")
	})

	build, err := mgr.StartBuild(context.Background(), "tenant-1", "svc-1", StartBuildRequest{
		SourceType: "git",
		SourceURL:  "https://github.com/example/repo.git",
		SourceRef:  "main",
	})
	require.NoError(t, err)

	waitForBuildStatus(t, stateDB, build.ID, "failed")

	logs, err := mgr.GetBuildLogs(context.Background(), "tenant-1", build.ID)
	require.NoError(t, err)
	assert.Contains(t, logs, "Deploy failed: deploy boom")
}

func seedBuildTestData(t *testing.T, stateDB *sql.DB) {
	t.Helper()
	if _, err := stateDB.Exec(
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at) VALUES (?, ?, ?, 'active', 1, 1)`,
		"tenant-1", "Tenant", "tenant@example.com",
	); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	if _, err := stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`, "tenant-1"); err != nil {
		t.Fatalf("insert quota: %v", err)
	}
	if _, err := stateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, created_at, updated_at) VALUES (?, ?, ?, 'created', ?, 8080, 1, 1)`,
		"svc-1", "tenant-1", "Service", "nginx:latest",
	); err != nil {
		t.Fatalf("insert service: %v", err)
	}
}

func getBuildStatus(t *testing.T, stateDB *sql.DB, buildID string) string {
	t.Helper()
	var status string
	if err := stateDB.QueryRow(`SELECT status FROM builds WHERE id = ?`, buildID).Scan(&status); err != nil {
		t.Fatalf("query build status: %v", err)
	}
	return status
}

func waitForBuildStatus(t *testing.T, stateDB *sql.DB, buildID, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := getBuildStatus(t, stateDB, buildID); got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for build %s to reach status %q", buildID, want)
}

func TestAppendLog_SmallLogPreservedInFull(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedBuildTestData(t, stateDB)

	mgr := NewManager(stateDB, &stubBuilder{}, nil)

	// Insert a build record directly
	buildID := "test-build-small"
	_, err := stateDB.Exec(
		`INSERT INTO builds (id, service_id, tenant_id, status, source_type, source_ref, image, log, created_at)
		 VALUES (?, 'svc-1', 'tenant-1', 'running', 'git', 'main', 'img:latest', '', 1)`,
		buildID,
	)
	require.NoError(t, err)

	ctx := context.Background()
	// Append a modest number of lines
	for i := 0; i < 100; i++ {
		mgr.appendLog(ctx, buildID, fmt.Sprintf("line %d: some build output", i))
	}
	mgr.flushTailBuffer(ctx, buildID)

	logs, err := mgr.GetBuildLogs(ctx, "tenant-1", buildID)
	require.NoError(t, err)

	// All 100 lines should be present — no truncation
	assert.NotContains(t, logs, "[truncated:")
	for i := 0; i < 100; i++ {
		assert.Contains(t, logs, fmt.Sprintf("line %d: some build output", i))
	}
}

func TestAppendLog_LargeLogTruncatedWithTail(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedBuildTestData(t, stateDB)

	mgr := NewManager(stateDB, &stubBuilder{}, nil)

	buildID := "test-build-large"
	_, err := stateDB.Exec(
		`INSERT INTO builds (id, service_id, tenant_id, status, source_type, source_ref, image, log, created_at)
		 VALUES (?, 'svc-1', 'tenant-1', 'running', 'git', 'main', 'img:latest', '', 1)`,
		buildID,
	)
	require.NoError(t, err)

	ctx := context.Background()

	// Use large lines (~10KB each) so we hit the 5MB cap in ~520 DB writes,
	// then write 600 more lines past the cap for the tail buffer.
	bigPadding := strings.Repeat("X", 10*1024) // 10KB per line
	linesBeforeCap := (maxLogSize / (10*1024 + 1)) + 1 // +1 for newline char
	linesAfterCap := 600
	totalLines := linesBeforeCap + linesAfterCap

	for i := 0; i < totalLines; i++ {
		mgr.appendLog(ctx, buildID, fmt.Sprintf("line-%06d:%s", i, bigPadding))
	}
	mgr.flushTailBuffer(ctx, buildID)

	logs, err := mgr.GetBuildLogs(ctx, "tenant-1", buildID)
	require.NoError(t, err)

	// Should contain truncation notice
	assert.Contains(t, logs, "[truncated:")
	assert.Contains(t, logs, "5MB limit reached]")

	// Should contain the last tailLines lines
	logLines := strings.Split(strings.TrimRight(logs, "\n"), "\n")
	// First line is the truncation header
	assert.True(t, strings.HasPrefix(logLines[0], "[truncated:"), "first line should be truncation header")

	// The tail should contain lines from the end
	lastLineTag := fmt.Sprintf("line-%06d:", totalLines-1)
	assert.Contains(t, logs, lastLineTag, "should contain the very last line")

	// Should NOT contain the first line (it was truncated)
	assert.NotContains(t, logs, "line-000000:")

	_ = logLines // suppress unused warning
}

func TestAppendLog_TruncationMessageFormat(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedBuildTestData(t, stateDB)

	mgr := NewManager(stateDB, &stubBuilder{}, nil)

	buildID := "test-build-truncmsg"
	_, err := stateDB.Exec(
		`INSERT INTO builds (id, service_id, tenant_id, status, source_type, source_ref, image, log, created_at)
		 VALUES (?, 'svc-1', 'tenant-1', 'running', 'git', 'main', 'img:latest', '', 1)`,
		buildID,
	)
	require.NoError(t, err)

	ctx := context.Background()

	// Use large lines to exceed 5MB quickly, then add exactly tailLines+100 more
	bigLine := strings.Repeat("X", 10*1024)
	linesBeforeCap := (maxLogSize / (10*1024 + 1)) + 1
	totalLines := linesBeforeCap + tailLines + 100 // enough to fully fill the ring buffer

	for i := 0; i < totalLines; i++ {
		mgr.appendLog(ctx, buildID, fmt.Sprintf("L%06d %s", i, bigLine))
	}
	mgr.flushTailBuffer(ctx, buildID)

	logs, err := mgr.GetBuildLogs(ctx, "tenant-1", buildID)
	require.NoError(t, err)

	lines := strings.Split(strings.TrimRight(logs, "\n"), "\n")

	// The truncation header must be the very first line
	assert.True(t, strings.HasPrefix(lines[0], "[truncated:"))
	assert.True(t, strings.HasSuffix(lines[0], "5MB limit reached]"))

	// After the header, there should be exactly tailLines (500) lines
	assert.Equal(t, tailLines+1, len(lines), "should have header + 500 tail lines")
}

func TestCollectTailLines_FewerThanCapacity(t *testing.T) {
	ls := &logState{tail: make([]string, tailLines)}
	for i := 0; i < 10; i++ {
		ls.tail[ls.tailIdx%tailLines] = fmt.Sprintf("line-%d", i)
		ls.tailIdx++
		ls.tailCount++
	}

	lines := collectTailLines(ls)
	assert.Len(t, lines, 10)
	for i := 0; i < 10; i++ {
		assert.Equal(t, fmt.Sprintf("line-%d", i), lines[i])
	}
}

func TestCollectTailLines_WrapsAround(t *testing.T) {
	ls := &logState{tail: make([]string, tailLines)}
	total := tailLines + 200 // write more than capacity to wrap
	for i := 0; i < total; i++ {
		ls.tail[ls.tailIdx%tailLines] = fmt.Sprintf("line-%d", i)
		ls.tailIdx++
		ls.tailCount++
	}

	lines := collectTailLines(ls)
	assert.Len(t, lines, tailLines)
	// Should contain the last `tailLines` entries
	for i := 0; i < tailLines; i++ {
		expected := fmt.Sprintf("line-%d", total-tailLines+i)
		assert.Equal(t, expected, lines[i])
	}
}

func TestStartBuild_LogTruncationEndToEnd(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedBuildTestData(t, stateDB)

	bigPadding := strings.Repeat("X", 10*1024) // 10KB lines for speed
	mgr := NewManager(stateDB, &stubBuilder{
		buildFn: func(ctx context.Context, req builder.BuildRequest, logCb func(string)) error {
			// Generate enough log lines to exceed 5MB with large lines
			for i := 0; i < 600; i++ {
				logCb(fmt.Sprintf("build-line-%06d:%s", i, bigPadding))
			}
			return nil
		},
	}, nil)

	build, err := mgr.StartBuild(context.Background(), "tenant-1", "svc-1", StartBuildRequest{
		SourceType: "git",
		SourceURL:  "https://github.com/example/repo.git",
		SourceRef:  "main",
	})
	require.NoError(t, err)

	waitForBuildStatus(t, stateDB, build.ID, "succeeded")

	logs, err := mgr.GetBuildLogs(context.Background(), "tenant-1", build.ID)
	require.NoError(t, err)

	// Verify truncation happened
	assert.Contains(t, logs, "[truncated:")
	assert.Contains(t, logs, "5MB limit reached]")

	// Verify the build completion lines from runBuild are in the tail
	// (these are appended after the builder returns)
	assert.Contains(t, logs, "[ah] Build complete")
}
