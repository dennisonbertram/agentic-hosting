# Deployment History & Redeploy — UX Path Stories

## Story 1: Viewing Deployment History After Initial Service Creation

**Actor:** Emma, DevOps engineer for an AI training pipeline

**Goal:** Confirm her service deployed successfully and see the deployment timestamp

**Scenario:**
1. Emma creates a service with `POST /v1/services` (image: `pytorch-trainer:3.11`)
2. API returns 201 with status "deploying"
3. She immediately queries `GET /v1/services/{serviceID}/deployments` to check status
4. Response shows one DeploymentRecord with status="deploying", image="pytorch-trainer:3.11", started_at=<timestamp>
5. Emma can verify the deployment is tracked and uses the correct image

**Outcome:** Emma gains confidence the service is being deployed and can monitor its progress via polling or log streaming


## Story 2: Understanding Restart vs. Redeploy Terminology

**Actor:** Marcus, backend engineer unfamiliar with the platform

**Goal:** Understand the semantic difference between restart and redeploy

**Scenario:**
1. Marcus reads the API docs which state: "POST /redeploy is an explicit alias for restart — no new build is triggered"
2. He deploys a service from a built image (127.0.0.1:5000/my-app:v1.2)
3. He updates environment variables with `POST /services/{serviceID}/env`
4. He calls `POST /services/{serviceID}/redeploy` to pick up the new env vars
5. The API stops the old container, reloads env vars from the database, and starts a new container
6. Marcus observes: no Nixpacks build occurred, image unchanged, only the container recreated
7. He later tries `POST /services/{serviceID}/restart` with identical behavior

**Outcome:** Marcus understands that redeploy and restart are equivalent — both recreate the container without triggering a new build. He can choose either endpoint based on semantic clarity


## Story 3: Redeploying a Service After Environment Variable Changes

**Actor:** Priya, cloud engineer managing an agent service

**Goal:** Apply new environment variables without rebuilding the application

**Scenario:**
1. Priya's service is running with image "agents:latest" and env var LLM_MODEL="gpt-4"
2. She updates the LLM_MODEL to "gpt-4-turbo" via `PATCH /services/{serviceID}/env`
3. API responds with: "restart will recreate the container with the new env vars"
4. She calls `POST /services/{serviceID}/redeploy`
5. Service stops (~2 second timeout), container is removed
6. New container starts with the same image ("agents:latest") but with LLM_MODEL="gpt-4-turbo"
7. Service status transitions from "running" → (briefly) → "running" again
8. Priya verifies the change took effect by querying the service and seeing updated_at timestamp

**Outcome:** Priya can apply configuration changes without waiting for a full rebuild, keeping deployment cycles fast for config iterations


## Story 4: Tracking Deployment State During Circuit Breaker Open

**Actor:** Jake, on-call engineer for a flaky service

**Goal:** Understand why redeploy is blocked and how to recover

**Scenario:**
1. Jake's service "unstable-worker" crashes 5 times in 10 minutes
2. Circuit breaker automatically opens (circuit_open=true in service state)
3. Jake tries to redeploy with `POST /services/{serviceID}/redeploy`
4. API returns 409 Conflict: "circuit breaker is open: service has crashed too many times; use POST /reset to clear"
5. Jake reads the deployment history with `GET /services/{serviceID}/deployments` and sees the last deployment status reflects the crashed state
6. Jake calls `POST /services/{serviceID}/reset` to reset the circuit breaker
7. He then calls `POST /services/{serviceID}/redeploy` successfully
8. New container starts fresh
9. Updated deployment record reflects the new start time

**Outcome:** Jake understands the circuit breaker protection mechanism and knows to reset it before retrying redeploy


## Story 5: Redeploy Fails Due to Disk Space

**Actor:** Alex, infrastructure operator monitoring resource constraints

**Goal:** Handle redeploy failure gracefully when disk is full

**Scenario:**
1. Alex's cluster is at 87% disk utilization across `/var/lib/ah` and `/var/lib/docker`
2. He attempts `POST /services/{serviceID}/redeploy`
3. Service manager runs disk check preflight: `/var/lib/ah` = 87%, `/var/lib/docker` = 89% (both < 90% threshold)
4. Redeploy proceeds normally
5. Later, utilization hits 91% in `/var/lib/ah`
6. Another redeploy attempt fails: `503 Service Unavailable: disk check: ...`
7. Alex must delete old volumes/images or expand storage before retrying
8. The service deployment history still reflects the last successful deployment

