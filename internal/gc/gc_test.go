package gc

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCollectOnce_RemovesOrphanedServiceContainers(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{
		ListContainersByLabelFn: func(ctx context.Context, label, value string) ([]string, error) {
			if label == "ah.service" {
				return []string{"orphan-ctr-1"}, nil
			}
			return nil, nil
		},
		InspectContainerFn: func(ctx context.Context, id string) (*docker.ContainerInfo, error) {
			return &docker.ContainerInfo{
				Status:    "running",
				CreatedAt: time.Now().Add(-1 * time.Hour), // old enough
			}, nil
		},
	}

	g := New(stateDB, mock, 5*time.Minute, t.TempDir())
	err := g.collectOnce(context.Background())
	require.NoError(t, err)

	assert.Contains(t, mock.StopContainerCalls, "orphan-ctr-1")
	assert.Contains(t, mock.RemoveContainerCalls, "orphan-ctr-1")
}

func TestCollectOnce_SkipsYoungContainers(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{
		ListContainersByLabelFn: func(ctx context.Context, label, value string) ([]string, error) {
			if label == "ah.service" {
				return []string{"young-ctr"}, nil
			}
			return nil, nil
		},
		InspectContainerFn: func(ctx context.Context, id string) (*docker.ContainerInfo, error) {
			return &docker.ContainerInfo{
				Status:    "running",
				CreatedAt: time.Now().Add(-1 * time.Minute), // too young
			}, nil
		},
	}

	g := New(stateDB, mock, 5*time.Minute, t.TempDir())
	err := g.collectOnce(context.Background())
	require.NoError(t, err)

	assert.Empty(t, mock.StopContainerCalls, "should not stop young containers")
	assert.Empty(t, mock.RemoveContainerCalls, "should not remove young containers")
}

func TestCollectOnce_SkipsContainersInDB(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	// Insert a service with this container ID
	seedService(t, stateDB, "svc-1", "tenant-1", "known-ctr")

	mock := &testutil.MockDockerClient{
		ListContainersByLabelFn: func(ctx context.Context, label, value string) ([]string, error) {
			if label == "ah.service" {
				return []string{"known-ctr"}, nil
			}
			return nil, nil
		},
		InspectContainerFn: func(ctx context.Context, id string) (*docker.ContainerInfo, error) {
			return &docker.ContainerInfo{
				Status:    "running",
				CreatedAt: time.Now().Add(-1 * time.Hour),
			}, nil
		},
	}

	g := New(stateDB, mock, 5*time.Minute, t.TempDir())
	err := g.collectOnce(context.Background())
	require.NoError(t, err)

	assert.Empty(t, mock.RemoveContainerCalls, "should not remove container that's in DB")
}

func TestCollectOnce_RemovesOrphanedVolumes(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{
		ListVolumesFn: func(ctx context.Context, prefix string) ([]string, error) {
			return []string{"ah-db-orphan-vol"}, nil
		},
	}

	g := New(stateDB, mock, 5*time.Minute, t.TempDir())
	err := g.collectOnce(context.Background())
	require.NoError(t, err)

	assert.Contains(t, mock.RemoveVolumeSafeCalls, "ah-db-orphan-vol")
}

func TestCleanOldBuildDirs(t *testing.T) {
	// Resolve symlinks in temp dir path (macOS /var -> /private/var)
	baseDir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	// Create an old directory
	oldDir := filepath.Join(baseDir, "old-build")
	require.NoError(t, os.Mkdir(oldDir, 0o755))
	oldTime := time.Now().Add(-2 * time.Hour)
	os.Chtimes(oldDir, oldTime, oldTime)

	// Create a new directory
	newDir := filepath.Join(baseDir, "new-build")
	require.NoError(t, os.Mkdir(newDir, 0o755))

	g := &GC{}
	removed := g.cleanOldBuildDirs(baseDir, 1*time.Hour)

	assert.Equal(t, 1, removed)
	assert.NoDirExists(t, oldDir, "old dir should be removed")
	assert.DirExists(t, newDir, "new dir should remain")
}

func TestCleanOldBuildDirs_SkipsSymlinks(t *testing.T) {
	baseDir := t.TempDir()

	// Create a target dir outside base
	targetDir := t.TempDir()

	// Create a symlink in base pointing to target
	linkPath := filepath.Join(baseDir, "symlink-build")
	require.NoError(t, os.Symlink(targetDir, linkPath))

	g := &GC{}
	removed := g.cleanOldBuildDirs(baseDir, 0) // maxAge=0 means clean everything

	assert.Equal(t, 0, removed, "should skip symlinks")
	assert.DirExists(t, targetDir, "target should not be removed")
}

