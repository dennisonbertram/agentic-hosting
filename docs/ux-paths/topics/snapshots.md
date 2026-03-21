# Snapshots & Service Templating — UX Path Stories

### **1. Create a Snapshot from a Running Service**

**Actor:** Platform Engineer managing staging deployment  
**Goal:** Preserve the exact configuration of a working service for later reuse  
**Preconditions:** Service "web-api-staging" running with image `127.0.0.1:5000/web-api:v2.1.0`, port 8080, 3 env vars set  

**Steps:**
1. Engineer identifies service in production that works well
2. Calls `POST /v1/services/{serviceID}/snapshots` with name "web-api-stable-2.1.0" and description "Verified for Q2 release"
3. System validates service has a built image
4. Docker image is tagged as `127.0.0.1:5000/snapshots/{tenantID}:{snapshotID}`
5. All environment variables captured and encrypted at rest (AES-256-GCM)
6. Resource config (CPU, memory) read from tenant quotas
7. HTTP 201 created, returns snapshot object with ID, image_ref, port, created_at

**Outcomes:**
- Snapshot stored in SQLite snapshots table
- Docker image tagged in local registry (immutable reference)
- Env vars safely encrypted, no plaintext in database
- Snapshot ready for service creation within 500ms

**Error Cases:**
- Service not found → 404 NotFound
- Service has no image → 409 Conflict ("service has no built image to snapshot")
- Docker tag operation fails → 500 InternalServerError (orphaned tag cleaned up)

---

### **2. List Snapshots with Pagination**

**Actor:** DevOps team reviewing available templates  
**Goal:** Discover what snapshots exist to understand available deployment options  
**Preconditions:** Tenant has 47 snapshots across multiple services  

**Steps:**
1. Engineer calls `GET /v1/snapshots?limit=20&offset=0`
2. System returns 20 snapshots ordered by created_at DESC
3. Each snapshot includes ID, service_id, name, description, image_ref, port, resource_config
4. Engineer reviews list and notes "web-api-stable-2.1.0" snapshot
5. To fetch next page, calls `GET /v1/snapshots?limit=20&offset=20`

**Outcomes:**
- Snapshots paginated efficiently (default 100, max 200 per request)
- Created_at timestamp allows sorting by age
- Description field helps identify purpose at a glance
- Full snapshot library visible without loading all at once

**Edge Cases:**
- limit > 200 → capped at 100
- negative offset → reset to 0
- empty result → returns empty array (not 404)

---

### **3. Get Snapshot Details**

**Actor:** QA engineer preparing regression test environment  
**Goal:** Inspect exact configuration of a snapshot before using it  
**Preconditions:** Snapshot ID "abc123" exists  

**Steps:**
1. Engineer calls `GET /v1/snapshots/{snapshotID}`
2. System verifies tenant ownership (tenant_id match)
3. Returns full snapshot object: id, service_id, name, description, image_ref, port, resource_config, created_at
4. Engineer notes the port (3000) and resource_config shows max_memory_mb=512, max_cpu_cores=1
5. Engineer can now proceed with creating service from this snapshot

**Outcomes:**
- Full snapshot metadata available in single request
- Resource config visible for capacity planning
- Image reference shows exact Docker tag (immutable)
- NotFound error if snapshot doesn't exist

**Security:**
- Tenant isolation verified; cannot access snapshots from other tenants
- Env vars NOT returned in Get operation (only decrypted on service creation)

---

### **4. Restore a Service from a Snapshot**

**Actor:** Staging environment manager forking for demo  
**Goal:** Quickly create a new service with identical configuration  
**Preconditions:** Snapshot "web-api-stable-2.1.0" exists with encrypted env vars  

**Steps:**
1. Engineer calls `POST /v1/services?from_snapshot=abc123` with JSON body: `{"name": "web-api-demo"}`
2. System fetches snapshot metadata (image_ref, port, resource_config)
3. Decrypts environment variables stored in snapshot using master key
4. Validates snapshot image, env vars, and port (defense-in-depth)
5. Creates new service record with snapshot data:
   - Image: snapshot.ImageRef
   - Port: snapshot.Port
   - Env: decrypted from snapshot
6. Triggers async deployment (bounded 10-min context)
7. Returns service with status="deploying"

**Outcomes:**
- New service "web-api-demo" created in ~5 seconds (image already in local registry)
- All env vars (API keys, DB credentials) restored automatically
- Resource config applied to new service
- No manual env var re-entry needed

**Error Cases:**
- Snapshot not found → 404
- Snapshot contains invalid image → 400 BadRequest
- Snapshot env vars fail validation → 400 BadRequest
- Snapshot port out of range → 400 BadRequest

