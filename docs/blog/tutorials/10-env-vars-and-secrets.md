---
title: "Manage environment variables and secrets safely"
description: "Set, merge, mask, reveal and delete service env vars on agentic-hosting, hit the validation limits on purpose, and learn where the encryption key lives."
slug: env-vars-and-secrets
tags: [env-vars, secrets, security, api, tutorial]
cover_diagram: diagrams/10-env-vars-and-secrets.png
sources:
  - internal/services/services.go
  - internal/api/services.go
  - internal/api/server.go
  - internal/crypto/crypto.go
  - cmd/ahc/service_env.go
  - cmd/ahc/env.go
  - cmd/ah/main.go
  - cmd/ah/rotate_key.go
  - cmd/ahc/service.go
  - cmd/ahc/root.go
  - internal/api/tenants.go
  - internal/db/migrations/state_014_quota_columns.sql
  - docs-site/src/content/docs/env.md
  - docs-site/src/content/docs/security.md
  - docs/website/github-pages.md
---

# Manage environment variables and secrets safely

Your service needs a database URL and an API token. You do not want either one in a Git repo, in a log line, or in a chat transcript an agent will keep forever. I think secrets should be hard to read by accident and easy to read on purpose, and agentic-hosting is built that way: reads are masked unless you ask otherwise, and every reveal is logged.

This tutorial covers setting variables, reading them masked, revealing them, deleting a key, and breaking the validation rules so you know what the errors look like. By the end you will have a service with correct configuration and you will know which file to back up.

These commands were checked against the source code, not executed for this post. Output shapes below come from the handlers.

## Prerequisites

- An API key for a tenant, exported as `$AH_API_KEY`. (The `ahc` CLI does not read `AH_API_KEY`. It takes `--key`, the `AH_KEY` variable, or `ahc configure`.)
- A service ID. The examples use `$SVC`.
- The API base. The hosted one is `https://api.agentic.hosting/v1/`. If you self-host, use your own host.

```bash
export AH=https://api.agentic.hosting/v1
export AH_API_KEY=<your-key>
export SVC=<your-service-id>
```

## 1. Set variables, at create time or later

You can pass `env` when you create a service. It goes through the same validation as every other path.

```bash
curl -X POST $AH/services \
  -H "Authorization: Bearer $AH_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"my-api","image":"nginx:latest","port":80,"env":{"NODE_ENV":"production"}}'
```

`ahc service create` has `--name`, `--image` and `--port` but no env flag, so use curl for create-time env.

To add more later, post a flat JSON object of key and value strings:

```bash
curl -X POST $AH/services/$SVC/env \
  -H "Authorization: Bearer $AH_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"DATABASE_URL":"postgres://user:pass@host/db"}'
```

```json
{"status":"updated","note":"restart will recreate the container with the new env vars"}
```

The same thing with the CLI. `ahc` splits on the first `=`, so values can contain `=`:

```bash
ahc service env set my-api DATABASE_URL=postgres://user:pass@host/db TOKEN=abc=def
```

### Each call merges

Each call is an upsert per key. Keys you leave out stay as they were. Post `{"FEATURE_X":"on"}` now and `NODE_ENV` and `DATABASE_URL` are still there. There is no replace-all call, so to remove something you delete it by name (step 3).

## 2. Read masked, then reveal

```bash
curl -H "Authorization: Bearer $AH_API_KEY" $AH/services/$SVC/env
```

```json
{"DATABASE_URL":"********","FEATURE_X":"********","NODE_ENV":"********"}
```

The default read returns key names with a fixed mask. It never decrypts anything. The CLI prints a table with `***` in the value column:

```bash
ahc service env get my-api
```

When you need the real values, ask for them:

```bash
curl -H "Authorization: Bearer $AH_API_KEY" "$AH/services/$SVC/env?reveal=true"
# or
ahc service env get my-api --reveal
```

Only the exact string `reveal=true` triggers a reveal. Every reveal writes audit lines to the server log, including the tenant, the service and the ID of the API key used:

```
AUDIT: action=env.revealed tenant=... service=... api_key=...
```

Setting variables is logged too, but with key names only, never values.

## 3. Delete one key

