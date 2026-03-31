package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBootstrapTokenFromEnv verifies that the bootstrap token is read from
// the AH_BOOTSTRAP_TOKEN environment variable when no --bootstrap-token flag
// is provided.
func TestBootstrapTokenFromEnv(t *testing.T) {
	t.Setenv("AH_BOOTSTRAP_TOKEN", "test-token-from-env")

	token, err := resolveBootstrapToken("", "AH_BOOTSTRAP_TOKEN")
	require.NoError(t, err)
	assert.Equal(t, "test-token-from-env", token)
}

// TestBootstrapTokenFlagOverridesEnv verifies that the --bootstrap-token flag
// takes priority over the environment variable.
func TestBootstrapTokenFlagOverridesEnv(t *testing.T) {
	t.Setenv("AH_BOOTSTRAP_TOKEN", "env-token")

	token, err := resolveBootstrapToken("flag-token", "AH_BOOTSTRAP_TOKEN")
	require.NoError(t, err)
	assert.Equal(t, "flag-token", token)
}

// TestBootstrapTokenMissingReturnsError verifies that resolveBootstrapToken
// returns an error when no token is available from either source.
func TestBootstrapTokenMissingReturnsError(t *testing.T) {
	// Ensure env var is not set
	t.Setenv("AH_BOOTSTRAP_TOKEN", "")

	_, err := resolveBootstrapToken("", "AH_BOOTSTRAP_TOKEN")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bootstrap token")
}

// TestSaveFlagWritesConfigFile verifies that when --save is used, the API key
// is written to the config file in the format that ahclient.LoadConfig can read
// (fields "url" and "key", not "api_key" and "server_url").
func TestSaveFlagWritesConfigFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	err := saveAPIKey(cfgPath, "test-api-key-value", "https://api.example.com")
	require.NoError(t, err)

	// Must be loadable by ahclient.LoadConfig
	cfg, err := ahclient.LoadConfig(ahclient.LoadOptions{ConfigPath: cfgPath})
	require.NoError(t, err, "saved config must be readable by ahclient.LoadConfig")
	assert.Equal(t, "test-api-key-value", cfg.Key, "Key field must match")
	assert.Equal(t, "https://api.example.com", cfg.URL, "URL field must match")
}

// TestSaveFlagCreatesParentDir verifies that saveAPIKey creates parent
// directories if they don't exist.
func TestSaveFlagCreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "nested", "deep", "config.json")

	err := saveAPIKey(cfgPath, "my-key", "https://api.example.com")
	require.NoError(t, err)

	_, err = os.Stat(cfgPath)
	assert.NoError(t, err, "config file should exist after saveAPIKey")
}

// TestRevokeRequiresConfirmFlag verifies that the revoke command is guarded
// by a --confirm flag and returns an error without it.
func TestRevokeRequiresConfirmFlag(t *testing.T) {
	err := validateRevokeConfirm(false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--confirm")
}

// TestRevokeWithConfirmPasses verifies that the revoke confirm guard
// passes when --confirm is provided.
func TestRevokeWithConfirmPasses(t *testing.T) {
	err := validateRevokeConfirm(true)
	assert.NoError(t, err)
}

// TestRegisterCallsServerAndReturnsAPIKey verifies that the register command
// calls POST /v1/tenants/register with the bootstrap token and extracts
// the api_key from the response.
func TestRegisterCallsServerAndReturnsAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/tenants/register", r.URL.Path)
		assert.Equal(t, "test-bootstrap-token", r.Header.Get("X-Bootstrap-Token"))
		assert.Equal(t, "POST", r.Method)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{
			"tenant_id": "tid-abc",
			"api_key":   "keyid.secretvalue",
		})
	}))
	defer srv.Close()

	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--url", srv.URL, "--key", "unused",
		"register", "Alice", "alice@example.com",
		"--bootstrap-token", "test-bootstrap-token",
	})

	err := cmd.Execute()
	require.NoError(t, err)
	assert.Contains(t, out.String(), "keyid.secretvalue", "output should display the API key")
	assert.Contains(t, out.String(), "tid-abc", "output should display the tenant ID")
}

// TestRegisterPropagatesServerError verifies that a non-201 response from
// the server is surfaced as an error to the caller.
func TestRegisterPropagatesServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "missing or invalid bootstrap token"})
	}))
	defer srv.Close()

	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{
		"--url", srv.URL, "--key", "unused",
		"register", "Alice", "alice@example.com",
		"--bootstrap-token", "bad-token",
	})

	err := cmd.Execute()
	assert.Error(t, err, "a server error should be propagated as a command error")
}

// TestRecoverCallsServerWithEmail verifies that the recover command calls
// POST /v1/auth/recover with both email and bootstrap_token in the body.
func TestRecoverCallsServerWithEmail(t *testing.T) {
	var capturedEmail string
	var capturedToken string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/auth/recover", r.URL.Path)
		assert.Equal(t, "POST", r.Method)

		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		capturedEmail = body["email"]
		capturedToken = body["bootstrap_token"]

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":         "keyid-recover",
			"key":        "keyid.recoveredkey",
			"name":       "recovery-20260330",
			"created_at": 1234567890,
		})
	}))
	defer srv.Close()

	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--url", srv.URL, "--key", "unused",
		"recover",
		"--email", "alice@example.com",
		"--bootstrap-token", "test-token",
	})

	err := cmd.Execute()
	require.NoError(t, err)
	assert.Equal(t, "alice@example.com", capturedEmail)
	assert.Equal(t, "test-token", capturedToken)
	assert.Contains(t, out.String(), "keyid.recoveredkey", "output should display the recovered key")
}

