# Agentic Hosting — Build Log

**Repository**: https://github.com/dennisonbertram/agentic-hosting
**Production**: https://agentic.hosting
**Server**: Hetzner, 65.21.67.254 (12 cores / 62GB RAM / 436GB NVMe RAID1, Ubuntu 24.04)
**Stack**: Go 1.25 · SQLite · Docker + gVisor · Nixpacks · Traefik · Let's Encrypt

---

## What It Is

A production-grade, multi-tenant PaaS designed specifically for AI agent workloads. Tenants get:
- Container hosting with gVisor sandbox isolation
- Source-to-image builds via Nixpacks (git URL → running container)
- Managed Postgres and Redis databases
- Per-tenant network isolation (no container-to-container communication)
- Automatic crash recovery with circuit breaking
- REST API — no dashboard, intentionally agent-friendly

Single Go binary (`ah`) with zero runtime dependencies beyond Docker.

---

## Architecture Overview

```
[Traefik] → (TLS termination, routing by Host header)
    ↓
[ah HTTP server, port 9090]
    ├── API handlers (chi v5 router)
    ├── Auth middleware (HMAC, cached 30s)
    ├── Rate limiting (per-tenant + global)
    └── Idempotency cache

[Reconciler goroutine, 30s interval]
    → Syncs DB state ↔ Docker container reality
    → Circuit breaker, liveness probes, auto-recovery

[GC goroutine, 5min interval]
    → Prunes orphaned containers, volumes, images, build dirs

[State DB: /var/lib/ah/ah.db]        (SQLite WAL, 9 migrations)
[Metering DB: /var/lib/ah/ah-metering.db]

[Docker Engine]
    ├── gVisor (runsc) runtime for service containers
    ├── Per-tenant bridge networks (internal, ICC=false)
    ├── Private registry (127.0.0.1:5000)
    └── Traefik container (ingress)
```

---

## Database Schema

### State DB (9 migrations)

**tenants** — one row per customer
**api_keys** — HMAC-SHA256 hashed, max 20 per tenant
**tenant_quotas** — max services, databases, memory, CPU, disk, rate limit
**services** — container lifecycle state, crash tracking, circuit breaker state
**service_env** — AES-256-GCM encrypted per-service environment variables
**builds** — Nixpacks build records with log storage
**databases** — Postgres/Redis provisioning state, encrypted credentials

**services columns across all migrations:**
```
id, tenant_id, name, status, image, source_type, source_ref, container_id,
created_at, updated_at,
port,                    -- state_002
last_error,              -- state_003
crash_count,             -- state_007
circuit_open,            -- state_007
last_crashed_at,         -- state_007
crash_window_start,      -- state_008
circuit_retry_at,        -- state_009
circuit_open_count       -- state_009
```

### Metering DB (1 migration)

**usage_events** — raw usage events (cpu_seconds, memory_mb_seconds, network bytes, disk)
**usage_daily** — pre-aggregated daily rollups per tenant/service

---

## API Reference

### Public (no auth)
| Method | Path | Description |
|--------|------|-------------|
| GET | `/v1/system/health` | Constant-time liveness check |
| POST | `/v1/tenants/register` | Create tenant (bootstrap token required) |

### Authenticated (Bearer `{keyID}.{secret}`)
| Method | Path | Description |
|--------|------|-------------|
| GET | `/v1/system/health/detailed` | Docker, gVisor, disk status (30s cached) |
| GET/PATCH/DELETE | `/v1/tenant` | Tenant read/update/suspend |
| POST/GET/DELETE | `/v1/auth/keys` | API key management |
| POST/GET | `/v1/services` | Create/list services |
| GET/DELETE | `/v1/services/{id}` | Read/delete service |
| POST | `/v1/services/{id}/{start,stop,restart,reset}` | Lifecycle control |
| GET/POST/DELETE | `/v1/services/{id}/env/{key}` | Encrypted env vars |
| POST/GET | `/v1/services/{id}/builds` | Trigger/list builds |
| GET | `/v1/services/{id}/builds/{bid}` | Build status |
| GET | `/v1/services/{id}/builds/{bid}/logs?follow=true` | Stream build logs |
| DELETE | `/v1/services/{id}/builds/{bid}` | Cancel build |
| POST/GET | `/v1/databases` | Provision/list databases |
| GET/DELETE | `/v1/databases/{id}` | Read/delete database |
| GET | `/v1/databases/{id}/connection-string` | Plaintext connection string |

---

## Security Model

