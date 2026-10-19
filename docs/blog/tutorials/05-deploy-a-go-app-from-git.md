---
title: "Deploy a Go app from GitHub with one command"
description: "Point ahc at a public GitHub repo, let Nixpacks build it without a Dockerfile, watch the logs stream, and get a running service."
slug: deploy-a-go-app-from-git
tags: [go, nixpacks, git-deploy, tutorial, agentic-hosting]
cover_diagram: diagrams/05-deploy-a-go-app-from-git.png
sources:
  - cmd/ahc/deploy.go
  - cmd/ahc/build.go
  - internal/api/builds.go
  - internal/api/server.go
  - internal/api/services.go
  - internal/builds/builds.go
  - internal/builder/builder.go
  - internal/builder/worker_exec.go
  - internal/services/services.go
  - internal/services/deploy_image.go
  - internal/status/status.go
  - docs/ops/build-worker.md
  - docs/assets/demo.cast
  - docs/discoverability/README.md
  - deploy/ah-build@.service
  - internal/diskcheck
  - docs-site/src/content/docs/deploy.md
---

# Deploy a Go app from GitHub with one command

If an AI agent writes your code, it should also be able to ship it. I think the git URL is the right unit for that: the agent already has the repo, so the deploy should need nothing else. No Dockerfile, no CI pipeline.

You have a Go HTTP service in a GitHub repo and you want it running with a URL. By the end of this post you will have deployed a small Go service from its git URL with one `ahc` command, watched Nixpacks build it, and seen it reach `running`. You will also know what the same flow looks like as raw API calls, and which URLs the server will refuse to build.

![Diagram of the git deploy pipeline: source_url, git validator, build slot, ah-builder, local registry, DeployImage, gVisor container, Traefik route, with build statuses pending, running, succeeded or failed](diagrams/05-deploy-a-go-app-from-git.png)

A note on how this was written: the commands below were checked against the agentic-hosting source code, not run for this post. The repo also keeps a recorded session of this exact flow (`docs/assets/demo.cast`), and the sample output below follows it.

## Prerequisites

- An agentic-hosting account and API key. On the hosted API the base URL is `https://api.agentic.hosting/v1/`. If you self-host, use your own host.
- The `ahc` CLI, configured with your key (`ahc configure`, or `ahc register` for a new tenant).
- A public repo on GitHub, GitLab, Bitbucket, sourcehut or Codeberg.

For the examples I use `hashicorp/http-echo`, a tiny Go web server. The repo's recorded demo deploys it with `--port 5678`, so that is the port I use. If you use your own repo, pass whatever port your app listens on.

## Step 1: Deploy with one command

```bash
ahc deploy https://github.com/hashicorp/http-echo my-go-app --port 5678
```

`ahc` treats any source that starts with `http://` or `https://` as a git URL. Anything else is treated as a Docker image. Here is what it does for a git URL:

1. Creates a service named `my-go-app` with the placeholder image `pending`. The create call requires an image, and the build replaces it later. If a service with that name already exists, `ahc` reuses it.
2. Starts a build with `source_type: "git"` and your URL.
3. Opens the build log stream and prints each line with a `[build]` prefix.
4. Polls the build until it is `succeeded`, `failed` or `cancelled`.
5. Polls the service every 2 seconds, up to 150 times (5 minutes), until it is `running`, then prints its URL.

Expected output, shortened from the recorded run:

```text
Deploying https://github.com/hashicorp/http-echo as "my-go-app"...
Service "my-go-app" created (id: c0b46d3f)
Starting build...
Build started (id: db2c0b04c2a02f10a27ae096c9b69498)
[build] [ah] Starting build db2c0b04c2a02f10a27ae096c9b69498
[build] [ah] Cloning https://github.com/hashicorp/http-echo (ref: main)
...
[build] [ah] Build complete
[build] [ah] Deploying built image...
[build] [ah] Deploy succeeded
Waiting for service to start
  URL: http://<service-id>.localhost
```

Your ids will differ. The URL depends on how the server was started: with no `--base-domain` it is `http://{serviceID}.localhost`, and with one it is `https://{dns_label}.<your-base-domain>`.

One caveat for this particular repo. `http-echo` expects a `-text` flag or an `ECHO_TEXT` environment variable. Without it the container logs `Missing -text option or ECHO_TEXT env var!`. It is still a fine way to see the pipeline work, but if you want a page that says something, pick an app that does not need a flag, or set `ECHO_TEXT` with `ahc service env set my-go-app ECHO_TEXT=hello`.

## Step 2: The same thing as raw API calls

`ahc` is a thin wrapper, so an agent can skip it and call the API directly with curl. Set `$AH_API_KEY` and `$AH_URL` (for the hosted API, `https://api.agentic.hosting`).

Create the service:

```bash
curl -s -X POST "$AH_URL/v1/services" \
  -H "Authorization: Bearer $AH_API_KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"name":"my-go-app","image":"pending","port":5678}'
```

Take the `id` from the response as `$SERVICE_ID`, then start the build:

```bash
curl -s -X POST "$AH_URL/v1/services/$SERVICE_ID/builds" \
  -H "Authorization: Bearer $AH_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"source_type":"git","source_url":"https://github.com/hashicorp/http-echo","source_ref":"main"}'
```

The server answers `202 Accepted`:

```json
{"build_id":"<32 hex chars>","status":"pending","image":"127.0.0.1:5000/ah/<tenant>-<service>:<build_id>"}
```

