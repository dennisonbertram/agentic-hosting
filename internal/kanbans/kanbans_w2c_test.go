package kanbans

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGet_NullContainerID(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)
	_, err := stateDB.Exec(
		`INSERT INTO kanbans (id, tenant_id, status, host, port, volume_name, url, created_at, updated_at)
		 VALUES ('k1', 'tenant-1', 'provisioning', '127.0.0.1', 7200, 'vol', 'u', 1, 1)`)
	require.NoError(t, err)
	mgr := newTestManager(t, stateDB, &testutil.MockDockerClient{})

	k, err := mgr.Get(context.Background(), "tenant-1")
	require.NoError(t, err)
	assert.Equal(t, "", k.ContainerID)

	mgr.ReconcileStale()
	var status string
	require.NoError(t, stateDB.QueryRow(`SELECT status FROM kanbans WHERE id='k1'`).Scan(&status))
	assert.Equal(t, "failed", status)
}

func TestCreate_ReplacesFailedRow(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)
	_, err := stateDB.Exec(
		`INSERT INTO kanbans (id, tenant_id, status, host, port, volume_name, url, created_at, updated_at)
		 VALUES ('kfail', 'tenant-1', 'failed', '127.0.0.1', 7200, 'vol', 'u', 1, 1)`)
	require.NoError(t, err)
	dockerClient := &testutil.MockDockerClient{
		RunDatabaseFn: func(ctx context.Context, cfg docker.RunDatabaseConfig) (string, error) {
			startFakeVikunja(t, cfg.HostPort)
			return "c-" + cfg.Name, nil
		},
	}
	mgr := newTestManager(t, stateDB, dockerClient)

	kb, err := mgr.Create(context.Background(), "tenant-1")
	require.NoError(t, err, "a failed kanban must not block re-create")
	assert.NotEqual(t, "kfail", kb.ID)
	waitForStatus(t, stateDB, "tenant-1", "ready", 10*time.Second)
}

func TestProvision_SetupFailureIsNotReady(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)
	dockerClient := &testutil.MockDockerClient{
		RunDatabaseFn: func(ctx context.Context, cfg docker.RunDatabaseConfig) (string, error) {
			mux := http.NewServeMux()
			mux.HandleFunc("/api/v1/info", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
			mux.HandleFunc("/api/v1/register", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
			ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.HostPort))
			require.NoError(t, err)
			srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: mux}}
			srv.Start()
			t.Cleanup(srv.Close)
			return "c-" + cfg.Name, nil
		},
	}
	mgr := newTestManager(t, stateDB, dockerClient)
	_, err := mgr.Create(context.Background(), "tenant-1")
	require.NoError(t, err)
	waitForStatus(t, stateDB, "tenant-1", "failed", 10*time.Second)
}

func TestStartForTenant_RecreatesStoppedKanban(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)
	var cfgs []docker.RunDatabaseConfig
	dockerClient := &testutil.MockDockerClient{
		RunDatabaseFn: func(ctx context.Context, cfg docker.RunDatabaseConfig) (string, error) {
			cfgs = append(cfgs, cfg)
			if len(cfgs) == 1 {
				startFakeVikunja(t, cfg.HostPort) // stays up across the restart
			}
			return fmt.Sprintf("c-%d", len(cfgs)), nil
		},
	}
	mgr := newTestManager(t, stateDB, dockerClient)
	_, err := mgr.Create(context.Background(), "tenant-1")
	require.NoError(t, err)
	waitForStatus(t, stateDB, "tenant-1", "ready", 10*time.Second)

	mgr.StopForTenant(context.Background(), "tenant-1")
	mgr.StartForTenant(context.Background(), "tenant-1")

	require.Len(t, cfgs, 2, "stopped kanban must be recreated")
	assert.Equal(t, cfgs[0].VolumeName, cfgs[1].VolumeName)
	assert.Equal(t, cfgs[0].HostPort, cfgs[1].HostPort)
	k, err := mgr.Get(context.Background(), "tenant-1")
	require.NoError(t, err)
	assert.Equal(t, "ready", k.Status)
	assert.Equal(t, "c-2", k.ContainerID)
}
