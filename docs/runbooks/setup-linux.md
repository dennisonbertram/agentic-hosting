# Runbook: set up a Linux host

`scripts/setup-linux.sh` is the single command for a Linux server. It has two modes, picked from what is already on the host:

| Host state | Mode | What happens |
|---|---|---|
| None of `/var/lib/ah`, `/etc/default/ah`, `/etc/systemd/system/ah.service` exist | **fresh** | Runs `scripts/bootstrap-stack.sh`, then builds and installs `ahc`, then checks API health |
| All of those plus `/var/lib/ah/master.key` exist | **existing** | Idempotent upgrade/repair (see [operations runbook](operations.md#upgrade)) |
| Some but not all exist | **partial** | Refuses to change anything; see "Partial failure and recovery" in [fresh-install.md](../ops/fresh-install.md) |

The script never regenerates the master key or the bootstrap token.

## Requirements

- Ubuntu 24.04, amd64 or arm64, for automated fresh installs. Other Debian or Ubuntu releases are reported as unsupported by the check. Use the manual steps in the [README Installation section](../../README.md#installation) for those; `scripts/bootstrap-stack.sh` is the reference for what a correct install contains.
- Root (`sudo`). `--dry-run` works without root.
- A checkout of this repository on the host. The installer builds `ah` from it.
- `python3`, `curl` and `git`, which Ubuntu server images include.
- For a fresh install, the host must be new. `bootstrap-stack.sh` refuses a host with an existing `/etc/docker/daemon.json`, running Docker containers, a `traefik-public` network or `ah-registry` volume, ports 80/443/5000/8080/8081 in use, existing ah users, or an ingress subnet that overlaps `172.24.0.0/24`.
- Inbound TCP 80 and 443 from the internet, and SSH restricted to trusted operator addresses.

## Fresh install

```sh
git clone https://github.com/dennisonbertram/agentic-hosting
cd agentic-hosting

# 1. Check prerequisites and see the plan. Nothing changes.
sudo scripts/setup-linux.sh --dry-run --domain example.com --acme-email ops@example.com

# 2. Install.
sudo scripts/setup-linux.sh --domain example.com --acme-email ops@example.com
```

`--dry-run` prints a `[ok]`/`[miss]` prerequisite list and the plan. When every check passes on a fresh host it also runs `bootstrap-stack.sh --dry-run`, which checks the host preflight conditions listed above.

Options:

| Flag | Meaning |
|---|---|
| `--domain DOMAIN` | Parent domain. API at `api.DOMAIN`, services at `*.apps.DOMAIN`. Required for fresh installs |
| `--acme-email EMAIL` | Let's Encrypt contact. Required unless `--external-tls-terminated` |
| `--external-tls-terminated` | Port 80 is reachable only through a trusted TLS tunnel (the Mac mini/ngrok layout). Never use it on a host whose port 80 is reachable from a network |
| `--skip-website` | Do not install the website container |
| `--dry-run` | Check and print the plan only |
| `--yes`, `-y` | Do not prompt before making changes |

What the fresh install does (from `bootstrap-stack.sh`):

- Installs Docker 29 from Docker's signed apt repository (the signing key fingerprint is verified) and gVisor `runsc` from the gVisor apt repository. It writes `/etc/docker/daemon.json` with the `runsc` runtime.
- Installs Go and Nixpacks from checksummed downloads, and pins the Traefik, registry and nginx images by digest.
- Creates the `ah`, `ah-builder`, `ah-proxy` and `ah-build` system users.
- Generates `/var/lib/ah/master.key` (owner `ah`, mode 0600) and writes `AH_BOOTSTRAP_TOKEN`, `AH_ADMIN_TOKEN`, `AH_BASE_DOMAIN=apps.DOMAIN`, `AH_NIXPACKS_PATH` and `AH_BUILD_DIR` to `/etc/default/ah` (root, mode 0600).
- Installs `ah.service`, `ah-api-proxy.service`, `ah-build-broker.service` and `ah-build@.service` from `deploy/`.
- Starts the file-provider Traefik (`paas-traefik`), the loopback registry and the optional website, then waits for `GET /v1/system/health`.

`setup-linux.sh` then builds `cmd/ahc` with `/usr/local/go/bin/go`, installs `/usr/local/bin/ahc`, and repeats the health check.

The full topology, DNS records and credential handling are described in [fresh-install.md](../ops/fresh-install.md).

## After installing

1. Point DNS at the host: `api`, `*.apps` and `www` A records (see the table in [fresh-install.md](../ops/fresh-install.md)). ACME certificates are issued once DNS resolves to the host.
2. Read the tokens from `/etc/default/ah` through an approved secure channel. Do not paste them into tickets or chat.
3. Register the first tenant:

   ```sh
   export AH_URL=https://api.example.com
   AH_BOOTSTRAP_TOKEN=... ahc register my-team ops@example.com --save
   ```

4. Check it from the host:

   ```sh
   curl -fsS -H 'X-Forwarded-Proto: https' http://127.0.0.1:8080/v1/system/health
   sudo systemctl --no-pager status ah ah-api-proxy ah-build-broker
   ```

## If something fails

- **A `[miss]` line in the dry run**: fix that prerequisite, then rerun the dry run.
- **`bootstrap-stack.sh` stops partway**: do not delete state to make a rerun pass. Follow "Partial failure and recovery" in [fresh-install.md](../ops/fresh-install.md). `setup-linux.sh` reports such a host as `mode=partial` and refuses to change it.
- **Health check fails**: `sudo journalctl -u ah -n 100 --no-pager`. Common causes are listed in [operations.md](operations.md#troubleshooting).
