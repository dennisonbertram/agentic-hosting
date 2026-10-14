package services

import (
	"context"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/require"
)

// "env-" labels are reserved for environment preview hostnames.
func TestCreateRejectsEnvPrefixedName(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedTenant(t, stateDB)
	mgr := NewManager(stateDB, &testutil.MockDockerClient{}, []byte("0123456789abcdef0123456789abcdef"), "example.com", "", nil)
	_, err := mgr.Create(context.Background(), "tenant-1", CreateRequest{Name: "env-deadbeef-web", Image: "nginx:latest", Port: 8080})
	require.Error(t, err)
}
