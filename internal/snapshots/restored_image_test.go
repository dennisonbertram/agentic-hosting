package snapshots

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A service restored from a snapshot runs from the snapshot's local-only tag;
// retention GC and explicit delete must drop the row but keep that tag.
func TestCleanExpired_KeepsImageUsedByRestoredService(t *testing.T) {
	mgr, mock, db, tenantID, serviceID := setupTestWithDB(t)
	ctx := context.Background()
	now := time.Now().Unix()
	insertSnapshotWithAge(t, db, "old", tenantID, serviceID, now-500)
	insertSnapshotWithAge(t, db, "new", tenantID, serviceID, now-100)
	restoredRef := "127.0.0.1:5000/snapshots/" + tenantID + ":old"
	_, err := db.Exec(`INSERT INTO services (id, tenant_id, name, status, image, port, created_at, updated_at) VALUES ('svc-restored', ?, 'restored', 'running', ?, 8080, 1, 1)`, tenantID, restoredRef)
	require.NoError(t, err)

	removed, err := mgr.CleanExpired(ctx, 1, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	_, err = mgr.Get(ctx, tenantID, "old")
	assert.True(t, errors.Is(err, apierr.ErrNotFound), "row is deleted")
	assert.NotContains(t, mock.RemoveImageCalls, restoredRef, "tag used by a service must be kept")
}

func TestDelete_KeepsImageUsedByRestoredService(t *testing.T) {
	mgr, mock, db, tenantID, serviceID := setupTestWithDB(t)
	ctx := context.Background()
	insertSnapshotWithAge(t, db, "s1", tenantID, serviceID, time.Now().Unix())
	ref := "127.0.0.1:5000/snapshots/" + tenantID + ":s1"
	_, err := db.Exec(`INSERT INTO services (id, tenant_id, name, status, image, port, created_at, updated_at) VALUES ('svc-restored', ?, 'restored', 'running', ?, 8080, 1, 1)`, tenantID, ref)
	require.NoError(t, err)

	require.NoError(t, mgr.Delete(ctx, tenantID, "s1"))
	assert.Empty(t, mock.RemoveImageCalls)

	// Unreferenced snapshot images are still removed.
	insertSnapshotWithAge(t, db, "s2", tenantID, serviceID, time.Now().Unix())
	require.NoError(t, mgr.Delete(ctx, tenantID, "s2"))
	assert.Equal(t, []string{"127.0.0.1:5000/snapshots/" + tenantID + ":s2"}, mock.RemoveImageCalls)
}
