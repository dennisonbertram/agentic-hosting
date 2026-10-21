# Stale claims vs the live code

Operator docs, the homepage, and the Claude skill drifted from `internal/`. This table is the audit from aligning them. Historical `CHANGELOG.md` entries are left as published history.

| Stale claim | Correct form | Source of truth |
|-------------|--------------|-----------------|
| Slash commands `/deploy`, `/status`, `/db`, `/logs` | `/ah-deploy`, `/ah-status`, `/ah-db`, `/ah-logs`, plus `/ah-register` and `/ah-snapshot` | `.claude/commands/ah-*.md` |
| `website/install.sh` downloads `deploy.md` / `status.md` / `db.md` / `logs.md` | Downloads `ah-*.md` and skill references (same as `install-skill.sh`) | `website/install.sh` |
| Build body `{"git_url","branch"}` (or `git_ref`) | `{"source_type":"git","source_url":"https://…","source_ref":"main"}` | `builds.StartBuildRequest` |
| `POST /v1/auth/bootstrap/validate` with `X-Bootstrap-Token` → 200/401 | `POST /v1/system/bootstrap-token/validate` with body `{"token":"…"}` → always 200 `{"valid":true\|false}` | `internal/api/bootstrap.go` |
| `POST /v1/tenant/reactivate` | `POST /v1/tenants/{tenantID}/reactivate` (bootstrap header; returns a new `api_key`) | `internal/api/server.go`, `handleTenantReactivate` |
| Disk block at 95% | Warn at **80%**, block new deploys/builds/DBs at **90%** | `diskcheck.CheckAll(..., 80, 90)` |
| Idempotency TTL 24 hours | **10 minutes**; scoped tenant + method + path + body hash. `POST /v1/environments/{id}/exec` is excluded | `internal/middleware/idempotency.go` |
| Idempotency on POST/PUT/DELETE (or “all mutating routes”) | **POST and PUT only**; DELETE and PATCH are skipped. No PUT routes exist. `POST /v1/auth/keys` excluded. Only 2xx stored; in-memory; max 50 keys/tenant | `idempotency.go:146`, `:157-162` |
| Disk ≥90% returns 500 (2026-10-19 correction) | Since #168, sync paths (build, DB/kanban create, restart, redeploy) return **503** `{"error":"insufficient disk space; try again later"}` with `Retry-After: 10`. Service create returns 201; async deploy sets `failed` + `last_error` | `apierr.Unavailable` in `services.go`, `builds.go`, `databases.go`, `kanbans.go` |
| Circuit reset then `/start` | Reset **removes** the container; `/start` returns 409. Use `/reset` then `/redeploy` | `services.go` `ResetCircuitBreaker`, `Start` |
| Poll `status == "circuit_open"` | `status` stays `crashed`; `circuit_open` is a boolean | `services.Service` JSON; reconciler `tripBreaker` |
| Default per-app resources 512 MB / 1 CPU | New tenants: **2048 MB / 2 CPUs** from `tenant_quotas`. 512/1 is the RunContainer fallback if the quota row cannot be loaded | `state_001_init.sql`; `docker/client.go`; `getResourceLimits` |
| Quickstart `nginx:alpine` | Crash-loops under ReadonlyRootfs. Use `nginxinc/nginx-unprivileged` on **8080** | `docker/client.go` tmpfs list; InstaBox investigation |
| Secrets use “tenant-derived keys” | AES-256-GCM with the **server master key**; no per-tenant key separation | `internal/crypto`; `docs/security/hkdf-key-separation-plan.md` |
| HTTP limiter ignores `tenant_quotas.api_rate_limit` | Auth reads the column per request; limiter uses it with burst 2× | `middleware/auth.go`, `ratelimit.go` |
| Reconciler interval 60s (AGENT_RUNBOOK) | Every **30 seconds** | `cmd/ah/main.go` |
| Unhealthy containers are stopped; auto-recovery restarts | Unhealthy containers are **restarted**. Auto-recovery leaves the service **stopped** (no container) | `reconciler.go` |
| Runtime logs unavailable (issue #11) | `GET /v1/services/{id}/logs?follow=true&tail=100` streams Docker stdout/stderr | `handleServiceLogs` |
| `PidsLimit: 100` | `PidsLimit: 256` | `internal/docker/client.go` |
| API keys hashed with bcrypt | HMAC-SHA256 of `keyID.secret` with the master key; bcrypt is for passwords | `internal/crypto` |
| Reconciler every 60s | Every **30 seconds** | `cmd/ah/main.go` |
| Create service accepts `memory_mb` / `cpu_count` | `CreateRequest` is `name`, `image`, `port`, `env` only | `internal/services.CreateRequest` |
| `image` optional on create (omit for git) | `image` is **required** (`ValidateImage`). Git flow uses placeholder `"pending"` (what `ahc` sends) | `handleServiceCreate`, `cmd/ahc/deploy.go` |
| `PATCH /v1/tenant` updates email | **`name` only** (2–128 chars) | `UpdateTenantRequest` |
| Activity `since` is RFC3339 | Unix timestamp (seconds). Metering `since`/`until` **are** RFC3339 | `handleActivityList` vs `handleUsageMetrics` |
| `GET`/`PUT /v1/environments/{id}/files` | Not routed. Sync is `POST /v1/environments/{id}/sync` with `{"git_url","git_ref"}` | `setupRoutes` |
| JSON errors include a `code` field | `{"error":"…"}` only | `httpx.WriteError` |
| Traefik routes via Docker labels | File provider YAML; containers set `traefik.enable=false` | `internal/services` Traefik writer |
| Binary / paths `paasd`, `/var/lib/paasd`, `/etc/default/paasd`, `journalctl -u paasd` | `ah`, `/var/lib/ah`, `/etc/default/ah`, `journalctl -u ah` | `deploy/ah.service` |
| Key create response field `key` | `api_key` | `CreateKeyResponse` |
| Env-var quota 100 per service | Schema default **50** (`max_env_vars_per_service`); **100 keys per POST** | migrations + `handleServiceEnvSet` |
| List pagination default 50 | `parsePagination` default **100**, max 200. Activity default **50** | `internal/api/server.go`, `dashboard.go` |
| Container name `paasd-{tenant}-{service}` | `ah-{tenantID}-{serviceID}` | `docker.containerName` |
| `scripts/deploy.sh` sends `git_url`/`branch` and `memory_mb`/`cpu_count` | Sends `source_type`/`source_url`/`source_ref` and `name`/`image`/`port` | `scripts/deploy.sh` |

Environment sync still uses `git_url` / `git_ref` — that is **not** a typo. Only the **build** API uses `source_*`.
