package databases

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strconv"
	"strings"
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
				c.Read(buf)                  //nolint:errcheck
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
	got := redisReady(addr, "")
	assert.False(t, got, "a silent TCP listener should not be considered redis-ready")
}

// TestRedisReady_ProperPongIsReady verifies that a server which responds with
// +PONG to a PING command IS considered ready.
func TestRedisReady_ProperPongIsReady(t *testing.T) {
	port := serveFakeRedis(t)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	got := redisReady(addr, "")
	assert.True(t, got, "a fake redis server responding +PONG should be considered ready")
}

func TestRedisReady_AuthenticatesBeforePingAndHandlesPartialReplies(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		auth, err := readTestRedisCommand(reader)
		if err != nil || !reflect.DeepEqual(auth, []string{"AUTH", "synthetic-password"}) {
			return
		}
		_, _ = conn.Write([]byte("+O"))
		_, _ = conn.Write([]byte("K\r\n"))
		ping, err := readTestRedisCommand(reader)
		if err != nil || !reflect.DeepEqual(ping, []string{"PING"}) {
			return
		}
		_, _ = conn.Write([]byte("+PO"))
		_, _ = conn.Write([]byte("NG\r\n"))
	}()

	assert.True(t, redisReady(ln.Addr().String(), "synthetic-password"))
}

func TestRedisReady_RejectsInvalidPassword(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = readTestRedisCommand(bufio.NewReader(conn))
		_, _ = conn.Write([]byte("-WRONGPASS invalid username-password pair\r\n"))
	}()

	assert.False(t, redisReady(ln.Addr().String(), "synthetic-password"))
}

func TestRedisReady_RejectsUnauthenticatedRedis(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = readTestRedisCommand(bufio.NewReader(conn))
		_, _ = conn.Write([]byte("-NOAUTH Authentication required.\r\n"))
	}()

	assert.False(t, redisReady(ln.Addr().String(), ""))
}

func readTestRedisCommand(reader *bufio.Reader) ([]string, error) {
	line, err := reader.ReadString('\n')
	if err != nil || len(line) < 4 || line[0] != '*' {
		return nil, fmt.Errorf("invalid RESP array")
	}
	count, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")[1:])
	if err != nil || count < 1 || count > 4 {
		return nil, fmt.Errorf("invalid RESP array length")
	}
	args := make([]string, 0, count)
	for range count {
		line, err = reader.ReadString('\n')
		if err != nil || len(line) < 4 || line[0] != '$' {
			return nil, fmt.Errorf("invalid RESP bulk length")
		}
		length, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")[1:])
		if err != nil || length < 0 || length > 1024 {
			return nil, fmt.Errorf("invalid RESP bulk length")
		}
		arg := make([]byte, length+2)
		if _, err := io.ReadFull(reader, arg); err != nil || !bytes.HasSuffix(arg, []byte("\r\n")) {
			return nil, fmt.Errorf("invalid RESP bulk argument")
		}
		args = append(args, string(arg[:length]))
	}
	return args, nil
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
	got := redisReady(addr, "")
	assert.False(t, got, "a redis server returning a non-PONG response should not be considered ready")
}

// ---------------------------------------------------------------------------
// Manager integration tests
// ---------------------------------------------------------------------------

func TestCreate_PullFailureWithLocalImageProceeds(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedDatabaseTestData(t, stateDB, 5)
	mock := &testutil.MockDockerClient{
		PullImageFn:   func(context.Context, string) error { return errors.New("registry down") },
		ImageExistsFn: func(context.Context, string) (bool, error) { return true, nil },
	}
	mgr := NewManager(stateDB, mock, []byte("0123456789abcdef0123456789abcdef"))
	_, err := mgr.Create(context.Background(), "tenant-1", CreateRequest{Name: "cached", Type: "postgres"})
	if err != nil {
		assert.NotContains(t, err.Error(), "pull database image")
	}
	assert.Equal(t, 1, mock.RunDatabaseCalls, "a cached image must let provisioning proceed")
}

