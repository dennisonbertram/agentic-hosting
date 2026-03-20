# Daemonless Build Prototype for Nixpacks Output

**Issue:** #7
**Status:** Spike / Design Document
**Date:** 2026-03-20

## Problem Statement

The current build pipeline in `internal/builder/builder.go` shells out to `nixpacks build`, which internally runs `docker build`. This requires the build process to have access to the Docker daemon socket (`/var/run/docker.sock`). Granting socket access to the build user (`ah-builder`) is a security concern: a malicious Dockerfile or build script could use the socket to escape the build sandbox, inspect other containers, or pivot to the host.

The goal is to evaluate a daemonless build path that can convert Nixpacks-generated build plans into OCI images without requiring Docker socket access.

## Approach: Nixpacks Plan + Daemonless Image Build

Nixpacks has two modes:

1. `nixpacks build <dir>` -- plans **and** builds in one shot (current path; needs Docker).
2. `nixpacks plan <dir>` -- outputs a JSON build plan (Dockerfile, install/build/start commands, Nix packages). No Docker needed.

The daemonless strategy is:

1. Run `nixpacks plan` to generate the build plan JSON.
2. Run `nixpacks plan --generate` (or extract the Dockerfile from the JSON) to produce a Dockerfile plus build context.
3. Feed the Dockerfile + context to a daemonless image builder (Kaniko or BuildKit).
4. Push the resulting image to the local registry at `127.0.0.1:5000`.

## Kaniko vs BuildKit Comparison

| Criterion | Kaniko | BuildKit (buildkitd + buildctl) |
|---|---|---|
| **Daemon required** | No. Single binary, runs as a one-shot process. | Yes. `buildkitd` is a long-running daemon (though it does NOT need the Docker socket). |
| **Runs in container** | Designed for it (`gcr.io/kaniko-project/executor`). | Can run containerized or as a host binary. |
| **Docker socket needed** | No. | No (uses its own containerd-based workers). |
| **OCI push support** | Native `--destination` flag pushes directly. | Native via `buildctl` `--output type=image,push=true`. |
| **Cache support** | Registry-based cache, layer cache. | Local and registry-based cache, superior caching. |
| **Build speed** | Slower (no parallel layer builds, no snapshotting daemon). | Faster (parallel stage execution, smart caching). |
| **Maturity** | Stable, widely used in CI (GitHub Actions, GitLab CI). | Stable, the engine behind `docker build` since Docker 23+. |
| **Multi-stage builds** | Full support. | Full support with parallel execution. |
| **Operational complexity** | Minimal. One binary, no daemon. | Moderate. Daemon must be managed (systemd unit, health checks). |
| **Security model** | Runs as root inside its container, but no host daemon access. | Daemon runs as root but can be configured rootless. |
| **Image size fidelity** | Produces standard OCI images. | Produces standard OCI images. |

### Recommendation: Kaniko for the Prototype

**Kaniko** is the better fit for this prototype for the following reasons:

1. **Zero daemon overhead.** Kaniko is a single binary that runs, builds, pushes, and exits. No long-running process to manage, no new systemd unit, no health probes. This matches our existing one-shot-per-build execution model in `builder.go`.

2. **Simpler integration.** The current code shells out to `nixpacks build` via `exec.Command`. Replacing that with a shell-out to `kaniko executor` is a minimal diff. BuildKit would require standing up and managing `buildkitd`.

3. **Contained blast radius.** Kaniko runs inside a container with no Docker socket mount. A malicious build cannot reach the host Docker daemon.

4. **Sufficient for prototype validation.** The goal of this spike is to prove the path works end-to-end, not to optimize build speed. Kaniko's slower builds are acceptable here. If build speed becomes an issue in production, migrating to BuildKit later is straightforward since both consume the same Dockerfile input.

**For production**, BuildKit would likely be the better long-term choice due to superior caching and parallel builds. But for proving the daemonless path, Kaniko is simpler and lower risk.

## Sample App: Minimal Node.js HTTP Server

```javascript
// server.js
const http = require("http");
const server = http.createServer((req, res) => {
  res.writeHead(200, { "Content-Type": "text/plain" });
  res.end("hello from daemonless build\n");
});
server.listen(process.env.PORT || 3000);
```

```json
// package.json
{
  "name": "hello-daemonless",
  "version": "1.0.0",
  "main": "server.js",
  "scripts": { "start": "node server.js" }
}
```

## Reproducible Command Sequence

The following sequence goes from source code to a pushed image in the local registry, without the build process ever touching the Docker socket.

### Prerequisites

- Docker running (only for: running the Kaniko container itself, and the local registry)
- Local registry at `127.0.0.1:5000`
- `nixpacks` CLI installed
- The sample app above in a directory called `/tmp/hello-app/`

### Step-by-step

