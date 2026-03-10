# agentic-paasd

An agentic-first self-hosted PaaS built for bare metal servers.

## Stack
- **Runtime**: Docker + gVisor (syscall interception for tenant isolation)
- **Routing**: Traefik (automatic HTTPS, service discovery)
- **Builds**: Nixpacks (source → container image, zero config)
- **Data**: SQLite (state + metering, WAL mode)
- **Control plane**: Single Go binary (`paasd`)

## Design
- No web dashboard — designed to be operated by AI agents via API
- One happy path per primitive (Postgres, Redis, MinIO, env vars)
- Multi-tenant with API key auth and namespace isolation
- Build sandboxing via gVisor-restricted containers

## Development
See `docs/investigations/agentic-paas-design-v2.md` for the full architecture.

```bash
make build
./bin/paasd --port 8080
```
