# Full API Reference

Base URL: `$AH_URL` (e.g. `https://agentic.hosting`)
Auth header: `Authorization: Bearer $AH_KEY` (all endpoints except health)

---

## System

```bash
# Public health check
GET /v1/system/health
# → {"status":"ok"}

# Detailed health (auth required)
GET /v1/system/health/detailed
# → {"status":"ok","docker":{"available":true,"version":"29.x","storage_info":{"driver":"overlay2","data_space_used":"12.3 GB","data_space_total":"435 GB"}},"gvisor":{"available":true},"disk":{"total_gb":435,"free_gb":397,"used_percent":8.7}}
# Add ?fresh=true to bypass the 30-second cache:
GET /v1/system/health/detailed?fresh=true
```

---

## Tenants

```bash
# Register (requires X-Bootstrap-Token header)
POST /v1/tenants/register
Body: {"name":"string","email":"string"}
# → {"tenant_id":"hex32","api_key":"keyid.secret"}
# api_key shown exactly once — save it immediately

# Get current tenant
GET /v1/tenant
# → {"id":"...","name":"...","email":"...","status":"active","created_at":unix}

# Get usage stats
GET /v1/tenant/usage
# → {"services":{"count":2,"limit":20},"databases":{"count":1,"limit":3},...}

# Update tenant name (email is not updatable here)
PATCH /v1/tenant
Body: {"name":"new-name"}

# Reactivate a suspended tenant (requires admin token + tenant ID in the path)
POST /v1/tenants/:tenantID/reactivate
Headers: X-Admin-Token: <admin-token>
# → {"tenant_id":"...","api_key":"keyid.secret","status":"active"}
# Already-active tenants return 409

# Delete tenant (irreversible — removes ALL resources)
DELETE /v1/tenant
```

**Note:** Quota exceeded errors return `409 Conflict` (not `403`).

---

## API Keys

```bash
# Create a named key
POST /v1/auth/keys
Body: {"name":"agent-name","expires_in":2592000}  # expires_in in seconds, omit for no expiry
# → {"id":"...","api_key":"keyid.secret","name":"...","prefix":"...","expires_at":unix}
# api_key shown exactly once (field name is api_key, not key)

# List keys (no plaintext values)
GET /v1/auth/keys
# → [{"id":"...","name":"...","created_at":unix,"expires_at":unix|null}]

# Revoke a key
DELETE /v1/auth/keys/:keyID

# Recover access (requires admin token — use when all keys are lost)
POST /v1/auth/recover
Headers: X-Admin-Token: <admin-token>, Content-Type: application/json
Body: {"email":"tenant@example.com"}
# → {"api_key":"newkeyid.newsecret"}
# Rate limited: same as tenant registration (5/hour per IP, 20/hour global)
# Email must match the tenant's registered email

# Validate a bootstrap token (does not register — just checks validity)
POST /v1/system/bootstrap-token/validate
Body: {"token":"<bootstrap-token>"}
# → 200 {"valid":true} or 200 {"valid":false} — never 401 for a bad token
# Rate limited like registration (5/IP/hour, 20/hour global)
```

**Multi-token support:** `AH_BOOTSTRAP_TOKEN` accepts comma-separated values (e.g. `token1,token2`). Any token in the list is accepted for registration, recovery, and validation.

---

## Services

