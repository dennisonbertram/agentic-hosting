package databases

import (
	"context"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A suspension that set 'stopped' while a database was provisioning must not
// be overwritten with 'ready' (reactivation only restarts 'stopped' rows).
func TestCasStatus_DoesNotOverwriteConcurrentStop(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedDatabaseTestData(t, stateDB, 5)
	m := &Manager{db: stateDB, docker: &testutil.MockDockerClient{}, masterKey: testKey}
	stoppedPG(t, m)
	ctx := context.Background()

	assert.Equal(t, casChanged, m.casStatus(ctx, "dbs0123456789abcdef", "provisioning", "ready"))
	var status string
	require.NoError(t, stateDB.QueryRow(`SELECT status FROM databases WHERE id='dbs0123456789abcdef'`).Scan(&status))
	assert.Equal(t, "stopped", status)

	assert.Equal(t, casGone, m.casStatus(ctx, "missing", "provisioning", "ready"))
	assert.Equal(t, casApplied, m.casStatus(ctx, "dbs0123456789abcdef", "stopped", "ready"))
}
