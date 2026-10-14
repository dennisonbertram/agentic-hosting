package ahclient

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type deadlineRoundTripper struct {
	deadlines []time.Duration
}

func (rt *deadlineRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	deadline, ok := req.Context().Deadline()
	if ok {
		rt.deadlines = append(rt.deadlines, time.Until(deadline))
	} else {
		rt.deadlines = append(rt.deadlines, 0)
	}

	status := http.StatusOK
	body := `{"id":"db-1","status":"ready"}`
	if req.Method == http.MethodGet {
		body = `[]`
	} else {
		status = http.StatusCreated
	}
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

func TestCreateDatabaseUsesExtendedTimeoutAndPreservesClientSettings(t *testing.T) {
	transport := &deadlineRoundTripper{}
	client := NewClient("https://api.example", "key")
	client.HTTPClient.Transport = transport
	originalCheckRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.HTTPClient.CheckRedirect = originalCheckRedirect

	db, err := client.CreateDatabase(CreateDatabaseRequest{Name: "cold-db", Type: "postgres"})
	require.NoError(t, err)
	require.Equal(t, "db-1", db.ID)
	require.Len(t, transport.deadlines, 1)
	require.Greater(t, transport.deadlines[0], 6*time.Minute)
	require.LessOrEqual(t, transport.deadlines[0], 6*time.Minute+5*time.Second)
	require.Equal(t, 30*time.Second, client.HTTPClient.Timeout, "operation-specific timeout must not mutate shared client")
	require.Same(t, transport, client.HTTPClient.Transport, "configured transport must remain installed")
	require.NotNil(t, client.HTTPClient.CheckRedirect, "other HTTP client settings must remain configured")
}

func TestOtherDatabaseRequestsKeepDefaultTimeout(t *testing.T) {
	transport := &deadlineRoundTripper{}
	client := NewClient("https://api.example", "key")
	client.HTTPClient.Transport = transport

	_, err := client.ListDatabases()
	require.NoError(t, err)
	require.Len(t, transport.deadlines, 1)
	require.Greater(t, transport.deadlines[0], 29*time.Second)
	require.LessOrEqual(t, transport.deadlines[0], 30*time.Second)
	require.Equal(t, 30*time.Second, client.HTTPClient.Timeout)
}

var _ http.RoundTripper = (*deadlineRoundTripper)(nil)
