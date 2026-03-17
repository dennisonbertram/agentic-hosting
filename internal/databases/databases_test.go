package databases

import (
	"context"
	"database/sql"
	"net"
	"strconv"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreate_UsesTenantMaxDatabasesQuota(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedDatabaseTestData(t, stateDB, 1)

	var listeners []net.Listener
	t.Cleanup(func() {
		for _, ln := range listeners {
			_ = ln.Close()
		}
	})

	dockerClient := &testutil.MockDockerClient{
		RunDatabaseFn: func(ctx context.Context, cfg docker.RunDatabaseConfig) (string, error) {
			ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(cfg.HostPort))
			require.NoError(t, err)
			listeners = append(listeners, ln)
			return "container-" + cfg.Name, nil
		},
	}

	mgr := NewManager(stateDB, dockerClient, []byte("0123456789abcdef0123456789abcdef"))

	first, err := mgr.Create(context.Background(), "tenant-1", CreateRequest{Name: "db-1", Type: "postgres"})
	require.NoError(t, err)
	require.Equal(t, "ready", first.Status)

	second, err := mgr.Create(context.Background(), "tenant-1", CreateRequest{Name: "db-2", Type: "postgres"})
	require.Error(t, err)
	assert.Nil(t, second)
	assert.EqualError(t, err, "database quota exceeded (max 1)")
}

func seedDatabaseTestData(t *testing.T, stateDB *sql.DB, maxDatabases int) {
	t.Helper()
	_, err := stateDB.Exec(
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at) VALUES (?, ?, ?, 'active', 1, 1)`,
		"tenant-1", "Tenant", "tenant@example.com",
	)
	require.NoError(t, err)
	_, err = stateDB.Exec(
		`INSERT INTO tenant_quotas (tenant_id, max_databases) VALUES (?, ?)`,
		"tenant-1", maxDatabases,
	)
	require.NoError(t, err)
}
