package middleware

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInflightLimiter_PerTenantCap(t *testing.T) {
	il := NewInflightLimiter(2)
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	h := il.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
	}))
	do := func(tenant string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil).WithContext(contextWithTenant(tenant)))
		return rec
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); do("a") }()
		<-entered
	}
	assert.Equal(t, http.StatusTooManyRequests, do("a").Code, "third concurrent request for tenant a")
	close(release)
	wg.Wait()
	assert.Equal(t, http.StatusOK, do("a").Code, "slots are released")
}

func TestRateLimiter_PerTenantRateOverride(t *testing.T) {
	rl := NewRateLimiter(100, 200)
	now := time.Now()
	rl.now = func() time.Time { return now }
	h := newRateLimitHandler(rl)
	do := func(limit int) int {
		ctx := ContextWithRateLimit(contextWithTenant("t"), limit)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil).WithContext(ctx))
		return rec.Code
	}
	// tenant limit 3 -> burst 6
	for i := 0; i < 6; i++ {
		require.Equal(t, 200, do(3), "request %d", i)
	}
	assert.Equal(t, 429, do(3))
}

func TestRateLimiter_ConcurrentGetLimiterSingleInstance(t *testing.T) {
	rl := NewRateLimiter(1, 1)
	var wg sync.WaitGroup
	ptrs := make([]any, 50)
	for i := range ptrs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ptrs[i] = rl.getLimiter("k", 1)
			rl.sweep(time.Now().Add(-time.Hour))
		}()
	}
	wg.Wait()
	for _, p := range ptrs {
		assert.Same(t, ptrs[0], p)
	}
}

// slowClient connects, sends a GET and never reads the response.
func slowClient(t *testing.T, addr string) {
	c, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	_, err = c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
	require.NoError(t, err)
	t.Cleanup(func() { c.Close() })
}

// bigWriter writes up to 1 GiB and reports the first write error.
func bigWriter(done chan<- error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		for i := 0; i < 1024; i++ {
			if _, err := w.Write(buf); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	})
}

func TestWriteDeadline_CutsOffSlowReader(t *testing.T) {
	done := make(chan error, 1)
	srv := httptest.NewServer(WriteDeadline(200 * time.Millisecond)(bigWriter(done)))
	defer srv.Close()
	slowClient(t, srv.Listener.Addr().String())
	select {
	case err := <-done:
		assert.Error(t, err, "write must fail once the deadline passes")
	case <-time.After(5 * time.Second):
		t.Fatal("handler still blocked: slow reader was not cut off")
	}
}

func TestIdleWriteDeadline_SurvivesWhileProgressing(t *testing.T) {
	srv := httptest.NewServer(IdleWriteDeadline(300 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 8; i++ { // 8*100ms = 800ms > 300ms idle
			w.Write([]byte("x"))
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
	})))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "xxxxxxxx", string(b))
}

func TestIdleWriteDeadline_CutsOffStalledReader(t *testing.T) {
	done := make(chan error, 1)
	srv := httptest.NewServer(IdleWriteDeadline(200 * time.Millisecond)(bigWriter(done)))
	defer srv.Close()
	slowClient(t, srv.Listener.Addr().String())
	select {
	case err := <-done:
		assert.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("stalled reader held the handler")
	}
}
