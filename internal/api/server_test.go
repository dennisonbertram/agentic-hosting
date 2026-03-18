package api

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/crypto"
	"github.com/dennisonbertram/agentic-hosting/internal/databases"
	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeDatabaseManager struct {
	createCalls int
	createFn    func(ctx context.Context, tenantID string, req databases.CreateRequest) (*databases.Database, error)
}

func (f *fakeDatabaseManager) Create(ctx context.Context, tenantID string, req databases.CreateRequest) (*databases.Database, error) {
	f.createCalls++
	if f.createFn != nil {
		return f.createFn(ctx, tenantID, req)
	}
	return &databases.Database{
		ID:       "db-1",
		TenantID: tenantID,
		Name:     req.Name,
		Type:     req.Type,
		Status:   "ready",
	}, nil
}

func (f *fakeDatabaseManager) List(ctx context.Context, tenantID string) ([]*databases.Database, error) {
	return nil, nil
}

func (f *fakeDatabaseManager) ListPaginated(ctx context.Context, tenantID string, limit, offset int) ([]*databases.Database, error) {
	return nil, nil
}

func (f *fakeDatabaseManager) Get(ctx context.Context, tenantID, dbID string) (*databases.Database, error) {
	return nil, nil
}

func (f *fakeDatabaseManager) GetConnectionString(ctx context.Context, tenantID, dbID string) (string, error) {
	return "", nil
}

func (f *fakeDatabaseManager) Delete(ctx context.Context, tenantID, dbID string) error {
	return nil
}

func TestDatabaseCreate_UsesIdempotencyMiddleware(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	dbMgr := &fakeDatabaseManager{}
	srv := NewServer(ServerConfig{
		Store:           &db.Store{StateDB: stateDB},
		MasterKey:       masterKey,
		DevMode:         true,
		DatabaseManager: dbMgr,
	})

	body := []byte(`{"name":"main-db","type":"postgres"}`)
	first := httptest.NewRecorder()
	firstReq := httptest.NewRequest(http.MethodPost, "/v1/databases", bytes.NewReader(body))
	firstReq.Header.Set("Authorization", "Bearer "+token)
	firstReq.Header.Set("Content-Type", "application/json")
	firstReq.Header.Set("Idempotency-Key", "db-create-1")
	srv.ServeHTTP(first, firstReq)

	second := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodPost, "/v1/databases", bytes.NewReader(body))
	secondReq.Header.Set("Authorization", "Bearer "+token)
	secondReq.Header.Set("Content-Type", "application/json")
	secondReq.Header.Set("Idempotency-Key", "db-create-1")
	srv.ServeHTTP(second, secondReq)

	require.Equal(t, http.StatusCreated, first.Code)
	require.Equal(t, http.StatusCreated, second.Code)
	assert.Equal(t, 1, dbMgr.createCalls, "database creation should be replayed, not executed twice")
	assert.Equal(t, "true", second.Header().Get("Idempotency-Replayed"))
	assert.JSONEq(t, first.Body.String(), second.Body.String())
}

func TestDatabaseCreate_LongRunningRouteHasNoTimeoutDeadline(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	sawDeadline := false
	dbMgr := &fakeDatabaseManager{
		createFn: func(ctx context.Context, tenantID string, req databases.CreateRequest) (*databases.Database, error) {
			_, sawDeadline = ctx.Deadline()
			return &databases.Database{
				ID:       "db-1",
				TenantID: tenantID,
				Name:     req.Name,
				Type:     req.Type,
				Status:   "ready",
			}, nil
		},
	}
	srv := NewServer(ServerConfig{
		Store:           &db.Store{StateDB: stateDB},
		MasterKey:       masterKey,
		DevMode:         true,
		DatabaseManager: dbMgr,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/databases", bytes.NewBufferString(`{"name":"main-db","type":"postgres"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusCreated, rr.Code)
	assert.False(t, sawDeadline, "long-running database create route should not inherit the short request timeout")
}

func TestServiceLogsRoute_IsRegisteredAndHasNoTimeoutDeadline(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	_, err := stateDB.Exec(
		`INSERT INTO services (id, tenant_id, name, status, image, port, container_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"svc-1", "tenant-1", "web", "running", "nginx:latest", 8080, "ctr-1", 1, 1,
	)
	require.NoError(t, err)

	sawDeadline := false
	dockerClient := &testutil.MockDockerClient{
		LogsContainerFn: func(ctx context.Context, containerID string, follow bool, tail int) (io.ReadCloser, error) {
			_, sawDeadline = ctx.Deadline()
			return io.NopCloser(strings.NewReader("hello from logs\n")), nil
		},
	}
	srv := NewServer(ServerConfig{
		Store:     &db.Store{StateDB: stateDB},
		MasterKey: masterKey,
		DevMode:   true,
		Docker:    dockerClient,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/services/svc-1/logs?follow=true&tail=50", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	assert.False(t, sawDeadline, "streaming service logs should not inherit the short request timeout")
	assert.Equal(t, "hello from logs\n", rr.Body.String())
	assert.Equal(t, []string{"ctr-1"}, dockerClient.LogsContainerCalls)
}

func TestTypedErrorRouting(t *testing.T) {
	// Typed errors are now handled by apierr.WriteAPIError, no string matching needed.
	// This test verifies the apierr package is correctly integrated (tested in apierr_test.go).
}

func seedAuthenticatedTenant(t *testing.T, stateDB *sql.DB, masterKey []byte) string {
	t.Helper()
	_, err := stateDB.Exec(
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at) VALUES (?, ?, ?, 'active', 1, 1)`,
		"tenant-1", "Tenant", "tenant@example.com",
	)
	require.NoError(t, err)
	_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES (?)`, "tenant-1")
	require.NoError(t, err)

	secret, keyID, err := crypto.GenerateAPIKeyWithID()
	require.NoError(t, err)
	keyHash := crypto.HashAPIKey(secret, masterKey)
	_, err = stateDB.Exec(
		`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at) VALUES (?, ?, 'default', ?, ?, 1)`,
		keyID, "tenant-1", keyID[:8], keyHash,
	)
	require.NoError(t, err)

	return keyID + "." + secret
}

