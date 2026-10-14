# Fresh Ubuntu host install

`scripts/bootstrap-stack.sh` provisions one fresh Ubuntu 24.04 amd64 host from this checkout. It installs the API, Docker with gVisor, Nixpacks, a loopback-only registry, a file-provider Traefik ingress, a constrained host relay, and (by default) the website container. It does not change DNS or firewall rules. Run it only on a newly provisioned host with no existing ah state.

The script stops before making changes when it sees `/var/lib/ah`, the ah environment files or units, an existing Docker daemon config, the named registry resources, or an overlapping ingress subnet. It refuses to merge an existing Docker daemon config because doing so without understanding the host's current runtime settings could break other workloads. Choose an unused private `/24` in the script if the planned `172.24.0.0/24` range overlaps host routes or Docker networks; the script will fail closed on overlap.

## Run the installer

First, provision a clean Ubuntu 24.04 amd64 server, allow SSH only from trusted operator addresses, and allow inbound TCP 80 and 443. Install a source checkout of this repository on the host. Do not place production secrets in shell history or pass them as arguments; the installer generates the master key and bootstrap token itself. Then run:

```sh
sudo scripts/bootstrap-stack.sh --domain agentic.hosting --acme-email ops@example.org
```

Replace the example email with the mailbox used for ACME notices. `--domain` must be a lowercase DNS domain. Use `--skip-website` to install only the API and service ingress. `--dry-run` checks host prerequisites and subnet collisions without changing the host. Running the installer again is intentionally refused after it has created persistent state. Do not delete `/var/lib/ah`, keys, tokens, registry data, Docker networks, or units to make a rerun pass.

The website hostnames are `agentic.hosting` and `www.agentic.hosting`. The public API remains available at `https://agentic.hosting/v1/...` for existing website clients and also at `https://api.agentic.hosting/v1/...`; tenant services use `apps.agentic.hosting`. The API route has explicit higher priority for `/v1/` paths, leaving the homepage and other website paths unchanged. Replace the example domain in the command and configure these DNS records at the DNS provider after provisioning:

| Name | Type | Value |
| --- | --- | --- |
| `@` | A | New server IPv4 address |
| `www` | A | New server IPv4 address |
| `api` | A | New server IPv4 address |
| `*.apps` | A | New server IPv4 address |

If the host has a working public IPv6 address and firewall policy, add matching AAAA records. Do not publish AAAA records for an address the host cannot serve. ACME HTTP-01 issuance needs public DNS to resolve to this host and inbound TCP 80 to reach Traefik. Until then, Traefik serves its default certificate; the API health endpoint on loopback remains available.

## Installed topology and credentials

The API listens only on `127.0.0.1:8080`. The `traefik-public` bridge uses `172.24.0.0/24`, gateway `172.24.0.1`, and the fixed Traefik address `172.24.0.2`. Traefik's file provider points the API route at `ah-api-relay:8081`, which resolves to the bridge gateway. The host `ah-api-proxy.service` listener binds only to that gateway and socat accepts connections only from Traefik's `/32` address before forwarding to `127.0.0.1:8080`. Port 8081 is not published by Docker and must not be opened in the host firewall. The API continues to trust `X-Forwarded-Proto` only from its loopback peer.

Traefik has no Docker provider, Docker socket, public dashboard, or dashboard port. Tenant containers remain on their per-tenant networks; the API joins `paas-traefik` to those networks for service reachability and writes file-provider routes. The shared `traefik-public` network contains ingress and platform containers. The registry is published on host loopback port 5000 only. The website container is optional. Docker's `runsc` runtime is configured and the application uses it for tenant containers.

