package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression: servers with AH_ADMIN_TOKEN accept only the admin token for
// recovery (403 for a bootstrap token). `ahc recover` must be able to send it
// as X-Admin-Token from --admin-token or $AH_ADMIN_TOKEN.
func TestRecover_SendsAdminToken(t *testing.T) {
	var gotAdmin string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAdmin = r.Header.Get("X-Admin-Token")
		b, _ := io.ReadAll(r.Body)
		gotBody = nil
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"k1","key":"k1.secret","name":"recovery"}`)
	}))
	t.Cleanup(srv.Close)

	run := func(args ...string) string {
		isolateHome(t)
		t.Setenv("AH_URL", srv.URL)
		cmd := newRootCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"recover", "--email", "a@b.c"}, args...))
		require.NoError(t, cmd.Execute(), out.String())
		return out.String()
	}

	t.Setenv("AH_BOOTSTRAP_TOKEN", "")
	t.Setenv("AH_ADMIN_TOKEN", "admin-from-env")
	out := run()
	assert.Equal(t, "admin-from-env", gotAdmin)
	assert.Empty(t, gotBody["bootstrap_token"])
	assert.Contains(t, out, "k1.secret")

	run("--admin-token", "admin-from-flag")
	assert.Equal(t, "admin-from-flag", gotAdmin)

	// An explicit --bootstrap-token beats $AH_ADMIN_TOKEN and goes in the body.
	run("--bootstrap-token", "boot")
	assert.Empty(t, gotAdmin)
	assert.Equal(t, "boot", gotBody["bootstrap_token"])
}

func TestRecover_NoTokenIsError(t *testing.T) {
	isolateHome(t)
	t.Setenv("AH_URL", "http://127.0.0.1:1")
	t.Setenv("AH_ADMIN_TOKEN", "")
	t.Setenv("AH_BOOTSTRAP_TOKEN", "")
	cmd := newRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"recover", "--email", "a@b.c"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "admin or bootstrap token required")
}
