package api

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// BootstrapTokenValidateRequest is the body for POST /v1/system/bootstrap-token/validate.
type BootstrapTokenValidateRequest struct {
	Token string `json:"token"`
}

// BootstrapTokenValidateResponse indicates whether the provided token is valid.
type BootstrapTokenValidateResponse struct {
	Valid bool `json:"valid"`
}

// handleBootstrapTokenValidate handles POST /v1/system/bootstrap-token/validate.
//
// This endpoint allows operators to verify that a bootstrap token is accepted
// by the server, which is useful during token rotation to confirm that both
// the old and new tokens work before removing the old one.
//
// Security model:
//   - Rate-limited by the same per-IP / global limiter used for tenant
//     registration (5/IP/hour, 20/global/hour) to prevent brute-force.
//   - Returns a simple boolean; does not distinguish between "no tokens
//     configured" and "token not matched" to avoid information leakage.
func (s *Server) handleBootstrapTokenValidate(w http.ResponseWriter, r *http.Request) {
	// Apply the same rate limiter as tenant registration.
	ip := trustedRealIP(r)
	if ok, wait := regLimiter.allow(ip); !ok {
		secs := int(math.Ceil(wait.Seconds()))
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded, try again later")
		return
	}

	var req BootstrapTokenValidateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}

	provided := strings.TrimSpace(req.Token)
	valid := len(s.bootstrapTokens) > 0 && validateBootstrapToken(provided, s.bootstrapTokens, s.masterKey)

	writeJSON(w, http.StatusOK, BootstrapTokenValidateResponse{Valid: valid})
}
