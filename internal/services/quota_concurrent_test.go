package services

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/deployments"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
)

func TestCreate_ConcurrentQuotaIsEnforced(t *testing.T) {
	stateDB := testutil.NewFileStateDB(t)
	seedTenantWithQuota(t, stateDB, 1)
	mgr := NewManager(stateDB, &testutil.MockDockerClient{}, []byte("0123456789abcdef0123456789abcdef"), "", "", deployments.NewStore(stateDB))

	const n = 8
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(i int) {
			<-start
			_, err := mgr.Create(context.Background(), "tenant-1", CreateRequest{Name: fmt.Sprintf("svc-%d", i), Image: "nginx:latest", Port: 8080})
			errs <- err
		}(i)
	}
	close(start)
	ok, quota := 0, 0
	for i := 0; i < n; i++ {
		err := <-errs
		switch {
		case err == nil:
			ok++
		case strings.Contains(err.Error(), "quota exceeded"):
			quota++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, 1, ok)
	assert.Equal(t, n-1, quota)
}
