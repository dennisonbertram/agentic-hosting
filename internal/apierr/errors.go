// Package apierr defines typed API errors for consistent HTTP error handling.
package apierr

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Sentinel errors for error routing via errors.Is().
var (
	ErrNotFound      = errors.New("not found")
	ErrConflict      = errors.New("conflict")
	ErrValidation    = errors.New("validation error")
	ErrQuotaExceeded = errors.New("quota exceeded")
	ErrForbidden     = errors.New("forbidden")
)

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

// WriteAPIError writes an appropriate JSON error response based on the error type.
// Uses errors.As() to extract APIError; falls back to 500 for unknown errors.
func WriteAPIError(w http.ResponseWriter, err error) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		writeJSON(w, apiErr.Code, apiErr.Message)
		return
	}
	writeJSON(w, http.StatusInternalServerError, "internal error")
}

func writeJSON(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}