func TestCreate_PullsDatabaseImageBeforeCreatingResources(t *testing.T) {
	for _, tc := range []struct {
		dbType string
		image  string
	}{
		{"postgres", "postgres:16-alpine"},
		{"redis", "redis:7-alpine"},
	} {
		t.Run(tc.dbType, func(t *testing.T) {
			stateDB := testutil.NewStateDB(t)
			seedDatabaseTestData(t, stateDB, 5)
			mock := &testutil.MockDockerClient{
				PullImageFn: func(context.Context, string) error {
					return errors.New("synthetic pull failure")
				},
			}
			mgr := NewManager(stateDB, mock, []byte("0123456789abcdef0123456789abcdef"))
			got, err := mgr.Create(context.Background(), "tenant-1", CreateRequest{Name: "image-check", Type: tc.dbType})
			require.ErrorContains(t, err, "pull database image")
			assert.Nil(t, got)
			assert.Equal(t, []string{tc.image}, mock.PullImageCalls)
			assert.Empty(t, mock.CreateVolumeCalls, "volume must not be created when the base image cannot be pulled")
			assert.Zero(t, mock.RunDatabaseCalls, "container must not be started when the base image cannot be pulled")
			var rows int
			require.NoError(t, stateDB.QueryRow(`SELECT COUNT(*) FROM databases WHERE tenant_id = ?`, "tenant-1").Scan(&rows))
			assert.Zero(t, rows, "failed image pull must not leave a failed database record")
		})
	}
}

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
	assert.ErrorContains(t, err, "quota exceeded: maximum 1 databases reached")
	assert.Equal(t, []string{"postgres:16-alpine", "postgres:16-alpine"}, dockerClient.PullImageCalls)
}

// TestIssue164_CreateConnectionStringUsesHostLoopbackAndSkipsTenantNetwork
// reproduces issue #164: the API returns a connection string whose host is
// 127.0.0.1, stored that way in the databases row, and Create never attaches
// the container to the tenant network. Combined with databaseHostConfig
// publishing only on host loopback, a service container cannot use this URL.
func TestIssue164_CreateConnectionStringUsesHostLoopbackAndSkipsTenantNetwork(t *testing.T) {
	for _, tc := range []struct {
		dbType     string
		wantScheme string
		wantPath   string
		fakeReady  func(t *testing.T, port int)
	}{
		{
			dbType:     "postgres",
			wantScheme: "postgres://",
			wantPath:   "/ah?sslmode=disable",
			fakeReady:  startFakePostgresOnPort,
		},
		{
			dbType:     "redis",
			wantScheme: "redis://",
			wantPath:   "/0",
			fakeReady:  startFakeRedisOnPort,
		},
	} {
		t.Run(tc.dbType, func(t *testing.T) {
			stateDB := testutil.NewStateDB(t)
			seedDatabaseTestData(t, stateDB, 5)
			mock := &testutil.MockDockerClient{
				RunDatabaseFn: func(ctx context.Context, cfg docker.RunDatabaseConfig) (string, error) {
					tc.fakeReady(t, cfg.HostPort)
					return "container-" + cfg.Name, nil
				},
			}
			mgr := NewManager(stateDB, mock, []byte("0123456789abcdef0123456789abcdef"))
			db, err := mgr.Create(context.Background(), "tenant-1", CreateRequest{Name: "app-data", Type: tc.dbType})
			require.NoError(t, err)
			require.Equal(t, "ready", db.Status)

			assert.Equal(t, "127.0.0.1", db.Host, "Create response host")
			require.True(t, strings.HasPrefix(db.ConnectionString, tc.wantScheme), db.ConnectionString)
			assert.Contains(t, db.ConnectionString, "@127.0.0.1:")
			assert.True(t, strings.HasSuffix(db.ConnectionString, strconv.Itoa(db.Port)+tc.wantPath), db.ConnectionString)

			var storedHost string
			require.NoError(t, stateDB.QueryRow(`SELECT host FROM databases WHERE id = ?`, db.ID).Scan(&storedHost))
			assert.Equal(t, "127.0.0.1", storedHost)

			got, err := mgr.GetConnectionString(context.Background(), "tenant-1", db.ID)
			require.NoError(t, err)
			assert.Equal(t, db.ConnectionString, got, "stored connection string must match create response")

			assert.Empty(t, mock.EnsureNetworkCalls, "Create must not create a tenant/data network for the database")
			assert.Empty(t, mock.EnsureIngressNetworkCalls)
			assert.Empty(t, mock.ConnectNetworkCalls, "database container is not attached to any extra network")
			assert.Empty(t, mock.ConnectNetworkAliasCalls)
			assert.Equal(t, 1, mock.RunDatabaseCalls)
		})
	}
}

