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
	dbPath := flag.String("db-path", "/var/lib/paasd/paasd.db", "Path to state SQLite database")
	masterKeyPath := flag.String("master-key-path", "/var/lib/paasd/master.key", "Path to master encryption key")
	devMode := flag.Bool("dev", false, "Development mode (relaxes security requirements)")
	flag.Parse()

	// Check bootstrap token requirement
	bootstrapToken := strings.TrimSpace(os.Getenv("PAASD_BOOTSTRAP_TOKEN"))
	if bootstrapToken == "" && !*devMode {
		log.Fatalf("PAASD_BOOTSTRAP_TOKEN must be set (or use --dev for development mode)")
	}
	if bootstrapToken == "" {
		log.Printf("WARNING: running in dev mode without bootstrap token — registration is open")
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

	// Create server — pass bootstrap token via config, not env
	srv := api.NewServer(api.ServerConfig{
		Store:          store,
		MasterKey:      masterKey[:32],
		DevMode:        *devMode,
		BootstrapToken: bootstrapToken,
	})

	// Bind to 127.0.0.1 in production (only Traefik can reach it).
	// In dev mode, bind to all interfaces for direct testing.
	listenAddr := "127.0.0.1:" + *port
	if *devMode {
		listenAddr = ":" + *port
	}

	httpServer := &http.Server{
		Addr:              listenAddr,
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
	log.Printf("paasd listening on %s", listenAddr)
	if *devMode {
		log.Printf("WARNING: running in dev mode — HTTPS enforcement disabled, binding to all interfaces")
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
