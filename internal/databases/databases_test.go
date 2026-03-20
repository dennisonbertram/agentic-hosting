package databases

import (
	"context"
	"database/sql"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveFakePostgres starts a TCP listener that immediately sends a
// single-byte Postgres Authentication message ('R') to every connection,
// simulating a fully-initialised Postgres server. It returns the bound port
// and a cleanup function.
func serveFakePostgres(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				// Read (and discard) the StartupMessage so the client write succeeds.
				buf := make([]byte, 256)
				c.Read(buf) //nolint:errcheck
				// Send a minimal AuthenticationOK response:
				// 'R' (1 byte message type) + int32 length (5) + int32 auth-type (0 = OK)
				// Total: 1 + 4 + 4 = 9 bytes
				resp := []byte{'R', 0, 0, 0, 8, 0, 0, 0, 0}
				c.Write(resp) //nolint:errcheck
			}(conn)
		}
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	return port
}

// serveFakeRedis starts a TCP listener that replies "+PONG\r\n" to every
// incoming connection, simulating a ready Redis server.
func serveFakeRedis(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 64)
				c.Read(buf) //nolint:errcheck
				c.Write([]byte("+PONG\r\n")) //nolint:errcheck
			}(conn)
		}
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	return port
}

// serveSilentTCP starts a TCP listener that accepts connections but never
// sends any data, simulating a container that has bound its port but has not
// finished initialising its database engine.
func serveSilentTCP(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Accept but never write — hold the connection open briefly.
			go func(c net.Conn) {
				time.Sleep(5 * time.Second)
				c.Close()
			}(conn)
		}
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	return port
}

// ---------------------------------------------------------------------------
// Protocol-level readiness unit tests
// ---------------------------------------------------------------------------

// TestPostgresReady_SilentTCPIsNotReady verifies that a server which accepts
// TCP connections but never sends any Postgres protocol bytes is NOT
// considered ready. This is the core regression test for issue #51.
func TestPostgresReady_SilentTCPIsNotReady(t *testing.T) {
	port := serveSilentTCP(t)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	got := postgresReady(addr)
	assert.False(t, got, "a silent TCP listener should not be considered postgres-ready")
}

// TestPostgresReady_ProperResponseIsReady verifies that a server which sends a
// valid Postgres authentication byte in response to a StartupMessage IS
// considered ready.
func TestPostgresReady_ProperResponseIsReady(t *testing.T) {
	port := serveFakePostgres(t)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	got := postgresReady(addr)
	assert.True(t, got, "a fake postgres server sending 'R' should be considered ready")
}

// TestPostgresReady_ErrorResponseIsReady verifies that even an ErrorResponse
// ('E') from Postgres — e.g. wrong password — is treated as "server is up".
func TestPostgresReady_ErrorResponseIsReady(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 256)
				c.Read(buf) //nolint:errcheck
				// Send ErrorResponse header byte 'E'
				c.Write([]byte{'E', 0, 0, 0, 5, 0}) //nolint:errcheck
			}(conn)
		}
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	addr := "127.0.0.1:" + strconv.Itoa(port)
	got := postgresReady(addr)
	assert.True(t, got, "a Postgres ErrorResponse means the server is up and should be considered ready")
}

// TestRedisReady_SilentTCPIsNotReady verifies that a server which accepts TCP
// connections but never sends +PONG is NOT considered ready.
func TestRedisReady_SilentTCPIsNotReady(t *testing.T) {
	port := serveSilentTCP(t)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	got := redisReady(addr)
	assert.False(t, got, "a silent TCP listener should not be considered redis-ready")
}

// TestRedisReady_ProperPongIsReady verifies that a server which responds with
// +PONG to a PING command IS considered ready.
func TestRedisReady_ProperPongIsReady(t *testing.T) {
	port := serveFakeRedis(t)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	got := redisReady(addr)
	assert.True(t, got, "a fake redis server responding +PONG should be considered ready")
}

// TestRedisReady_WrongResponseIsNotReady verifies that a server responding
// with something other than +PONG is not considered ready.
func TestRedisReady_WrongResponseIsNotReady(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 64)
				c.Read(buf) //nolint:errcheck
				// Send a LOADING error, as Redis does during RDB restore
				c.Write([]byte("-LOADING Redis is loading\r\n")) //nolint:errcheck
			}(conn)
		}
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	addr := "127.0.0.1:" + strconv.Itoa(port)
	got := redisReady(addr)
	assert.False(t, got, "a redis server returning a non-PONG response should not be considered ready")
}

// ---------------------------------------------------------------------------
// Manager integration tests
// ---------------------------------------------------------------------------

