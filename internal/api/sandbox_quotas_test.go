package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandleQuotaUpdate_SandboxFields(t *testing.T) {
	resetLimitersForTest()
	t.Cleanup(resetLimitersForTest)
	const bootstrapToken = "test-bootstrap-token-for-sandbox-quotas"
	stateDB := testutil.NewStateDB(t)
	_, err := stateDB.Exec(`INSERT INTO tenants (id, name, email, status, created_at, updated_at)
		VALUES ('tenant-sbx-q', 'Q', 'q@example.com', 'active', 1, 1)`)
	require.NoError(t, err)
	_, err = stateDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES ('tenant-sbx-q')`)
	require.NoError(t, err)
	srv := NewServer(ServerConfig{
		Store:           &db.Store{StateDB: stateDB},
		MasterKey:       []byte("0123456789abcdef0123456789abcdef"),
		DevMode:         true,
		BootstrapTokens: []string{bootstrapToken},
	})

	// Defaults are reported for an untouched tenant.
	logBuf := captureLog(t)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, quotaRequest("10.9.0.1", "tenant-sbx-q", bootstrapToken, []byte(`{}`)))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var resp QuotaUpdateResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal(t, 5, resp.MaxSandboxes)
	assert.Equal(t, 14400, resp.MaxSandboxTTLSeconds)
	assert.Equal(t, 86400, resp.MaxSandboxLifetimeSeconds)
	assert.Equal(t, 0, resp.SandboxEgressAllowed)
	assert.Equal(t, 1024, resp.MaxSandboxDiskMB)

	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, quotaRequest("10.9.0.2", "tenant-sbx-q", bootstrapToken,
		[]byte(`{"max_sandboxes":0,"max_sandbox_ttl_seconds":600,"max_sandbox_lifetime_seconds":604800,"sandbox_egress_allowed":1,"max_sandbox_disk_mb":256}`)))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	assert.Equal(t, 0, resp.MaxSandboxes, "0 disables sandboxes for the tenant")
	assert.Equal(t, 600, resp.MaxSandboxTTLSeconds)
	assert.Equal(t, 604800, resp.MaxSandboxLifetimeSeconds)
	assert.Equal(t, 1, resp.SandboxEgressAllowed)
	assert.Equal(t, 256, resp.MaxSandboxDiskMB)
	assert.Contains(t, logBuf.String(), "max_sandboxes=0 max_sandbox_ttl_seconds=600 max_sandbox_lifetime_seconds=604800 sandbox_egress_allowed=1 max_sandbox_disk_mb=256")

	for i, body := range []string{
		`{"max_sandboxes":51}`,
		`{"max_sandboxes":-1}`,
		`{"max_sandbox_ttl_seconds":59}`,
		`{"max_sandbox_ttl_seconds":86401}`,
		`{"max_sandbox_lifetime_seconds":604801}`,
		`{"sandbox_egress_allowed":2}`,
		`{"max_sandbox_disk_mb":15}`,
		`{"max_sandbox_disk_mb":8193}`,
	} {
		rr = httptest.NewRecorder()
		srv.ServeHTTP(rr, quotaRequest("10.9.1."+string(rune('1'+i)), "tenant-sbx-q", bootstrapToken, []byte(body)))
		assert.Equal(t, http.StatusBadRequest, rr.Code, body)
	}
}
