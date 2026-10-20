package databases

import (
	"context"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/crypto"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRewriteLoopbackAddress_RespectsPortBoundary(t *testing.T) {
	got, changed := rewriteLoopbackAddress("redis://:pw@127.0.0.1:54321/0", "127.0.0.1:5432", "ah-db-x:5432")
	assert.False(t, changed, "a longer port must not be rewritten")
	assert.Equal(t, "redis://:pw@127.0.0.1:54321/0", got)

	got, changed = rewriteLoopbackAddress("a=127.0.0.1:5432,b=127.0.0.1:54321,c=127.0.0.1:5432", "127.0.0.1:5432", "h:1")
	assert.True(t, changed)
	assert.Equal(t, "a=h:1,b=127.0.0.1:54321,c=h:1", got)

	got, changed = rewriteLoopbackAddress("x", "", "h:1")
	assert.False(t, changed)
	assert.Equal(t, "x", got)
}

// A failed row's host port can be reused by a live database of the same
// tenant; service_env values on that port must follow the live database.
func TestMigrateLoopbackURLs_FailedRowDoesNotClaimServiceEnv(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedDatabaseTestData(t, stateDB, 5)
	passEnc, err := crypto.Encrypt([]byte("pw"), testKey)
	require.NoError(t, err)
	for _, row := range []struct{ id, status string }{
		{"aaaafailed0000000000000000000000", "failed"},
		{"bbbblive000000000000000000000000", "ready"},
	} {
		_, err = stateDB.Exec(
			`INSERT INTO databases (id, tenant_id, name, type, status, host, port, db_name, username,
			 password_encrypted, volume_name, created_at, updated_at)
			 VALUES (?, 'tenant-1', ?, 'postgres', ?, '127.0.0.1', 5460, 'ah', 'ah', ?, 'vol', 10, 10)`,
			row.id, row.id, row.status, passEnc)
		require.NoError(t, err)
	}
	_, err = stateDB.Exec(`INSERT INTO services (id, tenant_id, name, status, image, port, created_at, updated_at)
		VALUES ('svc-1', 'tenant-1', 'app', 'running', 'nginx', 8080, 1, 1)`)
	require.NoError(t, err)
	envEnc, err := crypto.Encrypt([]byte("postgres://ah:pw@127.0.0.1:5460/ah"), testKey)
	require.NoError(t, err)
	_, err = stateDB.Exec(`INSERT INTO service_env (service_id, key, value_encrypted, created_at, updated_at)
		VALUES ('svc-1', 'DATABASE_URL', ?, 1, 1)`, envEnc)
	require.NoError(t, err)

	NewManager(stateDB, &testutil.MockDockerClient{}, testKey)

	var enc string
	require.NoError(t, stateDB.QueryRow(`SELECT value_encrypted FROM service_env WHERE service_id = 'svc-1'`).Scan(&enc))
	plain, err := crypto.Decrypt(enc, testKey)
	require.NoError(t, err)
	assert.Equal(t, "postgres://ah:pw@"+docker.DatabaseAlias("bbbblive000000000000000000000000")+":5432/ah", string(plain))
}

// A client that disconnects mid-provision must not leak the container or leave
// the row 'provisioning' (counting toward quota): cleanup runs on a context
// detached from the request.
func TestCreate_RequestCancelDuringProvisionCleansUp(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedDatabaseTestData(t, stateDB, 5)
	ctx, cancel := context.WithCancel(context.Background())
	mock := &testutil.MockDockerClient{
		RunDatabaseFn: func(context.Context, docker.RunDatabaseConfig) (string, error) {
			cancel() // request goes away while the engine is still starting
			return "c-unhealthy", nil
		},
		StopContainerFn:   func(c context.Context, _ string) error { return c.Err() },
		RemoveContainerFn: func(c context.Context, _ string) error { return c.Err() },
	}
	var removed []string
	mock.RemoveVolumeFn = func(c context.Context, name string) error {
		if c.Err() == nil {
			removed = append(removed, name)
		}
		return c.Err()
	}
	m := NewManager(stateDB, mock, testKey)
	m.healthTimeout = 200 * time.Millisecond

	_, err := m.Create(ctx, "tenant-1", CreateRequest{Name: "slow", Type: "redis"})
	require.ErrorContains(t, err, "context canceled")
	assert.Contains(t, mock.RemoveContainerCalls, "c-unhealthy")
	require.Len(t, removed, 1, "volume removal must run on a live context")

	var status string
	require.NoError(t, stateDB.QueryRow(`SELECT status FROM databases WHERE name = 'slow'`).Scan(&status))
	assert.Equal(t, "failed", status)
	assert.Empty(t, mock.RemoveNetworkCalls, "a failed row keeps its network until Delete")
}

func TestCreate_HealthFailureMarksFailedAndCleansUp(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedDatabaseTestData(t, stateDB, 5)
	mock := &testutil.MockDockerClient{
		RunDatabaseFn: func(context.Context, docker.RunDatabaseConfig) (string, error) { return "c-dead", nil },
	}
	m := NewManager(stateDB, mock, testKey)
	m.healthTimeout = 200 * time.Millisecond

	_, err := m.Create(context.Background(), "tenant-1", CreateRequest{Name: "dead", Type: "postgres"})
	require.ErrorContains(t, err, "health check failed after 200ms")
	assert.Contains(t, mock.RemoveContainerCalls, "c-dead")
	assert.Len(t, mock.RemoveVolumeCalls, 1)
	var status string
	require.NoError(t, stateDB.QueryRow(`SELECT status FROM databases WHERE name = 'dead'`).Scan(&status))
	assert.Equal(t, "failed", status)
}

