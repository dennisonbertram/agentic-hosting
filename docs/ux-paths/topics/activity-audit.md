# Activity & Audit Trail — UX Path Stories

The activity audit trail (`GET /v1/activity`) is a multi-resource event aggregator that provides real-time operational visibility across all tenant resources. Events are sorted by timestamp descending, with configurable limits (1-200 events per request).

### Story 1: First-Time Onboarding - Verify Account Created
**User**: New tenant admin (just registered)  
**Goal**: Confirm successful tenant registration  
**Steps**:
1. Issue `GET /v1/activity?limit=10` after registration completes
2. Observe `tenant.created` event at timestamp T0 with message "Tenant {name} was created"
3. ResourceType = "tenant", Status = "active"

**Why It Matters**: Users want immediate confirmation their account exists before creating services.

---

### Story 2: Recent Activity Summary - 30-Second Status Check
**User**: Operator monitoring multiple services  
**Goal**: Quick overview of what happened in the last 30 seconds  
**Steps**:
1. Query `GET /v1/activity?limit=50` at 14:05:30
2. See events from last 30 seconds (14:05:00 → 14:05:30)
3. Sort includes: service status changes, build completions, database readiness
4. Excludes older noise; default limit (50) is usually sufficient

**Why It Matters**: Operators need real-time situational awareness without log scrolling.

---

### Story 3: Debugging a Deployment - Find Exact Failure Point
**User**: Engineer deploying "chatbot-service"  
**Goal**: Understand why service failed to start  
**Steps**:
1. Issue `GET /v1/activity?limit=100` after deployment fails
2. Find sequence:
   - `service.created` @ 10:15:02 (message: "Service chatbot-service was created")
   - `build.queued` @ 10:15:05 (message: "Build queued for chatbot-service from main")
   - `build.started` @ 10:15:12 (message: "Build started for chatbot-service")
   - `build.failed` @ 10:15:45 (message: "Build failed for chatbot-service (https://github.com/...)")
3. Cross-reference `build_id` to get detailed build logs via `/v1/services/{serviceID}/builds/{buildID}/logs`

**Why It Matters**: Activity timeline helps narrow failure scope before digging into logs.

---

### Story 4: Compliance Audit - List All Key Rotations
**User**: Security team (compliance officer)  
**Goal**: Verify API key rotation policy (keys rotated at least quarterly)  
**Steps**:
1. Poll `GET /v1/activity?limit=200` repeatedly (paginate through all events)
2. Filter locally for `api_key.revoked` events in past 90 days
3. Match each revocation to a corresponding `api_key.created` event (new key)
4. Generate report: "Keys rotated on {dates}" with key IDs and responsible users (from external log source)

**Why It Matters**: Compliance requires audit trail proof of key management practices.

---

### Story 5: Service Crash Investigation - Identify Circuit Breaker Activation
**User**: SRE investigating why "ml-inference-api" stopped handling traffic  
**Goal**: Understand when/why circuit breaker opened  
**Steps**:
1. Issue `GET /v1/activity?limit=200`
2. Find sequence for "ml-inference-api":
   - `service.running` @ 09:15:00 (message: "Service ml-inference-api is running")
   - `service.failed` @ 09:15:43 (message: "Service ml-inference-api failed: exit code 137") — crash #1
   - `service.running` @ 09:16:12 (auto-recovery, exponential backoff)
   - ... 4 more crashes in 10-minute window ...
   - `service.circuit_open` @ 09:25:30 (message: "Circuit breaker opened for ml-inference-api")
3. Activity shows **when** circuit opened; cross-reference service health logs for **why**

**Why It Matters**: SREs need to distinguish between single failures and systemic problems.

---

### Story 6: Database Provisioning Workflow - Track Multi-Step Async Operation
**User**: Application architect deploying "agent-db"  
**Goal**: Verify database is ready before running migrations  
**Steps**:
1. Issue `POST /v1/databases` to create Postgres "agent-db"
2. Poll `GET /v1/activity?limit=50` every 5 seconds
3. Observe:
   - `database.created` @ 11:02:15 (message: "Postgres database agent-db provisioning started", status = "provisioning")
   - `database.ready` @ 11:02:47 (message: "Postgres database agent-db is ready", status = "ready")
4. Once `database.ready` is visible, safe to retrieve connection string and run migrations

**Why It Matters**: Activity provides lightweight polling for async operation completion without extra endpoints.

---

### Story 7: Build Pipeline Monitoring - Track Multi-Stage Build Progress
**User**: DevOps engineer building "model-server" from git  
**Goal**: Monitor multi-stage Nixpacks build without streaming logs  
**Steps**:
1. Trigger build: `POST /v1/services/{serviceID}/builds` with git_url + git_ref
2. Poll `GET /v1/activity?limit=50`
3. Observe progression:
   - `build.queued` @ 12:00:05 (status = "queued")
   - `build.started` @ 12:00:12 (status = "building")
   - `build.succeeded` @ 12:03:42 (status = "succeeded", message: "Build succeeded for model-server")
4. Each event has consistent build ID; use to fetch detailed logs if needed

**Why It Matters**: Coarse-grained polling is easier than websocket/SSE integration for batch systems.

---

### Story 8: Key Revocation Verification - Confirm Old Key Is No Longer Active
**User**: Tenant admin rotating API keys  
**Goal**: Ensure old key is revoked before revoking the next  
**Steps**:
1. Create new API key: `POST /v1/auth/keys`
   - Response includes new `keyID` and secret
   - Activity shows `api_key.created` event immediately
2. Verify in activity: `GET /v1/activity?limit=50`
   - Confirm: `api_key.created` event for new key (with new name)
