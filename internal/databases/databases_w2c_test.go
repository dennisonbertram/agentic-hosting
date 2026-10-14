package databases

import (
	"bufio"
	"context"
	"database/sql"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

// serveAuthRedis starts a fake Redis that enforces requirepass: any command
// before a correct AUTH gets -NOAUTH.
func serveAuthRedis(t *testing.T, password string) int {
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
				authed := false
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil || !strings.HasPrefix(line, "*") {
						return
					}
					n, _ := strconv.Atoi(strings.TrimSpace(line[1:]))
					var args []string
					for i := 0; i < n; i++ {
						if _, err := r.ReadString('\n'); err != nil {
							return
						}
						arg, err := r.ReadString('\n')
						if err != nil {
							return
						}
						args = append(args, strings.TrimSpace(arg))
					}
					switch {
					case len(args) == 2 && strings.EqualFold(args[0], "AUTH") && args[1] == password:
						authed = true
						c.Write([]byte("+OK\r\n")) //nolint:errcheck
					case !authed:
						c.Write([]byte("-NOAUTH Authentication required.\r\n")) //nolint:errcheck
					case strings.EqualFold(args[0], "PING"):
						c.Write([]byte("+PONG\r\n")) //nolint:errcheck
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestWaitForRedis_RequirePass(t *testing.T) {
	port := serveAuthRedis(t, "s3cret")
	m := &Manager{}
	assert.True(t, m.waitForRedis(port, "s3cret", 3*time.Second), "readiness check must AUTH before PING")
}

func insertNullContainerDB(t *testing.T, stateDB *sql.DB, id, status string) {
	t.Helper()
	_, err := stateDB.Exec(
		`INSERT INTO databases (id, tenant_id, name, type, status, host, port, db_name, username,
		 password_encrypted, connection_string_encrypted, volume_name, created_at, updated_at)
		 VALUES (?, 'tenant-1', ?, 'postgres', ?, '127.0.0.1', 5999, 'ah', 'ah', 'x', 'x', ?, 10, 10)`,
		id, "n-"+id, status, "vol-"+id,
	)
	require.NoError(t, err)
}

func TestGetAndDelete_NullContainerID(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedDatabaseTestData(t, stateDB, 5)
	insertNullContainerDB(t, stateDB, "dbnull", "failed")
	mgr := &Manager{db: stateDB, docker: &testutil.MockDockerClient{}, masterKey: testKey}

	d, err := mgr.Get(context.Background(), "tenant-1", "dbnull")
	require.NoError(t, err)
	assert.Equal(t, "", d.ContainerID)
	require.NoError(t, mgr.Delete(context.Background(), "tenant-1", "dbnull"))
}

func TestReconcileStale_NullContainerID(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedDatabaseTestData(t, stateDB, 5)
	insertNullContainerDB(t, stateDB, "dbstuck", "provisioning")
	mgr := &Manager{db: stateDB, docker: &testutil.MockDockerClient{}, masterKey: testKey}
	mgr.ReconcileStale()
	var status string
	require.NoError(t, stateDB.QueryRow(`SELECT status FROM databases WHERE id='dbstuck'`).Scan(&status))
	assert.Equal(t, "failed", status)
}

func TestStartAllForTenant_RecreatesStoppedDatabase(t *testing.T) {
	stateDB := testutil.NewStateDB(t)
	seedDatabaseTestData(t, stateDB, 5)
	var cfgs []docker.RunDatabaseConfig
	dockerClient := &testutil.MockDockerClient{
		RunDatabaseFn: func(ctx context.Context, cfg docker.RunDatabaseConfig) (string, error) {
			cfgs = append(cfgs, cfg)
			ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(cfg.HostPort))
			if err != nil {
				return "container-new-" + cfg.Name, nil // fake server from first start still listening
			}
			t.Cleanup(func() { ln.Close() })
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					go func(c net.Conn) {
						defer c.Close()
						buf := make([]byte, 256)
						c.Read(buf)                                  //nolint:errcheck
						c.Write([]byte{'R', 0, 0, 0, 8, 0, 0, 0, 0}) //nolint:errcheck
					}(c)
				}
			}()
			return "container-new-" + cfg.Name, nil
		},
	}
	mgr := NewManager(stateDB, dockerClient, testKey)
	d, err := mgr.Create(context.Background(), "tenant-1", CreateRequest{Name: "pg", Type: "postgres"})
	require.NoError(t, err)
	require.Len(t, cfgs, 1)
	first := cfgs[0]

	mgr.StopAllForTenant(context.Background(), "tenant-1")
	// Free the port the way a removed container would.
	cfgs = nil
	mgr.StartAllForTenant(context.Background(), "tenant-1")

	require.Len(t, cfgs, 1, "stopped database must be recreated")
	assert.Equal(t, first.VolumeName, cfgs[0].VolumeName, "same volume")
	assert.Equal(t, first.Env["POSTGRES_PASSWORD"], cfgs[0].Env["POSTGRES_PASSWORD"], "same credentials")
	assert.Equal(t, first.HostPort, cfgs[0].HostPort)
	got, err := mgr.Get(context.Background(), "tenant-1", d.ID)
	require.NoError(t, err)
	assert.Equal(t, "ready", got.Status)
	assert.NotEmpty(t, got.ContainerID)
}
