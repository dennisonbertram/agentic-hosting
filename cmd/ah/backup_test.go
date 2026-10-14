package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnforceRetention_PerPrefix(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 12; i++ {
		ts := fmt.Sprintf("20260101-0000%02d", i)
		for _, p := range []string{"ah-", "ah-metering-"} {
			require.NoError(t, os.WriteFile(filepath.Join(dir, p+ts+".db.gz"), nil, 0600))
		}
	}
	enforceRetention(dir, 10)

	for _, p := range []string{"ah-", "ah-metering-"} {
		m, _ := filepath.Glob(filepath.Join(dir, p+"2*.db.gz"))
		assert.Len(t, m, 10, p)
		assert.NoFileExists(t, filepath.Join(dir, p+"20260101-000000.db.gz"))
		assert.NoFileExists(t, filepath.Join(dir, p+"20260101-000001.db.gz"))
		assert.FileExists(t, filepath.Join(dir, p+"20260101-000011.db.gz"))
	}
}

func TestBackupDSN_ReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a b.db")
	rw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = rw.Exec("CREATE TABLE t (x)")
	require.NoError(t, err)
	rw.Close()

	ro, err := sql.Open("sqlite3", backupDSN(path))
	require.NoError(t, err)
	defer ro.Close()
	_, err = ro.Exec("INSERT INTO t VALUES (1)")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "readonly")
}

func TestBackupDSN_RelativePath(t *testing.T) {
	dir := t.TempDir()
	wd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	defer os.Chdir(wd)

	require.NoError(t, os.Mkdir("data", 0700))
	rw, err := sql.Open("sqlite3", "data/ah.db")
	require.NoError(t, err)
	_, err = rw.Exec("CREATE TABLE t (x)")
	require.NoError(t, err)
	rw.Close()

	ro, err := sql.Open("sqlite3", backupDSN("data/ah.db"))
	require.NoError(t, err)
	defer ro.Close()
	_, err = ro.Exec("SELECT * FROM t")
	require.NoError(t, err)
}

func TestMeteringPath_ShortPath(t *testing.T) {
	assert.NotPanics(t, func() {
		assert.Equal(t, "a-metering.db", db.MeteringPath("a"))
	})
	assert.Equal(t, "/d/ah-metering.db", db.MeteringPath("/d/ah.db"))
}
