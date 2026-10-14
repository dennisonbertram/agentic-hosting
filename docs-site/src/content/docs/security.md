---
title: Security model
description: gVisor, tenant networks, AES-256-GCM secrets, HMAC API keys, HTTPS trust, and systemd hardening — as implemented.
section: Reference
order: 15
---

# Security model

Layers that exist in code today. Plans (HKDF split, Firecracker, mTLS) are not claimed here — see [Roadmap](./roadmap/).

## Container isolation

Every tenant **service** container (`RunContainer`):

| Control | Value |
|---------|--------|
| Runtime | `runsc` (gVisor) — required in production |
| Capabilities | drop ALL |
| Privileges | `no-new-privileges` |
| Rootfs | read-only + tmpfs on `/tmp`, `/var/run`, `/var/tmp`, `/run` |
| PIDs | 256 |
| Swap | `MemorySwap = Memory` |
| Restart | `unless-stopped` (defeated when the circuit opens — reconciler stops the container) |
| Traefik Docker provider | `traefik.enable=false` so image labels cannot register routes |

Database and environment containers also use `runsc` (see `internal/docker` / `internal/environments`).

## Network isolation

- Per-tenant bridge `ah-tenant-{id}`
- Comments and construction treat the network as **internal**, **ICC=false**, no container internet egress
- Traefik is attached to each tenant network for ingress
- Cross-tenant: separate bridges
- API bind: `127.0.0.1` unless `--dev` + explicit `--listen-addr`

Tenant containers are not given a path to the Docker socket.

## Secrets

| Secret | Storage |
|--------|---------|
| Service env | AES-256-GCM in `service_env` |
| DB passwords / connection strings | AES-256-GCM |
| Snapshot env | encrypted blob |
| Kanban admin token | encrypted; reveal AUDIT-logged |
| API keys | HMAC-SHA256 hex (64 chars) keyed by master key |
| Master key | 32-byte file, chmod 600 |
| Bootstrap tokens | env only, HMAC-compared |

`ah rotate-key` re-encrypts rows. There is **no** HKDF domain separation in the running crypto helpers yet (that is a planned doc under `docs/security/`).

## API / HTTP

- Production: `requireHTTPS`. Trust `X-Forwarded-Proto` only from loopback
- `stripUntrustedForwardedHeaders` always (including `--dev`)
- Client IP for registration limits: `X-Real-Ip` from loopback only
- Auth cache 30s (revocation delay)
- Body 1 MB; idempotency hash cap matches
- Chi logger logs method/path/status/latency — not headers or bodies
- Registration errors are generic 422 to reduce email enumeration

## systemd (`deploy/ah.service`)

`NoNewPrivileges`, `ProtectHome`, `PrivateTmp`, `ProtectSystem=strict`, `ProtectKernelTunables/Modules`, `ProtectControlGroups`, `RestrictSUIDSGID`, `LockPersonality`, `PrivateDevices`, `RestrictNamespaces`, `ProtectKernelLogs`, `UMask=0077`.

`ReadWritePaths` includes `/var/lib/ah`, Traefik dynamic `ah.yml`, and the Docker socket.

Runs as user `ah`, group `docker`.

## What a stolen tenant key can do

Everything in that tenant: deploy, delete services, read revealed env, fetch DB connection strings, create keys (up to 20), delete the tenant.

It cannot:

- Register new tenants (bootstrap token)
- Change another tenant’s quotas
- Talk to another tenant’s containers over the bridge
- Enable Traefik routes via container labels

Treat `AH_KEY` like a production credential. Prefer a dedicated key per agent (`POST /v1/auth/keys`) and revoke it.

## Build egress

Git clone only talks to five HTTPS hosts. That is not a general egress proxy. `docs/security/build-egress-allowlist.md` describes a Squid approach that is **not** wired into `ah` as a running feature.
