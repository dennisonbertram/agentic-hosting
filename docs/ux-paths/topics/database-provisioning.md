# Database Provisioning — UX Path Stories

Database provisioning enables tenants to provision managed Postgres and Redis databases with automatic volume management, health checking, and secure deletion. All databases run in gVisor-isolated containers on the tenant's private network, with encrypted credentials and connection strings.

## Story 1: Provision a Postgres Database

**Persona:** Alice, MLOps engineer deploying a chatbot agent  
**Goal:** Quickly provision a production Postgres database for application state storage  
**Preconditions:** Tenant registered, API key available, disk space > 20% on `/var/lib/ah`

### Request
```
POST /v1/databases HTTP/1.1
Authorization: Bearer app-key-prefix.secret
Content-Type: application/json

{
  "name": "chatbot_state_db",
  "type": "postgres"
}
```

### Response (202 Created)
```json
{
  "id": "a1b2c3d4e5f6g7h8",
  "tenant_id": "acme-tenant-001",
  "name": "chatbot_state_db",
  "type": "postgres",
  "status": "provisioning",
  "host": "127.0.0.1",
  "port": 5456,
  "db_name": "ah",
  "username": "ah",
  "created_at": 1710946800,
  "updated_at": 1710946800
}
```

### Timeline
- **T+0s:** Request arrives; quota check inside IMMEDIATE transaction; port 5456 claimed and DB record inserted with status `provisioning`
- **T+1s:** Docker volume `ah-db-a1b2c3d4e5f6g7h8` created; Postgres container starts on `ah-db-tenantid-dbid`
- **T+3s:** Health check begins: Postgres protocol StartupMessage sent to 127.0.0.1:5456
- **T+4s:** Container responds with AuthenticationOK; health check succeeds
- **T+5s:** Status updated to `ready`; connection string encrypted and returned
- **T+6s:** Agent service can now connect via environment variable

### Key Details
- **Port allocation is atomic:** UNIQUE constraint on `(tenant_id, port)` prevents race conditions
- **Health check uses protocol probe:** Raw TCP StartupMessage detects when Postgres process is initialized (not just container TCP accept)
- **Connection string encrypted:** Format is `postgres://ah:PASSWORD@127.0.0.1:5456/ah?sslmode=disable`; password never returned in list response
- **Stale reconciliation:** On startup, databases stuck in `provisioning` for >3600s are marked `failed`

---

## Story 2: Provision a Redis Database

**Persona:** Bob, data scientist caching embeddings  
**Goal:** Provision a Redis cache for a real-time embedding server  
**Preconditions:** Tenant with max_databases >= 2, API key available

### Request
```
POST /v1/databases HTTP/1.1
Authorization: Bearer api-key.secret
Content-Type: application/json

{
  "name": "embeddings_cache",
  "type": "redis"
}
```

### Response (201 Created, Status: provisioning)
```json
{
  "id": "x9y8z7w6v5u4t3s2",
  "tenant_id": "acme-tenant-001",
  "name": "embeddings_cache",
  "type": "redis",
  "status": "provisioning",
  "host": "127.0.0.1",
  "port": 6467,
  "db_name": null,
  "username": null,
  "created_at": 1710946920,
  "updated_at": 1710946920
}
```

### Timeline
- **T+0s:** Port 6467 allocated in 6379–7000 range; record inserted
- **T+1s:** Docker volume created; Redis container starts with `--requirepass RANDOMPASSWORD`
- **T+2s:** Health check: RESP `PING` command sent to Redis
- **T+5s:** Redis responds `+PONG`; health check succeeds
- **T+6s:** Status updated to `ready`

### Connection String
```
redis://:PASSWORD@127.0.0.1:6467/0
```

### Key Details
- **No db_name/username:** Redis uses optional password-only auth; connection string path is always `/0` (db 0)
- **Port range isolation:** Postgres and Redis use separate ranges to avoid collisions
- **Idempotent health checks:** Probe runs every 1 second for 30 seconds; first success transitions to `ready`

---

## Story 3: Poll Database Status Until Ready

