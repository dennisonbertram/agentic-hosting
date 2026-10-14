---
title: Overview
description: agentic-hosting is a self-hosted PaaS for AI agents. One Go binary (ah) runs containers with gVisor, builds from git via Nixpacks, and exposes a JSON REST API.
section: Start
order: 1
---

# Hosting infrastructure built for AI agents

**agentic-hosting** is a self-hosted PaaS designed to be operated by AI agents over HTTP. You install a single Go binary (`ah`) on a Linux server. Agents then deploy containers, build from git, provision Postgres or Redis, and recover from crashes — all through a versioned JSON API.

There is no required dashboard. Humans can use the optional supervisory UI or the `ahc` CLI. Agents use `/v1/`.

## What you get

- **Control plane** — `ah`, a single Go 1.25 binary with CGO (SQLite). Default listen address: `127.0.0.1:8080`.
- **Isolation** — tenant service containers run under **gVisor (`runsc`)**, with `CapDrop=ALL`, `ReadonlyRootfs`, `no-new-privileges`, and a per-tenant Docker bridge (`ICC=false`, no internet egress).
- **Builds** — **Nixpacks** source-to-image from HTTPS git URLs on GitHub, GitLab, Bitbucket, sr.ht, or Codeberg. Images land in a loopback registry at `127.0.0.1:5000`.
- **Ingress** — **Traefik** terminates TLS and routes by host. `ah` writes file-provider config; service containers set `traefik.enable=false` so Docker labels cannot hijack routing.
- **State** — **SQLite** in WAL mode: `ah.db` (control plane) and a metering DB. Secrets at rest use **AES-256-GCM**.
- **Self-heal** — a reconciler every **30s**, a garbage collector every **5 min**, a crash-loop circuit breaker, and disk watermarks that block new work at **90%**.

## Design principles (from the code, not a pitch)

1. **Agentic-first** — every mutating endpoint returns JSON. Errors are `{"error":"..."}`. Write routes accept an `Idempotency-Key`.
2. **One happy path per primitive** — Postgres or Redis, Nixpacks, Traefik. No plugin marketplace.
3. **Multi-tenant** — API keys are scoped to one tenant. Resources are namespaced. Rate limits are per-tenant and global.
4. **Secure by default** — gVisor is required in production. The API binds loopback unless you pass `--dev`. HTTPS is enforced unless `--dev` is set.

## Two binaries

| Binary | Role |
|--------|------|
| `ah` | Control plane daemon (`serve`), plus `ah backup` and `ah rotate-key` |
| `ahc` | Client CLI for humans and scripts (`deploy`, `service`, `db`, `env`, `status`, …) |

Install the client with:

```bash
curl -fsSL https://agentic.hosting/install-cli.sh | bash
```

## How agents talk to it

```
Authorization: Bearer <keyID>.<secret>
```

Public (no API key):

- `GET /v1/system/health` → `{"status":"ok"}`
- `POST /v1/tenants/register` (bootstrap token)
- `POST /v1/auth/recover`
- `POST /v1/tenants/{tenantID}/reactivate`
- `POST /v1/system/bootstrap-token/validate`
- `PATCH /v1/tenants/{tenantID}/quotas`

Everything else requires a tenant API key. See [Registration & API keys](./auth/) and the [REST API](./api/).

## Production shape

```
Internet
   │
Traefik :80 / :443
   │
   ├── ah API          127.0.0.1:8080
   ├── Service A       gVisor, tenant bridge
   └── Service B       gVisor, tenant bridge

SQLite + master.key    /var/lib/ah/
Private registry       127.0.0.1:5000
```

The public site is [agentic.hosting](https://agentic.hosting). Source is [github.com/dennisonbertram/agentic-hosting](https://github.com/dennisonbertram/agentic-hosting).

<a href="https://github.com/dennisonbertram/agentic-hosting">★ Star on GitHub</a> if you run this or send agents to it.