func TestCreate_UsesTenantMaxDatabasesQuota(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedDatabaseTestData(t, stateDB, 1)

	dockerClient := &testutil.MockDockerClient{
		// RunDatabaseFn starts a fake Postgres server on the allocated port so
		// that waitForPostgres succeeds with full protocol-level verification.
		RunDatabaseFn: func(ctx context.Context, cfg docker.RunDatabaseConfig) (string, error) {
			// Start a fake Postgres server on the exact port that the manager
			// allocated and will probe.
			ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(cfg.HostPort))
			require.NoError(t, err)
			t.Cleanup(func() { ln.Close() })

			go func() {
				for {
					conn, err := ln.Accept()
					if err != nil {
						return
					}
					go func(c net.Conn) {
						defer c.Close()
						buf := make([]byte, 256)
						c.Read(buf) //nolint:errcheck
						// AuthenticationOK response
						resp := []byte{'R', 0, 0, 0, 8, 0, 0, 0, 0}
						c.Write(resp) //nolint:errcheck
					}(conn)
				}
			}()

			return "container-" + cfg.Name, nil
		},
	}

	mgr := NewManager(stateDB, dockerClient, []byte("0123456789abcdef0123456789abcdef"))

	first, err := mgr.Create(context.Background(), "tenant-1", CreateRequest{Name: "db-1", Type: "postgres"})
	require.NoError(t, err)
	require.Equal(t, "ready", first.Status)

	second, err := mgr.Create(context.Background(), "tenant-1", CreateRequest{Name: "db-2", Type: "postgres"})
	require.Error(t, err)
	assert.Nil(t, second)
	assert.ErrorContains(t, err, "database quota exceeded (max 1)")
}

// TestDelete_WipesVolumeBeforeRemoval verifies that WipeVolume is called before
// RemoveVolume when a database is deleted, ensuring data cannot be recovered by
// a future tenant (issue #9).
func TestDelete_WipesVolumeBeforeRemoval(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedDatabaseTestData(t, stateDB, 5)

	var (
		listeners  []net.Listener
		callOrder  []string
		wipeVolume string
	)
	t.Cleanup(func() {
		for _, ln := range listeners {
			_ = ln.Close()
		}
	})

	dockerClient := &testutil.MockDockerClient{
		RunDatabaseFn: func(ctx context.Context, cfg docker.RunDatabaseConfig) (string, error) {
			// Start a fake Postgres server on the allocated port so that
			// waitForPostgres succeeds with full protocol-level verification.
			ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(cfg.HostPort))
			require.NoError(t, err)
			listeners = append(listeners, ln)

			go func() {
				for {
					conn, err := ln.Accept()
					if err != nil {
						return
					}
					go func(c net.Conn) {
						defer c.Close()
						buf := make([]byte, 256)
						c.Read(buf) //nolint:errcheck
						// AuthenticationOK response
						resp := []byte{'R', 0, 0, 0, 8, 0, 0, 0, 0}
						c.Write(resp) //nolint:errcheck
					}(conn)
				}
			}()

			return "container-" + cfg.Name, nil
		},
		WipeVolumeFn: func(ctx context.Context, name string) error {
			callOrder = append(callOrder, "wipe")
			wipeVolume = name
			return nil
		},
		RemoveVolumeFn: func(ctx context.Context, name string) error {
			callOrder = append(callOrder, "remove")
			return nil
		},
	}

	mgr := NewManager(stateDB, dockerClient, []byte("0123456789abcdef0123456789abcdef"))

	// Create a postgres database
	db, err := mgr.Create(context.Background(), "tenant-1", CreateRequest{Name: "wipe-test", Type: "postgres"})
	require.NoError(t, err)
	require.Equal(t, "ready", db.Status)
	expectedVolume := db.VolumeName

	// Reset call order tracking to isolate delete behaviour
	callOrder = nil

	// Delete the database
	err = mgr.Delete(context.Background(), "tenant-1", db.ID)
	require.NoError(t, err)

	// Wipe must have been called before remove, and on the correct volume
	require.Equal(t, []string{"wipe", "remove"}, callOrder,
		"WipeVolume must be called before RemoveVolume")
	assert.Equal(t, expectedVolume, wipeVolume,
		"WipeVolume must receive the database's volume name")
}

func seedDatabaseTestData(t *testing.T, stateDB *sql.DB, maxDatabases int) {
	t.Helper()
	_, err := stateDB.Exec(
		`INSERT INTO tenants (id, name, email, status, created_at, updated_at) VALUES (?, ?, ?, 'active', 1, 1)`,
		"tenant-1", "Tenant", "tenant@example.com",
	)
	require.NoError(t, err)
	_, err = stateDB.Exec(
		`INSERT INTO tenant_quotas (tenant_id, max_databases) VALUES (?, ?)`,
		"tenant-1", maxDatabases,
	)
	require.NoError(t, err)
}
