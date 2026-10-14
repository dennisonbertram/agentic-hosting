# Learnings

Cross-cutting facts found while building the documentation site (2026-10-14).

## Incorrect assumptions (docs vs code)

| Assumption in README / skill | What the code does |
|------------------------------|--------------------|
| Reconciler interval 60s | `30 * time.Second` in `cmd/ah/main.go` |
| Disk block at 95% (0.1.0 changelog) | Every `diskcheck` call site is **80 / 90** |
| API keys bcrypt-hashed (README security section) | HMAC-SHA256 with master key; bcrypt is for passwords |
| Build body `git_url` / `branch` | `source_type`, `source_url`, `source_ref` |
| `POST /v1/auth/bootstrap/validate` + `X-Bootstrap-Token` → 401 | `POST /v1/system/bootstrap-token/validate` body `{"token"}` → always 200 `{"valid":bool}` |
| Create service with no `image` for git | `image` is required; `ahc` sends `"pending"` |
| `POST /v1/tenant/reactivate` | `POST /v1/tenants/{tenantID}/reactivate` |
| Idempotency TTL 24h (skill operations.md) | `10 * time.Minute` |
| Activity `since` is RFC3339 | Unix timestamp |
| Runtime logs not implemented (#11 in skill) | `GET /v1/services/{id}/logs` exists |
| PidsLimit 100 (landing page terminal) | `256` |
| Global build concurrency 3 | Channel buffer **10**; quota column default is 3 |
| `GET/PUT .../environments/{id}/files` | Not routed; sync is `POST .../sync` |
| PATCH tenant email | PATCH only accepts `name` |
| Homepage `/deploy` commands | Repo files are `/ah-*`; `install.sh` now installs those same files |

## Build constraints

- `ah` requires CGO (`go-sqlite3`). Do not cross-compile with `CGO_ENABLED=0`.
- Website Docker context is now the **repo root** (`-f website/Dockerfile .`) so the docs stage can `COPY docs-site`.
- GitHub project Pages needs `base: /agentic-hosting/`. Same origin `/docs/` uses `ASTRO_BASE=/docs/`.

## Homepage location

The marketing homepage lives in this repo at `website/index.html` (also changelog + dashboard). Production hostname is `https://agentic.hosting`.
