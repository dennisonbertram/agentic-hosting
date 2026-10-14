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

func stoppedPG(t *testing.T, m *Manager) {
	t.Helper()
	enc, err := crypto.Encrypt([]byte("pw"), testKey)
	require.NoError(t, err)
	_, err = m.db.Exec(
		`INSERT INTO databases (id, tenant_id, name, type, status, host, port, db_name, username,
		 password_encrypted, connection_string_encrypted, volume_name, created_at, updated_at)
		 VALUES ('dbs0123456789abcdef', 'tenant-1', 'n', 'postgres', 'stopped', '127.0.0.1', 5999, 'ah', 'ah', ?, 'x', 'vol', 10, 10)`, enc)
	require.NoError(t, err)
}

func TestStartAllForTenant_HealthFailureRemovesContainer(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedDatabaseTestData(t, stateDB, 5)
	mock := &testutil.MockDockerClient{
		RunDatabaseFn: func(ctx context.Context, cfg docker.RunDatabaseConfig) (string, error) { return "c-new", nil },
	}
	m := &Manager{db: stateDB, docker: mock, masterKey: testKey, healthTimeout: 300 * time.Millisecond}
	stoppedPG(t, m)

	m.StartAllForTenant(context.Background(), "tenant-1")

	var status string
	require.NoError(t, stateDB.QueryRow(`SELECT status FROM databases WHERE id='dbs0123456789abcdef'`).Scan(&status))
	assert.Equal(t, "failed", status)
	assert.Contains(t, mock.RemoveContainerCalls, "c-new")
	assert.Empty(t, mock.RemoveVolumeCalls)
}

func TestStartAllForTenant_SkipsSuspendedTenant(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedDatabaseTestData(t, stateDB, 5)
	mock := &testutil.MockDockerClient{}
	m := &Manager{db: stateDB, docker: mock, masterKey: testKey, healthTimeout: 300 * time.Millisecond}
	stoppedPG(t, m)
	_, err := stateDB.Exec(`UPDATE tenants SET status='suspended'`)
	require.NoError(t, err)

	m.StartAllForTenant(context.Background(), "tenant-1")

	assert.Equal(t, 0, mock.RunDatabaseCalls)
}

func TestStopAllForTenant_RemovesFailedRowContainer(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedDatabaseTestData(t, stateDB, 5)
	insertNullContainerDB(t, stateDB, "dbf", "failed")
	_, err := stateDB.Exec(`UPDATE databases SET container_id='leftover' WHERE id='dbf'`)
	require.NoError(t, err)
	mock := &testutil.MockDockerClient{}
	m := &Manager{db: stateDB, docker: mock, masterKey: testKey}

	m.StopAllForTenant(context.Background(), "tenant-1")

	assert.Contains(t, mock.RemoveContainerCalls, "leftover")
}
