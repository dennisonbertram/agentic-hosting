# Security Spike: Tenant-to-Traefik Reachability

**Issue**: [#50](https://github.com/dennisonbertram/agentic-hosting/issues/50)
**Date**: 2026-03-20
**Status**: Confirmed reachable; mitigation documented

---

## 1. Current Network Topology

```
                          Internet
                             |
                      [ Host: ports 80, 443 ]
                             |
                     +-------+-------+
                     | paas-traefik  |   (Docker container, runtime: runc)
                     |  - :80  HTTP  |   (redirects to HTTPS)
                     |  - :443 HTTPS |   (TLS termination)
                     +---+---+---+---+
                         |   |   |
            +------------+   |   +------------+
            |                |                |
    +-------+-------+ +-----+-------+ +------+--------+
    | ah-tenant-AAA | | ah-tenant-BBB | | ah-tenant-CCC |
    | (bridge,       | | (bridge,       | | (bridge,       |
    |  internal,     | |  internal,     | |  internal,     |
    |  ICC=false)    | |  ICC=false)    | |  ICC=false)    |
    +-------+-------+ +-------+-------+ +-------+--------+
            |                 |                  |
    +-------+-------+ +------+--------+ +-------+--------+
    | ah-AAA-svc1   | | ah-BBB-svc1   | | ah-CCC-svc1    |
    | (gVisor/runsc, | | (gVisor/runsc, | | (gVisor/runsc,  |
    |  CapDrop ALL,  | |  CapDrop ALL,  | |  CapDrop ALL,   |
    |  RO rootfs)    | |  RO rootfs)    | |  RO rootfs)     |
    +----------------+ +---------------+ +-----------------+
```

**Key facts from the codebase** (`internal/docker/client.go`):

- Each tenant network is created with `Internal: true` and `ICC=false`
  (lines 96-101 of `client.go`).
- `Internal: true` means no default gateway to the host network -- containers
  cannot reach the internet.
- `ICC=false` means containers on the same bridge cannot reach each other
  directly (Docker inserts iptables DROP rules for inter-container traffic).
- Traefik (`paas-traefik`) is connected to **every** tenant network so it can
  route HTTP traffic to tenant containers (lines 214-224 of `client.go`).
- Tenant containers are hardcoded with `traefik.enable=false` (line 195 of
  `client.go`), preventing Traefik's Docker provider from discovering them.
- Routing is handled by the Traefik **file provider**: each service gets a
  YAML file in `/etc/traefik/dynamic/` written by `writeTraefikRoute()`
  (`internal/services/services.go`, lines 185-227).

---

## 2. Threat Model

Because Traefik joins every tenant bridge, it has an IP address reachable from
every tenant container on that bridge. Two concerns:

| # | Threat | Description |
|---|--------|-------------|
| T1 | **Tenant reaches Traefik entrypoints** | A tenant container sends TCP traffic to Traefik's container IP on port 80 or 443, reaching the reverse proxy directly from inside the tenant network. |
| T2 | **Cross-tenant routing via Host header** | After reaching Traefik, the tenant sends a crafted `Host:` header matching another tenant's subdomain (e.g., `Host: victim-app.apps.example.com`). Traefik routes the request to the victim's container. |

---

## 3. Reproduction: T1 -- Tenant Container Reaches Traefik

### Prerequisites

- A running agentic-hosting instance with at least one deployed service.
- SSH access to the server: `ssh -i ~/.ssh/id_hetzner_claudeops root@65.21.67.254`

### Steps

```bash
# 1. Identify a running tenant container
docker ps --filter "label=ah.tenant" --format "{{.ID}} {{.Names}}" | head -1
# Example output: a1b2c3d4e5f6  ah-TENANT1-svc1

# 2. Identify Traefik's IP on the tenant's network
TENANT_NET=$(docker inspect ah-TENANT1-svc1 \
  --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' \
  | tr ' ' '\n' | grep ah-tenant)
TRAEFIK_IP=$(docker inspect paas-traefik \
  --format "{{(index .NetworkSettings.Networks \"$TENANT_NET\").IPAddress}}")
echo "Traefik IP on $TENANT_NET: $TRAEFIK_IP"
# Example output: Traefik IP on ah-tenant-TENANT1: 172.19.0.2

# 3. Exec into the tenant container and attempt TCP connection to Traefik
#    Note: gVisor (runsc) blocks docker exec by default. To reproduce,
#    temporarily start a test container on the same tenant network with
#    the default runc runtime:
docker run --rm -it --network "$TENANT_NET" curlimages/curl:latest \
  curl -v --max-time 5 "http://$TRAEFIK_IP:80/"
```

### Expected result (WITHOUT mitigation)

The `curl` command **succeeds**. Traefik responds with its default HTTP-to-HTTPS
redirect (301) or, if the `Host:` header matches a configured route, serves the
response from the backend. This confirms **T1: tenant containers can reach
Traefik on ports 80 and 443 over the shared bridge**.

### Why gVisor does not prevent this

gVisor intercepts syscalls at the container boundary but does **not** filter
network traffic. TCP connections to other containers on the same Docker bridge
are standard socket operations that gVisor passes through to the network stack.
gVisor's isolation is at the kernel-API level, not the network level.

---

## 4. Reproduction: T2 -- Cross-Tenant Host Header Routing

### Prerequisites

- Two tenants (Tenant A, Tenant B) each with a deployed service.
- Both services have Traefik file-provider routes (YAML files in
  `/etc/traefik/dynamic/`).

### Steps

```bash
# 1. Confirm both services have Traefik routes
ls /etc/traefik/dynamic/
# Example: svc-AAA.yml  svc-BBB.yml

# 2. Get Tenant B's hostname from the route file
cat /etc/traefik/dynamic/svc-BBB.yml
# Shows: rule: "Host(`tenant-b-app.apps.example.com`)"
# And:   url: "http://ah-TENANTB-svcBBB:8000"

# 3. From Tenant A's network, reach Traefik with Tenant B's Host header
TENANT_A_NET="ah-tenant-TENANTA"
TRAEFIK_IP=$(docker inspect paas-traefik \
  --format "{{(index .NetworkSettings.Networks \"$TENANT_A_NET\").IPAddress}}")

docker run --rm --network "$TENANT_A_NET" curlimages/curl:latest \
  curl -v --max-time 5 \
  -H "Host: tenant-b-app.apps.example.com" \
  "http://$TRAEFIK_IP:80/"
```

### Expected result (WITHOUT mitigation)

Traefik receives the request, matches the `Host:` header to Tenant B's route,
and **forwards the request to Tenant B's container**. The response from Tenant
B's application is returned to the caller on Tenant A's network.

This confirms **T2: a tenant can route requests to another tenant's backend
by sending a crafted Host header to Traefik**.

### Why this works

Traefik is connected to **all** tenant networks simultaneously. When Traefik
receives a request on any network interface, it resolves the backend container
name via Docker DNS. Because Traefik is a member of every tenant network,
Docker DNS resolution succeeds for any tenant's container name. The file-provider
route simply specifies `url: "http://ah-{tenantID}-{serviceID}:{port}"` -- Traefik
can reach any of these because it shares a bridge with each of them.

---

## 5. Risk Assessment

| Factor | Assessment |
|--------|------------|
| **Exploitability** | Low-Medium. Requires the attacker to (a) achieve code execution inside their gVisor-sandboxed container, (b) discover Traefik's IP on the bridge (predictable -- usually .2 or .3), and (c) know or guess the victim's `Host:` header (subdomain is the DNS label chosen at service creation, publicly visible in DNS). |
| **Impact** | Medium. The attacker can send HTTP requests to another tenant's application as if they were an external user. They cannot bypass application-level authentication, but they can reach endpoints that may not be intended for public access if the tenant relies on network isolation. No direct container escape or privilege escalation. |
| **Existing mitigations** | gVisor (syscall sandbox), CapDrop ALL, no-new-privileges, read-only rootfs, PidsLimit, ICC=false (blocks container-to-container, but NOT container-to-Traefik). `Internal: true` blocks internet egress but does not prevent intra-bridge communication with Traefik. |
| **What ICC=false does NOT do** | ICC (Inter-Container Communication) disables direct container-to-container traffic. However, Traefik is not "just another container" from iptables' perspective -- it has its own iptables rules for port publishing. Traffic from a container to Traefik's bridge IP is routed through the bridge, not through ICC rules. |

---

## 6. Recommended Short-Term Mitigation: iptables DOCKER-USER Rules

The least invasive, operator-side control that requires no code changes, no
per-tenant Traefik deployment, and no network architecture rewrite.

### What to apply

```bash
# Block new TCP connections to Traefik's listening ports within Docker networks.
# DOCKER-USER is evaluated before Docker's own forwarding rules and survives
# container restarts. These rules apply to all Docker bridge traffic.

# Block port 80 (HTTP)
iptables -I DOCKER-USER -m conntrack --ctstate NEW -p tcp --dport 80 -j DROP

# Block port 443 (HTTPS)
iptables -I DOCKER-USER -m conntrack --ctstate NEW -p tcp --dport 443 -j DROP
```

### Why this works

- The `DOCKER-USER` chain is a Docker-managed hook specifically designed for
  operator-inserted rules. Docker never overwrites rules in this chain, and
  they persist across container restarts and Docker daemon restarts.
- `-m conntrack --ctstate NEW` matches only new connection attempts. Established
  and related traffic (such as Traefik's outbound connections to backend
  containers) is not affected. This means Traefik can still **initiate**
  connections to tenant containers for proxying, but tenant containers cannot
  **initiate** connections to Traefik.
- `-j DROP` silently discards the packet. Use `-j REJECT` instead if you prefer
  tenant code to receive an immediate connection-refused error rather than a
  timeout.

### Make persistent across reboots

```bash
apt-get install -y iptables-persistent
netfilter-persistent save
```

### Verification after applying

```bash
# Re-run the T1 repro from Section 3:
docker run --rm --network "$TENANT_NET" curlimages/curl:latest \
  curl -v --max-time 5 "http://$TRAEFIK_IP:80/"

# Expected: connection times out (DROP) or connection refused (REJECT).
# Traefik is no longer reachable from tenant containers.
```

### Scope and limitations

- These rules block **all** Docker-bridged containers from initiating
  connections to ports 80 and 443. This is acceptable because:
  - Tenant containers are on `Internal: true` networks (no internet access
    anyway).
  - The only legitimate traffic to ports 80/443 comes from external clients
    via the host's published ports, which bypass DOCKER-USER (they enter
    through the host's INPUT/FORWARD chains).
- If Traefik's entrypoint ports change, the iptables rules must be updated
  to match.
- This does not block tenant containers from reaching Traefik on the
  dashboard port (8090) if Traefik is configured with `api.insecure: true`.
  Consider adding a rule for port 8090 as well, or disabling the insecure
  dashboard in production:

  ```bash
  iptables -I DOCKER-USER -m conntrack --ctstate NEW -p tcp --dport 8090 -j DROP
  ```

---

## 7. Long-Term Considerations (Out of Scope for This Spike)

These are noted for future reference but are explicitly **not** recommended as
short-term actions:

1. **Dedicated Traefik network per tenant** -- Each tenant gets its own Traefik
   sidecar or a Traefik instance that only joins one tenant network. Eliminates
   cross-tenant routing entirely but significantly increases operational
   complexity.

2. **Network policy enforcement** -- Use a CNI plugin (Calico, Cilium) instead
   of Docker's default bridge driver for fine-grained network policies. Requires
   a container orchestrator (Kubernetes) or standalone CNI setup.

3. **Traefik on host network only** -- Run Traefik on the host network and
   publish tenant container ports to localhost. Removes Traefik from tenant
   bridges entirely but requires port allocation management and changes to the
   routing architecture.

---

## References

- `internal/docker/client.go` -- network creation (lines 82-119), RunContainer
  with Traefik connection (lines 137-232)
- `internal/services/services.go` -- writeTraefikRoute (lines 185-227)
- `.claude/skills/agentic-hosting/references/server-setup.md` -- "Security:
  Isolating Tenant Containers from Traefik" section
- `deploy/traefik/traefik.yml` -- Traefik static configuration
