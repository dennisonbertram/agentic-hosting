# Build Egress Allowlist Architecture

**Issue**: #3
**Status**: Proposed
**Author**: Claude Code
**Date**: 2026-03-20

---

## Problem Statement

Build containers currently have unrestricted outbound network access. When a tenant triggers a Nixpacks build (via `POST /v1/services/{id}/builds`), the build process can reach any external host. This creates several risks:

1. **Exfiltration** -- a malicious `package.json` postinstall script could POST secrets to an attacker-controlled server.
2. **Supply-chain injection** -- builds could pull packages from unofficial registries or typosquatted domains.
3. **Resource abuse** -- build containers could be used as proxies, torrent nodes, or crypto miners during the build window.
4. **Legal liability** -- uncontrolled egress from a multi-tenant system exposes the operator to abuse complaints.

The build pipeline (`internal/builder/builder.go`) shells out to `nixpacks build`, which internally runs `docker build`. During this process, the Docker build context has full internet access. We need to restrict egress to only the package registries and services required by the supported language stacks (Node, Python, Go, Rust).

---

## Current Build Flow

```
Builder.Build()
  -> gitClone()      # systemd-run + runuser + git clone  (needs github.com, etc.)
  -> nixpacksBuild()  # systemd-run + runuser + nixpacks build (needs npm, PyPI, etc.)
  -> pushImage()      # docker push to 127.0.0.1:5000 (local only)
```

Key observations:
- `gitClone` already runs inside a systemd scope with `MemoryMax=512M`.
- `nixpacksBuild` runs inside a systemd scope with `MemoryMax=2G`, `CPUQuota=200%`.
- Nixpacks internally invokes `docker build`, which creates its own container.
- The Docker daemon runs on the host; Nixpacks connects via `DOCKER_HOST=unix:///var/run/docker.sock`.
- The `pushImage` step is local (`127.0.0.1:5000`) and does not need external network access.

---

## Proxy Architecture Options

### Option A: Squid Forward Proxy

**How it works**: Run a Squid HTTP/HTTPS proxy on the host. Inject `HTTP_PROXY`/`HTTPS_PROXY` into the Docker build environment. Squid enforces a domain allowlist via `ssl_bump` + ACLs.

| Pros | Cons |
|------|------|
| Battle-tested, widely deployed | HTTPS interception requires `ssl_bump` (MITM CA) or `CONNECT` tunnel with SNI peek |
| Rich ACL language (domain, time-of-day, src IP) | Another daemon to operate, monitor, restart |
| Caching reduces bandwidth and speeds up repeat builds | Configuration is complex and error-prone |
| Mature logging (access.log) | Certificate management for MITM mode |
| Supports CONNECT tunneling with SNI-based allowlisting (no MITM needed) | Squid memory usage can spike under load |

### Option B: Custom Go Forward Proxy

**How it works**: Write a minimal HTTP CONNECT proxy in Go, embedded in the `ah` binary. It reads the allowlist from config, accepts CONNECT requests, validates the target domain against the allowlist using SNI/Host header, and pipes bytes.

| Pros | Cons |
|------|------|
| Single binary -- no new daemon | Must implement and maintain proxy logic ourselves |
| Allowlist config lives in the same place as everything else | No caching (every build re-downloads) |
| Trivial to add structured JSON audit logging | Must handle edge cases (WebSocket upgrade, chunked encoding, connection reuse) |
| ~200 lines of Go for a basic CONNECT proxy | Less battle-tested than Squid |
| Direct integration with build lifecycle (start/stop per build) | HTTPS inspection harder (same MITM problem) |

### Option C: BuildKit / Docker Network-Level Restriction

**How it works**: Use Docker's built-in `--network` flag on the build step to attach the build container to a restricted Docker network with iptables rules that only allow traffic to allowlisted IPs.

