# Service Lifecycle Control — UX Path Stories

User journeys covering daily operations: starting, stopping, restarting services; log streaming; circuit breaker triggering and recovery; crash detection; and reconciliation behavior.

## Story 1: Basic Start, Stop, Restart Workflow

**Goal**: Operator deploys a Flask API, tests it, stops it temporarily, then restarts it.

**Setup**:
- Service "flask-api" already created, status="running", container_id=abc123
- Service has image "127.0.0.1:5000/flask-api:latest" and port 5000
- Container is healthy and responsive

**Steps**:

1. Operator checks service status
   - `GET /v1/services/flask-api`
   - Response: status="running", url="http://flask-api.localhost"
   - Operator can now reach the service at its URL

2. Operator stops the service for maintenance
   - `POST /v1/services/flask-api/stop`
   - Response: {"status": "stopped"}
   - Container is stopped but not removed; circuit breaker remains closed

3. Operator verifies it's stopped
   - `GET /v1/services/flask-api`
   - Response: status="stopped"
   - Service is no longer accessible

4. Operator restarts the service
   - `POST /v1/services/flask-api/restart`
   - Response: {"status": "running"}
   - Same container ID is restarted (preserves volume data)
   - Environment variables are reloaded from database

5. Operator verifies it's running again
   - `GET /v1/services/flask-api`
   - Response: status="running", same url and port
   - Service is accessible again

**Expected Behavior**:
- Stop is idempotent: calling it twice succeeds both times
- Restart reuses the existing image; no rebuild occurs
- Env vars persisted in database are automatically applied
- No circuit breaker triggers during manual stop/restart

**Error Cases**:
- If service is already stopped, stop request returns 200 (idempotent)
- If service doesn't exist, all operations return 404


## Story 2: Log Streaming with Follow Mode

**Goal**: Operator deploys a web scraper and watches its logs in real-time.

**Setup**:
- Service "web-scraper" created with status="deploying"
- Deployment has started; container is running and producing output
- Service logs to stdout (normal Docker behavior)

**Steps**:

1. Operator checks if deployment is complete
   - `GET /v1/services/web-scraper`
   - Response: status="deploying" (still in progress)

2. Operator streams last 50 lines of logs (non-follow)
   - `GET /v1/services/web-scraper/logs?tail=50&follow=false`
   - Response: plain text, chunked transfer
   - Headers: `Transfer-Encoding: chunked`, `Content-Type: text/plain`
   - Returns last 50 log lines immediately and closes

3. Operator switches to follow mode to watch deployment progress
   - `GET /v1/services/web-scraper/logs?follow=true&tail=10`
   - Response: 200 OK, chunked transfer, streaming continuously
   - Headers include `Transfer-Encoding: chunked`
   - Connection stays open; logs are flushed to client as they appear

4. Operator observes logs as deployment happens
   - Initial 10 lines shown
   - Buildpack output appears: "Detecting build plan..."
   - Image build completes: "Successfully tagged..."
   - Container starts: "Listening on :8080"
   - Application boots: "Ready to accept requests"

5. Deployment complete signal
   - `GET /v1/services/web-scraper` (parallel request)
   - Response: status="running" (deployment finished)

6. Operator closes the follow stream
   - Client closes the HTTP connection (or waits for EOF)
   - Server closes the log reader gracefully

**Expected Behavior**:
- Logs are read from Docker daemon's log driver (json-file or similar)
- Follow mode never times out (no HTTP timeout applies)
- If container is already stopped, logs show only archived output and stream returns EOF
- Tail parameter is capped at 10,000 lines (server-side limit)
- Non-follow requests complete immediately even if container is still running

**Error Cases**:
- If service doesn't exist: 404
- If follow=true but container has exited: returns available logs and EOF
- If Docker daemon is down: 503 "service management is not available"


## Story 3: Circuit Breaker Triggering (5 Crashes in 10 Minutes)

**Goal**: A buggy Python app crashes repeatedly. Operator watches the circuit breaker open.

**Setup**:
- Service "python-worker" created with image containing a memory leak bug
- Service is status="running" initially
- Crash window and crash_count are tracked in the database

**Crash Timeline**:

**T+0s**: First crash
- Reconciler detects container has exited (exit code 1)
- Database updated: crash_count=1, crash_window_start=now
- Service status changed to "crashed"
- last_error="container exited (exit code 1)"
- Circuit remains closed; service is still in "crashed" state awaiting restart

