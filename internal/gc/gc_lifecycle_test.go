package gc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A build dir under a symlinked parent (macOS /var -> /private/var) must still
// be cleaned; previously every entry was rejected as "outside base".
func TestCleanOldBuildDirs_UnresolvedSymlinkedBase(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "builds-link")
	require.NoError(t, os.Symlink(real, link))

	oldDir := filepath.Join(real, "old-build")
	require.NoError(t, os.Mkdir(oldDir, 0o755))
	oldTime := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(oldDir, oldTime, oldTime))

	removed := (&GC{}).cleanOldBuildDirs(link, time.Hour)
	assert.Equal(t, 1, removed)
	assert.NoDirExists(t, oldDir)
}

func TestCleanOldBuildDirs_SkipsFilesAndDotDirsAndMissingBase(t *testing.T) {
	base := t.TempDir()
	oldTime := time.Now().Add(-2 * time.Hour)
	for _, name := range []string{".cache", "keep.txt"} {
		p := filepath.Join(base, name)
		if name == ".cache" {
			require.NoError(t, os.Mkdir(p, 0o755))
		} else {
			require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))
		}
		require.NoError(t, os.Chtimes(p, oldTime, oldTime))
	}
	g := &GC{}
	assert.Equal(t, 0, g.cleanOldBuildDirs(base, time.Hour))
	assert.DirExists(t, filepath.Join(base, ".cache"))
	assert.FileExists(t, filepath.Join(base, "keep.txt"))
	assert.Equal(t, 0, g.cleanOldBuildDirs(filepath.Join(base, "missing"), time.Hour))
}

func TestFindOrphanedVolumes_AllKinds(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedService(t, stateDB, "svc-x", "ten-x", "other")
	_, err := stateDB.Exec(`INSERT INTO environments (id, tenant_id, name, status, volume_name, created_at, updated_at)
		VALUES ('env-1', 'ten-x', 'e', 'running', 'ah-env-owned', 1, 1)`)
	require.NoError(t, err)
	_, err = stateDB.Exec(`INSERT INTO kanbans (id, tenant_id, status, host, port, volume_name, url, created_at, updated_at)
		VALUES ('kb-1', 'ten-x', 'running', '127.0.0.1', 7000, 'ah-kanban-owned', '', 1, 1)`)
	require.NoError(t, err)

	mock := &testutil.MockDockerClient{
		ListVolumesFn: func(ctx context.Context, prefix string) ([]string, error) {
			switch prefix {
			case "ah-db-":
				// Substring matches from Docker's name filter must be ignored.
				return []string{"ah-db-orphan", "x-ah-db-lookalike"}, nil
			case "ah-env-":
				return []string{"ah-env-owned", "ah-env-orphan"}, nil
			case "ah-kanban-":
				return []string{"ah-kanban-owned", "ah-kanban-orphan"}, nil
			}
			return nil, errors.New("pool list failed") // later kinds fail independently
		},
	}
	got, err := New(stateDB, mock, time.Minute, t.TempDir()).findOrphanedVolumes(context.Background())
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"ah-db-orphan", "ah-env-orphan", "ah-kanban-orphan"}, got)
}

func TestFindOrphanedVolumes_DatabaseListFailureAborts(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{
		ListVolumesFn: func(ctx context.Context, prefix string) ([]string, error) {
			if prefix == "ah-db-" {
				return nil, errors.New("daemon down")
			}
			return []string{prefix + "orphan"}, nil
		},
	}
	g := New(stateDB, mock, time.Minute, t.TempDir())
	_, err := g.findOrphanedVolumes(context.Background())
	require.Error(t, err)
	require.NoError(t, g.collectOnce(context.Background()))
	assert.Empty(t, mock.RemoveVolumeSafeCalls)
}

func TestFindOrphanedVolumes_DBErrorKeepsVolume(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	_, err := stateDB.Exec(`DROP TABLE kanbans`)
	require.NoError(t, err)
	mock := &testutil.MockDockerClient{
		ListVolumesFn: func(ctx context.Context, prefix string) ([]string, error) {
			if prefix == "ah-kanban-" {
				return []string{"ah-kanban-unknown"}, nil
			}
			return nil, nil
		},
	}
	got, err := New(stateDB, mock, time.Minute, t.TempDir()).findOrphanedVolumes(context.Background())
	require.NoError(t, err)
	assert.Empty(t, got, "ownership unknown: never orphaned")
}