```bash
# ---------------------------------------------------------------
# 0. Set up the sample app
# ---------------------------------------------------------------
mkdir -p /tmp/hello-app
cat > /tmp/hello-app/server.js << 'JSEOF'
const http = require("http");
const server = http.createServer((req, res) => {
  res.writeHead(200, { "Content-Type": "text/plain" });
  res.end("hello from daemonless build\n");
});
server.listen(process.env.PORT || 3000);
JSEOF

cat > /tmp/hello-app/package.json << 'JSONEOF'
{
  "name": "hello-daemonless",
  "version": "1.0.0",
  "main": "server.js",
  "scripts": { "start": "node server.js" }
}
JSONEOF

# ---------------------------------------------------------------
# 1. Ensure local registry is running
# ---------------------------------------------------------------
docker ps --filter name=registry -q | grep -q . || \
  docker run -d -p 5000:5000 --name registry registry:2

# ---------------------------------------------------------------
# 2. Generate the Dockerfile using nixpacks plan
#    (this does NOT need Docker — pure CLI operation)
# ---------------------------------------------------------------
nixpacks plan --generate /tmp/hello-app
# This creates /tmp/hello-app/.nixpacks/Dockerfile (and environment files)

# Verify the Dockerfile was generated
ls -la /tmp/hello-app/.nixpacks/
cat /tmp/hello-app/.nixpacks/Dockerfile

# ---------------------------------------------------------------
# 3. Build the image with Kaniko (daemonless, no Docker socket)
#
#    Key points:
#    - We mount the source dir into /workspace (Kaniko's build context)
#    - We mount /tmp/hello-app/.nixpacks/Dockerfile as the Dockerfile
#    - NO Docker socket is mounted (-v /var/run/docker.sock is absent)
#    - --insecure flag for the local HTTP registry
#    - --destination pushes directly to the registry
# ---------------------------------------------------------------
IMAGE_TAG="127.0.0.1:5000/ah/hello-daemonless:prototype-001"

docker run --rm \
  -v /tmp/hello-app:/workspace:ro \
  -v /tmp/hello-app/.nixpacks/Dockerfile:/kaniko/Dockerfile:ro \
  gcr.io/kaniko-project/executor:latest \
  --context dir:///workspace \
  --dockerfile /kaniko/Dockerfile \
  --destination "$IMAGE_TAG" \
  --insecure \
  --insecure-pull

# ---------------------------------------------------------------
# 4. Verify the image exists in the local registry
# ---------------------------------------------------------------
curl -s http://127.0.0.1:5000/v2/_catalog
# Expected: {"repositories":["ah/hello-daemonless"]}

curl -s http://127.0.0.1:5000/v2/ah/hello-daemonless/tags/list
# Expected: {"name":"ah/hello-daemonless","tags":["prototype-001"]}

# ---------------------------------------------------------------
# 5. (Optional) Pull and run the image to verify it works
# ---------------------------------------------------------------
docker pull 127.0.0.1:5000/ah/hello-daemonless:prototype-001
docker run --rm -p 3001:3000 127.0.0.1:5000/ah/hello-daemonless:prototype-001 &
sleep 2
curl http://localhost:3001
# Expected: "hello from daemonless build"
docker stop $(docker ps -q --filter ancestor=127.0.0.1:5000/ah/hello-daemonless:prototype-001)
```

### What the Docker Socket is Used For (and Not Used For)

| Step | Uses Docker socket? | Why |
|---|---|---|
| `nixpacks plan --generate` | No | Pure CLI; generates Dockerfile from source analysis |
| `docker run ... kaniko ...` | Yes (host level) | We need Docker to run the Kaniko container itself |
| Kaniko executor (inside container) | **No** | Builds OCI image layers in userspace, pushes via HTTP |
| `docker push` | N/A | Not used; Kaniko pushes directly |

The critical security improvement: the **build process itself** (the part that executes untrusted Dockerfiles from user code) never has access to the Docker daemon. Kaniko runs in an isolated container with no socket mount.

## Changes Required in `internal/builder/` for Production

If this prototype is promoted to production, the following changes are needed in `internal/builder/builder.go`:

### 1. Replace `nixpacksBuild()` with a Two-Phase Method

The current `nixpacksBuild()` method (lines 351-404) runs `nixpacks build`, which does plan + build + Docker image creation in one call. This must be split:

```go
// Phase 1: Generate Dockerfile (no Docker needed)
func (b *Builder) nixpacksPlan(ctx context.Context, req BuildRequest, buildDir string, logCb func(string)) error {
    cmd := exec.CommandContext(ctx,
        "systemd-run", "--scope", "--quiet",
        "--unit="+unitName,
        "-p", "MemoryMax=512M",
        "/usr/sbin/runuser", "-u", "ah-builder", "--",
        b.nixpacks, "plan", "--generate", buildDir,
    )
    // No DOCKER_HOST in env — this step is Docker-free
    cmd.Env = []string{
        "HOME=/tmp",
        "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
    }
    // ... exec and stream logs ...
}

// Phase 2: Build OCI image with Kaniko (no Docker socket)
func (b *Builder) kanikoBuild(ctx context.Context, req BuildRequest, buildDir string, logCb func(string)) error {
    dockerfilePath := filepath.Join(buildDir, ".nixpacks", "Dockerfile")
    cmd := exec.CommandContext(ctx,
        "systemd-run", "--scope", "--quiet",
        "--unit="+unitName,
        "-p", "MemoryMax=2G",
        "-p", "CPUQuota=200%",
        "docker", "run", "--rm",
        "--network=host",
        "-v", buildDir+":/workspace:ro",
        "-v", dockerfilePath+":/kaniko/Dockerfile:ro",
        b.kanikoImage,
        "--context", "dir:///workspace",
        "--dockerfile", "/kaniko/Dockerfile",
        "--destination", req.ImageTag,
        "--insecure",
    )
    // No DOCKER_HOST needed inside the Kaniko container
    // ... exec and stream logs ...
}
```

