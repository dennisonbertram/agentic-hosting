package reconciler_test

import (
	"context"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/reconciler"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReconciler_CircuitBreaker verifies that 5 crashes in the window opens the circuit.
func TestReconciler_CircuitBreaker(t *testing.T) {
	db := testutil.NewStateDB(t)
	ctx := context.Background()
	now := time.Now().Unix()

	// Insert tenant (required by FK constraint)
	_, err := db.ExecContext(ctx,
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"tenant-1", "Test Tenant", "test@example.com", "active", now, now)
	require.NoError(t, err)

	// Service already has crash_count=4 and a window started 100s ago (within 600s).
	// One more crash should push crash_count to 5 and open the circuit.
	windowStart := now - 100
	_, err = db.ExecContext(ctx,
		`INSERT INTO services (id, tenant_id, name, status, container_id, crash_count, circuit_open, crash_window_start, circuit_open_count, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-1", "tenant-1", "test-service", "running", "container-abc",
		4, 0, windowStart, 0, now, now)
	require.NoError(t, err)

	// Mock Docker: container appears in the ah.tenant list but inspect shows it exited.
	mock := &testutil.MockDockerClient{}
	mock.ListContainersByLabelFn = func(ctx context.Context, label, value string) ([]string, error) {
		if label == "ah.tenant" {
			return []string{"container-abc"}, nil
		}
		return nil, nil
	}
	mock.InspectContainerFn = func(ctx context.Context, containerID string) (*docker.ContainerInfo, error) {
		return &docker.ContainerInfo{Status: "exited", ExitCode: 1}, nil
	}

	r := reconciler.New(db, mock, time.Minute, nil)
	require.NoError(t, r.ReconcileOnce(ctx))

	var circuitOpen int
	var circuitRetryAt *int64
	err = db.QueryRowContext(ctx,
		`SELECT circuit_open, circuit_retry_at FROM services WHERE id = ?`, "svc-1").
		Scan(&circuitOpen, &circuitRetryAt)
	require.NoError(t, err)

	assert.Equal(t, 1, circuitOpen, "circuit_open should be 1 after 5 crashes in window")
	assert.NotNil(t, circuitRetryAt, "circuit_retry_at should be set when circuit opens")
	assert.Greater(t, *circuitRetryAt, now, "circuit_retry_at should be in the future")

	// When the circuit opens the reconciler stops the container.
	assert.Contains(t, mock.StopContainerCalls, "container-abc",
		"StopContainer should be called when circuit opens")
}

// TestReconciler_AutoRecovery verifies that a circuit past circuit_retry_at is auto-recovered.
func TestReconciler_AutoRecovery(t *testing.T) {
	db := testutil.NewStateDB(t)
	ctx := context.Background()
	now := time.Now().Unix()
	pastRetryAt := now - 60 // retry_at already elapsed

	_, err := db.ExecContext(ctx,
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"tenant-1", "Test Tenant", "test@example.com", "active", now, now)
	require.NoError(t, err)

	// Service with open circuit whose retry_at is in the past.
	_, err = db.ExecContext(ctx,
		`INSERT INTO services (id, tenant_id, name, status, container_id, crash_count, circuit_open, crash_window_start, circuit_retry_at, circuit_open_count, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-2", "tenant-1", "auto-recover-svc", "crashed", "",
		5, 1, now-700, pastRetryAt, 1, now, now)
	require.NoError(t, err)

	// No containers running — nothing else for the reconciler to act on.
	mock := &testutil.MockDockerClient{}
	mock.ListContainersByLabelFn = func(ctx context.Context, label, value string) ([]string, error) {
		return nil, nil
	}

	r := reconciler.New(db, mock, time.Minute, nil)
	require.NoError(t, r.ReconcileOnce(ctx))

	var circuitOpen, crashCount int
	var status string
	var circuitRetryAt *int64
	err = db.QueryRowContext(ctx,
		`SELECT circuit_open, crash_count, status, circuit_retry_at FROM services WHERE id = ?`, "svc-2").
		Scan(&circuitOpen, &crashCount, &status, &circuitRetryAt)
	require.NoError(t, err)

	assert.Equal(t, 0, circuitOpen, "circuit_open should be reset to 0 after retry_at elapsed")
	assert.Equal(t, 0, crashCount, "crash_count should be reset to 0")
	assert.Equal(t, "stopped", status, "status should be set to 'stopped' for auto-recovery")
	assert.Nil(t, circuitRetryAt, "circuit_retry_at should be cleared after recovery")
}

// TestReconciler_UnhealthyRestart verifies that unhealthy containers are stopped.
func TestReconciler_UnhealthyRestart(t *testing.T) {
	db := testutil.NewStateDB(t)
	ctx := context.Background()
	now := time.Now().Unix()

	_, err := db.ExecContext(ctx,
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"tenant-1", "Test Tenant", "test@example.com", "active", now, now)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx,
		`INSERT INTO services (id, tenant_id, name, status, container_id, crash_count, circuit_open, circuit_open_count, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-3", "tenant-1", "unhealthy-svc", "running", "container-xyz",
		0, 0, 0, now, now)
	require.NoError(t, err)

	// Container is running but health check reports "unhealthy".
	mock := &testutil.MockDockerClient{}
	mock.ListContainersByLabelFn = func(ctx context.Context, label, value string) ([]string, error) {
		if label == "ah.tenant" {
			return []string{"container-xyz"}, nil
		}
		return nil, nil
	}
	mock.InspectContainerFn = func(ctx context.Context, containerID string) (*docker.ContainerInfo, error) {
		return &docker.ContainerInfo{Status: "running", HealthStatus: "unhealthy"}, nil
	}

	r := reconciler.New(db, mock, time.Minute, nil)
	require.NoError(t, r.ReconcileOnce(ctx))

	assert.Contains(t, mock.StopContainerCalls, "container-xyz",
		"StopContainer should be called for unhealthy container")

	var status string
	err = db.QueryRowContext(ctx, `SELECT status FROM services WHERE id = ?`, "svc-3").Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, "crashed", status, "unhealthy service should be marked as crashed")
}

// TestReconciler_CircuitBreakerBackoffEscalation verifies that the retry delay escalates with
// circuit_open_count: after 3 circuit opens the delay should be 4h, not 30m.
func TestReconciler_CircuitBreakerBackoffEscalation(t *testing.T) {
	db := testutil.NewStateDB(t)
	ctx := context.Background()
	now := time.Now().Unix()

	_, err := db.ExecContext(ctx,
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"tenant-1", "Test Tenant", "test@example.com", "active", now, now)
	require.NoError(t, err)

	// Service with crash_count=4 and circuit_open_count=2 (already opened twice before).
	// The next crash will be the 3rd circuit open → backoff should be 4h.
	windowStart := now - 100
	_, err = db.ExecContext(ctx,
		`INSERT INTO services (id, tenant_id, name, status, container_id, crash_count, circuit_open, crash_window_start, circuit_open_count, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-escalate", "tenant-1", "escalation-svc", "running", "container-esc",
		4, 0, windowStart, 2, now, now)
	require.NoError(t, err)

	// Mock Docker: container is in the list but reports exited.
	mock := &testutil.MockDockerClient{}
	mock.ListContainersByLabelFn = func(ctx context.Context, label, value string) ([]string, error) {
		if label == "ah.tenant" {
			return []string{"container-esc"}, nil
		}
		return nil, nil
	}
	mock.InspectContainerFn = func(ctx context.Context, containerID string) (*docker.ContainerInfo, error) {
		return &docker.ContainerInfo{Status: "exited", ExitCode: 1}, nil
	}

	before := time.Now()
	r := reconciler.New(db, mock, time.Minute, nil)
	require.NoError(t, r.ReconcileOnce(ctx))

	var circuitOpen, circuitOpenCount int
	var circuitRetryAt int64
	err = db.QueryRowContext(ctx,
		`SELECT circuit_open, circuit_open_count, circuit_retry_at FROM services WHERE id = ?`, "svc-escalate").
		Scan(&circuitOpen, &circuitOpenCount, &circuitRetryAt)
	require.NoError(t, err)

	assert.Equal(t, 1, circuitOpen, "circuit should be open after 5th crash")
	assert.Equal(t, 3, circuitOpenCount, "circuit_open_count should be 3")

	// circuit_retry_at must be ~4h from now, not 30m.
	minExpected := before.Add(4 * time.Hour).Unix()
	maxExpected := time.Now().Add(4*time.Hour + 5*time.Second).Unix()
	assert.GreaterOrEqual(t, circuitRetryAt, minExpected,
		"retry delay after 3rd open should be at least 4h (got %ds from now)", circuitRetryAt-now)
	assert.LessOrEqual(t, circuitRetryAt, maxExpected,
		"retry delay should not exceed 4h+5s")
}

// TestReconciler_StaleDeployment verifies services stuck deploying >10min are marked failed.
func TestReconciler_StaleDeployment(t *testing.T) {
	db := testutil.NewStateDB(t)
	ctx := context.Background()
	now := time.Now().Unix()
	elevenMinAgo := now - 11*60 // past the 10-minute threshold
	fiveMinAgo := now - 5*60   // within the threshold — should be left alone

	_, err := db.ExecContext(ctx,
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"tenant-1", "Test Tenant", "test@example.com", "active", now, now)
	require.NoError(t, err)

	// Stale deploying service (11 minutes old).
	_, err = db.ExecContext(ctx,
		`INSERT INTO services (id, tenant_id, name, status, container_id, crash_count, circuit_open, circuit_open_count, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-4", "tenant-1", "deploying-svc-stale", "deploying", "",
		0, 0, 0, elevenMinAgo, elevenMinAgo)
	require.NoError(t, err)

	// Recently-started deploying service (5 minutes old) — must not be touched.
	_, err = db.ExecContext(ctx,
		`INSERT INTO services (id, tenant_id, name, status, container_id, crash_count, circuit_open, circuit_open_count, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-5", "tenant-1", "deploying-svc-recent", "deploying", "",
		0, 0, 0, fiveMinAgo, fiveMinAgo)
	require.NoError(t, err)

	mock := &testutil.MockDockerClient{}
	mock.ListContainersByLabelFn = func(ctx context.Context, label, value string) ([]string, error) {
		return nil, nil
	}

	r := reconciler.New(db, mock, time.Minute, nil)
	require.NoError(t, r.ReconcileOnce(ctx))

	var status, lastError string
	err = db.QueryRowContext(ctx,
		`SELECT status, last_error FROM services WHERE id = ?`, "svc-4").
		Scan(&status, &lastError)
	require.NoError(t, err)
	assert.Equal(t, "failed", status, "stale deploying service should be marked failed")
	assert.Equal(t, "deploy timed out (reconciler)", lastError)

	var recentStatus string
	err = db.QueryRowContext(ctx, `SELECT status FROM services WHERE id = ?`, "svc-5").Scan(&recentStatus)
	require.NoError(t, err)
	assert.Equal(t, "deploying", recentStatus, "recently-started deploying service should not be affected")
}
