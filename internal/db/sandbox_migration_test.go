package db_test

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSandboxMigration_DefaultsExistingRows applies every state migration
// before 021, inserts an environment and a quota row, then applies the rest:
// the existing rows must read back as kind='environment' with the sandbox
// quota defaults.
func TestSandboxMigration_DefaultsExistingRows(t *testing.T) {
	sqlDB, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "s.db")+"?_foreign_keys=on")
	require.NoError(t, err)
	t.Cleanup(func() { sqlDB.Close() })
	_, err = sqlDB.Exec(`CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`)
	require.NoError(t, err)

	entries, err := db.MigrationsFS.ReadDir("migrations")
	require.NoError(t, err)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "state_") || name >= "state_021" {
			continue
		}
		data, err := db.MigrationsFS.ReadFile("migrations/" + name)
		require.NoError(t, err)
		_, err = sqlDB.Exec(string(data))
		require.NoError(t, err, name)
		_, err = sqlDB.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`, name, time.Now().Unix())
		require.NoError(t, err)
	}
	_, err = sqlDB.Exec(`INSERT INTO tenants (id, name, email, created_at, updated_at) VALUES ('t1','t','t@e.x',1,1)`)
	require.NoError(t, err)
	_, err = sqlDB.Exec(`INSERT INTO tenant_quotas (tenant_id) VALUES ('t1')`)
	require.NoError(t, err)
	_, err = sqlDB.Exec(`INSERT INTO environments (id, tenant_id, name, created_at, updated_at) VALUES ('e1','t1','dev',1,1)`)
	require.NoError(t, err)

	require.NoError(t, db.ApplyStateMigrations(sqlDB))
	require.NoError(t, db.ApplyStateMigrations(sqlDB), "second run is a no-op")

	var kind, sourceType, runMode, egress string
	var mem, cpu int
	var maxExp sql.NullInt64
	require.NoError(t, sqlDB.QueryRow(`SELECT kind, source_type, run_mode, egress, memory_mb, cpu_millicores, max_expires_at
		FROM environments WHERE id='e1'`).Scan(&kind, &sourceType, &runMode, &egress, &mem, &cpu, &maxExp))
	assert.Equal(t, "environment", kind)
	assert.Equal(t, "template", sourceType)
	assert.Equal(t, "idle", runMode)
	assert.Equal(t, "none", egress)
	assert.Zero(t, mem)
	assert.Zero(t, cpu)
	assert.False(t, maxExp.Valid)

	var maxSbx, ttl, lifetime, egressAllowed int
	require.NoError(t, sqlDB.QueryRow(`SELECT max_sandboxes, max_sandbox_ttl_seconds, max_sandbox_lifetime_seconds, sandbox_egress_allowed
		FROM tenant_quotas WHERE tenant_id='t1'`).Scan(&maxSbx, &ttl, &lifetime, &egressAllowed))
	assert.Equal(t, 5, maxSbx)
	assert.Equal(t, 14400, ttl)
	assert.Equal(t, 86400, lifetime)
	assert.Zero(t, egressAllowed)

	// 022: existing rows keep the volume workspace (disk_mb 0) and get the
	// default tenant workspace cap.
	var diskMB, maxDisk int
	require.NoError(t, sqlDB.QueryRow(`SELECT disk_mb FROM environments WHERE id='e1'`).Scan(&diskMB))
	require.NoError(t, sqlDB.QueryRow(`SELECT max_sandbox_disk_mb FROM tenant_quotas WHERE tenant_id='t1'`).Scan(&maxDisk))
	assert.Zero(t, diskMB)
	assert.Equal(t, 1024, maxDisk)
}

func TestMeteringMigration_ResourceKind(t *testing.T) {
	m := testutil.NewMeteringDB(t)
	_, err := m.Exec(`INSERT INTO usage_events (id, tenant_id, service_id, event_type, recorded_at) VALUES ('u1','t1','s1','sample',1)`)
	require.NoError(t, err)
	_, err = m.Exec(`INSERT INTO usage_daily (tenant_id, service_id, date) VALUES ('t1','s1','2026-10-20')`)
	require.NoError(t, err)
	var k1, k2 string
	require.NoError(t, m.QueryRow(`SELECT resource_kind FROM usage_events WHERE id='u1'`).Scan(&k1))
	require.NoError(t, m.QueryRow(`SELECT resource_kind FROM usage_daily WHERE tenant_id='t1'`).Scan(&k2))
	assert.Equal(t, "service", k1)
	assert.Equal(t, "service", k2)
	require.NoError(t, db.ApplyMeteringMigrations(m), "idempotent")
}
