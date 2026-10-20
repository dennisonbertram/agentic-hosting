# Runbook: local dev sandbox

`make sandbox` starts a throwaway agentic-hosting instance on the local Docker engine, on macOS or Linux. `make sandbox-down` removes it.

```sh
make sandbox          # scripts/sandbox.sh up
make sandbox-status   # scripts/sandbox.sh status
make sandbox-down     # scripts/sandbox.sh down
```

Pass flags through `SANDBOX_FLAGS`, for example `make sandbox SANDBOX_FLAGS=--yes` or `make sandbox-down SANDBOX_FLAGS=--keep-data`. `scripts/sandbox.sh up --dry-run` prints the plan without touching Docker.

> **Dev only.** The sandbox runs `ah --dev`, which disables HTTPS enforcement. When Docker has no gVisor runtime (always the case on macOS), the sandbox sets `AH_CONTAINER_RUNTIME=runc`, so tenant containers have no syscall sandbox. Use it for development against your own code only.

## What `up` does

1. Checks for `docker`, `curl`, `python3`, `go` and `od`, and checks that `docker info` works.
2. Safety checks. It refuses to start when:
   - a production `ah.service` is active on the host,
   - a container named `paas-traefik` exists that the sandbox did not create, or
   - Docker already has `ah.managed=true` containers that the sandbox did not create.

   If Docker is running other containers, it asks before continuing (`--yes` skips the prompt).
3. Creates the state dir (`.sandbox/` in the repo, mode 0700, git-ignored). It generates a master key (`data/master.key`, 0600) and bootstrap and admin tokens (`secrets.env`, 0600). Existing files are reused.
4. Builds `ah` into `.sandbox/bin/ah`.
5. Starts Traefik as `paas-traefik`, labelled `ah.sandbox=true`. It uses the image digest pinned in `scripts/bootstrap-stack.sh`, a single HTTP entrypoint published on `127.0.0.1:18400`, the file provider reading `.sandbox/traefik/dynamic`, and no dashboard. The name is fixed because `ah` attaches the container named `paas-traefik` to each service's ingress network.
6. Starts `ah serve --dev --port 18480` in the background (log: `.sandbox/logs/ah.log`) with:

   | Variable | Value | Why |
   |---|---|---|
   | `AH_DATA_DIR` | `.sandbox/data` | Separate state from any real install |
   | `AH_TRAEFIK_CONFIG_DIR` | `.sandbox/traefik/dynamic` | Routes go to the sandbox Traefik |
   | `AH_CONTAINER_RUNTIME` | `runsc` when Docker on Linux lists it, else `runc` | gVisor when available |
   | `AH_WARM_POOL_ENABLED` | `false` | Avoids pulling four environment template images |
   | `AH_GC_PRUNE_DOCKER` | `false` | GC will not prune dangling images or anonymous volumes that belong to your other projects |
   | `AH_BASE_DOMAIN` | empty | Localhost routing: `http://{serviceID}.localhost` |

7. Registers the tenant `sandbox-demo` and stores its ID and API key in `.sandbox/tenant.env` (0600).
8. Builds `examples/openrouter-agent` as `ah-sandbox/openrouter-agent:dev` and deploys it as service `demo` on port 3000. It waits for the service to reach `running` and for `/health` to answer through Traefik. If the route is not live after 10 seconds, it restarts Traefik once, because some macOS bind mounts do not deliver file-change events.
9. Prints the API URL, the tenant API key, the `ahc` environment, and the demo URL.

Example output:

```
  ah sandbox is up (dev mode, runtime: runc)
  API:            http://127.0.0.1:18480   (log: .../.sandbox/logs/ah.log)
  Tenant API key: <key>
  Use the CLI:    export AH_URL=http://127.0.0.1:18480 AH_KEY=<key>
                  go run ./cmd/ahc status
  Demo service:   http://<service-id>.localhost:18400/health
```

`curl` and browsers resolve `*.localhost` to 127.0.0.1. If yours does not, send the Host header: `curl -H 'Host: <service-id>.localhost' http://127.0.0.1:18400/health`.

Running `up` again while the sandbox is healthy only prints the summary.

## Using it

```sh
export AH_URL=http://127.0.0.1:18480 AH_KEY=<key from the summary>
go run ./cmd/ahc status
go run ./cmd/ahc logs demo
go run ./cmd/ahc service list
```

The demo's `/api/chat` endpoint needs a real OpenRouter key. Set `OPENROUTER_API_KEY` before the first `make sandbox` to pass it through. Without it, the service uses a placeholder key and only `/health` is useful. The demo also requires a `DEMO_AUTH_TOKEN`; the sandbox generates a random one.

## Sandboxes

Disposable sandboxes (`/v1/sandboxes`, `ahc sandbox`) work against the local instance. Do not confuse them with this "local dev sandbox": an `ahc sandbox` is a throwaway container that `ah` deletes when its TTL expires. They run under the same runtime as everything else here (`runc` on macOS, so no syscall sandbox), with no network egress.

