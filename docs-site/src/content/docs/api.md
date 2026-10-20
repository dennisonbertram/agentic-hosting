---
title: REST API
description: Complete /v1/ REST API reference taken from internal/api/server.go and the handler request structs.
section: Reference
order: 13
---

# REST API reference

Base path: `/v1/`. Versioning rule in this repo: breaking changes require `/v2/`; additive fields do not.

Auth header for tenant routes:

```
Authorization: Bearer <keyID>.<secret>
```

Errors (both `httpx.WriteError` and `apierr.WriteAPIError`):

```json
{"error":"human-readable message"}
```

There is no machine `code` field in the JSON body. Status codes carry the class of error.

Pagination (where implemented): `limit` default **100** (activity default **50**), max **200**, `offset` ≥ 0. Oversize `limit` is clamped.

Timestamps in bodies are **unix seconds** unless a query param says RFC3339 (metrics `since`/`until`).

## Conventions

| Header | Where |
|--------|--------|
| `Authorization: Bearer …` | All tenant routes |
| `X-Bootstrap-Token` | Register (and recover/reactivate/quotas only if `AH_ADMIN_TOKEN` is unset) |
| `X-Admin-Token` | Recover, reactivate, quotas when `AH_ADMIN_TOKEN` is set |
| `Idempotency-Key` | POST and PUT only (not DELETE/PATCH; no PUT routes). Excluded: `POST /v1/auth/keys`, `POST .../exec` |
| `Content-Type: application/json` | Bodies |
| `X-Forwarded-Proto: https` | Required in production from loopback Traefik |

CORS (browser dashboard on another origin): `AH_CORS_ORIGINS` / `--cors-origins` is an allowlist. Defaults are `https://agentic.hosting`, `https://www.agentic.hosting`, `https://dennisonbertram.github.io`. Allowed origins get `Access-Control-Allow-Origin` set to that origin (never `*`), `Allow-Headers: Authorization, Content-Type, Idempotency-Key`, and `Vary: Origin`. `OPTIONS` is answered before auth. Credentials are not enabled.

Public health is constant-time and does not touch SQLite.

---

## System

### `GET /v1/system/health`

Public. `{"status":"ok"}`.

### `GET /v1/system/health/detailed`

Auth. Cached 30s. `?fresh=true` bypasses and refreshes the cache.

```json
{
  "status": "ok",
  "docker": {"available": true, "version": "29.x"},
  "gvisor": {"available": true, "version": "..."},
  "disk": {"total_gb": 0, "free_gb": 0, "used_percent": 0},
  "docker_disk": {"total_gb": 0, "free_gb": 0, "used_percent": 0},
  "docker_storage": {
    "images_size_bytes": 0,
    "containers_size_bytes": 0,
    "volumes_size_bytes": 0,
    "build_cache_size_bytes": 0,
    "total_size_gb": 0
  },
  "traefik_networks": 0,
  "container_runtime": "runsc"
}
```

`container_runtime` is the OCI runtime tenant workloads use (`AH_CONTAINER_RUNTIME`): `runsc` (gVisor) in production, `runc` only in a `--dev` sandbox. Omitted when unknown.

`status` becomes `degraded` if SQLite ping fails, either disk path is >90%, or `ah-tenant-*` networks > 200.

### `POST /v1/system/bootstrap-token/validate`

Bootstrap token. Registration rate limiter. 200 if valid, 401 if not.

---

## Tenants

### `POST /v1/tenants/register`

Bootstrap (unless `--open-registration`). Body: `{"name","email"}`.  
`201`/`200` with `{"tenant_id","api_key"}`. See [Registration](./auth/).

### `GET /v1/tenant`

`{"id","name","email","status","created_at","updated_at"}`.

### `PATCH /v1/tenant`

`{"name":"new-name"}` only.

### `DELETE /v1/tenant`

Soft-delete / suspend. Stops services, DBs, kanban; cancels builds.

### `GET /v1/tenant/usage`

