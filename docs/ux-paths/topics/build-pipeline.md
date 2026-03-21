# Build Pipeline — UX Path Stories

The build pipeline enables tenants to build Docker images from Git repositories using Nixpacks. Builds are asynchronous with concurrency limits (3 global concurrent, 1 per-tenant concurrent, 20 per-tenant queued), SSRF protection, log streaming, and 20-minute timeouts. Built images are pushed to the local registry at `127.0.0.1:5000`.

## STORY-001: Basic Build from GitHub Repository

**Type**: short
**Persona**: AI agent developer deploying a Python inference service
**Goal**: Build a Docker image from a GitHub repository
**Preconditions**: Tenant registered, service created, GitHub repo is public

### Steps
1. Developer triggers build:
   ```
   POST /v1/services/{serviceID}/builds
   {
     "git_url": "https://github.com/acme/llm-service.git",
     "git_ref": "main"
   }
   ```
2. Response (202 Accepted):
   ```json
   {
     "id": "build-a1b2c3d4",
     "service_id": "svc-xyz",
     "status": "pending",
     "git_url": "https://github.com/acme/llm-service.git",
     "git_ref": "main",
     "created_at": 1742560000
   }
   ```
3. Build enters the queue and starts when a slot opens
4. Nixpacks detects Python, generates Dockerfile, builds image
5. Image pushed to `127.0.0.1:5000/{tenantID}/{serviceID}:{buildID}`
6. Build status transitions: pending -> running -> succeeded

### Variations
- **Private repository**: Git clone fails with auth error; build status = "failed"
- **No Nixpacks plan detected**: Build fails with "no build plan found"
- **Empty repository**: Build fails during clone

### Edge Cases
- **Build returns immediately**: API returns 202; build executes asynchronously in background
- **Service does not exist**: Returns 404

---

## STORY-002: Build with Custom Git Ref (Branch, Tag, SHA)

**Type**: short
**Persona**: CI/CD pipeline targeting specific versions
**Goal**: Build from a specific branch, tag, or commit SHA
**Preconditions**: Service exists, repository has multiple branches and tags

### Steps
1. **Branch ref**:
   ```json
   {"git_url": "https://github.com/acme/api.git", "git_ref": "feature/new-model"}
   ```
   Nixpacks performs shallow clone of branch (fast, minimal data transfer)

2. **Tag ref**:
   ```json
   {"git_url": "https://github.com/acme/api.git", "git_ref": "v2.1.0"}
   ```
   Shallow clone of tag

3. **SHA ref** (full 40-character SHA):
   ```json
   {"git_url": "https://github.com/acme/api.git", "git_ref": "a1b2c3d4e5f6789012345678901234567890abcd"}
   ```
   Full clone + checkout (SHA requires full history to resolve)

### Variations
- **Short SHA (7 chars)**: May fail if ambiguous; full 40-char SHA recommended
- **Non-existent ref**: Build fails with "git checkout failed"
- **Default branch**: If git_ref omitted, defaults to repository's default branch

### Edge Cases
- **SHA builds are slower**: Full clone required vs shallow clone for branches/tags
- **Deleted branch**: If branch deleted between request and build start, clone fails

---

## STORY-003: Polling Build Status

**Type**: short
**Persona**: DevOps automation script
**Goal**: Track build progress without blocking
**Preconditions**: Build triggered, build ID returned

### Steps
1. Build triggered, receive build ID "build-a1b2c3d4"
2. Poll status every 5 seconds:
   ```
   GET /v1/services/{serviceID}/builds/build-a1b2c3d4
   ```
3. First poll (T+5s): `status = "pending"` (waiting for queue slot)
4. Second poll (T+10s): `status = "running"` (Nixpacks building)
5. Poll at T+120s: `status = "succeeded"`, image field populated
6. Script proceeds to deploy the built image

### Variations
- **Build fails**: `status = "failed"`, error message in response
- **Build cancelled**: `status = "cancelled"`
- **Build not found**: Returns 404

### Edge Cases
- **Long build (15 minutes)**: Status remains "running" for extended periods; no timeout until 20 minutes
- **Polling too fast**: No rate limit on status checks, but recommended interval is 5-10 seconds

---

## STORY-004: Streaming Build Logs in Real Time

**Type**: medium
**Persona**: Developer watching build output live
**Goal**: Stream build logs as they are produced
**Preconditions**: Build is in "running" state

### Steps
1. Developer opens log stream:
   ```
   GET /v1/services/{serviceID}/builds/build-a1b2c3d4/logs?follow=true
   ```
2. Response headers:
   ```
   HTTP/1.1 200 OK
   Transfer-Encoding: chunked
   Content-Type: text/plain
   ```
3. Log lines stream as Nixpacks executes:
   ```
   [nixpacks] Detecting build plan...
   [nixpacks] Detected Python 3.11
   [nixpacks] Installing dependencies...
   [nixpacks] pip install -r requirements.txt
   ...
   [nixpacks] Build complete
   [docker] Pushing to 127.0.0.1:5000/...
   [docker] Push complete
   ```