| Pros | Cons |
|------|------|
| No proxy at all -- kernel-level enforcement | IP-based, not domain-based (CDN IPs change frequently) |
| Zero additional processes | Requires maintaining IP ranges for npm, PyPI, crates.io, etc. |
| Lowest latency (no proxy hop) | No caching |
| Works for non-HTTP protocols too | No audit logging without extra tooling (iptables LOG or nftables) |
| | Nixpacks controls the `docker build` invocation, so passing `--network` requires patching Nixpacks or wrapping the Docker socket |

---

## Recommendation: Squid Forward Proxy (Option A)

**Squid with CONNECT tunneling and SNI-based allowlisting** is the recommended approach. Here is the rationale:

1. **Domain-based filtering is essential.** CDN IP ranges for npm, PyPI, and crates.io change regularly. An IP-based allowlist (Option C) would break builds silently and require constant maintenance. Squid's `ssl_bump peek` mode reads the SNI from the TLS ClientHello without performing full MITM, so we can filter on domain name without injecting a CA certificate.

2. **Operational maturity.** Squid has 30 years of production deployment history. Edge cases around HTTP/1.1 keep-alive, chunked transfers, CONNECT tunneling, and large responses are already solved. A custom Go proxy (Option B) would need to replicate all of this.

3. **Caching saves time and bandwidth.** Repeat builds of the same stack re-download the same packages. Squid's object cache can serve `registry.npmjs.org` tarballs, PyPI wheels, and Go module zips from local disk. This reduces build times and external bandwidth usage significantly.

4. **Audit logging is built in.** Squid's `access.log` provides timestamp, source IP, method, URL, status code, bytes transferred, and response time -- all the fields we need. We parse this into structured JSON for our audit pipeline.

5. **Manageable operational overhead.** Squid runs as a single process under systemd. It has clear resource limits (cache size, max connections). The configuration, while verbose, is well-documented and deterministic.

**Why not Option B (Custom Go Proxy)?** While appealing for its single-binary simplicity, the long tail of HTTP proxy edge cases (connection pooling, `Expect: 100-continue`, proxy auth, chunked encoding, timeout handling) makes a from-scratch implementation risky. The caching advantage of Squid alone justifies the operational cost.

**Why not Option C (Docker Network)?** IP-based filtering is fundamentally fragile for cloud-hosted registries. npm uses Cloudflare, PyPI uses Fastly, Go uses Google Cloud CDN. Their IP ranges overlap with millions of other services, making meaningful allowlisting impossible.

---

## Required Domains Per Language Stack

### Node.js / npm

| Domain | Purpose |
|--------|---------|
| `registry.npmjs.org` | npm package metadata and tarballs |
| `*.npmjs.org` | CDN shards for package downloads |
| `*.npmjs.com` | Alternate CDN domain |

### Python / pip

| Domain | Purpose |
|--------|---------|
| `pypi.org` | Package metadata (JSON API) |
| `files.pythonhosted.org` | Package downloads (wheels, sdists) |

### Go / go modules

| Domain | Purpose |
|--------|---------|
| `proxy.golang.org` | Module proxy (default GOPROXY) |
| `sum.golang.org` | Checksum database |
| `storage.googleapis.com` | Module storage backend |

### Rust / cargo

| Domain | Purpose |
|--------|---------|
| `crates.io` | Crate metadata |
| `index.crates.io` | Sparse registry index |
| `static.crates.io` | Crate downloads |
| `github.com` | Git-based crate dependencies (common) |

### Shared / Nixpacks Infrastructure

| Domain | Purpose |
|--------|---------|
| `github.com` | Git clone source (tenant repos) |
| `*.github.com` | GitHub API, raw content |
| `*.githubusercontent.com` | GitHub raw file hosting |
| `gitlab.com` | Alternate git source |
| `bitbucket.org` | Alternate git source |
| `cache.nixos.org` | Nix binary cache (Nixpacks base layers) |
| `channels.nixos.org` | Nix channel metadata |
| `releases.nixos.org` | Nix release tarballs |
| `nixos.org` | Nix infrastructure |
| `dl-cdn.alpinelinux.org` | Alpine base image packages |
| `deb.debian.org` | Debian base image packages |
| `security.debian.org` | Debian security updates |
| `archive.ubuntu.com` | Ubuntu base image packages |
| `security.ubuntu.com` | Ubuntu security updates |

