---
title: "What gVisor and the container lockdown actually protect"
description: "Inspect a real agentic-hosting container to see each isolation layer, then read a plain list of what the sandbox does not protect."
slug: gvisor-and-container-lockdown
tags: [security, gvisor, isolation, docker, networking]
cover_diagram: diagrams/21-gvisor-and-container-lockdown.png
sources:
  - internal/docker/client.go
  - internal/services/deploy_image.go
  - internal/api/health.go
  - internal/api/server.go
  - internal/environments/environments.go
  - docs-site/src/content/docs/security.md
  - docs-site/src/content/docs/roadmap.md
  - docs-site/src/content/docs/troubleshooting.md
  - docs/security/tenant-to-traefik-repro.md
  - docs/security/hkdf-key-separation-plan.md
  - deploy/ah.service
  - README.md
  - cmd/ahc/service.go
  - docs/website/github-pages.md
  - cmd/ah/main.go
  - cmd/ahc/deploy.go
  - internal/services/services.go
---

# What gVisor and the container lockdown actually protect

If you run agents, you will run code you did not write, or code an agent wrote five minutes ago. I think you should know which walls stand between that code and the rest of the host, and which walls are missing. "Sandboxed" is a claim. A `docker inspect` is evidence.

In this post you will read the isolation settings off a real container, watched one of them stop a normal image from starting, and seen the network layout that keeps tenants apart. You will also have a plain list of what is not protected.

![Left: nested layers from host kernel to Docker to the gVisor sentry (runsc) to the service container process, with CapDrop ALL, no-new-privileges, read-only rootfs with tmpfs, PID and memory limits. Right: paas-traefik attached to separate ingress bridges for tenant A and tenant B, with no direct network path between them.](diagrams/21-gvisor-and-container-lockdown.png)

## Prerequisites

- A running agentic-hosting server and an API key. The public API is `https://api.agentic.hosting/v1/`. Steps 1 and 4 need shell access to the server, so they only work if you self-host. Everything else works over the API.
- `curl`, `jq`, and optionally the `ahc` CLI.

```bash
export AH_URL="https://api.agentic.hosting"   # or your own host
export AH_API_KEY="<keyID>.<secret>"          # placeholder, use your own key
```

I checked these commands against the source code. I did not run them for this post, so the output blocks show shapes, not captured output.

## Step 1: Deploy a service and inspect its container

Deploy anything. The unprivileged nginx image is a safe choice:

```bash
curl -s -X POST "$AH_URL/v1/services" \
  -H "Authorization: Bearer $AH_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"probe","image":"nginxinc/nginx-unprivileged","port":8080}'
```

On the server, find the container. Service containers are named `ah-<tenantID>-<serviceID>`:

```bash
docker ps --filter name=ah- --format 'table {{.Names}}\t{{.Status}}'
```

Then read the settings that `RunContainer` in `internal/docker/client.go` applies:

```bash
C=ah-<tenantID>-<serviceID>
docker inspect "$C" --format '
runtime:   {{.HostConfig.Runtime}}
capdrop:   {{.HostConfig.CapDrop}}
secopt:    {{.HostConfig.SecurityOpt}}
readonly:  {{.HostConfig.ReadonlyRootfs}}
pids:      {{.HostConfig.PidsLimit}}
memory:    {{.HostConfig.Memory}}
swap:      {{.HostConfig.MemorySwap}}
traefik:   {{index .Config.Labels "traefik.enable"}}
tmpfs:     {{.HostConfig.Tmpfs}}'
```

Expect this shape:

```text
runtime:   runsc
capdrop:   [ALL]
secopt:    [no-new-privileges]
readonly:  true
pids:      256
memory:    536870912
swap:      536870912
traefik:   false
tmpfs:     map[/run:... /tmp:... /var/run:... /var/tmp:...]
```

The memory value is the default of 512 MB. A tenant's quota can change it. Swap equals memory, so the container gets no swap. The tmpfs mounts are in-memory and mounted `noexec,nosuid`, sized 64 MB for `/tmp` and 16 MB for the other three.

## Step 2: What gVisor does

A normal container shares the host's Linux kernel. Every system call the container makes goes straight to that kernel, so a kernel bug reachable from a syscall is a way out.

gVisor puts a second kernel in between. Its runtime, `runsc`, runs a user-space kernel (called the sentry) that implements the Linux system call interface itself. The container's process talks to the sentry. The sentry makes a much smaller set of calls to the host kernel. For tenant code, that means a hostile or buggy program attacks gVisor's re-implementation first, not the host kernel directly. It is one extra layer, not a guarantee. gVisor has its own bugs, and it costs some syscall performance.

