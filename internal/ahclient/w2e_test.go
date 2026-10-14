package ahclient_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/ahclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Finding 4: env and file merge per field.
func TestLoadConfig_MergesPerField(t *testing.T) {
	t.Setenv("AH_URL", "https://env.example.com")
	t.Setenv("AH_KEY", "")
	p := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, (&ahclient.Config{URL: "https://file.example.com", Key: "filekey"}).Save(p))
	cfg, err := ahclient.LoadConfig(ahclient.LoadOptions{ConfigPath: p})
	require.NoError(t, err)
	assert.Equal(t, "https://env.example.com", cfg.URL)
	assert.Equal(t, "filekey", cfg.Key)
}

func TestLoadConfig_CorruptFileWithEnvStillErrors(t *testing.T) {
	t.Setenv("AH_URL", "https://env.example.com")
	t.Setenv("AH_KEY", "k")
	p := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(p, []byte("{nope"), 0600))
	_, err := ahclient.LoadConfig(ahclient.LoadOptions{ConfigPath: p})
	require.Error(t, err)
}

// Finding 5: exec gets a request timeout above the server's (default 60s, max 300s).
func TestExecRequestTimeout(t *testing.T) {
	assert.Equal(t, 30*time.Second, ahclient.NewClient("http://x", "k").HTTPClient.Timeout)
	assert.Greater(t, ahclient.ExecRequestTimeout(300), 300*time.Second)
	assert.Greater(t, ahclient.ExecRequestTimeout(1000), 300*time.Second)
	assert.Greater(t, ahclient.ExecRequestTimeout(0), 60*time.Second)
}

// Finding 10: trailing slash must not produce //v1.
func TestNewClient_TrimsTrailingSlash(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		json.NewEncoder(w).Encode([]ahclient.Service{})
	}))
	defer srv.Close()
	_, err := ahclient.NewClient(srv.URL+"/", "k").ListServices()
	require.NoError(t, err)
	assert.Equal(t, "/v1/services", path)
}
