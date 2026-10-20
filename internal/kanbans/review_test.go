package kanbans

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveVikunja starts a fake Vikunja on port. onProjects runs inside the
// project-creation handler; buckets records bucket titles created.
func serveVikunja(t *testing.T, port int, onProjects func(), buckets *[]string, mu *sync.Mutex) {
	t.Helper()
	mux := http.NewServeMux()
	ok := func(v any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(v) }
	}
	mux.HandleFunc("/api/v1/info", ok(map[string]string{"version": "test"}))
	mux.HandleFunc("/api/v1/register", ok(map[string]any{"id": 1}))
	mux.HandleFunc("/api/v1/login", ok(map[string]any{"token": "jwt"}))
	mux.HandleFunc("/api/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		if onProjects != nil {
			onProjects()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 7})
	})
	mux.HandleFunc("/api/v1/projects/7/views", ok([]map[string]any{
		{"id": 1, "view_kind": "list"}, {"id": 4, "view_kind": "kanban"},
	}))
	mux.HandleFunc("/api/v1/projects/7/views/4/buckets", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Title string `json:"title"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		*buckets = append(*buckets, body.Title)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	})
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err)
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: mux}}
	srv.Start()
	t.Cleanup(srv.Close)
}

// newIsolatedManager returns a test manager whose port range is a single
// OS-assigned free port, so a fake Vikunja for this test cannot collide with
// another test's server.
func newIsolatedManager(t *testing.T, stateDB *sql.DB, mock *testutil.MockDockerClient) *Manager {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return newTestManagerWithPortRange(t, stateDB, mock, port, port)
}

func TestProvision_CreatesDefaultBucketsAndStoresCredentials(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)
	var mu sync.Mutex
	var buckets []string
	mock := &testutil.MockDockerClient{
		RunDatabaseFn: func(_ context.Context, cfg docker.RunDatabaseConfig) (string, error) {
			serveVikunja(t, cfg.HostPort, nil, &buckets, &mu)
			return "c-ok", nil
		},
	}
	mgr := newIsolatedManager(t, stateDB, mock)

	_, err := mgr.Create(context.Background(), "tenant-1")
	require.NoError(t, err)
	mgr.waitBackground()
	waitForStatus(t, stateDB, "tenant-1", "ready", time.Second)

	mu.Lock()
	assert.Equal(t, []string{"Backlog", "In Progress", "In Review", "Done"}, buckets)
	mu.Unlock()
	tok, err := mgr.GetAdminToken(context.Background(), "tenant-1")
	require.NoError(t, err)
	assert.Len(t, tok, 64)
}

// A tenant suspension that lands while setup is running must leave the board
// 'stopped' (so reactivation restarts it) with its volume intact.
func TestProvision_SuspendedDuringSetupStaysStopped(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)
	var mu sync.Mutex
	var buckets []string
	mock := &testutil.MockDockerClient{}
	mgr := newIsolatedManager(t, stateDB, mock)
	mock.RunDatabaseFn = func(_ context.Context, cfg docker.RunDatabaseConfig) (string, error) {
		serveVikunja(t, cfg.HostPort, func() {
			_, err := stateDB.Exec(`UPDATE kanbans SET status = 'stopped' WHERE tenant_id = 'tenant-1'`)
			assert.NoError(t, err)
		}, &buckets, &mu)
		return "c-susp", nil
	}

	_, err := mgr.Create(context.Background(), "tenant-1")
	require.NoError(t, err)

	mgr.waitBackground() // provision has made its final transition

	// Setup completed (credentials stored), so the compare-and-set was reached.
	_, err = mgr.GetAdminToken(context.Background(), "tenant-1")
	require.NoError(t, err, "provision must have finished setup")

	k, err := mgr.Get(context.Background(), "tenant-1")
	require.NoError(t, err)
	assert.Equal(t, "stopped", k.Status, "provision must not flip a suspended board to ready")
	assert.Empty(t, mock.RemoveVolumeCalls, "set-up board data must be kept")
}

func TestStopForTenant_FailedBoardStaysFailed(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)
	_, err := stateDB.Exec(`INSERT INTO kanbans (id, tenant_id, status, container_id, host, port, volume_name, url, created_at, updated_at)
		VALUES ('kf0123456789abcdef', 'tenant-1', 'failed', 'c-left', '127.0.0.1', 7300, 'vol', 'u', 1, 1)`)
	require.NoError(t, err)
	mock := &testutil.MockDockerClient{}
	mgr := newTestManager(t, stateDB, mock)

	mgr.StopForTenant(context.Background(), "tenant-1")
	mgr.StartForTenant(context.Background(), "tenant-1")

	assert.Equal(t, []string{"c-left"}, mock.RemoveContainerCalls)
	assert.Zero(t, mock.RunDatabaseCalls, "a failed board must not be restarted on reactivation")
	k, err := mgr.Get(context.Background(), "tenant-1")
	require.NoError(t, err)
	assert.Equal(t, "failed", k.Status)
	assert.Empty(t, k.ContainerID)
}

func TestStopForTenant_NoBoardIsNoop(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)
	mock := &testutil.MockDockerClient{}
	newTestManager(t, stateDB, mock).StopForTenant(context.Background(), "tenant-1")
	assert.Empty(t, mock.StopContainerCalls)
}

// A client that disconnects mid-create must not leak the container or leave a
// 'provisioning' row that blocks re-create.
func TestCreate_RequestCancelCleansUp(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)
	ctx, cancel := context.WithCancel(context.Background())
	mock := &testutil.MockDockerClient{
		RunDatabaseFn: func(context.Context, docker.RunDatabaseConfig) (string, error) {
			cancel()
			return "c-cancel", nil
		},
		RemoveContainerFn: func(c context.Context, _ string) error { return c.Err() },
	}
	mgr := newTestManager(t, stateDB, mock)

	_, err := mgr.Create(ctx, "tenant-1")
	require.ErrorContains(t, err, "context canceled")
	assert.Contains(t, mock.RemoveContainerCalls, "c-cancel")
	assert.Len(t, mock.RemoveVolumeCalls, 1)
	k, err := mgr.Get(context.Background(), "tenant-1")
	require.NoError(t, err)
	assert.Equal(t, "failed", k.Status)
}

func TestCreate_RunFailureMarksFailed(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)
	mock := &testutil.MockDockerClient{
		RunDatabaseFn: func(context.Context, docker.RunDatabaseConfig) (string, error) {
			return "", errors.New("boom")
		},
	}
	mgr := newTestManager(t, stateDB, mock)
	_, err := mgr.Create(context.Background(), "tenant-1")
	require.ErrorContains(t, err, "run container")
	assert.Len(t, mock.RemoveVolumeCalls, 1)
	k, err := mgr.Get(context.Background(), "tenant-1")
	require.NoError(t, err)
	assert.Equal(t, "failed", k.Status)
}

func TestCreate_RejectsNonDNSTenantID(t *testing.T) {
	mgr := newTestManager(t, testutil.NewStateDB(t), &testutil.MockDockerClient{})
	_, err := mgr.Create(context.Background(), "Bad_Tenant")
	var ae *apierr.APIError
	require.True(t, errors.As(err, &ae), "want an API validation error, got %v", err)
	assert.Contains(t, err.Error(), "DNS")
}

func TestGetAdminToken_NotFoundAndUnavailable(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)
	mgr := newTestManager(t, stateDB, &testutil.MockDockerClient{})

	_, err := mgr.GetAdminToken(context.Background(), "tenant-1")
	assert.ErrorContains(t, err, "kanban board not found")

	_, err = stateDB.Exec(`INSERT INTO kanbans (id, tenant_id, status, host, port, volume_name, url, created_at, updated_at)
		VALUES ('kp', 'tenant-1', 'provisioning', '127.0.0.1', 7301, 'vol', 'u', 1, 1)`)
	require.NoError(t, err)
	_, err = mgr.GetAdminToken(context.Background(), "tenant-1")
	assert.ErrorContains(t, err, "admin token not available")
}

func TestDelete_KeepsRecordWhenVolumeRemovalFails(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedKanbanTestData(t, stateDB)
	_, err := stateDB.Exec(`INSERT INTO kanbans (id, tenant_id, status, container_id, host, port, volume_name, url, created_at, updated_at)
		VALUES ('kd', 'tenant-1', 'ready', 'c-1', '127.0.0.1', 7302, 'vol', 'u', 1, 1)`)
	require.NoError(t, err)
	mock := &testutil.MockDockerClient{
		RemoveVolumeFn: func(context.Context, string) error { return errors.New("volume is in use") },
	}
	mgr := newTestManager(t, stateDB, mock)

	err = mgr.Delete(context.Background(), "tenant-1")
	require.ErrorContains(t, err, "failed to remove kanban volume")
	_, err = mgr.Get(context.Background(), "tenant-1")
	assert.NoError(t, err, "record must survive a failed volume removal")
}

func TestContainerNameFor(t *testing.T) {
	assert.Equal(t, "ah-kanban-abcdefgh-0123456789abcdef", containerNameFor("abcdefghijk", "0123456789abcdef0123"))
	assert.Equal(t, "ah-kanban-t-k", containerNameFor("t", "k"))
	assert.True(t, strings.HasPrefix(containerNameFor("tenant-1", "id"), "ah-kanban-tenant-1"))
}
