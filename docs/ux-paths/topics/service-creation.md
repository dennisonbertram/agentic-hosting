# Service Creation & Deployment — UX Path Stories

## STORY-001: Deploy Service from Docker Hub Image
**Type**: short
**Topic**: Service Creation & Deployment
**Persona**: AI Agent Developer
**Goal**: Deploy a pre-built LLM inference service from Docker Hub
**Preconditions**: Tenant has valid API key, Docker service running, base domain configured

### Steps
1. `POST /v1/services` with `{"name": "llm-chat-api", "image": "mistral-7b:latest", "port": 8000}` → 201 Created, status="deploying"
2. Poll `GET /v1/services/svc-abc123` → status still "deploying" (image pull in progress)
3. Wait 45 seconds, poll again → status="running", url="llm-chat-api.tenant.example.com"
4. Verify service is routable: `curl https://llm-chat-api.tenant.example.com/health` → 200 OK
5. `GET /v1/services/svc-abc123/deployments` → One deployment record with image and container_id

### Variations / Edge Cases
- **Slow Pull**: Large image (5GB) takes 2+ minutes; status="deploying" during entire pull
- **Network timeout**: If pull fails (e.g., hub.docker.com unreachable), status transitions to "failed", LastError contains pull error
- **Port Conflict**: Multiple requests with same port on same tenant bridge; Docker assigns unique port internally, Traefik routes via hostname

---

## STORY-002: Deploy with Inline Environment Variables
**Type**: short
**Topic**: Service Creation & Deployment
**Persona**: LLM Fine-tuner
**Goal**: Deploy inference service with model path and auth token
**Preconditions**: Service manager available, tenant quota allows services

### Steps
1. `POST /v1/services` with body:
   ```json
   {
     "name": "gpt-finetuned",
     "image": "gpt-ft:v2",
     "port": 9000,
     "env": {
       "MODEL_PATH": "/models/gpt-ft-v2",
       "AUTH_TOKEN": "secret-token-xyz",
       "LOGLEVEL": "info"
     }
   }
   ```
2. Response: 201, status="deploying", service record created
3. Poll `GET /v1/services/svc-def456` → status="running" (after ~10s)
4. `GET /v1/services/svc-def456/env?reveal=true` → Returns all env vars including secrets
5. Stop/Restart service: `POST /v1/services/svc-def456/restart` → Recreates container with same env vars

### Variations / Edge Cases
- **Invalid Env Key**: Name like `12INVALID` or `LD_PRELOAD` → 400 Bad Request, validation error message
- **Env Too Large**: 101+ environment variables → 400 Bad Request
- **Env Value Truncation**: 32KB limit per value; oversized values → 400 Bad Request before creation
- **Env Persistence**: Env vars survive restarts but NOT redeploys from scratch (would require fresh creation with same env)

---

## STORY-003: Create from Snapshot to Clone Environment
**Type**: short
**Topic**: Service Creation & Deployment
**Persona**: Agent Infrastructure Engineer
**Goal**: Rapidly fork a production inference service to staging
**Preconditions**: Snapshot of production service exists (image + env vars captured), snapshot data valid

### Steps
1. `GET /v1/snapshots` → List available snapshots, find "prod-llm-2026-03-21"
2. `GET /v1/snapshots/snap-prod123` → Confirm image, env vars, port
3. `POST /v1/services?from_snapshot=snap-prod123` with body `{"name": "staging-llm-clone"}` → 201 Created, status="deploying"
4. Wait 2-3 seconds, `GET /v1/services/svc-new456` → status="running", url="staging-llm-clone.tenant.example.com"
5. Verify env vars match: `GET /v1/services/svc-new456/env?reveal=true` → Same values as snapshot
6. Compare deployments: both services have identical image

### Variations / Edge Cases
- **Snapshot Image Unavailable**: Image was pruned from local registry → 400 Bad Request, "snapshot contains invalid image"
- **Corrupted Env in Snapshot**: Snapshot data corrupted in DB → 500 Internal Server Error, audit log shows restore failed
- **Port Collision**: Snapshot and new service both try port 8000; Traefik routes by hostname, no collision
- **Name Exists**: New service name "staging-llm-clone" already used → 409 Conflict

