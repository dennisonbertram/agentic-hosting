package testutil_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/docker/docker/errdefs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewStateDB_IsolatedPerCall(t *testing.T) {
	a := testutil.NewStateDB(t)
	b := testutil.NewStateDB(t)
	_, err := a.Exec(`INSERT INTO tenants (id, name, email, created_at, updated_at) VALUES ('t1','n','e@x',1,1)`)
	require.NoError(t, err)

	var n int
	require.NoError(t, b.QueryRow(`SELECT COUNT(*) FROM tenants`).Scan(&n))
	assert.Zero(t, n, "each NewStateDB must be a separate database")
}

func TestNewMeteringDB_HasSchema(t *testing.T) {
	m := testutil.NewMeteringDB(t)
	var n int
	require.NoError(t, m.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name LIKE 'metering_%'`).Scan(&n))
	assert.Positive(t, n)
}

func TestNewFileStateDB_HasSchema(t *testing.T) {
	d := testutil.NewFileStateDB(t)
	var mode string
	require.NoError(t, d.QueryRow(`PRAGMA journal_mode`).Scan(&mode))
	assert.Equal(t, "wal", mode)
	_, err := d.Exec(`SELECT 1 FROM services LIMIT 1`)
	assert.NoError(t, err)
}

func TestNewNotFoundError_IsNotFound(t *testing.T) {
	err := testutil.NewNotFoundError("c1")
	assert.True(t, errdefs.IsNotFound(err))
	assert.Contains(t, err.Error(), "c1")
}

func TestMockDockerClient_DefaultsAndOverrides(t *testing.T) {
	ctx := context.Background()
	m := &testutil.MockDockerClient{}

	id, err := m.RunContainer(ctx, "t", "svc", "img", 80, nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "mock-container-svc", id)

	net, err := m.EnsureNetwork(ctx, "n")
	require.NoError(t, err)
	assert.Equal(t, "net-n", net)

	m.NetworkListFn = func(context.Context) ([]docker.NetworkInfo, error) {
		return []docker.NetworkInfo{{ID: "x"}}, nil
	}
	info, err := m.NetworkInspect(ctx, "x")
	require.NoError(t, err)
	assert.Equal(t, "x", info.ID)

	boom := errors.New("boom")
	m.StopContainerFn = func(context.Context, string) error { return boom }
	assert.ErrorIs(t, m.StopContainer(ctx, "c"), boom)
	assert.Equal(t, []string{"c"}, m.StopContainerCalls)
}

// The call recorders are written under the mock's mutex so concurrent callers
// (run with -race) do not corrupt them.
func TestMockDockerClient_ConcurrentRecording(t *testing.T) {
	ctx := context.Background()
	m := &testutil.MockDockerClient{}
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.RenameContainer(ctx, "c", "new")
			_ = m.StopContainer(ctx, "c")
		}()
	}
	wg.Wait()
	assert.Equal(t, n, m.RenameContainerCalls)
	assert.Len(t, m.RenameContainerArgs, n)
	assert.Len(t, m.StopContainerCalls, n)
}
