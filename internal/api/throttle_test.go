package api

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
)

// blockingWriter holds its request (and so its throttle slot) in Write until released.
type blockingWriter struct {
	*httptest.ResponseRecorder
	entered chan<- struct{}
	release <-chan struct{}
}

func (b *blockingWriter) Write(p []byte) (int, error) {
	b.entered <- struct{}{}
	<-b.release
	return b.ResponseRecorder.Write(p)
}

// Public and authenticated short routes share one pool of shortRouteConcurrency slots.
func TestShortRoutes_ShareOneThrottlePool(t *testing.T) {
	srv := NewServer(ServerConfig{
		Store: &db.Store{StateDB: testutil.NewStateDB(t)}, MasterKey: []byte("0123456789abcdef0123456789abcdef"), DevMode: true,
	})
	release := make(chan struct{})
	entered := make(chan struct{}, shortRouteConcurrency)
	var wg sync.WaitGroup
	for i := 0; i < shortRouteConcurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := &blockingWriter{ResponseRecorder: httptest.NewRecorder(), entered: entered, release: release}
			srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/system/health", nil))
		}()
		<-entered
	}
	// Pool is full of public requests; an authenticated-group request must be throttled too.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/system/health/detailed", nil))
	assert.Contains(t, []int{http.StatusServiceUnavailable, http.StatusTooManyRequests}, rec.Code)
	close(release)
	wg.Wait()
}
