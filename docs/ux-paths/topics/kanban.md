# Kanban Board Integration — UX Path Stories

## Story 1: First-Time Kanban Provisioning

**Role**: Tenant Admin (Agent Platform)

**Goal**: Set up a kanban board to track AI agent tasks and workflows.

**Persona**: Sarah, an AI platform operator running multi-agent systems. She needs a central task coordination hub.

### Journey

1. Sarah authenticates to the API with her tenant API key: `Authorization: Bearer {keyID}.{secret}`
2. She sends: `POST /v1/kanban` (empty body, no parameters needed)
3. The API immediately returns `201 Created`:
   ```json
   {
     "id": "a7f3c9b2e1d5...",
     "tenant_id": "sarah-tenant-id",
     "status": "ready",
     "host": "127.0.0.1",
     "port": 7234,
     "url": "sarah-tenant-id.kanban.agentic.hosting",
     "credentials": {
       "username": "admin",
       "password": "5a8d1c3e...",
       "jwt": "eyJhbGc...",
       "setup_success": true
     },
     "created_at": 1711000000,
     "updated_at": 1711000005
   }
   ```
4. The response includes:
   - **URL**: Pre-configured public domain (DNS routing via Traefik)
   - **Credentials**: Admin username + randomly generated 64-char hex password
   - **JWT**: Pre-authenticated token for API access
   - **Default Project**: Auto-created as "{tenant_id} Kanban"
   - **Default Buckets**: Backlog, In Progress, In Review, Done
5. Sarah immediately has a fully operational kanban board with TLS termination.

**Behind the Scenes**:
- Disk check (must have >10% free space in `/var/lib/ah` and `/var/lib/docker`)
- Port allocation from 7100-7500 (UNIQUE constraint prevents race conditions)
- Docker volume creation: `ah-kanban-{id}`
- Vikunja v0.24.6 container startup
- 60-second health check (polling `/api/v1/info`)
- Admin user registration + login
- Project + 4 buckets auto-created
- Admin password encrypted (AES-256-GCM) before storage

**Success Criteria**:
- Kanban accessible at the returned URL
- Admin can log in with returned credentials
- Default project and buckets exist

---

## Story 2: Accessing the Kanban via Web UI

**Role**: Tenant Admin

**Goal**: Access the Vikunja web interface to begin organizing tasks.

**Persona**: Sarah (continued from Story 1)

### Journey

1. Sarah uses the credentials from the provision response:
   - Username: `admin`
   - Password: `5a8d1c3e...` (from response)
2. She navigates to: `https://sarah-tenant-id.kanban.agentic.hosting`
3. The Traefik reverse proxy terminates TLS and routes to the container on port 7234
4. She logs in with the provided credentials
5. She sees the pre-created "Sarah Tenant Kanban" project with four buckets
6. She creates her first task: "Deploy GPT-4 agent to production"
7. The task starts in the "Backlog" bucket

**Technical Notes**:
- Container is only accessible on loopback (127.0.0.1 port binding) internally
- No external registration possible (Vikunja registration is enabled but only accessible via localhost)
- TLS certificate issued by Let's Encrypt via Traefik
- Frontend URL pre-configured as `https://{tenant_id}.kanban.agentic.hosting`

**Success Criteria**:
- Web UI loads and is usable
- Login succeeds
- Tasks can be created and moved between buckets

---

## Story 3: Retrieving the Admin Token Later

**Role**: Tenant Admin

