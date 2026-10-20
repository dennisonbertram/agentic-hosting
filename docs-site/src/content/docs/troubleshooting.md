---
title: Troubleshooting
description: Common ah failures — HTTPS required, circuit open, disk 90%, gVisor missing, CGO builds, and wrong slash-command names.
section: Reference
order: 16
---

# Troubleshooting

## `{"error":"HTTPS required"}` on curl to :8080

Production mode only accepts `X-Forwarded-Proto: https` from **loopback**. Direct `http://127.0.0.1:8080` without that header is 403.

```bash
curl -H 'X-Forwarded-Proto: https' http://127.0.0.1:8080/v1/system/health
```

Or hit the Traefik HTTPS URL. Or run `--dev`.

## `ah` will not start

| Log / symptom | Cause |
|---------------|--------|
| `AH_BOOTSTRAP_TOKEN must be set` | Missing `/etc/default/ah` or empty token |
| `each bootstrap token must be at least 32 characters` | Token too short |
| `master key must be 64 hex characters` | Wrong `master.key` |
| `gVisor is required` | `runsc` not in Docker runtimes (non-dev) |
| `address already in use` | Orphan `ah` still bound; `ss -tlnp \| grep 8080` |
| Service inactive after reboot | `systemctl enable ah` was never run |

The unit reads `EnvironmentFile=-/etc/default/ah` (the `-` means missing file is ignored — then the binary fatals on the empty token).

## `FATAL: non-loopback listen address`

You passed `--listen-addr=0.0.0.0` without `--dev`. That is intentional.

## Circuit open / service `crashed`

Five exits or unhealthies in 10 minutes. Container is **stopped and removed**. `/logs` then returns 409 `service has no container`.

1. `GET /v1/services/{id}` — read `last_error`, `crash_count`, and the boolean `circuit_open` (`status` is `crashed`, not `"circuit_open"`)
2. `GET /v1/services/{id}/deployments` for crash history
3. Fix image or env
4. `POST /v1/services/{id}/reset`
5. `POST /v1/services/{id}/redeploy` — **not** `/start` (start returns 409 because reset cleared `container_id`)

Or wait for auto-backoff (30m / 1h / 4h), which **closes the circuit and leaves the service stopped**. Then redeploy.

## Deploy / build returns disk error

`used_percent >= 90` on `/var/lib/ah` or `/var/lib/docker`. Sync paths (build create, DB create, kanban create, deploy, restart, redeploy) return **503** `{"error":"insufficient disk space; try again later"}` with `Retry-After: 10`. Service create returns 201 and the async deploy fails with `last_error` set.

```bash
curl -s -H "Authorization: Bearer $AH_KEY" \
  "$AH_URL/v1/system/health/detailed?fresh=true"
# prune Docker objects if you understand the blast radius
```

## Build `failed` immediately with `unsupported source_type`

You sent `git_url` / `branch`. Send `source_type`, `source_url`, `source_ref`.

## Build rejected host

Only the five allowlisted git hostnames. HTTPS, no credentials in the URL.

## `409` on create

Quota (`max_services`, keys, databases, environments), duplicate DNS label, idempotency body mismatch, or build queue full. Read `error`.

## `401` on every call

Key format is `keyID.secret` (one dot). Tenant suspended. Key revoked or expired. Auth cache is 30s — a brand-new key should work immediately; a just-revoked key may work for up to 30s.

## Registration 422

Name or email already taken. The message is intentionally vague.

## Registration 429

5/IP/hour or 20/hour global, including **failed** token guesses.

## gVisor / containers never start

`docker info` must list `runsc`. Production `ah` exits if it does not. `--dev` only warns — then `Runtime: runsc` still fails at container create.

## CGO / crash on start after a Mac build

`go-sqlite3` needs `CGO_ENABLED=1` on Linux. Build on the server.

## Website / docs container not updating

`website/` is a baked nginx image. Rebuild the image after HTML or docs-site changes. The container does not bind-mount the repo in the documented production flow.

## Skill commands 404

Use `/ah-deploy` not `/deploy`. Both `install-skill.sh` and `install.sh` now download the `/ah-*` command files.

## Logs die at ~30 seconds

`http.Server.WriteTimeout` is 30s. The route group removes chi’s timeout for streams, but the server write timeout still applies. Use `follow=true` in short bursts or raise the server timeout if you fork the binary.

## Useful inspect

```bash
journalctl -u ah -f
curl -H 'X-Forwarded-Proto: https' http://127.0.0.1:8080/v1/system/health
sqlite3 /var/lib/ah/ah.db 'SELECT name FROM schema_migrations ORDER BY name;'
docker ps --format 'table {{.Names}}\t{{.Status}}\t{{.Runtime}}'
```
