package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// DELETE /v1/databases/{id} wipes the volume and can run longer than the 30s
// short-route timeout, so it must not carry a request deadline.
func TestDatabaseDelete_NoRequestDeadline(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	var hasDeadline bool
	dbMgr := &fakeDatabaseManager{deleteFn: func(ctx context.Context, _, _ string) error {
		_, hasDeadline = ctx.Deadline()
		return nil
	}}
	srv := NewServer(ServerConfig{
		Store: &db.Store{StateDB: stateDB}, MasterKey: masterKey, DevMode: true, DatabaseManager: dbMgr,
	})
	req := httptest.NewRequest(http.MethodDelete, "/v1/databases/db-1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
	assert.False(t, hasDeadline, "database delete must not run under the 30s short-route timeout")
}
