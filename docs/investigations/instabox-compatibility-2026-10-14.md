# InstaBox compatibility — 2026-10-14

**Verdict: likely works as a constrained single-tenant `--dev` box; blocked for an unmodified production install, for unattended signup, and for a Host-based multi-service PaaS.**

This is a docs-and-lab investigation. **No InstaBox machine was created.** Signup requires a human GitHub or Google account. A git-deploy end-to-end on InstaBox was **not** run. Claims below are labeled **tested** or **inferred**.

---

## What InstaBox is

InstaBox (InsForge / Hang Huang, YC P26) is a **cloud Linux VM for AI agents**, not the same product as InsForge's hosted backend (`insforge.dev`) or InstaCloud (`instacloud.com`).

| Item | URL |
|---|---|
| Marketing site | https://instabox.sh |
| Dashboard / login | https://dashboard.instabox.sh (`/login` on the marketing site 308s here) |
| Pricing | https://instabox.sh/pricing |
| Docs (SPA; also in the site JS bundle) | https://instabox.sh/docs , `/docs/cli`, `/docs/ports`, `/docs/machines`, … |
| Agent-readable overview | https://instabox.sh/llms.txt |
| API | https://api.instabox.sh |
| CLI installer | https://api.instabox.sh/install.sh |
| CLI npm entry | https://www.npmjs.com/package/instabox (`npx instabox@latest`) |
| TypeScript SDK | https://www.npmjs.com/package/@insforge/instabox-sdk |
| Public port hostname | `https://<port>-<machine-id-without-m_>.instabox.tech` |
| Discord | https://discord.gg/DvBtaEc9Jz |
| X | https://x.com/insforge |

GitHub repos named in npm (`InsForge/instabox`, `InsForge/instabox-sdk`) returned **404** from this environment (private or not yet published). SDK keywords: `microvm`, `firecracker`.

### Documented machine

- Full VM: own kernel, **root**, Docker + Docker Compose inside.
- Fixed size: **4 vCPU / 8 GiB RAM / 20 GB storage** (CLI help and Limits page).
- Templates: `agent-browser` (default), `universal` (“dev plus Docker, Python (uv), Go and Rust”), `herdr`, `orca`.
- Persistent `/data` (HOME is `/data/home`) plus a root disk.
- Pause keeps memory; stop keeps disks; snapshot is disks only.
- Default **idle pause after 30 minutes**. `instabox auto-suspend <machine> 0` turns it off. **A public URL visit does not wake a paused machine.**
- Up to **16 public HTTPS ports** per machine.
- Free now: **5 machines** (any state, including paused/stopped), 10 snapshots, 200 GB snapshot+file storage, 20 API keys. Pricing page: “Free until we figure it out.”

### CLI / SDK / auth

- People: `instabox login` (browser) or `instabox login --device` (GitHub device code). Optional `--invite-code` (6 digits) if the server asks.
- Programs: `INSTABOX_API_KEY=ibk_...` + `INSTABOX_API_URL=https://api.instabox.sh` (`X-API-Key`).
- Installed CLI this session: `instabox a415c1d` from `https://api.instabox.sh/dl/instabox-linux-amd64`.
- SDK 0.2.0 ships `skills/instabox/SKILL.md`.

---

## Signup: does a free tier work without a human?

**No. Tested.**

| Attempt | Result |
|---|---|
| `GET/POST https://api.instabox.sh/v1/machines` with no token | `401 unauthenticated` / `missing or malformed access token` |
| Same with `X-API-Key: ibk_fake` | `401 invalid API key` |
| `instabox whoami` / `templates` / `create` | `not logged in; run instabox login` |
| `instabox login --device --provider github` | Printed `https://github.com/login/device` + a one-time code. Stopped here — a human must approve it. |
| Env `INSTABOX_API_KEY` | Absent in this environment |

There is a free tier **after** a GitHub or Google account exists. API keys cannot create accounts or other keys. InsForge’s hosted backend advertises ID-JAG autonomous registration; **InstaBox’s published login path does not.**

---

## What agentic-hosting needs

From `README.md`, `.claude/skills/agentic-hosting/references/server-setup.md`, and the code:

| Need | Why | Hardness |
|---|---|---|
| Linux + root | Install Docker, bind privileged ports, systemd | Docs |
| Docker 29+ | All workloads | Production start opens a Docker client |
| gVisor `runsc` registered with Docker | `Runtime: "runsc"` on services, DBs, environments | **Fatal in production** if missing (`cmd/ah/main.go`); `--dev` only warns, then containers still request `runsc` |
| Nixpacks | Git → image | Missing builder → `build system is not available`. Builds run via `systemd-run` + `runuser -u ah-builder` |
| Loopback registry `:5000` | Nixpacks push target | Operational, not a start-time fatal |
| Traefik on **80/443** + file provider | Host-based URLs, Let’s Encrypt in production | Service URLs are `https://{dns}.{baseDomain}` or `http://{id}.localhost` |
| systemd | `ah.service`; git clone/build/push use `systemd-run --scope` | Git deploy fails if PID 1 is not systemd |
| Disk | Warn 80% / block 90% on `/var/lib/ah` and `/var/lib/docker` | Install docs: 20+ GB |
| `AH_BOOTSTRAP_TOKEN` + `master.key` | Auth / encryption | Required unless `--dev --open-registration` |
| Production bind | Loopback only; TLS proxy must be on loopback | `--listen-addr 0.0.0.0` requires `--dev` |