```bash
curl -X DELETE -H "Authorization: Bearer $AH_API_KEY" $AH/services/$SVC/env/FEATURE_X
# or
ahc service env delete my-api FEATURE_X
```

A successful delete returns 204 with no body. Deleting a key that does not exist returns a not-found error ("env var not found").

## 4. Break the validation rules on purpose

Run each of these once so you recognize the 400 responses. All validation happens before anything is encrypted or written.

| Try this | Rule |
|---|---|
| `{"1BAD":"x"}` or `{"has-dash":"x"}` | Keys must match `^[a-zA-Z_][a-zA-Z0-9_]{0,127}$` |
| A value longer than 32,768 bytes | Values are capped at 32 KiB |
| A value containing a null byte | Rejected |
| `{"PATH":"/tmp"}`, `{"ld_preload":"x"}`, `{"LD_LIBRARY_PATH":"x"}` | Denied, and the check ignores case |

```bash
curl -i -X POST $AH/services/$SVC/env \
  -H "Authorization: Bearer $AH_API_KEY" -H "Content-Type: application/json" \
  -d '{"path":"/tmp"}'
```

The error message names the key exactly as you sent it: `env var "path" is not allowed`. A bad key name reads `invalid env var key "1BAD": must match [A-Za-z_][A-Za-z0-9_]{0,127}`. If any key in a request fails, the whole request fails and nothing from it is saved.

## 5. Hit the limits

- An empty JSON object (`{}`) returns 400, "no environment variables provided". A zero-byte body fails JSON decoding first and returns a different 400.
- More than 100 keys in one request returns 400, "too many environment variables (max 100)".
- The tenant quota `max_env_vars_per_service` defaults to 50 and can be changed with `PATCH /v1/tenants/{tenantID}/quotas` (1 to 500).

One caveat on the 50 limit: I found the column, its default and the quota update endpoint in the code, but I could not find the place where the set-env path checks it. Do not rely on it as a guard until you have tried it on your own server.

## 6. Restart to apply

Changing env does not touch a running container. Docker fixes a container's environment at creation, so a plain stop and start would not help. The restart endpoint stops the old container, removes it, and creates a new one from the current image and the current stored variables:

```bash
curl -X POST -H "Authorization: Bearer $AH_API_KEY" $AH/services/$SVC/restart
# or
ahc service redeploy my-api
```

`ahc service env set` prints a reminder to run `ahc service redeploy`. The API also has `POST /v1/services/{id}/redeploy`. If you set `PORT`, the new container uses it as the service port.

## How it works

![Data flow: client JSON goes through ValidateEnvVars, is encrypted with AES-256-GCM using master.key, and is stored in the service_env table. GET returns masked values; reveal=true decrypts and writes an audit log line; restart recreates the container with env injected.](diagrams/10-env-vars-and-secrets.png)

A request lands in the handler, which checks the 100-key cap and calls `ValidateEnvVars`. Each value is then encrypted with AES-256-GCM and upserted into the `service_env` table in one transaction, so a failure mid-way saves nothing. A masked read selects key names only. A reveal decrypts every row and logs it. A restart reads and decrypts the rows again and hands them to the new container.

The encryption key is the server's master key, a file holding 64 hex characters (32 bytes). The setup message in `cmd/ah/main.go` suggests `/var/lib/ah/master.key`, and the path is set with `--master-key-path`. All encrypted columns use this one key. There is no per-tenant key derivation yet, so the server operator can decrypt everyone's values.

Back up `master.key` somewhere that is not the server. If you lose it, stored env values cannot be decrypted. The `ah rotate-key` command re-encrypts stored secrets under a new key; keep the old key until you have confirmed services still start.

## Don't confuse `ahc env` with service env

`ahc env` manages instant sandbox environments (list, create, get, exec, start, stop, delete). It does not set variables. For variables, use `ahc service env get|set|delete`.

## What to try next

- Read the reference page: https://agentic.hosting/docs/env/
- See what else is encrypted and what is not: https://agentic.hosting/docs/security/
- Deploy a service to try this on: https://agentic.hosting/docs/deploy/
- Source and issues: https://github.com/dennisonbertram/agentic-hosting
