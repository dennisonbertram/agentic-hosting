# App Discovery: Agentic Hosting

## Application Type

**HTTP REST API (no web UI)** — A production-grade, multi-tenant PaaS for containerized AI agent workloads. All interaction is via REST endpoints. The optional supervisory dashboard is served separately but is not part of the core API surface.

## Tech Stack

| Component | Details |
|-----------|---------|
| **Language** | Go 1.25 (CGO-enabled) |
| **HTTP Router** | chi v5 |
| **State Database** | SQLite (WAL mode, ~12 migrations) |
| **Metering Database** | SQLite (1 migration) |
| **Container Runtime** | Docker Engine API + gVisor (runsc) |
| **Build System** | Nixpacks (plan → Docker image) |
| **Reverse Proxy** | Traefik (TLS via Let's Encrypt) |
| **Private Registry** | 127.0.0.1:5000 (local Docker registry) |
| **Kanban Boards** | Vikunja (self-hosted per tenant) |
| **Encryption** | AES-256-GCM (env vars, DB passwords), bcrypt (passwords), HMAC-SHA256 (API keys) |

## User Roles

1. **Bootstrap Token Holder** — Single administrative token that gates tenant registration. Can issue recovery API keys via `POST /v1/auth/recover`.
2. **Tenant Admin** — One per customer. Has one default API key at registration. Can create up to 20 API keys total. Can read/update/delete own tenant, manage services, databases, builds.
3. **API Key Holder** — Authenticates all other requests via `Authorization: Bearer {keyID}.{secret}`. Each key can optionally expire. Keys can be revoked.

## Feature Map

### System Health & Status
- `GET /v1/system/health` — Public liveness check
- `GET /v1/system/health/detailed` — Authenticated, Docker/gVisor versions, disk usage (cached 30s)

### Tenant Management
- `POST /v1/tenants/register` — Create tenant with bootstrap token. Rate limited: 5/IP/hour, 20/global/hour
- `GET /v1/tenant` — Read own tenant info
- `PATCH /v1/tenant` — Update tenant name
- `DELETE /v1/tenant` — Suspend tenant (revokes keys, stops containers)
- `GET /v1/tenant/usage` — Read quota usage

### API Key Management
- `POST /v1/auth/keys` — Create new API key (max 20, optional expiry)
- `GET /v1/auth/keys` — List active keys (prefix only, no secrets)
- `DELETE /v1/auth/keys/{keyID}` — Revoke a key
- `POST /v1/auth/recover` — Recovery endpoint (bootstrap token + email)

### Service Management
- `POST /v1/services` — Create service from image or snapshot
- `GET /v1/services` — List services (paginated)
- `GET /v1/services/{serviceID}` — Get service details
- `DELETE /v1/services/{serviceID}` — Delete service
- `POST /v1/services/{serviceID}/start` — Start
- `POST /v1/services/{serviceID}/stop` — Stop
- `POST /v1/services/{serviceID}/restart` — Restart
- `POST /v1/services/{serviceID}/redeploy` — Redeploy (alias for restart)
- `POST /v1/services/{serviceID}/reset` — Reset circuit breaker
- `GET /v1/services/{serviceID}/logs?follow=true&tail=100` — Stream/fetch logs
- `GET /v1/services/{serviceID}/deployments` — Deployment history

### Environment Variables
- `GET /v1/services/{serviceID}/env?reveal=true` — Get env vars
- `POST /v1/services/{serviceID}/env` — Set/update env vars (max 100)
- `DELETE /v1/services/{serviceID}/env/{key}` — Delete env var

### Builds (Nixpacks Pipeline)
- `POST /v1/services/{serviceID}/builds` — Trigger build (git_url, git_ref)
- `GET /v1/builds` — List all builds for tenant
- `GET /v1/services/{serviceID}/builds` — List builds for service
- `GET /v1/services/{serviceID}/builds/{buildID}` — Get build status
- `GET /v1/services/{serviceID}/builds/{buildID}/logs?follow=true` — Stream build logs
- `DELETE /v1/services/{serviceID}/builds/{buildID}` — Cancel build

### Database Provisioning
- `POST /v1/databases` — Create Postgres or Redis database
- `GET /v1/databases` — List databases (paginated)
- `GET /v1/databases/{dbID}` — Get database details
- `DELETE /v1/databases/{dbID}` — Delete database (wipes volume)
- `GET /v1/databases/{dbID}/connection-string` — Retrieve connection string

### Snapshots
- `POST /v1/services/{serviceID}/snapshots` — Create snapshot
- `GET /v1/snapshots` — List snapshots (paginated)
- `GET /v1/snapshots/{snapshotID}` — Get snapshot details
- `DELETE /v1/snapshots/{snapshotID}` — Delete snapshot

### Kanban Boards
- `POST /v1/kanban` — Provision per-tenant Vikunja kanban
- `GET /v1/kanban` — Get kanban details
- `GET /v1/kanban/admin-token` — Retrieve admin token
- `DELETE /v1/kanban` — Delete kanban

### Activity / Audit Trail
- `GET /v1/activity?limit=50` — List activity events across all resources

## Navigation Structure (API Organization)

```
/v1/system
  /health                          [public]
  /health/detailed                 [authed]

/v1/tenants/register               [public, rate-limited]

/v1/auth
  /keys                            [CRUD]
  /keys/{keyID}                    [revoke]
  /recover                         [public, rate-limited]

/v1/tenant                         [singular, current tenant]
  /usage

/v1/services
  /{serviceID}
    /start, /stop, /restart, /redeploy, /reset
    /logs
    /env, /env/{key}
    /snapshots
    /deployments
    /builds/{buildID}/logs

/v1/databases/{dbID}/connection-string

/v1/snapshots/{snapshotID}

/v1/kanban, /kanban/admin-token

/v1/activity
```

**Timeouts:** 30s for most endpoints. No timeout for streaming logs, database provisioning, kanban provisioning.

**Auth:** Public: `/health`, `/tenants/register`, `/auth/recover`. All others: `Authorization: Bearer {keyID}.{secret}`

**Rate Limits:** Per-tenant: 100 req/sec, 200 burst. Global: 500 req/sec, 1000 burst. Registration: 5/IP/hour, 20/global/hour.

## Data Entities

- **Tenants**: id, name, email, status (active/suspended), quotas
- **API Keys**: id, key_hash (HMAC-SHA256), key_prefix, name, expires_at, revoked_at
- **Services**: id, name, status (deploying/running/stopped/failed/crashed), image, port, url, container_id, circuit breaker state
- **Builds**: id, service_id, status (queued/building/completed/failed/cancelled), git_url, git_ref, image, logs
- **Databases**: id, name, engine (postgres/redis), status, port, encrypted password/connection_string
- **Snapshots**: id, service_id, name, image_ref, port, encrypted env_snapshot
- **Kanbans**: id, tenant_id, container_id, encrypted admin_token (one per tenant)
- **Activity Events**: aggregated from service/build/database/key records on-demand

## Integrations

- **Docker Engine API**: Container lifecycle, image management, volume management, network management
- **gVisor (runsc)**: Syscall interception, readonly rootfs, capability dropping, PID limits
- **Nixpacks**: Git clone → plan → Docker build → push to local registry
- **Traefik**: Reverse proxy, TLS termination, Host-based routing, per-service dynamic config
- **Vikunja**: Per-tenant kanban board containers
- **Local Docker Registry**: `127.0.0.1:5000` for built images

## Error Handling & Recovery

- **Circuit Breaker**: 5+ crashes in 10min → circuit open → auto-recovery with exponential backoff (30min → 1hr → 4hr)
- **Reconciler (30s loop)**: Detects crashed containers, stale deployments, missing containers
- **Garbage Collector (5min loop)**: Removes orphaned containers, volumes, temp dirs, dangling images
- **Rate Limiting**: 429 with Retry-After header
- **Idempotency**: SHA-256 body hash, 10min cache, max 50 per tenant

## Recommended Story Topics

1. **Tenant Onboarding & Registration** — First-time user journey, foundational for all other stories
2. **API Key Management** — Core auth workflow; recovery path is unique security feature
3. **Service Creation & Deployment** — Core feature; introduces async deployment pattern
4. **Service Lifecycle Control** — Daily operations; exposes crash recovery and log streaming
5. **Build Pipeline from Git** — Nixpacks integration; concurrency limits; async with log streaming
6. **Environment Variable Management** — Configuration management; encryption + restart semantics
7. **Database Provisioning** — Data tier; volume management, async provisioning, two engines
8. **Snapshots & Service Templating** — Rapid environment forking; captures image + env vars
9. **Kanban Board Integration** — Unique feature for agent coordination; one per tenant
10. **Health Monitoring & System Status** — Operations; load balancer integration; infra awareness
11. **Activity Audit Trail** — Compliance, debugging, security awareness
12. **Deployment History & Redeploy** — State recovery without rebuilding
