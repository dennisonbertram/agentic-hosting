package environments

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newReaperSandbox(t *testing.T, mgr *Manager) *Sandbox {
	t.Helper()
	s, err := mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "template"}})
	require.NoError(t, err)
	return s
}

func setSandboxRow(t *testing.T, mgr *Manager, id, status string, leaseExpiresAt, updatedAt int64) {
	t.Helper()
	_, err := mgr.db.Exec(`UPDATE environments SET status = ?, lease_expires_at = ?, updated_at = ? WHERE id = ?`,
		status, leaseExpiresAt, updatedAt, id)
	require.NoError(t, err)
}

func rowExists(t *testing.T, mgr *Manager, id string) bool {
	t.Helper()
	var n int
	require.NoError(t, mgr.db.QueryRow(`SELECT COUNT(*) FROM environments WHERE id = ?`, id).Scan(&n))
	return n == 1
}

func TestReap_ExpiredRunningIsDeleted(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	s := newReaperSandbox(t, mgr)
	now := time.Now().Unix()
	setSandboxRow(t, mgr, s.ID, "running", now-1, now)

	n, err := mgr.ReapSandboxes(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.False(t, rowExists(t, mgr, s.ID))
	assert.Contains(t, mock.StopContainerCalls, "mock-env-container-"+s.ID)
	assert.Contains(t, mock.RemoveContainerCalls, "mock-env-container-"+s.ID)
	assert.Contains(t, mock.RemoveVolumeCalls, "ah-env-"+s.ID)
}

func TestReap_ExpiredStoppedIsDeletedImmediately(t *testing.T) {
	mgr, _, _, _ := newSandboxManager(t)
	s := newReaperSandbox(t, mgr)
	now := time.Now().Unix()
	// The reconciler's expireLeases stopped it just now.
	setSandboxRow(t, mgr, s.ID, "stopped", now-5, now)
	n, err := mgr.ReapSandboxes(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.False(t, rowExists(t, mgr, s.ID))
}

func TestReap_GracePeriodForStoppedAndFailed(t *testing.T) {
	mgr, _, _, _ := newSandboxManager(t)
	now := time.Now().Unix()
	future := now + 3600
	stoppedFresh := newReaperSandbox(t, mgr)
	failedFresh := newReaperSandbox(t, mgr)
	stoppedOld := newReaperSandbox(t, mgr)
	failedOld := newReaperSandbox(t, mgr)
	running := newReaperSandbox(t, mgr)
	setSandboxRow(t, mgr, stoppedFresh.ID, "stopped", future, now-60)
	setSandboxRow(t, mgr, failedFresh.ID, "failed", future, now-60)
	setSandboxRow(t, mgr, stoppedOld.ID, "stopped", future, now-601)
	setSandboxRow(t, mgr, failedOld.ID, "failed", future, now-601)
	setSandboxRow(t, mgr, running.ID, "running", future, now-100000)

	n, err := mgr.ReapSandboxes(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.True(t, rowExists(t, mgr, stoppedFresh.ID))
	assert.True(t, rowExists(t, mgr, failedFresh.ID))
	assert.True(t, rowExists(t, mgr, running.ID), "an unexpired running sandbox is never reaped")
	assert.False(t, rowExists(t, mgr, stoppedOld.ID))
	assert.False(t, rowExists(t, mgr, failedOld.ID))
}

func TestReap_StuckDeletingIsRetried(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	s := newReaperSandbox(t, mgr)
	now := time.Now().Unix()

	// A teardown that fails keeps the row 'deleting'.
	mock.RemoveContainerFn = func(context.Context, string) error { return errors.New("daemon busy") }
	require.Error(t, mgr.DeleteSandbox(context.Background(), testTenantID, s.ID))
	require.True(t, rowExists(t, mgr, s.ID))

	// Too recent: not retried yet.
	n, err := mgr.ReapSandboxes(context.Background())
	require.NoError(t, err)
	assert.Zero(t, n)

	mock.RemoveContainerFn = nil
	setSandboxRow(t, mgr, s.ID, "deleting", now+3600, now-121)
	n, err = mgr.ReapSandboxes(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.False(t, rowExists(t, mgr, s.ID))
}

func TestReap_ExtendedRowIsNotReaped(t *testing.T) {
	mgr, _, _, _ := newSandboxManager(t)
	s := newReaperSandbox(t, mgr)
	_, err := mgr.ExtendSandbox(context.Background(), testTenantID, s.ID, 600)
	require.NoError(t, err)
	n, err := mgr.ReapSandboxes(context.Background())
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.True(t, rowExists(t, mgr, s.ID))
}

func TestReap_EnvironmentsAreNeverTouched(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "dev"})
	require.NoError(t, err)
	now := time.Now().Unix()
	for _, status := range []string{"running", "stopped", "failed", "deleting"} {
		setSandboxRow(t, mgr, env.ID, status, now-100000, now-100000)
		n, err := mgr.ReapSandboxes(context.Background())
		require.NoError(t, err)
		assert.Zero(t, n, status)
		assert.True(t, rowExists(t, mgr, env.ID), status)
	}
	assert.Empty(t, mock.RemoveContainerCalls)
}

func TestReap_NotFoundDuringDeleteStillRemovesRow(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	s := newReaperSandbox(t, mgr)
	mock.StopContainerFn = func(context.Context, string) error { return errors.New("No such container: x") }
	mock.RemoveContainerFn = func(context.Context, string) error { return errors.New("No such container: x") }
	mock.RemoveVolumeFn = func(context.Context, string) error { return errors.New("no such volume") }
	now := time.Now().Unix()
	setSandboxRow(t, mgr, s.ID, "running", now-1, now)
	n, err := mgr.ReapSandboxes(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.False(t, rowExists(t, mgr, s.ID))
}

func TestRunSandboxReaper_StopsOnCancelAndSurvivesPanic(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	s := newReaperSandbox(t, mgr)
	now := time.Now().Unix()
	setSandboxRow(t, mgr, s.ID, "running", now-1, now)
	var calls atomic.Int32
	mock.StopContainerFn = func(context.Context, string) error {
		if calls.Add(1) == 1 {
			panic("boom")
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { mgr.RunSandboxReaper(ctx, 10*time.Millisecond); close(done) }()

	// The first pass panics after claiming the row; the claim leaves it
	// 'deleting', which a later pass retries once it is stale.
	require.Eventually(t, func() bool { return calls.Load() >= 1 }, 2*time.Second, 5*time.Millisecond)
	_, err := mgr.db.Exec(`UPDATE environments SET updated_at = ? WHERE id = ?`, now-1000, s.ID)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return !rowExists(t, mgr, s.ID) }, 2*time.Second, 5*time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reaper did not stop on cancel")
	}
}
