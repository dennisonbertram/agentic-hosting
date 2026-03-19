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
# → {"status":"ok","docker":{"available":true,"version":"29.x"},"gvisor":{"available":true},"disk":{"total_gb":435,"free_gb":397,"used_percent":8.7}}
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

# Update tenant name or email
PATCH /v1/tenant
Body: {"name":"new-name","email":"new@email.com"}

# Delete tenant (irreversible — removes ALL resources)
DELETE /v1/tenant
```

---

## API Keys

```bash
# Create a named key
POST /v1/auth/keys
Body: {"name":"agent-name","expires_in":2592000}  # expires_in in seconds, omit for no expiry
# → {"id":"...","key":"keyid.secret","name":"...","created_at":unix}
# key shown exactly once

# List keys (no plaintext values)
GET /v1/auth/keys
# → [{"id":"...","name":"...","created_at":unix,"expires_at":unix|null}]

# Revoke a key
DELETE /v1/auth/keys/:keyID
```

---

## Services

```bash
# Create service
POST /v1/services
Body: {
  "name": "string",           # required
  "image": "nginx:alpine",    # optional; omit for git-build workflow
  "port": 80,                 # port the app listens on
  "memory_mb": 256,           # optional, default 512
  "cpu_count": 1              # optional, default 1
}
# → {"id":"hex32","name":"...","status":"deploying","url":"http://<id>.localhost",...}

# List services (paginated)
GET /v1/services?limit=50&offset=0
# → [{"id":"...","name":"...","status":"running","url":"...","image":"..."}]

# Get one service
GET /v1/services/:serviceID
# → {"id":"...","name":"...","status":"running","url":"...","last_error":"...","circuit_open":false}

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

# Env vars: set (merge — not replace)
POST /v1/services/:serviceID/env
Body: {"KEY":"value","OTHER":"value2"}

# Env vars: delete one key
DELETE /v1/services/:serviceID/env/:key

# Stream runtime logs (basic — stdout/stderr not yet fully supported, see #11)
GET /v1/services/:serviceID/logs
```

---

## Builds

```bash
# Start a Nixpacks build from git
POST /v1/services/:serviceID/builds
Body: {"git_url":"https://github.com/org/repo","branch":"main"}
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
# → {"connection_string":"postgres://user:pass@127.0.0.1:5432/dbname"}

# Delete database (irreversible — destroys container and data volume)
DELETE /v1/databases/:dbID
```

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

---

## Kanban (Vikunja)

Provisions a per-tenant Vikunja kanban board in a dedicated container.

```bash
# Provision board (takes ~30s — use idempotency key)
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
```

---

## Service Status Values

| Status | Meaning |
|--------|---------|
| `created` | Just created, not yet deploying |
| `deploying` | Container pull/start in progress |
| `running` | Container is up and passing health checks |
| `stopped` | Manually stopped |
| `failed` | Deploy timed out (10 min) or fatal error |
| `circuit_open` | Crash-looped (5 crashes / 10 min); manually reset required |
| `crashed` | Container exited unexpectedly; reconciler will restart |

---

## Idempotency

Add to any mutating request to make retries safe:
```
Idempotency-Key: <any-stable-uuid>
```
Same key + same tenant + same endpoint = same result. Valid 24 hours.
