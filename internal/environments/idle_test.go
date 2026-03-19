package environments

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupIdleTest creates a DB, seeds tenant data, creates a running environment,
// then backdates last_activity_at by the given offset. Returns the idle detector,
// DB, mock client, and environment ID.
func setupIdleTest(t *testing.T, activityOffset time.Duration, idleTimeoutSec int) (*IdleDetector, *sql.DB, *testutil.MockDockerClient, string) {
	t.Helper()
	stateDB := testutil.NewStateDB(t)
	seedTestData(t, stateDB, 5)

	mock := &testutil.MockDockerClient{}
	mgr := NewManager(stateDB, mock)

	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{
		Name:           "idle-env",
		BaseImage:      "node:20",
		IdleTimeoutSec: idleTimeoutSec,
	})
	require.NoError(t, err)

	// Backdate last_activity_at by the given offset.
	past := time.Now().Add(-activityOffset).Unix()
	_, err = stateDB.Exec(`UPDATE environments SET last_activity_at = ? WHERE id = ?`, past, env.ID)
	require.NoError(t, err)

	detector := NewIdleDetector(stateDB, mock, 1*time.Minute)
	return detector, stateDB, mock, env.ID
}

func TestIdleDetector_StopsIdleEnvironments(t *testing.T) {
	// Environment with 60s timeout, last activity 120s ago → should be stopped.
	detector, stateDB, mock, envID := setupIdleTest(t, 120*time.Second, 60)

	err := detector.CheckOnce(context.Background())
	require.NoError(t, err)

	// Verify Docker stop was called.
	assert.NotEmpty(t, mock.StopContainerCalls, "expected StopContainer to be called for idle env")

	// Verify DB status changed to stopped.
	var status string
	err = stateDB.QueryRow(`SELECT status FROM environments WHERE id = ?`, envID).Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, "stopped", status)
}

func TestIdleDetector_SkipsActiveEnvironments(t *testing.T) {
	// Environment with 3600s (1h) timeout, last activity 10s ago → should stay running.
	detector, stateDB, mock, envID := setupIdleTest(t, 10*time.Second, 3600)

	err := detector.CheckOnce(context.Background())
	require.NoError(t, err)

	// StopContainer should not have been called for the idle check.
	// Note: the mock may have StopContainerCalls from reconcileStale (none expected here),
	// but we check that no NEW stop calls were made by idle detector.
	// Since setupIdleTest creates one running env, there should be zero stop calls from the detector.
	stopCallsFromDetector := len(mock.StopContainerCalls)
	assert.Equal(t, 0, stopCallsFromDetector, "expected no stop calls for active environment")

	// Verify DB status is still running.
	var status string
	err = stateDB.QueryRow(`SELECT status FROM environments WHERE id = ?`, envID).Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, "running", status)
}
