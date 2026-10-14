package main

import (
	"compress/gzip"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/diskcheck"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func runBackup(dbPath string) {
	backupDir := filepath.Join(filepath.Dir(dbPath), "backups")
	if err := os.MkdirAll(backupDir, 0700); err != nil {
		log.Fatalf("create backup dir: %v", err)
	}

	// Check disk space before backup to avoid filling disk
	if err := diskcheck.Check(backupDir, 80, 90); err != nil {
		log.Fatalf("disk check before backup: %v", err)
	}

	ts := time.Now().Format("20060102-150405")

	// Backup state DB using VACUUM INTO for WAL-safe consistent snapshot
	if err := backupSQLite(dbPath, filepath.Join(backupDir, fmt.Sprintf("ah-%s.db.gz", ts))); err != nil {
		log.Fatalf("backup state db: %v", err)
	}

	// Backup metering DB if it exists
	meteringPath := db.MeteringPath(dbPath)
	if _, err := os.Stat(meteringPath); err == nil {
		if err := backupSQLite(meteringPath, filepath.Join(backupDir, fmt.Sprintf("ah-metering-%s.db.gz", ts))); err != nil {
			log.Printf("WARNING: backup metering db: %v", err)
		}
	}

	// Enforce backup retention: keep only the 10 most recent backups
	enforceRetention(backupDir, 10)

	log.Printf("backup complete: %s", backupDir)
}

// enforceRetention keeps the newest `keep` backups for each of the state
// (ah-<ts>) and metering (ah-metering-<ts>) series; they are pruned separately.
func enforceRetention(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var state, metering []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".db.gz") {
			continue
		}
		if strings.HasPrefix(name, "ah-metering-") {
			metering = append(metering, name)
		} else {
			state = append(state, name)
		}
	}
	for _, files := range [][]string{state, metering} {
		if len(files) <= keep {
			continue
		}
		// Sort ascending (oldest first); timestamped names sort naturally
		sort.Strings(files)
		for _, name := range files[:len(files)-keep] {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				log.Printf("backup: failed to remove old backup %s: %v", name, err)
			} else {
				log.Printf("backup: pruned old backup %s", name)
			}
		}
	}
}

// backupDSN returns a read-only SQLite URI for path. The file: prefix is
// required; without it the driver ignores mode=ro.
func backupDSN(path string) string {
	return (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String()
}

// backupSQLite creates a consistent backup using SQLite's VACUUM INTO,
// then gzips the result with atomic temp-file rename.
func backupSQLite(srcDB, dstGz string) error {
	// Open the source DB in read-only mode
	db, err := sql.Open("sqlite3", backupDSN(srcDB))
	if err != nil {
		return fmt.Errorf("open %s: %w", srcDB, err)
	}
	defer db.Close()

	// Use a temp file for the VACUUM INTO target
	tmpDB := dstGz + ".vacuumtmp"
	defer os.Remove(tmpDB) // clean up intermediate file

	// VACUUM INTO creates a consistent, standalone copy of the database
	// Use a 5-minute timeout to prevent hanging on busy/locked DB.
	vacuumCtx, vacuumCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer vacuumCancel()
	// Escape single quotes in path to prevent SQL injection
	escapedPath := strings.ReplaceAll(tmpDB, "'", "''")
	_, err = db.ExecContext(vacuumCtx, fmt.Sprintf("VACUUM INTO '%s'", escapedPath))
	if err != nil {
		return fmt.Errorf("vacuum into: %w", err)
	}

	// Now gzip the consistent copy with atomic write
	if err := gzipFileAtomic(tmpDB, dstGz); err != nil {
		return err
	}

	return nil
}

// gzipFileAtomic compresses src to dst using a temp+rename pattern.
func gzipFileAtomic(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}

	tmpDst := dst + ".tmp"
	out, err := os.OpenFile(tmpDst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("create temp %s: %w", tmpDst, err)
	}

	gz := gzip.NewWriter(out)

	_, copyErr := io.Copy(gz, in)

	if err := gz.Close(); err != nil {
		out.Close()
		os.Remove(tmpDst)
		if copyErr != nil {
			return fmt.Errorf("compress: %w", copyErr)
		}
		return fmt.Errorf("finalize gzip: %w", err)
	}

	if copyErr != nil {
		out.Close()
		os.Remove(tmpDst)
		return fmt.Errorf("compress: %w", copyErr)
	}

	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmpDst)
		return fmt.Errorf("sync: %w", err)
	}

	if err := out.Close(); err != nil {
		os.Remove(tmpDst)
		return fmt.Errorf("close: %w", err)
	}

	if err := os.Rename(tmpDst, dst); err != nil {
		os.Remove(tmpDst)
		return fmt.Errorf("rename: %w", err)
	}

	log.Printf("backed up %s → %s (%.1f KB)", filepath.Base(src), filepath.Base(dst), float64(info.Size())/1024)
	return nil
}
