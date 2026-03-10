package middleware

import (
	"net/http"

	"github.com/dennisonbertram/agentic-hosting/internal/httpx"
)

// writeJSONError delegates to the shared httpx.WriteError for consistent
// error formatting across both middleware and API handler packages.
func writeJSONError(w http.ResponseWriter, code int, message string) {
	httpx.WriteError(w, code, message)
}