---

### **5. Delete a Snapshot**

**Actor:** Infrastructure manager cleaning up old templates  
**Goal:** Remove obsolete snapshots and free Docker image storage  
**Preconditions:** Snapshot "web-api-old-v1.0" no longer needed  

**Steps:**
1. Engineer calls `DELETE /v1/snapshots/{snapshotID}`
2. System verifies tenant ownership
3. Attempts to remove Docker image from local registry (127.0.0.1:5000)
4. Deletes snapshot record from SQLite
5. Returns HTTP 204 No Content

**Outcomes:**
- Snapshot removed from list operations
- Docker image untagged (freed for garbage collection)
- Cannot create services from deleted snapshot
- Completes in <500ms even if Docker untagging is slow

**Resilience:**
- If Docker image removal fails, log warning but still delete DB record
- Database consistency maintained even if image cleanup is incomplete
- Orphaned images cleaned up by garbage collector (5-min loop)

---

### **6. Snapshot with Encrypted Environment Variables**

**Actor:** Security-conscious DevOps engineer protecting secrets  
**Goal:** Ensure API keys and database passwords are never stored in plaintext  
**Preconditions:** Service has 5 env vars including "DB_PASSWORD" and "STRIPE_API_KEY"  

**Steps:**
1. Engineer snapshots a service: `POST /v1/services/{serviceID}/snapshots`
2. System queries `service_env` table for all env vars
3. Each value is encrypted using AES-256-GCM with tenant's master key
4. Encrypted values stored as hex string in snapshot.env_encrypted (JSON blob)
5. Create operation completes; no plaintext captured

**Later: Service Restoration**
1. Engineer creates new service from snapshot: `POST /v1/services?from_snapshot=abc123`
2. System calls `RestoreEnvVars(tenantID, snapshotID)`
3. Env vars decrypted using master key (tenant-scoped for isolation)
4. Decrypted values injected into new service container
5. No plaintext written to logs or response bodies

**Outcomes:**
- Secrets protected in transit and at rest
- Encryption/decryption happens only within trusted boundary (Go process memory)
- Master key stored securely on server (not in code or env vars)
- Snapshots safe to store long-term without exposing credentials

**Audit Trail:**
- Activity log shows snapshot creation, but no env var values
- Service creation from snapshot logged without plaintext values

---

### **7. Multi-Environment Forking: Staging to Production**

**Actor:** Release manager preparing production deployment  
**Goal:** Verify staging configuration is production-ready, then fork to production  
**Preconditions:** "api-staging" service running successfully for 2 weeks  

**Steps:**
1. Manager snapshots staging: `POST /v1/services/staging-id/snapshots` → name="api-prod-candidate-2026-03-21"
2. Manager reviews snapshot in staging for 3 days (performance tests, regression suite)
3. Manager creates production service: `POST /v1/services?from_snapshot=snap-id` with name="api-prod"
4. Production service inherits:
   - Same Docker image as staging
   - Same port (3000)
   - Same env vars (but tied to production DB credentials)
5. Production service deploys; monitoring confirms health
6. Staging can continue running independently

**Outcomes:**
- Production config guaranteed identical to tested staging config
- No manual transcription of env vars → fewer errors
- Each environment has isolated containers, can scale independently
- Snapshots act as "release candidate" templates

**Advanced Option:**
- Manager could override specific env vars before deployment by:
  1. Creating service from snapshot
  2. Calling `POST /v1/services/{serviceID}/env` to update production-specific settings
  3. Then redeploying

---

### **8. Rapid Development Environment Setup (Onboarding)**

**Actor:** New team member joining platform  
**Goal:** Get a local development environment up in <5 minutes  
**Preconditions:** Team maintains snapshot "dev-env-complete" with full stack  

**Steps:**
1. New member receives snapshot ID via team docs
2. Member calls `POST /v1/services?from_snapshot=dev-env-complete` with name="dev-[username]"
3. Service deploys; all env vars for local dev loaded from snapshot
4. Service accessible at https://dev-[username].internal.example.com
5. Member can connect debugging tools, run tests, iterate

**Time Breakdown:**
- Service creation: ~100ms
- Docker deployment (image already cached): ~5s
- Health check: ~2s
- Ready to code: <10s after API call

**Outcomes:**
- No documentation drift (snapshot is single source of truth for dev environment)
- All team members have identical base configuration
- Eliminates "works on my machine" problems
- New member productive immediately

---

### **9. Disaster Recovery: Restore Service from Snapshot**

