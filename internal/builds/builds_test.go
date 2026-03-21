package builds

import (
	"context"
	"database/sql"
	"errors"
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
	mgr := NewManager(stateDB, &stubBuilder{}, func(ctx context.Context, tenantID, serviceID, imageTag, buildID string) error {
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

	mgr := NewManager(stateDB, &stubBuilder{}, func(ctx context.Context, tenantID, serviceID, imageTag, buildID string) error {
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
