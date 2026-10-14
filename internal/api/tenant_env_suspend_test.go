package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/environments"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Suspending a tenant must stop its running environments.
func TestTenantDelete_StopsEnvironments(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	token := seedAuthenticatedTenant(t, stateDB, masterKey)

	_, err := stateDB.Exec(
		`INSERT INTO environments (id, tenant_id, name, template_id, status, container_id, volume_name, lease_duration_seconds, created_at, updated_at)
		 VALUES ('env1', 'tenant-1', 'e', 'tmpl_node', 'running', 'ctr-env1', 'ah-env-env1', 3600, 1, 1)`)
	require.NoError(t, err)

	mock := &testutil.MockDockerClient{}
	var stopped []string
	mock.StopContainerFn = func(_ context.Context, id string) error { stopped = append(stopped, id); return nil }

	srv := NewServer(ServerConfig{
		Store:              &db.Store{StateDB: stateDB},
		MasterKey:          masterKey,
		DevMode:            true,
		EnvironmentManager: environments.NewManager(stateDB, mock, "", ""),
	})
	req := httptest.NewRequest(http.MethodDelete, "/v1/tenant", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	require.Equal(t, http.StatusNoContent, rr.Code)

	assert.Equal(t, []string{"ctr-env1"}, stopped)
	var status string
	require.NoError(t, stateDB.QueryRow(`SELECT status FROM environments WHERE id='env1'`).Scan(&status))
	assert.Equal(t, "stopped", status)
}
