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

func oldInspect(ctx context.Context, id string) (*docker.ContainerInfo, error) {
	return &docker.ContainerInfo{Status: "running", CreatedAt: time.Now().Add(-1 * time.Hour)}, nil
}

// Pool containers keep ah.type=warm-pool while pooled and claimed ones may carry
// other labels; an ID in environments or warm_pool must be kept whichever label found it.
func TestCollectOnce_KeepsContainersByIDInEnvironmentsOrPool(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedService(t, stateDB, "svc-x", "ten-x", "other")
	_, err := stateDB.Exec(`INSERT INTO environments (id, tenant_id, name, status, container_id, created_at, updated_at)
		VALUES ('env-1', 'ten-x', 'e', 'running', 'claimed-env-ctr', 1, 1)`)
	require.NoError(t, err)
	_, err = stateDB.Exec(`INSERT INTO warm_pool (id, template_id, container_id, volume_name, status, created_at)
		VALUES ('wp1', 'tmpl_default', 'pooled-ctr-0001', 'ah-pool-v', 'ready', 1)`)
	require.NoError(t, err)

	mock := &testutil.MockDockerClient{
		ListContainersByLabelFn: func(ctx context.Context, label, value string) ([]string, error) {
			switch {
			case label == "ah.service":
				return []string{"claimed-env-ctr", "pooled-ctr-0001"}, nil
			case label == "ah.type" && value == "warm-pool":
				return []string{"claimed-env-ctr"}, nil
			case label == "ah.type" && value == "environment":
				return []string{"pooled-ctr-0001"}, nil
			}
			return nil, nil
		},
		InspectContainerFn: oldInspect,
	}
	require.NoError(t, New(stateDB, mock, time.Minute, t.TempDir()).collectOnce(context.Background()))
	assert.Empty(t, mock.RemoveContainerCalls)
}

func TestCollectOnce_SkipsYoungNetworks(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{
		NetworkListFn: func(ctx context.Context) ([]docker.NetworkInfo, error) {
			return []docker.NetworkInfo{
				{ID: "n1", Name: "ah-tenant-new", Created: time.Now().Add(-2 * time.Minute)},
				{ID: "n2", Name: "ah-tenant-old", Created: time.Now().Add(-1 * time.Hour)},
			}, nil
		},
	}
	require.NoError(t, New(stateDB, mock, time.Minute, t.TempDir()).collectOnce(context.Background()))
	assert.Equal(t, []string{"ah-tenant-old"}, mock.RemoveNetworkCalls)
}

func TestCollectOnce_SkipsNetworksWithStoppedContainers(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{
		NetworkListFn: func(ctx context.Context) ([]docker.NetworkInfo, error) {
			return []docker.NetworkInfo{
				{ID: "n1", Name: "ah-tenant-stopped", Created: time.Now().Add(-1 * time.Hour)},
				{ID: "n2", Name: "ah-tenant-traefik-only", Created: time.Now().Add(-1 * time.Hour)},
			}, nil
		},
		ListNetworkContainersFn: func(ctx context.Context, name string) ([]string, error) {
			if name == "ah-tenant-stopped" {
				return []string{"paas-traefik", "ah-ten-svc"}, nil
			}
			return []string{"paas-traefik"}, nil
		},
	}
	require.NoError(t, New(stateDB, mock, time.Minute, t.TempDir()).collectOnce(context.Background()))
	assert.Equal(t, []string{"ah-tenant-traefik-only"}, mock.RemoveNetworkCalls)
}

func TestCollectOnce_RemovesUnusedBuiltImages(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedService(t, stateDB, "svc-i", "ten-i", "ctr-i")
	_, err := stateDB.Exec(`UPDATE services SET image = '127.0.0.1:5000/ah/ten-i-svc-i:cur' WHERE id = 'svc-i'`)
	require.NoError(t, err)
	_, err = stateDB.Exec(`INSERT INTO snapshots (id, tenant_id, service_id, name, image_ref, created_at)
		VALUES ('sn1', 'ten-i', 'svc-i', 's', '127.0.0.1:5000/snapshots/ten-i:sn1', 1)`)
	require.NoError(t, err)

	old := time.Now().Add(-2 * time.Hour)
	mock := &testutil.MockDockerClient{
		ListImagesFn: func(ctx context.Context) ([]docker.ImageInfo, error) {
			return []docker.ImageInfo{
				{ID: "sha:1", Tags: []string{"127.0.0.1:5000/ah/ten-i-svc-i:old"}, Created: old},
				{ID: "sha:2", Tags: []string{"127.0.0.1:5000/ah/ten-i-svc-i:cur"}, Created: old},
				{ID: "sha:3", Tags: []string{"127.0.0.1:5000/snapshots/ten-i:sn1"}, Created: old},
				{ID: "sha:4", Tags: []string{"127.0.0.1:5000/ah/ten-i-svc-i:running"}, Created: old},
				{ID: "sha:5", Tags: []string{"127.0.0.1:5000/ah/ten-i-svc-i:fresh"}, Created: time.Now()},
				{ID: "sha:6", Tags: []string{"nginx:latest"}, Created: old},
			}, nil
		},
		ListContainerImagesFn: func(ctx context.Context) ([]string, error) {
			return []string{"127.0.0.1:5000/ah/ten-i-svc-i:running", "sha:4"}, nil
		},
	}
	require.NoError(t, New(stateDB, mock, time.Minute, t.TempDir()).collectOnce(context.Background()))
	assert.Equal(t, []string{"127.0.0.1:5000/ah/ten-i-svc-i:old"}, mock.RemoveImageCalls)
	assert.Equal(t, 1, mock.PruneAnonymousVolumesCalls)
}
