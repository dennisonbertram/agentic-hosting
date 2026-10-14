package api

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/crypto"
	"github.com/dennisonbertram/agentic-hosting/internal/databases"
	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedTenantWithKey creates an active tenant with the given api_rate_limit and
// returns a bearer token for it.
func seedTenantWithKey(t *testing.T, stateDB *sql.DB, masterKey []byte, id string, rateLimit int) string {
	t.Helper()
	_, err := stateDB.Exec(
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at) VALUES (?, ?, ?, 'active', 1, 1)`,
		id, id, id+"@example.com")
	require.NoError(t, err)
	_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id, api_rate_limit) VALUES (?, ?)`, id, rateLimit)
	require.NoError(t, err)
	secret, keyID, err := crypto.GenerateAPIKeyWithID()
	require.NoError(t, err)
	_, err = stateDB.Exec(
		`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, created_at) VALUES (?, ?, 'default', ?, ?, 1)`,
		keyID, id, keyID[:8], crypto.HashAPIKey(secret, masterKey))
	require.NoError(t, err)
	return keyID + "." + secret
}

func postDB(srv *Server, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/databases", bytes.NewReader([]byte(`{"name":"d","type":"postgres"}`)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestLongRoutes_PerTenantCapDoesNotStarveOthers(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	tokenA := seedTenantWithKey(t, stateDB, masterKey, "tenant-a", 100)
	tokenB := seedTenantWithKey(t, stateDB, masterKey, "tenant-b", 100)

	release := make(chan struct{})
	entered := make(chan struct{}, maxLongPerTenant)
	dbMgr := &fakeDatabaseManager{
		createFn: func(ctx context.Context, tenantID string, req databases.CreateRequest) (*databases.Database, error) {
			if tenantID == "tenant-a" {
				entered <- struct{}{}
				<-release
			}
			return &databases.Database{ID: "db", TenantID: tenantID, Name: req.Name, Type: req.Type, Status: "ready"}, nil
		},
	}
	srv := NewServer(ServerConfig{
		Store: &db.Store{StateDB: stateDB}, MasterKey: masterKey, DevMode: true, DatabaseManager: dbMgr,
	})

	var wg sync.WaitGroup
	for i := 0; i < maxLongPerTenant; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); postDB(srv, tokenA) }()
		<-entered
	}

	over := postDB(srv, tokenA)
	assert.Equal(t, http.StatusTooManyRequests, over.Code, "tenant A's extra long request")
	assert.Contains(t, over.Body.String(), "concurrent")
	assert.Equal(t, http.StatusCreated, postDB(srv, tokenB).Code, "tenant B is unaffected")

	// Health (public, short route) still answers while A holds its slots.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/system/health", nil))
	assert.NotEqual(t, http.StatusTooManyRequests, rec.Code)

	close(release)
	wg.Wait()
}

func TestAuthenticatedRoutes_EnforceTenantAPIRateLimit(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedTenantWithKey(t, stateDB, masterKey, "tenant-slow", 1) // 1 rps -> burst 2
	srv := NewServer(ServerConfig{Store: &db.Store{StateDB: stateDB}, MasterKey: masterKey, DevMode: true})

	get := func() int {
		req := httptest.NewRequest(http.MethodGet, "/v1/tenant", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code
	}
	assert.Equal(t, http.StatusOK, get())
	assert.Equal(t, http.StatusOK, get())
	assert.Equal(t, http.StatusTooManyRequests, get(), "third request exceeds a limit of 1 rps (burst 2)")
}
