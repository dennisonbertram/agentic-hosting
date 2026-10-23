package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func suspendRequest(ip, tenantID, token string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/tenants/"+tenantID+"/suspend", nil)
	req.Header.Set("X-Bootstrap-Token", token)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Real-Ip", ip)
	return req
}

func TestHandleTenantSuspend(t *testing.T) {
	resetLimitersForTest()
	t.Cleanup(resetLimitersForTest)
	const bootstrapToken = "test-bootstrap-token-for-suspend"
	masterKey := []byte("0123456789abcdef0123456789abcdef")

	t.Run("suspends active tenant and revokes keys", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		token := seedAuthenticatedTenant(t, stateDB, masterKey)
		dbMgr := &fakeDatabaseManager{}
		kanbanMgr := &fakeKanbanManager{}
		srv := NewServer(ServerConfig{
			Store:           &db.Store{StateDB: stateDB},
			MasterKey:       masterKey,
			DevMode:         true,
			BootstrapTokens: []string{bootstrapToken},
			DatabaseManager: dbMgr,
			KanbanManager:   kanbanMgr,
		})

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, suspendRequest("10.3.0.1", "tenant-1", bootstrapToken))
		require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())

		var status string
		require.NoError(t, stateDB.QueryRow(`SELECT status FROM tenants WHERE id = 'tenant-1'`).Scan(&status))
		assert.Equal(t, "suspended", status)

		var live int
		require.NoError(t, stateDB.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE tenant_id = 'tenant-1' AND revoked_at IS NULL`).Scan(&live))
		assert.Equal(t, 0, live)

		deadline := time.After(2 * time.Second)
		for {
			if len(dbMgr.stopAllForTenantCalls) > 0 && len(kanbanMgr.stopForTenantCalls) > 0 {
				break
			}
			select {
			case <-deadline:
				t.Fatal("timed out waiting for workload stop")
			case <-time.After(10 * time.Millisecond):
			}
		}
		assert.Equal(t, []string{"tenant-1"}, dbMgr.stopAllForTenantCalls)
		assert.Equal(t, []string{"tenant-1"}, kanbanMgr.stopForTenantCalls)

		// The tenant's own key must stop working immediately.
		get := httptest.NewRequest(http.MethodGet, "/v1/tenant", nil)
		get.Header.Set("Authorization", "Bearer "+token)
		getRR := httptest.NewRecorder()
		srv.ServeHTTP(getRR, get)
		assert.Equal(t, http.StatusUnauthorized, getRR.Code)
	})

	t.Run("already suspended returns 409", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		_, err := stateDB.Exec(
			`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
			 VALUES ('tenant-susp', 'S', 's@example.com', 'suspended', 1, 1)`)
		require.NoError(t, err)
		srv := NewServer(ServerConfig{
			Store:           &db.Store{StateDB: stateDB},
			MasterKey:       masterKey,
			DevMode:         true,
			BootstrapTokens: []string{bootstrapToken},
		})
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, suspendRequest("10.3.0.2", "tenant-susp", bootstrapToken))
		assert.Equal(t, http.StatusConflict, rr.Code)
		assert.Contains(t, rr.Body.String(), "already suspended")
	})

	t.Run("missing tenant returns 404", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		srv := NewServer(ServerConfig{
			Store:           &db.Store{StateDB: stateDB},
			MasterKey:       masterKey,
			DevMode:         true,
			BootstrapTokens: []string{bootstrapToken},
		})
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, suspendRequest("10.3.0.3", "no-such", bootstrapToken))
		assert.Equal(t, http.StatusNotFound, rr.Code)
	})

	t.Run("wrong token returns 401", func(t *testing.T) {
		stateDB := testutil.NewStateDB(t)
		srv := NewServer(ServerConfig{
			Store:           &db.Store{StateDB: stateDB},
			MasterKey:       masterKey,
			DevMode:         true,
			BootstrapTokens: []string{bootstrapToken},
		})
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, suspendRequest("10.3.0.4", "tenant-1", "wrong-token"))
		assert.Equal(t, http.StatusUnauthorized, rr.Code)
	})
}
