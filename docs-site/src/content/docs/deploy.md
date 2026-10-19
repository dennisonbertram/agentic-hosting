---
title: Deploy from git
description: Create a service and start a Nixpacks build from an HTTPS git URL. Allowed hosts, refs, logs, cancel, and redeploy.
section: Use
order: 6
---

# Deploy from git

Two deploy paths exist:

1. **Image** — `POST /v1/services` with `"image"` (Docker Hub name or `127.0.0.1:5000/...` / `localhost:5000/...`). Deploy starts asynchronously (10 minute timeout).
2. **Git** — create a service with a placeholder image (`ahc` sends `"pending"` — `image` is required), then `POST /v1/services/{id}/builds` with `source_type: "git"`.

There is no `git_url` / `branch` field on the build API. The struct is:

```json
{
  "source_type": "git",
  "source_url": "https://github.com/org/repo",
  "source_ref": "main"
}
```

`source_type` must be `git`. `source_ref` defaults to **`main`**. SHA refs are allowed (sanitized, then clone + checkout).

## Allowed git hosts

From `allowedGitHosts` in `internal/builds`:

- `github.com`
- `gitlab.com`
- `bitbucket.org`
- `sr.ht`
- `codeberg.org`

Also enforced:

- HTTPS only
- No userinfo in the URL
- URL max 2048 chars
- Ref max 256 chars, charset `A-Za-z0-9._/-`

This is SSRF / DNS-rebinding protection, not a preference list.

## Sequence

```bash
# 1. Create the service (`image` is required; "pending" is the ahc placeholder)
curl -s -X POST "$AH_URL/v1/services" \
  -H "Authorization: Bearer $AH_KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"name":"web","image":"pending","port":3000}'

# 2. Start the build
curl -s -X POST "$AH_URL/v1/services/$SERVICE_ID/builds" \
  -H "Authorization: Bearer $AH_KEY" \
  -H "Content-Type: application/json" \
  -d '{"source_type":"git","source_url":"https://github.com/org/repo","source_ref":"main"}'
# 202 {"build_id":"...","status":"pending","image":"127.0.0.1:5000/..."}

# 3. Stream logs (no 30s request timeout on this route)
curl -sN -H "Authorization: Bearer $AH_KEY" \
  "$AH_URL/v1/services/$SERVICE_ID/builds/$BUILD_ID/logs?follow=true"
```

On success the build manager calls `DeployImage` and the service moves to running (or failed). Poll `GET /v1/services/{id}`.

Stuck `deploying` longer than **10 minutes** is marked `failed` by the reconciler.

## Images ah will accept

`services.ValidateImage`:

- Docker Hub: `name`, `name:tag`, `org/name:tag` (lowercase path rules)
- Local registry: `127.0.0.1:5000/...` or `localhost:5000/...` with at least two path segments after the host

Arbitrary registries are rejected.

Quickstart images must survive **ReadonlyRootfs** + `CapDrop ALL` (tmpfs only on `/tmp`, `/var/run`, `/var/tmp`, `/run`). Stock `nginx:alpine` crash-loops trying to `mkdir /var/cache/nginx`. Use `nginxinc/nginx-unprivileged` on port **8080**.

## CLI and skill shortcuts

```bash
ahc deploy https://github.com/org/repo web --port 3000
# or
ahc deploy nginxinc/nginx-unprivileged web --port 8080
```

`ahc deploy` treats `http://` or `https://` as git. The Claude command `/ah-deploy` does the same, then streams build logs.

## Redeploy

```
POST /v1/services/{id}/redeploy
```

Starts a new build from the **last** git `source_url` / `source_ref` recorded for that service. Returns the new build object.

## Cancel

```
DELETE /v1/services/{id}/builds/{buildID}
```

Tenant suspend also runs `CancelAllForTenant` for pending/running builds.

## Concurrency and disk

- In-flight build slots: **10** process-wide. A full queue fails the new build with **409** `build queue full; try again later`.
- Disk: warn 80%, block 90% on `/var/lib/ah` and `/var/lib/docker` before the build starts.
- Logs: 5 MB then last 500 lines kept.

## List

```
GET /v1/services/{id}/builds
GET /v1/builds?limit=100          # tenant-wide, max 200
GET /v1/services/{id}/builds/{id}
GET /v1/services/{id}/deployments
POST /v1/services/{id}/deployments/{deploymentID}/cancel
```

## URLs after deploy

| Mode | URL |
|------|-----|
| No `--base-domain` | `http://{serviceID}.localhost` |
| `--base-domain apps.example.com` | `https://{dns_label}.apps.example.com` |

`dns_label` is stored at create time and does not change if you later restart `ah` with a different `--base-domain`. Reserved names are rejected (see [Architecture](./architecture/)). Labels are globally unique across tenants.

## Snapshots (fork, not a build)

```
POST /v1/services/{id}/snapshots   {"name":"before"}
POST /v1/services?from_snapshot={snapshotID}   {"name":"fork"}
```

Copies image, port, and env. No Nixpacks run. Still goes through the async deploy path.
