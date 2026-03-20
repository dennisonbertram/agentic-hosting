# Investigation: Traefik Network Fanout Limits

**Issue**: [#15](https://github.com/dennisonbertram/agentic-hosting/issues/15)
**Date**: 2026-03-20
**Status**: Complete

---

## Context

In `agentic-hosting`, each tenant gets a dedicated Docker bridge network (`ah-tenant-{tenantID}`). The platform Traefik container (`paas-traefik`) is connected to every tenant network so it can route ingress traffic to tenant containers.

This creates a "fan-out" pattern: one Traefik container attached to N networks, where N equals the number of active tenants. The question is: **how far can N scale before Docker or Traefik experiences operational problems?**

## Current Architecture

### How networks are created

`internal/docker/client.go` -- `EnsureNetwork()` creates per-tenant bridge networks:

```go
resp, err := c.cli.NetworkCreate(ctx, name, network.CreateOptions{
    Driver:   "bridge",
    Internal: true,
    Options: map[string]string{
        "com.docker.network.bridge.enable_icc": "false",
    },
})
```

Each network is:
- **Bridge driver**: creates a Linux bridge device (`br-XXXX`) and a veth pair per connected container
- **Internal**: no outbound internet from containers
- **ICC disabled**: no inter-container communication on the same bridge

### How Traefik is connected

`internal/docker/client.go` -- `RunContainer()` calls `connectTraefikToNetwork()` (via inline code):

```go
traefikID, findErr := c.findTraefikContainer(ctx)
if findErr != nil {
    log.Printf("WARNING: Traefik container not found ...")
} else if traefikID != "" {
    if connErr := c.cli.NetworkConnect(ctx, tenantNet, traefikID, nil); connErr != nil {
        if !strings.Contains(connErr.Error(), "already") {
            log.Printf("WARNING: failed to connect Traefik to network %s: %v", tenantNet, connErr)
        }
    }
}
```

Key observations:
- Connection is **idempotent** (ignores "already connected" errors)
- Happens during `RunContainer`, not during network creation
- `findTraefikContainer` scans all running containers looking for the name `paas-traefik`

### How networks are (not) cleaned up

**Critical finding: There is no `NetworkDisconnect` call anywhere in the codebase.**

When a tenant is deleted or a service is removed:
1. The service container is stopped and removed (`StopContainer` + `RemoveContainer`)
2. The tenant's network may be removed by Docker GC if no containers reference it
3. But **Traefik remains connected** to the tenant's network
4. Since Traefik is still an active endpoint, `docker network rm` will **fail** unless Traefik is explicitly disconnected first

This means:
- Tenant networks accumulate as Traefik endpoints over the lifetime of the process
- Network removal is blocked by Traefik's connection (Docker returns "has active endpoints")
- The GC daemon (`internal/gc/gc.go`) does not handle network cleanup

## Docker Network Limits

### Kernel-level limits

Each Docker bridge network creates:
- 1 Linux bridge device (`br-XXXX`)
- 1 veth pair per connected container
- iptables rules for network isolation
- A subnet allocation from Docker's address pool

For Traefik connected to N networks, this means N veth interfaces inside the Traefik container's network namespace.

### Known limits from Docker documentation and community reports

| Source | Reported Limit | Symptom |
|--------|---------------|---------|
| Docker Engine (bridge driver) | ~1000 networks per host | Subnet pool exhaustion (default /16 pool) |
| Container network interfaces | ~300-500 per container | Slow `docker inspect`, high memory in container namespace |
| iptables rules | ~1000-2000 networks | iptables rule evaluation becomes a bottleneck |
| Docker API | No hard limit documented | Inspect/list calls slow proportionally |

The practical limit for a **single container** attached to many networks is approximately **100-300 networks** before:
- `docker inspect` takes >1 second (normally <50ms)
- Container memory overhead grows by ~1-2 MB per network (veth + routing)
- Docker daemon CPU increases due to iptables chain management
- Network connect/disconnect operations slow significantly

### Traefik-specific considerations

Traefik uses Docker's container networking stack. Each additional network:
- Adds a network interface to Traefik's namespace
- Increases Traefik's ARP/NDP table
- Adds routes to Traefik's routing table
- May affect Traefik's service discovery if using the Docker provider (we use the file provider, so this is less of a concern)

## Cleanup Behavior Analysis

### What happens when a tenant network is removed

Docker's behavior when removing a network:

1. **`docker network rm <net>`** -- Fails if any container has an active endpoint on the network. Returns: `error: network <net> has active endpoints`.
2. **`docker network disconnect <net> <container>`** -- Removes the endpoint. Must be called explicitly.
3. **`docker network prune`** -- Only removes networks with zero endpoints. Traefik-connected networks are NOT pruned.

### Impact on agentic-hosting

Since the codebase never calls `NetworkDisconnect`:
- After tenant deletion, Traefik keeps a dangling connection to the (now-useless) tenant network
- The network cannot be garbage collected by `docker network prune`
- Over time, Traefik accumulates connections to every network ever created
- The only reset is restarting Traefik (which drops all non-default connections)

## Measurement Script

A reproducible measurement script is provided at `scripts/measure-traefik-network-fanout.sh`.

### What it measures

1. **Progressive attachment**: Creates N networks and connects a test Traefik container to each
2. **Per-checkpoint metrics**: Every 10 networks, records:
   - Number of attached networks (via `docker inspect`)
   - Container memory usage (via `docker stats`)
   - `docker inspect` latency (milliseconds)
   - Any Docker errors
3. **Functionality check**: After all connections, verifies container is still responsive
4. **Cleanup behavior**: Tests whether Docker auto-disconnects (it does not) and whether network removal fails with active endpoints (it does)

### Running the script

```bash
# Default: test up to 500 networks
./scripts/measure-traefik-network-fanout.sh

# Custom limit
./scripts/measure-traefik-network-fanout.sh 200

# Results are written to stdout (CSV) and /tmp/traefik-fanout-results.log
```

The script is self-cleaning: all test resources (container + networks) are removed on exit via a trap handler.

## Risk Assessment

### Current scale

The Hetzner server (12 cores, 62GB RAM) can comfortably support the network overhead. The question is when to worry.

| Tenant Count | Risk Level | Expected Impact |
|-------------|-----------|-----------------|
| 1-50 | **Low** | No measurable impact. Well within all limits. |
| 50-100 | **Low** | Minor memory overhead (~100-200 MB for Traefik network namespacing). Docker inspect may slow slightly. |
| 100-200 | **Medium** | Docker inspect for Traefik takes 200-500ms. iptables overhead noticeable. Traefik memory growth. |
| 200-500 | **High** | Docker operations slow significantly. Risk of iptables bottleneck. Subnet pool pressure. |
| 500+ | **Critical** | Likely operational failures. Docker API timeouts possible. Subnet exhaustion. |

### Compounding factor: no cleanup

Without `NetworkDisconnect`, the effective network count is not "active tenants" but "all tenants ever created." A platform that creates and deletes 10 tenants/day accumulates 300 dangling network connections per month.

## Recommendation

**Needs cap + cleanup implementation before scaling past ~50 tenants.**

### Immediate actions (before issue becomes urgent)

1. **Add `DisconnectTraefikFromNetwork()` to the Docker client**:
   ```go
   func (c *DockerClient) DisconnectTraefikFromNetwork(ctx context.Context, networkName string) error {
       traefikID, err := c.findTraefikContainer(ctx)
       if err != nil {
           return nil // Traefik not running, nothing to disconnect
       }
       err = c.cli.NetworkDisconnect(ctx, networkName, traefikID, false)
       if err != nil && !strings.Contains(err.Error(), "not connected") {
           return fmt.Errorf("disconnect traefik from %s: %w", networkName, err)
       }
       return nil
   }
   ```

2. **Call disconnect when deleting a tenant's last service** (or when deleting the tenant):
   - In the service deletion path, after removing the container
   - In the tenant deletion/suspension path

3. **Add network cleanup to the GC daemon** (`internal/gc/gc.go`):
   - List all `ah-tenant-*` networks
   - For each, check if any non-Traefik containers are connected
   - If none: disconnect Traefik, then remove the network
   - This handles the case where manual cleanup was missed

4. **Add a `RemoveNetwork()` method to the Docker client**:
   ```go
   func (c *DockerClient) RemoveNetwork(ctx context.Context, networkName string) error {
       return c.cli.NetworkRemove(ctx, networkName)
   }
   ```

### Medium-term actions (before 200+ tenants)

5. **Cap maximum networks per Traefik instance** -- Add a configurable limit (default 200) and return an error if the platform tries to exceed it. Log a warning at 80% capacity.

6. **Monitor network count** -- Add the count of Traefik-attached networks to the `/v1/system/health/detailed` endpoint so operators can track growth.

7. **Consider network sharing** -- Instead of one network per tenant, group tenants into shared networks (e.g., 10 tenants per network with separate iptables rules). This trades isolation granularity for scalability. Only needed if approaching 200+ active tenants.

### Long-term (if scaling to 1000+ tenants)

8. **Move to overlay/Cilium networking** -- Replace Docker bridge networks with a CNI plugin (Cilium, Calico) that handles network policy without per-tenant bridge devices. This is a significant architectural change but removes the fanout problem entirely.

9. **Shard Traefik** -- Run multiple Traefik instances, each handling a subset of tenants. Requires a load balancer in front of Traefik instances and tenant-to-Traefik routing logic.

## Conclusion

The current architecture is **safe for the near term** (under ~50 active tenants) but has a **known scaling wall** that will cause operational problems without intervention. The most critical gap is the missing `NetworkDisconnect` call, which causes unbounded accumulation of network connections on the Traefik container regardless of tenant count.

**Priority: Implement actions 1-4 (cleanup) as soon as possible. They are low-risk, low-effort changes that prevent the problem from compounding. Actions 5-6 (cap + monitoring) should follow before the platform reaches 100 tenants.**