---

## Sample Allowlist: Node.js Stack

```
# /etc/squid/allowlist-node.txt
# Node.js / npm build egress allowlist

# --- npm registry ---
.npmjs.org
.npmjs.com

# --- Git sources ---
.github.com
.githubusercontent.com
.gitlab.com
.bitbucket.org

# --- Nixpacks / Nix ---
cache.nixos.org
channels.nixos.org
releases.nixos.org
nixos.org

# --- Base image OS packages ---
dl-cdn.alpinelinux.org
deb.debian.org
security.debian.org
archive.ubuntu.com
security.ubuntu.com
```

**Squid ACL configuration (excerpt):**

```squid
# /etc/squid/squid.conf (build proxy)

acl build_allowlist dstdomain "/etc/squid/allowlist-node.txt"

# For HTTPS: peek at SNI, allow only allowlisted domains
acl step1 at_step SslBump1
ssl_bump peek step1
ssl_bump splice build_allowlist
ssl_bump terminate all

# For HTTP: straightforward domain check
http_access allow CONNECT build_allowlist
http_access deny all
```

---

## Environment / Config Shape for Build Jobs

The proxy is injected via standard environment variables in `Builder.nixpacksBuild()`:

```go
// internal/builder/builder.go -- nixpacksBuild()
cmd.Env = []string{
    "HOME=/tmp",
    "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
    "DOCKER_HOST=unix:///var/run/docker.sock",
    "NIXPACKS_NO_CACHE=1",
    "DOCKER_CONFIG=/tmp/.docker",

    // Build egress proxy
    "HTTP_PROXY=http://127.0.0.1:3128",
    "HTTPS_PROXY=http://127.0.0.1:3128",
    "NO_PROXY=127.0.0.1,localhost,127.0.0.1:5000",
    "http_proxy=http://127.0.0.1:3128",
    "https_proxy=http://127.0.0.1:3128",
    "no_proxy=127.0.0.1,localhost,127.0.0.1:5000",
}
```

Notes:
- Both upper and lowercase variants are set because different tools respect different cases (`curl` uses lowercase, `wget` uses uppercase, Go uses uppercase, Python `requests` checks both).
- `NO_PROXY` includes `127.0.0.1:5000` so the local registry push step is never proxied.
- Squid listens on `127.0.0.1:3128` (loopback only -- not exposed to containers at runtime, only during builds).
- Nixpacks propagates these env vars into the Docker build context via `--build-arg`.

### Configuration File

```toml
# /etc/ah/build-proxy.toml

[proxy]
enabled = true
listen = "127.0.0.1:3128"
cache_dir = "/var/cache/squid"
cache_size_mb = 2048
max_object_size_mb = 256

[allowlist]
# Loaded at Squid start; changes require `systemctl reload squid`
node = "/etc/squid/allowlist-node.txt"
python = "/etc/squid/allowlist-python.txt"
go = "/etc/squid/allowlist-go.txt"
rust = "/etc/squid/allowlist-rust.txt"
shared = "/etc/squid/allowlist-shared.txt"

[audit]
log_path = "/var/log/ah/build-egress.jsonl"
log_rotation = "daily"
retention_days = 90
```

---

## Audit Logging

### Required Fields

| Field | Type | Description |
|-------|------|-------------|
| `timestamp` | ISO 8601 | When the request was made |
| `build_id` | string | The build that generated this request |
| `tenant_id` | string | Tenant that owns the build |
| `service_id` | string | Service being built |
| `method` | string | HTTP method (GET, CONNECT) |
| `url` | string | Full request URL |
| `domain` | string | Target domain extracted from URL/SNI |
| `status_code` | int | HTTP response status (200, 403, 502, etc.) |
| `bytes_sent` | int | Response body size in bytes |
| `duration_ms` | int | Request duration in milliseconds |
| `action` | string | `allowed` or `denied` |
| `allowlist_rule` | string | Which allowlist file matched (or `none`) |
| `source_ip` | string | Source IP of the request (always 127.0.0.1 for local proxy) |

