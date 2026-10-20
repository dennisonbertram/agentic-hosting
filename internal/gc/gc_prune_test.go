package gc

import (
	"context"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCollectOnce_DaemonWidePruneOnByDefault(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{}
	require.NoError(t, New(stateDB, mock, time.Minute, t.TempDir()).collectOnce(context.Background()))
	assert.Equal(t, 1, mock.PruneDanglingImagesCalls)
	assert.Equal(t, 1, mock.PruneAnonymousVolumesCalls)
}

// A shared daemon (local sandbox on a laptop) must not lose unrelated
// dangling images or anonymous volumes to ah's GC.
func TestCollectOnce_SetDockerPruneFalseSkipsDaemonWidePrune(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{}
	g := New(stateDB, mock, time.Minute, t.TempDir())
	g.SetDockerPrune(false)
	require.NoError(t, g.collectOnce(context.Background()))
	assert.Equal(t, 0, mock.PruneDanglingImagesCalls)
	assert.Equal(t, 0, mock.PruneAnonymousVolumesCalls)

	g.SetDockerPrune(true)
	require.NoError(t, g.collectOnce(context.Background()))
	assert.Equal(t, 1, mock.PruneDanglingImagesCalls)
	assert.Equal(t, 1, mock.PruneAnonymousVolumesCalls)
}