---

## STORY-004: Monitor Deployment Progress with Polling
**Type**: short
**Topic**: Service Creation & Deployment
**Persona**: DevOps Automation Script
**Goal**: Track deployment completion without blocking
**Preconditions**: Service created, deployment in progress

### Steps
1. `POST /v1/services` → 201, status="deploying", get serviceID
2. Loop: `GET /v1/services/{serviceID}` every 2s, check status
   - First 30s: status="deploying"
   - At 35s: status="running", url populated, container_id populated
3. When status != "deploying", exit loop
4. If status="failed" or "crashed", read LastError field for diagnosis
5. On success, use returned url for health checks

### Variations / Edge Cases
- **Slow Image Pull**: 5GB image takes 120+ seconds; status="deploying" for full duration
- **Disk Full During Deploy**: At 40s, status transitions to "failed", LastError="disk check: device at 95% capacity"
- **Circuit Breaker Triggered**: After successful creation but before first start, if container crashes 5 times in 10min, status="crashed", CircuitOpen=true
- **Service Deleted Mid-Deploy**: Admin deletes while deploying; deployment loop fails, container orphaned and removed by GC (5min loop)

---

## STORY-005: Handle Deploy Queue Backpressure
**Type**: medium
**Topic**: Service Creation & Deployment
**Persona**: Multi-agent Cluster Controller
**Goal**: Deploy 10 services concurrently, handle queue limits
**Preconditions**: maxConcurrentDeploys=5, maxQueuedDeploys=20

### Steps
1. Issue 25 `POST /v1/services` requests in parallel
   - First 5: Accepted, status="deploying", deployment starts immediately
   - Next 15: Accepted, status="deploying", queued for a deploy slot
   - Next 5: Rejected with 503 Service Unavailable, "deploy queue full; try again later"
2. Wait 30 seconds for first batch to complete (containers running)
   - Queued batch automatically promoted to deploy slots
3. Status of rejected services still "pending" in DB
4. Retry rejected batch: `POST /v1/services` → Now accepted as earlier deploys complete

### Variations / Edge Cases
- **Queue Timeout**: Service stuck in queue > 10 minutes → deployment context expires, service left in "deploying" state indefinitely
- **Concurrent Deletes**: Delete service during deployment → service record removed, orphan container cleaned up by GC
- **Network Partition**: Deploy goroutine blocked on Docker API → slot held until 10min timeout, blocks other deploys
- **Resource Exhaustion**: All deploy slots in use; new request waits in queue → clients should implement exponential backoff

---

## STORY-006: Deploy Local Registry Image (Nixpacks Build Output)
**Type**: short
**Topic**: Service Creation & Deployment
**Persona**: Continuous Integration Pipeline
**Goal**: Deploy service from freshly built Docker image in local registry
**Preconditions**: Image built and pushed to `127.0.0.1:5000/myapp:v1.0`, service not yet created

### Steps
1. Nixpacks build completes, image tagged `127.0.0.1:5000/myapp:v1.0`
2. `POST /v1/services` with `{"name": "myapp-v1", "image": "127.0.0.1:5000/myapp:v1.0", "port": 3000}` → 201, status="deploying"
3. Deploy skips pull (image already local), container starts in ~2s
4. `GET /v1/services/svc-build123` → status="running", url="myapp-v1.tenant.example.com"
5. Service immediately passes health checks

### Variations / Edge Cases
- **Image Doesn't Exist**: Image `127.0.0.1:5000/myapp:v1.0` not in local registry → deployment fails, status="failed", LastError="pull image: no such image"
- **Untrusted Registry**: Attempt `{"image": "evil-registry.com/malware:latest"}` → 400 Bad Request, "invalid image format (only Docker Hub and 127.0.0.1:5000 allowed)"
- **Image Missing Tag**: `{"image": "127.0.0.1:5000/myapp"}` → Accepted but Docker defaults to `:latest`
- **Registry Connectivity**: Local registry unavailable → Status "deploying" hangs until timeout, then fails

---

## STORY-007: Recover from Failed Deployment
**Type**: medium
**Topic**: Service Creation & Deployment
**Persona**: Service Operator
**Goal**: Diagnose and retry failed deployment
**Preconditions**: Service in "failed" state with LastError populated