**Outcome:** Alex receives clear feedback about disk pressure and knows to clean up resources before retrying


## Story 6: Viewing Deployment History for a Long-Running Service

**Actor:** Chen, compliance officer auditing service changes

**Goal:** Trace all deployment events for a service over time

**Scenario:**
1. Chen's service "payment-processor" has been running for 3 months
2. She queries `GET /v1/services/{serviceID}/deployments` expecting full history
3. Current implementation returns only the latest deployment record (derived from service.updated_at)
4. She sees: `[{"id": "deploy-...", "service_id": "...", "status": "running", "image": "payments:v2.1", "started_at": 1711027200}]`
5. Chen realizes the deployment table is not yet implemented (issue #6) and notes this limitation
6. She requests the feature via her product manager to enable full audit trails
7. In the interim, she cross-references activity logs and service status history to reconstruct timeline

**Outcome:** Chen understands the current limitation and can plan for the dedicated deployments table feature


## Story 7: Redeploy During Container Crash Recovery

**Actor:** Sofia, agent orchestration engineer

**Goal:** Quickly recover a service that crashed unexpectedly

**Scenario:**
1. Sofia's service "agent-executor" crashes due to an out-of-memory condition
2. Container is gone; service.container_id is stale in the database
3. Sofia tries `POST /services/{serviceID}/redeploy`
4. Restart handler checks: container_id is invalid (empty or stale after crash)
5. API returns 409 Conflict: "service has no container — deploy the service first"
6. Sofia realizes she needs to trigger a full deploy from the original image
7. She calls `POST /services/{serviceID}` with the original image and proper env vars
8. New deployment begins; after ~30s, the service is running again
9. Deployment history now shows a new record with the fresh container

**Outcome:** Sofia learns that redeploy assumes a valid container exists; full deploy is needed after crash


## Story 8: Redeploy with Port Mismatch Detection

**Actor:** Danny, junior engineer testing a service redeploy

**Goal:** Ensure port configuration is preserved during redeploy

**Scenario:**
1. Danny created a service on port 8080
2. He calls `POST /services/{serviceID}/redeploy`
3. Service manager reads the service record from DB (Port=8080), stops and removes the old container
4. New container is created with the same port (8080) and network config
5. Traefik configuration is refreshed for the service (reads DNSLabel and BaseDomain from service state)
6. Service routing continues uninterrupted; no connection drops
7. Danny verifies the deployment history shows no port changes

**Outcome:** Danny confirms that redeploy is truly non-disruptive for routing and configuration


## Story 9: Redeploy Blocked by Deployment Queue Overflow

**Actor:** Rajesh, load testing an agent platform

**Goal:** Understand backpressure when many redeploys are queued

**Scenario:**
1. Rajesh launches 50 services in rapid succession
2. Each service creation queues a deploy (max 5 concurrent, max 20 queued globally)
3. All 50 services eventually deploy successfully as queue drains
4. Rajesh then triggers redeploy for 15 services simultaneously
5. First 5 redeploys enter the semaphore immediately
6. Next 10 wait in the queue (now at 15/20)
7. Redeploy attempt #51 arrives, but queue is full (20/20)
8. API returns error: "deploy queue full; try again later"
9. Rajesh backs off and retries after 5 seconds; slot opens up
10. Redeploy succeeds
11. Deployment history shows the eventual successful deployment

**Outcome:** Rajesh learns about deployment concurrency limits and implements exponential backoff for bulk redeploys


## Story 10: Redeploy Preserves Environment Variables After Update

**Actor:** Yuki, configuration-driven application owner

**Goal:** Verify environment variables persist through redeploy

**Scenario:**
1. Yuki's service has API_KEY and DATABASE_URL set
2. She reveals the env vars: `GET /services/{serviceID}/env?reveal=true`
3. Response includes: `{"API_KEY": "sk-...", "DATABASE_URL": "postgres://..."}`
4. She redeploys: `POST /services/{serviceID}/redeploy`
5. Restart handler loads env vars from the database before starting the new container
6. New container inherits the same API_KEY and DATABASE_URL
7. Yuki confirms application connectivity by checking logs: no AUTH errors
8. She queries `/services/{serviceID}/env?reveal=true` again and sees identical values

**Outcome:** Yuki trusts that redeploy is configuration-preserving and safe for rapid iterations


## Story 11: Redeploy Timeline with Concurrent Operations

**Actor:** Marcus, debugging a race condition in service startup

**Goal:** Understand the precise timeline of a redeploy operation

**Scenario:**
1. Service is running with container ID "abc123..."
2. Marcus calls `POST /services/{serviceID}/redeploy` at T=0
3. API acquires deployment backpressure (queue + semaphore + per-service lock)
4. At T=0.5s, Restart handler calls `docker.StopContainer("abc123...")`
5. At T=0.8s, handler calls `docker.RemoveContainer("abc123...")`
6. At T=0.9s, handler calls `docker.StopAndRemoveByName("ah-tenant-service")`
7. At T=1.2s, buildServiceContainerConfig is called to rebuild container spec
8. At T=1.5s, `docker.CreateContainer` is called, new container "def456..." is created
9. At T=1.8s, new container is started
10. At T=2.0s, health check begins (circuit breaker reset for first check)
11. At T=2.5s, API returns 200 with updated service (status="running", container_id="def456...")
12. Deployment history shows single record with new container_id and updated timestamp

**Outcome:** Marcus understands the sub-3-second redeploy timeline and can correlate with application logs


## Story 12: Redeploy Failure with Cleanup Verification

**Actor:** Lisa, debugging a deployment that fails mid-way

**Goal:** Ensure orphaned containers are cleaned up if redeploy fails

**Scenario:**
1. Service has old container "old-123..."
2. Restart handler stops and removes old container successfully
3. Handler attempts to create new container but Docker API returns an error: "invalid port binding"
4. The function returns an error; DB is not updated with the new container
5. Service state still shows old container_id and updated status as before
6. Lisa queries the service and sees it's still "running" (actually broken)
7. She calls `/services/{serviceID}/logs` and sees the application crashed
8. She checks deployment history: still shows the old deployment
9. Lisa now fixes the root cause (e.g., port conflict with another service)
10. She redeploys again, and this time it succeeds

**Outcome:** Lisa understands that partial failures leave the service in a recoverable state, and she can retry


## Story 13: Viewing Deployment Status Across Multiple Services

**Actor:** Aisha, platform admin monitoring a multi-service tenant

**Goal:** Get a quick overview of deployment status for all services

**Scenario:**
1. Aisha lists all services: `GET /v1/services?limit=50`
2. Response includes 10 services with statuses: "running", "deploying", "stopped", "crashed", "failed"
3. For each service, she can query `GET /services/{serviceID}/deployments` to see the last deployment timestamp
4. Services with status="crashed" show a LastCrashedAt timestamp from the deployment record
5. Aisha identifies 2 services that need redeploy: "agent-1" and "agent-2"
6. She batch-triggers redeploys for both
7. Polling deployment status every 5s shows the transition to "running"
8. Deployment records are updated with new started_at timestamps

**Outcome:** Aisha can quickly assess platform health by scanning service statuses and deployment timestamps


## Story 14: Idempotent Redeploy with Retry

**Actor:** Tom, building a CLI tool that retries redeploy on network failure

**Goal:** Safely retry a redeploy request without creating duplicate deployments

**Scenario:**
1. Tom's CLI calls `POST /services/{serviceID}/redeploy` with an idempotency key (SHA256 of body, default is empty for simple redeploy)
2. Network disconnects before response is received
3. CLI retries the same request with the same body
4. Server detects idempotency key match (10min window, max 50 per tenant)
5. Server returns the cached response from the first attempt (200 OK with updated service state)
6. No second redeploy is triggered; deployment count stays at 1
7. Tom's CLI presents the same result to the user

**Outcome:** Tom can build reliable automation without worrying about duplicate deployments


## Story 15: Redeploy with Service Name Update (Metadata Only)

**Actor:** Sofia, refactoring service names in her application

**Goal:** Rename a service and verify redeploy works with the new name

**Scenario:**
1. Sofia updates service name from "old-agent" to "new-agent" via `PATCH /services/{serviceID}`
2. Service name is updated; container name remains deterministic based on serviceID (not name)
3. She calls `POST /services/{serviceID}/redeploy`
4. Restart handler uses serviceID to find old container (name format: "ah-tenant-service")
5. Stops and removes old container
6. Creates new container with same deterministic name (still "ah-tenant-service")
7. Traefik routes are updated to reflect the DNS label (derived from service name)
8. New service name is visible in all API responses
9. Deployment history shows the deployment with the updated service reference

**Outcome:** Sofia can rename services without affecting the container identity or requiring manual DNS updates
