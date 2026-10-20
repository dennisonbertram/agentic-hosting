package gc

import (
	"context"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A sandbox forked from a service build keeps that tag alive while its row
// exists, even before (or without) a container referencing it.
func TestRemoveUnusedBuiltImages_KeepsSandboxImages(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedService(t, stateDB, "svc-s", "ten-s", "ctr-s")
	_, err := stateDB.Exec(`INSERT INTO environments (id, tenant_id, name, kind, status, image_ref, created_at, updated_at)
		VALUES ('sbx-1', 'ten-s', 'sbx', 'sandbox', 'creating', '127.0.0.1:5000/ah/ten-s-svc-s:forked', 1, 1)`)
	require.NoError(t, err)
	old := time.Now().Add(-2 * time.Hour)
	mock := &testutil.MockDockerClient{
		ListImagesFn: func(ctx context.Context) ([]docker.ImageInfo, error) {
			return []docker.ImageInfo{
				{ID: "sha:1", Tags: []string{"127.0.0.1:5000/ah/ten-s-svc-s:forked"}, Created: old},
				{ID: "sha:2", Tags: []string{"127.0.0.1:5000/ah/ten-s-svc-s:unused"}, Created: old},
			}, nil
		},
	}
	assert.Equal(t, 1, New(stateDB, mock, time.Minute, t.TempDir()).removeUnusedBuiltImages(context.Background()))
	assert.Equal(t, []string{"127.0.0.1:5000/ah/ten-s-svc-s:unused"}, mock.RemoveImageCalls)
}

// Regression: public images ah pulled for image-sourced sandboxes were never
// collected, so a tenant could fill the Docker disk with disposable sandboxes.
// An unreferenced, unused recorded image is removed; referenced, in-use and
// recently requested ones are kept; a ref whose image is gone is forgotten.
func TestRemoveUnusedPulledImages(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedService(t, stateDB, "svc-p", "ten-p", "ctr-p")
	old := time.Now().Add(-2 * time.Hour).Unix()
	recent := time.Now().Unix()
	for ref, at := range map[string]int64{
		"python:3.12":  old,    // unused: removed
		"nginx:latest": old,    // services.image of svc-p (seedService): kept
		"busybox:1":    old,    // used by a container: kept
		"node:22":      recent, // just requested: kept
		"gone:1":       old,    // no longer present: row forgotten
	} {
		_, err := stateDB.Exec(`INSERT INTO pulled_images (ref, pulled_at) VALUES (?, ?)`, ref, at)
		require.NoError(t, err)
	}
	_, err := stateDB.Exec(`UPDATE services SET image = 'nginx:latest' WHERE id = 'svc-p'`)
	require.NoError(t, err)
	mock := &testutil.MockDockerClient{
		ListContainerImagesFn: func(context.Context) ([]string, error) { return []string{"busybox:1", "sha256:b"}, nil },
		ImageExistsFn:         func(_ context.Context, ref string) (bool, error) { return ref != "gone:1", nil },
	}

	assert.Equal(t, 1, New(stateDB, mock, time.Minute, t.TempDir()).removeUnusedPulledImages(context.Background()))
	assert.Equal(t, []string{"python:3.12"}, mock.RemoveImageCalls)

	var left []string
	rows, err := stateDB.Query(`SELECT ref FROM pulled_images ORDER BY ref`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var r string
		require.NoError(t, rows.Scan(&r))
		left = append(left, r)
	}
	assert.Equal(t, []string{"busybox:1", "nginx:latest", "node:22"}, left)
}
