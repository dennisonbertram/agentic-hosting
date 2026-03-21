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

// waitForStatus polls the database until the kanban reaches the desired status
// or the timeout expires.
func waitForStatus(t *testing.T, db *sql.DB, tenantID, wantStatus string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var status string
		err := db.QueryRow(`SELECT status FROM kanbans WHERE tenant_id = ?`, tenantID).Scan(&status)
		if err == nil && status == wantStatus {
			return status
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Return whatever we have at timeout
	var status string
	_ = db.QueryRow(`SELECT status FROM kanbans WHERE tenant_id = ?`, tenantID).Scan(&status)
	t.Fatalf("timed out waiting for status %q, got %q", wantStatus, status)
	return status
}

func TestCreate_ReturnsProvisioningImmediately(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)

	dockerClient := &testutil.MockDockerClient{
		RunDatabaseFn: func(ctx context.Context, cfg docker.RunDatabaseConfig) (string, error) {
			startFakeVikunja(t, cfg.HostPort)
			return "container-" + cfg.Name, nil
		},
	}

	mgr := newTestManager(t, stateDB, dockerClient)

	kb, err := mgr.Create(context.Background(), "tenant-1")
	require.NoError(t, err)
	require.NotNil(t, kb)

	// Create returns immediately with "provisioning" status — no credentials yet
	assert.Equal(t, "provisioning", kb.Status)
	assert.Equal(t, "tenant-1", kb.TenantID)
	assert.NotEmpty(t, kb.ID)
	assert.Nil(t, kb.Credentials, "credentials must not be returned on create")
	assert.Equal(t, "tenant-1.kanban.agentic.hosting", kb.URL)
	assert.True(t, kb.Port >= 7100 && kb.Port <= 7500)

	// Verify Docker calls happened synchronously (container started before return)
	assert.Equal(t, 1, dockerClient.RunDatabaseCalls)
	assert.Len(t, dockerClient.CreateVolumeCalls, 1)
	assert.Contains(t, dockerClient.CreateVolumeCalls[0], "ah-kanban-")
}

func TestCreate_BackgroundProvisioningCompletes(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)

	dockerClient := &testutil.MockDockerClient{
		RunDatabaseFn: func(ctx context.Context, cfg docker.RunDatabaseConfig) (string, error) {
			startFakeVikunja(t, cfg.HostPort)
			return "container-" + cfg.Name, nil
		},
	}

	mgr := newTestManager(t, stateDB, dockerClient)

	kb, err := mgr.Create(context.Background(), "tenant-1")
	require.NoError(t, err)
	assert.Equal(t, "provisioning", kb.Status)

	// Poll until background goroutine finishes and marks status "ready"
	status := waitForStatus(t, stateDB, "tenant-1", "ready", 10*time.Second)
	assert.Equal(t, "ready", status)

	// Verify admin token was stored
	got, err := mgr.Get(context.Background(), "tenant-1")
	require.NoError(t, err)
	assert.Equal(t, "ready", got.Status)

	// Admin token should be retrievable after provisioning completes
	token, err := mgr.GetAdminToken(context.Background(), "tenant-1")
	require.NoError(t, err)
	assert.NotEmpty(t, token)
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

	// Second create should return Conflict (even while first is still provisioning)
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

	// Wait for provisioning to complete
	waitForStatus(t, stateDB, "tenant-1", "ready", 10*time.Second)

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

func TestGet_ProvisioningStatus(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)

	// Use a channel to block the fake Vikunja from responding until we check status
	healthGate := make(chan struct{})

	dockerClient := &testutil.MockDockerClient{
		RunDatabaseFn: func(ctx context.Context, cfg docker.RunDatabaseConfig) (string, error) {
			// Start a fake Vikunja that blocks on health check until released
			mux := http.NewServeMux()
			mux.HandleFunc("/api/v1/info", func(w http.ResponseWriter, r *http.Request) {
				<-healthGate // Block until test releases the gate
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
			})

			listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.HostPort))
			if err != nil {
				return "", err
			}
			srv := &httptest.Server{
				Listener: listener,
				Config:   &http.Server{Handler: mux},
			}
			srv.Start()
			t.Cleanup(srv.Close)

			return "container-" + cfg.Name, nil
		},
	}

	mgr := newTestManager(t, stateDB, dockerClient)

	kb, err := mgr.Create(context.Background(), "tenant-1")
	require.NoError(t, err)
	assert.Equal(t, "provisioning", kb.Status)

	// GET while still provisioning should return "provisioning"
	got, err := mgr.Get(context.Background(), "tenant-1")
	require.NoError(t, err)
	assert.Equal(t, "provisioning", got.Status)

	// Release the health check gate
	close(healthGate)

	// Wait for background provisioning to complete
	waitForStatus(t, stateDB, "tenant-1", "ready", 10*time.Second)

	// Now GET should return "ready"
	got2, err := mgr.Get(context.Background(), "tenant-1")
	require.NoError(t, err)
	assert.Equal(t, "ready", got2.Status)
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

	// Wait for provisioning to complete before deleting
	waitForStatus(t, stateDB, "tenant-1", "ready", 10*time.Second)

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
