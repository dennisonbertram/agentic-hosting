package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

// A secondary limiter stacked behind the general one must not overwrite the
// general X-RateLimit-* headers.
func TestRateLimiterWithHeaderPrefix_DoesNotOverwriteGeneralHeaders(t *testing.T) {
	general := NewRateLimiter(100, 200)
	secondary := NewRateLimiterWithHeaderPrefix(20.0/60.0, 2, "X-Sandbox-Create-RateLimit")
	handler := general.Middleware(secondary.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	codes := []int{}
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(contextWithTenant("tenant-prefix"))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		codes = append(codes, rec.Code)
		assert.Equal(t, "100", rec.Header().Get("X-RateLimit-Limit"))
		assert.Equal(t, "0.333", rec.Header().Get("X-Sandbox-Create-RateLimit-Limit"))
		require.NotEmpty(t, rec.Header().Get("X-Sandbox-Create-RateLimit-Remaining"))
	}
	assert.Equal(t, []int{http.StatusOK, http.StatusOK, http.StatusTooManyRequests}, codes)

	// An empty prefix keeps the default headers.
	assert.Equal(t, DefaultRateLimitHeaderPrefix, NewRateLimiterWithHeaderPrefix(1, 1, "").headerPrefix)
}

func TestFormatLimit(t *testing.T) {
	assert.Equal(t, "100", formatLimit(rate.Limit(100)))
	assert.Equal(t, "0.333", formatLimit(rate.Limit(20.0/60.0)))
	assert.Equal(t, "0.5", formatLimit(rate.Limit(0.5)))
}