**T+2s**: Operator manually restarts
- `POST /v1/services/python-worker/restart`
- Container restarts from the same image
- Service status="running"

**T+15s**: Second crash (13s after restart)
- Reconciler detects container exited again
- Since (now - crash_window_start) = 15s < 600s, crash_count increments
- Database: crash_count=2, crash_window_start stays the same
- Service status="crashed"

**T+20s**: Operator restarts again
- `POST /v1/services/python-worker/restart`
- Service status="running"

**T+40s**: Third crash
- Reconciler detects exit
- crash_count=3, still within 10-minute window
- Service status="crashed"

**T+45s**: Operator restarts again
- Service status="running"

**T+60s**: Fourth crash
- crash_count=4
- Service status="crashed"

**T+65s**: Operator restarts again
- Service status="running"

**T+90s**: Fifth crash (CIRCUIT OPENS)
- Reconciler detects exit
- Since crash_count (4) + 1 = 5, and window is still active (90s < 600s):
  - **Circuit breaker opens**: circuit_open=1
  - circuit_open_count=1
  - circuit_retry_at = now + 30 minutes
- Reconciler immediately stops and removes the container
- Service status="crashed"
- last_error="container exited (exit code 1)"

**T+91s**: Operator checks service status
- `GET /v1/services/python-worker`
- Response: status="crashed", circuit_open=true, last_crashed_at=<timestamp>, last_error="..."
- Operator sees crash_count=5

**T+92s**: Operator attempts to restart while circuit is open
- `POST /v1/services/python-worker/restart`
- Returns 400 Bad Request or similar
- Message: "circuit breaker is open; retry after <time>"
- Container is NOT started

**T+600s onwards**: Crash window expires
- If no more crashes after the first one, the window would reset 600s later
- But with repeated crashes, window keeps active until last crash + 600s

**T+30min**: Auto-recovery attempt
- circuit_retry_at timestamp is reached
- Reconciler automatically resets:
  - circuit_open=0
  - circuit_open_count remains 1 (incremented, not reset)
  - crash_count=0
  - crash_window_start=NULL
  - status="stopped"
- Next reconciler tick starts container again

**Expected Behavior**:
- Crash detection is automatic every 30 seconds
- Circuit opens at exactly 5 crashes within a 10-minute window
- Once open, restarts are blocked for the configured duration (30min for first open)
- circuit_open_count increments each time the circuit opens (governs retry backoff)
- Auto-recovery is automatic when retry time elapses
- Operator can manually reset the circuit at any time

