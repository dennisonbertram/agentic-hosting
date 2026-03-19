package main

import (
	"context"
	"encoding/hex"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/dennisonbertram/agentic-hosting/internal/api"
	"github.com/dennisonbertram/agentic-hosting/internal/builder"
	"github.com/dennisonbertram/agentic-hosting/internal/builds"
	"github.com/dennisonbertram/agentic-hosting/internal/config"
	"github.com/dennisonbertram/agentic-hosting/internal/databases"
	"github.com/dennisonbertram/agentic-hosting/internal/db"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/gc"
	"github.com/dennisonbertram/agentic-hosting/internal/kanbans"
	"github.com/dennisonbertram/agentic-hosting/internal/reconciler"
	"github.com/dennisonbertram/agentic-hosting/internal/services"
)

func main() {
	cfg := config.FromEnv()

	// Check for subcommands before flag.Parse
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "backup":
			dbPath := cfg.DBPath
			if len(os.Args) > 2 {
				dbPath = os.Args[2]
			}
			runBackup(dbPath)
			return
		case "serve":
			// Remove "serve" from args so flag.Parse works
			os.Args = append(os.Args[:1], os.Args[2:]...)
		}
	}

	port := flag.String("port", "8080", "HTTP port")
	listenAddr := flag.String("listen-addr", "", "Listen address (default: 127.0.0.1; use 0.0.0.0 to bind all interfaces)")
	dbPath := flag.String("db-path", cfg.DBPath, "Path to state SQLite database")
	masterKeyPath := flag.String("master-key-path", cfg.MasterKeyPath, "Path to master encryption key")
	baseDomain := flag.String("base-domain", cfg.BaseDomain, "Base domain for public service URLs (e.g. example.com)")
	devMode := flag.Bool("dev", false, "Development mode (disables HTTPS enforcement)")
	openRegistration := flag.Bool("open-registration", false, "Allow registration without bootstrap token (requires --dev)")
	flag.Parse()

	// Validate --base-domain if provided.
	if *baseDomain != "" {
		if strings.Contains(*baseDomain, "://") {
			log.Fatalf("invalid --base-domain %q: must be a bare domain (e.g. example.com), not a URL", *baseDomain)
		}
		baseDomainValidRe := regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
		if !baseDomainValidRe.MatchString(*baseDomain) {
			log.Fatalf("invalid --base-domain %q: must be a valid DNS name (lowercase alphanumeric, dots, hyphens)", *baseDomain)
		}
	}

	// Bootstrap token is always required unless --dev + --open-registration
	bootstrapToken := strings.TrimSpace(os.Getenv("AH_BOOTSTRAP_TOKEN"))
	if bootstrapToken == "" {
		if !*devMode {
			log.Fatalf("AH_BOOTSTRAP_TOKEN must be set (or use --dev --open-registration)")
		}
		if !*openRegistration {
			log.Fatalf("AH_BOOTSTRAP_TOKEN must be set. Use --open-registration with --dev to allow open registration.")
		}
		log.Printf("WARNING: open registration enabled — anyone can create tenants")
	} else if len(bootstrapToken) < 32 {
		log.Fatalf("AH_BOOTSTRAP_TOKEN must be at least 32 characters for brute-force resistance (got %d)", len(bootstrapToken))
	}

	if *openRegistration && !*devMode {
		log.Fatalf("--open-registration requires --dev")
	}

	// In production (non-dev) mode, refuse to bind to non-loopback addresses.
	// The server must be behind a TLS-terminating reverse proxy on loopback.
	if !*devMode && *listenAddr != "" && *listenAddr != "127.0.0.1" && *listenAddr != "::1" {
		log.Fatalf("FATAL: non-loopback listen address (%s) is not allowed in production mode. Use --dev for development or bind to 127.0.0.1 behind a reverse proxy.", *listenAddr)
	}
	// Warn in dev mode about non-loopback
	if *devMode && *listenAddr != "" && *listenAddr != "127.0.0.1" && *listenAddr != "::1" {
		log.Printf("WARNING: dev mode with non-loopback listen address (%s) disables HTTPS enforcement.", *listenAddr)
	}

	// Read master key.
	// Expected format: exactly 64 lowercase hex characters (encoding 32 raw bytes),
	// optionally followed by a single newline. Generate with:
	//   head -c 32 /dev/urandom | xxd -p -c 64 > /var/lib/ah/master.key
	const keyErrMsg = "master key must be 64 hex characters (32 bytes). Generate with: head -c 32 /dev/urandom | xxd -p -c 64 > /var/lib/ah/master.key"
	masterKeyData, err := os.ReadFile(*masterKeyPath)
	if err != nil {
		log.Fatalf("failed to read master key from %s: %v\n%s", *masterKeyPath, err, keyErrMsg)
	}
	// Trim only a trailing newline so that any other unexpected whitespace or
	// extra characters are caught by the length and hex validation below.
	masterKeyHex := strings.TrimRight(string(masterKeyData), "\n")
	if len(masterKeyHex) != 64 {
		log.Fatalf("master key file %s contains %d characters (expected 64).\n%s", *masterKeyPath, len(masterKeyHex), keyErrMsg)
	}
	masterKey, err := hex.DecodeString(masterKeyHex)
	if err != nil {
		log.Fatalf("master key file %s contains invalid hex characters: %v\n%s", *masterKeyPath, err, keyErrMsg)
	}

	// Open databases
	store, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	// Create Docker client
	dockerClient, err := docker.NewClient()
	if err != nil {
		log.Fatalf("failed to create Docker client: %v", err)
	}
	defer dockerClient.Close()

	// Verify gVisor runtime is available (fail closed in production)
	if err := dockerClient.VerifyGVisorRuntime(context.Background()); err != nil {
		if !*devMode {
			log.Fatalf("FATAL: %v. gVisor is required for container isolation in production.", err)
		}
		log.Printf("WARNING: %v. Containers will fail to start without gVisor.", err)
	} else {
		log.Printf("gVisor (runsc) runtime verified")
	}

	// Create Nixpacks builder and build manager
	nixBuilder, err := builder.NewBuilder(cfg.BuildDir, cfg.NixpacksPath)
	if err != nil {
		log.Printf("WARNING: Nixpacks builder not available: %v", err)
	}

	var buildMgr *builds.Manager
	if nixBuilder != nil {
		// Create service manager early to get DeployImage function
		svcMgr := services.NewManager(store.StateDB, dockerClient, masterKey[:32], *baseDomain)
		buildMgr = builds.NewManager(store.StateDB, nixBuilder, svcMgr.DeployImage)
	}

	// Create database manager
	dbMgr := databases.NewManager(store.StateDB, dockerClient, masterKey[:32])

	// Create kanban manager
	kanbanMgr := kanbans.NewManager(store.StateDB, dockerClient, masterKey[:32])

	// Create server
	srv := api.NewServer(api.ServerConfig{
		Store:            store,
		MasterKey:        masterKey[:32],
		DevMode:          *devMode,
		BootstrapToken:   bootstrapToken,
		OpenRegistration: *openRegistration,
		Docker:           dockerClient,
		BuildManager:     buildMgr,
		DatabaseManager:  dbMgr,
		KanbanManager:    kanbanMgr,
		BaseDomain:       *baseDomain,
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
	log.Printf("ah listening on %s", addr)
	if *devMode {
		log.Printf("WARNING: running in dev mode — HTTPS enforcement disabled")
	} else {
		log.Printf("HTTPS enforcement is ON. The server must be behind a TLS-terminating proxy (e.g. Traefik) that connects via loopback (127.0.0.1). X-Forwarded-Proto is only trusted from loopback RemoteAddr.")
	}

	// Start reconciler (30s interval)
	reconcilerCtx, reconcilerCancel := context.WithCancel(context.Background())
	defer reconcilerCancel()
	rec := reconciler.New(store.StateDB, dockerClient, 30*time.Second)
	go rec.Run(reconcilerCtx)

	// Start garbage collector (5min interval)
	gcCtx, gcCancel := context.WithCancel(context.Background())
	defer gcCancel()
	garbageCollector := gc.New(store.StateDB, dockerClient, 5*time.Minute, cfg.BuildDir)
	go garbageCollector.Run(gcCtx)

	<-done
	log.Println("shutting down...")
	reconcilerCancel()
	gcCancel()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		log.Fatalf("shutdown error: %v", err)
	}
	log.Println("shutdown complete")
}
