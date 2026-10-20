---
name: agentic-hosting
description: This skill should be used when the user asks to "deploy a service", "deploy an app", "add a domain", "provision a database", "check service status", "view logs", "restart a service", "set environment variables", "reset circuit breaker", "register a tenant", "add a kanban board", "take a snapshot", "redeploy a service", "recover an API key", "check tenant usage", "view deployment history", "fork a service", or mentions operating an agentic-hosting instance. Also trigger when the user says "use agentic-hosting", "spin up on my PaaS", or "deploy to my server".
---

# agentic-hosting Operator Skill

agentic-hosting (`ah`) is an agentic-first self-hosted PaaS (v0.4.0). Operate it entirely via REST API — no GUI required. Every action is a curl command. The system builds and runs containerized apps using Nixpacks (from Git) or Docker images, with full gVisor sandbox isolation.

## Prerequisites

Set these before issuing any commands:

```bash
export AH_URL="https://agentic.hosting"   # or http://127.0.0.1:8080 on the server
export AH_KEY="keyid.secret"              # your tenant API key
```

Verify connectivity:
```bash
curl -s $AH_URL/v1/system/health          # → {"status":"ok"}
```

Don't have an API key? See **Register a Tenant** or **Recover an API Key** below.

## Slash Commands

Six slash commands are available when the Claude Code skill is installed:

| Command | Purpose |
|---------|---------|
| `/ah-deploy <git-url-or-image> <name> [port]` | Deploy a service from git URL or Docker image |
| `/ah-status` | Full health dashboard — disk, services, databases, circuits |
| `/ah-db <service-name> <postgres\|redis> [db-name]` | Provision a database and wire it to a service |
| `/ah-logs <service-name> [build-id]` | Stream runtime logs, or build logs when a build-id is given |
| `/ah-register <tenant-name> <email>` | Register a new tenant and get an API key |
| `/ah-snapshot <service-name> [snapshot-name]` | Take a snapshot for instant environment forking |

---

## Register a Tenant (one-time)

Requires the bootstrap token. On the server, read it with:
```bash
ssh root@<server> "grep AH_BOOTSTRAP_TOKEN /etc/default/ah"
```

Multiple bootstrap tokens are supported — set `AH_BOOTSTRAP_TOKEN` to a comma-separated list (e.g. `token1,token2`). Any valid token in the list is accepted.

`AH_ADMIN_TOKEN` (also comma-separated, 32+ chars) authorizes the operator-only endpoints: key recovery, tenant reactivation and quota updates. Send it as `X-Admin-Token` (or in the `X-Bootstrap-Token` slot). Once set, bootstrap tokens are rejected (403) on those endpoints and only work for registration. If unset, bootstrap tokens still work there (deprecated; the server logs a warning at startup).

To verify a bootstrap token without registering, use `POST /v1/system/bootstrap-token/validate` with body `{"token":"..."}` — always returns `200` with `{"valid":true}` or `{"valid":false}`. Invalid tokens are not a 401.

```bash
curl -s -X POST $AH_URL/v1/tenants/register \
  -H "X-Bootstrap-Token: $AH_BOOTSTRAP_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name": "my-agent", "email": "agent@example.com"}'
# → {"tenant_id": "...", "api_key": "keyid.secret"}
# SAVE the api_key — it is shown exactly once
```

## Recover an API Key

Lost all API keys? Recover access using the admin token (or bootstrap token if `AH_ADMIN_TOKEN` is unset) and your tenant email:

```bash
curl -s -X POST $AH_URL/v1/auth/recover \
  -H "X-Admin-Token: $AH_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"email": "agent@example.com"}'
# → {"api_key": "newkeyid.newsecret"}
# Rate limited: same limits as tenant registration (5/hour per IP)
```

---

## Deploy a Service

### From a Docker image (fast path)

```bash
SVC=$(curl -s -X POST $AH_URL/v1/services \
  -H "Authorization: Bearer $AH_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"my-app","image":"nginxinc/nginx-unprivileged","port":8080}')
# Stock nginx:alpine crash-loops under ReadonlyRootfs (no /var/cache/nginx tmpfs).
# nginxinc/nginx-unprivileged listens on 8080 and writes temp files under /tmp.
SERVICE_ID=$(echo $SVC | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')
echo "Service ID: $SERVICE_ID"
echo "URL: $(echo $SVC | python3 -c 'import sys,json; print(json.load(sys.stdin)["url"])')"
```

### From a snapshot (instant fork)

