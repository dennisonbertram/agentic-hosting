package db

import (
	"database/sql"
	"fmt"
	"io/fs"
	"log"
	"sort"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	StateDB    *sql.DB
	MeteringDB *sql.DB
}

// Open opens the state DB and a metering DB derived from it (<db>-metering.db).
func Open(dbPath string) (*Store, error) {
	return OpenWithMetering(dbPath, "")
}

// OpenWithMetering is Open with an explicit metering DB path; empty means derived.
func OpenWithMetering(dbPath, meteringPath string) (*Store, error) {
	stateDB, err := openDB(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open state db: %w", err)
	}

	if meteringPath == "" {
		meteringPath = MeteringPath(dbPath)
	}
	meteringDB, err := openDB(meteringPath)
	if err != nil {
		stateDB.Close()
		return nil, fmt.Errorf("open metering db: %w", err)
	}

	store := &Store{StateDB: stateDB, MeteringDB: meteringDB}

	if err := store.runMigrations(); err != nil {
		store.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	return store, nil
}

// MeteringPath returns the metering database path for a state database path.
func MeteringPath(dbPath string) string {
	return strings.TrimSuffix(dbPath, ".db") + "-metering.db"
}

// openDB opens a WAL-mode SQLite pool. _txlock=immediate makes every
// Begin/BeginTx a BEGIN IMMEDIATE: a deferred transaction that reads and
// then writes must upgrade its lock, and if another connection committed in
// between SQLite returns SQLITE_BUSY_SNAPSHOT at once without consulting the
// busy timeout. Taking the write lock up front lets _busy_timeout apply.
func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}

	// WAL mode allows concurrent readers with a single writer.
	// Allow a small pool so reads don't block on writes.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)

	return db, nil
}

func (s *Store) runMigrations() error {
	// Migration files must be prefixed with their target database:
	// "state_*.sql" -> StateDB, "metering_*.sql" -> MeteringDB.
	// Any other naming is rejected to prevent silent misrouting.
	names, err := migrationNames()
	if err != nil {
		return err
	}
	for _, name := range names {
		if !strings.HasPrefix(name, "state_") && !strings.HasPrefix(name, "metering_") {
			return fmt.Errorf("migration %q must start with state_ or metering_", name)
		}
	}

	if err := applyMigrations(s.StateDB, "state_", true); err != nil {
		return err
	}
	return applyMigrations(s.MeteringDB, "metering_", true)
}

// ApplyStateMigrations applies all state_*.sql migrations from the embedded FS
// to the given database. It is exported so that testutil and other packages can
// bring up an in-memory SQLite database with the real schema.
func ApplyStateMigrations(stateDB *sql.DB) error {
	return applyMigrations(stateDB, "state_", false)
}

// ApplyMeteringMigrations applies all metering_*.sql migrations from the
// embedded FS to the given database. Exported for use by testutil and other
// packages that need an in-memory metering schema for testing.
func ApplyMeteringMigrations(meteringDB *sql.DB) error {
	return applyMigrations(meteringDB, "metering_", false)
}

// migrationNames returns the embedded *.sql migration file names in
// lexical (application) order.
func migrationNames() ([]string, error) {
	entries, err := fs.ReadDir(MigrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// applyMigrations applies every embedded migration whose name starts with
// prefix to target, skipping those already recorded in schema_migrations.
// Each migration and its bookkeeping row commit in one transaction.
func applyMigrations(target *sql.DB, prefix string, verbose bool) error {
	if _, err := target.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}

	names, err := migrationNames()
	if err != nil {
		return err
	}

	for _, name := range names {
		if !strings.HasPrefix(name, prefix) {
			continue
		}

		var count int
		if err := target.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, name).Scan(&count); err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if count > 0 {
			if verbose {
				log.Printf("migration already applied: %s", name)
			}
			continue
		}

		data, err := fs.ReadFile(MigrationsFS, "migrations/"+name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		tx, err := target.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", name, err)
		}

		// Execute the entire migration file as a single exec (SQLite3 driver
		// supports multi-statement execution). This avoids fragile semicolon
		// splitting that breaks on semicolons inside strings or triggers.
		if _, err := tx.Exec(string(data)); err != nil {
			tx.Rollback()
			return fmt.Errorf("exec migration %s: %w", name, err)
		}

		if _, err := tx.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`,
			name, time.Now().Unix()); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}

		if verbose {
			log.Printf("migration applied: %s", name)
		}
	}

	return nil
}

func (s *Store) Close() error {
	var errs []error
	if s.StateDB != nil {
		if err := s.StateDB.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.MeteringDB != nil {
		if err := s.MeteringDB.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("close errors: %v", errs)
	}
	return nil
}
