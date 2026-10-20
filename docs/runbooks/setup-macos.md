# Runbook: set up a Mac for development

gVisor needs a Linux kernel, so a Mac cannot run agentic-hosting with production isolation directly. You have two options:

| Goal | Path |
|---|---|
| Develop and try the API locally | `scripts/setup-macos.sh`, then `make sandbox` (this runbook) |
| Run the real stack, with gVisor, on a Mac | The Lima VM on Apple Silicon described in [docs/ops/macmini.md](../ops/macmini.md) (`scripts/bootstrap-macmini.sh`) |

> **Dev only.** The local sandbox runs tenant containers with Docker's `runc` runtime. There is no syscall sandbox: a container shares the Docker VM's kernel. Do not run untrusted code in it, and do not expose it to a network. `ah` accepts `AH_CONTAINER_RUNTIME=runc` only together with `--dev`, and it logs a warning banner at startup when it is set.

## One command

```sh
git clone https://github.com/dennisonbertram/agentic-hosting
cd agentic-hosting
scripts/setup-macos.sh --dry-run   # see what it would install or start
scripts/setup-macos.sh             # do it (asks before installing)
make sandbox
```

`setup-macos.sh`:

1. Checks for the Xcode Command Line Tools (cgo needs them for SQLite) and Homebrew. It does not install either; it tells you how.
2. Picks a Docker engine. It uses the one that is already running. If none is running, it picks Docker Desktop if installed, then OrbStack, then Colima. Override with `--engine docker-desktop|orbstack|colima`.
3. Uses `brew install` only for what is missing: `go`, `jq`, `python3`, and for Colima also `docker` (the CLI) and `colima`.
4. Starts the engine: `open -a Docker`, `open -a OrbStack`, or `colima start --cpu 2 --memory 4 --disk 30`.
5. Builds `bin/ah` and `bin/ahc` with `make build build-cli` (skip with `--skip-build`).

Options: `--dry-run`, `--yes`/`-y` (no prompts), `--engine NAME`, `--skip-build`, `--help`.

Nixpacks is not installed on macOS. Git builds go through the Linux-only systemd build broker (`ah-build-broker.service`), so the sandbox deploys from images. To test git builds, use the Lima VM path.

## Docker engine notes

- **Docker Desktop / OrbStack**: share `/Users` with the engine by default, which the sandbox's Traefik bind mount needs.
- **Colima**: mounts your home directory by default. Keep the checkout (or `AH_SANDBOX_DIR`) under `$HOME`; otherwise the Traefik route directory appears empty inside the VM and services return 404.
- To keep the sandbox off a Docker daemon you use for other work, use a separate Colima profile:

  ```sh
  colima start --profile ah-sandbox --cpu 2 --memory 4
  docker context use colima-ah-sandbox
  make sandbox
  ```

## Troubleshooting

| Symptom | Fix |
|---|---|
| `Xcode Command Line Tools are required` | `xcode-select --install`, then rerun |
| `Homebrew is required` | Install from https://brew.sh, then rerun |
| `Docker is not reachable` from `make sandbox` | Start the engine (`colima start`, or open Docker Desktop or OrbStack) and wait until `docker info` works |
| `go build` fails with cgo or `sqlite3` errors | Reinstall the Command Line Tools: `sudo rm -rf /Library/Developer/CommandLineTools && xcode-select --install` |

Next: [local-sandbox.md](local-sandbox.md).
