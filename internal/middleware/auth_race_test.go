package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A revoke + InvalidateKey that lands between the auth DB read and the cache
// write must not leave a stale "active" entry behind.
func TestAuth_RevokeDuringDBReadIsNotCached(t *testing.T) {
	sqlDB := testutil.NewStateDB(t)
	tenantID := insertTenant(t, sqlDB, "active")
	token := insertAPIKey(t, sqlDB, tenantID, nil, nil)
	keyID := token[:strings.IndexByte(token, '.')]

	handler, inv := newAuthMiddleware(t, sqlDB)

	authAfterDBRead = func() {
		_, err := sqlDB.Exec(`UPDATE api_keys SET revoked_at = ? WHERE id = ?`, time.Now().Unix(), keyID)
		require.NoError(t, err)
		inv.InvalidateKey(keyID)
	}
	t.Cleanup(func() { authAfterDBRead = nil })

	do := func() int {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}
	// The in-flight request already read the row, so it may succeed once.
	do()
	authAfterDBRead = nil
	// The next request must see the revocation, not a stale cache entry.
	assert.Equal(t, http.StatusUnauthorized, do())
}
