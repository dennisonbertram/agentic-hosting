# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Fixed
- GC: tenant networks younger than 15 minutes or with stopped containers are kept; unused built/snapshot images (over 1h old, unreferenced) and anonymous volumes are removed; container ownership is checked by ID across all tables. PullImage now returns pull-stream errors; RemoveContainer removes anonymous volumes.
- `ah backup`: retention now keeps the newest 10 state and 10 metering backups separately (state backups were being deleted first), the source DB is opened truly read-only, and the metering path reuses `db.MeteringPath`.
- Build-triggered deploys (`DeployImage`) now take the per-service lock, refuse when the circuit breaker is open, and fall back to name-based cleanup of the old container. Idempotency middleware serializes concurrent same-key requests (waiters replay the first response) and evicts the tenant's oldest entry at the per-tenant cap instead of skipping storage.

- Removed server-wide 30s WriteTimeout that cut off long routes (log follow, exec, sync); `--listen-addr ::1` now works (IPv6 host:port join).

- **Master key rotation no longer invalidates API keys** — API key hashes now use a dedicated HMAC key stored encrypted in new `server_secrets` table (seeded from the current master key so existing keys keep working); `ah rotate-key` re-encrypts it.
### Fixed (W3-B cancellation)
- Build cancel now cancels the build context, never overwrites a finished build, and a cancelled build no longer deploys; deployment cancel/status updates can no longer overwrite each other. Build log streaming no longer leaks subscribers or hangs, lines over 256KB no longer hang a build, and build logs are written in batches instead of one UPDATE per line.

### Fixed (W2-C databases/kanban)

- Redis create no longer fails (readiness check now AUTHs); NULL `container_id` no longer 500s Get/Delete/reconcile; failed kanban can be re-created; kanban setup failure is `failed`, not `ready`.
- Tenant reactivation now restarts databases and kanban from their stored volumes/credentials. Database containers are pulled first and get memory/pids limits, no-new-privileges and a reduced capability set.

- **`ahc` CLI review fixes** — uses the server's real statuses (db `ready`, service/build `failed`), fails deploys on failed builds, merges env and file config per field and saves it atomically (0600), honors long exec/create timeouts, propagates `env exec` exit codes, and verifies the installer download against `checksums.txt`.

### Added

- **Usage metering collector**: samples CPU and memory of running services every 60s into the metering DB. `memory_mb_avg` is now a real average (it was a sum). `AH_METER_DB_PATH` is honored when set.
- **`ahc` CLI client** — cross-platform command-line tool for managing agentic-hosting (install: `curl -fsSL https://agentic.hosting/install-cli.sh | bash`)
  - `ahc deploy` — deploy from Docker images or git repos with build log streaming
  - `ahc service` — full service lifecycle management with env var support
  - `ahc env` — instant environment management (create, exec, stop, delete)
  - `ahc db` — database provisioning and management
  - `ahc build` — build logs and history
  - `ahc status` — system health dashboard with exit codes for CI
  - `ahc snapshot` — service snapshots
  - `ahc register` / `ahc key` — tenant registration and API key management

### Changed

### Fixed

- **Reconciler crash loops**: crashes are now counted from Docker RestartCount across ticks so the circuit breaker actually trips; crashed services stay watched and return to running; unhealthy containers are restarted instead of stopped; status updates are compare-and-swap; not-found detection no longer matches any "404"; failed environment lease stops are retried.

- **`ahc register --save`** — config was written to `~/.ahc/config.json` with wrong field names (`api_key`/`server_url`), making it unreadable by `ahclient.LoadConfig`; now writes to `~/.ah/config.json` with the correct `url`/`key` fields matching the standard config format
- **Warm pool integration** — `Create()` now acquires pre-warmed containers from the pool instead of always cold-starting; environment creation is now sub-200ms when pool has capacity
- **Ghost pool rows** — pool refill now prunes DB rows pointing to non-existent containers, preventing stale entries from blocking pool capacity
- **Pool resource GC** — garbage collector now handles `ah-pool-` volumes and `ah.type=warm-pool` containers to prevent orphaned resources after service restarts
- **Volume wipe** — `WipeVolume` no longer writes until the shared disk is full: each file is overwritten with exactly its own size. The wipe container now uses pinned, pulled `busybox:1.36`, gVisor, no network, dropped capabilities, memory/pids limits, and is force-removed on timeout

### Removed

### Fixed (services)

- `/redeploy` now deploys services with no container (after a failed deploy, failed restart or circuit reset); deleting a service with snapshots works; snapshot restore skips the registry pull when the image is local; failed restarts no longer leave deployments stuck as `deploying`; rename keeps the `PORT` override in the Traefik route; Docker not-found detection uses typed errors instead of matching "404".

