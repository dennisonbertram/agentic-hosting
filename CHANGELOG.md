# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Fixed

- **Deploys are serialized across the API and build pipeline.** The build pipeline and API used separate service managers, so a build-triggered deploy could overlap a restart or circuit reset of the same service. They now share one.
- **Suspended tenants stay stopped.** In-flight deploys, restarts, database and kanban provisioning re-check the tenant before starting containers and before marking them running/ready, and remove anything they started if the tenant was suspended meanwhile.
- **Snapshot images in use are kept.** Snapshot delete and retention no longer untag an image that a restored service still runs from.
- **Quotas `max_builds_concurrent` and `max_env_vars_per_service` are enforced** (they were stored but ignored).
- Container-less restart runs its fallback deploy on a detached 10-minute context instead of the 30s request context; failed container creates are rolled back on a fresh context; a restart interrupted by an ah crash no longer leaves the service `restarting` forever; build status is written on a fresh context after the deploy.
- Idempotency: a request running longer than the replay TTL can no longer be executed a second time by a retry. Concurrent tenant reactivations and concurrent API key creates can no longer both succeed past their checks.
- `ah rotate-key` uses the configured database path and refuses a missing file (it used to "rotate" an empty new SQLite file).
- SQLite transactions use `BEGIN IMMEDIATE` (`_txlock=immediate`) so the busy timeout applies to read-then-write transactions in WAL mode.
- `ahc` escapes path segments (an argument can no longer change which endpoint is called), `ahc configure` no longer writes `AH_URL`/`AH_KEY` into the config file, and error bodies are read with a size cap.
- GC removes public images pulled only for sandboxes once unused for an hour.
- Kanban boards that completed setup and later failed are restarted in place from their volume instead of being garbage-collected.
- Metering: a failed sandbox query no longer drops CPU/network baselines (which re-billed every sandbox from zero). A kanban board suspended before its setup ran is marked `failed` instead of being revived as `ready` with no admin user.