**Error Cases**:
- If circuit is open and operator tries to restart, operation fails
- If Docker daemon is down, reconciler skips the check (doesn't mark crashed)
- If container is already removed, reconciler only marks crashed in DB if it can confirm via Docker inspect


## Story 4: Circuit Breaker Recovery and Exponential Backoff

**Goal**: Service keeps crashing repeatedly over hours. Operator observes escalating recovery times.

**Setup**:
- Service has been through multiple crash-and-restart cycles

**First Outage (circuit_open_count=0 → 1)**:
- Five crashes detected within 10 minutes
- Circuit opens at T+0
- circuit_retry_at = T+0 + 30 minutes
- Operator is blocked from restarting for 30 minutes

**T+30min**: First auto-recovery attempt
- Circuit resets: circuit_open=0, status="stopped"
- Service restarts automatically next reconciliation cycle
- However, the same bug is still in the code
- Service crashes again within seconds

**Second Outage (circuit_open_count=1 → 2)**:
- Five more crashes detected
- Circuit opens again at T+30min+small
- circuit_retry_at = T+30min+small + 1 hour (backoff escalates)
- circuit_open_count=2
- Operator is now blocked for 60 minutes

**T+90min+**: Second auto-recovery attempt
- Circuit resets
- Service attempts to restart, but same bug causes immediate crash
- Crashes happen again

**Third Outage (circuit_open_count=2 → 3)**:
- Circuit opens
- circuit_retry_at = T+90min + 4 hours (max backoff)
- circuit_open_count=3
- Operator is blocked for 4 hours

**T+4 hours+**: Third auto-recovery attempt
- Circuit resets
- If operator has fixed the code and redeployed, service will run
- If bug is still present, cycle continues (still 4-hour backoff)

**Expected Behavior**:
- Backoff progression: 30min (1st open) → 1hr (2nd) → 4hr (3rd+)
- Backoff is based on circuit_open_count, not the wall-clock time
- Each time circuit opens, the next recovery window is longer
- Auto-recovery happens automatically once the timer expires
- Operator can manually reset at any time using `/reset` endpoint (sets circuit_open=0 immediately)

**Error Cases**:
- If auto-recovery happens but service still crashes immediately, new crash cycle starts fresh
- Multiple services can have open circuits independently


## Story 5: Health Check Failure Recovery

**Goal**: A service is running but its health check continuously fails. Reconciler detects and recovers.

**Setup**:
- Service "api-server" has a health check configured in its Dockerfile (HEALTHCHECK instruction)
- Container is running but the health endpoint is timing out

**T+0s**: Container starts
- Docker health check begins probing (default: 30s initial delay, then every 10s)
- Container is marked as "starting"

**T+30s**: Health check probe fails
- Docker health check reports: status="unhealthy"
- Container status in Docker: State.Health.Status="unhealthy"

**T+30s (next reconciliation tick, ~30 seconds later)**: Reconciler detects unhealthy state
- Reconciler inspects the container
- Sees info.HealthStatus="unhealthy"
- Immediately stops the container
- Records in database:
  - status="crashed"
  - crash_count=1
  - crash_window_start=now
  - last_error="container unhealthy (health check failed)"

**T+30.5s**: Docker restart policy kicks in
- Container is restarted automatically by Docker's restart policy
- Container comes back up and health checks resume

**T+60s**: Health checks pass on second attempt
- Docker now reports Health.Status="healthy"
- Container is serving requests normally

**Next reconciliation (T+60s+)**: Reconciler sees healthy container
- No action taken; service continues running

**Expected Behavior**:
- Health check failures are detected at reconciliation time (every 30s)
- Unhealthy containers trigger a stop, which allows Docker's restart policy to restart them
- Health check is image-defined; ah does not inject one
- Crash counters increment for unhealthy state (same as exit detection)
- Circuit breaker can open if 5 unhealthy events occur within 10 minutes

**Error Cases**:
- If health check is misconfigured, container may never become healthy
- If the root cause is a code bug, restarting won't fix it (same as exit-code crashes)


## Story 6: Manual Circuit Reset

**Goal**: Operator diagnoses and fixes the bug, then resets the circuit to allow the service to run.

**Setup**:
- Service has been down for 2 hours due to open circuit
- circuit_open=1, circuit_retry_at is in the future
- circuit_open_count=2 (so auto-recovery would take 1 hour minimum)

**T+0s**: Operator diagnoses the issue
- Reviews recent logs
- Identifies a missing dependency in the Python requirements.txt

**T+5min**: Operator fixes the code and rebuilds the image
- Pushes updated code to Git
- Triggers a new build: `POST /v1/services/api-service/builds`
- Build completes successfully
- Image is pushed to local registry

**T+10min**: Operator resets the circuit
- `POST /v1/services/api-service/reset`
- Response: {"status": "circuit breaker reset"}
- Database updated:
  - circuit_open=0
  - circuit_open_count stays the same (not reset, only circuit_open clears)
  - crash_count=0
  - crash_window_start=NULL
  - circuit_retry_at=NULL
  - status="stopped"

**T+11min**: Operator restarts the service
- `POST /v1/services/api-service/restart`
- Service restarts with the new image (from new build)
- status="running"
- Container is healthy

**T+15min**: Operator verifies
- `GET /v1/services/api-service`
- Response: status="running", circuit_open=false, crash_count=0
- Service is fully operational

**Expected Behavior**:
- Reset endpoint clears circuit_open=0 and related crash state
- Reset is idempotent: calling it twice succeeds both times
- After reset, operator can immediately restart the service
- Reset does not clear circuit_open_count (used for historical tracking)
- Reset allows the service to attempt startup again even if auto-recovery window hasn't elapsed

**Error Cases**:
- If service doesn't exist: 404
- If circuit is already closed: reset succeeds but is a no-op


## Story 7: Redeploy vs Rebuild Semantics

**Goal**: Operator redeploys a service with the same image vs rebuilding from source.

**Setup**:
- Service "ml-service" created from image "127.0.0.1:5000/ml-model:v1.2.3"
- Service has environment variables: MODEL_PATH=/models/bert, LOG_LEVEL=DEBUG
- Service is currently running

**Scenario A: Redeploy (no code change, just reconfig)**

1. Operator updates an environment variable
   - `POST /v1/services/ml-service/env`
   - Body: {"MODEL_PATH": "/models/gpt2", "LOG_LEVEL": "INFO"}
   - Response: {"status": "updated", "note": "restart will recreate the container with the new env vars"}

2. Operator redeploys to apply the new env
   - `POST /v1/services/ml-service/redeploy`
   - Response: {"id": "...", "status": "running", ...}
   - Internally: Restart is called
   - Container is stopped and removed
   - New container is started with the same image but new env vars
   - Environment variables are reloaded from database before restart

3. Operator verifies new config
   - `GET /v1/services/ml-service/env?reveal=true`
   - Response: MODEL_PATH="/models/gpt2", LOG_LEVEL="INFO"

**Expected Behavior**:
- Redeploy is semantically a restart (stop → start with current env)
- No build is triggered
- Image stays the same
- Env vars are reloaded from database

**Scenario B: Rebuild (code change)**

1. Operator pushes code changes to Git repo
   - Updates the model training logic
   - Commits to main branch

2. Operator rebuilds the service from source
   - `POST /v1/services/ml-service/builds`
   - Body: {"git_url": "https://github.com/user/ml-model.git", "git_ref": "main"}
   - Response: {"id": "build-123", "status": "queued"}
   - Build is queued and eventually runs

3. Build completes
   - `GET /v1/services/ml-service/builds/build-123`
   - Response: status="completed", image="127.0.0.1:5000/ml-model:build-123"
   - Build logs are available: `GET /v1/services/ml-service/builds/build-123/logs`

4. Operator redeploys with new image
   - New image is tagged in local registry
   - Service can now use the new image if explicitly restarted

**Expected Behavior**:
- Build creates a new image in the local registry
- Build is a separate operation from restart
- After build, operator must restart the service to use the new image
- Rebuild + restart is the full deployment cycle

**Difference Summary**:
- **Redeploy**: Restart with current env vars, same image
- **Build → Restart**: New image from source, then start
- Redeploy is faster (no build step)
- Build is needed when code changes


## Story 8: Deploy Timeout and Reconciliation

**Goal**: A build gets stuck. Reconciler detects and cleans up after 10 minutes.

**Setup**:
- Service "data-processor" is in status="deploying"
- Deployment started 5 minutes ago
- Build is hanging (network issue, Nixpacks timeout, etc.)

**T+0min**: Deployment starts
- `POST /v1/services/data-processor` or restart triggered
- Service status="deploying"
- updated_at=T+0

**T+5min**: Operator checks status
- `GET /v1/services/data-processor`
- Response: status="deploying"
- Operator waits (assumes it's still building)

**T+10min**: Reconciler timeout threshold reached
- Reconciler queries for services where status="deploying" AND updated_at < (now - 10min)
- Matches data-processor
- Updates database:
  - status="failed"
  - last_error="deploy timed out (reconciler)"
  - updated_at=now

**T+10min+1s**: Operator checks status again
- `GET /v1/services/data-processor`
- Response: status="failed", last_error="deploy timed out (reconciler)"

**T+10min+2s**: Operator restarts to retry
- `POST /v1/services/data-processor/restart`
- Container is stopped/removed if still running
- New deployment is attempted
- status="deploying"

**Expected Behavior**:
- Services stuck in "deploying" for more than 10 minutes are marked failed
- Reconciler runs every 30 seconds, so detection is within ~10 minutes 30 seconds
- last_error is recorded for debugging
- Operator must manually restart to retry
- Deploy timeout is a backstop against goroutine leaks and stuck builds

**Error Cases**:
- If Docker daemon is down, reconciler skips the timeout check (doesn't mark failed)
- If deploy completes right at the 10-minute mark, it's a race (usually OK, status will be "running" before timeout)


## Story 9: Crash Detection and Reconciliation

**Goal**: A service container crashes unexpectedly. Reconciler detects it and updates state.

**Setup**:
- Service "chat-bot" is status="running", container_id="xyz789"
- Container is healthy and serving requests
- Application code has a rare memory leak bug

**T+0s**: Container crashes
- Memory usage grows and OOM killer terminates the container
- Docker daemon marks container as exited with code 137 (SIGKILL)
- API layer is unaware at this moment

**T+30s**: Next reconciliation cycle
- Reconciler queries: `SELECT * FROM services WHERE status='running' AND container_id != ''`
- Gets chat-bot with container_id="xyz789"

- Reconciler calls ListContainersByLabel("ah.tenant", "")
- Container "xyz789" is NOT in the list (it's exited)

- Reconciler double-checks via InspectContainer("xyz789")
- Docker returns: Status="exited", ExitCode=137

- Reconciler marks the service crashed:
  - status="crashed"
  - crash_count=1 (first crash in new window)
  - crash_window_start=now
  - last_crashed_at=now
  - last_error="container exited (exit code 137)"
  - Circuit remains closed (1 crash < 5 threshold)

**T+31s**: Operator polls service status
- `GET /v1/services/chat-bot`
- Response: status="crashed", crash_count=1, circuit_open=false, last_error="container exited (exit code 137)"

**T+32s**: Operator restarts
- `POST /v1/services/chat-bot/restart`
- Container is restarted
- status="running"

**Expected Behavior**:
- Crash detection is automatic and non-intrusive
- Crash_count and crash_window_start are only updated if within the window
- If the window expires (600 seconds since first crash in window), next crash resets the window
- ExitCode is logged for debugging
- Circuit remains closed until 5 crashes occur

**Error Cases**:
- If Docker daemon is temporarily down, reconciler skips the service (doesn't mark crashed)
- If container was manually stopped via docker stop, reconciler treats it as a crash (accurate behavior)


## Story 10: Service Creation with Async Deployment

**Goal**: Operator creates a new service. Deployment happens asynchronously while API returns immediately.

**Setup**:
- Operator has an existing image in the local registry

**T+0s**: Operator creates the service
- `POST /v1/services`
- Body: {"name": "web-app", "image": "127.0.0.1:5000/nginx:latest", "port": 8080}
- Response: 201 Created
  - Body: {"id": "svc-abc", "name": "web-app", "status": "deploying", "image": "127.0.0.1:5000/nginx:latest", "port": 8080, ...}
- API returns immediately (< 100ms)
- Service record is written to database
- status="deploying"

**T+0.5s**: Deployment goroutine starts
- Background goroutine calls svcManager.Deploy(ctx, tenantID, serviceID)
- Deploy has a timeout of 10 minutes
- Docker pull/run operations happen in the background

**T+1s**: Operator checks status immediately
- `GET /v1/services/svc-abc`
- Response: status="deploying"
- Container might not exist yet; client should poll

**T+5s**: Image pull and container setup completes
- Docker finishes pulling the image
- Container is created and started
- Deployment goroutine updates database:
  - status="running"
  - container_id="container-abc123"
  - url="http://svc-abc.localhost"
  - updated_at=now

**T+6s**: Operator checks status again
- `GET /v1/services/svc-abc`
- Response: status="running", container_id="...", url="..."
- Service is ready to use

**T+7s**: Operator accesses the service
- HTTP request to http://svc-abc.localhost:8080
- Traefik routes it to the container
- Nginx responds

**Expected Behavior**:
- Create returns immediately with status="deploying"
- Deployment happens in a bounded goroutine (10-min timeout)
- Client is expected to poll or stream logs to observe progress
- Once status="running", service is accessible
- If deployment fails, status="failed" and last_error is set

**Error Cases**:
- If Docker daemon is down: status="failed", last_error="Docker unavailable"
- If image doesn't exist: status="failed", last_error="image not found"
- If port is already in use: status="failed", last_error="port conflict"
- If deploy goroutine times out: reconciler marks status="failed" at 10min mark


## Story 11: Log Streaming with Tail and Pagination

**Goal**: Operator retrieves specific ranges of logs without overwhelming the network.

**Setup**:
- Service "worker" has been running for hours
- Logs are accumulated (1000+ lines)

**Scenario A: Last N lines only**

1. Operator retrieves last 100 lines
   - `GET /v1/services/worker/logs?tail=100`
   - Response: plain text, ~10KB
   - Returns most recent 100 lines
   - Connection closes

2. Operator retrieves last 1000 lines
   - `GET /v1/services/worker/logs?tail=1000`
   - Response: plain text, ~100KB
   - Returns most recent 1000 lines
   - Connection closes

**Scenario B: Follow mode with large tail**

1. Operator starts follow with 50-line tail
   - `GET /v1/services/worker/logs?follow=true&tail=50`
   - Response: 200 OK, streaming
   - Initial 50 lines sent immediately, flushed to client
   - Connection stays open

2. New logs appear
   - Client receives updates as they appear (within Docker logging latency)
   - Each line is flushed immediately (http.Flusher)

3. Operator closes the stream
   - Client closes connection gracefully
   - Server closes the log reader

**Expected Behavior**:
- tail parameter limits the initial output (default 100, max 10,000)
- Non-follow requests return exactly that many lines and close
- Follow requests return the tail and continue streaming
- Tail=10,000 is a hard limit (prevents DoS)
- Old logs are read from Docker's log driver (file-based usually)

**Error Cases**:
- If tail parameter is invalid (negative, non-numeric): 400 Bad Request
- If tail > 10,000: silently capped at 10,000
- If service doesn't exist: 404
- If container has no logs yet: returns empty or placeholder


## Story 12: Service Recovery After Network Partition

**Goal**: Network glitch causes Docker daemon to appear down. Service eventually recovers.

**Setup**:
- Service "reliable-api" is status="running"
- A network partition isolates the API server from the Docker daemon

**T+0s**: Network partition occurs
- API cannot communicate with Docker daemon
- All Docker calls start timing out

**T+0.5s**: Operator attempts to restart the service
- `POST /v1/services/reliable-api/restart`
- Request times out after 30 seconds
- Response: 503 Service Unavailable or timeout error

**T+30s+**: Reconciliation cycle runs
- Reconciler tries to list containers: ListContainersByLabel(...)
- Docker call times out
- Reconciler returns an error: "list ah containers (skipping reconciliation): context deadline exceeded"
- **Important**: Reconciler does NOT mark the service as crashed (transient error handling)
- Database state remains unchanged: status="running"

**T+1min**: Network partition heals
- Docker daemon is reachable again
- API server can communicate with Docker

**T+2min**: Next reconciliation cycle
- Reconciler successfully lists containers
- Inspects reliable-api container
- Container is still running (was never marked crashed)
- No action taken

**T+2min+5s**: Operator retries restart
- `POST /v1/services/reliable-api/restart`
- Succeeds: 200 OK
- Service is restarted cleanly

**Expected Behavior**:
- Transient Docker errors do NOT trigger crash marking
- Reconciler explicitly handles timeout and connection errors
- isNotFoundError() is strict: only marks crashed on definitive 404s
- Service state survives transient network issues
- Once Docker is reachable, normal operations resume

**Error Cases**:
- If partition lasts longer than service's own timeout, service might actually crash
- If partition is permanent, operator must investigate Docker infrastructure


## Story 13: Environment Variable Restart Semantics

**Goal**: Operator changes configuration and ensures it's applied correctly.

**Setup**:
- Service "api-gateway" is running with DATABASE_URL="postgres://localhost/prod"
- Operator wants to change to a new database

**T+0s**: Operator updates env vars
- `POST /v1/services/api-gateway/env`
- Body: {"DATABASE_URL": "postgres://new-host/prod"}
- Response: {"status": "updated", "note": "restart will recreate the container with the new env vars"}
- Database is updated: services.env_vars table is encrypted and stored

**T+1s**: Operator verifies the change (without revealing)
- `GET /v1/services/api-gateway/env`
- Response: {"DATABASE_URL": "***"} (masked)
- Only the key is shown, not the value (for security)

**T+2s**: Operator verifies with reveal=true
- `GET /v1/services/api-gateway/env?reveal=true`
- Response: {"DATABASE_URL": "postgres://new-host/prod"}
- Full values shown only with explicit reveal parameter

**T+3s**: Service is still running with OLD config
- Env vars are stored but not yet applied
- Container is still using the old connection string

**T+4s**: Operator restarts to apply
- `POST /v1/services/api-gateway/restart`
- Container is stopped and removed
- New container is started
- Env vars are loaded from database and passed to Docker
- status="running"

**T+5s**: Verify new config is active
- Service connects to new database
- Requests work with new configuration

**Expected Behavior**:
- Env vars are stored encrypted in the database
- Setting env vars does NOT restart the container automatically
- Restart loads env vars from database and applies them
- Reveal parameter is required to see decrypted values (security)
- Max 100 env vars per service
- Env var keys and values are validated (no LD_PRELOAD, etc.)

**Error Cases**:
- If setting too many env vars (>100): 400 Bad Request
- If env var value is too long (>32KB): 400 Bad Request
- If restart happens before env set completes: race condition (unlikely but possible)


---

**End of Service Lifecycle Control Stories**

The 13 stories above cover the main UX paths for service lifecycle operations:
1. Basic operations (start/stop/restart)
2. Log streaming with follow mode
3. Circuit breaker triggering
4. Circuit breaker recovery and escalating backoff
5. Health check failures
6. Manual circuit reset
7. Redeploy vs rebuild
8. Deploy timeout detection
9. Crash detection and reconciliation
10. Async deployment creation
11. Log streaming pagination
12. Recovery from network issues
13. Environment variable restart semantics

Each story is realistic and grounded in the actual implementation.