```bash
SVC=$(curl -s -X POST "$AH_URL/v1/services?from_snapshot=$SNAPSHOT_ID" \
  -H "Authorization: Bearer $AH_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"my-fork"}')
# Creates a new service with the same image, env vars, and resource limits as the snapshot
```

### Build from Git (Nixpacks — zero config)

Supported: GitHub, GitLab, Bitbucket, sr.ht, Codeberg (HTTPS URLs only). Supports branch names and full SHA commit refs.

```bash
# 1. Create service (`image` is required; `pending` is the placeholder ahc uses
#    for git deploys — create always kicks off an async Deploy that will fail
#    the pull, then the build replaces the image)
SVC=$(curl -s -X POST $AH_URL/v1/services \
  -H "Authorization: Bearer $AH_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"my-app","image":"pending","port":3000}')
SERVICE_ID=$(echo $SVC | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')

# 2. Start build (branch name or SHA). There is no git_url / branch field.
BUILD=$(curl -s -X POST $AH_URL/v1/services/$SERVICE_ID/builds \
  -H "Authorization: Bearer $AH_KEY" \
  -H "Content-Type: application/json" \
  -d '{"source_type":"git","source_url":"https://github.com/org/repo","source_ref":"main"}')
BUILD_ID=$(echo $BUILD | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')

# 3. Stream build logs (blocks until done)
curl -sN -H "Authorization: Bearer $AH_KEY" \
  "$AH_URL/v1/services/$SERVICE_ID/builds/$BUILD_ID/logs?follow=true"
```

### Redeploy (rebuild from same git source)

```bash
curl -s -X POST $AH_URL/v1/services/$SERVICE_ID/redeploy \
  -H "Authorization: Bearer $AH_KEY"
# Triggers a new build from the same source_url / source_ref as the last build
```

### Poll until running

Large images can take up to 10 minutes. Always use a 10-minute timeout (120 × 5s):

`status` is never the string `"circuit_open"` — that is a separate boolean field:

```bash
for i in $(seq 1 120); do
  read STATUS CIRCUIT <<EOF
$(curl -s -H "Authorization: Bearer $AH_KEY" \
    $AH_URL/v1/services/$SERVICE_ID \
    | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d.get("status",""), "true" if d.get("circuit_open") else "false")')
EOF
  echo "[$i/120] $STATUS circuit_open=$CIRCUIT"
  [ "$STATUS" = "running" ] && { echo "RUNNING"; break; }
  [ "$STATUS" = "failed" ]  && { echo "FAILED — check last_error / deployments"; break; }
  [ "$CIRCUIT" = "true" ] && { echo "CIRCUIT OPEN — fix app, then POST .../reset then POST .../redeploy"; break; }
  sleep 5
done
```

### Deployment History

```bash
# View recent deployments for a service
curl -s -H "Authorization: Bearer $AH_KEY" \
  $AH_URL/v1/services/$SERVICE_ID/deployments | python3 -m json.tool
# → [{"id":"...","image":"...","status":"running","deployed_at":unix}]
```

### Service URLs

The URL a service receives depends on whether the platform was started with `--base-domain`:

**Without `--base-domain` (default):**
```
http://{dns-label}.localhost
```
When running locally, Traefik file-provider routes are auto-generated for each service. Access via `curl -H "Host: {dns-label}.localhost" http://localhost`.

**With `--base-domain apps.example.com`:**
```
https://{dns-label}.apps.example.com
```
The `dns_label` is derived from the service name: lowercased, non-alphanumeric characters replaced with hyphens. Example: service named `My App` gets label `my-app` → URL `https://my-app.apps.example.com`.

**Check which mode the platform is in:**
Create any service and inspect the `url` field in the response. If it contains `.localhost`, the platform is in localhost mode. If it contains a real domain, subdomain mode is active.

**Service name constraints when base-domain is set:**
- Must produce a valid DNS label (max 63 chars after conversion)
- Reserved names are blocked: `api`, `admin`, `dashboard`, `traefik`, `www`, `auth`, `login`, `registry` — using one returns `422`
- DNS labels are globally unique across all tenants (first-come-first-served) — if another tenant has `blog.apps.example.com`, your service named `blog` will be rejected with `422`

**The URL is stable.** Once a service is deployed its URL does not change, even if the daemon restarts with a different `--base-domain` value. The `dns_label` is stored in the database at creation time.

See `references/custom-domains.md` for DNS wildcard setup and the full procedure for server operators.

---

## Provision a Database

Database creation takes up to 30 seconds. Always use an idempotency key so retries are safe:

