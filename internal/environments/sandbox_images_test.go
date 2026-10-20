package environments

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureEnvLog(t *testing.T) *syncBuffer {
	t.Helper()
	b := &syncBuffer{}
	log.SetOutput(b)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return b
}

func pulledRefs(t *testing.T, mgr *Manager) []string {
	t.Helper()
	rows, err := mgr.db.Query(`SELECT ref FROM pulled_images ORDER BY ref`)
	require.NoError(t, err)
	defer rows.Close()
	var refs []string
	for rows.Next() {
		var r string
		require.NoError(t, rows.Scan(&r))
		refs = append(refs, r)
	}
	return refs
}

// Regression: template sandboxes use the operator's local image without a
// registry round trip, so a rate-limited or unreachable registry does not
// break them.
func TestCreateSandbox_TemplateUsesLocalImageWithoutPull(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	mock.ImageExistsFn = func(_ context.Context, ref string) (bool, error) { return ref == "ubuntu:24.04", nil }
	mock.PullImageFn = func(context.Context, string) error { return errors.New("toomanyrequests") }

	_, err := mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "template"}})
	require.NoError(t, err)
	assert.Empty(t, mock.PullImageCalls)
	assert.True(t, lastRunCfg(t, mock).ImagePrepared, "RunEnvironment must not pull it again")
}

func TestCreateSandbox_TemplateMissingLocallyIsPulled(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	_, err := mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "template"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"ubuntu:24.04"}, mock.PullImageCalls)
}

// The pull of a tenant-chosen image is what stops a tenant from running an
// image that exists only on this host, so it always happens.
func TestCreateSandbox_ImageSourceAlwaysPulls(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	mock.ImageExistsFn = func(context.Context, string) (bool, error) { return true, nil }
	_, err := mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "image", Image: "python:3.12"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"python:3.12"}, mock.PullImageCalls)

	mock.PullImageFn = func(context.Context, string) error { return errors.New("manifest unknown") }
	_, err = mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "image", Image: "python:3.12"}})
	require.Error(t, err, "a present image does not excuse a failed pull")
	assert.Contains(t, err.Error(), "pull image python:3.12")
}

// Regression: a pulled public image over AH_SANDBOX_MAX_IMAGE_MB is refused
// with 400 and, since this create brought it onto the host, removed.
func TestCreateSandbox_ImageTooLargeIsRefusedAndRemoved(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	mock.ImageSizeFn = func(context.Context, string) (int64, error) { return 5000 << 20, nil }

	_, err := mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "image", Image: "huge/image:1"}})
	assertAPICode(t, err, 400)
	assert.Contains(t, err.Error(), "image too large")
	assert.Equal(t, []string{"huge/image:1"}, mock.RemoveImageCalls)
	assert.Empty(t, pulledRefs(t, mgr), "the removed image is forgotten")
	assert.Zero(t, mock.RunEnvironmentCalls)

	var status string
	require.NoError(t, mgr.db.QueryRow(`SELECT status FROM environments WHERE kind = 'sandbox'`).Scan(&status))
	assert.Equal(t, "failed", status, "the refused create does not hold a quota slot")
}

func TestCreateSandbox_ImageTooLargeAlreadyPresentIsNotRemoved(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	mock.ImageExistsFn = func(context.Context, string) (bool, error) { return true, nil }
	mock.ImageSizeFn = func(context.Context, string) (int64, error) { return 5000 << 20, nil }
	_, err := mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "image", Image: "huge/image:1"}})
	assertAPICode(t, err, 400)
	assert.Empty(t, mock.RemoveImageCalls, "an image this create did not pull is left alone")
}

func TestCreateSandbox_ImageSizeLimitIsConfigurable(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	mgr.SetSandboxPullLimits(2, 100)
	mock.ImageSizeFn = func(_ context.Context, ref string) (int64, error) {
		if ref == "big:1" {
			return 101 << 20, nil
		}
		return 100 << 20, nil
	}
	_, err := mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "image", Image: "small:1"}})
	require.NoError(t, err)
	_, err = mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "image", Image: "big:1"}})
	assertAPICode(t, err, 400)

	// Templates and the tenant's own builds are not size-checked.
	mock.ImageSizeFn = func(context.Context, string) (int64, error) { return 1 << 40, nil }
	_, err = mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "template"}})
	require.NoError(t, err)
}

// Regression: the disk check runs again after the pull; a pull that crossed
// the block threshold fails the create with 503 and removes the image.
func TestCreateSandbox_DiskCheckedAgainAfterPull(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	var checks atomic.Int32
	mgr.SetSandboxDiskCheck(func() error {
		if checks.Add(1) > 1 {
			return errors.New("insufficient disk space at /var/lib/docker (91.0% used, threshold 90%)")
		}
		return nil
	})
	_, err := mgr.CreateSandbox(context.Background(), testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "image", Image: "python:3.12"}})
	assertAPICode(t, err, 503)
	assert.Equal(t, int32(2), checks.Load())
	assert.Equal(t, []string{"python:3.12"}, mock.RemoveImageCalls)
	assert.Zero(t, mock.RunEnvironmentCalls)
}

