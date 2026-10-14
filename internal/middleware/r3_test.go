package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A short request on a keep-alive connection must not leave its write
// deadline behind for a later long-running request on the same connection.
func TestIdleWriteDeadline_ClearsDeadlineFromPriorRequest(t *testing.T) {
	short := 150 * time.Millisecond
	mux := http.NewServeMux()
	mux.Handle("/short", WriteDeadline(short)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})))
	mux.Handle("/long", IdleWriteDeadline(time.Minute)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * short) // work, then finish without an explicit write
	})))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := &http.Client{Transport: &http.Transport{}}

	resp, err := client.Get(srv.URL + "/short")
	require.NoError(t, err)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	resp, err = client.Get(srv.URL + "/long")
	require.NoError(t, err)
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Empty(t, b)
}

func TestIdempotency_SweepKeepsPendingEntries(t *testing.T) {
	store := &IdempotencyStore{entries: map[string]*idempotencyEntry{}}
	past := time.Now().Add(-time.Minute)
	store.entries["pending"] = &idempotencyEntry{pending: true, expiresAt: past}
	store.entries["done"] = &idempotencyEntry{expiresAt: past}
	store.sweep(time.Now())
	assert.Contains(t, store.entries, "pending", "a running handler keeps its reservation")
	assert.NotContains(t, store.entries, "done")
}
