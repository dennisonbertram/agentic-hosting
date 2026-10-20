package apierr

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPIError_Error(t *testing.T) {
	err := NotFound("service not found")
	assert.Equal(t, "service not found: not found", err.Error())

	err2 := &APIError{Code: 500, Message: "oops"}
	assert.Equal(t, "oops", err2.Error())
}

func TestAPIError_Unwrap(t *testing.T) {
	err := NotFound("x")
	assert.True(t, errors.Is(err, ErrNotFound))
	assert.False(t, errors.Is(err, ErrConflict))
}

func TestAPIError_ErrorsAs(t *testing.T) {
	err := Conflict("already running")
	var apiErr *APIError
	require.True(t, errors.As(err, &apiErr))
	assert.Equal(t, http.StatusConflict, apiErr.Code)
	assert.Equal(t, "already running", apiErr.Message)
}

func TestAPIError_WrappedInFmtErrorf(t *testing.T) {
	inner := NotFound("service not found")
	wrapped := fmt.Errorf("get service: %w", inner)

	assert.True(t, errors.Is(wrapped, ErrNotFound))

	var apiErr *APIError
	require.True(t, errors.As(wrapped, &apiErr))
	assert.Equal(t, http.StatusNotFound, apiErr.Code)
}

func TestAllConstructors(t *testing.T) {
	tests := []struct {
		fn       func(string) *APIError
		sentinel error
		code     int
	}{
		{NotFound, ErrNotFound, 404},
		{Conflict, ErrConflict, 409},
		{Validation, ErrValidation, 400},
		{QuotaExceeded, ErrQuotaExceeded, 409},
		{Forbidden, ErrForbidden, 403},
	}
	for _, tt := range tests {
		err := tt.fn("test message")
		assert.Equal(t, tt.code, err.Code)
		assert.True(t, errors.Is(err, tt.sentinel))
	}
}

func TestWriteAPIError_TypedError(t *testing.T) {
	w := httptest.NewRecorder()
	WriteAPIError(w, NotFound("thing not found"))

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "thing not found")
}

func TestWriteAPIError_WrappedTypedError(t *testing.T) {
	w := httptest.NewRecorder()
	inner := Conflict("already exists")
	wrapped := fmt.Errorf("create: %w", inner)
	WriteAPIError(w, wrapped)

	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), "already exists")
}

func TestWriteAPIError_UntypedError(t *testing.T) {
	w := httptest.NewRecorder()
	WriteAPIError(w, fmt.Errorf("some random error"))

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "internal error")
	assert.NotContains(t, w.Body.String(), "some random error", "untyped error details must not leak")
}

// Typed errors go through the shared httpx writer, so they carry the same
// headers as every other API error response.
func TestWriteAPIError_ConsistentHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	WriteAPIError(w, DeployFailed(fmt.Errorf("no such image")))

	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
	assert.JSONEq(t, `{"error":"deploy failed: no such image"}`, w.Body.String())
	assert.Empty(t, w.Header().Get("Retry-After"), "only 503 carries Retry-After")
}
