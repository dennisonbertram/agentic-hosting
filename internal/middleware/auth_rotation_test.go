package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/crypto"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/require"
)

func TestAuth_SurvivesMasterKeyRotation(t *testing.T) {
	sqlDB := testutil.NewStateDB(t)
	newKey := []byte("fedcba9876543210fedcba9876543210")

	tenantID := insertTenant(t, sqlDB, "active")
	token := insertAPIKey(t, sqlDB, tenantID, nil, nil) // hashed under testMasterKey

	_, err := crypto.RotateKeys(sqlDB, testMasterKey, newKey, false)
	require.NoError(t, err)

	mw, _ := Auth(sqlDB, newKey)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, 200, rec.Code)
}
