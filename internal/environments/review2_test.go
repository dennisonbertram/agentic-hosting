package environments

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/apierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The reconciler can stop an environment between ExtendLease's status read and
// its UPDATE. The conditional update must notice and report a conflict rather
// than claim an extension the stopped environment will not get.
func TestExtendLease_ConcurrentStopIsConflict(t *testing.T) {
	mgr, _ := newTestManager(t, 5)
	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "race"})
	require.NoError(t, err)

	// Simulate the stop landing between Get and the UPDATE: the trigger flips
	// the row to 'stopped' and drops the lease write, so the conditional
	// UPDATE matches no row exactly as it would after a real concurrent stop.
	_, err = mgr.db.Exec(`CREATE TRIGGER stop_on_extend BEFORE UPDATE OF lease_expires_at ON environments
		BEGIN
			UPDATE environments SET status = 'stopped' WHERE id = OLD.id;
			SELECT RAISE(IGNORE);
		END`)
	require.NoError(t, err)

	err = mgr.ExtendLease(context.Background(), testTenantID, env.ID, 600)
	var ae *apierr.APIError
	require.True(t, errors.As(err, &ae), "want an API error, got %v", err)
	assert.Equal(t, http.StatusConflict, ae.Code)

	got, err := mgr.Get(context.Background(), testTenantID, env.ID)
	require.NoError(t, err)
	assert.Equal(t, "stopped", got.Status)
	assert.Equal(t, env.LeaseExpiresAt, got.LeaseExpiresAt, "lease must not be extended")
}

// git parses an argument starting with '-' as an option, so such a ref must be
// rejected before any exec.
func TestSyncWorkspace_RejectsOptionLikeRef(t *testing.T) {
	mgr, mock := newTestManager(t, 5)
	env, err := mgr.Create(context.Background(), testTenantID, CreateRequest{Name: "optref"})
	require.NoError(t, err)
	before := len(mock.ExecCreateCalls)

	err = mgr.SyncWorkspace(context.Background(), testTenantID, env.ID,
		SyncRequest{GitURL: "https://example.com/r.git", GitRef: "--upload-pack=touch /tmp/x"})
	var ae *apierr.APIError
	require.True(t, errors.As(err, &ae), "want an API error, got %v", err)
	assert.Equal(t, http.StatusBadRequest, ae.Code)
	assert.Len(t, mock.ExecCreateCalls, before, "no exec may run for a rejected ref")
}