```bash
# Create postgres or redis
DB=$(curl -s -X POST $AH_URL/v1/databases \
  -H "Authorization: Bearer $AH_KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"name":"mydb","type":"postgres"}')
DB_ID=$(echo $DB | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')

# Get connection string (decrypted)
CONN=$(curl -s -H "Authorization: Bearer $AH_KEY" \
  $AH_URL/v1/databases/$DB_ID/connection-string \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["connection_string"])')

# Wire to service, then restart (restart attaches the service to ah-dbnet-*)
curl -s -X POST $AH_URL/v1/services/$SERVICE_ID/env \
  -H "Authorization: Bearer $AH_KEY" -H "Content-Type: application/json" \
  -d "{\"DATABASE_URL\": \"$CONN\"}"
curl -s -X POST $AH_URL/v1/services/$SERVICE_ID/restart \
  -H "Authorization: Bearer $AH_KEY"
```

The URL host is `ah-db-<databaseID>` (port `5432` / `6379`). It works only from that tenant's containers on the database network — not from the host. Never print the full string; show host:port only. Re-GET and re-SET env if a service still has a `127.0.0.1` copy, then restart.

---

## Common Day-to-Day Operations

```bash
# Health check (no auth)
curl -s $AH_URL/v1/system/health

# Detailed health: disk, Docker, gVisor
curl -s -H "Authorization: Bearer $AH_KEY" $AH_URL/v1/system/health/detailed | python3 -m json.tool

# Detailed health bypassing 30-second cache
curl -s -H "Authorization: Bearer $AH_KEY" "$AH_URL/v1/system/health/detailed?fresh=true" | python3 -m json.tool

# List services / databases
curl -s -H "Authorization: Bearer $AH_KEY" $AH_URL/v1/services | python3 -m json.tool
curl -s -H "Authorization: Bearer $AH_KEY" $AH_URL/v1/databases | python3 -m json.tool

# Stop / start / restart / delete a service
curl -s -X POST   $AH_URL/v1/services/$SERVICE_ID/stop    -H "Authorization: Bearer $AH_KEY"
curl -s -X POST   $AH_URL/v1/services/$SERVICE_ID/start   -H "Authorization: Bearer $AH_KEY"
curl -s -X POST   $AH_URL/v1/services/$SERVICE_ID/restart -H "Authorization: Bearer $AH_KEY"
curl -s -X DELETE $AH_URL/v1/services/$SERVICE_ID         -H "Authorization: Bearer $AH_KEY"

# Rename a service
curl -s -X PATCH $AH_URL/v1/services/$SERVICE_ID \
  -H "Authorization: Bearer $AH_KEY" -H "Content-Type: application/json" \
  -d '{"name":"new-name"}'

# Redeploy (rebuild from same git source)
curl -s -X POST $AH_URL/v1/services/$SERVICE_ID/redeploy -H "Authorization: Bearer $AH_KEY"

# Reset circuit breaker (after 5 crashes in 10 min)
curl -s -X POST $AH_URL/v1/services/$SERVICE_ID/reset -H "Authorization: Bearer $AH_KEY"

# Env vars: get (revealed) / set / delete one key
curl -s -H "Authorization: Bearer $AH_KEY" "$AH_URL/v1/services/$SERVICE_ID/env?reveal=true"
curl -s -X POST $AH_URL/v1/services/$SERVICE_ID/env \
  -H "Authorization: Bearer $AH_KEY" -H "Content-Type: application/json" \
  -d '{"KEY":"value","OTHER":"value2"}'
curl -s -X DELETE $AH_URL/v1/services/$SERVICE_ID/env/KEY -H "Authorization: Bearer $AH_KEY"

# Create / list / revoke API keys
curl -s -X POST $AH_URL/v1/auth/keys \
  -H "Authorization: Bearer $AH_KEY" -H "Content-Type: application/json" \
  -d '{"name":"agent-key"}'
curl -s -H "Authorization: Bearer $AH_KEY" $AH_URL/v1/auth/keys | python3 -m json.tool
curl -s -X DELETE $AH_URL/v1/auth/keys/$KEY_ID -H "Authorization: Bearer $AH_KEY"

# Tenant info and usage quotas
curl -s -H "Authorization: Bearer $AH_KEY" $AH_URL/v1/tenant | python3 -m json.tool
curl -s -H "Authorization: Bearer $AH_KEY" $AH_URL/v1/tenant/usage | python3 -m json.tool

# Update tenant name
curl -s -X PATCH $AH_URL/v1/tenant \
  -H "Authorization: Bearer $AH_KEY" -H "Content-Type: application/json" \
  -d '{"name":"new-name"}'

# Activity feed (synthetic event log across all resources)
curl -s -H "Authorization: Bearer $AH_KEY" "$AH_URL/v1/activity?limit=50" | python3 -m json.tool

# Stream runtime logs (container stdout/stderr)
curl -sN -H "Authorization: Bearer $AH_KEY" \
  "$AH_URL/v1/services/$SERVICE_ID/logs?follow=true&tail=100"

# Reactivate a suspended tenant (requires admin token + tenant ID)
curl -s -X POST $AH_URL/v1/tenants/$TENANT_ID/reactivate \
  -H "X-Admin-Token: $AH_ADMIN_TOKEN"
# → {"tenant_id":"...","api_key":"keyid.secret","status":"active"}
```