```bash
# Create service
POST /v1/services
Body: {
  "name": "string",           # required, max 128
  "image": "nginxinc/nginx-unprivileged",  # required (Docker Hub or 127.0.0.1:5000 / localhost:5000)
  "port": 8080,               # 0 / omitted → 8000; must be 1–65535 if set
  # Stock nginx:alpine crash-loops under ReadonlyRootfs (needs /var/cache/nginx).
  "env": {"KEY": "value"}     # optional inline env vars
}
# There is no memory_mb / cpu_count on CreateRequest.
# For git builds, send image "pending" (what ahc uses); create always starts
# an async Deploy (the placeholder pull fails), then POST .../builds replaces it.
# → {"id":"hex32","name":"...","status":"deploying","url":"http://<id>.localhost",...}
# Port must be > 0; returns 400 if invalid

# Create service from snapshot (instant fork — copies image, env, resource limits)
POST /v1/services?from_snapshot=:snapshotID
Body: {"name":"forked-service"}
# → same response as create service

# List services (paginated)
GET /v1/services?limit=50&offset=0
# → [{"id":"...","name":"...","status":"running","url":"...","image":"..."}]

# Get one service
GET /v1/services/:serviceID
# → {"id":"...","name":"...","status":"running","url":"...","last_error":"...","circuit_open":false}

# Rename a service (validates DNS-safe name)
PATCH /v1/services/:serviceID
Body: {"name":"new-name"}
# → returns updated service object

# Delete service
DELETE /v1/services/:serviceID

# Start / Stop / Restart
POST /v1/services/:serviceID/start
POST /v1/services/:serviceID/stop
POST /v1/services/:serviceID/restart

# Reset circuit breaker
POST /v1/services/:serviceID/reset

# Env vars: list (optionally revealed)
GET /v1/services/:serviceID/env?reveal=true
# → {"KEY":"value",...}  (values masked by default, plaintext with reveal=true)

# Env vars: set/update (additive merge — does NOT replace all vars)
# This endpoint uses upsert semantics: keys in the request body are added or
# updated, but existing keys NOT mentioned in the request body are left
# untouched. To remove a key, use DELETE. There is no PATCH endpoint —
# POST is the only write method and it always merges.
#
# Example: if the service already has {"A":"1","B":"2"} and you POST {"B":"99","C":"3"},
# the result is {"A":"1","B":"99","C":"3"} — A is preserved, B is updated, C is added.
POST /v1/services/:serviceID/env
Body: {"KEY":"value","OTHER":"value2"}
# → {"status":"updated","note":"restart will recreate the container with the new env vars"}
# Max 100 keys per request.

# Env vars: delete one key
DELETE /v1/services/:serviceID/env/:key

# Redeploy (rebuild from same source_url / source_ref as last build)
POST /v1/services/:serviceID/redeploy
# → {"id":"...","status":"pending","created_at":unix}
# Returns the new build object

# Deployment history
GET /v1/services/:serviceID/deployments
# → [{"id":"...","build_id":"...","source":"image|build","image":"...","status":"running","started_at":unix,"finished_at":unix,"deployed_at":unix}]

# Stream runtime logs (Docker stdout/stderr — implemented)
GET /v1/services/:serviceID/logs?follow=true&tail=100
# text/plain; tail default 100, max 10000
```

### Environment Variable Rules

**Key format**: Keys must match the pattern `^[a-zA-Z_][a-zA-Z0-9_]{0,127}$` — start with a letter or underscore, followed by up to 127 alphanumeric characters or underscores.

**Value limits**: Values may be up to 32,768 bytes (32 KB). Null bytes (`\x00`) are not allowed.

**Forbidden keys**: The following environment variable names are reserved by the platform and cannot be set by tenants:

| Key | Reason |
|-----|--------|
| `PATH` | Overriding the system PATH could break container execution |
| `LD_PRELOAD` | Security risk — could inject shared libraries into processes |
| `LD_LIBRARY_PATH` | Security risk — could redirect shared library loading |

**Case-insensitive matching**: Forbidden key checks are **case-insensitive**. All of the following will be rejected: `PATH`, `path`, `Path`, `LD_PRELOAD`, `ld_preload`, `Ld_Preload`. The error message will report the key name exactly as you sent it (e.g., `env var "path" is not allowed`).

**Per-request limit**: A single POST request can set at most 100 keys. To set more, issue multiple requests (they merge additively).

---

## Builds

