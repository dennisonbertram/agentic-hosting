package snapshots

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The orphaned-tag cleanup must survive a cancelled request yet stay bounded,
// so a stalled Docker daemon cannot hang the request goroutine forever.
func TestSnapshotCreate_OrphanTagCleanupIsDetachedAndBounded(t *testing.T) {
	mgr, mock, db, tenantID, serviceID := setupTestWithDB(t)
	_, err := db.Exec(`CREATE TRIGGER block_snap BEFORE INSERT ON snapshots BEGIN SELECT RAISE(ABORT, 'blocked'); END`)
	require.NoError(t, err)

	var live, bounded bool
	mock.RemoveImageFn = func(c context.Context, _ string) error {
		live = c.Err() == nil
		_, bounded = c.Deadline()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	mock.TagImageFn = func(context.Context, string, string) error {
		cancel() // the client goes away right after the tag is created
		return nil
	}

	_, err = mgr.Create(ctx, tenantID, serviceID, CreateRequest{Name: "s"})
	require.Error(t, err)
	require.Len(t, mock.RemoveImageCalls, 1, "orphaned tag must be removed")
	assert.True(t, live, "cleanup must not inherit the request's cancellation")
	assert.True(t, bounded, "cleanup must carry a deadline")
}