The server insists on it. Production `ah` exits if Docker does not list `runsc`, and the container is created with `Runtime: "runsc"`, so a missing runtime fails at create time instead of silently falling back to `runc`.

## Step 3: Watch the read-only root fail a normal image

The root filesystem is read-only, and only four paths are writable. Stock `nginx:alpine` is meant to write to its own cache and pid paths at startup, and I expect some of those to fall outside those four.

```bash
ahc deploy nginx:alpine stock --port 80
ahc service get stock
ahc service logs stock --tail 50
```

Expect the service to cycle through `crashed` rather than stay `running`, with a log line about a write failing on a read-only filesystem. I did not run this and did not check which path nginx writes to, so read the log yourself. This is the lockdown doing its job: the same image runs fine on a default Docker host.

A limit of this demo: you cannot run an arbitrary command inside a service container through the API, so I have no clean way to show a dropped capability being refused. The settings in Step 1 show `CapDrop` is `ALL`; I am not claiming a captured denial.

## Step 4: The network layout

Each tenant gets a bridge network named `ah-tenant-<tenantID>`. It is created with `Internal: true` (no route to the internet) and `com.docker.network.bridge.enable_icc=false` (containers on the same bridge cannot talk to each other).

```bash
docker network ls --filter name=ah-
docker network inspect ah-tenant-<tenantID> \
  --format '{{.Internal}} {{index .Options "com.docker.network.bridge.enable_icc"}}'
```

Expect `true false`.

Routing works differently. For a routed service, the server creates a separate network named `ah-ingress-<tenantID>-service-<serviceID>` containing only `paas-traefik` and that one workload. ICC is on there, because the proxy has to reach the workload. Traefik's Docker provider is off for these containers (`traefik.enable=false`), so a label baked into an image cannot register a route. Routes come from a file the server writes.

## Step 5: What is not protected

I would read this part slowly. I think a short, honest list of gaps is worth more than a long list of features.

- **One master key.** Service env vars and database passwords are encrypted with the same 32-byte master key on the server. API keys are hashed with a separate HMAC key kept in the `server_secrets` table. It was seeded from the master key on first start and does not change when the master key is rotated. HKDF key separation is a written plan (`docs/security/hkdf-key-separation-plan.md`), not running code. There is no per-tenant key.
- **A stolen tenant key is a stolen tenant.** It can deploy, delete, read revealed env values, fetch database connection strings, create more keys, and delete the tenant. Give each agent its own key with `POST /v1/auth/keys` and revoke it when done.
- **Sandbox environments do not enforce outbound policy.** Environment templates have an `egress_policy` field and the server copies it into an `EgressAllow` setting. I searched the code and nothing reads that value to change networking. Do not rely on it.
- **Traefik is reachable from a workload.** A workload can reach its own ingress bridge and send a Host header for another public route. That is the same as calling the public hostname, so the target app's own authentication still has to hold.
- **gVisor is not a proof.** It narrows the kernel attack surface. It does not remove it.
- **Builds are separate.** Git clone only talks to five HTTPS hosts, and the build egress proxy is a decision document, not a running feature.

## Step 6: Check gVisor from the API

The detailed health endpoint reports whether the server can run `runsc`:

```bash
curl -s -H "Authorization: Bearer $AH_API_KEY" \
  "$AH_URL/v1/system/health/detailed" | jq '{docker, gvisor}'
```

```json
{
  "docker": { "available": true, "version": "<docker version>" },
  "gvisor": { "available": true, "version": "<runsc version>" }
}
```

The server runs `runsc --version` with a 5 second timeout. If it fails, `gvisor.available` is `false`. Results are cached for 30 seconds, and `?fresh=true` shortens that to 10. The endpoint needs authentication.

## How it works

Read the diagram from the outside in. The host kernel is the thing you are protecting. Docker starts the container, but because the runtime is `runsc`, the process is actually running inside the sentry. Inside that, the container's own restrictions apply: no capabilities, no privilege gain through setuid binaries, a root it cannot write to, a cap of 256 processes, and a memory limit with no swap.

On the right, Traefik is the only component attached to every ingress bridge, and no container shares a bridge with another tenant's container, so there is no direct network path between tenants. Traefik itself is still reachable from a workload (see Step 5), so a request through the proxy to another public route remains possible.

## What to try next

- Read the [security model](https://agentic.hosting/docs/security/) and the [architecture](https://agentic.hosting/docs/architecture/) pages.
- Learn how crash loops are contained in [reconciliation](https://agentic.hosting/docs/reconciliation/) and [guards](https://agentic.hosting/docs/guards/).
- Review key handling in [auth](https://agentic.hosting/docs/auth/).
- Read the code: [github.com/dennisonbertram/agentic-hosting](https://github.com/dennisonbertram/agentic-hosting).
