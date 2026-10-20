package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The 8-character IDs printed by `snapshot list` (and the help example) must
// be accepted by `snapshot delete`.
func TestSnapshotDelete_AcceptsShortIDFromListTable(t *testing.T) {
	var deleted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodDelete {
			deleted = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path == "/v1/snapshots" {
			json.NewEncoder(w).Encode([]ahclient.Snapshot{
				{ID: "aabbccdd11223344aabbccdd11223344", Name: "pre-change-20261020"},
				{ID: "aabbccee11223344aabbccdd11223344", Name: "pre-change-20261020"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := newTestClient(srv)

	require.NoError(t, runSnapshotDelete(c, "aabbccdd", true))
	assert.Equal(t, "/v1/snapshots/aabbccdd11223344aabbccdd11223344", deleted)

	err := runSnapshotDelete(c, "aabbcc", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous")

	_, err = resolveSnapshotID(c, "ffff0000")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestResolveIDPrefix(t *testing.T) {
	ids := []string{"0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"}
	id, err := resolveIDPrefix("service", "01234", ids)
	require.NoError(t, err)
	assert.Equal(t, ids[0], id)

	_, err = resolveIDPrefix("service", "012", ids)
	assert.Error(t, err, "prefixes shorter than the minimum are not matched")
	_, err = resolveIDPrefix("service", "not-hex", ids)
	assert.Error(t, err)
}
