---
name: agentic-hosting
description: This skill should be used when the user asks to install, set up, or operate agentic-hosting — a self-hosted PaaS for AI agents. Trigger on "deploy a service", "provision a database", "register a tenant", "view logs", "install ah", "give this to your agent", or any request to run the ah control plane via its REST API.
---

# agentic-hosting

Fetch this file from a raw GitHub URL and follow it end to end. You do not need a GUI. Every action is curl against `/v1/`.

Canonical routes live in `internal/api/server.go`. If this skill and a blog post disagree, trust the live API.

Companion files (same repo):

- `llms.txt` — llmstxt.org index
- `llms-full.txt` — full docs dump
- `.claude/skills/agentic-hosting/SKILL.md` — Claude Code operator skill (same API)
- `.claude/commands/ah-*.md` — slash commands `/ah-deploy` `/ah-status` `/ah-db` `/ah-logs` `/ah-register` `/ah-snapshot`
- `.claude/skills/agentic-hosting/references/` — api-reference, operations, server-setup, custom-domains

## What it is

`ah` is a single Go 1.25 binary (CGO + SQLite) that runs on a Linux server. It:

- Deploys Docker images and Nixpacks git builds
- Isolates tenant containers with gVisor (`runsc`)
- Routes via Traefik **file provider** (`traefik.enable=false` on containers)
- Stores state in `/var/lib/ah/ah.db` (WAL) and encrypts secrets with AES-256-GCM
- Reconciles every **30s**, warns disk at **80%**, blocks new work at **90%**
- Opens a circuit after **5 crashes in 10 minutes**
- Accepts `Idempotency-Key` on mutating routes (TTL **10 minutes**; `POST /v1/environments/{id}/exec` excluded)

API keys are `keyID.secret`, hashed with HMAC-SHA256 (not bcrypt). Errors are `{"error":"..."}` — no `code` field.

## 0. Credentials

```bash
export AH_URL="https://your-host"          # or http://127.0.0.1:8080 on the server
export AH_KEY="keyid.secret"               # tenant API key
export AH_BOOTSTRAP_TOKEN="..."            # from /etc/default/ah — min 32 chars
```

On the server, production health checks need a forwarded HTTPS header:

```bash
curl -s -H 'X-Forwarded-Proto: https' http://127.0.0.1:8080/v1/system/health
# → {"status":"ok"}
```

`--dev` skips HTTPS enforcement. `--dev --open-registration` skips the bootstrap header.

## 1. Install on a server

Requirements: Ubuntu 22.04+, Docker with `runsc`, Go 1.25+ **CGO**, Nixpacks, Traefik on `traefik-public`, loopback registry `127.0.0.1:5000`.

```bash
# Docker + gVisor
curl -fsSL https://get.docker.com | sh
curl -fsSL https://gvisor.dev/archive.key | sudo gpg --dearmor -o /usr/share/keyrings/gvisor-archive-keyring.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] https://storage.googleapis.com/gvisor/releases release main" | sudo tee /etc/apt/sources.list.d/gvisor.list
sudo apt-get update && sudo apt-get install -y runsc
cat > /etc/docker/daemon.json <<'EOF'
{
  "runtimes": {
    "runsc": { "path": "/usr/bin/runsc" }
  },
  "default-address-pools": [{"base":"172.20.0.0/14","size":24}]
}
EOF
sudo systemctl restart docker
docker info 2>/dev/null | grep runsc

# Go + Nixpacks (build ah ON the server — CGO_ENABLED=0 will not work)
wget -q https://go.dev/dl/go1.25.0.linux-amd64.tar.gz -O /tmp/go.tar.gz
sudo tar -C /usr/local -xzf /tmp/go.tar.gz
export PATH=$PATH:/usr/local/go/bin
curl -sSL https://nixpacks.com/install.sh | bash

# Network + registry
docker network create --driver bridge traefik-public || true
docker run -d --name paas-registry --restart unless-stopped \
  -p 127.0.0.1:5000:5000 -v /var/lib/ah/registry:/var/lib/registry registry:2

# Build and install
git clone https://github.com/dennisonbertram/agentic-hosting /agentic-hosting
cd /agentic-hosting
CGO_ENABLED=1 go build -o bin/ah ./cmd/ah
sudo cp bin/ah /usr/local/bin/ah

# Secrets
sudo mkdir -p /var/lib/ah/builds /var/lib/ah/backups
head -c 32 /dev/urandom | xxd -p -c 64 | sudo tee /var/lib/ah/master.key >/dev/null
sudo chmod 600 /var/lib/ah/master.key
BOOTSTRAP=$(openssl rand -hex 32)
echo "AH_BOOTSTRAP_TOKEN=$BOOTSTRAP" | sudo tee /etc/default/ah
sudo chmod 600 /etc/default/ah
# SAVE the bootstrap token — it cannot be recovered from the file if you lose it

# systemd — prefer the in-repo unit
sudo cp deploy/ah.service /etc/systemd/system/ah.service
sudo systemctl daemon-reload
sudo systemctl enable --now ah
```

