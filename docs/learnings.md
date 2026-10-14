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

## 2026-10-14 — InstaBox / nested VMs

- **InstaBox is a Firecracker guest with Docker, not a Host-based PaaS.** Public ingress is `https://<port>-<id>.instabox.tech` (TLS already terminated). Designing ah as “Traefik on 80/443 + Let’s Encrypt + `{name}.{domain}`” does not map 1:1. Use `--dev --listen-addr 0.0.0.0` and publish one port, or add an explicit public-base-URL mode.
- **Signup is human-gated** (GitHub/Google or device code). `ibk_` keys cannot register accounts. Do not plan unattended “agent spins up InstaBox then installs ah” without an existing key.
- **gVisor does not need KVM.** Default platform is `systrap` (seccomp `SECCOMP_RET_TRAP`). Official guidance: systrap inside VMs; KVM on bare metal. In this nested/containerized VM, systrap `docker run --runtime=runsc` worked; `--platform=kvm` hung and `/dev/kvm` was unusable even as root. Prefer systrap on InstaBox; do not require nested virt.
- **`--dev` does not disable the runsc runtime**, only the startup check. Containers still set `Runtime: "runsc"`.
- **Git deploys assume systemd.** `internal/builder` always uses `systemd-run --scope` and `runuser -u ah-builder`. If PID 1 is not systemd (tini, s6, some microVM images), clone/build/push cannot start. Need a fallback before calling a platform “git-deploy ready.”
- **Warm pool is hardcoded on** (2×3 templates, max 6). On a 4 vCPU / 8 GB / 20 GB InstaBox that will dominate disk and RAM before any tenant workload. Need a flag before recommending ah on small VMs.
- **ReadonlyRootfs breaks stock nginx.** ah tmpfs covers `/tmp` `/var/run` `/var/tmp` `/run` but not `/var/cache/nginx`. `nginx:alpine` crash-loops under the default security profile. Use a writable-cache image or extra tmpfs; this is independent of InstaBox.
- **Idle pause is a PaaS footgun.** InstaBox pauses after 30 idle minutes and **public URL hits do not wake the box**. A control plane must `auto-suspend 0`.
- **Incorrect assumption:** “4 vCPU / 8 GB / Docker / root / public URLs ⇒ stock ah install just works.” Isolation (gVisor) is fine on systrap; **ingress, init, disk, idle pause, and warm pool** are the real mismatches.
