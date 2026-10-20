package main

import (
	"compress/gzip"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// backupSQLite must produce a gzipped, standalone copy of the DB (including a
// path that needs quote escaping in VACUUM INTO) and leave no temp files.
func TestBackupSQLite_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "ah.db")
	rw, err := sql.Open("sqlite3", src)
	require.NoError(t, err)
	_, err = rw.Exec("CREATE TABLE t (x TEXT); INSERT INTO t VALUES ('kept')")
	require.NoError(t, err)
	require.NoError(t, rw.Close())

	outDir := filepath.Join(dir, "it's backups")
	require.NoError(t, os.Mkdir(outDir, 0o700))
	dst := filepath.Join(outDir, "ah-20260101-000000.db.gz")
	require.NoError(t, backupSQLite(src, dst))

	entries, err := os.ReadDir(outDir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "only the final .db.gz may remain")

	f, err := os.Open(dst)
	require.NoError(t, err)
	defer f.Close()
	zr, err := gzip.NewReader(f)
	require.NoError(t, err)
	restored := filepath.Join(dir, "restored.db")
	out, err := os.Create(restored)
	require.NoError(t, err)
	_, err = io.Copy(out, zr)
	require.NoError(t, err)
	require.NoError(t, out.Close())

	rdb, err := sql.Open("sqlite3", restored)
	require.NoError(t, err)
	defer rdb.Close()
	var x string
	require.NoError(t, rdb.QueryRow("SELECT x FROM t").Scan(&x))
	assert.Equal(t, "kept", x)
}

func TestBackupSQLite_MissingSourceFails(t *testing.T) {
	dir := t.TempDir()
	err := backupSQLite(filepath.Join(dir, "missing.db"), filepath.Join(dir, "out.db.gz"))
	require.Error(t, err)
	assert.NoFileExists(t, filepath.Join(dir, "missing.db"), "read-only open must not create the source")
	assert.NoFileExists(t, filepath.Join(dir, "out.db.gz"))
}

func TestGzipFileAtomic_MissingSource(t *testing.T) {
	dir := t.TempDir()
	err := gzipFileAtomic(filepath.Join(dir, "nope"), filepath.Join(dir, "out.gz"))
	require.Error(t, err)
	assert.NoFileExists(t, filepath.Join(dir, "out.gz.tmp"))
}
