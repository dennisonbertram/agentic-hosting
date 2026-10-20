package db

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A read-then-write transaction must not fail with SQLITE_BUSY_SNAPSHOT when
// another connection commits between its read and its write.
func TestOpenDB_ReadThenWriteTxSurvivesConcurrentCommit(t *testing.T) {
	sqlDB, err := openDB(filepath.Join(t.TempDir(), "tx.db"))
	require.NoError(t, err)
	t.Cleanup(func() { sqlDB.Close() })
	_, err = sqlDB.Exec(`CREATE TABLE kv (k TEXT PRIMARY KEY, v INTEGER)`)
	require.NoError(t, err)
	_, err = sqlDB.Exec(`INSERT INTO kv VALUES ('a', 1), ('b', 1)`)
	require.NoError(t, err)

	tx, err := sqlDB.Begin()
	require.NoError(t, err)
	var v int
	require.NoError(t, tx.QueryRow(`SELECT v FROM kv WHERE k = 'a'`).Scan(&v))

	// Another connection writes while tx is open. With BEGIN IMMEDIATE it
	// waits (busy timeout) for tx; run it concurrently.
	done := make(chan error, 1)
	go func() {
		_, err := sqlDB.Exec(`UPDATE kv SET v = v + 1 WHERE k = 'b'`)
		done <- err
	}()

	// The tx took the write lock at BEGIN, so the other writer waits rather
	// than committing underneath our read snapshot.
	select {
	case err := <-done:
		t.Fatalf("concurrent writer committed inside the tx window (err=%v); BEGIN is not IMMEDIATE", err)
	case <-time.After(150 * time.Millisecond):
	}

	_, err = tx.Exec(`UPDATE kv SET v = ? WHERE k = 'a'`, v+1)
	require.NoError(t, err, "write after read in the same tx must not get SQLITE_BUSY")
	require.NoError(t, tx.Commit())
	require.NoError(t, <-done)
}
