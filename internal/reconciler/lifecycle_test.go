package reconciler_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/deployments"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/reconciler"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A database re-provisioned while the reconciler inspects its old container
// must not be marked unavailable.
func TestReconciler_DatabaseUnavailableIsCompareAndSwap(t *testing.T) {
	db := testutil.NewStateDB(t)
	ctx := context.Background()
	seedService(t, db, "svc-x", "stopped", "")
	now := time.Now().Unix()
	_, err := db.Exec(`INSERT INTO databases (id, tenant_id, name, type, status, container_id, volume_name, password_encrypted, created_at, updated_at)
		VALUES ('db-1','tenant-1','a','postgres','ready','cid-old','ah-db-a','x',?,?)`, now, now)
	require.NoError(t, err)

	mock := &testutil.MockDockerClient{}
	mock.ListContainersByLabelFn = func(ctx context.Context, label, value string) ([]string, error) { return nil, nil }
	mock.InspectContainerFn = func(ctx context.Context, id string) (*docker.ContainerInfo, error) {
		// Concurrent re-provision swaps in a new container mid-reconcile.
		_, err := db.Exec(`UPDATE databases SET container_id = 'cid-new' WHERE id = 'db-1'`)
		require.NoError(t, err)
		return nil, testutil.NewNotFoundError(id)
	}
	require.NoError(t, reconciler.New(db, mock, time.Minute, nil).ReconcileOnce(ctx))

	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM databases WHERE id = 'db-1'`).Scan(&status))
	assert.Equal(t, "ready", status)
}

func TestReconciler_DatabaseGoneMarkedUnavailable(t *testing.T) {
	db := testutil.NewStateDB(t)
	ctx := context.Background()
	seedService(t, db, "svc-x", "stopped", "")
	now := time.Now().Unix()
	_, err := db.Exec(`INSERT INTO databases (id, tenant_id, name, type, status, container_id, volume_name, password_encrypted, created_at, updated_at)
		VALUES ('db-gone','tenant-1','a','postgres','ready','cid-gone','ah-db-a','x',?,?),
		       ('db-live','tenant-1','b','postgres','ready','cid-live','ah-db-b','x',?,?),
		       ('db-flaky','tenant-1','c','postgres','ready','cid-flaky','ah-db-c','x',?,?)`,
		now, now, now, now, now, now)
	require.NoError(t, err)

	mock := &testutil.MockDockerClient{}
	mock.ListContainersByLabelFn = func(ctx context.Context, label, value string) ([]string, error) {
		if label == "ah.tenant" {
			return []string{"cid-live"}, nil
		}
		return nil, nil
	}
	mock.InspectContainerFn = func(ctx context.Context, id string) (*docker.ContainerInfo, error) {
		switch id {
		case "cid-gone":
			return nil, testutil.NewNotFoundError(id)
		case "cid-flaky":
			return nil, errors.New("daemon timeout")
		}
		return &docker.ContainerInfo{Status: "running"}, nil
	}
	require.NoError(t, reconciler.New(db, mock, time.Minute, nil).ReconcileOnce(ctx))

	status := func(id string) string {
		var s string
		require.NoError(t, db.QueryRow(`SELECT status FROM databases WHERE id = ?`, id).Scan(&s))
		return s
	}
	assert.Equal(t, "unavailable", status("db-gone"))
	assert.Equal(t, "ready", status("db-live"))
	assert.Equal(t, "ready", status("db-flaky"), "transient Docker errors must not change state")
	assert.NotContains(t, mock.InspectContainerCalls, "cid-live", "listed containers are not inspected")
}

func TestReconciler_EnvironmentGoneOrExitedMarkedStopped(t *testing.T) {
	db := testutil.NewStateDB(t)
	ctx := context.Background()
	seedService(t, db, "svc-x", "stopped", "")
	now := time.Now().Unix()
	_, err := db.Exec(`INSERT INTO environments (id, tenant_id, name, status, container_id, created_at, updated_at)
		VALUES ('env-gone','tenant-1','a','running','cid-gone',?,?),
		       ('env-exited','tenant-1','b','running','cid-exited',?,?),
		       ('env-unlisted-exited','tenant-1','c','running','cid-unlisted',?,?),
		       ('env-live','tenant-1','d','running','cid-live',?,?),
		       ('env-flaky','tenant-1','e','running','cid-flaky',?,?)`,
		now, now, now, now, now, now, now, now, now, now)
	require.NoError(t, err)

	mock := &testutil.MockDockerClient{}
	mock.ListContainersByLabelFn = func(ctx context.Context, label, value string) ([]string, error) {
		if label == "ah.tenant" {
			return []string{"cid-exited", "cid-live", "cid-flaky"}, nil
		}
		return nil, nil
	}
	mock.InspectContainerFn = func(ctx context.Context, id string) (*docker.ContainerInfo, error) {
		switch id {
		case "cid-gone":
			return nil, testutil.NewNotFoundError(id)
		case "cid-flaky":
			return nil, errors.New("daemon timeout")
		case "cid-exited", "cid-unlisted":
			return &docker.ContainerInfo{Status: "exited"}, nil
		}
		return &docker.ContainerInfo{Status: "running"}, nil
	}
	require.NoError(t, reconciler.New(db, mock, time.Minute, nil).ReconcileOnce(ctx))

	status := func(id string) string {
		var s string
		require.NoError(t, db.QueryRow(`SELECT status FROM environments WHERE id = ?`, id).Scan(&s))
		return s
	}
	assert.Equal(t, "stopped", status("env-gone"))
	assert.Equal(t, "stopped", status("env-exited"))
	assert.Equal(t, "stopped", status("env-unlisted-exited"))
	assert.Equal(t, "running", status("env-live"))
	assert.Equal(t, "running", status("env-flaky"))
}

func TestReconciler_StaleEnvironmentCreateFails(t *testing.T) {
	db := testutil.NewStateDB(t)
	ctx := context.Background()
	seedService(t, db, "svc-x", "stopped", "")
	now := time.Now().Unix()
	old := now - 6*60
	_, err := db.Exec(`INSERT INTO environments (id, tenant_id, name, status, created_at, updated_at)
		VALUES ('env-old','tenant-1','a','creating',?,?), ('env-new','tenant-1','b','creating',?,?)`,
		old, old, now, now)
	require.NoError(t, err)

	require.NoError(t, reconciler.New(db, &testutil.MockDockerClient{}, time.Minute, nil).ReconcileOnce(ctx))

	var oldStatus, newStatus string
	require.NoError(t, db.QueryRow(`SELECT status FROM environments WHERE id='env-old'`).Scan(&oldStatus))
	require.NoError(t, db.QueryRow(`SELECT status FROM environments WHERE id='env-new'`).Scan(&newStatus))
	assert.Equal(t, "failed", oldStatus)
	assert.Equal(t, "creating", newStatus)
}

func TestReconciler_StaleDeployRecordsDeploymentEvent(t *testing.T) {
	db := testutil.NewStateDB(t)
	ctx := context.Background()
	seedService(t, db, "svc-stale", "deploying", "")
	seedService(t, db, "svc-noimg", "deploying", "")
	old := time.Now().Add(-11 * time.Minute).Unix()
	_, err := db.Exec(`UPDATE services SET image = 'img:1', updated_at = ? WHERE id = 'svc-stale'`, old)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE services SET image = NULL, updated_at = ? WHERE id = 'svc-noimg'`, old)
	require.NoError(t, err)

	store := deployments.NewStore(db)
	require.NoError(t, reconciler.New(db, &testutil.MockDockerClient{}, time.Minute, store).ReconcileOnce(ctx))

	for _, id := range []string{"svc-stale", "svc-noimg"} {
		assert.Equal(t, "failed", getSvc(t, db, id).status)
		deps, err := store.ListByService(ctx, "tenant-1", id, 10, 0)
		require.NoError(t, err)
		require.Len(t, deps, 1, "one timeout event for %s", id)
		assert.Equal(t, deployments.StatusFailed, deps[0].Status)
		assert.Equal(t, deployments.TriggerReconciler, deps[0].Trigger)
		assert.Equal(t, "deploy timed out (reconciler)", deps[0].ErrorMessage)
	}
	deps, err := store.ListByService(ctx, "tenant-1", "svc-stale", 10, 0)
	require.NoError(t, err)
	assert.Equal(t, "img:1", deps[0].Image)
}

