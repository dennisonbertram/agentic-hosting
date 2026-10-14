# Tenant-to-Traefik Reachability

**Issue:** [#50](https://github.com/dennisonbertram/agentic-hosting/issues/50)
**Verified:** 2026-10-14
**Status:** Workload-to-proxy reachability is required for routing. Public Host-header routing remains public.

## Network layout

Tenant workloads remain on their tenant bridge (`Internal=true`, `enable_icc=false`). Each routed service and each environment with previews also joins its own internal ingress bridge with `paas-traefik`. That bridge contains only the proxy and the routed workload. The tenant bridge remains the boundary between tenant workloads; no workload ingress bridge is shared with another workload. Routes use a network-scoped alias on the ingress bridge, avoiding ambiguous DNS answers from the tenant bridge.

Docker's `Internal` setting restricts access outside the internal network. `enable_icc=false` blocks communication between endpoints on the tenant bridge. It also blocks the proxy from reaching a workload when both are attached only to that bridge, which caused the observed HTTP 502. The per-workload ingress bridge enables communication for that proxy/workload pair only. Docker's `GET /networks` response may omit endpoint membership; cleanup code must use `NetworkInspect` before treating a listed network as empty.

## Security boundary and remaining exposure

The ingress bridge prevents one workload from directly reaching another workload's private address or service port. It does not make the public reverse proxy private. A workload connected to its own ingress bridge can reach Traefik's entrypoint and can send a Host header for another configured public route. Traefik may then serve that public route, subject to the target application’s own authentication and authorization. This is equivalent to making a request to that public hostname from outside the platform; it is not a private-network authorization boundary.

Do not use Host-header routing as tenant authorization. Applications that need private endpoints must enforce authentication themselves, and platform routes must not publish private service ports. This change does not claim to block a workload from making requests to public services or from reaching Traefik's public entrypoint.

## Reproduction checks

On an isolated synthetic deployment, inspect the workload and proxy network attachments:

```bash
docker inspect <workload> --format '{{json .NetworkSettings.Networks}}'
docker inspect paas-traefik --format '{{json .NetworkSettings.Networks}}'
```

The workload and proxy should share exactly its own `ah-ingress-<tenant>-service-<service>` bridge (or the corresponding environment ingress bridge for previews). They should not share a tenant ingress bridge with another workload. Confirm backend reachability from Traefik on the workload's configured published port, and confirm a second workload cannot connect directly to the first workload's private IP and port.

To check the public Host-header limitation, send a request from the synthetic workload to Traefik's entrypoint using another configured public hostname. A response from that route demonstrates that the proxy remains a public routing surface; it does not demonstrate private backend connectivity or an authorization bypass. Do not run these checks against tenant data or change host firewall rules as part of this test.

## References

- `internal/docker/client.go` creates tenant and per-workload ingress bridges and assigns the route aliases.
- `internal/services/services.go` writes service routes and manages service ingress networks.
- `internal/environments/environments.go` writes preview routes and manages environment ingress networks.
- Docker Engine bridge networking documentation: <https://docs.docker.com/engine/network/drivers/bridge/>