**Persona:** Alice (from Story 1), polling API to determine when to wire env var  
**Goal:** Know when database is ready to accept connections  
**Preconditions:** Database in `provisioning` state, database ID known

### Request (Polling Loop)
```bash
# Poll every 2 seconds until status = ready
for i in {1..15}; do
  curl -H "Authorization: Bearer $KEY" \
    https://api.agentic.host/v1/databases/a1b2c3d4e5f6g7h8
  sleep 2
done
```

### Responses

**Poll 1 (T+2s):**
```json
{
  "id": "a1b2c3d4e5f6g7h8",
  "status": "provisioning",
  "port": 5456
}
```

**Poll 3 (T+6s):**
```json
{
  "id": "a1b2c3d4e5f6g7h8",
  "status": "ready",
  "port": 5456,
  "host": "127.0.0.1",
  "db_name": "ah",
  "username": "ah"
}
```

### Expected Behavior
- Status transitions: `provisioning` → `ready` (on success) or `provisioning` → `failed` (if health check fails after 30s)
- Connection string only populated during creation response; separate GET returns status without secret
- Polling frequency: reasonable interval is 2–5s (no exponential backoff needed; service typically ready in 4–8s)

### Failure Scenarios
- **Health check timeout:** Container is slow to start (e.g. disk I/O during initialization); status → `failed` after 30s
- **Stale state:** If database is deleted during provisioning, record is removed; subsequent GET returns 404
- **Quota exceeded:** If tenant hits max_databases limit, creation rejected with 429 during quota check

---

## Story 4: Retrieve Connection String

**Persona:** Alice, connecting service to provisioned database  
**Goal:** Retrieve encrypted connection string securely  
**Preconditions:** Database in `ready` state, API key available (different from creation request)

### Request
```
GET /v1/databases/a1b2c3d4e5f6g7h8/connection-string HTTP/1.1
Authorization: Bearer different-key.secret
```

### Response (200 OK)
```json
{
  "connection_string": "postgres://ah:7f3a9d2e1b4c6f8e0a5d2c9f1b3e5a7c@127.0.0.1:5456/ah?sslmode=disable"
}
```

### Key Details
- **Encryption:** Connection string stored encrypted with AES-256-GCM using master key; decrypted on-demand
- **Scoped to tenant:** GET request automatically filters by `tenant_id` from auth context; cannot access other tenant's databases
- **Not in list response:** List endpoint returns only metadata (name, type, status, port, host); connection strings never in list to prevent accidental leaks in logs
- **Separate endpoint:** Connection string endpoint ensures explicit access control audit trail

---

## Story 5: Wire Database to Service via Environment Variable

**Persona:** Alice, connecting her service to the chatbot_state_db  
**Goal:** Inject Postgres connection string as environment variable into running service  
**Preconditions:** Service created, database ready, service stopped or running (env update applies on next start)

### Step 1: Retrieve Connection String
```
GET /v1/databases/a1b2c3d4e5f6g7h8/connection-string
```
Response:
```json
{
  "connection_string": "postgres://ah:...@127.0.0.1:5456/ah?sslmode=disable"
}
```

### Step 2: Set Environment Variable on Service
```
POST /v1/services/svc-abc123/env HTTP/1.1
Authorization: Bearer app-key.secret
Content-Type: application/json

{
  "DATABASE_URL": "postgres://ah:7f3a9d2e1b4c6f8e0a5d2c9f1b3e5a7c@127.0.0.1:5456/ah?sslmode=disable"
}
```

Response (200 OK):
```json
{
  "DATABASE_URL": "postgres://ah:****@127.0.0.1:5456/ah?sslmode=disable"
}
```

### Step 3: Restart Service (if running)
```
POST /v1/services/svc-abc123/restart HTTP/1.1
Authorization: Bearer app-key.secret
```

### Key Details
- **Encryption at rest:** Env var stored encrypted in database; only decrypted when service container starts
- **Tenant network isolation:** Service and database both on `ah-tenant-acme` network; no cross-tenant communication possible
- **Max 100 env vars per service:** Quota enforced on SET endpoint
- **Secrets not returned in GET:** Service env GET returns masked values (`****`) for secrets; only service container receives plaintext
- **Restart required:** Env var changes only apply on container start; running containers keep old values until restart