// Regression: sandbox image pulls share a host-wide limit.
func TestCreateSandbox_PullsShareHostWideLimit(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	seedTenant(t, mgr.db, "t_pull_third_003", 5)
	mgr.SetSandboxPullLimits(2, 4096)
	release := make(chan struct{})
	var inFlight, maxInFlight atomic.Int32
	mock.PullImageFn = func(ctx context.Context, _ string) error {
		n := inFlight.Add(1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		defer inFlight.Add(-1)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	tenants := []string{testTenantID, testTenantID, otherTenantID, "t_pull_third_003"}
	var wg sync.WaitGroup
	errs := make(chan error, len(tenants))
	for _, tenant := range tenants {
		wg.Add(1)
		go func(tenant string) {
			defer wg.Done()
			_, err := mgr.CreateSandbox(context.Background(), tenant, SandboxCreateRequest{Source: SandboxSource{Type: "image", Image: "python:3.12"}})
			errs <- err
		}(tenant)
	}
	require.Eventually(t, func() bool { return inFlight.Load() == 2 }, 2*time.Second, 5*time.Millisecond)
	time.Sleep(50 * time.Millisecond) // a third pull would start now if it could
	assert.Equal(t, int32(2), maxInFlight.Load())
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, int32(2), maxInFlight.Load())
}

// A create waiting for a pull slot gives up when its request is cancelled.
func TestPullSandboxImage_WaitHonoursContext(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	mgr.SetSandboxPullLimits(1, 4096)
	mgr.sandboxPullSem <- struct{}{} // the only slot is taken
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := mgr.pullSandboxImage(ctx, "python:3.12", false)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Empty(t, mock.PullImageCalls)
}

// ---------------------------------------------------------------------------
// Disk watchdog
// ---------------------------------------------------------------------------

func runningSandboxes(t *testing.T, mgr *Manager) int {
	t.Helper()
	var n int
	require.NoError(t, mgr.db.QueryRow(`SELECT COUNT(*) FROM environments WHERE kind = 'sandbox' AND status = 'running'`).Scan(&n))
	return n
}

// Regression: while the disk is at the block threshold the reaper deletes
// running sandboxes, largest disk_mb first, until usage drops below it.
func TestDiskWatchdog_DeletesLargestFirstUntilBelowThreshold(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	mgr.SetSandboxWorkspaceTmpfs(true)
	ctx := context.Background()
	ids := map[int]string{}
	for _, disk := range []int{64, 256, 128} {
		s, err := mgr.CreateSandbox(ctx, testTenantID, SandboxCreateRequest{Source: SandboxSource{Type: "template"}, DiskMB: intp(disk)})
		require.NoError(t, err)
		ids[disk] = s.ID
	}
	env, err := mgr.Create(ctx, testTenantID, CreateRequest{Name: "keep"})
	require.NoError(t, err)

	// Usage stays at the threshold until only one sandbox is left.
	mgr.SetSandboxDiskCheck(func() error {
		if runningSandboxes(t, mgr) > 1 {
			return errors.New("insufficient disk space at /var/lib/docker (95.0% used, threshold 90%)")
		}
		return nil
	})
	logs := captureEnvLog(t)
	n, err := mgr.DeleteSandboxesForDiskPressure(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.False(t, rowExists(t, mgr, ids[256]))
	assert.False(t, rowExists(t, mgr, ids[128]))
	assert.True(t, rowExists(t, mgr, ids[64]), "deletion stops once usage is below the threshold")
	assert.True(t, rowExists(t, mgr, env.ID), "environments are never touched")
	assert.Contains(t, mock.RemoveContainerCalls, "mock-env-container-"+ids[256])

	out := logs.String()
	assert.Contains(t, out, "AUDIT: action=sandbox.disk_pressure_deleted tenant="+testTenantID+" sandbox="+ids[256]+" disk_mb=256")
	assert.Contains(t, out, "AUDIT: action=sandbox.disk_pressure_deleted tenant="+testTenantID+" sandbox="+ids[128]+" disk_mb=128")
	assert.Less(t, bytes.Index([]byte(out), []byte(ids[256])), bytes.Index([]byte(out), []byte(ids[128])), "largest first")
}

func TestDiskWatchdog_NoPressureDeletesNothing(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	s := newReaperSandbox(t, mgr)
	n, err := mgr.DeleteSandboxesForDiskPressure(context.Background())
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.True(t, rowExists(t, mgr, s.ID))
	assert.Empty(t, mock.RemoveContainerCalls)
}

func TestDiskWatchdog_PressureWithNoSandboxesStops(t *testing.T) {
	mgr, _, _, _ := newSandboxManager(t)
	s := newReaperSandbox(t, mgr)
	mgr.SetSandboxDiskCheck(func() error { return errors.New("disk full") })
	n, err := mgr.DeleteSandboxesForDiskPressure(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n, "every running sandbox goes while the disk stays full")
	assert.False(t, rowExists(t, mgr, s.ID))
}

// A sandbox whose teardown fails stays 'deleting' for the reaper to retry;
// the watchdog moves on to the next one.
func TestDiskWatchdog_FailedTeardownMovesOn(t *testing.T) {
	mgr, mock, _, _ := newSandboxManager(t)
	a := newReaperSandbox(t, mgr)
	b := newReaperSandbox(t, mgr)
	mock.RemoveContainerFn = func(_ context.Context, id string) error {
		if id == "mock-env-container-"+a.ID {
			return errors.New("daemon busy")
		}
		return nil
	}
	mgr.SetSandboxDiskCheck(func() error { return errors.New("disk full") })
	n, err := mgr.DeleteSandboxesForDiskPressure(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.False(t, rowExists(t, mgr, b.ID))
	var status string
	require.NoError(t, mgr.db.QueryRow(`SELECT status FROM environments WHERE id = ?`, a.ID).Scan(&status))
	assert.Equal(t, "deleting", status)
}

// The watchdog runs on every reaper pass.
func TestSafeReap_RunsDiskWatchdog(t *testing.T) {
	mgr, _, _, _ := newSandboxManager(t)
	s := newReaperSandbox(t, mgr)
	mgr.SetSandboxDiskCheck(func() error { return errors.New("disk full") })
	mgr.safeReap(context.Background())
	assert.False(t, rowExists(t, mgr, s.ID))
}