func TestReconciler_CrashAndRecoveryRecordDeploymentEvents(t *testing.T) {
	db := testutil.NewStateDB(t)
	ctx := context.Background()
	seedService(t, db, "svc-c", "running", "cid-c")
	seedService(t, db, "svc-r", "crashed", "")
	_, err := db.Exec(`UPDATE services SET image = 'img:c' WHERE id = 'svc-c'`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE services SET circuit_open = 1, circuit_retry_at = ? WHERE id = 'svc-r'`, time.Now().Unix()-1)
	require.NoError(t, err)

	mock := mockWith("cid-c", func() *docker.ContainerInfo { return &docker.ContainerInfo{Status: "exited", ExitCode: 2} })
	store := deployments.NewStore(db)
	require.NoError(t, reconciler.New(db, mock, time.Minute, store).ReconcileOnce(ctx))

	crash, err := store.ListByService(ctx, "tenant-1", "svc-c", 10, 0)
	require.NoError(t, err)
	require.Len(t, crash, 1)
	assert.Equal(t, deployments.StatusCrashed, crash[0].Status)
	assert.Equal(t, "img:c", crash[0].Image)
	assert.Contains(t, crash[0].ErrorMessage, "exit code 2")

	rec, err := store.ListByService(ctx, "tenant-1", "svc-r", 10, 0)
	require.NoError(t, err)
	require.Len(t, rec, 1)
	assert.Equal(t, deployments.StatusStopped, rec[0].Status)
	assert.Equal(t, deployments.TriggerAutoRecovery, rec[0].Trigger)
	assert.Equal(t, "stopped", getSvc(t, db, "svc-r").status)
}

func TestReconciler_ListFailureLeavesStateUntouched(t *testing.T) {
	db := testutil.NewStateDB(t)
	ctx := context.Background()
	seedService(t, db, "svc-1", "running", "cid-1")
	mock := &testutil.MockDockerClient{}
	mock.ListContainersByLabelFn = func(ctx context.Context, label, value string) ([]string, error) {
		return nil, errors.New("daemon down")
	}
	err := reconciler.New(db, mock, time.Minute, nil).ReconcileOnce(ctx)
	require.Error(t, err)
	assert.Equal(t, "running", getSvc(t, db, "svc-1").status)
	assert.Empty(t, mock.InspectContainerCalls)
}

func TestReconciler_SplitBrainIsLogOnly(t *testing.T) {
	db := testutil.NewStateDB(t)
	ctx := context.Background()
	seedService(t, db, "svc-1", "stopped", "cid-db")
	mock := &testutil.MockDockerClient{}
	mock.ListContainersByLabelFn = func(ctx context.Context, label, value string) ([]string, error) {
		if label == "ah.service" {
			return []string{"cid-rogue", "cid-nolabels"}, nil
		}
		return nil, nil
	}
	mock.GetContainerLabelsFn = func(ctx context.Context, id string) map[string]string {
		if id == "cid-rogue" {
			return map[string]string{"ah.service": "svc-1", "ah.tenant": "tenant-1"}
		}
		return nil
	}
	require.NoError(t, reconciler.New(db, mock, time.Minute, nil).ReconcileOnce(ctx))
	assert.Equal(t, "cid-db", getSvc(t, db, "svc-1").containerID, "labels must never rewrite DB state")
	assert.Empty(t, mock.StopContainerCalls)
	assert.Empty(t, mock.RemoveContainerCalls)
}

func TestReconciler_RunStopsOnCancelAndSurvivesErrors(t *testing.T) {
	db := testutil.NewStateDB(t)
	ticks := make(chan struct{}, 10)
	mock := &testutil.MockDockerClient{}
	mock.ListContainersByLabelFn = func(ctx context.Context, label, value string) ([]string, error) {
		select {
		case ticks <- struct{}{}:
		default:
		}
		return nil, errors.New("daemon down") // every tick fails; the loop must keep going
	}
	r := reconciler.New(db, mock, 5*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	for i := 0; i < 2; i++ {
		select {
		case <-ticks:
		case <-time.After(2 * time.Second):
			t.Fatal("reconciler did not tick")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestReconciler_RunRecoversFromPanic(t *testing.T) {
	db := testutil.NewStateDB(t)
	calls := make(chan struct{}, 10)
	mock := &testutil.MockDockerClient{}
	mock.ListContainersByLabelFn = func(ctx context.Context, label, value string) ([]string, error) {
		calls <- struct{}{}
		panic("boom")
	}
	r := reconciler.New(db, mock, 5*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	for i := 0; i < 2; i++ {
		select {
		case <-calls:
		case <-time.After(2 * time.Second):
			t.Fatal("reconciler loop died after panic")
		}
	}
}
