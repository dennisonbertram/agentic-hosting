---
title: Roadmap
description: What ah already ships versus known gaps tracked in-repo — no vapor features.
section: Reference
order: 17
---

# Roadmap

This page lists **gaps the codebase itself acknowledges**. It is not a promise of delivery dates.

## Shipped (see the rest of these docs)

Control plane API, tenants, HMAC keys, services, Nixpacks git builds, Postgres/Redis, env encryption, reconciler + circuit breaker, GC, disk watermarks, backups, `ah rotate-key`, snapshots, Vikunja kanban, instant environments + warm pool + previews, metering **query** endpoints, activity feed, supervisory dashboard assets, `ahc` CLI, Claude skill / `ah-*` commands.

Current tagged line in `CHANGELOG.md` is **0.5.0** (instant environments). Unreleased work includes `ahc` polish and warm-pool fixes.

## Explicitly not done

From `docs/implementation/build-log.md` and architecture notes:

| Gap | Evidence |
|-----|----------|
| Billing | Metering tables exist; nothing invoices |
| Prometheus / Grafana | No exporter |
| Multi-node / HA control plane | SQLite + local Docker; `docs/architecture/horizontal-scaling-gap-analysis.md` |
| HKDF key separation | Plan in `docs/security/hkdf-key-separation-plan.md` — runtime still uses the raw master key for HMAC + AES |
| Firecracker | Plan only (`docs/architecture/firecracker-integration-plan.md`) |
| Daemonless builds (Kaniko) | Prototype note, Nixpacks+Docker is what runs |
| Build egress proxy (Squid) | Decision doc, not a running proxy in `ah` |
| mTLS between services | Listed as planned in README; networks already deny ICC |
| CI/CD deploy pipeline | `docs/decisions/cicd-pipeline.md` — implementation deferred (this docs workflow is static Pages only) |

## Open GitHub issues (at the time this page was written)

The tracker is the live list: [github.com/dennisonbertram/agentic-hosting/issues](https://github.com/dennisonbertram/agentic-hosting/issues).

Examples that were open:

- [#108](https://github.com/dennisonbertram/agentic-hosting/issues/108) — env var set response masking connection strings
- [#82](https://github.com/dennisonbertram/agentic-hosting/issues/82) — custom domain management API UX

README’s older “planned” bullets (HKDF, multi-domain routing, billing API, mTLS, audit logging) are partly stale: **AUDIT log lines** already exist for sensitive reads; custom domains exist as `--base-domain` + Traefik, not as a tenant self-serve multi-domain API.

## Docs vs skill drift

Operator skill references were not all updated when routes moved (`/v1/system/bootstrap-token/validate`, `/v1/tenants/{id}/reactivate`, build `source_url`). Treat `internal/api/server.go` as canonical.

## How to follow along

- [Changelog](https://agentic.hosting/changelog/)
- [CHANGELOG.md](https://github.com/dennisonbertram/agentic-hosting/blob/main/CHANGELOG.md)
- [Build log](https://github.com/dennisonbertram/agentic-hosting/blob/main/docs/implementation/build-log.md)

<a href="https://github.com/dennisonbertram/agentic-hosting">★ Star on GitHub</a> to watch releases.