```json
{
  "services": {"used": 0, "max": 5},
  "databases": {"used": 0, "max": 3},
  "api_keys": {"used": 0, "max": 20},
  "memory_mb": 2048,
  "cpu_cores": 2.0,
  "disk_gb": 20,
  "rate_limit": 100
}
```

### `GET /v1/tenant/usage/metrics`

Query: `period=hourly|daily` (default daily), `since`, `until` (RFC3339), `service_id`. 503 if metering store is nil.

### `POST /v1/tenants/{tenantID}/reactivate`

Bootstrap. New API key. Tenant must exist and be suspended.

### `PATCH /v1/tenants/{tenantID}/quotas`

Bootstrap. Body fields (all optional, must be ≥ 1 and under caps):

| Field | Cap |
|-------|-----|
| `max_services` | 100 |
| `max_databases` | 50 |
| `max_builds_concurrent` | 10 |
| `max_env_vars_per_service` | 500 |

Response includes those plus unchanged `max_memory_mb`, `max_cpu_cores`, `max_disk_gb`, `api_rate_limit`.

---

## API keys

### `POST /v1/auth/keys`

`{"name","expires_in"}` → `{"id","name","api_key","prefix","expires_at"}`.

### `GET /v1/auth/keys`

Array of key metadata (no secrets).

### `DELETE /v1/auth/keys/{keyID}`

Revoke.

### `POST /v1/auth/recover`

Bootstrap + `{"email"}`.

---

## Services

### `POST /v1/services`

```json
{"name":"web","image":"nginxinc/nginx-unprivileged","port":8080,"env":{"KEY":"val"}}
```

- `name` required, max 128
- `image` validated (Docker Hub or loopback registry)
- `port` 0 means default **8000**; explicit values must be 1–65535
- Returns **201** with `status: "deploying"`; deploy runs in a 10-minute background context
- Stock `nginx:alpine` crash-loops under ReadonlyRootfs (no `/var/cache/nginx` tmpfs). Use `nginxinc/nginx-unprivileged` on **8080**.

### `POST /v1/services?from_snapshot={id}`

`{"name":"fork"}`. Copies snapshot image, port, env.

### `GET /v1/services`

Paginated list.

### `GET /v1/services/{serviceID}`

Includes `url`, `dns_label`, `last_error`, crash/circuit fields.

### `PATCH /v1/services/{serviceID}`

Rename only: `{"name"}`. Allowed characters: alnum, hyphen, underscore, space, dot. DNS uniqueness still applies when `--base-domain` is set.

### `DELETE /v1/services/{serviceID}`

### Lifecycle

| Method | Path |
|--------|------|
| POST | `/v1/services/{id}/start` |
| POST | `/v1/services/{id}/stop` |
| POST | `/v1/services/{id}/restart` |
| POST | `/v1/services/{id}/redeploy` |
| POST | `/v1/services/{id}/reset` |

Restart recreates the container (env changes apply).

### Logs

```
GET /v1/services/{id}/logs?follow=true&tail=100
```

`text/plain`. `tail` default 100, max 10000. Implemented (`handleServiceLogs` → Docker logs).

### Env

See [Env vars](./env/).

### Deployments

```
GET /v1/services/{id}/deployments
POST /v1/services/{id}/deployments/{deploymentID}/cancel
```

### Metrics

```
GET /v1/services/{id}/metrics?period=daily&since=&until=
```

---

## Builds

### `POST /v1/services/{id}/builds`

```json
{"source_type":"git","source_url":"https://github.com/org/repo","source_ref":"main"}
```

**202** `{"build_id","status","image"}`.

### `GET /v1/services/{id}/builds`

### `GET /v1/builds?limit=`

Tenant-wide.

### `GET /v1/services/{id}/builds/{buildID}`

### `GET /v1/services/{id}/builds/{buildID}/logs?follow=true`

`text/plain`. `follow=true` preflights existence before writing 200.

### `DELETE /v1/services/{id}/builds/{buildID}`

`{"status":"cancelled"}`.

---

## Databases

```json
{"name":"mydb","type":"postgres"}
```

`type`: `postgres` | `redis`.

