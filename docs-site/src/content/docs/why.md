---
title: Why it exists
description: Why agentic-hosting exists — agents need JSON, idempotency, crash-loop protection, and scoped keys. Human-first clouds do not provide those.
section: Start
order: 2
---

# Why it exists

Standard hosting platforms assume a human at a keyboard: HTML error pages, OAuth consent screens, dashboards that must be clicked. Agents fail those surfaces in predictable ways.

agentic-hosting was built so an agent can own the full lifecycle of a service without a human in the loop.

## What breaks agents on typical clouds

| Failure mode | What `ah` does instead |
|--------------|------------------------|
| HTML 500 pages | Every handler writes JSON. Errors are `{"error":"<message>"}` via `httpx.WriteError` / `apierr.WriteAPIError`. |
| Duplicate creates on retry | **POST and PUT** run through an idempotency store keyed by tenant + method + path + `Idempotency-Key` (not DELETE/PATCH). TTL is **10 minutes**. Body hash mismatches return 409. |
| Crash loops at machine speed | Reconciler opens a circuit after **5 crashes in 600 seconds**, **stops and removes** the container, and backs off 30m → 1h → 4h (circuit closes; service stays stopped until redeploy). |
| Unbounded blast radius | Keys belong to one tenant. Max **20** active keys. Registration requires a bootstrap token (unless `--dev --open-registration`). |
| Disk filled by a runaway deploy | `diskcheck.CheckAll` on `/var/lib/ah` and `/var/lib/docker`: warn at **80%**, **block at 90%** on deploy, build, database create, kanban create, and backup. |

These are implemented behaviors, not slogans. The circuit math lives in `internal/reconciler`. Idempotency TTL is `idempotencyTTL = 10 * time.Minute` in `internal/middleware/idempotency.go`. Disk thresholds are hardcoded `80, 90` at every call site.

## Who it is for

- An operator who will run **one Linux server** (the documented production host is a Hetzner box) and hand agents an API key.
- Agents that can call HTTP, store a `keyID.secret`, and poll `status` until `running` or `failed`.
- Humans who want a supervisory view — the repo ships `website/dashboard/` and `GET /v1/activity`, but they are optional.

## Who it is not for

- Multi-region or multi-node control planes. State is local SQLite. Horizontal scaling is explicitly a gap (`docs/architecture/horizontal-scaling-gap-analysis.md`).
- Billing. Metering tables and `GET /v1/tenant/usage/metrics` exist; nothing invoices from them.
- Arbitrary git hosts. Builds accept only `github.com`, `gitlab.com`, `bitbucket.org`, `sr.ht`, and `codeberg.org` over HTTPS.

## Economics the code actually encodes

There is no platform fee in the binary. Quotas are per-tenant rows in `tenant_quotas`:

| Quota | Default |
|-------|---------|
| Services | 5 |
| Databases | 3 |
| Environments | 2 |
| Memory | 2048 MB |
| CPU | 2.0 cores |
| Disk | 20 GB |
| API rate limit column | 100 |
| Concurrent builds (quota column) | 3 |
| Env vars per service | 50 |
| API keys | 20 (enforced in `handleKeyCreate`, not the quota table) |
| Tenants on the instance | 1000 active |

New tenants therefore get **2048 MB / 2 CPUs** as container limits (not the 512 MB / 1 CPU fallback in `RunContainer`, which applies only when the quota row cannot be loaded). `PATCH /v1/tenants/{tenantID}/quotas` currently updates service/database/build/env-var caps, not memory/CPU/disk; those quota columns are still operator-configurable in the database. All limits are still bounded by the host’s RAM, CPU, and disk.

## Related reading

- [Architecture](./architecture/) — how the pieces connect
- [Security model](./security/) — isolation layers
- [Roadmap](./roadmap/) — what the code does **not** do yet