---

## Snapshots — fork a service instantly

```bash
# Take a snapshot of a running service
curl -s -X POST $AH_URL/v1/services/$SERVICE_ID/snapshots \
  -H "Authorization: Bearer $AH_KEY" -H "Content-Type: application/json" \
  -d '{"name":"before-migration"}' | python3 -m json.tool

# List snapshots
curl -s -H "Authorization: Bearer $AH_KEY" $AH_URL/v1/snapshots | python3 -m json.tool

# Create a new service from a snapshot (instant fork)
curl -s -X POST "$AH_URL/v1/services?from_snapshot=$SNAPSHOT_ID" \
  -H "Authorization: Bearer $AH_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"forked-service"}'

# Delete a snapshot
curl -s -X DELETE $AH_URL/v1/snapshots/$SNAPSHOT_ID -H "Authorization: Bearer $AH_KEY"
```

## Kanban — per-tenant Vikunja board

Provisioning is async — the `POST` returns immediately and the kanban URL becomes available once the container is ready. Poll `GET /v1/kanban` until the `url` field is populated.

```bash
# Provision a Vikunja kanban board (async — poll GET /v1/kanban for URL)
curl -s -X POST $AH_URL/v1/kanban \
  -H "Authorization: Bearer $AH_KEY" \
  -H "Idempotency-Key: $(uuidgen)" | python3 -m json.tool

# Get board URL + credentials (poll until url is available)
curl -s -H "Authorization: Bearer $AH_KEY" $AH_URL/v1/kanban | python3 -m json.tool
curl -s -H "Authorization: Bearer $AH_KEY" $AH_URL/v1/kanban/admin-token | python3 -m json.tool

# Delete board (irreversible)
curl -s -X DELETE $AH_URL/v1/kanban -H "Authorization: Bearer $AH_KEY"
```

---

## Operations / Admin

**Master key rotation:** Rotate the encryption key used for secrets at rest:
```bash
ah rotate-key --new-key-file /path/to/new.key
```

**Snapshot retention:** Configure automatic cleanup of old snapshots via daemon flags:
- `--snapshot-max-per-service N` — keep at most N snapshots per service (oldest pruned first)
- `--snapshot-max-age DURATION` — delete snapshots older than the given duration (e.g. `720h` for 30 days)

**Kanban port range:** Control the host port range allocated to kanban containers:
- `--kanban-port-start PORT` and `--kanban-port-end PORT`

---

## Quick Error Reference

| Error | Fix |
|-------|-----|
| `401 Unauthorized` | Check key format: `keyid.secret` |
| `422 Unprocessable Entity` | Name/email already exists, missing required field, reserved subdomain (`api`, `admin`, etc.), or DNS label too long |
| `429 Too Many Requests` | Back off; respect `Retry-After` header |
| `500` `{"error":"internal error"}` on build/DB/restart/redeploy | Disk ≥90% — check `/v1/system/health/detailed?fresh=true` (not 503) |
| `503 Service Unavailable` | Docker/build/metering/registration unavailable |
| Service stuck `deploying` >10 min | Server marks it `failed` at 10 min; check health, delete and retry |
| `circuit_open: true` (boolean; status is `crashed`) | Fix the app, then `POST .../reset`, then `POST .../redeploy` (`/start` returns 409 — reset removes the container) |
| Build `failed` | Stream build logs with `?follow=true` for the error |
| All API keys lost | Use `POST /v1/auth/recover` with bootstrap token + tenant email |

For idempotency (10-minute TTL), limits, and advanced operations see `references/operations.md`.

---

## Additional Resources

- **`references/api-reference.md`** — Full endpoint listing with request/response shapes
- **`references/operations.md`** — Idempotency, rate limits, circuit breaker, disk management
- **`references/custom-domains.md`** — How to expose a service on a real domain via Traefik
- **`references/server-setup.md`** — Fresh server installation from scratch