```sh
export AH_URL=http://127.0.0.1:18480 AH_KEY=<key from the summary>

# One shot: create, run, delete. The exit code of the command is ahc's exit code.
go run ./cmd/ahc sandbox run --image alpine:3.20 -- sh -c 'uname -a; id'

# Keep one around (default TTL 30m; the server deletes it when it expires)
go run ./cmd/ahc sandbox create scratch --image python:3.12-slim --ttl 15m
go run ./cmd/ahc sandbox exec scratch -- python3 -c 'print(2+2)'
go run ./cmd/ahc sandbox list
go run ./cmd/ahc sandbox extend scratch --ttl 30m
go run ./cmd/ahc sandbox delete scratch

# Inspect a copy of the demo service: runs its app with its env vars (values never shown)
go run ./cmd/ahc sandbox create demo-copy --service <demo-service-id> --include-env --ttl 10m
go run ./cmd/ahc sandbox logs demo-copy --tail 50
go run ./cmd/ahc sandbox exec demo-copy -- ps aux
go run ./cmd/ahc sandbox delete demo-copy
```

Notes for the local instance:

- The warm pool is off here (`AH_WARM_POOL_ENABLED=false`), so the first `--template` sandbox pulls the template image. `--image alpine:3.20` is the quickest start.
- `--service` and `--snapshot` use local images (`127.0.0.1:5000/...` or the image the service was deployed from); a failed pull falls back to the local copy.
- Exec needs the `timeout` binary inside the image; `alpine`, `debian`, `ubuntu` and `python` images have it, distroless images do not (exit code 127).
- `--max-sandboxes-total` / `AH_MAX_SANDBOXES_TOTAL` (default 20) caps sandboxes host-wide; the tenant default is 5 (`max_sandboxes`).
- `make sandbox-down` removes sandbox containers with the tenant's other labelled containers. Their `ah-env-*` workspace volumes are left behind if `ah` was stopped before deleting them; remove them with `docker volume ls -q --filter name=ah-env- | xargs docker volume rm`.

Settings (environment variables read by `scripts/sandbox.sh`):

| Variable | Default | Meaning |
|---|---|---|
| `AH_SANDBOX_DIR` | `<repo>/.sandbox` | State dir (absolute path) |
| `AH_SANDBOX_API_PORT` | `18480` | `ah` API port on 127.0.0.1 |
| `AH_SANDBOX_HTTP_PORT` | `18400` | Traefik HTTP port on 127.0.0.1 |
| `AH_SANDBOX_RUNTIME` | `auto` | `auto`, `runsc` or `runc` |
| `AH_SANDBOX_SKIP_EXAMPLE` | `0` | `1` registers the tenant but deploys nothing |
| `OPENROUTER_API_KEY` | unset | Passed to the demo service |

## What `down` does

1. Deletes every service of the sandbox tenant through the API, so `ah` removes the containers, networks and routes it created.
2. Stops `ah`.
3. Removes any containers left with the tenant's `ah.tenant` label and the tenant's `ah-tenant-*` / `ah-ingress-*` networks.
4. Removes `paas-traefik`, but only if it carries `ah.sandbox=true`.
5. Deletes the state dir, only if it contains the `.ah-sandbox` marker. `--keep-data` keeps it.

The `ah-sandbox/openrouter-agent:dev` image and the pulled Traefik image are kept as build cache. Remove them with `docker rmi`.

## Limits

- No git builds: Nixpacks builds need the Linux systemd build broker. Deploy from images, or use a full Linux install or the Lima VM ([macmini.md](../ops/macmini.md)).
- No TLS and no public domain.
- The sandbox shares the Docker engine with anything else you run. Use a separate Colima profile or Docker context if that matters (see [setup-macos.md](setup-macos.md#docker-engine-notes)).
- Postgres/Redis provisioning, environments, disposable sandboxes and kanban boards have not been exercised in the sandbox.

## Troubleshooting

| Symptom | Fix |
|---|---|
| `a non-sandbox container named paas-traefik exists` | This Docker engine hosts a real install. Do not run the sandbox there |
| `ah exited during startup` | Read `.sandbox/logs/ah.log`. A port conflict on 18480 means you should set `AH_SANDBOX_API_PORT` |
| Traefik fails to start: port already allocated | Set `AH_SANDBOX_HTTP_PORT` |
| Service stuck in `deploying` or `failed` | `go run ./cmd/ahc logs demo`; `.sandbox/logs/ah.log`; `docker ps -a --filter label=ah.managed=true` |
| `/health` returns 404 through Traefik | `ls .sandbox/traefik/dynamic` (expect `<service-id>.yml`); `docker logs paas-traefik`. On Colima, keep the repo under `$HOME` |
| Sandbox half-started after a crash | `make sandbox-down`, then `make sandbox` |

## Verification status

`--help` and `--dry-run` for `up` and `down` are covered by `make test-scripts`. `ah` was started on macOS with `--dev` and `AH_CONTAINER_RUNTIME=runc`; it printed the warning banner and answered `GET /v1/system/health`. It also refused `runc` without `--dev`. A full `make sandbox` run (Traefik, tenant registration, example deploy) has not yet been recorded. Treat the first run as a smoke test, and note the result here.
