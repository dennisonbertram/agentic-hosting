---
name: agentic-hosting
description: This skill should be used when the user asks to "deploy a service", "deploy an app", "add a domain", "provision a database", "check service status", "view logs", "restart a service", "set environment variables", "reset circuit breaker", "register a tenant", "add a kanban board", "take a snapshot", or mentions operating an agentic-hosting instance. Also trigger when the user says "use agentic-hosting", "spin up on my PaaS", or "deploy to my server".
---

# agentic-hosting Operator Skill

agentic-hosting (`ah`) is an agentic-first self-hosted PaaS. Operate it entirely via REST API — no GUI required. Every action is a curl command. The system builds and runs containerized apps using Nixpacks (from Git) or Docker images, with full gVisor sandbox isolation.

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

Don't have an API key? See **Register a Tenant** below.

---

## Register a Tenant (one-time)

Requires the bootstrap token. On the server, read it with:
```bash
ssh root@<server> "grep AH_BOOTSTRAP_TOKEN /etc/default/paasd"
# (path varies by install — may be /etc/default/ah on fresh installs)
```

```bash
curl -s -X POST $AH_URL/v1/tenants/register \
  -H "X-Bootstrap-Token: $AH_BOOTSTRAP_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name": "my-agent", "email": "agent@example.com"}'
# → {"tenant_id": "...", "api_key": "keyid.secret"}
# SAVE the api_key — it is shown exactly once
```

---

## Deploy a Service

### From a Docker image (fast path)

```bash
SVC=$(curl -s -X POST $AH_URL/v1/services \
  -H "Authorization: Bearer $AH_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"my-app","image":"nginx:alpine","port":80,"memory_mb":256,"cpu_count":1}')
SERVICE_ID=$(echo $SVC | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')
echo "Service ID: $SERVICE_ID"
echo "URL: $(echo $SVC | python3 -c 'import sys,json; print(json.load(sys.stdin)["url"])')"
```

### Build from Git (Nixpacks — zero config)

Supported: GitHub, GitLab, Bitbucket, sr.ht, Codeberg (HTTPS URLs only)

```bash
# 1. Create service
SVC=$(curl -s -X POST $AH_URL/v1/services \
  -H "Authorization: Bearer $AH_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"my-app","port":3000}')
SERVICE_ID=$(echo $SVC | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')

# 2. Start build
BUILD=$(curl -s -X POST $AH_URL/v1/services/$SERVICE_ID/builds \
  -H "Authorization: Bearer $AH_KEY" \
  -H "Content-Type: application/json" \
  -d '{"git_url":"https://github.com/org/repo","branch":"main"}')
BUILD_ID=$(echo $BUILD | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')

# 3. Stream build logs (blocks until done)
curl -sN -H "Authorization: Bearer $AH_KEY" \
  "$AH_URL/v1/services/$SERVICE_ID/builds/$BUILD_ID/logs?follow=true"
```

### Poll until running

Large images can take up to 10 minutes. Always use a 10-minute timeout (120 × 5s):

```bash
for i in $(seq 1 120); do
  STATUS=$(curl -s -H "Authorization: Bearer $AH_KEY" \
    $AH_URL/v1/services/$SERVICE_ID \
    | python3 -c 'import sys,json; print(json.load(sys.stdin)["status"])')
  echo "[$i/120] $STATUS"
  [ "$STATUS" = "running" ] && { echo "RUNNING"; break; }
  [ "$STATUS" = "failed" ]  && { echo "FAILED — check logs"; break; }
  [ "$STATUS" = "circuit_open" ] && { echo "CIRCUIT OPEN — fix app, then POST .../reset"; break; }
  sleep 5
done
```

### Service URLs

The URL a service receives depends on whether the platform was started with `--base-domain`:

**Without `--base-domain` (default):**
```
http://{service-id}.localhost
```
Not publicly routable — only accessible inside the server's Docker network. Useful for internal services or when you are managing routing yourself.

**With `--base-domain apps.example.com`:**
```
https://{dns-label}.apps.example.com
```
The `dns_label` is derived from the service name: lowercased, non-alphanumeric characters replaced with hyphens. Example: service named `My App` gets label `my-app` → URL `https://my-app.apps.example.com`.

**Check which mode the platform is in:**
```bash
curl -s -H "Authorization: Bearer $AH_KEY" \
  $AH_URL/v1/system/health/detailed | python3 -m json.tool
# Look for "baseDomain" in the response
# If present and non-empty → subdomain mode is active
# If absent or empty → localhost mode
```

