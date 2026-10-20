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