```bash
# Start a Nixpacks build from git
POST /v1/services/:serviceID/builds
Body: {"source_type":"git","source_url":"https://github.com/org/repo","source_ref":"main"}
# source_type must be "git". source_ref defaults to "main" if omitted.
# There is no git_url / branch field.
# → {"id":"...","status":"pending","created_at":unix}

# List builds for a service
GET /v1/services/:serviceID/builds
# → [{"id":"...","status":"building|succeeded|failed|cancelled","image":"..."}]

# Get one build
GET /v1/services/:serviceID/builds/:buildID
# → {"id":"...","status":"succeeded","image":"127.0.0.1:5000/ah-.../svc-...:latest"}

# Stream build logs
GET /v1/services/:serviceID/builds/:buildID/logs?follow=true
# Streams newline-delimited text; blocks until build completes when follow=true

# Cancel a build
DELETE /v1/services/:serviceID/builds/:buildID

# List all builds across all services (tenant-wide)
GET /v1/builds?limit=50
```

**Build log ring buffer:** Logs are tail-preserved when they exceed the maximum size -- the oldest lines are dropped so the most recent output is always available.

**Auto-cancel on suspension:** When a tenant is suspended, all in-progress builds for that tenant are automatically cancelled (`CancelAllForTenant`).

**Pagination limit cap:** List endpoints enforce a server-side maximum on `limit`; requests above the cap are silently clamped.

---

## Databases

```bash
# Create postgres or redis (takes up to 30s — use idempotency key)
POST /v1/databases
Header: Idempotency-Key: <uuid>
Body: {"name":"string","type":"postgres"}  # type: "postgres" | "redis"
# → {"id":"...","name":"...","type":"postgres","status":"running","port":5432}

# List databases
GET /v1/databases
# → [{"id":"...","name":"...","type":"postgres","status":"running"}]

# Get one database
GET /v1/databases/:dbID

# Get decrypted connection string
GET /v1/databases/:dbID/connection-string
# → {"connection_string":"postgres://user:pass@ah-db-<databaseID>:5432/ah?sslmode=disable"}

# Delete database (irreversible — destroys container and data volume)
DELETE /v1/databases/:dbID
```

**Name validation:** Database names must be DNS-safe -- alphanumeric characters and hyphens only, must start and end with an alphanumeric character.

---

## Snapshots

Snapshots capture a service's current image, env config, and resource settings for instant forking.

```bash
# Take a snapshot of a service
POST /v1/services/:serviceID/snapshots
Body: {"name":"before-migration"}
# → {"id":"...","name":"...","service_id":"...","image":"...","created_at":unix}

# List all snapshots (tenant-wide)
GET /v1/snapshots
# → [{"id":"...","name":"...","service_id":"...","created_at":unix}]

# Get one snapshot
GET /v1/snapshots/:snapshotID

# Delete snapshot
DELETE /v1/snapshots/:snapshotID
```

**Retention policy:** Snapshots older than `--snapshot-max-age` or exceeding `--snapshot-max-per-service` are automatically cleaned up by the GC.

---

## Sandboxes (disposable)

A sandbox is a throwaway container that the server deletes when its TTL expires (no stop/start). Same isolation as environments (gVisor in production, caps dropped, PID/memory/CPU limits). Network egress is `none`: no internet, no other tenant containers, no other tenants. Separate quota from environments: `max_sandboxes` (default 5) plus a host-wide cap (default 20). Creates are rate limited to ~20/min per tenant.

```bash
# Create (long route; Idempotency-Key honoured). source.type: template|image|snapshot|service
POST /v1/sandboxes
Body: {"source":{"type":"image","image":"python:3.12-slim"},"ttl_seconds":1800}
#   {"source":{"type":"template","template":"python"}}
#   {"source":{"type":"snapshot","snapshot_id":"..."},"include_env":true}   # runs the app (run=app)
#   {"source":{"type":"service","service_id":"..."},"run":"idle"}            # service's current image
#   optional: name, run (idle|app), include_env, memory_mb (128..2048), cpu_millicores (100..2000), egress ("none" only; "internet" → 403)
# → 201 {"id":"...","name":"sbx-1a2b3c4d","status":"running","source":{"type":"image","ref":"python:3.12-slim"},
#        "image":"python:3.12-slim","run":"idle","egress":"none","memory_mb":512,"cpu_millicores":1000,
#        "ttl_seconds":1800,"created_at":unix,"expires_at":unix,"max_expires_at":unix,"last_activity_at":unix}
# ttl_seconds: default 1800, 60..max_sandbox_ttl_seconds (default 14400)
# Images: Docker Hub, or your own 127.0.0.1:5000 build/snapshot images. Other tenants' resources → 404.

GET /v1/sandboxes?status=running          # list (limit/offset)
GET /v1/sandboxes/:sandboxID
DELETE /v1/sandboxes/:sandboxID           # → 204

# Exec: same body/response as environment exec; not idempotent
POST /v1/sandboxes/:sandboxID/exec
Body: {"command":["python3","-c","print(2+2)"],"work_dir":"/workspace","timeout":60}
# → {"exit_code":0,"stdout":"4\n","stderr":"","truncated":false,"timed_out":false,"duration_ms":120}

# Main process logs (useful for run=app sandboxes)
GET /v1/sandboxes/:sandboxID/logs?tail=100&follow=false

# Push expiry to now + ttl (not past max_expires_at = created_at + 24h by default)
POST /v1/sandboxes/:sandboxID/extend
Body: {"ttl_seconds":3600}
# 400 past max lifetime; 409 when not running / already expired
```