## [0.5.0] - 2026-03-30

### Added

- **Instant Environments** — first-class interactive workspaces for AI agents with full lifecycle management (create, start, stop, exec, delete)
- **Warm Pool** — pre-created containers for instant environment startup with configurable pool sizes per template
- **Language Templates** — built-in environment templates for default (Ubuntu), Node.js, Python, and Go workspaces
- **Workspace Sync** — git-based file synchronization to environment workspaces via HTTPS
- **Preview Routing** — HTTP routing to running environment containers with automatic Traefik configuration
- Deployment tracking — persistent `deployments` table records every deploy with status, timing, build ID, and source metadata (#79)
- `PATCH /v1/services/{id}` — rename services without redeploying (#83)
- Health endpoint cache bypass — `?fresh=true` query parameter skips the 30-second cache (#99)
- Docker storage info in `GET /v1/system/health/detailed` — disk usage and storage driver details (#105)
- Audit logging — AUDIT-level log entries when env vars are revealed, connection strings accessed, or kanban admin tokens retrieved (#101)
- Activity feed filtering — `resource_type`, `action`, `service_id`, `since`, and `offset` query parameters on `GET /v1/activity` (#89)
- Build log tail preservation — ring buffer retains the last N log lines when builds exceed max log size (#106)
- Tenant reactivation — `POST /v1/tenant/reactivate` restores a suspended tenant using the bootstrap token (#107)
- Snapshot retention policy — configurable `snapshot-max-per-service` and `snapshot-max-age` with automatic GC cleanup (#103)
- Async kanban provisioning — kanban board creation is now non-blocking with configurable port range (#102)
- Configurable kanban port range — `--kanban-port-start` and `--kanban-port-end` CLI flags (#109)
- Master key rotation — `ah rotate-key` CLI subcommand re-encrypts all secrets with a new AES-256-GCM key (#84)
- Bootstrap token rotation — `POST /v1/auth/bootstrap/validate` endpoint and multi-token support via comma-separated `AH_BOOTSTRAP_TOKEN` (#85)

### Fixed

- Localhost-mode Traefik routes now work alongside public-domain HTTPS redirects by moving HTTP→HTTPS redirects into per-service dynamic routers instead of a global `web` entrypoint redirect
- Health check script now defaults to the shipped API port (`8080`) and supports both GNU `timeout` and Homebrew `gtimeout` when available
- Health check disk monitoring now checks the ah state dir and Docker data dir instead of only `/`
- Port validation rejects `port <= 0` on service creation with a clear 400 error (#87)
- Build cancellation on tenant suspend — `CancelAllForTenant` stops queued and running builds (#88)
- Pagination limit cap on list endpoints prevents unbounded queries (#93)
- Database name validation enforces DNS-safe names via regex (#104)
- Quota error codes — `QuotaExceeded` now returns 409 Conflict instead of 403 Forbidden (#100)
- Fixed flaky `TestStartBuild_LogTruncationEndToEnd` — resolved SQLite write contention and build log flush race
- Fixed `StopAllForTenant` deadlock with serialized SQLite connections in tests

### Changed

- Localhost-mode runbook guidance now describes host-local Traefik routing instead of manual exposure steps
- Environment variable documentation — documented forbidden keys and semantics in API reference (#90, #111)
- Comprehensive rate limit test suite for per-tenant and global rate limiters (#80)

## [0.4.0] - 2026-03-20

### Added

- Custom domain support — `--base-domain` CLI flag makes service URLs `https://{name}.{base-domain}` with Traefik auto-TLS via Let's Encrypt (#14)
- Snapshot and template support for instant environment forking (#43)
- Per-tenant Vikunja kanban board provisioning (#46, #47)
- Supervisory dashboard — tenant control plane for services, databases, builds, and keys (#37)
- API key recovery via bootstrap token: `POST /v1/auth/recover` (#12)
- Redeploy endpoint: `POST /v1/services/{id}/redeploy` and deployment history: `GET /v1/services/{id}/deployments` (#6)
- Dev-only localhost Traefik routing when `baseDomain` is unset (#67)
- Cron-friendly health check script with webhook alerting (#21)
- Typed API errors, LRU auth cache, pagination improvements (#39)
- Claude Code skill restructured with progressive disclosure and `ah-` slash commands
- Changelog page on website (`/changelog/`)

### Fixed

- Protocol-level readiness checks for Postgres and Redis — no more false positives from silent TCP listeners (#51)
- Support SHA refs in git builds via two-phase clone+checkout (#56)
- Preflight-check build existence before streaming logs (#53)
- Circuit breaker backoff now escalates with `circuit_open_count` (#54)
- Databases and kanbans stopped when tenant is suspended or deleted (#55)
- Volume data wiped before removal on database delete (#9)
- Restart now recreates container so env var changes take effect
- Renamed `/agentic-paasd` to `/agentic-hosting` across docs and server (#19)

### Security

- HKDF key-separation scheme documented with 5 purpose-specific subkeys (#8)
- Tenant-to-Traefik reachability analysis with iptables mitigation (#50)
- Build egress allowlist architecture decision — Squid proxy approach (#3)
- Firecracker integration plan from gVisor (#1)
- Horizontal scaling gap analysis — 24 single-host assumptions identified (#2)
- Daemonless build prototype — Kaniko recommended (#7)
- Dev-environments MVP specification (#41)

## [0.3.0] - 2026-03-10

### Added

- Self-healer: liveness probes via Docker HEALTHCHECK (wget) on all service containers
- Self-healer: reconciler auto-detects unhealthy containers and stops them (Docker's RestartPolicy handles restart)
- Self-healer: auto circuit breaker recovery with exponential backoff (30m → 1h → 4h)
- DB migration state_009: `circuit_retry_at`, `circuit_open_count` columns
- `ContainerInfo.HealthStatus` field (nil-safe)
- `circuitRetryBackoff()` helper

### Changed

- Reconciler interval: 60s → 30s (matches documented behavior)
- `circuit_open` UPDATE now sets `circuit_retry_at` and increments `circuit_open_count`

## [0.2.0] - 2026-02-15

### Added

- Claude Code skill for AI agent automation of common operations
- `/security-review` slash command (4-pass audit: attacker, UX, equivalence, correctness)
- One-line curl installer (`curl agentic.hosting/install.sh | sh`)
- AI agent runbook (`docs/AI-AGENT-RUNBOOK.md`)
- Bash automation scripts: `register.sh`, `deploy.sh`, `status.sh`, `logs.sh`, `db-provision.sh`
- Traefik static config with Let's Encrypt support for agentic.hosting
- Website source with landing page

### Changed

- Binary renamed: `paasd` → `ah` throughout codebase
- Project renamed: `agentic-paasd` → `agentic-hosting`
- All env vars renamed: `PAASD_*` → `AH_*`
- Service renamed: `paasd.service` → `ah.service`
- Data dir renamed: `/var/lib/paasd` → `/var/lib/ah`

## [0.1.0] - 2026-01-20

### Added

- Multi-tenant HTTP API (chi v5 router)
- Tenant registration with bootstrap token (HMAC-compare, timing-safe)
- API key management: HMAC-SHA256 hashed, max 20/tenant, expiration support
- Service CRUD and lifecycle (start/stop/restart/reset)
- Container deployment via Docker Engine API with gVisor (runsc) runtime
- Per-tenant Docker bridge networks (internal, ICC=false, no internet egress)
- AES-256-GCM encrypted environment variables per service
- Nixpacks build pipeline: git URL → image → running container
- Build log streaming with `follow=true` (chunked HTTP)
- Managed Postgres 15 and Redis 7 provisioning
- Per-database Docker volumes with `ah-db-*` naming
- Encrypted database passwords and connection strings
- State reconciler: 30s loop syncing DB state to Docker reality
- Circuit breaker: 5 crashes / 10min window opens circuit
- Crash window tracking (state_008)
- Garbage collector: orphaned containers, volumes, images, build dirs (5min interval)
- Disk watermarks: warn at 80%, block at 95%
- WAL-safe SQLite backup with VACUUM INTO and gzip compression
- Auth middleware with 30s cache (5000 entries) and last-used sampling
- Per-tenant rate limiting: 100 rps / 200 burst
- Global rate limiting: 500 rps / 1000 burst
- Idempotency cache: SHA-256 request body hash, 8KB cap, 10min TTL, 50/tenant
- HTTPS enforcement (production), loopback-only proxy trust
- systemd unit with security hardening (ProtectSystem, PrivateTmp, NoNewPrivileges, etc.)
- Metering DB: `usage_events` and `usage_daily` tables
- Health endpoints: `GET /v1/system/health` (public) and `GET /v1/system/health/detailed` (authed, cached)

### Security

- gVisor (runsc) syscall interception for all service containers
- ReadonlyRootfs + tmpfs for container writable paths
- CAP DROP ALL, NO_NEW_PRIVILEGES on all containers
- PidsLimit=256, MemorySwap=Memory (no swap allowed)
- bcrypt password hashing
- Email enumeration prevention on tenant registration
- Forwarded header stripping from non-loopback requests
