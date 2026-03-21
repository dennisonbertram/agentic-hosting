package api

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureLog redirects the standard logger to a buffer for the duration of a
// test so we can assert on structured AUDIT lines. Returns a function that
// restores the original output and the buffer contents.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	original := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(original) })
	return &buf
}

func TestAudit_ConnectionStringAccess(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)
	logBuf := captureLog(t)

	// The fake database manager returns a connection string without error.
	dbMgr := &fakeDatabaseManager{}
	srv := NewServer(ServerConfig{
		Store:           &db.Store{StateDB: stateDB},
		MasterKey:       masterKey,
		DevMode:         true,
		DatabaseManager: dbMgr,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/databases/db-42/connection-string", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)

	logOutput := logBuf.String()
	assert.Contains(t, logOutput, "AUDIT:")
	assert.Contains(t, logOutput, "action=database.connection_string_accessed")
	assert.Contains(t, logOutput, "tenant=tenant-1")
	assert.Contains(t, logOutput, "database=db-42")
	assert.Contains(t, logOutput, "api_key=")
}

func TestAudit_KanbanAdminTokenAccess(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)
	logBuf := captureLog(t)

	kanbanMgr := &fakeKanbanManager{}
	srv := NewServer(ServerConfig{
		Store:         &db.Store{StateDB: stateDB},
		MasterKey:     masterKey,
		DevMode:       true,
		KanbanManager: kanbanMgr,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/kanban/admin-token", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)

	logOutput := logBuf.String()
	assert.Contains(t, logOutput, "AUDIT:")
	assert.Contains(t, logOutput, "action=kanban.admin_token_accessed")
	assert.Contains(t, logOutput, "tenant=tenant-1")
	assert.Contains(t, logOutput, "api_key=")
}

func TestAudit_EnvReveal(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	// Insert a service so the env handler can find it.
	_, err := stateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, container_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-env-1", "tenant-1", "web", "running", "nginx:latest", 8080, "ctr-1", 1, 1,
	)
	require.NoError(t, err)

	dockerClient := &testutil.MockDockerClient{}
	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
		Docker:    dockerClient,
	})

	t.Run("reveal=true produces audit log", func(t *testing.T) {
		logBuf := captureLog(t)

		req := httptest.NewRequest(http.MethodGet, "/v1/services/svc-env-1/env?reveal=true", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code)

		logOutput := logBuf.String()
		assert.Contains(t, logOutput, "AUDIT:")
		assert.Contains(t, logOutput, "action=env.revealed")
		assert.Contains(t, logOutput, "tenant=tenant-1")
		assert.Contains(t, logOutput, "service=svc-env-1")
		assert.Contains(t, logOutput, "api_key=")
	})

	t.Run("reveal=false does NOT produce audit log", func(t *testing.T) {
		logBuf := captureLog(t)

		req := httptest.NewRequest(http.MethodGet, "/v1/services/svc-env-1/env", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code)

		logOutput := logBuf.String()
		// The handler-level audit log for env.revealed should NOT appear
		// when reveal is not true.
		assert.False(t, strings.Contains(logOutput, "action=env.revealed"),
			"env.revealed audit should not appear when reveal=false")
	})
}

func TestAudit_KeyIDInContext(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)
	logBuf := captureLog(t)

	// Extract the key ID from the token (format: "keyID.secret")
	parts := strings.SplitN(token, ".", 2)
	expectedKeyID := parts[0]

	dbMgr := &fakeDatabaseManager{}
	srv := NewServer(ServerConfig{
		Store:           &db.Store{StateDB: stateDB},
		MasterKey:       masterKey,
		DevMode:         true,
		DatabaseManager: dbMgr,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/databases/db-99/connection-string", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)

	logOutput := logBuf.String()
	assert.Contains(t, logOutput, "api_key="+expectedKeyID,
		"audit log should contain the exact API key ID used for the request")
}