### Steps
1. `GET /v1/services/svc-failed789` → status="failed", LastError="container start failed: OOM killer"
2. `GET /v1/services/svc-failed789/logs?tail=50` → Last 50 lines show OOM errors
3. Check tenant quotas: `GET /v1/tenant/usage` → Memory usage at 95%
4. Increase service quota or reduce memory footprint (not yet implemented in v0.4.0)
5. `POST /v1/services/svc-failed789/restart` → Attempts to restart; still fails due to memory
6. Update image to lighter variant:
   - Create snapshot of old config: `POST /v1/services/svc-failed789/snapshots`
   - Delete old service: `DELETE /v1/services/svc-failed789`
   - Create new service with lighter image from same snapshot

### Variations / Edge Cases
- **Non-Retryable Error**: Image doesn't exist → Restarts also fail indefinitely
- **Circuit Breaker Active**: After multiple failures, CircuitOpen=true → Must call `POST /v1/services/svc-failed789/reset` before restart
- **Container Running After "Failed"**: Race condition where container crashed but not yet reconciled; status shows "failed" but container still running → Reconciler detects mismatch, stops container
- **Partial Deployment**: Network created but container failed to start → Orphan network left behind (must be cleaned manually or by GC)

---

## STORY-008: Understand Service Status Lifecycle
**Type**: short
**Topic**: Service Creation & Deployment
**Persona**: Monitoring & Alerting System
**Goal**: Interpret service status for alerting rules
**Preconditions**: Multiple services in various states

### Steps
1. Create service → status="deploying" (1-60 seconds)
2. If deployment succeeds → status="running", ContainerID populated, URL routable
3. If container crashes 1-4 times in 10min → status="crashed", CrashCount increments, LastCrashedAt updated
4. If container crashes 5+ times in 10min → status="crashed" AND CircuitOpen=true (auto-stop to prevent restart loop)
5. Query all services: `GET /v1/services?limit=50` → Filter by status field
6. Alert condition: CircuitOpen=true AND status="crashed" → Requires manual `POST /reset` before restart

### Variations / Edge Cases
- **Status "stopped"**: User called `POST /stop`, no crash involved
- **Status "running" but No Container**: Race condition; reconciler will correct, but may appear temporarily in API response
- **Crash Window Timing**: Crashes older than 10 minutes don't count; window rolls forward
- **Container Exit Code**: Reconciler checks exit code, treats code 0 as clean stop (not a crash), non-zero as crash

---

## STORY-009: Deploy with Custom Port
**Type**: short
**Topic**: Service Creation & Deployment
**Persona**: Multi-service Tenant
**Goal**: Deploy services on specific ports to avoid collision
**Preconditions**: Tenant can choose ports 1024-65535

### Steps
1. `POST /v1/services` with `{"name": "api-svc", "image": "node:20", "port": 8080}` → 201, service.Port=8080
2. `POST /v1/services` with `{"name": "web-svc", "image": "nginx:latest", "port": 8080}` → Also accepted (different services, same logical port)
3. Both containers receive PORT=8080 env var
4. Within container: service binds to 0.0.0.0:8080
5. Traefik routes incoming requests to correct container based on hostname (api-svc.domain vs web-svc.domain)
6. Users access: `https://api-svc.tenant.example.com` (routes to api-svc internal port 8080)

### Variations / Edge Cases
- **Port from ENV**: Container defines PORT env var, overrides requested port → `{"port": 8080, "env": {"PORT": "3000"}}` → Container starts on 3000
- **Invalid Port**: `{"port": 0}` or `{"port": 99999}` → Defaults to 8000; no validation error
- **Port Already Bound**: If Docker can't bind port internally → Deployment fails, status="failed", LastError contains bind error
- **No Port Specified**: `{"port": null}` → Defaults to 8000

---

## STORY-010: Validate Image References
**Type**: short
**Topic**: Service Creation & Deployment
**Persona**: Security Administrator
**Goal**: Ensure only trusted images can be deployed
**Preconditions**: Image validation rules configured (Docker Hub + local registry only)