**Service name constraints when base-domain is set:**
- Must produce a valid DNS label (max 63 chars after conversion)
- Reserved names are blocked: `api`, `admin`, `dashboard`, `traefik`, `www`, `auth`, `login`, `registry` — using one returns `422`
- DNS labels are globally unique across all tenants (first-come-first-served) — if another tenant has `blog.apps.example.com`, your service named `blog` will be rejected with `422`

**Predicting the URL before creating a service:**
Lowercase the name, replace any non-alphanumeric character with a hyphen, trim leading/trailing hyphens. Check `baseDomain` from the health endpoint, then the URL will be `https://{label}.{baseDomain}`.

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

# Wire to service, then restart
curl -s -X POST $AH_URL/v1/services/$SERVICE_ID/env \
  -H "Authorization: Bearer $AH_KEY" -H "Content-Type: application/json" \
  -d "{\"DATABASE_URL\": \"$CONN\"}"
curl -s -X POST $AH_URL/v1/services/$SERVICE_ID/restart \
  -H "Authorization: Bearer $AH_KEY"
```

---

## Common Day-to-Day Operations

```bash
# Health check (no auth)
curl -s $AH_URL/v1/system/health

# Detailed health: disk, Docker, gVisor
curl -s -H "Authorization: Bearer $AH_KEY" $AH_URL/v1/system/health/detailed | python3 -m json.tool

# List services / databases
curl -s -H "Authorization: Bearer $AH_KEY" $AH_URL/v1/services | python3 -m json.tool
curl -s -H "Authorization: Bearer $AH_KEY" $AH_URL/v1/databases | python3 -m json.tool

# Stop / start / restart / delete a service
curl -s -X POST   $AH_URL/v1/services/$SERVICE_ID/stop    -H "Authorization: Bearer $AH_KEY"
curl -s -X POST   $AH_URL/v1/services/$SERVICE_ID/start   -H "Authorization: Bearer $AH_KEY"
curl -s -X POST   $AH_URL/v1/services/$SERVICE_ID/restart -H "Authorization: Bearer $AH_KEY"
curl -s -X DELETE $AH_URL/v1/services/$SERVICE_ID         -H "Authorization: Bearer $AH_KEY"

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
```

---

## New Features (v2026-03)

### Snapshots — fork a service instantly

```bash
# Take a snapshot of a running service
curl -s -X POST $AH_URL/v1/services/$SERVICE_ID/snapshots \
  -H "Authorization: Bearer $AH_KEY" -H "Content-Type: application/json" \
  -d '{"name":"before-migration"}' | python3 -m json.tool

# List snapshots
curl -s -H "Authorization: Bearer $AH_KEY" $AH_URL/v1/snapshots | python3 -m json.tool
```

### Kanban — per-tenant Vikunja board

```bash
# Provision a Vikunja kanban board (takes ~30s, use idempotency key)
curl -s -X POST $AH_URL/v1/kanban \
  -H "Authorization: Bearer $AH_KEY" \
  -H "Idempotency-Key: $(uuidgen)" | python3 -m json.tool

# Get board URL + credentials
curl -s -H "Authorization: Bearer $AH_KEY" $AH_URL/v1/kanban | python3 -m json.tool
curl -s -H "Authorization: Bearer $AH_KEY" $AH_URL/v1/kanban/admin-token | python3 -m json.tool
```

---

## Quick Error Reference

| Error | Fix |
|-------|-----|
| `401 Unauthorized` | Check key format: `keyid.secret` |
| `422 Unprocessable Entity` | Name/email already exists, missing required field, reserved subdomain (`api`, `admin`, etc.), or DNS label too long |
| `429 Too Many Requests` | Back off; respect `Retry-After` header |
| `503 Service Unavailable` | Disk >90% or Docker down — check `/v1/system/health/detailed` |
| Service stuck `deploying` >10 min | Server marks it `failed` at 10 min; check health, delete and retry |
| `circuit_open` | Fix the app, then `POST .../reset`, then `POST .../start` |
| Build `failed` | Stream build logs with `?follow=true` for the error |

For idempotency, limits, and advanced operations see `references/operations.md`.

---

## Known Gaps

- No runtime log streaming (#11) — build logs work; container stdout/stderr via API not yet available
- No API key recovery if all keys lost (#12) — requires SSH to the server

---

## Additional Resources

- **`references/api-reference.md`** — Full endpoint listing with request/response shapes
- **`references/operations.md`** — Idempotency, rate limits, circuit breaker, disk management
- **`references/custom-domains.md`** — How to expose a service on a real domain via Traefik
