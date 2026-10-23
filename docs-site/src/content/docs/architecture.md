---
title: Architecture
description: How the ah control plane, Docker, gVisor, Traefik, Nixpacks, and SQLite fit together.
section: Start
order: 3
---

# Architecture

`ah` is the control plane. Docker, gVisor, Traefik, Nixpacks, and SQLite are the runtime. Nothing else is required at process start besides a master key and (in production) a bootstrap token.

## Process map

```
[Traefik]  TLS + Host routing (file provider + Docker provider for Traefik itself)
    │
[ah HTTP, default 127.0.0.1:8080]
    ├── chi v5 router
    ├── auth middleware (HMAC-SHA256 API keys, 30s cache, 5000 entries)
    ├── per-tenant rate limit (100 rps, burst 200)
    ├── global rate limit (500 rps, burst 1000)
    ├── idempotency cache (10 min, 8KB stored body)
    └── handlers under /v1/

[Reconciler]     30s   DB desired state ↔ Docker
[GC]             5min  orphan containers, volumes, images, build dirs, snapshots
[Warm pool]      60s   pre-warmed environment containers (size 2, max 6)
```

`cmd/ah/main.go` starts the HTTP server, then the reconciler, then GC. The warm pool goroutine starts with the environment manager.

## The `ah` binary

Entry point: `cmd/ah/main.go`.

| Subcommand | What it does |
|------------|----------------|
| `ah` / `ah serve` | HTTP API + background loops |
| `ah backup [db-path]` | WAL-safe `VACUUM INTO` + gzip, keep last 10 |
| `ah rotate-key` | Re-encrypt secrets with a new AES-256-GCM master key |

Flags (serve):

| Flag | Default | Notes |
|------|---------|-------|
| `--port` | `8080` | Combined with listen address |
| `--listen-addr` | empty → `127.0.0.1` | Non-loopback is **fatal** unless `--dev` |
| `--db-path` | `/var/lib/ah/ah.db` | From `AH_DB_PATH` / `AH_DATA_DIR` |
| `--master-key-path` | `/var/lib/ah/master.key` | 64 hex chars (32 bytes) |
| `--base-domain` | `AH_BASE_DOMAIN` or empty | Public URLs become `https://{dns_label}.{base}` |
| `--dev` | false | Disables HTTPS enforcement; gVisor missing is a warning, not fatal |
| `--open-registration` | false | Requires `--dev`. Skips bootstrap token |
| `--cors-origins` | Pages dashboard hosts | Comma-separated Origin allowlist (`AH_CORS_ORIGINS`) |

Environment (see `internal/config`): `AH_DATA_DIR`, `AH_DB_PATH`, `AH_METER_DB_PATH`, `AH_MASTER_KEY_PATH`, `AH_BUILD_DIR`, `AH_NIXPACKS_PATH`, `AH_DOCKER_DATA_DIR`, `AH_BASE_DOMAIN`, `AH_CORS_ORIGINS`, `AH_TRAEFIK_CONFIG_DIR`, `AH_KANBAN_PORT_START` / `END` (7100–9100), `AH_SNAPSHOT_MAX_PER_SERVICE` (10), `AH_SNAPSHOT_MAX_AGE` (30 days), `AH_BOOTSTRAP_TOKEN` (comma-separated, each ≥32 chars).

`AH_BOOTSTRAP_TOKEN` is required unless `--dev --open-registration`.

## Docker + gVisor

`internal/docker/client.go` creates service containers with:

- `Runtime: "runsc"`
- `PidsLimit: 256`
- `MemorySwap = Memory` (no extra swap)
- Memory / CPU from `tenant_quotas` (new tenants: **2048 MB / 2 CPUs**). Fallback **512 MB / 1 CPU** only if the quota row cannot be loaded. Limits are per-tenant and still bounded by the host.
- `CapDrop: ALL`, `SecurityOpt: no-new-privileges`, `ReadonlyRootfs: true`
- tmpfs on `/tmp` (64m), `/var/run`, `/var/tmp`, `/run` (16m each), `noexec,nosuid`
- `RestartPolicy: unless-stopped`
- Network: per-tenant bridge `ah-tenant-{tenantID}`
- Label `traefik.enable=false` (routing is file-provider only)

