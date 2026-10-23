package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSuspend_SendsAdminToken(t *testing.T) {
	var gotAdmin, gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAdmin = r.Header.Get("X-Admin-Token")
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	isolateHome(t)
	t.Setenv("AH_URL", srv.URL)
	t.Setenv("AH_BOOTSTRAP_TOKEN", "")
	t.Setenv("AH_ADMIN_TOKEN", "admin-from-env")
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"suspend", "tenant-abc", "--confirm"})
	require.NoError(t, cmd.Execute(), out.String())
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/v1/tenants/tenant-abc/suspend", gotPath)
	assert.Equal(t, "admin-from-env", gotAdmin)
	assert.Contains(t, out.String(), "suspended")
}

func TestSuspend_RequiresConfirm(t *testing.T) {
	isolateHome(t)
	t.Setenv("AH_URL", "http://127.0.0.1:1")
	t.Setenv("AH_ADMIN_TOKEN", "tok")
	cmd := newRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"suspend", "tenant-abc"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--confirm")
}

func TestSuspend_AdminTokenFlagHelpWarns(t *testing.T) {
	cmd := newRootCmd()
	sub, _, err := cmd.Find([]string{"suspend"})
	require.NoError(t, err)
	usage := sub.Flags().Lookup("admin-token").Usage
	assert.Contains(t, usage, "Prefer $AH_ADMIN_TOKEN")
}
