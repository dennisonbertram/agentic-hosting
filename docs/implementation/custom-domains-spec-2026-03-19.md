# Custom Domain Support — Implementation Spec

**Issue**: #14
**Date**: 2026-03-19
**Review source**: GPT-5.4 via Codex (read-only analysis)

---

## Problem

Service URLs are synthetic `http://<service-id>.localhost` hostnames — not routable from the public internet. Agents reading the URL field from the API get something useless.

## Goal

Add `--base-domain` CLI flag. When set, services get `https://<dns-label>.<tenant-id>.<base-domain>` URLs with Traefik auto-provisioning Let's Encrypt certs.

---

## Architecture Decisions (from GPT-5.4 review)

### URL scheme
`<dns-label>.<tenant-id>.<base-domain>`

- **dns-label**: human-readable, persisted as `services.dns_label`, DNS-safe
- **tenant-id**: already lowercase hex, provides namespace isolation without a new migration
- Friendly tenant slugs are a future feature

### Traefik label divergence
| Mode | Host rule | Entrypoints | TLS |
|------|-----------|-------------|-----|
| `--base-domain` set | `Host(<dns-label>.<tenant-id>.<base-domain>)` | `websecure` | `tls=true`, `certresolver=letsencrypt` |
| fallback (no base-domain) | `Host(<service-id>.localhost)` | `web` | none |

Router/service label **keys** always use `serviceID` — never service name — so they stay stable across renames.

### Traefik network fix (blocker)
`traefik.yml` pins Docker discovery to `traefik-public`, but service containers run on per-tenant networks. Fix: add `traefik.docker.network=traefik-public` label to every service container AND connect the service container to `traefik-public` in addition to its tenant network.

---

## Files Changed

### Wave 1 — Foundation (config + migration)
| File | Change |
|------|--------|
| `internal/config/config.go` | Add `BaseDomain string` field |
| `cmd/ah/main.go` | Add `--base-domain` flag, wire to config |
| `internal/api/server.go` | Add `BaseDomain` to `ServerConfig`, pass to managers |
| `internal/db/migrations/state_011_service_dns_label.sql` | Add `dns_label TEXT`, `UNIQUE(tenant_id, dns_label)` |

### Wave 2 — Core routing layer
| File | Change |
|------|--------|
| `internal/services/services.go` | Add `baseDomain` to `Manager`; centralize `publicURL()`, `traefikLabels()`; add `isDNSLabelSafe()`; update Create, Get, ListPaginated; set `dns_label` on insert |
| `internal/services/deploy_image.go` | Pass routing labels through `Deploy`/`DeployImage` |
| `internal/docker/client.go` | Remove hardcoded Traefik labels; connect containers to `traefik-public` in addition to tenant network; add `traefik.docker.network=traefik-public` label |

### Wave 3 — API enforcement + tests
| File | Change |
|------|--------|
| `internal/api/services.go` | Enforce DNS-safe name at create; derive `dns_label` from name |
| `internal/services/services_test.go` | Add tests for `publicURL`, `traefikLabels`, `isDNSLabelSafe`, dns_label uniqueness |
| `internal/docker/client_test.go` | Update label tests for new Traefik shape |

---

## Schema Migration

```sql
-- state_011_service_dns_label.sql
ALTER TABLE services ADD COLUMN dns_label TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_services_tenant_dns_label
  ON services(tenant_id, dns_label) WHERE dns_label != '';
```

Existing rows get empty `dns_label` — they fall back to UUID-based localhost URL until redeployed.

---

## DNS Validation Rules (from kanbans.go pattern)

```
^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$
```
- Lowercase alphanumeric + hyphens
- No leading/trailing hyphen
- Max 63 chars
- Full hostname (`<dns-label>.<tenant-id>.<base-domain>`) must be ≤253 chars total

---

## Key Risks

1. **Existing invalid names**: no rename endpoint — existing services with bad names fall back to UUID URL
2. **Traefik label drift**: changing `--base-domain` updates API responses immediately but running containers keep old labels until redeployed — acceptable, document in runbook
3. **Let's Encrypt rate limits**: 50 certs/domain/week — not a concern for single-tenant dev installs
4. **Traefik network gap**: MUST fix `traefik.docker.network` label or domain routing silently doesn't work

---

## Repomix Include Pattern

```
internal/services/**,internal/docker/**,internal/api/services.go,internal/config/**,cmd/ah/main.go,internal/db/migrations/state_011*
```
