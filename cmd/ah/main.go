package main

import (
	"context"
	"encoding/hex"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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
	"github.com/dennisonbertram/agentic-hosting/internal/deployments"
	"github.com/dennisonbertram/agentic-hosting/internal/diskcheck"
	"github.com/dennisonbertram/agentic-hosting/internal/docker"
	"github.com/dennisonbertram/agentic-hosting/internal/environments"
	"github.com/dennisonbertram/agentic-hosting/internal/gc"
	"github.com/dennisonbertram/agentic-hosting/internal/kanbans"
	"github.com/dennisonbertram/agentic-hosting/internal/metering"
	"github.com/dennisonbertram/agentic-hosting/internal/reconciler"
	"github.com/dennisonbertram/agentic-hosting/internal/services"
	"github.com/dennisonbertram/agentic-hosting/internal/snapshots"
)

func main() {
	// Fail fast on invalid settings: silently defaulting e.g. a retention
	// value would let snapshot GC delete data the operator meant to keep.
	cfg, cfgErr := config.LoadFromEnv()
	if cfgErr != nil {
		log.Fatalf("FATAL: invalid configuration: %v", cfgErr)
	}

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
		case "rotate-key":
			runRotateKey(os.Args[2:], cfg.DBPath)
			return
		case "build-broker":
			runBuildBroker()
			return
		case "build-worker":
			runBuildWorker(os.Args[2:], cfg)
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
	corsOrigins := flag.String("cors-origins", strings.Join(cfg.CORSOrigins, ","), "Comma-separated CORS Origin allowlist (\"none\" disables CORS)")
	devMode := flag.Bool("dev", false, "Development mode (disables HTTPS enforcement)")
	openRegistration := flag.Bool("open-registration", false, "Allow registration without bootstrap token (requires --dev)")
	maxSandboxesTotal := flag.Int("max-sandboxes-total", cfg.MaxSandboxesTotal, "Host-wide cap on disposable sandboxes across all tenants (0 = no host cap; env AH_MAX_SANDBOXES_TOTAL)")
	sandboxEgressNetwork := flag.String("sandbox-egress-network", cfg.SandboxEgressNetwork, "Bridge network for sandboxes with egress \"internet\" (empty = egress disabled; env AH_SANDBOX_EGRESS_NETWORK). Requires the host firewall rules from docs/ops/sandbox-egress-firewall.sh")
	flag.Parse()
	if *maxSandboxesTotal < 0 {
		log.Fatalf("--max-sandboxes-total must be >= 0, got %d", *maxSandboxesTotal)
	}

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

	// Bootstrap token is always required unless --dev + --open-registration.
	// Supports comma-separated list for graceful rotation:
	//   AH_BOOTSTRAP_TOKEN=new_token,old_token
	// All listed tokens are valid for registration and recovery.
	bootstrapTokenRaw := strings.TrimSpace(os.Getenv("AH_BOOTSTRAP_TOKEN"))
	var bootstrapTokens []string
	if bootstrapTokenRaw == "" {
		if !*devMode {
			log.Fatalf("AH_BOOTSTRAP_TOKEN must be set (or use --dev --open-registration)")
		}
		if !*openRegistration {
			log.Fatalf("AH_BOOTSTRAP_TOKEN must be set. Use --open-registration with --dev to allow open registration.")
		}
		log.Printf("WARNING: open registration enabled — anyone can create tenants")
	} else {
		for _, tok := range strings.Split(bootstrapTokenRaw, ",") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			if len(tok) < 32 {
				log.Fatalf("each bootstrap token must be at least 32 characters for brute-force resistance (got %d characters)", len(tok))
			}
			bootstrapTokens = append(bootstrapTokens, tok)
		}
		if len(bootstrapTokens) == 0 {
			log.Fatalf("AH_BOOTSTRAP_TOKEN is set but contains no valid tokens")
		}
		if len(bootstrapTokens) > 1 {
			log.Printf("bootstrap token rotation: %d tokens configured (all are valid for registration)", len(bootstrapTokens))
		}
	}

	// Admin token: authorizes recover, reactivate and quota update. Comma-separated for rotation.
	var adminTokens []string
	for _, tok := range strings.Split(cfg.AdminToken, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if len(tok) < 32 {
			log.Fatalf("each AH_ADMIN_TOKEN must be at least 32 characters (got %d characters)", len(tok))
		}
		adminTokens = append(adminTokens, tok)
	}
	if len(adminTokens) == 0 && len(bootstrapTokens) > 0 {
		log.Printf("WARNING: AH_ADMIN_TOKEN is not set; bootstrap tokens are also accepted for recover, reactivate and quota update. This fallback is deprecated and will be removed; set AH_ADMIN_TOKEN.")
	}

	if *openRegistration && !*devMode {
		log.Fatalf("--open-registration requires --dev")
	}

	// Workload runtime: runsc (gVisor) unless a dev sandbox explicitly opts
	// into the runc fallback. Never weakened silently or outside --dev.
	runtimeWarnings, err := checkContainerRuntime(cfg.ContainerRuntime, *devMode)
	if err != nil {
		log.Fatalf("FATAL: %v", err)
	}
	for _, line := range runtimeWarnings {
		log.Print(line)
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

	// AH_METER_DB_PATH is honored only when set explicitly; otherwise the metering
	// DB stays at <db>-metering.db so existing data is not orphaned.
	// Open databases
	store, err := db.OpenWithMetering(*dbPath, os.Getenv("AH_METER_DB_PATH"))
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	// Create Docker client
	dockerClient, err := docker.NewClientWithRuntime(cfg.ContainerRuntime)
	if err != nil {
		log.Fatalf("failed to create Docker client: %v", err)
	}
	defer dockerClient.Close()

	// Without gVisor (a --dev runc fallback for the workloads or the
	// sandboxes), the API must not be reachable from a network.
	if err := checkListenIsolation(context.Background(), *listenAddr, cfg.ContainerRuntime, cfg.SandboxRuntime, dockerClient); err != nil {
		log.Fatalf("FATAL: %v", err)
	}

	// Verify gVisor runtime is available (fail closed in production)
	if cfg.ContainerRuntime == config.RuntimeRunsc {
		if err := dockerClient.VerifyGVisorRuntime(context.Background()); err != nil {
			if !*devMode {
				log.Fatalf("FATAL: %v. gVisor is required for container isolation in production.", err)
			}
			log.Printf("WARNING: %v. Containers will fail to start without gVisor (set AH_CONTAINER_RUNTIME=runc for a dev-only sandbox).", err)
		} else {
			log.Printf("gVisor (runsc) runtime verified")
		}
	}

	// Create Nixpacks builder and build manager
	nixBuilder, err := builder.NewBuilder(cfg.BuildDir, cfg.NixpacksPath)
	if err != nil {
		log.Printf("WARNING: Nixpacks builder not available: %v", err)
	}

	// Create deployment store for tracking deployment history
	deployStore := deployments.NewStore(store.StateDB)

	// One service manager shared by the build pipeline and the API so build
	// deploys and API deploy/restart/reset serialize on the same per-service
	// locks and share one deploy semaphore/queue.
	svcMgr := services.NewManager(store.StateDB, dockerClient, masterKey[:32], *baseDomain, cfg.TraefikConfigDir, deployStore)
	svcMgr.SetExternalTLSTerminated(cfg.ExternalTLSTerminated)

	var buildMgr *builds.Manager
	if nixBuilder != nil {
		buildMgr = builds.NewManager(store.StateDB, nixBuilder, svcMgr.DeployImage)
	}

	// Create database manager
	dbMgr := databases.NewManager(store.StateDB, dockerClient, masterKey[:32])

	// Create metering store
	meteringStore := metering.NewStore(store.MeteringDB)

	// Create environment manager with warm pool
	envMgr := environments.NewManager(store.StateDB, dockerClient, *baseDomain, cfg.TraefikConfigDir)
	envMgr.SetExternalTLSTerminated(cfg.ExternalTLSTerminated)
	envPool := environments.NewPoolManager(store.StateDB, dockerClient, environments.PoolConfig{
		Enabled:      cfg.WarmPoolEnabled,
		PoolSize:     2,
		MaxTotal:     6,
		RefillPeriod: 60 * time.Second,
	})
	envMgr.SetPool(envPool)
	go envPool.Run(context.Background())

	// Disposable sandboxes: sources, host cap and the TTL reaper. The
	// snapshot manager is stateless, so the one used for GC retention below
	// is shared.
	snapMgr := snapshots.NewManager(store.StateDB, dockerClient, masterKey[:32])
	envMgr.SetSandboxSources(snapshotSandboxSource{mgr: snapMgr}, serviceSandboxSource{mgr: svcMgr})
	envMgr.SetMaxSandboxesTotal(*maxSandboxesTotal)
	log.Printf("sandboxes: host cap %d (0 = none)", *maxSandboxesTotal)
	// Sandbox image pulls are bounded host-wide; the disk check (create, and
	// the reaper's disk watchdog) covers the data dir and the Docker root.
	envMgr.SetSandboxPullLimits(cfg.SandboxMaxConcurrentPulls, cfg.SandboxMaxImageMB)
	diskPaths := cfg.DiskCheckPaths()
	envMgr.SetSandboxDiskCheck(func() error { return diskcheck.CheckAll(diskPaths, 80, 90) })
	log.Printf("sandboxes: at most %d concurrent image pulls, images up to %d MB", cfg.SandboxMaxConcurrentPulls, cfg.SandboxMaxImageMB)
	// Phase 2 hardening, all opt-in: a missing sandbox runtime is fatal; an
	// egress network without verified firewall rules stays disabled (403).
	if err := configureSandboxHardening(context.Background(), cfg, strings.TrimSpace(*sandboxEgressNetwork), *devMode,
		dockerClient, envMgr, sandboxEgressOptions{}, log.Printf); err != nil {
		log.Fatalf("FATAL: %v", err)
	}
	envMgr.LogSandboxHardening()
	reaperCtx, reaperCancel := context.WithCancel(context.Background())
	defer reaperCancel()
	go envMgr.RunSandboxReaper(reaperCtx, 30*time.Second)

	// Usage metering collector (60s samples of running services)
	meterCtx, meterCancel := context.WithCancel(context.Background())
	defer meterCancel()
	go metering.NewCollector(store.StateDB, store.MeteringDB, dockerClient).Run(meterCtx)

	// Create kanban manager
	kanbanMgr := kanbans.NewManagerWithPortRange(store.StateDB, dockerClient, masterKey[:32], cfg.KanbanPortStart, cfg.KanbanPortEnd)
	log.Printf("kanban port range: %d-%d (%d ports)", cfg.KanbanPortStart, cfg.KanbanPortEnd, cfg.KanbanPortEnd-cfg.KanbanPortStart+1)
	// Only re-parse when --cors-origins was given: cfg.CORSOrigins may be a
	// deliberately empty allowlist (AH_CORS_ORIGINS=none), which joins to ""
	// and would otherwise re-parse as the defaults.
	corsAllow := cfg.CORSOrigins
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "cors-origins" {
			corsAllow = config.ParseCORSOrigins(*corsOrigins)
		}
	})
	if len(corsAllow) == 0 {
		log.Printf("CORS: disabled (empty allowlist)")
	} else {
		log.Printf("CORS origins: %s", strings.Join(corsAllow, ", "))
	}

	// Create server
	srv := api.NewServer(api.ServerConfig{
		Store:                 store,
		MasterKey:             masterKey[:32],
		DevMode:               *devMode,
		BootstrapTokens:       bootstrapTokens,
		AdminTokens:           adminTokens,
		OpenRegistration:      *openRegistration,
		Docker:                dockerClient,
		BuildManager:          buildMgr,
		ServiceManager:        svcMgr,
		DeploymentStore:       deployStore,
		DatabaseManager:       dbMgr,
		KanbanManager:         kanbanMgr,
		EnvironmentManager:    envMgr,
		MeteringStore:         meteringStore,
		BaseDomain:            *baseDomain,
		TraefikConfigDir:      cfg.TraefikConfigDir,
		DataDir:               filepath.Dir(*dbPath),
		ExternalTLSTerminated: cfg.ExternalTLSTerminated,
		CORSOrigins:           corsAllow,
		ContainerRuntime:      dockerClient.Runtime(),
	})

	// Default to 127.0.0.1 in ALL modes (loopback only).
	// Must explicitly pass --listen-addr=0.0.0.0 to bind all interfaces.
	addr := listenAddress(*listenAddr, *port)
	httpServer := newHTTPServer(addr, srv)

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
		if cfg.ContainerRuntime != config.RuntimeRunsc {
			log.Printf("WARNING: workloads run under %s, not gVisor — local dev sandbox only", cfg.ContainerRuntime)
		}
	} else {
		log.Printf("HTTPS enforcement is ON. The server must be behind a TLS-terminating proxy (e.g. Traefik) that connects via loopback (127.0.0.1). X-Forwarded-Proto is only trusted from loopback RemoteAddr.")
	}

	// Start reconciler (30s interval)
	reconcilerCtx, reconcilerCancel := context.WithCancel(context.Background())
	defer reconcilerCancel()
	rec := reconciler.New(store.StateDB, dockerClient, 30*time.Second, deployStore)
	go rec.Run(reconcilerCtx)

	// Start garbage collector (5min interval)
	gcCtx, gcCancel := context.WithCancel(context.Background())
	defer gcCancel()
	garbageCollector := gc.New(store.StateDB, dockerClient, 5*time.Minute, cfg.BuildDir)
	garbageCollector.SetDockerPrune(cfg.GCPruneDocker)
	if !cfg.GCPruneDocker {
		log.Printf("gc: daemon-wide dangling image and anonymous volume prune disabled (AH_GC_PRUNE_DOCKER=false)")
	}

	// Wire snapshot retention into GC
	garbageCollector.SetSnapshotCleaner(snapMgr, cfg.SnapshotMaxPerService, cfg.SnapshotMaxAge)

	go garbageCollector.Run(gcCtx)

	<-done
	log.Println("shutting down...")
	reconcilerCancel()
	gcCancel()
	meterCancel()
	reaperCancel()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		log.Fatalf("shutdown error: %v", err)
	}
	log.Println("shutdown complete")
}