// When Delete removes the row while Create is still provisioning, Create owns
// the cleanup, including the per-database network it created after Delete ran.
func TestCreate_RowDeletedDuringProvisioningRemovesNetwork(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedDatabaseTestData(t, stateDB, 5)
	mock := &testutil.MockDockerClient{}
	mock.RunDatabaseFn = func(context.Context, docker.RunDatabaseConfig) (string, error) {
		_, err := stateDB.Exec(`DELETE FROM databases WHERE name = 'gone'`)
		require.NoError(t, err)
		return "c-orphan", nil
	}
	m := NewManager(stateDB, mock, testKey)

	_, err := m.Create(context.Background(), "tenant-1", CreateRequest{Name: "gone", Type: "postgres"})
	require.ErrorContains(t, err, "deleted during provisioning")
	assert.Contains(t, mock.RemoveContainerCalls, "c-orphan")
	require.Len(t, mock.EnsureDatabaseNetworkCalls, 1)
	assert.Equal(t, mock.EnsureDatabaseNetworkCalls, mock.RemoveNetworkCalls)
}

func TestCreate_Validation(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedDatabaseTestData(t, stateDB, 5)
	m := NewManager(stateDB, &testutil.MockDockerClient{}, testKey)

	_, err := m.Create(context.Background(), "tenant-1", CreateRequest{Name: "x", Type: "mysql"})
	assert.ErrorContains(t, err, "invalid database type")
	_, err = m.Create(context.Background(), "tenant-1", CreateRequest{Name: "-bad", Type: "redis"})
	assert.ErrorContains(t, err, "database name")
}

func TestListPaginated_TenantScopedAndExposesEngineAddress(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedDatabaseTestData(t, stateDB, 5)
	_, err := stateDB.Exec(`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
		VALUES ('tenant-2', 'o', 'o@example.com', 'active', 1, 1)`)
	require.NoError(t, err)
	for i, row := range []struct{ id, tenant, typ string }{
		{"db-a", "tenant-1", "postgres"},
		{"db-b", "tenant-1", "redis"},
		{"db-c", "tenant-2", "redis"},
	} {
		_, err := stateDB.Exec(`INSERT INTO databases (id, tenant_id, name, type, status, host, port, db_name, username,
			password_encrypted, volume_name, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'ready', '127.0.0.1', ?, '', '', 'x', 'v', ?, ?)`,
			row.id, row.tenant, row.id, row.typ, 6100+i, 10+i, 10+i)
		require.NoError(t, err)
	}
	m := NewManager(stateDB, &testutil.MockDockerClient{}, testKey)

	all, err := m.List(context.Background(), "tenant-1")
	require.NoError(t, err)
	require.Len(t, all, 2)
	assert.Equal(t, "db-b", all[0].ID, "newest first")
	assert.Equal(t, docker.DatabaseAlias("db-b"), all[0].Host)
	assert.Equal(t, 6379, all[0].Port)
	assert.Equal(t, 5432, all[1].Port)

	page, err := m.ListPaginated(context.Background(), "tenant-1", 1, 1)
	require.NoError(t, err)
	require.Len(t, page, 1)
	assert.Equal(t, "db-a", page[0].ID)

	page, err = m.ListPaginated(context.Background(), "tenant-1", -1, -5)
	require.NoError(t, err)
	assert.Len(t, page, 2, "invalid limit/offset fall back to defaults")

	_, err = m.GetConnectionString(context.Background(), "tenant-2", "db-a")
	assert.ErrorContains(t, err, "not found", "connection strings are tenant scoped")
	_, err = m.Get(context.Background(), "tenant-2", "db-a")
	assert.ErrorContains(t, err, "not found")
}

func TestStopAllForTenant_MarksLiveDatabasesStopped(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedDatabaseTestData(t, stateDB, 5)
	insertNullContainerDB(t, stateDB, "dbr", "ready")
	_, err := stateDB.Exec(`UPDATE databases SET container_id = 'c-live' WHERE id = 'dbr'`)
	require.NoError(t, err)
	mock := &testutil.MockDockerClient{}
	m := &Manager{db: stateDB, docker: mock, masterKey: testKey}

	m.StopAllForTenant(context.Background(), "tenant-1")

	assert.Equal(t, []string{"c-live"}, mock.StopContainerCalls)
	assert.Equal(t, []string{"c-live"}, mock.RemoveContainerCalls)
	var status string
	require.NoError(t, stateDB.QueryRow(`SELECT status FROM databases WHERE id = 'dbr'`).Scan(&status))
	assert.Equal(t, "stopped", status)
}

func TestDelete_RemovesDatabaseNetwork(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedDatabaseTestData(t, stateDB, 5)
	insertNullContainerDB(t, stateDB, "dbn", "ready")
	netName := docker.DatabaseNetworkName("tenant-1", "dbn")
	mock := &testutil.MockDockerClient{
		NetworkInspectFn: func(_ context.Context, name string) (docker.NetworkInfo, error) {
			return docker.NetworkInfo{Name: name, ContainerNames: []string{"svc-app", ""}}, nil
		},
	}
	m := &Manager{db: stateDB, docker: mock, masterKey: testKey}

	require.NoError(t, m.Delete(context.Background(), "tenant-1", "dbn"))
	assert.Equal(t, [][2]string{{netName, "svc-app"}}, mock.NetworkDisconnectCalls)
	assert.Equal(t, []string{netName}, mock.RemoveNetworkCalls)
}