Warm pool is **hardcoded on** (`Enabled: true`, 2 per template, max 6) in `cmd/ah/main.go`. That pulls `ubuntu:24.04`, `node:22-bookworm`, `python:3.12-bookworm` on first start.

---

## Requirement vs InstaBox vs this VM

This VM is a **nested KVM guest** (`systemd-detect-virt` → `kvm`; `Docker info` → “Ubuntu 24.04.5 LTS (containerized)”). PID 1 is `tini`, not systemd. It is **not** an InstaBox Firecracker guest, but it is a useful analog for “Docker + gVisor inside someone else’s VM.”

| Requirement | InstaBox (docs) | This VM (tested) | Fit |
|---|---|---|---|
| Linux root | Yes | Passwordless sudo | **OK** |
| Docker | “Docker and Docker Compose run inside”; `universal` includes Docker | Installed Docker **29.9.0**; `hello-world` via `runc` OK; overlayfs + cgroup v2 | **Likely OK** |
| gVisor / runsc | **Not mentioned** | Installed `runsc release-20261005.0`. Default/`systrap`: `hello-world` and `alpine uname` OK (kernel `4.19.0-gvisor`). `ptrace`: `runsc do` OK. `--platform=kvm` **hung** on `docker run`; `/dev/kvm` exists but is not usable even as root | **Likely OK via systrap** (gVisor’s recommended platform *inside* a VM). KVM platform not required and probably unavailable inside Firecracker |
| Nested / microVM | Firecracker microVM (SDK). Guest has its own kernel | Nested KVM, containerized userspace | Systrap does not need `/dev/kvm` ([gVisor platforms](https://gvisor.dev/docs/architecture_guide/platforms/)) |
| Traefik 80/443 | Public URL is **one HTTPS URL per published guest port**, already TLS-terminated. 16 ports max. No documented wildcard / Host vhost / Let’s Encrypt on the guest | Non-root cannot bind 80/443 (`EACCES`); 8080/8090 OK | **Blocked as designed** for production Host+LE Traefik. Workable if you publish `ah`’s 8080 (and maybe Traefik HTTP) and accept one URL per port |
| Alternate ports behind InstaBox proxy | `instabox port publish <box> 8080` → `https://8080-<id>.instabox.tech`. Does not start the server. Pause/stop/idle-pause take the URL offline | — | **OK for a single `--dev` API.** Bad for `{name}.{domain}` multi-service routing unless you put Traefik on one published port *and* the proxy forwards `Host` (unknown) |
| Nixpacks | Not mentioned; root + Docker imply you can install it | Not installed; `ah` started with a warning | **Inferred installable** |
| Local registry | Not mentioned | `registry:2` on `127.0.0.1:5000` answered `/v2/` | **Likely OK** |
| systemd | Not mentioned. “Full VM” often has it | `systemd-run` → `System has not been booted with systemd as init system`. Git builds **cannot** start | **Unknown on InstaBox; blocked here.** Ask Hang |
| `ah-builder` user | — | Not created | Required by `internal/builder/builder.go` even if systemd works |
| Disk | 20 GB / machine; 200 GB account cap | 254 GB free; not representative | **Tight.** Warm pool + Nixpacks + registry can blow 20 GB |
| RAM / CPU | 8 GiB / 4 vCPU | 16 GiB / 4 vCPU | **Demo-scale only.** Warm pool of 6 language images is too fat for 8 GiB |
| Idle pause | 30 min default; URL hits do not resume | — | **Blocked for a public PaaS** unless `auto-suspend 0` |
| systemd unit for `ah` | — | Can run the binary in the foreground | **OK as a process**; install docs assume systemd |

---

## What was actually run here

Tested on this Cursor VM, not on InstaBox:

1. Installed InstaBox CLI; probed API; started (and cancelled) GitHub device login.
2. Installed Docker 29.9.0 and started `dockerd` **without systemd**.
3. Installed gVisor; registered `runsc`, `runsc-systrap`, `runsc-kvm`.
4. Confirmed `runc` and **systrap `runsc` work**; KVM `runsc` hangs.
5. Internal `--internal` / ICC=false network: gVisor alpine could not reach `1.1.1.1`.
6. Built `ah` with Go 1.25.0 (`CGO_ENABLED=1`).
7. Started `/tmp/ah --dev --open-registration --port 8080`. Log: **`gVisor (runsc) runtime verified`**.
8. Registered a tenant; `GET /v1/system/health/detailed` → Docker 29.9.0, gVisor `release-20261005.0`.
9. Created `nginx:alpine` via the API. Container used `Runtime=runsc`, status went `running`, then crash-looped: **ReadonlyRootfs** — `mkdir /var/cache/nginx/client_temp` failed. That is an **ah security default**, not InstaBox.
10. Warm pool started 6 gVisor containers (`ubuntu` / `node` / `python`); `docker exec … uname` → `4.19.0-gvisor`.
11. `POST /v1/services/{id}/builds` → **`build system is not available`** (no Nixpacks). Even with Nixpacks, `systemd-run` would fail on this PID 1.

Not tested: InstaBox SSH, `instabox-port`, Let’s Encrypt, a real git→Nixpacks→registry→runsc path, Firecracker guest `/dev/kvm`, whether the public proxy forwards `Host`.

---

## How you would run ah on InstaBox (inferred)

If a human signs up and creates `universal` with **`auto-suspend 0`**:

```text
instabox create --name ah --template universal --auto-suspend 0
instabox ssh ah
# inside: install runsc, nixpacks, Go 1.25, ah-builder, optional systemd unit
# do NOT use stock Traefik :80/:443 + Let's Encrypt as the public edge
ah --dev --listen-addr 0.0.0.0 --port 8080 ...
instabox-port publish 8080 --name ah-api
```

Use Docker-image deploys first. Treat git deploys as blocked until systemd + `ah-builder` are confirmed.

For a service HTTP port, publish **that** port (or a single Traefik HTTP port) — you will not get `https://web.apps.example.com` from InstaBox’s default URL scheme.

Save an InstaBox snapshot after the install so later boxes skip the 20-minute bootstrap.

---

## Code / config changes that would help (not implemented)

No product PR from this investigation. These would make InstaBox (and any port-forward VM) less painful:

1. **`--runtime` / `AH_DOCKER_RUNTIME`** — today `runsc` is hardcoded in `RunContainer` / `RunDatabase` / `RunEnvironment`. `--dev` only skips the *check*. A runc fallback is a security change (Ralph Loop); do not silently default to it in production.
2. **`systemd-run` fallback** — if `systemctl is-system-running` fails, exec nixpacks/git/docker push directly (or via `nice`/`cgroup`). Without this, git deploy is dead on tini/s6/Firecracker-minimal init.
3. **`--dev --listen-addr 0.0.0.0` runbook** for “TLS already terminated in front of us.” Production still refuses non-loopback.
4. **Public URL override** — `AH_PUBLIC_BASE_URL=https://8080-….instabox.tech` so API/service URLs are not `{id}.localhost`.
5. **Single-port / path-based routing** — Traefik PathPrefix or skip Traefik and publish the container port through InstaBox.
6. **Disable or shrink warm pool by flag** — hardcoded 6 fat images will wreck 20 GB / 8 GiB.
7. **Optional `ah-builder`** — run builds as the current user when the dedicated account is missing.
8. **Richer ReadonlyRootfs tmpfs** — `/var/cache` (or image-specific mounts) so stock `nginx:alpine` survives. Seen on this VM; not InstaBox-specific.
9. **Don’t assume `:80/:443` + ACME** when `AH_TRUSTED_PROXY=1`.

None of these is small enough and InstaBox-proven enough to ship from this session.

---

## Questions for Hang

1. Is PID 1 **systemd** on `universal`, or a minimal init? Does `systemd-run --scope` work?
2. Is **`/dev/kvm`** inside the guest (nested virt), or should we assume **no KVM**?
3. Docker details: engine version, storage driver, cgroup v1/v2, is `dockerd` privileged, can we add a custom runtime (`runsc`)?
4. Public URL proxy: HTTP/1.1 `Host` forwarded? WebSockets? HTTP/2? One published port + Traefik Host rules — does that work?
5. Can a machine bind **80/443**, or should we only use high ports + `instabox-port`?
6. Custom domains / ACME on the guest vs only `*.instabox.tech`?
7. Can we grow **root + `/data` past 20 GB**?
8. Can **inbound HTTPS** count as activity so a public API does not idle-pause? Or is `auto-suspend 0` the supported “server” mode?
9. Agent-only account create (no GitHub/Google human)? ID-JAG / invite codes for launch?
10. Snapshot size/time after installing Docker + gVisor + Nixpacks + `ah`?
11. iptables / `DOCKER-USER` / extra bridges allowed (ah’s per-tenant internal networks)?
12. Any block on listening on `127.0.0.1:5000` or mounting `/var/run/docker.sock` into containers (Traefik)?

---

## Open questions (not Hang)

- Whether `nginx:alpine` + ah ReadonlyRootfs should be fixed independently.
- Whether a future ah Firecracker runtime (`docs/architecture/firecracker-integration-plan.md`) is a better fit than “ah-inside-InstaBox-Firecracker” (nested Docker+gVisor).
- Whether InstaBox is meant as the **host for ah** or as a **peer** (agent box vs agent PaaS).