### 2. Remove `pushImage()` Method

The current `pushImage()` method (lines 406-449) runs `docker push`. With Kaniko, the push happens as part of the build (`--destination` flag). This method can be deleted entirely.

### 3. Update `Builder` Struct

```go
type Builder struct {
    workDir      string
    nixpacks     string
    kanikoImage  string // e.g. "gcr.io/kaniko-project/executor:v1.23.0"
    buildSem     chan struct{}
    tenantMu     sync.Mutex
    tenantBuilds map[string]struct{}
    procMu       sync.Mutex
    procMap      map[string]string
}
```

### 4. Update `Build()` Orchestration

```go
func (b *Builder) Build(ctx context.Context, req BuildRequest, logCb func(string)) error {
    // ... concurrency checks, clone (unchanged) ...

    // Step 2: Generate Dockerfile (replaces nixpacksBuild)
    if err := b.nixpacksPlan(ctx, req, buildDir, logCb); err != nil {
        return fmt.Errorf("nixpacks plan: %w", err)
    }

    // Step 3: Build + push with Kaniko (replaces nixpacksBuild + pushImage)
    if err := b.kanikoBuild(ctx, req, buildDir, logCb); err != nil {
        return fmt.Errorf("kaniko build: %w", err)
    }

    logCb("[ah] Build succeeded: " + req.ImageTag)
    return nil
}
```

### 5. Remove Docker Socket from Build Environment

The `ah-builder` user no longer needs to be in the `docker` group for builds. The environment variables `DOCKER_HOST` and `DOCKER_CONFIG` can be removed from the nixpacks plan step. The `docker` group membership is only needed at the host level to run the Kaniko container, which is done by the `ah` service (running as root or a privileged user), not `ah-builder`.

### 6. Configuration Changes

- Add `kaniko-image` to the server config (pinned version, e.g. `gcr.io/kaniko-project/executor:v1.23.0`).
- Pre-pull the Kaniko image on server startup to avoid cold-start latency on the first build.
- Add `--insecure-registry` handling for the local registry push.

### 7. Update Build Cancellation

The current `CancelBuild` stops the systemd scope. With Kaniko running inside a Docker container, cancellation must also stop the Kaniko container. This can be done by:
- Giving Kaniko containers a predictable name: `--name ah-kaniko-<buildID>`.
- Updating `CancelBuild` to run `docker stop ah-kaniko-<buildID>` in addition to stopping the systemd scope.

## Blocker: Nixpacks-Generated Dockerfiles Assume Nix Store Availability

**This is the primary blocker that must be solved before replacing the current build path.**

Nixpacks-generated Dockerfiles use multi-stage builds where early stages install Nix packages (e.g., `nodejs`, `npm`, `python`) from the Nix store. These stages use base images like `ghcr.io/railwayapp/nixpacks:ubuntu-1735887258` that contain a pre-populated `/nix/store`.

When Kaniko builds these Dockerfiles, it must be able to pull these base images. In a production environment:

1. **The Nixpacks base images are large** (often 1-2 GB). Each Kaniko build starts from scratch with no layer cache (by default), meaning every build re-downloads and re-extracts these layers. This makes builds significantly slower than the current Docker-daemon path, which benefits from the local image and layer cache.

2. **Registry cache configuration is required.** Kaniko supports `--cache=true` with `--cache-repo` to cache intermediate layers in a registry. This must be configured and tested with our local registry. Without it, build times could be 3-5x slower.

3. **Network access from the Kaniko container.** The Kaniko container needs to reach both the Nix package registry (for base image pulls) and the local registry (for pushing). The `--network=host` flag works but reduces isolation. A dedicated build network with controlled egress (connecting to the build egress allowlist from issue #3) would be the production solution.

**Mitigation path:** Configure Kaniko with `--cache=true --cache-repo=127.0.0.1:5000/cache` and pre-warm the cache by running a build for each supported runtime (Node, Python, Go) during provisioning. Measure build times with and without cache to determine if the performance is acceptable.

## Summary

| Aspect | Current Path | Daemonless Prototype |
|---|---|---|
| Build tool | `nixpacks build` (uses Docker internally) | `nixpacks plan --generate` + Kaniko |
| Docker socket needed | Yes (by `ah-builder`) | No (by build process) |
| Image push | `docker push` | Kaniko `--destination` (built-in) |
| Build isolation | systemd scope only | systemd scope + container (no socket) |
| Caching | Docker layer cache (fast) | Registry cache (needs configuration) |
| Operational complexity | Low (one command) | Medium (two phases, Kaniko image management) |
| Security | Build can access Docker daemon | Build cannot access Docker daemon |
