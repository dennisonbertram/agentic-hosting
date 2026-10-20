package services

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// suspendDuringSlotWait starts fn while every deploy slot is taken, suspends
// the tenant and marks the row 'stopped' (as StopAllForTenant does) while fn
// waits, then frees a slot and returns fn's error.
func suspendDuringSlotWait(t *testing.T, mgr *Manager, svcID string, fn func() error) error {
	t.Helper()
	for i := 0; i < cap(mgr.deploySem); i++ {
		mgr.deploySem <- struct{}{}
	}
	done := make(chan error, 1)
	go func() { done <- fn() }()
	require.Eventually(t, func() bool { return len(mgr.deployQueue) == 1 }, 2*time.Second, time.Millisecond)

	_, err := mgr.db.Exec(`UPDATE tenants SET status = 'suspended' WHERE id = 'tenant-1'`)
	require.NoError(t, err)
	_, err = mgr.db.Exec(`UPDATE services SET status = 'stopped' WHERE id = ?`, svcID)
	require.NoError(t, err)
	<-mgr.deploySem

	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deploy did not return")
	}
	for len(mgr.deploySem) > 0 {
		<-mgr.deploySem
	}
	return err
}

// Regression: a tenant suspended while Deploy/DeployImage waited for a slot
// must keep StopAllForTenant's 'stopped' status, not be overwritten with
// 'deploying' (and later 'failed' by the stale-deploy sweep).
func TestDeploy_SuspendedDuringSlotWaitKeepsStoppedStatus(t *testing.T) {
	for name, call := range map[string]func(*Manager, string) error{
		"deploy": func(m *Manager, id string) error { return m.Deploy(context.Background(), "tenant-1", id) },
		"deployImage": func(m *Manager, id string) error {
			return m.DeployImage(context.Background(), "tenant-1", id, "img", "")
		},
	} {
		t.Run(name, func(t *testing.T) {
			pulled := false
			mock := &testutil.MockDockerClient{PullImageFn: func(context.Context, string) error { pulled = true; return nil }}
			mgr, _, svc := newTestMgr(t, mock, "")

			err := suspendDuringSlotWait(t, mgr, svc.ID, func() error { return call(mgr, svc.ID) })
			require.Error(t, err)
			assert.ErrorIs(t, err, apierr.ErrForbidden)
			status, _, _ := rowState(t, mgr, svc.ID)
			assert.Equal(t, "stopped", status)
			assert.False(t, pulled, "no image pull for a suspended tenant")
		})
	}
}

// Regression: Restart of a container-less service delegates to Deploy; on a
// full queue it must return the documented 503 + Retry-After and leave the row
// alone, like every other Restart.
func TestRestart_NoContainerQueueFullIs503(t *testing.T) {
	mgr, _, svc := newTestMgr(t, &testutil.MockDockerClient{}, "")
	fillQueue(mgr)

	err := mgr.Restart(context.Background(), "tenant-1", svc.ID)
	var apiErr *apierr.APIError
	require.True(t, errors.As(err, &apiErr), "got %v", err)
	assert.Equal(t, http.StatusServiceUnavailable, apiErr.Code)
	status, _, lastErr := rowState(t, mgr, svc.ID)
	assert.Equal(t, "created", status)
	assert.Empty(t, lastErr)
}
