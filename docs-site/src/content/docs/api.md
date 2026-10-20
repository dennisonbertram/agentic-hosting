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
  "sandboxes": {"used": 0, "max": 5},
  "memory_mb": 2048,
  "cpu_cores": 2.0,
  "disk_gb": 20,
  "rate_limit": 100
}
```

### `GET /v1/tenant/usage/metrics`

Query: `period=hourly|daily` (default daily), `since`, `until` (RFC3339), `service_id`, `kind=service|sandbox` (default `service`; with `kind=sandbox`, `service_id` filters by sandbox ID). 503 if metering store is nil. Sandbox samples are stored with `resource_kind='sandbox'` and never appear in service metrics.

### `POST /v1/tenants/{tenantID}/reactivate`

Bootstrap. New API key. Tenant must exist and be suspended.

### `PATCH /v1/tenants/{tenantID}/quotas`

Bootstrap. Body fields (all optional, must be within min and cap):

| Field | Min | Cap | Default |
|-------|-----|-----|---------|
| `max_services` | 1 | 100 | 5 |
| `max_databases` | 1 | 50 | 3 |
| `max_builds_concurrent` | 1 | 10 | 3 |
| `max_env_vars_per_service` | 1 | 500 | 50 |
| `max_sandboxes` | 0 | 50 | 5 |
| `max_sandbox_ttl_seconds` | 60 | 86400 | 14400 |
| `max_sandbox_lifetime_seconds` | 60 | 604800 | 86400 |
| `sandbox_egress_allowed` | 0 | 1 | 0 |
| `max_sandbox_disk_mb` | 16 | 8192 | 1024 |

`max_sandboxes: 0` disables sandboxes for the tenant. `sandbox_egress_allowed: 1` lets the tenant create sandboxes with `egress: "internet"`, but only on a server whose operator has enabled sandbox egress (`--sandbox-egress-network`); elsewhere it has no effect and those requests get 403. `max_sandbox_disk_mb` caps a sandbox's `disk_mb`; it only matters on a server with tmpfs sandbox workspaces (`AH_SANDBOX_WORKSPACE_TMPFS`, on by default).

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

Environment routes only see environments: a sandbox ID returns 404 here, and `GET /v1/environments` does not list sandboxes.

---

## Sandboxes (disposable)

A sandbox is a throwaway container with a TTL. The server deletes it (container, any `/workspace` volume and record) when the TTL expires; there is no stop/start. Isolation is the same as an environment: the configured workload runtime (`runsc` / gVisor in production), all capabilities dropped, `no-new-privileges`, 256 PIDs, memory = swap, CPU limit, `noexec` `/tmp`. An operator can run sandboxes under a separate gVisor runtime (`AH_SANDBOX_RUNTIME`, see below).

**Network:** egress `none` by default. The sandbox is attached only to the tenant's internal network (`Internal`, inter-container traffic off): no internet, no other tenant containers, no other tenants. A sandbox forked with `include_env` gets values such as `DATABASE_URL` but is not attached to database networks, so those connections fail.

**Internet egress (opt-in):** `egress: "internet"` works only when both the operator has set `--sandbox-egress-network` / `AH_SANDBOX_EGRESS_NETWORK` (and ah verified the host firewall rules at startup) **and** the tenant's `sandbox_egress_allowed` quota is 1. Otherwise it is 403: `egress "internet" is not available on this server` or `... is not enabled for this tenant`. An egress sandbox is attached **only** to the shared egress bridge, never to the tenant network: it can reach the public internet, but host firewall rules drop traffic to private ranges (RFC 1918, 100.64/10), link-local addresses including cloud metadata (169.254.169.254), loopback, multicast and the host itself; inter-container traffic on the bridge is off and IPv6 is disabled. Egress sandboxes always cold start (no warm pool). Their network bytes are metered and show in `GET /v1/tenant/usage/metrics?kind=sandbox` (`network_rx_bytes`, `network_tx_bytes`; the first 60s sample is a baseline). Operators: docs/runbooks/sandboxes-hardening.md. Status: unverified on the production host until the runbook's host verification checklist has been run.

**Hard disk limit (default):** `/workspace` is a tmpfs of `disk_mb` (default half of `memory_mb`) instead of a volume, so writes beyond `disk_mb` fail with "No space left on device". Under gVisor the tmpfs is sandbox memory, so `disk_mb` counts against `memory_mb`. With the operator's `runsc-sbx` runtime (`--overlay2=root:memory`) writes to the root filesystem are memory-backed too and bounded by `memory_mb`. An operator can opt out with `AH_SANDBOX_WORKSPACE_TMPFS=false`; on such a server `disk_mb` is 0 in responses (a volume with no hard size limit) and sending `disk_mb` is 400. Sandboxes are disposable: while the host disk is at the 90% block threshold the server deletes running sandboxes, largest `disk_mb` first, until usage drops below it. Status: unverified on the production host (in particular that gVisor charges the tmpfs and memory overlay to the sandbox's memory cgroup).

**Limits:** per-tenant `max_sandboxes` (default 5) and a host-wide cap (`--max-sandboxes-total` / `AH_MAX_SANDBOXES_TOTAL`, default 20), both enforced atomically at create; free-disk check before create and again after the image pull (503 `insufficient disk space`); image pulls are limited host-wide (`AH_SANDBOX_MAX_CONCURRENT_PULLS`, default 2), so a create may wait for a pull slot; an `image` source larger than `AH_SANDBOX_MAX_IMAGE_MB` (default 4096) is 400 `image too large`; `template` sources use the server's local copy of the template image without contacting the registry; creates are rate limited to about 20/minute per tenant (429 with `Retry-After`). Sandboxes and environments have separate quotas and separate name namespaces: a sandbox may share a name with an environment, and names are unique only within each kind (409 names the conflicting kind).

| Method | Path | Notes |
|--------|------|-------|
| POST | `/v1/sandboxes` | Long route, `Idempotency-Key` honoured, may pull an image |
| GET | `/v1/sandboxes?limit&offset&status` | `status` = `creating\|running\|stopped\|failed\|deleting` |
| GET | `/v1/sandboxes/{sandboxID}` | |
| DELETE | `/v1/sandboxes/{sandboxID}` | 204. Long route |
| POST | `/v1/sandboxes/{sandboxID}/exec` | Same body/response as environment exec. Not idempotent |
| GET | `/v1/sandboxes/{sandboxID}/logs?tail=100&follow=false` | Main process logs, `tail` clamped to 10000 |
| POST | `/v1/sandboxes/{sandboxID}/extend` | `{"ttl_seconds":1800}` |

### `POST /v1/sandboxes`

```json
{
  "name": "optional; default sbx-<8 hex>; same rule as environment names",
  "source": {"type": "template", "template": "python"},
  "ttl_seconds": 1800,
  "run": "idle",
  "include_env": false,
  "memory_mb": 512,
  "cpu_millicores": 1000,
  "egress": "none",
  "disk_mb": 256
}
```

| Field | Rules |
|-------|-------|
| `source` | One of `{"type":"template","template":"<name or id>"}` (default template `default`), `{"type":"image","image":"python:3.12-slim"}`, `{"type":"snapshot","snapshot_id":"…"}`, `{"type":"service","service_id":"…"}` |
| `ttl_seconds` | 60 to min(`max_sandbox_ttl_seconds`, `max_sandbox_lifetime_seconds`) (defaults 14400 and 86400). Omitted: 1800, lowered to that bound when the tenant's bound is below 1800. An explicit value outside the range is 400 |
| `run` | `idle` runs `sleep infinity` in `/workspace` (default for template and image); `app` keeps the image's ENTRYPOINT/CMD and WORKDIR (default for snapshot and service) |
| `include_env` | Snapshot and service sources only. Copies the source's decrypted env vars into the container. Values are never returned by the API |
| `memory_mb` | 128 to min(2048, tenant `max_memory_mb`). Omitted: template value, else 512, lowered to that maximum when the tenant's maximum is smaller (400 only if the maximum is below 128). An explicit value outside the range is 400 |
| `cpu_millicores` | 100 to min(2000, tenant `max_cpu_cores` × 1000). Omitted: template value, else 1000, lowered to that maximum the same way. An explicit value outside the range is 400 |
| `egress` | `none` (default) or `internet`. `internet` → 403 unless the server and the tenant both allow it (see above) |
| `disk_mb` | Only on servers with tmpfs workspaces, the default (otherwise 400). 16 to min(`memory_mb`, tenant `max_sandbox_disk_mb`, default 1024). Omitted: half of `memory_mb`, within that range |

Every resolved image goes through the service image allowlist: Docker Hub images, or this tenant's own `127.0.0.1:5000/ah/<tenant>-*` build images and `127.0.0.1:5000/snapshots/<tenant>:*` snapshot images. `localhost/…` and other registries → 400. A snapshot or service owned by another tenant → 404. A service with no built image → 409. A service source reads the service's current image directly and does not create a snapshot.

Response (201; `GET` returns the same shape, `list` an array):

```json
{
  "id": "1a2b…", "name": "sbx-1a2b3c4d", "status": "running",
  "source": {"type": "snapshot", "ref": "<snapshot_id>"},
  "image": "127.0.0.1:5000/snapshots/<tenant>:<id>",
  "run": "app", "egress": "none",
  "memory_mb": 512, "cpu_millicores": 1000, "disk_mb": 256, "ttl_seconds": 1800,
  "created_at": 1760000000, "expires_at": 1760001800,
  "max_expires_at": 1760086400, "last_activity_at": 1760000000
}
```

No `container_id`, volume name or env values are returned. `disk_mb` is the tmpfs `/workspace` size, or 0 on a server that uses volumes (no hard size limit).

The create limiter reports its state in `X-Sandbox-Create-RateLimit-Limit` (requests per second, `0.333` = 20/minute) and `X-Sandbox-Create-RateLimit-Remaining`; `X-RateLimit-*` keep describing the general per-tenant API limit.

### `POST /v1/sandboxes/{sandboxID}/exec`

Argv only (no shell; wrap with `sh -c` yourself). The image must contain the coreutils `timeout` binary (distroless images fail with exit code 127). Default working directory `/workspace`. A background process that keeps stdout open holds the request until the timeout; redirect it (`nohup cmd >/tmp/x 2>&1 &`). Exec bumps `last_activity_at` but does not extend the TTL.

### `POST /v1/sandboxes/{sandboxID}/extend`

```json
{"ttl_seconds": 3600}
```

Sets `expires_at = now + ttl_seconds`. 400 if `ttl_seconds` is outside 60..`max_sandbox_ttl_seconds` (no default: `ttl_seconds` is required) or the new expiry would pass `max_expires_at` (created_at + `max_sandbox_lifetime_seconds`, default 24h); 409 if the sandbox is not running or has already expired. Returns the updated sandbox.

### Lifecycle

```
creating → running → (TTL) → deleted within ~30s
running → container died → stopped → deleted after 10 min
creating → stuck 5 min → failed → deleted after 10 min
DELETE → deleted now
tenant suspended → stopped → deleted after 10 min
```

The sandbox reaper runs every 30s. A teardown that fails (Docker error other than not found) leaves the row `deleting`; it is retried after 2 minutes. Lifecycle events are logged as `AUDIT: action=sandbox.{create,delete,extend,expired,reaped}`.

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
| 500 | Unexpected (`{"error":"internal error"}`) |
| 503 | Docker/build/metering/registration unavailable (manager nil); disk ≥90% on build, DB create, kanban create, deploy and restart/redeploy (`{"error":"insufficient disk space; try again later"}`, `Retry-After: 10`); deploy queue full (`Retry-After`) |

## Timeouts

| Class | Timeout |
|-------|---------|
| Most routes | 30s (`chi.Timeout`) |
| Server `WriteTimeout` | 30s |
| `ReadTimeout` / `ReadHeaderTimeout` | 10s / 5s |
| Logs, DB create, kanban create, env create/sync, exec, sandbox create/delete/logs/exec | no chi 30s timeout |

Streaming past 30s write timeout can still be cut by `http.Server.WriteTimeout`. In practice log follow is used on the long-timeout group; watch server-level write timeout if streams die at ~30s.
