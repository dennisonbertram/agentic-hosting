// Package httpx provides shared HTTP response helpers used by both the api
// and middleware packages to ensure consistent error formatting.
package httpx

import (
	"encoding/json"
	"net/http"
)

// WriteError writes a consistent JSON error response with correct headers.
// All error responses across the codebase should use this function.
func WriteError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// WriteJSON writes a JSON success response with correct headers and status code.
// All success responses across the codebase should use this function.
func WriteJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