// TestKeyListCallsServer verifies that the key list command calls
// GET /v1/auth/keys with the API key in the Authorization header.
func TestKeyListCallsServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/auth/keys", r.URL.Path)
		assert.Equal(t, "GET", r.Method)
		assert.Equal(t, "Bearer myapikey", r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode([]map[string]interface{}{
			{"id": "k1", "name": "default", "prefix": "abcd1234", "created_at": 1234567890},
			{"id": "k2", "name": "ci", "prefix": "efgh5678", "created_at": 1234567891},
		})
	}))
	defer srv.Close()

	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--url", srv.URL, "--key", "myapikey", "key", "list"})

	err := cmd.Execute()
	require.NoError(t, err)
	output := out.String()
	assert.Contains(t, output, "default", "output should contain key name 'default'")
	assert.Contains(t, output, "ci", "output should contain key name 'ci'")
}

// TestKeyCreateCallsServer verifies that the key create command calls
// POST /v1/auth/keys with the API key in Authorization and returns the new key.
func TestKeyCreateCallsServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/auth/keys", r.URL.Path)
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "Bearer myapikey", r.Header.Get("Authorization"))

		var body map[string]interface{}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "ci-key", body["name"])

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      "newkeyid",
			"name":    "ci-key",
			"api_key": "newkeyid.secretvalue",
			"prefix":  "newkeyid",
		})
	}))
	defer srv.Close()

	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--url", srv.URL, "--key", "myapikey", "key", "create", "--name", "ci-key"})

	err := cmd.Execute()
	require.NoError(t, err)
	assert.Contains(t, out.String(), "newkeyid.secretvalue", "output should display the new API key")
}

// TestKeyRevokeCallsServer verifies that the key revoke command calls
// DELETE /v1/auth/keys/{keyID} with the API key in Authorization.
func TestKeyRevokeCallsServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/auth/keys/target-key-id", r.URL.Path)
		assert.Equal(t, "DELETE", r.Method)
		assert.Equal(t, "Bearer myapikey", r.Header.Get("Authorization"))

		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--url", srv.URL, "--key", "myapikey", "key", "revoke", "target-key-id", "--confirm"})

	err := cmd.Execute()
	require.NoError(t, err)
	assert.Contains(t, out.String(), "target-key-id", "output should mention the revoked key ID")
}

// TestRegisterSave_ConfigReadableByLoadConfig verifies that when --save is used,
// the config file is written in a format that ahclient.LoadConfig can read.
// This is a regression test: register --save previously used a different JSON schema
// (api_key/server_url) and a different path (~/.ahc/) than LoadConfig expects
// (key/url and ~/.ah/).
func TestRegisterSave_ConfigReadableByLoadConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{
			"tenant_id": "tid-save-test",
			"api_key":   "savedkeyid.savedsecret",
		})
	}))
	defer srv.Close()

	// Point HOME to a temp dir so ~/.ah/ is isolated
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--url", srv.URL,
		"register", "SaveTest", "save@test.com",
		"--bootstrap-token", "tok",
		"--save",
	})

	err := cmd.Execute()
	require.NoError(t, err, "register --save should succeed")

	// Now verify that ahclient.LoadConfig can read the saved config
	cfg, err := ahclient.LoadConfig(ahclient.LoadOptions{})
	require.NoError(t, err, "ahclient.LoadConfig should be able to read the saved config")
	assert.Equal(t, srv.URL, cfg.URL, "loaded URL should match the server URL used during registration")
	assert.Equal(t, "savedkeyid.savedsecret", cfg.Key, "loaded Key should match the API key from registration")
}

// TestRegisterSave_DefaultConfigPath verifies that --save writes to the same
// default path that DefaultConfigPath() returns (~/.ah/config.json).
func TestRegisterSave_DefaultConfigPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{
			"tenant_id": "tid-path-test",
			"api_key":   "pathkeyid.pathsecret",
		})
	}))
	defer srv.Close()

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	out := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--url", srv.URL,
		"register", "PathTest", "path@test.com",
		"--bootstrap-token", "tok",
		"--save",
	})

	err := cmd.Execute()
	require.NoError(t, err)

	// Verify the file was written at the expected default path
	expectedPath, err := ahclient.DefaultConfigPath()
	require.NoError(t, err)
	_, statErr := os.Stat(expectedPath)
	require.NoError(t, statErr, "config file should be at the default path returned by DefaultConfigPath()")
}

// TestKeyRevokeNotFoundReturnsError verifies that a 404 response from revoke
// is surfaced as an error.
func TestKeyRevokeNotFoundReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "key not found"})
	}))
	defer srv.Close()

	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	cmd := newRootCmd()
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs([]string{"--url", srv.URL, "--key", "myapikey", "key", "revoke", "nonexistent-id", "--confirm"})

	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}
