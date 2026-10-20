package api

import (
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/services"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
)

// The API must use the service manager handed in by main.go (the one whose
// DeployImage the build manager calls) so both share per-service locks and
// the deploy semaphore/queue.
func TestNewServer_UsesSuppliedServiceManager(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	mock := &testutil.MockDockerClient{}
	key := []byte("0123456789abcdef0123456789abcdef")
	shared := services.NewManager(stateDB, mock, key, "", "", nil)

	srv := NewServer(ServerConfig{Store: &db.Store{StateDB: stateDB}, MasterKey: key, DevMode: true, Docker: mock, ServiceManager: shared})
	assert.Same(t, shared, srv.svcManager)

	srv2 := NewServer(ServerConfig{Store: &db.Store{StateDB: stateDB}, MasterKey: key, DevMode: true, Docker: mock})
	assert.NotNil(t, srv2.svcManager, "falls back to its own manager when none is supplied")
	assert.NotNil(t, srv2.snapshotManager)
}