### Example Audit Log Line

```json
{
  "timestamp": "2026-03-20T14:32:07.123Z",
  "build_id": "bld_a1b2c3d4e5f6",
  "tenant_id": "tnt_x9y8z7w6",
  "service_id": "svc_m3n4o5p6",
  "method": "CONNECT",
  "url": "registry.npmjs.org:443",
  "domain": "registry.npmjs.org",
  "status_code": 200,
  "bytes_sent": 1482937,
  "duration_ms": 342,
  "action": "allowed",
  "allowlist_rule": "/etc/squid/allowlist-node.txt",
  "source_ip": "127.0.0.1"
}
```

### Implementation

Squid's native `access.log` uses a configurable `logformat` directive. We define a JSON format:

```squid
logformat ah_json {"timestamp":"%{%Y-%m-%dT%H:%M:%S}tl.%03tuZ","method":"%rm","url":"%ru","domain":"%rd","status_code":%>Hs,"bytes_sent":%<st,"duration_ms":%<tt,"action":"%Ss","source_ip":"%>a"}
access_log stdio:/var/log/squid/access-json.log ah_json
```

A sidecar log processor (or a cron job) enriches each line with `build_id`, `tenant_id`, and `service_id` by correlating timestamps with active build records from the database. Alternatively, the builder can tag each build's proxy requests by using a unique proxy username per build (Squid supports this via `proxy_auth`), which embeds the build ID directly into the log line.

---

## What Breaks If the Allowlist Is Too Small?

### Immediate build failures

- **Missing registry domain**: Builds fail with `ETIMEDOUT` or `403 Forbidden` from Squid. Error messages from package managers are often cryptic (e.g., npm: `ECONNREFUSED`, pip: `Could not find a version that satisfies the requirement`).
- **Missing CDN shard**: Some registries use CDN subdomains that rotate or change (e.g., `registry.npmjs.org` may redirect to a Cloudflare edge). If the CDN domain is not allowlisted, downloads hang or fail.
- **Missing Nix cache domain**: Nixpacks base layer downloads fail, causing every build to fail regardless of language stack. This is a total outage for the build system.

### Subtle / delayed failures

- **Git submodule fetches**: If a repo uses git submodules from a domain not in the allowlist, the clone succeeds but the submodule fetch fails. The build may partially succeed with missing code, producing a broken image.
- **Post-install scripts**: npm `postinstall` and Python `setup.py` may fetch additional dependencies from arbitrary URLs. Blocking these may break legitimate packages (e.g., `node-sass` downloads prebuilt binaries from GitHub Releases, `grpcio` downloads prebuilt wheels from Google Storage).
- **Private registries**: Tenants using private npm registries (e.g., GitHub Packages, Artifactory) will see builds fail. This requires a mechanism for tenants to request additional domains (see Rollout Plan below).
- **Certificate/key fetching**: Some build steps download signing keys or certificates from vendor domains (e.g., `keys.openpgp.org`, `keyserver.ubuntu.com`).

### Operational impact

- **Support burden increases**: Every new package that fetches from an unknown domain generates a support ticket. The allowlist must be maintained actively.
- **False sense of security**: If the allowlist is too permissive to avoid breakage (e.g., `*`), it provides no security value while adding operational complexity.
- **Build time regression**: If caching is disabled or the proxy adds significant latency, build times increase, degrading the tenant experience.

### Mitigation

- Start in **audit-only mode** (log but do not block) to discover the full set of domains builds actually need.
- Provide a clear error message when a request is denied: inject a custom Squid error page that says "Build egress blocked: domain X is not in the allowlist. Contact support to request access."
- Maintain a tenant-facing status page or API endpoint listing current allowlisted domains.

---

## Rollout Plan

### Phase 1: Audit Mode (Week 1-2)