**Actor:** On-call engineer recovering from data corruption  
**Goal:** Restore service to last known good state  
**Preconditions:** Service "content-api" crashed; last snapshot created 6 hours ago  

**Steps:**
1. Engineer verifies snapshot "content-api-hourly-backup-2026-03-21-14:00" exists
2. Engineer notes old service had container issues; deletes it: `DELETE /v1/services/{serviceID}`
3. Engineer creates new service from snapshot: `POST /v1/services?from_snapshot=snap-id` with name="content-api"
4. New service boots with:
   - Exact same Docker image (proven stable)
   - Same port configuration
   - Same env vars (including DB connection string)
5. Connections redirected to new service via Traefik
6. Service online within 30 seconds

**Outcomes:**
- RTO (Recovery Time Objective): <1 minute
- RPO (Recovery Point Objective): 6 hours (snapshot cadence)
- No manual configuration recovery needed
- Service identity restored exactly as it was

**Monitoring:**
- Engineer confirms new service health via `GET /v1/services/{serviceID}`
- Views deployment history: `GET /v1/services/{serviceID}/deployments`
- Streams logs: `GET /v1/services/{serviceID}/logs?follow=true`

---

### **10. Service Templating for Team Onboarding**

**Actor:** Platform architect designing service catalog  
**Goal:** Provide standardized templates for common workload patterns  
**Preconditions:** Team building 4 microservices with similar structure  

**Steps:**
1. Architect builds reference service "web-template-python":
   - Python 3.11 image, FastAPI framework
   - Port 8000
   - Standard env vars: LOG_LEVEL, DB_POOL_SIZE, CORS_ORIGINS
2. Architect snapshots: `POST /v1/services/{serviceID}/snapshots` → name="python-fastapi-template-v1.0"
3. Team members spin up new services from template:
   - Service 1 "user-service": `POST /v1/services?from_snapshot=template-id` with name="user-service"
   - Service 2 "payment-service": from same template, just different name
   - Service 3 "notification-service": from same template
4. Each service inherits logging config, monitoring setup, port convention
5. Team adds service-specific env vars via `POST /v1/services/{serviceID}/env`

**Outcomes:**
- Consistency enforced across microservices
- Zero-downtime template updates (new snapshots replace old ones)
- Self-service deployment: no manual setup needed
- Architecture documentation is living code (snapshots show actual production config)

**Scaling:**
- Organization maintains library of 20+ snapshots (python-web, go-worker, nodejs-api, etc.)
- All accessible via `GET /v1/snapshots` with filtering by name

---

### **11. CI/CD Integration: Automated Snapshot on Successful Build**

**Actor:** CI/CD pipeline automating deployment workflow  
**Goal:** Create reproducible snapshots after each successful release build  
**Preconditions:** Build system deploys image, confirms health checks pass  

**Steps:**
1. CI pipeline builds image: `go build ./...`, tags as `127.0.0.1:5000/api:v2.3.0`
2. Pipeline creates service: `POST /v1/services` with image=`127.0.0.1:5000/api:v2.3.0`, name="api-v2.3.0"
3. Service deploys; regression tests run for 5 minutes
4. If tests pass, pipeline creates snapshot:
   ```
   POST /v1/services/{serviceID}/snapshots
   {
     "name": "api-v2.3.0-release",
     "description": "Build #1247, commit abc123def456, all tests passing"
   }
   ```
5. Snapshot tagged as stable release candidate
6. Pipeline tags Git commit with snapshot ID for audit trail

**Outcomes:**
- Every release version has a snapshot (immutable reference)
- Deployment to production is one API call: `POST /v1/services?from_snapshot=snap-id`
- Rollback is equally simple: create service from previous snapshot
- Full audit: which snapshot was deployed when, by whom

**Disaster Scenario:**
- If production service fails, engineer can instantly rollback:
  1. Delete failed service
  2. Create new service from previous snapshot
  3. Service online in 30 seconds with zero configuration effort

---

### **12. A/B Testing: Create Service Variant from Snapshot**

**Actor:** Product manager testing new feature in production  
**Goal:** Run two versions of same service simultaneously for A/B testing  
**Preconditions:** "recommendation-engine" serving 100% traffic; new algorithm ready  

**Steps:**
1. Manager creates snapshot of current stable service: "recommendation-engine-baseline-2026-03-21"
2. Manager creates variant service: `POST /v1/services?from_snapshot=baseline-id` with name="recommendation-engine-variant"
3. For variant service, manager updates feature flag: `POST /v1/services/{variantID}/env` with "ENABLE_NEW_ALGORITHM=true"
4. Manager restarts variant: `POST /v1/services/{variantID}/restart`
5. Traefik routes 10% traffic to variant, 90% to baseline (configured externally)
6. Collect metrics for 1 week:
   - Baseline: p99 latency 45ms, error rate 0.01%
   - Variant: p99 latency 38ms, error rate 0.005%