| Layer | Mechanism |
|-------|-----------|
| Container isolation | gVisor (runsc) — syscall interception |
| Network isolation | Per-tenant Docker bridge, ICC=false, no internet egress |
| Secret storage | AES-256-GCM (env vars, DB passwords, connection strings) |
| Password hashing | bcrypt |
| API key verification | HMAC-SHA256, constant-time comparison |
| Container filesystem | ReadonlyRootfs + tmpfs for /tmp, /var/run, /var/tmp, /run |
| Capabilities | CAP DROP ALL, NO_NEW_PRIVILEGES |
| Resource limits | PidsLimit=256, MemorySwap=Memory (no swap), CPU quota |
| Rate limiting | 5 tenant registrations/IP/hour; 100 req/tenant/sec; 500 req/sec global |
| HTTPS | Let's Encrypt via Traefik, HTTP→HTTPS redirect |
| Proxy trust | Only loopback X-Forwarded-Proto trusted, headers stripped from non-loopback |
| Bootstrap | HMAC-compare for registration token (timing-safe) |
| Idempotency | SHA-256 request body hash, 8KB cap, 10min TTL |

---

## Key Components

### Reconciler (`internal/reconciler/`)
Runs every 30 seconds. Compares DB "what should be running" against Docker "what is actually running."

**Step 1 — crash detection**: For each service the DB says is `running`, inspect the container. If it's exited/dead, increment `crash_count`. If 5+ crashes in a 600-second window: open circuit breaker (`circuit_open=1`), set `circuit_retry_at` for auto-recovery, stop and remove the container.

**Step 1a — unhealthy detection** *(added 2026-03-10)*: If Docker marks a container `unhealthy` (3 consecutive failed health checks), stop it so Docker's `RestartPolicy: unless-stopped` triggers a fresh start. Counts as a crash for circuit breaker purposes.

**Step 1b — auto circuit recovery** *(added 2026-03-10)*: Queries for circuits past their `circuit_retry_at` timestamp. Resets circuit state and sets status to `stopped` — next reconciler tick triggers redeploy. Backoff: 30min → 1hr → 4hr (exponential, capped).

**Step 2 — stale deployments**: Mark services stuck `deploying` for >10 minutes as `failed`.

**Step 3 — missing containers**: Services with a container ID that Docker can't find → mark `stopped`.

**Step 4 — split-brain detection**: Containers with `ah.service` Docker labels that don't correspond to any DB service are logged for GC awareness.

### Health Probes (`internal/docker/`)
Every service container gets a Docker `HEALTHCHECK` on creation:
```
wget -qO- http://localhost:{port}/ > /dev/null 2>&1 || exit 1
```
Interval: 30s, Timeout: 5s, Retries: 3, StartPeriod: 60s (grace for slow starts).

### Build Pipeline (`internal/builder/`)
1. Clone git repo (SSRF DNS check, ref sanitization)
2. Run `nixpacks plan` → YAML build plan
3. Build Docker image from Nixpacks Dockerfile
4. Push to private registry (`127.0.0.1:5000/ah/{serviceID}:{buildID}`)
5. Deploy container via Docker Engine API

Global concurrency: 3 builds. Per-tenant: 1 concurrent build, 20 queued.

### GC (`internal/gc/`)
Runs every 5 minutes (delayed 2 minutes after startup). Removes:
- Orphaned service/database containers (not in DB, age >10min)
- Orphaned volumes (`ah-db-*` pattern, not referenced)
- Old build directories (>1 hour)
- Dangling images (no containers using them)

### Auth Cache (`internal/middleware/`)
Auth results cached for 30 seconds (5000 entry max) to reduce SQLite load while keeping revocation latency under 30s. Last-used timestamp sampled every 5 minutes (10000 key max).

---

## Build History (Phases)

The system was built in 5 phases with Ralph Loop review (Security, UX, Correctness reviewers) at each phase. 3 consecutive approvals required to ship each phase.

### Phase 1 — Core API & Auth
Initial repo setup, tenant registration, API key management, SQLite schema, HTTP router, HMAC auth, rate limiting.

### Phase 2 — Service Lifecycle
Container run/stop/restart/delete, per-tenant Docker networks, gVisor runtime integration, env var encryption, Traefik label generation, port management (32000–63000 range).

### Phase 3 — Build Pipeline
Nixpacks integration, git clone with SSRF protection, build log streaming (chunked HTTP, `follow=true`), private registry push, build concurrency limits, build cancellation.

### Phase 4 — Databases
Postgres 15 and Redis 7 provisioning, volume management, port uniqueness constraint, encrypted password/connection-string storage, `ah-db-*` volume naming for GC.

