package middleware

import (
	"net/http"
	"net/url"
	"strings"
)

// DefaultCORSOrigins are the public dashboard hosts. Overridden by
// AH_CORS_ORIGINS / --cors-origins.
var DefaultCORSOrigins = []string{
	"https://agentic.hosting",
	"https://www.agentic.hosting",
	"https://dennisonbertram.github.io",
}

const (
	corsAllowMethods = "GET, POST, PUT, PATCH, DELETE, OPTIONS"
	corsAllowHeaders = "Authorization, Content-Type, Idempotency-Key"
)

// CORS reflects Access-Control-Allow-Origin only for an exact allowlisted
// origin. It never uses "*" and never sets Allow-Credentials. OPTIONS
// preflight is answered here so it never hits auth.
func CORS(origins []string) func(http.Handler) http.Handler {
	allowed := allowlistSet(origins)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Add("Vary", "Origin")
			if origin := r.Header.Get("Origin"); origin != "" {
				if normalized, ok := NormalizeOrigin(origin); ok {
					if _, listed := allowed[normalized]; listed {
						// Echo the request Origin (not "*") after the allowlist check.
						w.Header().Set("Access-Control-Allow-Origin", origin)
						w.Header().Set("Access-Control-Allow-Methods", corsAllowMethods)
						w.Header().Set("Access-Control-Allow-Headers", corsAllowHeaders)
						w.Header().Set("Access-Control-Max-Age", "600")
					}
				}
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// NormalizeOrigin parses a browser Origin (scheme + host[:port], no path).
// "*" and anything else is rejected.
func NormalizeOrigin(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "*" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil {
		return "", false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	if u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	if u.Path != "" && u.Path != "/" {
		return "", false
	}
	return u.Scheme + "://" + strings.ToLower(u.Host), true
}

func allowlistSet(origins []string) map[string]struct{} {
	if origins == nil {
		origins = DefaultCORSOrigins
	}
	allowed := make(map[string]struct{}, len(origins))
	for _, raw := range origins {
		normalized, ok := NormalizeOrigin(raw)
		if !ok {
			continue
		}
		allowed[normalized] = struct{}{}
	}
	return allowed
}
