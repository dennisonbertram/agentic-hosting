package ahclient_test

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClient_SandboxRoutes(t *testing.T) {
	ttl := 600
	cases := []struct {
		name, method, path, query, resp string
		call                            func(c *ahclient.Client) error
	}{
		{"CreateSandbox", "POST", "/v1/sandboxes", "", `{}`, func(c *ahclient.Client) error {
			_, err := c.CreateSandbox(ahclient.CreateSandboxRequest{Source: ahclient.SandboxSource{Type: "template"}, TTLSeconds: &ttl})
			return err
		}},
		{"ListSandboxes", "GET", "/v1/sandboxes", "", `[]`, func(c *ahclient.Client) error { _, err := c.ListSandboxes(""); return err }},
		{"ListSandboxesStatus", "GET", "/v1/sandboxes", "status=running", `[]`, func(c *ahclient.Client) error { _, err := c.ListSandboxes("running"); return err }},
		{"GetSandbox", "GET", "/v1/sandboxes/s1", "", `{}`, func(c *ahclient.Client) error { _, err := c.GetSandbox("s1"); return err }},
		{"DeleteSandbox", "DELETE", "/v1/sandboxes/s1", "", ``, func(c *ahclient.Client) error { return c.DeleteSandbox("s1") }},
		{"SandboxExec", "POST", "/v1/sandboxes/s1/exec", "", `{}`, func(c *ahclient.Client) error {
			_, err := c.SandboxExec("s1", []string{"ls"}, "", 0)
			return err
		}},
		{"ExtendSandbox", "POST", "/v1/sandboxes/s1/extend", "", `{}`, func(c *ahclient.Client) error { _, err := c.ExtendSandbox("s1", 60); return err }},
		{"GetSandboxLogs", "GET", "/v1/sandboxes/s1/logs", "follow=true&tail=5", `x`, func(c *ahclient.Client) error {
			rc, err := c.GetSandboxLogs("s1", true, 5)
			if err == nil {
				_, _ = io.ReadAll(rc)
				rc.Close()
			}
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs := newAPIRecorder(t, tc.resp)
			require.NoError(t, tc.call(ahclient.NewClient(rs.URL, "k")))
			assert.Equal(t, tc.method, rs.method)
			assert.Equal(t, tc.path, rs.path)
			assert.Equal(t, tc.query, rs.query)
		})
	}
}

func TestClient_SandboxBodies(t *testing.T) {
	rs := newAPIRecorder(t, `{"id":"s1","source":{"type":"image","ref":"alpine"},"expires_at":5}`)
	c := ahclient.NewClient(rs.URL, "k")
	mem := 256
	s, err := c.CreateSandbox(ahclient.CreateSandboxRequest{
		Source: ahclient.SandboxSource{Type: "image", Image: "alpine"}, MemoryMB: &mem, Run: "idle",
	})
	require.NoError(t, err)
	assert.Equal(t, "s1", s.ID)
	assert.Equal(t, "alpine", s.Source.Ref)
	assert.Equal(t, int64(5), s.ExpiresAt)
	var sent map[string]any
	require.NoError(t, json.Unmarshal([]byte(rs.body), &sent))
	assert.Equal(t, map[string]any{"type": "image", "image": "alpine"}, sent["source"])
	assert.EqualValues(t, 256, sent["memory_mb"])
	assert.NotContains(t, sent, "ttl_seconds", "unset optional fields are omitted")
	assert.NotContains(t, sent, "include_env")

	_, err = c.SandboxExec("s1", []string{"echo", "hi"}, "/app", 30)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(rs.body), &sent))
	assert.Equal(t, []any{"echo", "hi"}, sent["command"])
	assert.Equal(t, "/app", sent["work_dir"])
	assert.EqualValues(t, 30, sent["timeout"])

	_, err = c.ExtendSandbox("s1", 900)
	require.NoError(t, err)
	assert.JSONEq(t, `{"ttl_seconds":900}`, rs.body)
	assert.Equal(t, http.MethodPost, rs.method)
}