---

## Story 6: List All Databases for Tenant

**Persona:** Bob, auditing all databases provisioned for the team  
**Goal:** List all databases with pagination to understand resource usage  
**Preconditions:** API key available, tenant has >= 1 database

### Request
```
GET /v1/databases?limit=50&offset=0 HTTP/1.1
Authorization: Bearer audit-key.secret
```

### Response (200 OK)
```json
[
  {
    "id": "a1b2c3d4e5f6g7h8",
    "tenant_id": "acme-tenant-001",
    "name": "chatbot_state_db",
    "type": "postgres",
    "status": "ready",
    "host": "127.0.0.1",
    "port": 5456,
    "db_name": "ah",
    "username": "ah",
    "created_at": 1710946800,
    "updated_at": 1710946800
  },
  {
    "id": "x9y8z7w6v5u4t3s2",
    "tenant_id": "acme-tenant-001",
    "name": "embeddings_cache",
    "type": "redis",
    "status": "ready",
    "host": "127.0.0.1",
    "port": 6467,
    "db_name": null,
    "username": null,
    "created_at": 1710946920,
    "updated_at": 1710946920
  }
]
```

### Pagination
- **Default limit:** 100 (max 200)
- **Offset-based:** `offset=0` returns first batch; `offset=50` skips first 50
- **Order:** Descending by `created_at` (newest first)
- **Empty response:** Returns empty array `[]` if tenant has no databases

### Key Details
- **Metrics only:** No secrets returned; connection strings excluded from list
- **Fast response:** Query uses indexed `(tenant_id, created_at DESC)`; typically < 100ms
- **Scoped to tenant:** Request auth context automatically filters results

---

## Story 7: Delete Database and Wipe Volume

**Persona:** Alice, deprovisioning the chatbot_state_db due to project sunset  
**Goal:** Permanently delete database and ensure data cannot be recovered  
**Preconditions:** Database exists, status != `failed`, service no longer connects (optional but recommended)

### Request
```
DELETE /v1/databases/a1b2c3d4e5f6g7h8 HTTP/1.1
Authorization: Bearer admin-key.secret
```

### Response (204 No Content)
*No body; status code only*

### Timeline
- **T+0s:** GET retrieves database record (container_id, volume_name)
- **T+0.5s:** Container stopped (SIGTERM, 10s timeout)
- **T+1s:** Secure volume wipe begins: busybox container created with `find /data -exec dd if=/dev/zero` to overwrite all files
- **T+90s:** Wipe completes (typical 1–2 min for small volumes)
- **T+91s:** Wipe container removed; Docker volume removed
- **T+92s:** Database record deleted from DB
- **T+93s:** Response sent (204 No Content)