The build request has exactly three fields: `source_type`, `source_url` and `source_ref`. There is no `git_url` and no `branch`. `source_type` must be `git`. If you leave `source_ref` out, it defaults to `main`. A repo whose default branch is `master` needs `"source_ref":"master"`.

## Step 3: Stream the build logs

```bash
curl -sN -H "Authorization: Bearer $AH_API_KEY" \
  "$AH_URL/v1/services/$SERVICE_ID/builds/$BUILD_ID/logs?follow=true"
```

With `follow=true` the server keeps the connection open and sends plain text as the build runs. Without it, you get the log as it stands when you ask. From `ahc`, `ahc build logs my-go-app` streams the latest build.

The log has a few recognizable phases:

- `[ah] Cloning <url> (ref: main)`, then `[ah] Clone complete`.
- `[ah] Running nixpacks build...`. Nixpacks prints its plan, a table with `setup`, `install`, `build` and `start` rows. For a Go module the recorded run shows `go`, `go mod download`, `go build -o out` and `./out`. Docker build steps follow.
- `[ah] Push complete`, after the image is pushed to the local registry at `127.0.0.1:5000`.
- `[ah] Build succeeded: <image tag>`, then `[ah] Build complete`.

## Step 4: Watch the build hand off to deploy

After `Build complete`, the log shows `[ah] Deploying built image...`. The build manager calls `DeployImage` with the image tag it just pushed. That replaces the `pending` placeholder, starts the container under gVisor, and sets up the Traefik route. The build is marked `succeeded` only after `DeployImage` returns, and the log ends with `[ah] Deploy succeeded`. If the deploy step fails, the build is marked `failed` even though Nixpacks finished.

Check on the build and the service:

```bash
curl -s -H "Authorization: Bearer $AH_API_KEY" "$AH_URL/v1/services/$SERVICE_ID/builds/$BUILD_ID"
curl -s -H "Authorization: Bearer $AH_API_KEY" "$AH_URL/v1/services/$SERVICE_ID"
```

The build moves through `pending`, `running`, and then `succeeded` or `failed`. The service response carries its own `status`; you are waiting for `running`. `ahc build list my-go-app` shows the same builds as a table with the first 8 characters of each id.

## Step 5: What the git validator enforces

Before a build is accepted, the server checks the URL and ref. A bad URL fails the request with a validation error and nothing is built.

- The host must be one of `github.com`, `gitlab.com`, `bitbucket.org`, `sr.ht` or `codeberg.org`.
- The URL must start with `https://`.
- The URL must not contain credentials (no `https://user:token@github.com/...`).
- The URL can be at most 2048 characters.
- The ref can be at most 256 characters and may contain only letters, digits, `.`, `_`, `/` and `-`. A commit SHA works as a ref.

The host list is short on purpose. The code comment says it exists to prevent DNS rebinding attacks. In practice, private repos and self-hosted git servers do not work through this path, since there is nowhere to put credentials.

Two other limits can reject a build. A build is refused if `/var/lib/ah` or `/var/lib/docker` is over 90% full. If 10 builds are already queued, you get a `409` with `build queue full; try again later`.

## Step 6: Redeploying

```bash
ahc deploy --redeploy my-go-app
```

This calls `POST /v1/services/{id}/redeploy`. Read the code before you assume what it does: the handler is documented as an alias for restart. It stops the container and starts a new one with the service's current image and environment variables. It does not start a new build.

You can see this by comparing build ids before and after:

```bash
ahc build list my-go-app   # note the id
ahc deploy --redeploy my-go-app
ahc build list my-go-app   # same id, no new row
```

To build new code, start a new build. Re-running `ahc deploy https://github.com/hashicorp/http-echo my-go-app --port 5678` reuses the existing service, starts a build with a new id, and replaces the running image when that build succeeds. Use redeploy when you changed environment variables and just need a restart.

## How it works

Follow the diagram left to right, then along the bottom row.

1. `POST /v1/services/{id}/builds` runs the git validator and checks that the service belongs to you.
2. The server inserts a build row with status `pending` and takes one of 10 build slots.
3. The build goes to the `ah-builder` worker. In production the API process does not run builds itself. A separate `ah-builder` systemd service does, started per build through a small broker, with a 2 GiB memory limit and a 20-minute runtime limit. The row moves to `running`.
4. The worker clones the repo, runs Nixpacks, and pushes the image to the registry at `127.0.0.1:5000`. The tag is `127.0.0.1:5000/ah/<tenant>-<service>:<build id>`.
5. `DeployImage` starts the container with gVisor (`runsc`) isolation and registers the route in Traefik.
6. The build becomes `succeeded`, or `failed` with the error written to the log.

One operational caveat from the ops doc: the build worker needs Docker access to push images, which is broad access on the host. Self-hosters should read `docs/ops/build-worker.md` before running builds in production.

## What to try next

- Read the full [deploy reference](https://agentic.hosting/docs/deploy/) for cancel, concurrency limits and the list endpoints.
- Cancel a build in flight with `ahc build cancel my-go-app <build-id>`.
- Add configuration to your app with the [env docs](https://agentic.hosting/docs/env/).
- Attach a Postgres or Redis instance with the [databases guide](https://agentic.hosting/docs/databases/).
- Hit a failure? The [troubleshooting page](https://agentic.hosting/docs/troubleshooting/) is the place to start.
- Star or fork the code at [github.com/dennisonbertram/agentic-hosting](https://github.com/dennisonbertram/agentic-hosting).