4. Stream closes when build completes (succeeded or failed)
5. Developer sees final output and knows build is done

### Variations
- **follow=false**: Returns all available logs immediately and closes connection
- **Build already completed**: Returns full log archive; stream closes immediately
- **Build pending (not yet started)**: Returns empty response; no logs yet

### Edge Cases
- **5MB log limit**: Logs are capped at 5MB per build; oldest lines dropped if exceeded
- **Client disconnects**: Server closes log reader gracefully; build continues
- **Concurrent log readers**: Multiple clients can stream the same build's logs simultaneously

---

## STORY-005: Build Completion and Auto-Deploy

**Type**: medium
**Persona**: Developer expecting automatic deployment after build
**Goal**: Understand the build-to-deploy pipeline
**Preconditions**: Build triggered for an existing service

### Steps
1. Developer triggers build:
   ```
   POST /v1/services/{serviceID}/builds
   {"git_url": "https://github.com/acme/api.git", "git_ref": "main"}
   ```
2. Build runs for 2 minutes, status transitions to "succeeded"
3. Built image: `127.0.0.1:5000/{tenantID}/{serviceID}:build-a1b2c3d4`
4. Service image field updated to the new build image
5. Service deployment triggered automatically:
   - Old container stopped and removed
   - New container created from built image
   - Environment variables loaded from database
   - Traefik route updated
6. Service status: "deploying" -> "running"
7. Developer verifies at service URL

### Variations
- **Build fails**: No deployment triggered; service continues running with old image
- **Deployment fails after build**: Build shows "succeeded" but service shows "failed" (separate status)

### Edge Cases
- **Build succeeds but image corrupt**: Deployment fails; service enters "failed" state
- **Concurrent build + deploy**: Only one build can run per tenant; second build queues

---

## STORY-006: Build Failure Diagnosis

**Type**: medium
**Persona**: Developer debugging a failed build
**Goal**: Understand why the build failed and fix the issue
**Preconditions**: Build completed with status "failed"

### Steps
1. Developer checks build status:
   ```
   GET /v1/services/{serviceID}/builds/build-a1b2c3d4
   ```
   Response: `status = "failed"`
2. Developer retrieves build logs:
   ```
   GET /v1/services/{serviceID}/builds/build-a1b2c3d4/logs
   ```
3. Logs reveal the issue:
   ```
   [nixpacks] pip install -r requirements.txt
   ERROR: Could not find a version that satisfies the requirement torch==99.0.0
   [nixpacks] Build failed: exit code 1
   ```
4. Developer fixes `requirements.txt` in the repository
5. Developer triggers a new build with the same git_url and updated ref
6. New build succeeds

### Variations
- **Dependency resolution failure**: Package not found or version conflict
- **Compilation error**: Source code doesn't compile (syntax error, missing import)
- **Docker build failure**: Dockerfile step fails (COPY missing file, RUN command errors)
- **Out of disk space**: Build fails with disk-related error

### Edge Cases
- **Logs available after failure**: Failed build logs are preserved and queryable
- **Partial logs**: If build fails early, only initial log lines available

---

## STORY-007: Cancel a Running Build

**Type**: short
**Persona**: Developer who triggered a build on the wrong branch
**Goal**: Stop the running build immediately
**Preconditions**: Build is in "running" state

### Steps
1. Developer realizes mistake (wrong git_ref)
2. Cancels build:
   ```
   DELETE /v1/services/{serviceID}/builds/build-a1b2c3d4
   ```
3. Response: 204 No Content
4. Build process is terminated via systemd scope unit
5. Build status transitions to "cancelled"
6. No image is produced; no deployment triggered

### Variations
- **Build already completed**: Returns 409 "build already completed"
- **Build pending (not yet started)**: Successfully removed from queue
- **Build not found**: Returns 404

### Edge Cases
- **Cancellation race**: If build completes between cancel request and processing, cancel is a no-op
- **Partial image**: If cancelled during Docker push, partial image is cleaned up

---

## STORY-008: Queue Full — Backpressure Scenario

**Type**: medium
**Persona**: Multi-agent cluster triggering many builds
**Goal**: Handle build queue overflow gracefully
**Preconditions**: Global concurrency limit = 3, per-tenant queue limit = 20

### Steps
1. Tenant triggers builds rapidly:
   - Build 1: Starts immediately (1 of 3 global slots used)
   - Build 2: Queued (tenant already has 1 running, per-tenant limit = 1 concurrent)
   - Builds 3-21: Queued (up to 20 queued per tenant)
   - Build 22: Rejected
2. Response for build 22 (503 Service Unavailable):
   ```json
   {
     "error": "build queue full; try again later"
   }
   ```
3. As builds complete, queued builds promote to running
4. Tenant retries build 22 after a few minutes; queue has space; build accepted

