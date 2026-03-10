package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/paasd/paasd/internal/api"
	"github.com/paasd/paasd/internal/db"
)

func main() {
	port := flag.String("port", "8080", "HTTP port")
	listenAddr := flag.String("listen-addr", "", "Listen address (default: 127.0.0.1; use 0.0.0.0 to bind all interfaces)")
	dbPath := flag.String("db-path", "/var/lib/paasd/paasd.db", "Path to state SQLite database")
	masterKeyPath := flag.String("master-key-path", "/var/lib/paasd/master.key", "Path to master encryption key")
	devMode := flag.Bool("dev", false, "Development mode (disables HTTPS enforcement)")
	openRegistration := flag.Bool("open-registration", false, "Allow registration without bootstrap token (requires --dev)")
	flag.Parse()

	// Bootstrap token is always required unless --dev + --open-registration
	bootstrapToken := strings.TrimSpace(os.Getenv("PAASD_BOOTSTRAP_TOKEN"))
	if bootstrapToken == "" {
		if !*devMode {
			log.Fatalf("PAASD_BOOTSTRAP_TOKEN must be set (or use --dev --open-registration)")
		}
		if !*openRegistration {
			log.Fatalf("PAASD_BOOTSTRAP_TOKEN must be set. Use --open-registration with --dev to allow open registration.")
		}
		log.Printf("WARNING: open registration enabled — anyone can create tenants")
	} else if len(bootstrapToken) < 32 {
		log.Fatalf("PAASD_BOOTSTRAP_TOKEN must be at least 32 characters for brute-force resistance (got %d)", len(bootstrapToken))
	}

	if *openRegistration && !*devMode {
		log.Fatalf("--open-registration requires --dev")
	}

	// Prevent --dev from being used with non-loopback listen address
	if *devMode && *listenAddr != "" && *listenAddr != "127.0.0.1" && *listenAddr != "::1" {
		log.Printf("CRITICAL WARNING: --dev mode with non-loopback listen address (%s) disables HTTPS enforcement. This is unsafe for anything other than local development.", *listenAddr)
	}

	// Read master key
	masterKeyData, err := os.ReadFile(*masterKeyPath)
	if err != nil {
		log.Fatalf("failed to read master key: %v", err)
	}
	masterKey := []byte(strings.TrimSpace(string(masterKeyData)))
	if len(masterKey) < 32 {
		log.Fatalf("master key must be at least 32 bytes, got %d", len(masterKey))
	}

	// Open databases
	store, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	// Create server
	srv := api.NewServer(api.ServerConfig{
		Store:            store,
		MasterKey:        masterKey[:32],
		DevMode:          *devMode,
		BootstrapToken:   bootstrapToken,
		OpenRegistration: *openRegistration,
	})

	// Default to 127.0.0.1 in ALL modes (loopback only).
	// Must explicitly pass --listen-addr=0.0.0.0 to bind all interfaces.
	addr := "127.0.0.1:" + *port
	if *listenAddr != "" {
		addr = *listenAddr + ":" + *port
	}

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	// Graceful shutdown
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()
	log.Printf("paasd listening on %s", addr)
	if *devMode {
		log.Printf("WARNING: running in dev mode — HTTPS enforcement disabled")
	}

	<-done
	log.Println("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		log.Fatalf("shutdown error: %v", err)
	}
	log.Println("shutdown complete")
}