### Key Details
- **Secure deletion:** Data overwritten with zeros before volume removal; prevents future tenants from recovering data (issue #9)
- **Best-effort wipe:** If wipe fails (e.g., container already stopped), volume is still removed; wipe failures logged but do not block deletion
- **No active connections:** If service still references this database and tries to connect after deletion, connection fails (normal behavior)
- **Async cleanup on deletion failure:** If removal fails (container not found, volume in use), operation aborts and record is kept; user must retry
- **Idempotent:** Deleting an already-deleted database returns 204 (same as success) after record lookup fails; 404 only if record never existed

---

## Story 8: Hit Quota Limit While Provisioning

**Persona:** Charlie, new tenant with default quota of 5 databases  
**Goal:** Understand why database creation fails when limit is reached  
**Preconditions:** Tenant has 5 active databases, max_databases = 5

### Request
```
POST /v1/databases HTTP/1.1
Authorization: Bearer charlie-key.secret
Content-Type: application/json

{
  "name": "sixth_db",
  "type": "postgres"
}
```

### Response (429 Too Many Requests)
```json
{
  "error": "quota_exceeded",
  "message": "database quota exceeded (max 5)"
}
```

### Key Details
- **Checked inside transaction:** Quota is verified in an IMMEDIATE transaction with a dummy write to lock the row; concurrent requests cannot both see count below limit
- **Counted as:** `SELECT COUNT(*) FROM databases WHERE tenant_id = ? AND status != 'failed'`
  - Failed databases do not count toward quota (can be retried without cleanup)
  - Provisioning databases count (prevents users from spinning up unlimited async requests)
- **Error code 429:** Not 400 (bad request); indicates temporary resource limit
- **No Retry-After header:** Standard 429; client should use exponential backoff
- **Admin override:** Only bootstrap token holder (via tenant update API) can raise max_databases limit

---

## Story 9: Understand Async Provisioning and Concurrent Creates

**Persona:** Dave, power user creating multiple databases quickly  
**Goal:** Understand provisioning timing and port allocation under concurrent load  
**Preconditions:** Tenant has quota for 5+ databases

### Scenario
Dave sends 3 concurrent `POST /v1/databases` requests at T+0:

```bash
curl -X POST ... -d '{"name": "db1", "type": "postgres"}' &
curl -X POST ... -d '{"name": "db2", "type": "redis"}' &
curl -X POST ... -d '{"name": "db3", "type": "postgres"}' &
```

### Response Timing
- **T+0s:** All 3 requests hit quota check
  - Request 1: IMMEDIATE txn acquired; count = 2 < 5; lock released; port 5432 claimed and record inserted
  - Request 2: IMMEDIATE txn acquired; count = 3 < 5; lock released; port 6379 claimed
  - Request 3: IMMEDIATE txn acquired; count = 4 < 5; lock released; port 5433 claimed (5432 was taken)
  - All 3 return 201 with `status: provisioning` immediately
- **T+3–8s:** Each container health-checks in parallel; 3 status transitions to `ready` happen independently
- **T+10s:** All 3 databases fully provisioned

### Key Details
- **Quota check is non-blocking:** Increment happens inside txn, lock released immediately; provisioning happens asynchronously
- **Port allocation is atomic:** UNIQUE constraint on port ensures no two requests claim same port
  - If port collision detected (rare race), retry loop picks new port (up to 5 retries)
  - Port pool: Postgres 5432–6000 (569 available), Redis 6379–7000 (622 available)
  - Per-tenant: Pool is shared; no per-tenant port namespacing
- **Concurrent deletes:** If database deleted during provisioning (record deleted), container/volume cleanup still happens; returned error is "record deleted during provisioning"

---

## Story 10: Handle Stale Provisioning Databases on Restart

**Persona:** Infrastructure team, restarting ah service  
**Goal:** Understand what happens to databases stuck in `provisioning` state  
**Preconditions:** Service abnormally shut down while 1+ databases were provisioning

### Timeline
- **T-30s:** Database created, status = `provisioning`
- **T-20s:** ah service crashes (OOM, panic, etc.); container starts to timeout
- **T-0s:** ah service restarts; on startup, `NewManager()` calls `ReconcileStale()`
- **T+0.1s:** Query finds database with status = `provisioning`
- **T+0.2s:** Attempted cleanup:
  - `StopContainer(containerID)` — succeeds or fails safely
  - `RemoveContainer(containerID)` — removes orphaned container
  - `RemoveVolume(volumeName)` — removes volume (usually already created)
  - `UpdateStatus(id, "failed")` — mark record as failed
- **T+0.5s:** Service fully started; stale databases now in `failed` state
- **T+1s:** User can GET or DELETE the failed database

### Key Details
- **Automatic recovery:** No manual intervention needed
- **Failed databases keep records:** User can inspect why it failed (logs checked by looking at container state)
- **Quota recovery:** Failed databases do NOT count toward quota (can attempt create again without deleting failed one)
- **Retry-able:** User can delete the failed database and retry the create

---

## Story 11: Verify Database Connectivity from Within Service

**Persona:** Alice, testing that service can actually reach the Postgres database  
**Goal:** Confirm network isolation allows intra-tenant database access  
**Preconditions:** Service and database both provisioned and ready

### Scenario
Service runs a simple connectivity check on startup:
```python
import psycopg2
conn_str = os.environ['DATABASE_URL']
try:
    conn = psycopg2.connect(conn_str)
    conn.close()
    print("✓ Database connected")
except Exception as e:
    print(f"✗ Database connection failed: {e}")
    sys.exit(1)
```

### Result
**Success (expected):** Service and database are on the same `ah-tenant-acme` internal network; connection succeeds at application startup.

### Key Details
- **Network topology:** Both service and database connected to `ah-tenant-X` bridge network (Internal=true, ICC=false means service cannot reach other tenants' databases)
- **Host resolution:** Container-to-container uses service name (DNS inside Docker bridge), but ah uses direct IP (127.0.0.1) with host ports for simplicity
- **Port binding:** Database port bound to 127.0.0.1 only (not 0.0.0.0); no external access possible
- **Traefik isolation:** Traefik is also connected to the tenant network but does NOT proxy database traffic (only HTTP services)

---

## Story 12: Understand Encryption at Rest

**Persona:** Security officer, auditing how secrets are stored  
**Goal:** Confirm database passwords and connection strings are encrypted in the state database  
**Preconditions:** Infrastructure team able to inspect SQLite state.db file

### Implementation Details
- **Master key:** Loaded from `/var/lib/ah/master.key` on startup (64 hex chars = 32 bytes)
- **Algorithm:** AES-256-GCM (authenticated encryption; tamper-evident)
- **Stored columns:**
  - `password_encrypted`: Random 32-byte hex password passed to container, encrypted before storage
  - `connection_string_encrypted`: Full URI (`postgres://ah:PASSWORD@127.0.0.1:PORT/ah`) encrypted before storage
- **Decryption on-demand:**
  - Connection string: Decrypted when `GET /databases/{dbID}/connection-string` called
  - Password: Never returned; only used internally during create

### Key Details
- **Master key rotation:** Not supported in current version; key loss = permanent data loss
- **Audit trail:** All decrypt operations are implicit in API calls; no explicit audit log of connection string accesses
- **Plaintext in memory:** Once decrypted, secret lives in memory during request; no additional secrets management layer (e.g., HashiCorp Vault)

---

## Story 13: Monitor Database Resource Usage

**Persona:** Bob, tracking CPU and memory consumption of Redis cache  
**Goal:** Understand why embeddings_cache is consuming more memory than expected  
**Preconditions:** Database provisioned and running, Docker stats available to infrastructure team

### Implementation Details
- **Tenant quotas:** Tenant has max_memory = 4096 MB total (across all services + databases)
- **Database resources:** Not separately quota'd from services (shared pool)
- **Container limits:** Each database container has memory and CPU limits (docker.ResourceLimits)
  - Default: 512 MB memory, 1 CPU (set in RunContainer)
  - No per-tenant resource profiles (flat quota across all resources)

### Key Details
- **Quota mechanism:** Tenant-level quota check happens at creation time
- **No runtime enforcement of per-database limits:** Infrastructure must monitor Docker stats separately
- **Scaling path:** If database exceeds limits, user must delete and recreate with larger instance (not supported in v0.4.0)

---

## Story 14: Recover from Orphaned Docker Resources

**Persona:** Infrastructure team, cleaning up after a crash  
**Goal:** Handle edge case where database record deleted but container/volume still exist  
**Preconditions:** Manual database record deletion or crash during delete operation

### Scenario
1. Database record deleted from state.db (e.g., due to manual SQL operation or bug)
2. Docker container `ah-db-...` and volume `ah-db-...` still running/exist
3. User calls `GET /v1/databases` — database not listed
4. Periodic garbage collector runs (5-min loop)

### GC Timeline
- **T+0s:** GC scans all Docker containers with label `ah.type=database`
- **T+0.5s:** Query state.db for all database IDs
- **T+1s:** Compare Docker containers to DB records
  - Container `ah-db-xyz` not found in records → marked as orphan
  - Orphan removed: StopContainer, RemoveContainer, RemoveVolume
- **T+90s:** Volume wipe completes; volume removed
- **T+91s:** Log entry: "removed orphaned database container ah-db-xyz"

### Key Details
- **Label-based discovery:** GC uses Docker API labels to find all ah-managed databases
- **Best-effort:** GC failures (e.g., container already removed) are logged but do not abort; next GC loop retries
- **5-min periodicity:** Orphans are cleaned up within 5 minutes of creation/crash

---

## Story 15: Database Type Selection and Constraints

**Persona:** Eve, choosing between Postgres and Redis for a use case  
**Goal:** Understand what databases are available and their constraints  
**Preconditions:** Tenant provisioning database

### Postgres (Type: "postgres")
- **Image:** postgres:16-alpine
- **Default DB:** `ah` (cannot be changed)
- **Default user:** `ah` (cannot be changed)
- **Auth:** Username + password
- **Port range:** 5432–6000 (5432 default; incremented on collision)
- **Connection string:** `postgres://ah:PASSWORD@127.0.0.1:PORT/ah?sslmode=disable`
- **Volume mount:** `/var/lib/postgresql/data`
- **Use cases:** Transactional data, relational queries, ACID guarantees, JSON support

### Redis (Type: "redis")
- **Image:** redis:7-alpine
- **Authentication:** Password-only (no username)
- **Port range:** 6379–7000 (6379 default; incremented on collision)
- **Connection string:** `redis://:PASSWORD@127.0.0.1:PORT/0`
- **Volume mount:** `/data`
- **Default DB:** 0 (Redis db 0; hardcoded in connection string)
- **Use cases:** Caching, sessions, rate limiting, pub/sub (no pub/sub exposed yet; agent code must use other means)

### Validation Rules
- **Type required:** Must be `"postgres"` or `"redis"` (case-sensitive); other values rejected with 400
- **Name validation:** 1–128 characters; no special characters enforced in current version
- **Cannot change type:** Database type is immutable; deletion + recreation required to switch engines

---

## Error Scenarios & Recovery

### Scenario A: Disk Space Exhausted
**Trigger:** Tenant creates database while `/var/lib/ah` at 85% capacity  
**Response:** 507 Insufficient Storage  
**Message:** "insufficient disk space (usage > 80%)"  
**Recovery:** Infrastructure team increases disk; retry create

### Scenario B: Port Exhaustion
**Trigger:** Tenant exhausts all ports in range (e.g., 5432–6000 all claimed)  
**Response:** 507 Insufficient Resources  
**Message:** "no free ports available in range 5432–6000"  
**Recovery:** Delete unused databases; retry

### Scenario C: Master Key Missing
**Trigger:** Service starts without `/var/lib/ah/master.key`  
**Response:** Service fails to start (panic during crypto.Decrypt in GetConnectionString)  
**Recovery:** Restore master.key from backup; restart

### Scenario D: Docker Daemon Unreachable
**Trigger:** Service starts but Docker socket unavailable  
**Response:** dbManager = nil; `POST /v1/databases` returns 503 "database management is not available"  
**Recovery:** Ensure Docker daemon running; restart service

### Scenario E: Volume Already Exists
**Trigger:** Name collision (rare; `volumeName = ah-db-{ID}` where ID is random 16 hex chars)  
**Response:** Handled in retries; unlikely to occur in practice  
**Recovery:** Automatic; no user action needed

---

## Summary

Database provisioning is a **fully async, quota-aware, encrypted, and multi-tenant-isolated** feature. Key patterns:

1. **Async health checking:** POST returns immediately with `status: provisioning`; client polls GET until `ready`
2. **Atomic port allocation:** Concurrent creates cannot collide; UNIQUE constraint on port prevents race
3. **Encryption at rest:** Passwords and connection strings encrypted with master key; never logged
4. **Quota enforcement:** Non-blocking IMMEDIATE transaction; count checked once, then provisioning proceeds async
5. **Secure deletion:** Volume wiped before removal; prevents data recovery by future tenants
6. **Stale reconciliation:** On startup, incomplete provisioning cleaned up; no orphaned state
7. **Network isolation:** Each tenant's database on a private bridge network; Traefik does not proxy DB traffic

All endpoint responses include both metadata (name, type, status, port) and secrets (connection string, password) **only in create response**; list and get responses exclude secrets to prevent accidental leaks.