**Goal**: Recover the admin password after provisioning (in case it wasn't saved).

**Persona**: Marcus, a platform operator who needs to add another user to the kanban board but lost the initial credential response.

### Journey

1. Marcus authenticates with his API key
2. He sends: `GET /v1/kanban/admin-token`
3. The API returns:
   ```json
   {
     "admin_token": "5a8d1c3e..."
   }
   ```
4. Marcus now has the plaintext admin password and can:
   - Log into Vikunja directly
   - Invite other team members via Vikunja's UI

**Technical Details**:
- The token is encrypted at rest with the master key
- Only decrypted on explicit API request
- Non-reversible: token is the plaintext password (no separate API token issued)
- If kanban doesn't exist, returns `404 Not Found`
- If kanban manager is unavailable, returns `503 Service Unavailable`

**Security Implications**:
- Requires valid tenant API key
- Plaintext password exposure over HTTPS only
- Best practice: use the JWT from the initial provision response instead of re-requesting the password

**Success Criteria**:
- Admin password is correctly decrypted and returned
- Marcus can log in with this password

---

## Story 4: Checking Kanban Status After Creation

**Role**: Tenant Admin

**Goal**: Verify the kanban board is healthy and retrieve its public URL.

**Persona**: DevOps engineer monitoring tenant resource status

### Journey

1. Engineer sends: `GET /v1/kanban`
2. Response (kanban exists and is ready):
   ```json
   {
     "id": "a7f3c9b2e1d5...",
     "tenant_id": "engineer-tenant",
     "status": "ready",
     "host": "127.0.0.1",
     "port": 7234,
     "url": "engineer-tenant.kanban.agentic.hosting",
     "created_at": 1711000000,
     "updated_at": 1711000005
   }
   ```
3. Engineer notes:
   - Status is "ready" (not "provisioning", "failed", or "stopped")
   - Public URL is fully qualified and resolvable
   - Timestamps indicate when kanban was created and last updated

**Possible Status Values**:
- `provisioning` — Container starting, health checks pending (typically <5s)
- `ready` — Fully operational
- `failed` — Provisioning failed (e.g., health check timeout, setup error)
- `stopped` — Tenant suspended (kanban killed but record retained)

**Response Without Kanban**:
- `404 Not Found`: "kanban board not found"

**Success Criteria**:
- Status accurately reflects kanban state
- URL is correct and matches tenant ID

---

## Story 5: Attempting to Create a Second Kanban (Should Fail)

**Role**: Tenant Admin

**Goal**: Understand one-per-tenant constraint

**Persona**: Alice, testing the API

### Journey

1. Alice already has a kanban board
2. She sends: `POST /v1/kanban` again
3. The API returns `409 Conflict`:
   ```json
   {
     "error": "tenant already has a kanban board"
   }
   ```
4. Alice learns: only one kanban board per tenant is allowed.
5. If she wants a new kanban, she must first delete the existing one.

**Why This Constraint?**:
- Simplifies per-tenant resource isolation
- Each kanban is tied to exactly one tenant ID
- Prevents accidental resource sprawl
- Enforced at DB level: `UNIQUE (tenant_id)` constraint on non-failed kanbans

**Workaround**:
- Delete existing kanban: `DELETE /v1/kanban`
- Wait for cleanup (Docker removal)
- Create new kanban: `POST /v1/kanban`

**Success Criteria**:
- Second creation is rejected with 409
- Error message clearly states the constraint

---

## Story 6: Deleting a Kanban Board

**Role**: Tenant Admin

**Goal**: Remove kanban and free up resources.

**Persona**: Finance director shutting down a test tenant

### Journey

1. Director authenticates and sends: `DELETE /v1/kanban`
2. The API:
   - Stops the Vikunja container
   - Removes the Docker container
   - Deletes the Docker volume (with all persisted tasks and data)
   - Removes the DB record
3. Response: `204 No Content`
4. All kanban data is permanently deleted

**Cleanup Sequence**:
1. Get kanban record (retrieve container_id, volume_name)
2. Stop Docker container (SIGTERM graceful shutdown)
3. Remove Docker container (force if needed)
4. Remove Docker volume (irreversible data deletion)
5. Delete DB record only after Docker cleanup succeeds

**Error Scenarios**:
- Kanban doesn't exist: `404 Not Found`
- Docker container already gone: Error logged but proceeds
- Volume removal fails: DB record is NOT deleted (safe: prevents orphan data re-creation)

**Success Criteria**:
- Kanban and all data are deleted
- Docker resources cleaned up
- DB record removed

---

## Story 7: Kanban Provisioning Timeout (Health Check Failure)

**Role**: Platform Support

**Goal**: Understand failure modes

**Persona**: Support engineer investigating why a kanban provision failed

### Journey

1. Engineer receives a complaint: `POST /v1/kanban` timed out
2. Engineer checks the kanban status: `GET /v1/kanban`
3. Response:
   ```json
   {
     "status": "failed",
     "id": "a7f3c9b2e1d5...",
     ...
   }
   ```
4. Kanban is stuck in "failed" state.
5. Root cause (from logs): Vikunja container did not respond to health check within 60 seconds.

**Why Provisioning Can Fail**:
- Vikunja startup timeout (container slow to boot)
- Port binding conflict (another service on the port)
- Docker daemon issues
- Setup registration failed (Vikunja already has an admin)
- Encryption failure (master key corruption)

**Recovery**:
- Engineer deletes the failed kanban: `DELETE /v1/kanban`
- System cleans up partially-created Docker resources
- Engineer retries: `POST /v1/kanban`

**Success Criteria**:
- Failed kanban status is visible
- Operator can recover by deleting and retrying

---

## Story 8: Using Kanban for Agent Task Coordination

**Role**: Agent Orchestrator (automated system)

**Goal**: Dynamically create tasks for AI agents to execute.

**Persona**: An automated workflow controller managing a swarm of agents

### Journey

1. Orchestrator authenticates to the agentic-hosting API
2. Orchestrator provisions a kanban: `POST /v1/kanban`
3. Orchestrator retrieves admin credentials from response
4. Orchestrator makes HTTP calls directly to Vikunja API:
   - `POST /api/v1/projects/{project_id}/tasks` — Create task for agent
   - `PUT /api/v1/tasks/{task_id}` — Mark task "In Progress" when agent starts
   - `PUT /api/v1/tasks/{task_id}` — Move to "Done" when agent completes
5. Agents query Vikunja directly for their next task:
   - `GET /api/v1/projects` — List projects
   - `GET /api/v1/projects/{id}/buckets` — List buckets
   - `GET /api/v1/projects/{id}/buckets/{bucket_id}/tasks` — Fetch tasks in a bucket
6. Agent claims a task, executes work, updates status to "Done"

**Architecture Benefits**:
- Decoupled task queue (not tied to Agentic Hosting API)
- Full audit trail in Vikunja (who moved what task, when)
- Web UI for human operators to monitor agent progress
- API extensibility (custom fields, automations via webhooks)

**Token Flow**:
1. Orchestrator calls `GET /v1/kanban/admin-token` to get JWT or password
2. Orchestrator calls Vikunja directly: `Authorization: Bearer {jwt}`
3. Agents use the same JWT or password to authenticate

**Success Criteria**:
- Orchestrator can create, read, update tasks in Vikunja
- Agents can fetch and update task status
- Web UI shows real-time progress

---

## Story 9: Kanban Stale Reconciliation at Startup

**Role**: Platform Operations

**Goal**: Ensure no orphan kanbans remain if provisioning crashes.

**Persona**: Platform admin after a service restart

### Journey

1. Agentic Hosting service crashed mid-kanban-provision
2. A kanban record was inserted with status "provisioning"
3. But the container failed to start (e.g., out of disk space)
4. Operator restarts the service
5. On startup, the Manager calls `ReconcileStale()`:
   - Query: `SELECT id, container_id, volume_name FROM kanbans WHERE status = 'provisioning'`
   - For each stale record:
     - Stop container (if exists)
     - Remove container
     - Remove volume
     - Update status to "failed"
6. Stale kanban is marked as "failed" and fully cleaned up
7. Operator sees the failed kanban and can decide to retry

**Why This Matters**:
- Prevents orphan Docker containers from consuming resources
- Prevents orphan volumes from holding data
- Prevents operator confusion ("why is this kanban stuck provisioning?")

**Success Criteria**:
- Stale kanbans are detected
- Docker resources are cleaned up
- Operator can retry provisioning

---

## Story 10: Port Collision Handling

**Role**: Platform Engineer

**Goal**: Understand port allocation under high load

**Persona**: Engineer provisioning many kanban boards

### Journey

1. Engineer provisions 10 kanban boards in quick succession:
   ```bash
   for i in {1..10}; do curl -X POST /v1/kanban ...; done
   ```
2. The system allocates ports from 7100-7500 (401 ports max)
3. Each provision:
   - Finds a free port (7100-7500 range)
   - Checks if port is actually available: `net.Listen("tcp", "127.0.0.1:{port}")`
   - Inserts DB record with the port (UNIQUE constraint prevents collisions)
   - If UNIQUE violation on port (race condition):
     - Retry with a different port (up to 5 attempts)
   - If UNIQUE violation on tenant_id:
     - Return 409 Conflict (not a port issue)
4. All 10 kanbans get unique ports without collisions

**Port Allocation Strategy**:
- Query DB for currently allocated ports
- Find first free port from 7100-7500
- Validate port is actually available on the OS
- UNIQUE DB constraint as final safety check
- Retry loop handles race conditions

**Success Criteria**:
- Multiple concurrent provisions don't collide on ports
- Port allocation is deterministic and efficient

---

## Story 11: Kanban During Tenant Suspension

**Role**: Platform Admin

**Goal**: Understand what happens to kanban when tenant is suspended.

**Persona**: Compliance officer suspending a tenant account

### Journey

1. Admin calls: `DELETE /v1/tenant` (suspend entire tenant)
2. The API:
   - Revokes all API keys for the tenant
   - Stops all running services
   - **Calls kanban manager: `StopForTenant(ctx, tenantID)`**
3. Kanban manager:
   - Retrieves the tenant's kanban (if exists)
   - Stops the Docker container
   - Removes the Docker container
   - Updates status to "stopped" (NOT deleted)
4. Kanban data is preserved in the volume (not deleted)
5. If tenant is ever reactivated, kanban can be restored

**Important**: StopForTenant does not delete the kanban record or volume, allowing recovery.

**Success Criteria**:
- Kanban container is stopped
- Data is preserved
- Tenant cannot access kanban while suspended
- Kanban can be recovered if tenant is reactivated

---

## Story 12: Disk Space Check Before Provisioning

**Role**: Platform Operations

**Goal**: Prevent kanban provision on full disk.

**Persona**: Operations engineer monitoring disk space

### Journey

1. Engineer monitors free disk space: < 10% on `/var/lib/ah`
2. Tenant tries to provision a kanban: `POST /v1/kanban`
3. Manager checks disk:
   - `/var/lib/ah`: 8% free (fails, needs >10%)
   - `/var/lib/docker`: 12% free (passes)
4. API returns:
   ```json
   {
     "error": "disk check: available space in /var/lib/ah is 8%, minimum is 10%"
   }
   ```
5. Provision fails before any Docker resource is created
6. Operator can free disk space and retry

**Disk Thresholds**:
- Warning: 80% used
- Critical (blocks provisioning): 90% used

**Success Criteria**:
- Provisioning is blocked when disk is too full
- Clear error message indicates which path is full

---

## Story 13: Kanban URL and DNS Routing

**Role**: Tenant Admin

**Goal**: Access kanban at the public DNS domain.

**Persona**: Sarah, sharing kanban URL with team members

### Journey

1. Sarah provisions a kanban for tenant "research-labs"
2. Response includes: `"url": "research-labs.kanban.agentic.hosting"`
3. Sarah shares this URL with her team: `https://research-labs.kanban.agentic.hosting`
4. Team members navigate to the URL
5. Traefik (reverse proxy) intercepts the request:
   - Parses Host header: `research-labs.kanban.agentic.hosting`
   - Routes to the Vikunja container on 127.0.0.1:7234
   - Terminates TLS (Let's Encrypt cert)
6. Team members see the login page
7. They log in with credentials Sarah shared
8. They can now use the kanban board

**DNS Setup**:
- Wildcard DNS: `*.kanban.agentic.hosting` → Server IP
- Traefik dynamic config: Host-based routing rule per kanban

**Success Criteria**:
- URL is publicly resolvable
- TLS works
- Routing to correct container succeeds

---

## Story 14: Kanban Credentials Encryption

**Role**: Platform Security Officer

**Goal**: Understand how admin credentials are protected.

**Persona**: Security team verifying data protection measures

### Journey

1. Security officer reviews kanban database schema
2. In the `kanbans` table, she finds: `admin_token_encrypted` column
3. Admin password is encrypted with AES-256-GCM:
   - Key source: Master key from `/var/lib/ah/master.key`
   - Each provision generates a random 64-char hex password
   - Password is immediately encrypted after setup
4. Encrypted password is never logged or exposed in API responses (except on initial create)
5. Officer calls `GET /v1/kanban/admin-token`:
   - Token is decrypted on-demand
   - Returned only with valid API key
   - Passed over HTTPS only
6. Officer verifies: Master key has restricted permissions (0600)

**Encryption Details**:
- Algorithm: AES-256-GCM (authenticated encryption)
- Key: Master key (stored encrypted in `/var/lib/ah/master.key`)
- Nonce: Random per encryption
- Ciphertext: Not human-readable

**Success Criteria**:
- Passwords are encrypted at rest
- Encryption is only decrypted on explicit API call
- Master key is protected with restrictive file permissions

---

## Story 15: Concurrent Kanban Creations for Multiple Tenants

**Role**: Batch Provisioning System

**Goal**: Provision kanbans for 50 tenants in parallel without conflicts.

**Persona**: Platform orchestration system initializing a multi-tenant deployment

### Journey

1. Orchestrator starts 50 concurrent `POST /v1/kanban` requests (different tenants)
2. Each request goes through:
   - Disk check (shared, but fast)
   - Port allocation (each finds a unique port from 7100-7500)
   - DB insert with UNIQUE constraints
   - Docker volume and container creation
3. Due to:
   - Mutex protection on `findFreePort()` (serializes port allocation)
   - UNIQUE constraint on (tenant_id, port) in DB
   - Atomic DB insert with conflict retry loop (up to 5 retries)
4. All 50 kanbans provision successfully without conflicts
5. Each gets a unique port and unique tenant_id
6. Port allocation serialization ensures no two kanbans get the same port

**Concurrency Strategy**:
- Mutex on port allocation (safe, not a bottleneck)
- DB UNIQUE constraints for final safety
- Retry loop on port collision (rare)
- Docker operations are asynchronous (no locking)

**Success Criteria**:
- 50 concurrent provisions all succeed
- No port collisions
- No tenant_id duplicates
- All containers start and pass health check

---

## Closing Notes

The kanban board integration provides a powerful, isolated task coordination system for AI agent workflows. Key strengths:
- **One-per-tenant isolation**: No cross-tenant data leakage
- **Automatic setup**: Admin user + default project created automatically
- **Encrypted credentials**: Master key protection
- **Full lifecycle**: Provision → use → delete with proper cleanup
- **Operator-friendly**: Clear error messages, stale reconciliation, health status

Operators should be aware of:
- 401-port limit (7100-7500 range)
- 60-second health check timeout during provisioning
- Disk space requirement (>10% free for provisioning)
- One-per-tenant constraint (delete to recreate)
- Master key criticality (encryption key for credentials)
