package main

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/dennisonbertram/agentic-hosting/internal/crypto"
	_ "github.com/mattn/go-sqlite3"
)

func runRotateKey(args []string) {
	// Parse flags manually since the global flag set is used by 'serve'.
	var oldKeyPath, newKeyPath, dbPath string
	var dryRun bool

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--old-key-path":
			if i+1 >= len(args) {
				log.Fatalf("--old-key-path requires an argument")
			}
			i++
			oldKeyPath = args[i]
		case "--new-key-path":
			if i+1 >= len(args) {
				log.Fatalf("--new-key-path requires an argument")
			}
			i++
			newKeyPath = args[i]
		case "--db-path":
			if i+1 >= len(args) {
				log.Fatalf("--db-path requires an argument")
			}
			i++
			dbPath = args[i]
		case "--dry-run":
			dryRun = true
		case "--help", "-h":
			printRotateKeyUsage()
			os.Exit(0)
		default:
			// Support --flag=value syntax
			if strings.HasPrefix(args[i], "--old-key-path=") {
				oldKeyPath = strings.TrimPrefix(args[i], "--old-key-path=")
			} else if strings.HasPrefix(args[i], "--new-key-path=") {
				newKeyPath = strings.TrimPrefix(args[i], "--new-key-path=")
			} else if strings.HasPrefix(args[i], "--db-path=") {
				dbPath = strings.TrimPrefix(args[i], "--db-path=")
			} else {
				log.Fatalf("unknown flag: %s (use --help for usage)", args[i])
			}
		}
	}

	if oldKeyPath == "" {
		log.Fatalf("--old-key-path is required")
	}
	if newKeyPath == "" {
		log.Fatalf("--new-key-path is required")
	}
	if dbPath == "" {
		dbPath = "/var/lib/ah/ah.db"
	}

	// Load keys
	oldKey, err := loadKeyFile(oldKeyPath)
	if err != nil {
		log.Fatalf("failed to load old key: %v", err)
	}
	newKey, err := loadKeyFile(newKeyPath)
	if err != nil {
		log.Fatalf("failed to load new key: %v", err)
	}

	// Open database directly (not via db.Open which starts migrations and metering DB)
	sqlDB, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		log.Fatalf("failed to open database %s: %v", dbPath, err)
	}
	defer sqlDB.Close()
	if err := sqlDB.Ping(); err != nil {
		log.Fatalf("failed to ping database %s: %v", dbPath, err)
	}

	mode := "LIVE"
	if dryRun {
		mode = "DRY-RUN"
	}
	log.Printf("[%s] starting master key rotation on %s", mode, dbPath)

	result, err := crypto.RotateKeys(sqlDB, oldKey, newKey, dryRun)
	if err != nil {
		log.Fatalf("key rotation failed: %v", err)
	}

	log.Printf("[%s] rotation complete:", mode)
	log.Printf("  service env vars:      %d", result.ServiceEnvVars)
	log.Printf("  database passwords:    %d", result.DatabasePasswords)
	log.Printf("  database conn strings: %d", result.DatabaseConnStrs)
	log.Printf("  kanban admin tokens:   %d", result.KanbanTokens)
	log.Printf("  snapshot env vars:     %d", result.SnapshotEnvs)
	log.Printf("  total fields:          %d", result.Total())
	log.Printf("  errors:                %d", result.Errors)

	if dryRun {
		log.Printf("dry-run mode: no changes were committed. Run without --dry-run to apply.")
	} else {
		log.Printf("all encrypted data has been re-encrypted with the new key.")
		log.Printf("IMPORTANT: replace your master key file with the new key and restart the server.")
	}
}

// loadKeyFile reads and validates a master key file. The file must contain
// exactly 64 lowercase hex characters (optionally followed by a newline),
// encoding 32 raw bytes.
func loadKeyFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	keyHex := strings.TrimRight(string(data), "\n")
	if len(keyHex) != 64 {
		return nil, fmt.Errorf("key file %s contains %d characters (expected 64 hex characters / 32 bytes)", path, len(keyHex))
	}
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, fmt.Errorf("key file %s contains invalid hex: %w", path, err)
	}
	return key, nil
}

func printRotateKeyUsage() {
	fmt.Fprintf(os.Stderr, `Usage: ah rotate-key [flags]

Re-encrypts all encrypted data in the state database from an old master key
to a new master key. This is an OFFLINE operation — stop the server first.

The operation is atomic: either all fields are re-encrypted or none are.

Flags:
  --old-key-path PATH   Path to the current (old) master key file (required)
  --new-key-path PATH   Path to the new master key file (required)
  --db-path PATH        Path to the state database (default: /var/lib/ah/ah.db)
  --dry-run             Report what would be re-encrypted without making changes

Example:
  # Generate a new master key
  head -c 32 /dev/urandom | xxd -p -c 64 > /var/lib/ah/master-new.key
  chmod 600 /var/lib/ah/master-new.key

  # Stop the server
  systemctl stop paasd.service

  # Dry-run first
  ah rotate-key --old-key-path /var/lib/ah/master.key \
                --new-key-path /var/lib/ah/master-new.key \
                --dry-run

  # Rotate for real
  ah rotate-key --old-key-path /var/lib/ah/master.key \
                --new-key-path /var/lib/ah/master-new.key

  # Swap the key file
  mv /var/lib/ah/master.key /var/lib/ah/master-old.key
  mv /var/lib/ah/master-new.key /var/lib/ah/master.key

  # Restart the server
  systemctl start paasd.service
`)
}
