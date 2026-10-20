// Package apierr defines typed API errors for consistent HTTP error handling.
package apierr

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/dennisonbertram/agentic-hosting/internal/httpx"
)

// Sentinel errors for error routing via errors.Is().
var (
	ErrNotFound      = errors.New("not found")
	ErrConflict      = errors.New("conflict")
	ErrValidation    = errors.New("validation error")
	ErrQuotaExceeded = errors.New("quota exceeded")
	ErrForbidden     = errors.New("forbidden")
	ErrUnavailable   = errors.New("temporarily unavailable")
)

// RetryAfterSeconds is the Retry-After value sent with 503 Unavailable errors.
const RetryAfterSeconds = "10"

// APIError is a typed error that carries an HTTP status code and user-facing message.
type APIError struct {
	Code    int    // HTTP status code
	Message string // user-facing message
	Err     error  // wrapped error for errors.Is() / errors.As()
}

func (e *APIError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s", e.Message, e.Err.Error())
	}
	return e.Message
}

func (e *APIError) Unwrap() error {
	return e.Err
}

// NotFound returns a 404 error.
func NotFound(msg string) *APIError {
	return &APIError{Code: http.StatusNotFound, Message: msg, Err: ErrNotFound}
}

// Conflict returns a 409 error.
func Conflict(msg string) *APIError {
	return &APIError{Code: http.StatusConflict, Message: msg, Err: ErrConflict}
}

// Validation returns a 400 error.
func Validation(msg string) *APIError {
	return &APIError{Code: http.StatusBadRequest, Message: msg, Err: ErrValidation}
}

// QuotaExceeded returns a 409 Conflict error for resource quota limits.
// 409 is used (not 429) because quota exceeded is a resource conflict, not a rate limit.
func QuotaExceeded(msg string) *APIError {
	return &APIError{Code: http.StatusConflict, Message: msg, Err: ErrQuotaExceeded}
}

// Forbidden returns a 403 error.
func Forbidden(msg string) *APIError {
	return &APIError{Code: http.StatusForbidden, Message: msg, Err: ErrForbidden}
}

// Unavailable returns a 503 error for transient, retryable backpressure
// (deploy queue full, low disk). WriteAPIError adds a Retry-After header.
// cause, if non-nil, is wrapped for errors.Is and logging but not shown.
func Unavailable(msg string, cause error) *APIError {
	err := ErrUnavailable
	if cause != nil {
		err = fmt.Errorf("%w: %w", ErrUnavailable, cause)
	}
	return &APIError{Code: http.StatusServiceUnavailable, Message: msg, Err: err}
}

// DeployFailed returns a 502 error: the platform worked but the container runtime
// could not start, stop or remove the service's container. The cause is shown to the client.
func DeployFailed(cause error) *APIError {
	return &APIError{Code: http.StatusBadGateway, Message: "deploy failed: " + cause.Error(), Err: cause}
}

// WriteAPIError writes an appropriate JSON error response based on the error type.
// Uses errors.As() to extract APIError; falls back to 500 for unknown errors.
func WriteAPIError(w http.ResponseWriter, err error) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if apiErr.Code == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", RetryAfterSeconds)
		}
		httpx.WriteError(w, apiErr.Code, apiErr.Message)
		return
	}
	httpx.WriteError(w, http.StatusInternalServerError, "internal error")
}