1. Install Squid on the production server (`apt install squid`).
2. Configure Squid as a forward proxy on `127.0.0.1:3128` with **no domain restrictions** (allow all).
3. Enable JSON audit logging.
4. Inject `HTTP_PROXY`/`HTTPS_PROXY` into `nixpacksBuild()` environment.
5. Monitor: confirm builds still pass, collect domain access logs.
6. Analyze logs to validate the proposed allowlist covers all observed domains.

**Rollback**: Remove `HTTP_PROXY`/`HTTPS_PROXY` from the build environment. Builds revert to direct internet access immediately.

### Phase 2: Warn Mode (Week 3-4)

1. Enable the domain allowlist in Squid.
2. Configure Squid to **log but not block** requests to non-allowlisted domains (use `http_access allow all` but tag non-matching requests in the log).
3. Alert on any non-allowlisted domain access. Investigate each one:
   - If legitimate, add to the allowlist.
   - If suspicious, document for the security log.
4. Iterate until zero non-allowlisted domains appear in a 7-day window.

**Rollback**: Disable the allowlist ACLs. Same as Phase 1.

### Phase 3: Enforce Mode (Week 5+)

1. Switch Squid to **deny** mode: requests to non-allowlisted domains return `403`.
2. Configure the Squid deny page to include actionable error text (domain name, build ID, support contact).
3. Monitor build success rates. If failure rate exceeds 5%, immediately revert to Warn Mode.
4. Add a tenant-facing API: `GET /v1/build-egress/domains` returns the current allowlist.

**Rollback**: Switch Squid back to warn mode (change `http_access deny` to `http_access allow` with logging).

### Phase 4: Tenant Customization (Future)

1. Allow tenants to request additional domains via `POST /v1/build-egress/requests`.
2. Requests go into a review queue (manual or automated approval).
3. Approved domains are added to a per-tenant supplemental allowlist.
4. Squid reloads its config on approval (`squid -k reconfigure`).

---

## Squid Deployment Details

### systemd Unit

```ini
# /etc/systemd/system/ah-build-proxy.service
[Unit]
Description=AH Build Egress Proxy (Squid)
After=network.target
Before=ah.service

[Service]
Type=simple
ExecStart=/usr/sbin/squid --foreground -f /etc/squid/squid-ah.conf
ExecReload=/usr/sbin/squid -k reconfigure -f /etc/squid/squid-ah.conf
Restart=on-failure
RestartSec=5
LimitNOFILE=65536

# Hardening
ProtectSystem=strict
ReadWritePaths=/var/cache/squid /var/log/squid /var/run/squid
PrivateTmp=true
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
```

### Resource Allocation

| Resource | Value | Rationale |
|----------|-------|-----------|
| Cache disk | 2 GB | Sufficient for npm/PyPI/Go module caching across builds |
| Max object size | 256 MB | Some Go modules and Python wheels are large |
| Max connections | 256 | 3 concurrent builds, each with many parallel downloads |
| Memory | ~200 MB RSS | Squid is lightweight for a caching proxy at this scale |

### Integration Point

In `internal/builder/builder.go`, the change is minimal: add 6 environment variables to the `cmd.Env` slice in `nixpacksBuild()`. No changes to `gitClone()` (git already respects `http_proxy`/`https_proxy` if set, and the proxy covers git traffic as well). No changes to `pushImage()` (local registry is in `NO_PROXY`).

---

## Open Questions

1. **Per-stack or single allowlist?** The simplest approach is a single merged allowlist for all stacks. Per-stack allowlists are more secure but require knowing the detected language before the build starts. Nixpacks auto-detects the language, so we would need to run `nixpacks plan` first, parse the output, then select the appropriate allowlist. This is a future optimization.

2. **Private npm/PyPI registries?** Tenants using private registries will need a way to add custom domains. This is deferred to Phase 4 but should be considered in the Squid config structure (per-tenant allowlist files).

3. **DNS resolution?** Squid resolves domains itself. If we want to prevent DNS-based exfiltration (e.g., `data.evil.com`), we need to also restrict DNS to known resolvers. This is out of scope for the initial implementation but noted for future hardening.