Traefik must reverse-proxy to `127.0.0.1:8080` and write file-provider config under `/etc/traefik/dynamic/`. See `.claude/skills/agentic-hosting/references/server-setup.md` and `deploy/ah.service`. Optional `--base-domain apps.example.com` needs wildcard DNS `*.apps.example.com`.

Paths are `/var/lib/ah`, `/etc/default/ah`, `journalctl -u ah` — never `paasd`.

## 2. Register (bootstrap token → tenant → API key)

Validate first (JSON body, **not** the header; always HTTP 200):

```bash
curl -s -X POST "$AH_URL/v1/system/bootstrap-token/validate" \
  -H "Content-Type: application/json" \
  -d "{\"token\":\"$AH_BOOTSTRAP_TOKEN\"}"
# → {"valid":true} or {"valid":false}
```

Register:

```bash
curl -s -X POST "$AH_URL/v1/tenants/register" \
  -H "X-Bootstrap-Token: $AH_BOOTSTRAP_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"my-agent","email":"agent@example.com"}'
# → {"tenant_id":"...","api_key":"keyid.secret"}
# SAVE api_key — shown exactly once
export AH_KEY="keyid.secret"
```

Rate limit: 5 / IP / hour and 20 / hour global (includes failed guesses). Duplicate name/email → 422. Lost all keys: `POST /v1/auth/recover` with bootstrap header + `{"email"}`. Suspended tenant: `POST /v1/tenants/{tenantID}/reactivate` (not `/v1/tenant/reactivate`) → new `api_key`.

## 3. Deploy from git

`image` is **required** on create. `ahc` uses placeholder `"pending"`. Create always starts an async Deploy (the placeholder pull fails); the build replaces the image.

Allowed git hosts (HTTPS only, no credentials in the URL): github.com, gitlab.com, bitbucket.org, sr.ht, codeberg.org.

```bash
SVC=$(curl -s -X POST "$AH_URL/v1/services" \
  -H "Authorization: Bearer $AH_KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"name":"web","image":"pending","port":3000}')
SERVICE_ID=$(echo "$SVC" | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')

BUILD=$(curl -s -X POST "$AH_URL/v1/services/$SERVICE_ID/builds" \
  -H "Authorization: Bearer $AH_KEY" \
  -H "Content-Type: application/json" \
  -d '{"source_type":"git","source_url":"https://github.com/org/repo","source_ref":"main"}')
# There is no git_url / branch field. source_ref defaults to main.
BUILD_ID=$(echo "$BUILD" | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')

curl -sN -H "Authorization: Bearer $AH_KEY" \
  "$AH_URL/v1/services/$SERVICE_ID/builds/$BUILD_ID/logs?follow=true"
```

Docker image path (no build):

```bash
curl -s -X POST "$AH_URL/v1/services" \
  -H "Authorization: Bearer $AH_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"web","image":"nginx:alpine","port":80}'
# CreateRequest is name, image, port, env only — no memory_mb / cpu_count
```

Poll up to 10 minutes:

```bash
for i in $(seq 1 120); do
  STATUS=$(curl -s -H "Authorization: Bearer $AH_KEY" \
    "$AH_URL/v1/services/$SERVICE_ID" \
    | python3 -c 'import sys,json; print(json.load(sys.stdin)["status"])')
  echo "[$i/120] $STATUS"
  [ "$STATUS" = "running" ] && break
  [ "$STATUS" = "failed" ] && break
  [ "$STATUS" = "circuit_open" ] && break
  sleep 5
done
```

Redeploy: `POST /v1/services/$SERVICE_ID/redeploy` (same `source_url` / `source_ref`).

## 4. Postgres / Redis

Creation can take ~30s — always send an idempotency key.

```bash
DB=$(curl -s -X POST "$AH_URL/v1/databases" \
  -H "Authorization: Bearer $AH_KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"name":"mydb","type":"postgres"}')   # or "redis"
DB_ID=$(echo "$DB" | python3 -c 'import sys,json; print(json.load(sys.stdin)["id"])')

CONN=$(curl -s -H "Authorization: Bearer $AH_KEY" \
  "$AH_URL/v1/databases/$DB_ID/connection-string" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["connection_string"])')

# postgres → DATABASE_URL; redis → REDIS_URL. POST env merges (does not replace).
curl -s -X POST "$AH_URL/v1/services/$SERVICE_ID/env" \
  -H "Authorization: Bearer $AH_KEY" -H "Content-Type: application/json" \
  -d "{\"DATABASE_URL\":\"$CONN\"}"
curl -s -X POST "$AH_URL/v1/services/$SERVICE_ID/restart" \
  -H "Authorization: Bearer $AH_KEY"
```