7. Variant performs better; manager scales to 100% traffic
8. Baseline service can be deleted or kept as fallback

**Outcomes:**
- Zero-downtime A/B testing enabled by snapshots
- Production traffic split without code changes
- Rollback instant (switch traffic back to baseline)
- Both services fully isolated, independently scalable

---

### **13. Compliance & Audit: Snapshot Retention Policy**

**Actor:** Compliance officer enforcing regulatory requirements  
**Goal:** Maintain immutable record of service configurations for audit purposes  
**Preconditions:** Industry requires 7-year audit trail of production changes  

**Steps:**
1. Organization maintains snapshot retention policy:
   - Daily snapshots of all production services
   - Snapshots named: `{service}-{date}-{time}` (e.g., "api-2026-03-21-15:00")
   - Stored indefinitely in local registry + replicated to S3 for cold storage
2. When regulatory audit occurs:
   - Auditor requests snapshot from specific date/time
   - System provides exact Docker image and env var snapshot
   - Can verify: What code was running? What configuration? Who approved?
3. If misconfiguration found, auditor can:
   - View all snapshots from that service for historical comparison
   - Identify when/how configuration drifted
4. Snapshots are immutable (stored in read-only Docker registry)

**Outcomes:**
- Regulatory compliance: full audit trail preserved
- Configuration drift detection: compare snapshots over time
- Incident investigation: "What was this service running 6 months ago?"
- Reproducibility: can recreate exact production state from any snapshot

**Long-term Storage:**
- Local registry (fast access): last 30 days of snapshots
- Cold storage (S3): 7-year archive of all snapshots
- Snapshot metadata logged: who created, when, from which service

---

### **14. Cost Optimization: Snapshot Sharing Across Services**

**Actor:** Infrastructure engineer optimizing cloud spend  
**Goal:** Reduce storage footprint by sharing common base images  
**Preconditions:** 20 microservices all use similar base (Python 3.11 + dependencies)  

**Steps:**
1. Engineer builds single base image: `python-base:3.11`
2. Creates 4 variation snapshots:
   - "python-base-web": adds Flask, gunicorn
   - "python-base-worker": adds Celery, redis-py
   - "python-base-ml": adds NumPy, PyTorch
   - "python-base-api": adds FastAPI, SQLAlchemy
3. All 20 microservices build FROM one of these snapshots
4. Docker layer deduplication saves 80% storage vs. building independently
5. Monthly snapshot cleanup removes old variations, retains only latest per type

**Outcomes:**
- Disk usage: 500 GB → 100 GB
- Build time: faster (layers cached across services)
- Security: single source for base image updates
- Consistency: all services use proven, tested base

**Update Scenario:**
- Security patch released for Python
- Engineer: rebuild base image, create new snapshot "python-base-patched"
- Team: create new services from patched snapshot over next sprint

---

### **15. Snapshot Resource Tracking: Memory & CPU Restoration**

**Actor:** Platform administrator managing resource constraints  
**Goal:** Ensure new services created from snapshot respect original resource allocations  
**Preconditions:** Snapshot captured when service was running with strict CPU/memory limits  

**Steps:**
1. Original service "batch-processor" created with: max_memory_mb=1024, max_cpu_cores=2
2. Service runs successfully for weeks; engineer snapshots: `POST /v1/services/{serviceID}/snapshots`
3. Snapshot captures resource config in `resource_config` field (JSON):
   ```json
   {
     "max_memory_mb": 1024,
     "max_cpu_cores": 2
   }
   ```
4. 3 months later, engineer restores from snapshot: `POST /v1/services?from_snapshot=snap-id` with name="batch-processor-replay"
5. New service inherits resource limits from snapshot
6. gVisor enforces those limits on container (memory cgroup, CPU quota)

**Outcomes:**
- Resource requirements captured as-is (no guessing about "how big was the original?")
- New services respect historical limits for fair quota management
- Prevents accidentally starving the container (memory) or allowing runaway CPU
- Audit trail: can see what resources were allocated at each point in time

**Advanced:**
- Snapshots can be used for capacity planning: "This service successfully ran on 2 CPU cores; we can provision similar services knowing the constraint"
- May be used for cost attribution: "Service X was using 512 MB memory on average when snapshots"

---