- **Managed databases are reachable from tenant services (#164).** Each database gets `ah-dbnet-<tenant>-<db>` (`Internal=true`, `ICC=true`) with DNS alias `ah-db-<id>`. Connection strings use that alias and the container port (`5432` / `6379`). Deploy and restart attach services whose env contains the alias. Host-loopback publish is kept for health probes and never binds `0.0.0.0`. GC knows `ah-dbnet-*`. Existing encrypted URLs and matching `service_env` copies are rewritten on startup; running containers still need a restart.
- **Resource managers no longer leak containers or wedge rows on failure.** Database and kanban create cleanup runs on a detached context (a client disconnect no longer leaks the container and volume), a failed `container_id` write marks the row `failed`, and a concurrent database delete no longer leaves the per-database network behind. Loopback URL migration ignores failed rows and no longer matches `127.0.0.1:5432` inside `127.0.0.1:54321`. Suspension keeps a `failed` kanban `failed`. Environment sync passes the git URL/ref as arguments (quotes and spaces work, and a requested ref is honored on a fresh clone) and rejects a `git_ref` starting with `-`. Stopping an environment whose container is already gone succeeds. Snapshot name filters treat `_` and `%` literally.
- **`POST /v1/environments/{id}/lease` returns 409** (instead of a misleading 200) when the environment is stopped concurrently between the status check and the update.

### Added

- **Disposable sandboxes** (`/v1/sandboxes`, `ahc sandbox`). Short-lived gVisor containers created from a template, a public image, one of the tenant's snapshots, or a copy of one of the tenant's services. Endpoints: create, list, get, exec, logs, extend, delete. Every sandbox has a TTL (`ttl_seconds`, at most `max_sandbox_ttl_seconds`) and an absolute lifetime (`max_sandbox_lifetime_seconds`); a reaper deletes expired sandboxes, and extend and expiry cannot both win. Limits: `max_sandboxes` per tenant (default 5), `--max-sandboxes-total` / `AH_MAX_SANDBOXES_TOTAL` per host (default 20), and a per-tenant create rate limit (reported in `X-Sandbox-Create-RateLimit-*`). By default sandboxes have no network egress and cannot reach other containers. Env values from a source (`include_env`) are never returned by the API. `ahc sandbox run` creates a sandbox, runs one command and always deletes it. Usage is metered with `resource_kind=sandbox` (`?kind=sandbox` on metrics). Environment and sandbox names are unique per kind.
- **Opt-in sandbox internet egress** (off by default). Requires the operator flag `--sandbox-egress-network` / `AH_SANDBOX_EGRESS_NETWORK`, the tenant quota `sandbox_egress_allowed=1`, and firewall rules installed by `docs/ops/sandbox-egress-firewall.sh`, which ah checks (presence and order) at startup. If any of these is missing, `egress: "internet"` returns 403. Egress sandboxes are never restarted by Docker and are deleted at startup if egress is disabled. **Not yet verified on a production host**; see `docs/runbooks/sandboxes-hardening.md`.
- **Optional hard disk limits for sandboxes** (off by default): `AH_SANDBOX_RUNTIME` (a registered gVisor runtime such as `runsc-sbx` with an in-memory overlay) and `AH_SANDBOX_WORKSPACE_TMPFS` (`/workspace` as a tmpfs of `disk_mb`, bounded by `max_sandbox_disk_mb`). `setup-linux.sh --sandbox-hardening` registers the runtime. **Not yet verified on a production host.**
- `ahc recover --admin-token` (and `$AH_ADMIN_TOKEN`) for installs that set `AH_ADMIN_TOKEN`.
- `ahc` resolvers accept a unique ID prefix of 4+ hex characters (as printed by `list`).
- `config.LoadFromEnv` rejects malformed integer/duration settings at startup (they used to fall back to defaults silently); durations accept a whole-day suffix such as `90d`.

- **One-command setup and a local sandbox.** `scripts/setup-linux.sh` wraps `bootstrap-stack.sh` for fresh Ubuntu 24.04 hosts, installs `ahc`, and does idempotent upgrade/repair on existing installs without touching keys or the bootstrap token. `scripts/setup-macos.sh` installs the Homebrew prerequisites and a Docker engine. `make sandbox` / `make sandbox-down` run a throwaway local instance with a demo tenant and the `examples/openrouter-agent` service. All three accept `--dry-run`, and `make test-scripts` (also run in CI) checks them. Runbooks are in `docs/runbooks/`.
- **`AH_CONTAINER_RUNTIME`** (`runsc` default, or `runc`). `runc` is a dev-only fallback for macOS sandboxes: `ah` exits at startup if it is set without `--dev`, and logs a warning banner when it is set with `--dev`. Detailed health reports `container_runtime`.
- **`AH_WARM_POOL_ENABLED`** (default `true`) and **`AH_GC_PRUNE_DOCKER`** (default `true`). The sandbox sets both to `false` so it does not pull every environment template image, and so GC does not prune dangling images or anonymous volumes that belong to other work on a shared Docker daemon.
- **API CORS allowlist** so the Pages dashboard can call `https://api.agentic.hosting`. Defaults: `https://agentic.hosting`, `https://www.agentic.hosting`, `https://dennisonbertram.github.io` (`AH_CORS_ORIGINS` / `--cors-origins`). OPTIONS preflight is handled before auth. `Authorization`, `Content-Type`, and `Idempotency-Key` are allowed. `Vary: Origin` is set. `*` and credentialed wildcards are rejected.

### Changed

- **Backpressure returns 503 + `Retry-After: 10`** instead of 500: a full deploy queue on restart, circuit reset and image deploy (deploy itself keeps its 409), and low disk on services, builds, databases and kanbans. Database create returns 409 if a tenant suspension interrupts it.
- **Admin routes have their own rate-limit bucket.** Recover, reactivate and quota update use `adminLimiter` (same limits as registration, separate counters), so bootstrap-token holders can no longer exhaust the budget the operator needs. Bootstrap-token validation no longer charges the global registration counter.
- CORS can be disabled (`AH_CORS_ORIGINS=none`, or a list with no valid origins).
- Large internal simplification (shared key minting, rate-limit responses, query parsing, migration runner, reconciler split into smaller steps) with no API changes; test coverage raised across `api`, `middleware`, `httpx`, `crypto` and `cmd/ah`.

- **Public site on GitHub Pages.** `https://agentic.hosting/` is the `website/` homepage; Astro docs are at `https://agentic.hosting/docs/`. `www.agentic.hosting` stays on Hetzner and 301s to the GitHub repo (TLS kept). Docs links that used `dennisonbertram.github.io/agentic-hosting/` now point at `/docs/`. The dashboard no longer defaults its API base to the Pages origin.
- **Docs and agent skills match live API behavior.** Disk ≥90% on sync paths is 503 `{"error":"insufficient disk space; try again later"}` with `Retry-After: 10` (it was 500 before this release). Circuit reset removes the container, so the next step is `/redeploy` not `/start`. `circuit_open` is a boolean, not a `status` string. Idempotency is POST/PUT only. New-tenant resource defaults are 2048 MB / 2 CPUs. Quickstart image is `nginxinc/nginx-unprivileged` on 8080 (stock nginx crash-loops under ReadonlyRootfs). Secrets use the server master key, not per-tenant keys.

## [0.6.0] - 2026-10-15

### Added

- **`ahc` CLI client**: cross-platform command-line tool (install: `curl -fsSL https://agentic.hosting/install-cli.sh | bash`). Commands: `deploy` (Docker images or git repos, with build log streaming), `service`, `env` (create, exec, stop, delete), `db`, `build`, `status` (exit codes for CI), `snapshot`, and `register` / `key`.
- **`AH_ADMIN_TOKEN`**: an operator token (comma-separated for rotation) required for `POST /v1/auth/recover`, `POST /v1/tenants/{id}/reactivate` and `PATCH /v1/tenants/{id}/quotas`. Send it as `X-Admin-Token`, or in the old bootstrap-token slot.
- **`scripts/bootstrap-stack.sh`**: installer for a fresh Ubuntu host. It generates the master key, bootstrap token and a separate admin token. See `docs/ops/fresh-install.md`.
- **Mac mini setup**: `scripts/bootstrap-macmini.sh` and the first-run guide in `docs/ops/macmini.md`, plus the OpenRouter agent example in `examples/openrouter-agent/`.
- **Documentation site**: Astro docs in `docs-site/` covering the live `/v1/` API and operations, with a GitHub Pages workflow at `.github/workflows/docs.yml`. InstaBox compatibility findings are in `docs/investigations/`.
- **Agent onboarding**: `llms.txt` and `llms-full.txt` at the repo root (also served at `/llms.txt` on the docs site) and a standalone skill at `skills/agentic-hosting/SKILL.md`.
- **Usage metering collector**: samples CPU and memory of running services every 60s into the metering database. `AH_METER_DB_PATH` is honored when set.
- **MIT license** at the repository root (Copyright 2026 Dennison Bertram), with a license badge and License section in the README.
- **README demo GIF** of the local `ah --dev` loop: register a tenant, deploy from git, tail logs, and see the service running (`docs/assets/demo.gif`).

### Changed

- **Master key rotation no longer invalidates API keys.** API key hashes now use a dedicated HMAC key stored encrypted in the new `server_secrets` table (seeded from the current master key, so existing keys keep working). `ah rotate-key` re-encrypts it and applies state migrations first.
- Service, database and environment quotas are enforced atomically (the count is inside the INSERT), so concurrent creates can no longer exceed `max_services`, `max_databases` or `max_environments`. `tenant_quotas.api_rate_limit` is now enforced per tenant (burst 2x, cached 30s with the auth lookup).
- Long routes (log follow, build logs, DB/kanban/environment create, sync, exec) leave the global 200-request throttle and get a per-tenant cap of 4 concurrent requests (429 beyond). Write deadlines are 60s per request, renewed per write on streaming routes. The server-wide 30s `WriteTimeout` is removed, so log follow, exec and sync are no longer cut off.
- Idempotency middleware serializes concurrent requests with the same key (waiters replay the first response) and evicts the tenant's oldest entry at the per-tenant cap instead of skipping storage.
- Health `?fresh=true` is limited to one live probe per 10s, concurrent probes share one result, and the disk check uses the configured data directory.
- `ah backup` keeps the newest 10 state and 10 metering backups separately, opens the source database read-only, and accepts relative paths.
- Garbage collection keeps tenant networks younger than 15 minutes or with stopped containers, and removes unused built and snapshot images (over 1h old, unreferenced) and anonymous volumes. Container ownership is checked by ID across all tables.
- Databases are pulled before start and get memory and pids limits, `no-new-privileges` and a reduced capability set.
- `ah.service` can write to `/etc/traefik/dynamic`.
- Docs (README, homepage, Claude skill and commands, `website/install.sh`) now match the live API. The audit list is in `docs-site/CORRECTIONS.md`.

### Fixed

- `scripts/bootstrap-stack.sh` builds the website image from the repository root. The website Dockerfile copies `docs-site/` and `llms*.txt` from the root, so building with `website/` as the context failed. The website container now serves the docs site at `https://agentic.hosting/docs/`.
- **Database delete no longer has a 30s limit.** Postgres deletes returned 500 under gVisor because container stop and volume wipe exceeded the request timeout. Delete cleanup now runs detached from the request, with a 1 minute budget for container stop and remove, 5 minutes for the volume wipe and 1 minute for volume removal.
- **`POST /v1/auth/recover` honors `X-Bootstrap-Token`.** Previously only the body field was read, so a bootstrap token sent in the header got 401. All three token slots now go through the same check, and bootstrap tokens get 403 consistently when `AH_ADMIN_TOKEN` is set.
- Reconciler: crashes are counted from Docker `RestartCount` across ticks so the circuit breaker trips; crashed services stay watched and return to running; unhealthy containers are restarted instead of stopped; status updates are compare-and-swap; not-found detection uses typed errors instead of matching "404"; failed environment lease stops are retried.
- Service lifecycle: the circuit breaker opens only after the container is stopped and removed; `Restart` marks the row `restarting` before stopping, so the reconciler no longer counts phantom crashes; deploys and restarts never overwrite an open circuit or reuse the old restart baseline; `Delete` aborts if the container cannot be removed. Build-triggered deploys take the per-service lock, refuse when the circuit breaker is open, and fall back to name-based cleanup of the old container.
- `/redeploy` works for services with no container (after a failed deploy, failed restart or circuit reset). Deleting a service with snapshots works. Snapshot restore skips the registry pull when the image is local. Failed restarts no longer leave deployments stuck as `deploying`. Rename keeps the `PORT` override in the Traefik route.
- Failed `/restart` and `/redeploy` return 502 `deploy failed: <reason>` instead of 500.
- Builds: cancel cancels the build context, never overwrites a finished build, and a cancelled build no longer deploys. Cancelling a build that is already deploying returns 409. Deployment cancel works only for `pending` rows, and cancel and status updates can no longer overwrite each other. Build log streams no longer leak subscribers, hang, or repeat a line from the initial snapshot; lines over 256KB no longer hang a build; logs are written in batches instead of one UPDATE per line.
- Environments: claimed warm-pool containers are renamed to the tenant environment name and kept by GC; pool refill no longer blocks `Acquire`, prunes rows that point to missing containers, and GC removes `ah-pool-` volumes and `ah.type=warm-pool` containers; `Create()` acquires pre-warmed containers (creation is under 200ms when the pool has capacity); exec timeouts are enforced and set `timed_out`; tenant suspend stops environments and previews on a detached 30-second context and no longer marks an environment stopped when the Docker stop failed; `Create` re-checks that the tenant is active; failed warm-pool adoption cleans up on a detached context; workspace sync is killed in-container after its timeout; previews are capped at 5 per environment.
- Databases and kanban: Redis create no longer fails (the readiness check now authenticates); a NULL `container_id` no longer causes 500 on Get, Delete or reconcile; `RunDatabase` falls back to a cached image when the pull fails; failed (re)starts remove the new container (volume kept); suspension also removes database containers of failed rows; a failed kanban can be re-created (its volume is deleted only if setup never completed), and a kanban setup failure is `failed`, not `ready`; tenant reactivation restarts databases and kanban from their stored volumes and credentials, and skips tenants that are no longer active.
- Volume wipe: `WipeVolume` overwrites each file with exactly its own size instead of writing until the disk is full. It uses pinned, pulled `busybox:1.36` under gVisor with no network, dropped capabilities and memory/pids limits, and is force-removed on timeout.
- Image pulls now return pull-stream errors, and `RemoveContainer` removes anonymous volumes.
- `ahc`: uses the server's real statuses (database `ready`, service and build `failed`), fails deploys on failed builds, merges environment and file config per field and saves it atomically (0600), honors long exec and create timeouts, propagates `env exec` exit codes, and verifies the installer download against `checksums.txt`. `ahc register --save` writes `~/.ah/config.json` with the `url` and `key` fields that `ahclient.LoadConfig` reads (it wrote `~/.ahc/config.json` with `api_key` and `server_url`).
- `ah rotate-key --dry-run` no longer applies migrations, and the rotation total counts the API key hash key. `install-cli.sh` no longer errors on its temp-dir trap under `set -u`. `--listen-addr ::1` works (IPv6 host:port join).
- Metering: `memory_mb_avg` is a real average (it was a sum); CPU baselines are keyed by container and advance only after a successful write; stats calls are bounded to 10s; a missing container is logged once. Idempotency cleanup keeps in-flight entries. Short routes share one throttle pool of 200, and streaming routes clear a stale write deadline. The rate limiter's get-or-create is atomic (fixes a data race).

### Security

- With `AH_ADMIN_TOKEN` set, bootstrap tokens get 403 on recover, reactivate and quota update, and keep working for tenant registration. If it is unset, bootstrap tokens are still accepted for those routes, with a startup warning (deprecated, to be removed).
- Requests with an invalid token no longer consume the global 20/hour budget, the startup log no longer prints rejected-token characters, and a key revoked during an auth DB read can no longer be re-cached as active.
- `ValidateImage` takes the tenant ID: `localhost` registries are rejected, and local-registry refs must be the caller's own `ah/<tenant>-` build or `snapshots/<tenant>:` image.
- Service names starting with `env-` are reserved for environment preview hosts.

### Removed

- `scripts/deploy.sh`, `scripts/status.sh`, `scripts/logs.sh` and `scripts/db-provision.sh`. Use `ahc`.

### Upgrade notes

- Two new state migrations run on first start: `state_020_server_secrets` and `state_020_restart_count`. Back up first with `ah backup`.
- Set `AH_ADMIN_TOKEN` (at least 32 characters per token; a shorter token stops startup) in `/etc/default/ah`. Without it the service starts but accepts bootstrap tokens for recover, reactivate and quota update and logs a deprecation warning.
- Service names starting with `env-` and images from `localhost/` registries are rejected.
- Deleting a service now also deletes its snapshots.
- Cancelling a build returns 409 once it is deploying, and deployment cancel works only for pending deployments.
- Failed deploys (`/restart`, `/redeploy`) return 502 instead of 500.
- `scripts/deploy.sh`, `status.sh`, `logs.sh` and `db-provision.sh` are gone; switch to `ahc deploy`, `ahc status`, `ahc build logs` and `ahc db provision`.

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