func startFakePostgresOnPort(t *testing.T, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
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
				resp := []byte{'R', 0, 0, 0, 8, 0, 0, 0, 0}
				c.Write(resp) //nolint:errcheck
			}(conn)
		}
	}()
}

func startFakeRedisOnPort(t *testing.T, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
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
				reader := bufio.NewReader(c)
				auth, err := readTestRedisCommand(reader)
				if err != nil || len(auth) != 2 || auth[0] != "AUTH" {
					return
				}
				if _, err := c.Write([]byte("+OK\r\n")); err != nil {
					return
				}
				ping, err := readTestRedisCommand(reader)
				if err != nil || !reflect.DeepEqual(ping, []string{"PING"}) {
					return
				}
				_, _ = c.Write([]byte("+PONG\r\n"))
			}(conn)
		}
	}()
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

// ---------------------------------------------------------------------------
// Name validation tests
// ---------------------------------------------------------------------------

func TestValidateName_ValidNames(t *testing.T) {
	valid := []string{
		"mydb",
		"my-db-1",
		"postgres_main",
		"a",                     // single letter
		"A",                     // single uppercase letter
		"db1",                   // letter then digits
		"my_db",                 // underscores
		"My-Mixed_Case1",        // mixed case with hyphens and underscores
		strings.Repeat("a", 63), // max length
	}
	for _, name := range valid {
		t.Run(name, func(t *testing.T) {
			err := ValidateName(name)
			assert.NoError(t, err, "expected name %q to be valid", name)
		})
	}
}

func TestValidateName_InvalidNames(t *testing.T) {
	tests := []struct {
		name    string
		display string // test sub-name for readability
	}{
		{"", "empty"},
		{"1starts-with-number", "starts_with_number"},
		{"has spaces", "has_spaces"},
		{strings.Repeat("a", 64), "too_long_64_chars"},
		{"special!chars", "special_chars"},
		{"-starts-with-hyphen", "starts_with_hyphen"},
		{"_starts-with-underscore", "starts_with_underscore"},
		{"ends-with-hyphen-", "ends_with_hyphen"},
		{"name.with.dots", "has_dots"},
		{"name@domain", "has_at_sign"},
		{"name with\ttab", "has_tab"},
		{"1", "single_digit"},
	}
	for _, tt := range tests {
		t.Run(tt.display, func(t *testing.T) {
			err := ValidateName(tt.name)
			assert.Error(t, err, "expected name %q to be invalid", tt.name)
		})
	}
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

func TestGetAndDeleteDatabaseWithNullContainerID(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedDatabaseTestData(t, stateDB, 5)
	dockerClient := &testutil.MockDockerClient{}
	mgr := NewManager(stateDB, dockerClient, []byte("0123456789abcdef0123456789abcdef"))
	for _, tc := range []struct {
		id     string
		status string
	}{
		{id: "failed-create", status: "failed"},
		{id: "provisioning-create", status: "provisioning"},
	} {
		_, err := stateDB.Exec(`INSERT INTO databases
			(id, tenant_id, name, type, status, container_id, port, db_name, username, password_encrypted, volume_name, created_at, updated_at)
			VALUES (?, 'tenant-1', ?, 'redis', ?, NULL, 16379, '', '', 'encrypted', ?, 1, 1)`,
			tc.id, tc.id, tc.status, "volume-"+tc.id)
		require.NoError(t, err)
	}

	for _, tc := range []struct {
		id     string
		status string
	}{
		{id: "failed-create", status: "failed"},
		{id: "provisioning-create", status: "provisioning"},
	} {
		db, err := mgr.Get(context.Background(), "tenant-1", tc.id)
		require.NoError(t, err)
		assert.Equal(t, tc.status, db.Status)
		assert.Empty(t, db.ContainerID)
	}

	err := mgr.Delete(context.Background(), "tenant-1", "failed-create")
	require.NoError(t, err)
	assert.Empty(t, dockerClient.StopContainerCalls)
	assert.Empty(t, dockerClient.RemoveContainerCalls)
	assert.Equal(t, []string{"volume-failed-create"}, dockerClient.WipeVolumeCalls)
	assert.Equal(t, []string{"volume-failed-create"}, dockerClient.RemoveVolumeCalls)
	var count int
	require.NoError(t, stateDB.QueryRow(`SELECT COUNT(*) FROM databases WHERE id = 'failed-create'`).Scan(&count))
	assert.Zero(t, count)
}