| Method | Path |
|--------|------|
| POST | `/v1/databases` |
| GET | `/v1/databases` |
| GET | `/v1/databases/{dbID}` |
| GET | `/v1/databases/{dbID}/connection-string` |
| DELETE | `/v1/databases/{dbID}` |

---

## Snapshots

```
POST /v1/services/{id}/snapshots   {"name","description"}
GET  /v1/snapshots
GET  /v1/snapshots/{snapshotID}
DELETE /v1/snapshots/{snapshotID}
```

---

## Kanban (Vikunja)

One board per tenant. Create is async; poll `GET /v1/kanban` until `url` is set. Host ports default **7100–9100**.

| Method | Path |
|--------|------|
| POST | `/v1/kanban` |
| GET | `/v1/kanban` |
| GET | `/v1/kanban/admin-token` |
| DELETE | `/v1/kanban` |

Admin token access is AUDIT-logged.

---

## Environments (workspaces)

Templates: `default` (Ubuntu 24.04), `node`, `python`, `go`. Default lease **3600s**. Quota default **2** environments.

### `GET /v1/environments/templates`

### `GET /v1/environments/templates/{templateID}`

### `POST /v1/environments`

```json
{"name":"scratch","template_id":"node","lease_duration_seconds":3600}
```

Name: `^[a-zA-Z][a-zA-Z0-9_-]*$`.

### `GET /v1/environments`

### `GET /v1/environments/{envID}`

### `DELETE /v1/environments/{envID}` → 204

### `POST /v1/environments/{envID}/start` → `{"status":"running"}`

### `POST /v1/environments/{envID}/stop` → `{"status":"stopped"}`

### `POST /v1/environments/{envID}/lease`

```json
{"duration_seconds":3600}
```

Must be positive. Returns the updated environment (including `lease_expires_at`).

### `POST /v1/environments/{envID}/exec`

```json
{"command":["uname","-a"],"work_dir":"/workspace","timeout":60}
```

```json
{"exit_code":0,"stdout":"...","stderr":"","truncated":false,"timed_out":false,"duration_ms":12}
```

Not idempotent.

### `POST /v1/environments/{envID}/sync`

```json
{"git_url":"https://github.com/org/repo","git_ref":"HEAD"}
```

`git_url` required, must start with `https://`.

### Previews

```
POST   /v1/environments/{id}/previews   {"name":"web","port":3000}
GET    /v1/environments/{id}/previews
DELETE /v1/environments/{id}/previews/{previewID}   → 204
```

Preview name: `^[a-zA-Z0-9][a-zA-Z0-9-]{0,62}$`.

---

## Activity

```
GET /v1/activity?limit=50&offset=0&resource_type=service&action=service.created&service_id=&since=1710000000
```

`since` is a **unix timestamp**, not RFC3339 (the skill’s ISO example is wrong).

Synthetic feed assembled from tenants, services, builds, databases, API keys. Sensitive reveals (env `reveal=true`, connection strings, kanban admin token) emit AUDIT log lines.

---

## HTTP status map

| Code | Typical cause |
|------|----------------|
| 400 | Validation (`apierr.Validation`) or bad JSON |
| 401 | Bad/missing API key or bootstrap token |
| 403 | HTTPS required; or `apierr.Forbidden` |
| 404 | Missing resource |
| 409 | Quota, name conflict, idempotency body mismatch, build queue full |
| 413 | Body > 1 MB |
| 422 | Registration uniqueness (deliberately vague) |
| 429 | Rate limit |
| 500 | Unexpected, including disk ≥90% on build/DB/restart/redeploy (`{"error":"internal error"}`) |
| 503 | Docker/build/metering/registration unavailable (manager nil) |

## Timeouts

| Class | Timeout |
|-------|---------|
| Most routes | 30s (`chi.Timeout`) |
| Server `WriteTimeout` | 30s |
| `ReadTimeout` / `ReadHeaderTimeout` | 10s / 5s |
| Logs, DB create, kanban create, env create/sync, exec | no chi 30s timeout |

Streaming past 30s write timeout can still be cut by `http.Server.WriteTimeout`. In practice log follow is used on the long-timeout group; watch server-level write timeout if streams die at ~30s.
