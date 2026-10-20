package apierr

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWriteAPIError_UnavailableSetsRetryAfter(t *testing.T) {
	cause := errors.New("disk 95% full")
	err := Unavailable("deploy queue full; try again later", cause)
	assert.ErrorIs(t, err, ErrUnavailable)
	assert.ErrorIs(t, err, cause)

	rec := httptest.NewRecorder()
	WriteAPIError(rec, err)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, RetryAfterSeconds, rec.Header().Get("Retry-After"))
	assert.Contains(t, rec.Body.String(), "deploy queue full")
	assert.NotContains(t, rec.Body.String(), "95%", "cause must not leak into the response")
}

func TestWriteAPIError_ConflictHasNoRetryAfter(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteAPIError(rec, Conflict("x"))
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Empty(t, rec.Header().Get("Retry-After"))
}