func TestCollectOnce_PrunesDanglingImages(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{
		PruneDanglingImagesFn: func(ctx context.Context) (int, error) {
			return 3, nil
		},
	}

	g := New(stateDB, mock, 5*time.Minute, t.TempDir())
	err := g.collectOnce(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 1, mock.PruneDanglingImagesCalls)
}

// mockSnapshotCleaner implements SnapshotCleaner for testing.
type mockSnapshotCleaner struct {
	called          bool
	maxPerService   int
	maxAge          time.Duration
	returnRemoved   int
	returnErr       error
}

func (m *mockSnapshotCleaner) CleanExpired(ctx context.Context, maxPerService int, maxAge time.Duration) (int, error) {
	m.called = true
	m.maxPerService = maxPerService
	m.maxAge = maxAge
	return m.returnRemoved, m.returnErr
}

func TestCollectOnce_CallsSnapshotCleaner(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{}

	cleaner := &mockSnapshotCleaner{returnRemoved: 5}
	maxAge := 30 * 24 * time.Hour

	g := New(stateDB, mock, 5*time.Minute, t.TempDir())
	g.SetSnapshotCleaner(cleaner, 10, maxAge)

	err := g.collectOnce(context.Background())
	require.NoError(t, err)

	assert.True(t, cleaner.called, "snapshot cleaner should have been called")
	assert.Equal(t, 10, cleaner.maxPerService)
	assert.Equal(t, maxAge, cleaner.maxAge)
}

func TestCollectOnce_SkipsSnapshotCleanerWhenNil(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{}

	g := New(stateDB, mock, 5*time.Minute, t.TempDir())
	// Don't set any snapshot cleaner

	err := g.collectOnce(context.Background())
	require.NoError(t, err)
	// Should not panic or fail — just skip snapshot cleanup
}

func TestCollectOnce_RemovesOrphanedNetworks(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{
		NetworkListFn: func(ctx context.Context) ([]docker.NetworkInfo, error) {
			return []docker.NetworkInfo{
				{ID: "net-1", Name: "ah-tenant-orphan1", Containers: 0},
				{ID: "net-2", Name: "ah-tenant-orphan2", Containers: 1}, // only Traefik connected
				{ID: "net-3", Name: "ah-tenant-active", Containers: 2},  // tenant + Traefik
				{ID: "net-4", Name: "bridge", Containers: 5},            // not a tenant network
			}, nil
		},
	}

	g := New(stateDB, mock, 5*time.Minute, t.TempDir())
	err := g.collectOnce(context.Background())
	require.NoError(t, err)

	// Should disconnect Traefik from orphaned networks
	assert.Len(t, mock.NetworkDisconnectCalls, 2, "should disconnect Traefik from 2 orphaned networks")
	// Should remove orphaned networks
	assert.Contains(t, mock.RemoveNetworkCalls, "ah-tenant-orphan1")
	assert.Contains(t, mock.RemoveNetworkCalls, "ah-tenant-orphan2")
	// Should NOT remove active or non-tenant networks
	assert.NotContains(t, mock.RemoveNetworkCalls, "ah-tenant-active")
	assert.NotContains(t, mock.RemoveNetworkCalls, "bridge")
}

func TestCollectOnce_SkipsNetworksWithActiveContainers(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{
		NetworkListFn: func(ctx context.Context) ([]docker.NetworkInfo, error) {
			return []docker.NetworkInfo{
				{ID: "net-1", Name: "ah-tenant-busy", Containers: 3},
			}, nil
		},
	}

	g := New(stateDB, mock, 5*time.Minute, t.TempDir())
	err := g.collectOnce(context.Background())
	require.NoError(t, err)

	assert.Empty(t, mock.NetworkDisconnectCalls, "should not disconnect from busy networks")
	assert.Empty(t, mock.RemoveNetworkCalls, "should not remove busy networks")
}

func seedService(t *testing.T, stateDB *sql.DB, svcID, tenantID, containerID string) {
	t.Helper()
	_, err := stateDB.Exec(
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at) VALUES (?, ?, ?, 'active', 1, 1)`,
		tenantID, "Test", "test@test.com",
	)
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`, tenantID)
	if err != nil {
		t.Fatalf("seed quota: %v", err)
	}
	_, err = stateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, container_id, created_at, updated_at)
		 VALUES (?, ?, 'test', 'running', 'nginx', 8080, ?, 1, 1)`,
		svcID, tenantID, containerID,
	)
	if err != nil {
		t.Fatalf("seed service: %v", err)
	}
}