// If the image reference tables cannot be read, no image may be removed.
func TestRemoveUnusedBuiltImages_FailsClosedOnReferenceError(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	_, err := stateDB.Exec(`DROP TABLE snapshots`)
	require.NoError(t, err)
	mock := &testutil.MockDockerClient{
		ListImagesFn: func(ctx context.Context) ([]docker.ImageInfo, error) {
			return []docker.ImageInfo{{ID: "sha:1", Tags: []string{"127.0.0.1:5000/snapshots/t:s"}, Created: time.Now().Add(-2 * time.Hour)}}, nil
		},
	}
	assert.Equal(t, 0, New(stateDB, mock, time.Minute, t.TempDir()).removeUnusedBuiltImages(context.Background()))
	assert.Empty(t, mock.RemoveImageCalls)
}

func TestRemoveUnusedBuiltImages_ListFailures(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	old := time.Now().Add(-2 * time.Hour)
	mock := &testutil.MockDockerClient{
		ListImagesFn: func(ctx context.Context) ([]docker.ImageInfo, error) {
			return []docker.ImageInfo{{ID: "sha:1", Tags: []string{"127.0.0.1:5000/ah/x:old"}, Created: old}}, nil
		},
		ListContainerImagesFn: func(ctx context.Context) ([]string, error) { return nil, errors.New("boom") },
	}
	g := New(stateDB, mock, time.Minute, t.TempDir())
	assert.Equal(t, 0, g.removeUnusedBuiltImages(context.Background()))
	mock.ListImagesFn = func(ctx context.Context) ([]docker.ImageInfo, error) { return nil, errors.New("boom") }
	assert.Equal(t, 0, g.removeUnusedBuiltImages(context.Background()))
	assert.Empty(t, mock.RemoveImageCalls)
}

func TestPersistedWorkloadNetworks_FailsClosedOnEachTable(t *testing.T) {
	for _, table := range []string{"environment_previews", "databases"} {
		t.Run(table, func(t *testing.T) {
			stateDB := testutil.NewStateDB(t)
			_, err := stateDB.Exec(`DROP TABLE ` + table)
			require.NoError(t, err)
			_, err = New(stateDB, &testutil.MockDockerClient{}, time.Minute, t.TempDir()).persistedWorkloadNetworks(context.Background())
			require.Error(t, err)
		})
	}
}

func TestFindOrphanedContainers_ListFailures(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{
		ListContainersByLabelFn: func(ctx context.Context, label, value string) ([]string, error) {
			if value == "kanban" {
				return nil, errors.New("optional list failed") // logged, others still scanned
			}
			if label == "ah.service" {
				return []string{"orphan"}, nil
			}
			return nil, nil
		},
		InspectContainerFn: oldInspect,
	}
	g := New(stateDB, mock, time.Minute, t.TempDir())
	got, err := g.findOrphanedContainers(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"orphan"}, got)

	mock.ListContainersByLabelFn = func(ctx context.Context, label, value string) ([]string, error) {
		if value == "database" {
			return nil, errors.New("required list failed")
		}
		return nil, nil
	}
	_, err = g.findOrphanedContainers(context.Background())
	require.Error(t, err)
}

func TestFindOrphanedContainers_InspectOrDBErrorKeepsContainer(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	_, err := stateDB.Exec(`DROP TABLE kanbans`)
	require.NoError(t, err)
	mock := &testutil.MockDockerClient{
		ListContainersByLabelFn: func(ctx context.Context, label, value string) ([]string, error) {
			if label == "ah.service" {
				return []string{"uninspectable", "unknown-owner"}, nil
			}
			return nil, nil
		},
		InspectContainerFn: func(ctx context.Context, id string) (*docker.ContainerInfo, error) {
			if id == "uninspectable" {
				return nil, errors.New("timeout")
			}
			return oldInspect(ctx, id)
		},
	}
	got, err := New(stateDB, mock, time.Minute, t.TempDir()).findOrphanedContainers(context.Background())
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestSafeCollect_RecoversFromPanic(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{
		ListContainersByLabelFn: func(ctx context.Context, label, value string) ([]string, error) {
			panic("boom")
		},
	}
	assert.NotPanics(t, func() {
		New(stateDB, mock, time.Minute, t.TempDir()).safeCollect(context.Background())
	})
}

func TestRun_ReturnsOnCancelDuringStartupDelay(t *testing.T) {
	g := New(testutil.NewStateDB(t), &testutil.MockDockerClient{}, time.Minute, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { g.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
