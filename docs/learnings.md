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
| Disk ≥90% → 503 (pre-#168: assumed stale) | Since #168, sync paths return 503 `{"error":"insufficient disk space; try again later"}` with `Retry-After: 10` via `apierr.Unavailable`. Builds use `insufficient disk space for builds; try again later`. Service create still returns 201; async deploy sets `failed` + `last_error`. |
| Circuit reset then `/start` | Reset removes the container; `/start` is 409; `/redeploy` creates a new one |
| Poll `status == "circuit_open"` | `circuit_open` is a boolean; `status` stays `crashed` |
| Idempotency on DELETE | POST and PUT only (`idempotency.go` method check) |
| Default resources 512 MB / 1 CPU | New tenants: 2048 MB / 2 CPUs from `tenant_quotas` |
| Secrets use tenant-derived keys | Single server master key; no per-tenant HKDF yet |
| Rate limiter ignores `api_rate_limit` | Auth reads the column; limiter uses it with burst 2× |
| Expired keys still count toward the 20-key cap | Create/list/usage/recover count only live keys (`revoked_at IS NULL` and not past `expires_at`) |
| Keys have no roles | `owner` vs `member`; only owner can revoke keys or `DELETE /v1/tenant` |

## Build constraints

- `ah` requires CGO (`go-sqlite3`). Do not cross-compile with `CGO_ENABLED=0`.
- Website Docker context is now the **repo root** (`-f website/Dockerfile .`) so the docs stage can `COPY docs-site`.
- GitHub Pages on the `agentic.hosting` custom domain uses `ASTRO_BASE=/docs/` (homepage from `website/` at `/`). The old project-pages base `/agentic-hosting/` is retired.

## Homepage location

The marketing homepage lives in this repo at `website/index.html` (also changelog + dashboard). Production hostname is `https://agentic.hosting`.

## 2026-10-14 — InstaBox / nested VMs

- **InstaBox is a Firecracker guest with Docker, not a Host-based PaaS.** Public ingress is `https://<port>-<id>.instabox.tech` (TLS already terminated). Designing ah as “Traefik on 80/443 + Let’s Encrypt + `{name}.{domain}`” does not map 1:1. Use `--dev --listen-addr 0.0.0.0` and publish one port, or add an explicit public-base-URL mode.
- **Signup is human-gated** (GitHub/Google or device code). `ibk_` keys cannot register accounts. Do not plan unattended “agent spins up InstaBox then installs ah” without an existing key.
- **gVisor does not need KVM.** Default platform is `systrap` (seccomp `SECCOMP_RET_TRAP`). Official guidance: systrap inside VMs; KVM on bare metal. In this nested/containerized VM, systrap `docker run --runtime=runsc` worked; `--platform=kvm` hung and `/dev/kvm` was unusable even as root. Prefer systrap on InstaBox; do not require nested virt.
- **`--dev` does not disable the runsc runtime**, only the startup check. Containers still set `Runtime: "runsc"`.

## 2026-10-15 — Pages custom domain

- **Apex Pages and www-on-Hetzner cannot share one A record.** Namecheap `@` must be the four GitHub Pages IPv4s; `www` stays on `178.104.44.25`. GitHub cannot 301 `www` if that name never reaches Pages.
- **Dashboard same-origin is wrong on Pages.** `window.location.origin` would be `https://agentic.hosting`, which no longer has `/v1/`. Default to `https://api.agentic.hosting`. CORS must be an origin allowlist (never `*` with credentials); OPTIONS must run before auth.
- **Astro `base` is URL prefix, not output nesting.** `ASTRO_BASE=/docs/` still writes `dist/index.html`. The assemble step must copy `dist/` into `pages-dist/docs/`.

## 2026-10-15 — Discoverability

- **The website already said MIT; there was no `LICENSE` file.** Adding the file is what GitHub’s license classifier and the README badge need.
- **Cursor `gh` cannot set repo topics/homepage** on this repo (`403 Resource not accessible by integration`). The owner-authenticated GitHub MCP (`admin: true`) has no `update_repository` / topics tool. Use the UI or a PAT with Administration.
- **gVisor 2026 release tarballs are not a single `runsc` binary.** `runsc` looks for `/usr/local/bin/gvisor-bin/gvisor_sentry` (STRICT sidecar policy). Installing only `runsc` fails container create.
- **Git deploys still need a build broker.** `Builder.Build` talks JSON to `AH_BUILD_BROKER_SOCKET` (default `/run/ah-build-broker/broker.sock`) and expects a worker that runs `ah build-worker`. PID 1 `tini` has `systemctl` but it does not start `ah-build@` units.
- **`setBuildGroup` requires the process to be in group `ah-build`.** A shell that started before `usermod -aG ah-build` will fail with `chown: operation not permitted` even after the group exists.
- **Docker `vfs` + BuildKit cache mounts** can fail Nixpacks `COPY` with `invalid argument`. `overlay2` worked. Container DNS needed `iptables: true` and `dns: [host resolver]`.
- **`ahc deploy` git ref defaults to `main`.** `traefik/whoami` is still `master`; `hashicorp/http-echo` is `main`.
- **http-echo without `-text` / `ECHO_TEXT` logs an error.** It is still a valid tiny Go git deploy for a demo; choose a Procfile/PORT app if you want friendlier runtime logs.
- **Git deploys assume systemd.** `internal/builder` always uses `systemd-run --scope` and `runuser -u ah-builder`. If PID 1 is not systemd (tini, s6, some microVM images), clone/build/push cannot start. Need a fallback before calling a platform “git-deploy ready.”
- **Warm pool is hardcoded on** (2×3 templates, max 6). On a 4 vCPU / 8 GB / 20 GB InstaBox that will dominate disk and RAM before any tenant workload. Need a flag before recommending ah on small VMs.
- **ReadonlyRootfs breaks stock nginx.** ah tmpfs covers `/tmp` `/var/run` `/var/tmp` `/run` but not `/var/cache/nginx`. `nginx:alpine` crash-loops under the default security profile. Use a writable-cache image or extra tmpfs; this is independent of InstaBox.
- **Idle pause is a PaaS footgun.** InstaBox pauses after 30 idle minutes and **public URL hits do not wake the box**. A control plane must `auto-suspend 0`.
- **Incorrect assumption:** “4 vCPU / 8 GB / Docker / root / public URLs ⇒ stock ah install just works.” Isolation (gVisor) is fine on systrap; **ingress, init, disk, idle pause, and warm pool** are the real mismatches.

## 2026-10-20 — managed database connectivity (#164)

- **Incorrect assumption (ux-paths + some docs):** service and database both sit on `ah-tenant-X`, so `DATABASE_URL` works after env + restart. Create stores `127.0.0.1`, `RunDatabase` publishes that loopback port on the default bridge, and Create never calls `EnsureNetwork` / `ConnectNetwork`.
- **Second blocker:** tenant bridges are `Internal=true` and `ICC=false`. Putting the DB on `ah-tenant-*` without enabling ICC still drops service-to-database TCP. Ingress already solved this for Traefik with a dedicated ICC=true pair network.
- **Host `SELECT 1` is not a service-path test.** `waitForPostgres` / `waitForRedis` dial host loopback; that is the control plane, not a gVisor service namespace.
- **Default-bridge ICC** means two database containers can reach each other’s *container* IPs even though published ports are loopback-only. Passwords are the real cross-DB isolation today.
- **Hostname-only or HostIP-only patches will not fix the issue.** Need overlay DNS + ICC=true on a network that is not the shared tenant bridge (or accept intra-tenant lateral movement).
- **Fix shipped as per-database `ah-dbnet-*` (Internal + ICC) + `ah-db-<id>` alias.** Tenant bridges stay ICC=false. Host loopback publish stays for probes. Attach is env-link (alias in service env), so another tenant cannot join even with a copied URL.
- **Tradeoff:** two services on the same DB network can talk to each other. Same class as Traefik ingress pair networks.
- **`service_env` is a separate ciphertext.** Rewriting `databases.connection_string_encrypted` is not enough; matching env values must be rewritten too, and the running container still needs a restart to pick up the new URL and join the network.

## 2026-10-22 — README rescue path fields

- **Build create returns `build_id`**, not `id`. `handleBuildCreate` writes `{"build_id","status","image"}`. GET still uses `json:"id"` on the `Build` struct — do not mix them.
- **Omitted service port is 8000**, not 8080. 8080 is the `ah` loopback listen address. `ahc deploy` passes `--port` default `0`, and `Create` fills 8000.
- **Git create still requires `image`.** `"pending"` is a valid Docker Hub-shaped placeholder; `ahc` sends that, then `POST .../builds` with `source_type` / `source_url`.
- **#164 connection strings** use `ah-db-<id>` and the engine port (`5432` / `6379`). Host loopback is probes only. Restart after setting `DATABASE_URL` so the service joins `ah-dbnet-*`.
- **Network names must stay ≤63 characters.** Full tenant ID + db ID overflows (`ah-dbnet-` + 32 + 1 + 32 = 73). Truncate the tenant ID first; keep the globally unique database ID.
