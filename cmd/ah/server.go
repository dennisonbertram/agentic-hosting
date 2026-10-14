package main

import (
	"net"
	"net/http"
	"time"
)

// listenAddress joins host and port, defaulting to loopback. JoinHostPort
// brackets IPv6 hosts ("::1" -> "[::1]:8080").
func listenAddress(host, port string) string {
	if host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// newHTTPServer deliberately sets no WriteTimeout: long routes (log follow,
// exec, sync, builds) are exempt from chimw.Timeout and a server-wide write
// deadline would cut them off. Normal routes stay bounded by chimw.Timeout.
func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}