### Variations
- **Global limit hit**: 3 builds running across all tenants; new builds from any tenant queue
- **Per-tenant limit**: Each tenant can have at most 1 concurrent build; additional builds queue

### Edge Cases
- **Stale queue entries**: If build process crashes, queue entry persists until cleanup
- **Queue draining**: Builds process FIFO within the queue

---

## STORY-009: Per-Tenant Build Concurrency

**Type**: short
**Persona**: Developer triggering a second build while first is running
**Goal**: Understand per-tenant build limits
**Preconditions**: One build is already running for this tenant

### Steps
1. Tenant has build "build-001" in "running" state
2. Tenant triggers another build:
   ```
   POST /v1/services/{serviceID}/builds
   {"git_url": "https://github.com/acme/other-service.git", "git_ref": "main"}
   ```
3. Response: 202 Accepted, status = "pending"
4. Build queues behind the running build
5. When build-001 completes, the new build starts automatically

### Variations
- **Same service, different ref**: Both builds target same service; second build uses newer code
- **Different services**: Builds queue per-tenant, not per-service

### Edge Cases
- **First build hangs**: Queued build waits until first completes or times out (20 minutes)

---

## STORY-010: SSRF Protection — Blocked Git URL

**Type**: short
**Persona**: Security researcher testing input validation
**Goal**: Verify SSRF protections on git_url parameter
**Preconditions**: Build endpoint accessible with valid API key

### Steps
1. Attempt private IP:
   ```json
   {"git_url": "https://192.168.1.1/repo.git", "git_ref": "main"}
   ```
   Response: 400 "git URL host not allowed"

2. Attempt localhost:
   ```json
   {"git_url": "https://127.0.0.1/repo.git", "git_ref": "main"}
   ```
   Response: 400 "git URL host not allowed"

3. Attempt unapproved host:
   ```json
   {"git_url": "https://evil.com/malware.git", "git_ref": "main"}
   ```
   Response: 400 "git URL host not allowed"

4. Approved hosts work normally:
   - `https://github.com/...` -- allowed
   - `https://gitlab.com/...` -- allowed
   - `https://bitbucket.org/...` -- allowed
   - `https://sr.ht/...` -- allowed
   - `https://codeberg.org/...` -- allowed

### Variations
- **HTTP (not HTTPS)**: Rejected; only HTTPS URLs accepted
- **Credentials in URL**: Rejected; `https://user:pass@github.com/...` returns 400
- **URL with port**: `https://github.com:443/...` -- allowed (standard port)

### Edge Cases
- **DNS rebinding**: Host is checked at URL parse time, not at connection time
- **Redirect to private IP**: Git clone follows redirects; SSRF protection at URL validation only

---

## STORY-011: Invalid Git URL Validation

**Type**: short
**Persona**: Developer with a typo in the git URL
**Goal**: Understand validation error messages
**Preconditions**: Valid API key, service exists

### Steps
1. Empty URL:
   ```json
   {"git_url": "", "git_ref": "main"}
   ```
   Response: 400 "git_url is required"

2. Malformed URL:
   ```json
   {"git_url": "not-a-url", "git_ref": "main"}
   ```
   Response: 400 "invalid git URL format"

3. FTP protocol:
   ```json
   {"git_url": "ftp://github.com/repo.git", "git_ref": "main"}
   ```
   Response: 400 "git URL must use HTTPS"

4. URL with credentials:
   ```json
   {"git_url": "https://token:x-oauth@github.com/repo.git", "git_ref": "main"}
   ```
   Response: 400 "git URL must not contain credentials"

### Variations
- **Missing git_ref**: Uses repository default branch
- **Very long URL**: Validated against length limits

### Edge Cases
- **URL normalization**: Trailing slashes, double slashes handled by URL parser

---

## STORY-012: Build Timeout — 20-Minute Limit

**Type**: medium
**Persona**: Developer building a large ML model image
**Goal**: Understand what happens when a build exceeds the time limit
**Preconditions**: Build involves downloading large model weights (5GB+)

### Steps
1. Developer triggers build for a large ML project
2. Build starts; Nixpacks begins dependency installation
3. At T+10min: Build still running; downloading model weights
4. At T+15min: Build still running; compiling C extensions
5. At T+20min: Build timeout reached
   - Build process is terminated
   - Build status transitions to "failed"
   - Build logs show: "build timed out after 20 minutes"
6. Developer checks build status:
   ```json
   {
     "id": "build-a1b2c3d4",
     "status": "failed",
     "error": "build timed out"
   }
   ```
7. Developer must optimize the build (pre-build dependencies, use smaller base image, or cache layers)

### Variations
- **Build completes at 19:59**: Succeeds normally; timeout is exact
- **Build hangs indefinitely**: Caught by 20-minute timeout; process killed

### Edge Cases
- **Partial image after timeout**: No image produced; partial build artifacts cleaned up
- **Timeout during push**: If build completes but push times out, build marked as failed
- **System resources**: Timed-out build releases its concurrency slot for other builds
