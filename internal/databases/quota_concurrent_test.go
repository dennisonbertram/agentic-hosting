package databases

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreate_ConcurrentQuotaIsEnforced(t *testing.T) {
	stateDB := testutil.NewFileStateDB(t)
	seedDatabaseTestData(t, stateDB, 1)

	// Provisioning blocks until the losers have all returned, so a quota
	// bypass shows up as fewer than n-1 early returns.
	release := make(chan struct{})
	mock := &testutil.MockDockerClient{
		RunDatabaseFn: func(ctx context.Context, cfg docker.RunDatabaseConfig) (string, error) {
			<-release
			return "", assert.AnError
		},
	}
	mgr := NewManager(stateDB, mock, []byte("0123456789abcdef0123456789abcdef"))

	const n = 8
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(i int) {
			<-start
			_, err := mgr.Create(context.Background(), "tenant-1", CreateRequest{Name: fmt.Sprintf("db-%d", i), Type: "postgres"})
			errs <- err
		}(i)
	}
	close(start)
	quota := 0
	for i := 0; i < n-1; i++ {
		select {
		case err := <-errs:
			if err != nil && strings.Contains(err.Error(), "quota exceeded") {
				quota++
			} else {
				t.Errorf("expected quota error, got %v", err)
			}
		case <-time.After(10 * time.Second):
			close(release)
			t.Fatalf("only %d creates were rejected; quota was bypassed", quota)
		}
	}
	close(release)
	err := <-errs
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "quota exceeded")
	assert.Equal(t, n-1, quota)
}
