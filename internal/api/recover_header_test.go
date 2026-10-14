package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// A bootstrap token in the X-Bootstrap-Token header must get the same 403 as
// one in the body or X-Admin-Token when an admin token is configured, and an
// unknown token must get 401.
func TestAdminToken_RecoverHeaderSlot(t *testing.T) {
	regLimiter.resetForTest()
	srv := newAdminTestServer(t, []string{atAdmin})
	body, _ := json.Marshal(KeyRecoverRequest{Email: "admin@example.com"})
	for _, header := range []string{"X-Bootstrap-Token", "X-Admin-Token"} {
		req := recoverRequest("10.9.0.4", body)
		req.Header.Set(header, atBootstrap)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusForbidden, rr.Code, "%s: %s", header, rr.Body.String())
	}
	req := recoverRequest("10.9.0.4", body)
	req.Header.Set("X-Bootstrap-Token", "unknown-token-xxxxxxxxxxxxxxxxxxxxxxxx")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusUnauthorized, rr.Code, rr.Body.String())
	// The admin token in the bootstrap header slot is accepted.
	req = recoverRequest("10.9.0.4", body)
	req.Header.Set("X-Bootstrap-Token", atAdmin)
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
}
