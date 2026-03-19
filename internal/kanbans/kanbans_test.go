package kanbans

import (
	"context"
	"database/sql"
	"encoding/json"
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

func seedKanbanTestData(t *testing.T, stateDB *sql.DB) {
	t.Helper()
	_, err := stateDB.Exec(
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at) VALUES (?, ?, ?, 'active', 1, 1)`,
		"tenant-1", "Tenant", "tenant@example.com",
	)
	require.NoError(t, err)
}

// startFakeVikunja starts a minimal HTTP server that mimics the Vikunja API
// endpoints needed for health check and setup. Returns port and cleanup func.
func startFakeVikunja(t *testing.T, port int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/info", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"version": "test"})
	})
	mux.HandleFunc("/api/v1/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{"id": 1, "username": "admin"})
	})
	mux.HandleFunc("/api/v1/login", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{"token": "test-jwt-token"})
	})
	mux.HandleFunc("/api/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{"id": 1, "title": "test"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err)

	srv := &httptest.Server{
		Listener: listener,
		Config:   &http.Server{Handler: mux},
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func newTestManager(t *testing.T, stateDB *sql.DB, dockerClient *testutil.MockDockerClient) *Manager {
	t.Helper()
	mgr := &Manager{
		db:                 stateDB,
		docker:             dockerClient,
		masterKey:          []byte("0123456789abcdef0123456789abcdef"),
		healthCheckTimeout: 5 * time.Second,
		baseURL:            "http://127.0.0.1",
	}
	// Don't call ReconcileStale in tests — no stale records to reconcile
	return mgr
}

func TestCreate_Success(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)

	dockerClient := &testutil.MockDockerClient{
		RunDatabaseFn: func(ctx context.Context, cfg docker.RunDatabaseConfig) (string, error) {
			// Start a fake Vikunja server on the allocated port
			startFakeVikunja(t, cfg.HostPort)
			return "container-" + cfg.Name, nil
		},
	}

	mgr := newTestManager(t, stateDB, dockerClient)

	kb, err := mgr.Create(context.Background(), "tenant-1")
	require.NoError(t, err)
	require.NotNil(t, kb)

	assert.Equal(t, "ready", kb.Status)
	assert.Equal(t, "tenant-1", kb.TenantID)
	assert.NotEmpty(t, kb.ID)
	require.NotNil(t, kb.Credentials)
	assert.Equal(t, "admin", kb.Credentials.Username)
	assert.NotEmpty(t, kb.Credentials.Password)
	assert.NotEmpty(t, kb.Credentials.JWT)
	assert.True(t, kb.Credentials.SetupSuccess)
	assert.Equal(t, "tenant-1.kanban.agentic.hosting", kb.URL)
	assert.True(t, kb.Port >= 7100 && kb.Port <= 7500)

	// Verify Docker calls
	assert.Equal(t, 1, dockerClient.RunDatabaseCalls)
	assert.Len(t, dockerClient.CreateVolumeCalls, 1)
	assert.Contains(t, dockerClient.CreateVolumeCalls[0], "ah-kanban-")

	// Verify DB record
	var status string
	err = stateDB.QueryRow(`SELECT status FROM kanbans WHERE tenant_id = ?`, "tenant-1").Scan(&status)
	require.NoError(t, err)
	assert.Equal(t, "ready", status)
}

func TestCreate_AlreadyExists(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)

	dockerClient := &testutil.MockDockerClient{
		RunDatabaseFn: func(ctx context.Context, cfg docker.RunDatabaseConfig) (string, error) {
			startFakeVikunja(t, cfg.HostPort)
			return "container-" + cfg.Name, nil
		},
	}

	mgr := newTestManager(t, stateDB, dockerClient)

	// First create should succeed
	_, err := mgr.Create(context.Background(), "tenant-1")
	require.NoError(t, err)

	// Second create should return Conflict
	kb2, err := mgr.Create(context.Background(), "tenant-1")
	require.Error(t, err)
	assert.Nil(t, kb2)
	assert.ErrorContains(t, err, "already has a kanban board")
}

func TestGet_Success(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)

	dockerClient := &testutil.MockDockerClient{
		RunDatabaseFn: func(ctx context.Context, cfg docker.RunDatabaseConfig) (string, error) {
			startFakeVikunja(t, cfg.HostPort)
			return "container-" + cfg.Name, nil
		},
	}

	mgr := newTestManager(t, stateDB, dockerClient)

	created, err := mgr.Create(context.Background(), "tenant-1")
	require.NoError(t, err)

	got, err := mgr.Get(context.Background(), "tenant-1")
	require.NoError(t, err)
	assert.Equal(t, created.ID, got.ID)
	assert.Equal(t, "ready", got.Status)
	assert.Equal(t, "tenant-1", got.TenantID)
	// Credentials should NOT be populated on Get
	assert.Nil(t, got.Credentials)
}

func TestGet_NotFound(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)

	dockerClient := &testutil.MockDockerClient{}
	mgr := newTestManager(t, stateDB, dockerClient)

	kb, err := mgr.Get(context.Background(), "tenant-1")
	require.Error(t, err)
	assert.Nil(t, kb)
	assert.ErrorContains(t, err, "not found")
}

func TestDelete_Success(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)

	dockerClient := &testutil.MockDockerClient{
		RunDatabaseFn: func(ctx context.Context, cfg docker.RunDatabaseConfig) (string, error) {
			startFakeVikunja(t, cfg.HostPort)
			return "container-" + cfg.Name, nil
		},
	}

	mgr := newTestManager(t, stateDB, dockerClient)

	_, err := mgr.Create(context.Background(), "tenant-1")
	require.NoError(t, err)

	err = mgr.Delete(context.Background(), "tenant-1")
	require.NoError(t, err)

	// Verify Docker cleanup
	assert.Len(t, dockerClient.StopContainerCalls, 1)
	assert.Len(t, dockerClient.RemoveContainerCalls, 1)
	assert.Len(t, dockerClient.RemoveVolumeCalls, 1)

	// Verify DB record deleted
	var count int
	err = stateDB.QueryRow(`SELECT COUNT(*) FROM kanbans WHERE tenant_id = ?`, "tenant-1").Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func TestDelete_NotFound(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)

	dockerClient := &testutil.MockDockerClient{}
	mgr := newTestManager(t, stateDB, dockerClient)

	err := mgr.Delete(context.Background(), "tenant-1")
	require.Error(t, err)
	assert.ErrorContains(t, err, "not found")
}