### Phase 5 — Resilience & Operations
Reconciler loop, circuit breaker (crash_count, crash_window, circuit_open), GC daemon, disk watermarks (warn 80%, block 95%), systemd unit with hardening, WAL-safe SQLite backup (`VACUUM INTO`), gzip compression, idempotency cache.

**Multiple fix rounds within Phase 5:**
- Round 4: split-brain container repair, volume safety
- Round 5: circuit breaker enforcement, timeouts, security hardening
- Round 6: systemd Docker access, image GC, path safety
- Round 7: two-step circuit breaker updates (SQLite evaluation order), backup retention, path safety

### Post-MVP Additions (2026-03-10)

**Self-Healer** — commit `064232a`:
- Reconciler interval: 60s → 30s (matches documented behavior)
- Liveness probes: wget health check on container creation; reconciler restarts unhealthy containers
- Auto circuit recovery: `circuit_retry_at` + `circuit_open_count` columns (migration state_009); exponential backoff auto-retry without human intervention

**Tooling additions:**
- Claude Code skill for AI agent automation
- `/security-review` slash command (4-pass audit)
- One-line curl installer (`curl agentic.hosting/install.sh | sh`)
- Scripts: `register.sh`, `deploy.sh`, `status.sh`, `logs.sh`, `db-provision.sh`
- AI agent runbook (`docs/AI-AGENT-RUNBOOK.md`)

---

## Operational Notes

- **Binary**: `/usr/local/bin/ah` on server (older server path: `/agentic-paasd/bin/ah`)
- **Service**: `ah.service` (systemd)
- **Data**: `/var/lib/ah/` (state DB, metering DB, master key)
- **Master key**: hex-encoded, 32+ bytes, at `/var/lib/ah/master.key`
- **API port**: 9090 (Traefik proxies to it)
- **gVisor path**: `/usr/bin/runsc`
- **Registry**: `127.0.0.1:5000`
- **Traefik network**: `traefik-public` — all backend containers must join this network
- **Traefik ports**: 80 (HTTP→HTTPS redirect), 443 (HTTPS), 8090 (dashboard)
- **Backups**: gzip-compressed `VACUUM INTO` snapshots, atomic rename

**Deploy a new binary:**
```bash
ssh -i ~/.ssh/id_hetzner_claudeops root@65.21.67.254
cd /path/to/repo && git pull && CGO_ENABLED=1 GOOS=linux go build -o bin/ah ./cmd/ah
cp bin/ah /usr/local/bin/ah
systemctl restart ah.service
journalctl -u ah.service -f
```

---

## Known Gaps / Deferred Work

- **Zero test coverage** — no `*_test.go` files exist anywhere; no CI. Highest priority: reconciler unit tests with mock Docker client.
- **No metrics export** — metering tables exist but no Prometheus/Grafana integration
- **No billing** — metering data is collected but not acted on
- **No admin dashboard** — all operations via REST API
- **GitHub Issues #1–#10** — deferred work tracked on the repo

---

## File Map

```
cmd/ah/
  main.go           — server bootstrap, goroutine startup
  backup.go         — WAL-safe gzip SQLite backup

internal/
  api/
    server.go       — chi router, middleware stack
    health.go       — /v1/system/health[/detailed]
    auth.go         — API key CRUD
    tenants.go      — tenant register/update/delete
    services.go     — service CRUD + lifecycle
    builds.go       — build trigger, status, log streaming
    databases.go    — Postgres/Redis provisioning
  middleware/
    auth.go         — HMAC verification + auth cache
    ratelimit.go    — per-tenant + global token bucket
    idempotency.go  — SHA-256 request dedup
    helpers.go      — shared error helpers
  db/
    db.go           — SQLite init, migration runner
    migrations/     — state_001 through state_009, metering_001
  services/
    services.go     — service manager
    deploy_image.go — container deployment workflow
  docker/
    client.go       — Docker Engine API wrapper
  builder/
    builder.go      — Nixpacks build executor
  builds/
    builds.go       — build state management
  databases/
    databases.go    — database provisioning
  reconciler/
    reconciler.go   — 30s state sync loop
  gc/
    gc.go           — 5min garbage collector
  diskcheck/
    diskcheck.go    — disk watermark checks
  crypto/
    crypto.go       — AES-GCM, bcrypt, HMAC, key generation
  httpx/
    httpx.go        — JSON response helpers

deploy/
  ah.service              — systemd unit
  traefik/traefik.yml     — Traefik static config (Let's Encrypt)

scripts/
  register.sh, deploy.sh, status.sh, logs.sh, db-provision.sh

website/
  index.html        — landing page (Tailwind CSS)

docs/
  implementation/
    build-log.md              — this file
    self-healer-2026-03-10.md — self-healer session notes
```
