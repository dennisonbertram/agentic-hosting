package environments

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
	db := testutil.NewFileStateDB(t)
	seedTenant(t, db, testTenantID, 1)

	// Provisioning blocks until the losers have all returned, so a quota
	// bypass shows up as fewer than n-1 early returns.
	release := make(chan struct{})
	mock := &testutil.MockDockerClient{
		RunEnvironmentFn: func(ctx context.Context, cfg docker.RunEnvironmentConfig) (string, error) {
			<-release
			return "", assert.AnError
		},
	}
	mgr := NewManager(db, mock, "", "")

	const n = 8
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(i int) {
			<-start
			_, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: fmt.Sprintf("env-%d", i)})
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
