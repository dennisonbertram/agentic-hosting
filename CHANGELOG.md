# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

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