Never print the full connection string to the user — show host:port only.

## 5. Logs

Runtime (implemented — not issue #11):

```bash
curl -sN -H "Authorization: Bearer $AH_KEY" \
  "$AH_URL/v1/services/$SERVICE_ID/logs?follow=true&tail=100"
```

Build logs: `GET /v1/services/{id}/builds/{buildID}/logs?follow=true`.

Server write timeout is 30s; use short follow bursts if the stream dies.

## 6. Full API (live router)

Public (no API key):

| Method | Path | Auth |
|--------|------|------|
| GET | `/v1/system/health` | none |
| POST | `/v1/tenants/register` | `X-Bootstrap-Token` |
| POST | `/v1/auth/recover` | `X-Bootstrap-Token` + `{"email"}` |
| POST | `/v1/tenants/{tenantID}/reactivate` | `X-Bootstrap-Token` |
| POST | `/v1/system/bootstrap-token/validate` | JSON `{"token"}` |
| PATCH | `/v1/tenants/{tenantID}/quotas` | `X-Bootstrap-Token` |

Bearer (`Authorization: Bearer $AH_KEY`):

| Method | Path |
|--------|------|
| GET | `/v1/system/health/detailed` (`?fresh=true` bypasses 30s cache) |
| GET/PATCH/DELETE | `/v1/tenant` (PATCH `{"name"}` only) |
| GET | `/v1/tenant/usage` · `/v1/tenant/usage/metrics` |
| POST/GET | `/v1/auth/keys` · DELETE `/v1/auth/keys/{keyID}` — create response field is `api_key` |
| GET | `/v1/activity?limit=50&since=<unix>` |
| POST/GET | `/v1/services` · GET/PATCH/DELETE `/v1/services/{id}` |
| POST | `.../start` `.../stop` `.../restart` `.../redeploy` `.../reset` |
| GET | `.../logs` `.../deployments` `.../env` `.../metrics` |
| POST/DELETE | `.../env` · `.../env/{key}` |
| POST | `.../builds` body `source_type` `source_url` `source_ref` |
| GET | `/v1/builds` · `.../builds` · `.../builds/{id}` · `.../builds/{id}/logs` |
| DELETE | `.../builds/{id}` |
| POST/GET/DELETE | `/v1/databases` · GET `.../connection-string` |
| POST | `.../snapshots` · GET/DELETE `/v1/snapshots` |
| POST/GET/DELETE | `/v1/kanban` · GET `.../admin-token` |
| POST/GET/DELETE | `/v1/environments` · `.../start` `.../stop` `.../lease` `.../exec` `.../sync` (`git_url`/`git_ref` on **sync only**) · previews |

Pagination default **100** (activity **50**), max 200. Quota exceeded → **409**. Disk/Docker down → **503**.

## Day-to-day

```bash
curl -s -H "Authorization: Bearer $AH_KEY" "$AH_URL/v1/services"
curl -s -X POST "$AH_URL/v1/services/$SERVICE_ID/reset" -H "Authorization: Bearer $AH_KEY"
curl -s -X POST "$AH_URL/v1/services/$SERVICE_ID/start" -H "Authorization: Bearer $AH_KEY"
curl -s -H "Authorization: Bearer $AH_KEY" "$AH_URL/v1/tenant/usage"
```

Circuit open: fix the app, `POST .../reset`, then `POST .../start`. Auto-backoff 30m → 1h → 4h.

Backup on the server: `ah backup` (VACUUM INTO, gzip, keep last 10). Also back up `/var/lib/ah/master.key` off-box.

## Errors

| Symptom | Fix |
|---------|-----|
| `{"error":"HTTPS required"}` | Add `X-Forwarded-Proto: https` from loopback, use Traefik, or `--dev` |
| 401 | Key format `keyID.secret`; tenant suspended; key revoked |
| 422 | Duplicate name/email, reserved DNS label (`api`,`admin`,…), bad field |
| 429 | Honor `Retry-After`; register limiter is 5/IP/hr |
| 503 | Disk ≥90% or Docker down — `GET /v1/system/health/detailed?fresh=true` |
| `unsupported source_type` | You sent `git_url`/`branch`. Send `source_type`/`source_url`/`source_ref` |
| `image is required` | Always send `image` (`pending` for git) |
| Slash command 404 | Commands are `/ah-deploy` not `/deploy` |