### Steps
1. Valid Docker Hub images:
   - `"image": "ubuntu"` → Accepted (defaults to `ubuntu:latest`)
   - `"image": "pytorch/pytorch:2.0"` → Accepted
   - `"image": "myuser/myapp:v1.0"` → Accepted
2. Valid local registry images:
   - `"image": "127.0.0.1:5000/myapp:latest"` → Accepted
   - `"image": "localhost:5000/builds/service:20260321"` → Accepted
3. Invalid images (rejected with 400):
   - `"image": "evil.com/malware:latest"` → "invalid image format"
   - `"image": "docker.io/ubuntu"` (explicit registry) → "invalid image format"
   - `"image": ""` → "image is required"
   - `"image": "..." (256+ chars)` → "image reference too long"

### Variations / Edge Cases
- **Registry Allowlist Bypass**: Attacker tries DNS rebinding on 127.0.0.1:5000 → Validation passes, but would connect to external registry if configured that way (not our concern)
- **Image Tag Injection**: `"image": "ubuntu'; DROP TABLE--:latest"` → Validation fails (regex doesn't match)
- **Normalized Image Name**: User provides `"ubuntu"` → Stored as exactly "ubuntu" in DB, Docker interprets as "ubuntu:latest"

---

## STORY-011: Quota Enforcement on Service Creation
**Type**: medium
**Topic**: Service Creation & Deployment
**Persona**: Tenant with Service Limit
**Goal**: Create 5 services, hit quota, understand error
**Preconditions**: Tenant has max_services quota of 5

### Steps
1. Create 5 services: `POST /v1/services` × 5 → All succeed, 201 responses
2. Attempt 6th service: `POST /v1/services` → 409 Conflict, "service quota exceeded (max 5)"
3. `GET /v1/tenant/usage` → Shows services_used=5, services_quota=5
4. Delete one service: `DELETE /v1/services/svc-1` → 204 No Content
5. Verify quota available: `GET /v1/tenant/usage` → Shows services_used=4, services_quota=5
6. Create new service: `POST /v1/services` → Now succeeds, services_used=5

### Variations / Edge Cases
- **Suspended Tenant**: Tenant flagged as suspended → All create attempts rejected with 403 "tenant is suspended"
- **Quota Not Enforced**: If tenant_quotas row missing → Assumed unlimited (should not happen in production)
- **Service in "Deploying"**: Service created but deployment failed; service still counts toward quota until deleted
- **Concurrent Quota Check**: Two requests simultaneously; both pass quota check, only one succeeds (SQL enforces uniqueness)

---

## STORY-012: Deployment Failure Diagnostics
**Type**: medium
**Topic**: Service Creation & Deployment
**Persona**: Debugging Engineer
**Goal**: Determine why deployment failed and take remediation
**Preconditions**: Service deployment failed, status="failed" with LastError populated

### Steps
1. Service creation succeeds but deployment fails: `GET /v1/services/svc-fail` → status="failed", LastError="network setup failed: could not create network (permission denied)"
2. Check system health: `GET /v1/system/health/detailed` → Docker version, gVisor version, disk usage, system status
3. Retrieve logs: `GET /v1/services/svc-fail/logs?tail=100` → Container may have started briefly; logs show error
4. Decision tree:
   - **Disk Full**: LastError="disk check: device at 95% capacity" → Clean up images/volumes, retry
   - **Image Bad**: LastError="run container: image entrypoint failed" → Verify image locally, test image
   - **Resource Limits**: LastError="run container: OOM killer" → Check tenant quotas, increase limit or reduce container resource needs
   - **Network Error**: LastError="network setup failed" → Docker daemon issue, check `systemctl status docker`
5. If retriable: `POST /v1/services/svc-fail/restart` → Attempts deployment again
6. If not retriable: Delete and recreate with corrected parameters

### Variations / Edge Cases
- **Logs Lost**: Container failed so quickly logs never written → LastError is only diagnostic
- **Transient Docker Errors**: Temporary API unavailability → Error message generic; manual retry may succeed
- **Circuit Breaker Active**: Multiple failures → CircuitOpen=true, must reset before retry
- **Partial Container State**: Container created but failed to start → Docker cleanup may be incomplete; reconciler corrects on next cycle

---

## STORY-013: Multi-Deployment Concurrency Limits
**Type**: long
**Topic**: Service Creation & Deployment
**Persona**: Large Workload Scheduler
**Goal**: Deploy 100 services across 5 tenants, respect system concurrency
**Preconditions**: 100 services across 5 tenant accounts, maxConcurrentDeploys=5 global

### Steps
1. Batch 1: POST 10 services simultaneously from each tenant (50 total)
   - First 5 complete deployment successfully (status="running")
   - Next 15 queued (status="deploying")
   - Remaining 30 rejected: 503 "deploy queue full"
2. Wait 1 minute for batch 1 to complete
3. Batch 2: POST remaining services
   - All accepted, processed through queue
4. By minute 5, all 100 services deployed (via 5 concurrent slots × 20 batch cycles)
5. Verify total deployment time: ~300s (not 5+ minutes if deployed sequentially)

### Variations / Edge Cases
- **Uneven Completion**: If some deployments take 30s and others 120s, queue throughput varies
- **Long-Running Deploy**: One image pull takes 5 min; blocks a slot for that duration, other queued services wait
- **Tenant-Level Limits**: If also enforcing per-tenant concurrency (not yet implemented), additional throttling occurs
- **Cancellation**: No way to cancel queued deployment; must wait or delete service (which fails if deploying)

---

## STORY-014: Service URL Routing (Production vs Localhost)
**Type**: short
**Topic**: Service Creation & Deployment
**Persona**: Development vs Production Administrator
**Goal**: Understand how service URLs are assigned based on configuration
**Preconditions**: baseDomain configured for production, can be empty for dev

### Steps
1. **With baseDomain set** (e.g., "example.com"):
   - `POST /v1/services` with name="chat-api" → service.URL="https://chat-api.tenant.example.com"
   - DNS label derived from name (lowercased, max 63 chars, hyphens normalized)
   - Traefik file-provider route written with Host rule: "chat-api.tenant.example.com"
2. **Without baseDomain** (empty, localhost mode):
   - `POST /v1/services` with name="chat-api" → service.URL="http://localhost:9000" (random port)
   - Each service gets unique ephemeral port
   - Useful for local development
3. Service responds at its assigned URL

### Variations / Edge Cases
- **Name to DNS Label Conversion**: "Chat-API v2.0" → "chat-api-v20" (truncates to 63 chars)
- **Name Conflicts**: Two services with same DNS label → Later service fails validation if name produces same label
- **URL Changes**: Restart doesn't change URL; service retains same route
- **External Access**: Production URLs require valid DNS and Let's Encrypt cert (Traefik handles); localhost URLs are direct port access

---

## STORY-015: Deployment via Snapshot (Advanced Cloning)
**Type**: long
**Topic**: Service Creation & Deployment
**Persona**: MLOps Engineer
**Goal**: Version-control service configurations via snapshots, deploy multiple variants
**Preconditions**: Snapshot of base llm service with tuned env vars and optimized image

### Steps
1. Create production snapshot: `POST /v1/services/svc-prod/snapshots` with name="prod-llm-2026-03-21" → Captures image, env vars, port
2. Create 3 variants from snapshot:
   - `POST /v1/services?from_snapshot=snap-123` with name="staging-llm" → Deployes in ~3s
   - `POST /v1/services?from_snapshot=snap-123` with name="test-llm" → Same
   - `POST /v1/services?from_snapshot=snap-123` with name="dev-llm" → Same
3. All 3 inherit:
   - Image: "127.0.0.1:5000/llm-base:v3.0" (same, already cached locally)
   - Env vars: MODEL_PATH, AUTH_TOKEN, LOGLEVEL (restored from snapshot)
   - Port: 8000
4. Each runs independently with own container, own logs, own lifecycle
5. If snapshot updated later, new variants use updated image/env

### Variations / Edge Cases
- **Snapshot Garbage Collected**: If old snapshot deleted, recreating from it fails
- **Image Pruned**: Snapshot references image no longer in local registry → Deployment fails
- **Env Corruption**: Snapshot data corrupted during encryption → Restore fails with 500
- **Env Size Explosion**: Snapshot with 1000+ env vars (impossible in current system, max 100) → Validation rejects at snapshot creation time