The installer stores the generated master key at `/var/lib/ah/master.key` (owned by `ah`, mode `0600`) and the bootstrap token in `/etc/default/ah` (owned by root, mode `0600`). Do not copy either value into a terminal transcript or ticket. Retrieve the bootstrap token only through an approved secure access channel. The ah daemon's membership in the Docker group grants host-level Docker control; protect root access and the Docker socket accordingly. Build artifacts live under `/var/lib/ah-builds`, owned by `ah:ah-build`, mode `0710`; workers can traverse this root but cannot list or create sibling builds. Each individual build directory has mode `0770`, owned by `ah:ah-build`, and is shared only by the daemon and its worker. The daemon explicitly assigns the shared group to build artifacts; the unit files keep `RestrictSUIDSGID=yes` and do not depend on setgid mode bits. The root-owned broker accepts requests from ah for fixed `ah-build@<id>.service` units at `/run/ah-build-broker/broker.sock`; it does not read the bootstrap-token environment file.

## Verify startup

The installer checks the local API health endpoint with the trusted forwarded-protocol header and fails if the API does not return a successful response. On the host, inspect service and container state without printing the environment file:

```sh
sudo systemctl --no-pager --full status ah ah-api-proxy ah-build-broker
sudo docker ps --format 'table {{.Names}}\t{{.Status}}\t{{.Ports}}'
sudo ss -lntp
```

Expected listeners are the API on `127.0.0.1:8080`, the relay on `172.24.0.1:8081`, registry on `127.0.0.1:5000`, and public ingress on ports 80 and 443. Port 8081 must not appear on a public or wildcard address. With DNS in place and a valid certificate issued, verify the public endpoint without disabling TLS checks:

```sh
curl --fail --show-error https://api.agentic.hosting/v1/system/health
curl --fail --show-error https://agentic.hosting/
```

Before DNS is configured, do not use `curl -k` as evidence of a valid public TLS route. Verify local API health and wait for DNS before the public TLS checks. Check Traefik and relay logs with `journalctl -u ah-api-proxy` and `docker logs paas-traefik` if routing fails.

The TCP relay allowlist can be checked locally by requesting the API route through Traefik, then attempting a connection to `172.24.0.1:8081` from a different container attached to `traefik-public`; the first must work and the second must be rejected by socat's source range. Also verify from an external machine that TCP 8081 is unreachable. Do not open the relay port to make a failing check pass.

## Partial failure and recovery

The installer is deliberately one-shot and refuses a second run after it creates ah state. If it stops partway through, preserve all files and volumes. Record the failing command and inspect only the relevant service journal and nonsecret config:

```sh
sudo journalctl -u ah -u ah-api-proxy -u ah-build-broker --no-pager -n 100
sudo systemctl --no-pager status docker ah ah-api-proxy ah-build-broker
sudo docker ps -a --format 'table {{.Names}}\t{{.Status}}'
```

Resume from the first incomplete section of the script using the exact pinned values and paths shown there. Before restarting a service, validate its nonsecret config and preserve generated credentials and registry volume. Do not rerun bootstrap, delete state, or regenerate keys. If a step is unclear, stop with the partial state intact and have the host operator review the completed commands before continuing.

## Dependency pin provenance

The installer pins upstream binaries and container image digests, verifies the Go and Nixpacks downloads by SHA-256, verifies the Docker repository signing-key fingerprint, and uses signed apt repositories for Docker and gVisor. The Go version and checksum come from the official [Go download metadata](https://go.dev/dl/?mode=json). Nixpacks is pinned to its official [v1.41.0 release](https://github.com/railwayapp/nixpacks/releases/tag/v1.41.0); its checksum in the script is the SHA-256 of that release asset. Docker package versions are exact versions published by Docker's signed Ubuntu Noble repository when this installer was prepared. Traefik, Registry, and Nginx are pinned by amd64 image manifest digest. To refresh a pin, verify the new upstream release and architecture-specific artifact or manifest first, then update the script and this note together. If an apt pin is no longer available, the install fails rather than silently selecting another version.

The script's Go compiler pin is Go 1.27.2; `go.mod` declares Go 1.25.0. Keep both values under review when the repository changes its minimum Go version.

## Known limitation

The installer starts the build broker and installs its per-build worker template, but this configuration has not been exercised on Ubuntu. Do not claim git deployments work until a real sequential smoke deployment proves the build, gVisor container startup, environment execution, and database provisioning on the new host.