Notes: a sandbox forked with `include_env` gets values like `DATABASE_URL` but is not attached to database networks, so those connections fail. Exec needs the `timeout` binary in the image (distroless → exit 127). Expired sandboxes are deleted within ~30s; stopped/failed ones after 10 minutes. Environment routes return 404 for sandbox IDs and vice versa.

CLI: `ahc sandbox run --image python:3.12-slim -- python3 -V` (create, exec, delete; exit code propagated), plus `ahc sandbox create|list|get|exec|logs|extend|delete` (alias `ahc sbx`).

---

## Kanban (Vikunja)

Provisions a per-tenant Vikunja kanban board in a dedicated container. Provisioning is async -- the `POST` returns immediately and the kanban URL becomes available once the container is ready. Poll `GET /v1/kanban` until the `url` field is populated.

The host port range for kanban containers is configurable via `--kanban-port-start` and `--kanban-port-end` daemon flags.

```bash
# Provision board (async — poll GET /v1/kanban for URL; use idempotency key)
POST /v1/kanban
Header: Idempotency-Key: <uuid>
# → {"id":"...","url":"http://...:PORT","created_at":unix}

# Get board info
GET /v1/kanban
# → {"id":"...","url":"http://...:PORT","created_at":unix}

# Get admin token (decrypted)
GET /v1/kanban/admin-token
# → {"admin_token":"..."}

# Delete board (irreversible)
DELETE /v1/kanban
```

---

## Activity

```bash
# Synthetic event log for the tenant
GET /v1/activity?limit=50
# → [{"id":"...","resource_type":"service|database|build","action":"created|started|failed","message":"...","created_at":unix}]

# Filtering parameters:
#   resource_type=service|database|build   — filter by resource type
#   action=created|started|failed|...      — filter by action
#   service_id=<id>                        — filter by service
#   since=1710000000                       — unix timestamp (seconds), not RFC3339
#   offset=0                               — pagination offset (int)
# Default limit is 50 (activity only); max 200.
```

**Audit events:** Sensitive operations are automatically logged: env var reveals (`?reveal=true`), connection string access, and kanban admin token access.

---

## Service Status Values

| Status | Meaning |
|--------|---------|
| `created` | Just created, not yet deploying |
| `deploying` | Container pull/start in progress |
| `running` | Container is up and passing health checks |
| `stopped` | Manually stopped |
| `failed` | Deploy timed out (10 min) or fatal error |
| `crashed` | Container exited unexpectedly; `circuit_open` is a **boolean field**, not a status. After 5 crashes / 10 min the breaker trips, the container is removed, and status stays `crashed` until reset |

---

## Idempotency

Add to **POST and PUT** requests (not DELETE/PATCH; no PUT routes exist today):
```
Idempotency-Key: <any-stable-uuid>
```
Same key + same tenant + same method + same path + same body hash = replayed 2xx. TTL is 10 minutes.
`POST /v1/auth/keys`, `POST /v1/environments/{id}/exec` and `POST /v1/sandboxes/{id}/exec` are excluded.