Production **refuses to start** if `runsc` is not a configured Docker runtime (`VerifyGVisorRuntime`). `--dev` logs a warning and continues.

Default container port if the service omits one: **8000**.

## Traefik

- Network name expected by ops docs and code comments: `traefik-public`
- Dynamic config directory: `AH_TRAEFIK_CONFIG_DIR` (default `/etc/traefik/dynamic`)
- `ah` writes host routes for services and environment previews
- Without `--base-domain`, URLs are `http://{serviceID}.localhost` and localhost file routes are generated
- With `--base-domain example.com`, URLs are `https://{dns_label}.example.com`
- `dns_label` is a normalized service name, unique across the whole server (first-come, not per tenant), max 63 chars. The hostname is `{dns_label}.{base-domain}`; namespacing by tenant would need a two-level DNS wildcard the install path does not set up.
- Reserved labels: `api`, `admin`, `dashboard`, `traefik`, `www`, `mail`, `smtp`, `health`, `status`, `metrics`, `grafana`, `prometheus`, `registry`, `auth`, `login`, `oauth`

The API itself must sit behind Traefik on loopback. `requireHTTPS` only trusts `X-Forwarded-Proto: https` from a loopback `RemoteAddr`. Non-loopback requests have `X-Forwarded-*` and `X-Real-Ip` stripped.

## Nixpacks

`internal/builder` clones the repo (SSRF-constrained to the allowlisted hosts), runs `nixpacks plan`, builds an image, and pushes to `127.0.0.1:5000/ah/{serviceID}:{buildID}` (see `builds.ImageTag`). A successful build calls the service manager's deploy function.

Global in-flight build slots: **10** (`buildQueue` buffer in `internal/builds`). Quota column `max_builds_concurrent` defaults to 3 per tenant in the schema.

Build logs cap at **5 MB**, then a 500-line tail ring is kept.

## SQLite

Two files under `/var/lib/ah/` by default:

| File | Purpose |
|------|---------|
| `ah.db` | Tenants, keys, quotas, services, env, builds, databases, deployments, snapshots, kanbans, environments, warm pool, previews |
| `{stem}-metering.db` | `usage_events` + `usage_daily` (backup looks for `ah-metering.db` next to `ah.db`) |

Migrations live in `internal/db/migrations/` (`state_001` … `state_019`, `metering_001`). WAL mode.

`go-sqlite3` requires **CGO**. Cross-compiling with `CGO_ENABLED=0` produces a binary that will not run correctly.

## Request path

1. Body cap **1 MB** (`MaxBytesReader`)
2. Global throttle **200** in-flight (`chi/middleware.Throttle`)
3. Strip untrusted forwarded headers
4. HTTPS check (unless `--dev`)
5. Auth (Bearer `keyID.secret` → HMAC-SHA256 verify against stored hash)
6. Per-tenant + global token buckets
7. Idempotency (**POST and PUT** only; **not** DELETE/PATCH, **not** `POST /v1/auth/keys` or `POST /v1/environments/{id}/exec`; no PUT routes exist)
8. 30s timeout on short routes; logs, DB create, kanban create, environment create/sync skip that timeout

## Packages that matter

| Path | Role |
|------|------|
| `internal/api` | HTTP surface |
| `internal/services` | Service lifecycle + deploy |
| `internal/builder` / `internal/builds` | Nixpacks pipeline |
| `internal/databases` | Postgres 15 / Redis 7 |
| `internal/environments` | Instant workspaces + warm pool |
| `internal/reconciler` | 30s sync + circuit breaker |
| `internal/gc` | 5 min cleanup |
| `internal/diskcheck` | 80 / 90 watermarks |
| `internal/crypto` | AES-GCM, HMAC API keys, bcrypt (passwords) |
| `internal/middleware` | Auth, rate limit, idempotency |

## What is not in the runtime

- No Prometheus exporter
- No multi-node consensus
- No mTLS between tenant services (they cannot reach each other on the tenant bridge anyway: `ICC=false`)
