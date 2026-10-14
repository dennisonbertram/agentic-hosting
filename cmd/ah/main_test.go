package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestListenAddr(t *testing.T) {
	assert.Equal(t, "127.0.0.1:8080", listenAddress("", "8080"))
	assert.Equal(t, "0.0.0.0:8080", listenAddress("0.0.0.0", "8080"))
	assert.Equal(t, "[::1]:8080", listenAddress("::1", "8080"))
	assert.Equal(t, "localhost:8080", listenAddress("localhost", "8080"))
}

// Long routes are exempt from chimw.Timeout, so a server-wide WriteTimeout
// would cut their responses (logs follow, exec, sync) at that limit.
func TestNewHTTPServerHasNoWriteTimeout(t *testing.T) {
	s := newHTTPServer("127.0.0.1:0", nil)
	assert.Zero(t, s.WriteTimeout)
	assert.NotZero(t, s.ReadHeaderTimeout)
	assert.NotZero(t, s.IdleTimeout)
}