3. Revoke old key: `DELETE /v1/auth/keys/{oldKeyID}`
4. Check activity again:
   - Confirm: `api_key.revoked` event for old key (with old name)
5. Old key no longer works; new key is active

**Why It Matters**: Activity provides proof of key lifecycle for audit and troubleshooting key auth failures.

---

### Story 9: Cross-Resource Activity Timeline - Trace Service → Build → Database Dependency Chain
**User**: Platform engineer reviewing "data-ingestion-pipeline" deployment  
**Goal**: Understand full dependency chain activation  
**Steps**:
1. Query `GET /v1/activity?limit=100`
2. See interleaved timeline:
   - T0: `service.created` — "data-ingestion" created
   - T1: `database.created` — "postgres-events" provisioning started
   - T2: `build.queued` — "data-ingestion" build from git
   - T3: `database.ready` — "postgres-events" ready
   - T4: `build.succeeded` — image built
   - T5: `service.deploying` — service started after build
   - T6: `service.running` — service healthy
3. Timeline confirms: database ready before service deployed ✓

**Why It Matters**: Activity provides end-to-end visibility of multi-resource workflows.

---

### Story 10: Troubleshooting Rate Limits - Verify Tenant Quota Exhaustion
**User**: API integration team getting 429 errors  
**Goal**: Check if activity requests are rate-limited  
**Steps**:
1. Attempt `GET /v1/activity?limit=50` multiple times rapidly
2. After 100 requests/sec (per-tenant limit) is hit:
   - Response: `429 Too Many Requests`
   - Header: `Retry-After: 5`
3. Activity endpoint itself respects rate limiting
4. Legitimate use: poll every 5-10 seconds instead of every 100ms

**Why It Matters**: Rate limiting is transparent; activity helps developers understand behavior.

---

### Story 11: Tenant Suspension Audit - Confirm Account Deactivation Timestamp
**User**: Finance team reconciling suspended accounts  
**Goal**: Verify exact suspension timestamp for billing cutoff  
**Steps**:
1. Account "acme-labs" is suspended by admin
2. Query activity: `GET /v1/activity?limit=200` using superuser key (if available)
3. Find `tenant.suspended` event with timestamp T
4. Use T for billing reconciliation: "Service stopped at 2026-03-21 14:30:05 UTC"
5. Correlate with service.stopped events for all services in that tenant

**Why It Matters**: Audit trail provides billing-system integration points.

---

### Story 12: Large-Scale Environment Discovery - Understanding Tenant Resource Usage
**User**: New SRE joining team; wants to understand "staging" tenant layout  
**Goal**: See all resources created and their timeline  
**Steps**:
1. Issue `GET /v1/activity?limit=200` for staging tenant
2. Scan activity to see all:
   - `service.created` events (names: web-api, background-worker, etc.)
   - `database.created` events (names: postgres-main, redis-cache, etc.)
   - `build.queued/succeeded` events (when each service was last deployed)
3. Cross-reference with `GET /v1/services` to see current state
4. Activity provides historical narrative; services endpoint provides current snapshot

**Why It Matters**: Activity bridges onboarding and operational understanding.

---

### Story 13: Error Recovery Path - Diagnosing Transient Failures
**User**: Infrastructure operator investigating intermittent service crashes  
**Goal**: Distinguish between transient failures (auto-recovery) vs. stuck circuit breaker  
**Steps**:
1. Query `GET /v1/activity?limit=200` for "api-gateway" service
2. Observe:
   - `service.running` @ 15:00:00
   - `service.failed` @ 15:00:45 (crash #1, auto-recovery initiated)
   - `service.running` @ 15:02:30 (recovered after 105s)
   - `service.failed` @ 15:02:47 (crash #2, backoff: 210s)
   - `service.running` @ 15:06:17 (recovered after 210s)
   - ... sequence shows healthy recovery until...
   - `service.circuit_open` @ 15:25:00 (5th crash in 10 min; human intervention needed)
3. If only seeing `running` → `failed` → `running` cycles: transient issue, self-healing
4. If seeing `circuit_open` with no subsequent recovery: requires manual reset

**Why It Matters**: Activity distinguishes system resilience from failure modes.

---

### Story 14: Security Event Forensics - Audit Key Creation During Incident
**User**: Security incident responder  
**Goal**: Identify who created a suspicious API key  
**Steps**:
1. Suspicious API key found in logs making unusual requests
2. Query activity: `GET /v1/activity?limit=200`
3. Find `api_key.created` event with matching key ID
4. Activity shows: key name, creation timestamp
5. Cross-reference timestamp with external logs/audit to identify requester

**Why It Matters**: Activity provides forensic hook for security investigations.

---

### Story 15: Integration Testing - Verify All Event Types Are Generated
**User**: QA engineer validating activity feature completeness  
**Goal**: Confirm all expected event types appear in activity  
**Steps**:
1. Create test service: `POST /v1/services`
   - Activity shows: `service.created`
2. Trigger build: `POST /v1/services/{serviceID}/builds`
   - Activity shows: `build.queued`, `build.started`, `build.succeeded/failed`
3. Create database: `POST /v1/databases`
   - Activity shows: `database.created`, `database.ready/failed`
4. Create API key: `POST /v1/auth/keys`
   - Activity shows: `api_key.created`
5. Revoke key: `DELETE /v1/auth/keys/{keyID}`
   - Activity shows: `api_key.revoked`
6. Query `GET /v1/activity?limit=200` and confirm all event types present

**Why It Matters**: Activity is a contract between backend and client; validation ensures completeness.

---
