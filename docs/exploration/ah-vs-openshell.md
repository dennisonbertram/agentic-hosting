# ah vs OpenShell / NemoClaw

A factual comparison to help you pick the right tool — or use both.

## What ah optimizes for

ah is a single Go binary that turns any Linux server into a PaaS operated entirely by AI agents via HTTP API.

- **Deploy-and-forget**: `POST /v1/services` with a git URL or Docker image. Nixpacks auto-detects the stack, builds, and deploys. No Dockerfile required.
- **Zero-config isolation**: Every container runs under gVisor (runsc) with all capabilities dropped, read-only rootfs, and per-tenant network segmentation. No configuration needed — it's the default.
- **Circuit breaker for autonomous loops**: Agents can crash-loop a service at machine speed. ah detects 5 crashes in 10 minutes and pauses automatically, with exponential backoff recovery (30m, 1h, 4h).
- **Idempotent by design**: Every write endpoint accepts an idempotency key. Retries from flaky LLM tool calls produce the same result, not duplicate deployments.
- **Cheap to run**: A single $4-54/mo Hetzner server runs 1-50+ services. No per-deploy fees, no platform markup.

**Target audience**: AI agent developers who need their agents to deploy and operate stateless web services, APIs, and databases without human intervention.

## Where OpenShell / NemoClaw are stronger

OpenShell and NemoClaw focus on interactive compute environments and GPU workloads — a different problem space.

- **GPU scheduling**: First-class support for GPU workloads, model inference, and training jobs. ah has no GPU awareness.
- **Interactive sessions**: Designed for agents that need a persistent shell, file system, or REPL. ah is built for stateless request/response services.
- **Policy engines**: Fine-grained policy-based access control for compute resources. ah uses simpler tenant-scoped API keys and rate limits.
- **Multi-node orchestration**: Designed to span clusters. ah runs on a single server by design.

## Where they complement each other

The two systems solve different parts of the AI infrastructure stack:

| Use case | Best fit |
|----------|----------|
| Deploy a web app or API from git | ah |
| Run a GPU inference job | OpenShell |
| Provision a managed Postgres/Redis | ah |
| Give an agent an interactive shell | OpenShell |
| Auto-recover crashed services with circuit breakers | ah |
| Schedule training workloads across a cluster | OpenShell |
| Operate infrastructure via pure HTTP API | ah |
| Enforce compute policies across teams | OpenShell |

A practical setup: ah runs your stateless services (APIs, frontends, databases) on a cheap bare-metal server, while OpenShell handles GPU workloads and interactive compute on a separate cluster. The agent calls whichever API matches the task.

## Who should use ah right now

ah is the right choice if:

- Your agents need to deploy web services, APIs, or databases and move on
- You want a single binary with no Kubernetes, no cloud vendor, no dashboard dependency
- You need isolation guarantees (gVisor) without configuring security policies
- Your workloads are CPU-bound and stateless
- You want predictable, flat-rate infrastructure costs

ah is **not** the right choice if your primary need is GPU compute, interactive sessions, or multi-node cluster orchestration. Use OpenShell or NemoClaw for those.
