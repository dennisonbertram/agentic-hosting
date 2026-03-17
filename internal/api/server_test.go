package api

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
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
}

func (f *fakeDatabaseManager) Create(ctx context.Context, tenantID string, req databases.CreateRequest) (*databases.Database, error) {
	f.createCalls++
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

func TestIsUserError_AcceptsDynamicDatabaseQuotaMessages(t *testing.T) {
	assert.True(t, isUserError(errString("database quota exceeded (max 1)")))
	assert.False(t, isUserError(errString("unexpected failure")))
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

type errString string

func (e errString) Error() string { return string(e) }
