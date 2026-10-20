package snapshots

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnapshotList_NameFilterIsLiteral(t *testing.T) {
	mgr, _, db, tenantID, serviceID := setupTestWithDB(t)
	now := time.Now().Unix()
	for i, name := range []string{"v1_0", "v1x0", "100%", "1000", `a\b`} {
		_, err := db.Exec(`INSERT INTO snapshots (id, tenant_id, service_id, name, image_ref, created_at)
			VALUES (?, ?, ?, ?, 'img', ?)`, "s"+string(rune('a'+i)), tenantID, serviceID, name, now)
		require.NoError(t, err)
	}
	names := func(filter string) []string {
		got, err := mgr.List(context.Background(), tenantID, 100, 0, &ListFilter{Name: filter})
		require.NoError(t, err)
		var out []string
		for _, s := range got {
			out = append(out, s.Name)
		}
		return out
	}
	assert.Equal(t, []string{"v1_0"}, names("_"), "underscore is literal, not a wildcard")
	assert.Equal(t, []string{"100%"}, names("%"), "percent is literal")
	assert.Equal(t, []string{`a\b`}, names(`\`), "backslash is literal")
	assert.ElementsMatch(t, []string{"v1_0", "v1x0"}, names("v1"))
}

func TestSnapshotCreate_Validation(t *testing.T) {
	mgr, mock, tenantID, serviceID := setupTest(t)
	_, err := mgr.Create(context.Background(), tenantID, serviceID, CreateRequest{Name: strings.Repeat("n", 129)})
	assert.ErrorContains(t, err, "128 characters")
	_, err = mgr.Create(context.Background(), tenantID, serviceID, CreateRequest{Name: "ok", Description: strings.Repeat("d", 1025)})
	assert.ErrorContains(t, err, "1024 characters")
	_, err = mgr.Create(context.Background(), "other-tenant", serviceID, CreateRequest{Name: "ok"})
	assert.ErrorContains(t, err, "service not found", "cannot snapshot another tenant's service")
	assert.Empty(t, mock.TagImageCalls)
}

func TestSnapshotCreate_TagFailureLeavesNoRow(t *testing.T) {
	mgr, mock, db, tenantID, serviceID := setupTestWithDB(t)
	mock.TagImageFn = func(context.Context, string, string) error { return errors.New("no such image") }

	_, err := mgr.Create(context.Background(), tenantID, serviceID, CreateRequest{Name: "s"})
	require.ErrorContains(t, err, "tag image")
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM snapshots`).Scan(&n))
	assert.Zero(t, n)
	assert.Empty(t, mock.RemoveImageCalls)
}

func TestSnapshotCreate_InsertFailureRemovesTag(t *testing.T) {
	mgr, mock, db, tenantID, serviceID := setupTestWithDB(t)
	_, err := db.Exec(`CREATE TRIGGER block_snap BEFORE INSERT ON snapshots BEGIN SELECT RAISE(ABORT, 'blocked'); END`)
	require.NoError(t, err)

	_, err = mgr.Create(context.Background(), tenantID, serviceID, CreateRequest{Name: "s"})
	require.ErrorContains(t, err, "insert snapshot")
	require.Len(t, mock.TagImageCalls, 1)
	assert.Equal(t, []string{mock.TagImageCalls[0][1]}, mock.RemoveImageCalls, "orphaned tag must be removed")
}

func TestSnapshotCreate_CapturesResourceConfig(t *testing.T) {
	mgr, _, db, tenantID, serviceID := setupTestWithDB(t)
	_, err := db.Exec(`UPDATE tenant_quotas SET max_memory_mb = 1024, max_cpu_cores = 1.5 WHERE tenant_id = ?`, tenantID)
	require.NoError(t, err)

	snap, err := mgr.Create(context.Background(), tenantID, serviceID, CreateRequest{Name: "rc"})
	require.NoError(t, err)
	var rc map[string]float64
	require.NoError(t, json.Unmarshal([]byte(snap.ResourceConfig), &rc))
	assert.Equal(t, map[string]float64{"max_memory_mb": 1024, "max_cpu_cores": 1.5}, rc)
	assert.Equal(t, 8080, snap.Port)
}

func TestSnapshotEnv_CorruptBlob(t *testing.T) {
	mgr, _, db, tenantID, serviceID := setupTestWithDB(t)
	_, err := db.Exec(`INSERT INTO snapshots (id, tenant_id, service_id, name, image_ref, env_encrypted, created_at)
		VALUES ('bad-json', ?, ?, 'n', 'img', 'not json', 1), ('bad-cipher', ?, ?, 'n', 'img', '{"K":"zz"}', 1)`,
		tenantID, serviceID, tenantID, serviceID)
	require.NoError(t, err)

	_, err = mgr.GetEnvKeys(context.Background(), tenantID, "bad-json")
	assert.ErrorContains(t, err, "unmarshal env blob")
	_, err = mgr.RestoreEnvVars(context.Background(), tenantID, "bad-json")
	assert.ErrorContains(t, err, "unmarshal env blob")

	keys, err := mgr.GetEnvKeys(context.Background(), tenantID, "bad-cipher")
	require.NoError(t, err, "listing keys never decrypts")
	assert.Equal(t, map[string]string{"K": "********"}, keys)
	_, err = mgr.RestoreEnvVars(context.Background(), tenantID, "bad-cipher")
	assert.ErrorContains(t, err, `decrypt env var "K"`)
}

func TestSnapshotDelete_WrongTenantKeepsRow(t *testing.T) {
	mgr, mock, db, tenantID, serviceID := setupTestWithDB(t)
	insertSnapshotWithAge(t, db, "keep", tenantID, serviceID, 1)

	err := mgr.Delete(context.Background(), "intruder", "keep")
	assert.ErrorContains(t, err, "not found")
	assert.Empty(t, mock.RemoveImageCalls)

	require.NoError(t, mgr.Delete(context.Background(), tenantID, "keep"))
	assert.Equal(t, []string{"127.0.0.1:5000/snapshots/" + tenantID + ":keep"}, mock.RemoveImageCalls)
}

func TestCleanExpired_NoLimitsIsNoop(t *testing.T) {
	mgr, mock, db, tenantID, serviceID := setupTestWithDB(t)
	insertSnapshotWithAge(t, db, "old", tenantID, serviceID, 1)
	n, err := mgr.CleanExpired(context.Background(), 0, 0)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, mock.RemoveImageCalls)
}
