# Tenant Onboarding & Registration — UX Path Stories

## STORY-001: New Developer Registers First Tenant with Bootstrap Token
**Type**: short
**Topic**: Tenant Onboarding & Registration
**Persona**: New developer deploying AI agent workloads
**Goal**: Create a new tenant account and obtain initial API key
**Preconditions**: Bootstrap token is configured on server; developer has 64-char hex token from platform operator

### Steps
1. Developer obtains bootstrap token via secure channel (email, 1Password, etc.)
2. POST `/v1/tenants/register` with `X-Bootstrap-Token` header and body:
   ```json
   {
     "name": "Claude-Ops Team",
     "email": "ops@myagents.ai"
   }
   ```
3. Receives 201 Created with response:
   ```json
   {
     "tenant_id": "a1b2c3d4e5f6789012345678",
     "api_key": "at3f1e2d4c5b6a7890z1y2x3.secretabcdef1234567890"
   }
   ```
4. API key is immediately usable for all subsequent authenticated endpoints
5. Bootstrap token is NOT stored; tenant is activated synchronously

### Variations
- **Invalid bootstrap token**: Returns 401 "missing or invalid bootstrap token"
- **Malformed email**: Returns 400 "invalid email format"
- **Very short name (1 char)**: Returns 400 "name must be at least 2 characters"
- **Long name (>128 chars)**: Returns 400 "name must be at most 128 characters"

### Edge Cases
- **Duplicate email per tenant**: Email is NOT unique per tenant (same email can appear multiple times). No validation enforces uniqueness.
- **Rate limit hit (5/IP/hour)**: Returns 429 with `Retry-After: 43` header (exact seconds until window resets)
- **Global limit exceeded (20/hour total)**: Returns 429 with `Retry-After: 3600` even if IP quota available
- **Maximum tenants reached (1000 active)**: Returns 403 "maximum tenants reached"
- **Bootstrap token not configured**: Returns 503 "registration temporarily unavailable" (server misconfiguration)
- **X-Bootstrap-Token from untrusted IP**: Still rate-limited and checked; HTTPS enforcement is per-session, not per-token

---

## STORY-002: Operator Enables Open Registration for Development
**Type**: short
**Topic**: Tenant Onboarding & Registration
**Persona**: Platform operator setting up local dev environment
**Goal**: Allow unrestricted tenant registration without bootstrap tokens
**Preconditions**: Server started with `--dev --open-registration` flags; `AH_BOOTSTRAP_TOKEN` environment variable is unset

### Steps
1. Server starts with warnings: "WARNING: open registration enabled — anyone can create tenants"
2. Developer POST `/v1/tenants/register` with no `X-Bootstrap-Token` header:
   ```json
   {
     "name": "Dev Tenant A",
     "email": "dev@localhost"
   }
   ```
3. Receives 201 Created immediately (no token validation)
4. Multiple developers can register simultaneously without coordination
5. Rate limiting still applies (5/IP/hour, 20/global/hour)

### Variations
- **--dev without --open-registration**: Returns 503 "registration temporarily unavailable" (bootstrap token required)
- **--open-registration without --dev**: Server fatals at startup with error message

### Edge Cases
- **Same IP exhausts quota**: Still rate-limited by IP (5 registrations)
- **Global quota exhausted**: Still respects global limit (20 total in 1 hour)
- **Multiple browsers/IPs**: Each gets independent quota

---

## STORY-003: Developer Recovers Lost API Key After Breach
**Type**: short
**Topic**: Tenant Onboarding & Registration
**Persona**: Tenant admin who lost all API keys due to accidental deletion
**Goal**: Generate a new API key using email + bootstrap token (recovery path)
**Preconditions**: Tenant exists and is active; all previous API keys have been revoked; developer has bootstrap token

### Steps
1. Developer POST `/v1/auth/recover` with body:
   ```json
   {
     "email": "ops@myagents.ai",
     "bootstrap_token": "a1b2c3d4e5f6789012345678"
   }
   ```
2. Server looks up tenant by email
3. If match found and tenant is active, returns 201 Created:
   ```json
   {
     "id": "kr_1e2d3c4b5a6f7890z",
     "key": "kr_1e2d3c4b5a6f7890z.newsecretsabcdef1234567890",
     "name": "recovery-20260321",
     "created_at": 1742560000
   }
   ```
4. Key is immediately usable for all tenant operations
5. Key is named `recovery-YYYYMMDD` for audit trail

### Variations
- **Unknown email**: Returns 401 "invalid bootstrap token or email" (no email enumeration)
- **Invalid bootstrap token**: Returns 401 "invalid bootstrap token or email" (indistinguishable from email error)
- **Tenant is suspended**: Returns 403 "tenant account is not active"
- **Already at max keys (20)**: Returns 403 "maximum API keys reached; revoke unused keys first"

### Edge Cases
- **Rate limit hit**: Same rate limiter as registration (5/IP/hour, 20/global/hour)
- **Response timing**: No difference in latency between unknown email and wrong token (prevents timing attacks)
- **Multiple recovery requests**: Each creates a new recovery key; no deduplication
- **Concurrent recovery requests**: No race condition; all succeed (20-key limit enforced at insert)

---

## STORY-004: First-Time Setup: From Registration to First Service
**Type**: medium
**Topic**: Tenant Onboarding & Registration
**Persona**: New developer onboarding to the platform
**Goal**: Complete minimal end-to-end flow: register, read tenant info, check quotas
**Preconditions**: Bootstrap token configured; server running

### Steps
1. New developer registers tenant:
   ```bash
   curl -X POST https://api.agentic.io/v1/tenants/register \
     -H "X-Bootstrap-Token: $BOOTSTRAP_TOKEN" \
     -H "Content-Type: application/json" \
     -d '{"name": "Acme Corp AI", "email": "platform@acme.com"}'
   ```
2. Stores returned `api_key` in environment variable: `AH_API_KEY`
3. GET `/v1/tenant` with header `Authorization: Bearer $AH_API_KEY` returns:
   ```json
   {
     "id": "tenant_12345678",
     "name": "Acme Corp AI",
     "email": "platform@acme.com",
     "status": "active",
     "created_at": 1742560000,
     "updated_at": 1742560000
   }
   ```
4. GET `/v1/tenant/usage` returns:
   ```json
   {
     "services": {"used": 0, "max": 10},
     "databases": {"used": 0, "max": 5},
     "api_keys": {"used": 1, "max": 20},
     "memory_mb": 2048,
     "cpu_cores": 2.0,
     "disk_gb": 10,
     "rate_limit": 100
   }
   ```
5. Developer now ready to create services, databases, etc.

### Variations
- **Invalid API key format**: Returns 401 "invalid authorization header"
- **Expired API key**: Returns 401 "key expired"
- **Revoked API key**: Returns 401 "key revoked"
- **Suspended tenant**: Returns 401 "tenant suspended"

### Edge Cases
- **API key leaked in logs**: Key is never logged by server (only request ID, method, path, status)
- **API key shown in browser history**: Full token visible in URL if used in GET request (use POST for sensitive operations)
- **Concurrent quota checks**: Quota query is point-in-time (race conditions possible but not practical)

---

## STORY-005: Platform Operator Diagnoses Registration Failure
**Type**: medium
**Topic**: Tenant Onboarding & Registration
**Persona**: Platform operator troubleshooting registration issues
**Goal**: Identify why tenant registration failed and take corrective action
**Preconditions**: Operator has server logs and SSH access

### Steps
1. Developer reports: "Registration returns 422, says 'registration failed, please check your details'"
2. Operator checks server logs:
   ```
   [2026-03-21 14:22:15] POST /v1/tenants/register - 422 Unprocessable Entity
   ```
3. Operator checks database constraints:
   ```sql
   SELECT name, email FROM tenants WHERE email = 'dev@acme.com';
   ```
4. Finds duplicate email exists (constraint violation), returns 422 without leaking details
5. Advises developer: "Email already registered; use different email or recovery endpoint"
6. Developer registers with new email: `dev2@acme.com`
7. Registration succeeds (201 Created)

### Variations
- **Constraint is UNIQUE(email)**: Not implemented; same email can register multiple tenants
- **Constraint is UNIQUE(tenant_name)**: Not implemented; same name can register multiple tenants
- **Database quota exceeded**: Hypothetical; not possible in SQLite WAL mode
- **Transaction rollback**: All-or-nothing (tenant + quotas + key created together)

### Edge Cases
- **Operator can't differentiate 422 errors**: Server returns same 422 for email duplication, constraint violation, or unknown issue
- **Race condition during check**: Operator sees tenant in DB, but it wasn't there on first try
- **Key generation fails**: Returns 500 (actual internal error, not 422)

---

## STORY-006: Tenant Admin Creates Rotation Keys for Service Deployments
**Type**: medium
**Topic**: Tenant Onboarding & Registration
**Persona**: Tenant admin rotating API keys monthly for security
**Goal**: Create new API key, test it, then revoke the old one
**Preconditions**: Tenant is active and has at least one valid API key

### Steps
1. Admin creates new key via POST `/v1/auth/keys`:
   ```json
   {
     "name": "ci-deploy-march-2026",
     "expires_in": 7776000
   }
   ```
   (expires_in = 90 days in seconds)
2. Receives 201 Created:
   ```json
   {
     "id": "key_newid123",
     "name": "ci-deploy-march-2026",
     "api_key": "key_newid123.newsecrettoken",
     "prefix": "key_new",
     "expires_at": 1750348800
   }
   ```
3. Admin tests new key:
   ```bash
   curl -H "Authorization: Bearer key_newid123.newsecrettoken" \
     https://api.agentic.io/v1/tenant
   ```
4. Returns 200 OK (new key works)
5. Admin revokes old key via DELETE `/v1/auth/keys/key_oldid456`
6. Returns 204 No Content
7. Old key now rejected (401) for all subsequent requests

### Variations
- **No expires_in provided**: Key never expires (useful for long-lived tokens)
- **expires_in = 0**: Returns 400 "expires_in must be positive"
- **expires_in > 10 years**: Returns 400 "expires_in exceeds maximum (10 years)"
- **Already at 20 keys**: Returns 403 "maximum API keys reached, revoke unused keys first"
- **Revoke non-existent key**: Returns 404 "key not found"

### Edge Cases
- **Race condition during revocation**: Old key is revoked but auth cache hasn't invalidated yet (eventual consistency ~100ms)
- **Key expires mid-request**: Request in-flight continues; next request fails 401
- **List keys shows prefix only**: Full secret never returned after creation (security by design)

---

## STORY-007: Multi-Team Collaboration — Creating Service-Specific API Keys
**Type**: medium
**Topic**: Tenant Onboarding & Registration
**Persona**: Tenant admin managing multiple CI/CD pipelines with separate keys
**Goal**: Create namespaced API keys for each pipeline to limit blast radius
**Preconditions**: Tenant has default key; 2+ CI/CD pipelines active

### Steps
1. Admin lists existing keys via GET `/v1/auth/keys`:
   ```json
   [
     {
       "id": "key_default",
       "name": "default",
       "prefix": "key_def",
       "created_at": 1742560000,
       "last_used_at": 1742650000,
       "expires_at": null
     }
   ]
   ```
2. Admin creates key for "training pipeline":
   ```json
   {
     "name": "training-pipeline-us-west",
     "expires_in": 2592000
   }
   ```
3. Creates key for "inference pipeline":
   ```json
   {
     "name": "inference-pipeline-eu",
     "expires_in": 2592000
   }
   ```
4. Provides each pipeline operator with their respective key
5. Both keys authenticate independently (no key hierarchy)
6. All requests logged to activity log with key ID

### Variations
- **Training pipeline key compromised**: Admin revokes only that key; inference pipeline unaffected
- **Key name contains special chars**: Stored as-is; no sanitization applied
- **Listing > 100 keys**: Query limit is 100, extra keys not returned (hard limit)

### Edge Cases
- **Same key used by 2 pipelines**: Both pipelines share the same quota (no per-pipeline metering)
- **Key never used**: last_used_at remains null until first request
- **All keys revoked**: Tenant can use recovery endpoint to get a new key
- **Max keys (20) reached**: Additional key creation fails; must revoke unused ones first

---

## STORY-008: Tenant Name Update After Registration
**Type**: medium
**Topic**: Tenant Onboarding & Registration
**Persona**: Tenant admin rebranding company
**Goal**: Update tenant display name after initial registration
**Preconditions**: Tenant exists and is active; API key is valid

### Steps
1. Admin reads current tenant via GET `/v1/tenant`:
   ```json
   {
     "id": "tenant_xyz",
     "name": "Acme Robotics v0",
     "email": "ops@acme.com",
     "status": "active",
     "created_at": 1742560000,
     "updated_at": 1742560000
   }
   ```
2. Admin updates name via PATCH `/v1/tenant`:
   ```json
   {
     "name": "Acme Robotics Inc"
   }
   ```
3. Receives 200 OK with updated response:
   ```json
   {
     "id": "tenant_xyz",
     "name": "Acme Robotics Inc",
     "email": "ops@acme.com",
     "status": "active",
     "created_at": 1742560000,
     "updated_at": 1742560002
   }
   ```
4. Updated_at timestamp increments; email remains unchanged

### Variations
- **Name too short (<2 chars)**: Returns 400 "name must be 2-128 characters"
- **Name too long (>128 chars)**: Returns 400 "name must be 2-128 characters"
- **Null name field**: Server ignores (no update); returns current state
- **Email field in patch request**: Server ignores (not updateable)

### Edge Cases
- **Concurrent updates**: Last write wins (no optimistic locking)
- **Name contains Unicode**: Stored as-is; no normalization (e.g., "Ångström" stays as-is)
- **Very long Unicode string**: Accepted if total UTF-8 bytes fit in VARCHAR(128)

---

## STORY-009: Tenant Suspension and Account Cleanup
**Type**: medium
**Topic**: Tenant Onboarding & Registration
**Persona**: Tenant admin closing account or operator removing inactive tenant
**Goal**: Suspend tenant and clean up all resources
**Preconditions**: Tenant is active; has running services and databases

### Steps
1. Admin confirms intent and initiates DELETE `/v1/tenant`
2. Server performs atomic transaction:
   - Updates tenant.status = 'suspended'
   - Revokes all API keys (sets revoked_at timestamp)
   - Evicts auth cache (keys rejected immediately)
   - Stops all running service containers
   - Stops all running database containers
   - Stops kanban board container (if provisioned)
3. Returns 204 No Content
4. Subsequent requests with revoked keys return 401 "key revoked"
5. All services/databases remain in DB for audit trail (status = 'stopped', not deleted)

### Variations
- **Tenant not found**: Returns 404 "tenant not found"
- **Concurrent suspension requests**: Both succeed idempotently (no extra work)
- **Suspended tenant tries to act**: Returns 401 "tenant suspended"

### Edge Cases
- **Containers already stopped**: No error; suspension succeeds anyway
- **Docker daemon unreachable**: Stops are fire-and-forget (returned before Docker acks)
- **Kanban never provisioned**: Kanban cleanup is skipped silently
- **High container count (100+ services)**: Suspension takes ~5s; no timeout on cleanup
- **Deletion during active build**: Build continues; tenant is suspended but build not cancelled
- **Multiple concurrent deletes**: Race to update status; last one wins (idempotent)

---

## STORY-010: Operator Audits Registration Rate Limiting
**Type**: medium
**Topic**: Tenant Onboarding & Registration
**Persona**: Platform operator monitoring for abuse or configuration issues
**Goal**: Validate rate limiting is functioning and diagnose limit exhaustion
**Preconditions**: Server has processed multiple registration requests

### Steps
1. Operator scripts registration attempts from single IP:
   ```bash
   for i in {1..6}; do
     curl -X POST https://api.agentic.io/v1/tenants/register \
       -H "X-Bootstrap-Token: $TOKEN" \
       -H "Content-Type: application/json" \
       -d "{\"name\": \"tenant$i\", \"email\": \"tenant$i@test.com\"}"
   done
   ```
2. First 5 requests (i=1..5) return 201 Created
3. 6th request returns 429 Too Many Requests with header `Retry-After: 3600`
4. Operator checks logs; all 6 requests logged with status code
5. Operator waits 1 hour (or resets limiter) and retries; 6th request succeeds

### Variations
- **Requests from different IPs**: Each IP gets independent 5/hour quota
- **Global limit (20/hour) exceeded**: Subsequent requests return 429 even from fresh IPs
- **Hybrid attack**: 2 IPs with 5 requests each (10 total), then 10 more requests across 10 IPs = all succeed (20/hour still available)
- **Rate limiter expires**: After ~5 min, idle entries are pruned; new window starts

### Edge Cases
- **Clock skew on server**: Window expiration might be off; requests could be allowed/denied inconsistently
- **LRU cache full (10k entries)**: Oldest IP entries evicted; IP quotas reset
- **Per-IP and global window desync**: One IP at limit but global still available = request fails on per-IP limit first
- **Distributed requests from proxy**: All requests appear from same proxy IP; quota exhausted quickly

---

## STORY-011: Secure Onboarding — Bootstrap Token Rotation and Secrets Management
**Type**: long
**Topic**: Tenant Onboarding & Registration
**Persona**: Platform operator managing bootstrap token lifecycle
**Goal**: Rotate bootstrap token securely without service interruption
**Preconditions**: Server running with bootstrap token in `AH_BOOTSTRAP_TOKEN` env var; multiple developers registered

### Steps
1. Operator generates new token:
   ```bash
   NEW_TOKEN=$(openssl rand -hex 32)
   echo $NEW_TOKEN
   # Output: a1b2c3d4e5f6789012345678abcdef1234567890abcdef1234567890
   ```
2. Current token in `AH_BOOTSTRAP_TOKEN` remains active
3. Operator schedules token rotation maintenance window
4. Updates `AH_BOOTSTRAP_TOKEN` in `/etc/default/paasd`:
   ```bash
   AH_BOOTSTRAP_TOKEN="a1b2c3d4e5f6789012345678abcdef1234567890abcdef1234567890"
   ```
5. Restarts service:
   ```bash
   systemctl restart paasd.service
   ```
6. Old token no longer works (returns 401)
7. New registrations now require new token
8. Existing tenants' API keys continue working (no dependency on bootstrap token)

### Variations
- **Token length < 32 chars**: Server fatals at startup (enforced in main.go)
- **Token in env var but not updated**: Old token still works after restart
- **Multiple boot tokens**: Not supported; only single token per server
- **Token in URL vs header**: Only header (`X-Bootstrap-Token`) accepted; URL tokens rejected

### Edge Cases
- **Registration in-flight during rotation**: Might succeed with old token or fail with new token depending on timing
- **Rate limiter state after restart**: Cleared; all IPs get fresh quota
- **Operator forgets to update env file**: Service runs but bootstrap token logic might fail (depends on config reload)
- **Token shared insecurely**: Revoke and rotate immediately; old token has no revocation mechanism (can't be selectively disabled)

---

## STORY-012: Enterprise Onboarding — Multi-Region Tenant Setup
**Type**: long
**Topic**: Tenant Onboarding & Registration
**Persona**: Enterprise IT team setting up multi-region infrastructure
**Goal**: Register single tenant across multiple regional servers with consistent identity
**Preconditions**: 3 regional servers (us-west, us-east, eu-west) all configured with same bootstrap token

### Steps
1. IT team generates bootstrap token shared across all regions:
   ```bash
   SHARED_TOKEN="abc123def456ghi789jkl012mno345pqr678stu901vwx234yz"
   ```
2. Configures all 3 servers with `AH_BOOTSTRAP_TOKEN=$SHARED_TOKEN`
3. Registers tenant on us-west server:
   ```bash
   curl -X POST https://us-west.agentic.io/v1/tenants/register \
     -H "X-Bootstrap-Token: $SHARED_TOKEN" \
     -H "Content-Type: application/json" \
     -d '{"name": "Acme Global", "email": "ops@acme.com"}'
   ```
4. Receives `tenant_id: acme_global_001` and `api_key: xxxxxxxx.yyyyyyyy`
5. Registers same tenant on us-east:
   ```bash
   curl -X POST https://us-east.agentic.io/v1/tenants/register \
     -H "X-Bootstrap-Token: $SHARED_TOKEN" \
     -H "Content-Type: application/json" \
     -d '{"name": "Acme Global", "email": "ops@acme.com"}'
   ```
6. Receives NEW `tenant_id: acme_global_002` and separate `api_key`
7. Creates tenant on eu-west as well (generates `acme_global_003` + unique key)
8. IT team discovers: **each server has isolated tenant registry** — no synchronization
9. IT team must manage 3 separate tenant IDs and 3 separate API key sets per logical tenant
10. Creates layer-7 orchestration (load balancer with sticky sessions or multi-region sync layer)

### Variations
- **Single bootstrap token across regions**: All regions can register, but each gets own tenant ID
- **Different tokens per region**: Requires managing 3 bootstrap tokens; registration still isolated
- **DNS round-robin to servers**: Requests randomly routed; tenant might register on us-west and api_key only valid on us-west

### Edge Cases
- **Rate limiting across regions**: Each server has independent rate limiter (5/IP/hour per server)
  - Same IP hitting us-west then us-east: gets 5 + 5 = 10 attempts (not globally limited across regions)
- **Email duplication across regions**: Same email can be registered on us-west and us-east independently
- **Tenant lookup at api.agentic.io (load balancer)**:
  - If request routed to us-west but tenant ID is from us-east: 404 "tenant not found"
- **Master key differs per region**: API keys generated with regional master key; key from us-west won't authenticate on us-east
- **Operator wants single tenant across regions**:
  - Must implement custom tenant sync mechanism
  - OR use shared database (SQLite not suitable for multi-region; would need PostgreSQL)
  - OR implement cross-region tenant mirroring layer
- **High availability within region**: Possible with load balancer + multiple server instances (shared DB)
- **Service deployed to region A but tenant registered on region B**: Service creation fails (wrong region/tenant mismatch)

---

## Related Contexts & Integration Points

### Tenant Quotas
After registration, each tenant receives default quotas (from `state_001_init.sql`):
- `max_services`: 10
- `max_databases`: 5
- `max_memory_mb`: 2048
- `max_cpu_cores`: 2.0
- `max_disk_gb`: 10
- `api_rate_limit`: 100 req/sec per tenant

Quotas are NOT user-updateable via API (operator must update DB directly).

### Bootstrap Token Security
- Compared via HMAC-SHA256 (no length leaks)
- Brute-force resistant: 5 attempts per IP per hour, 20 global per hour
- NOT stored in database (only held in memory during runtime)
- If leaked, only option is rotate + restart service (no selective revocation)

### API Key vs Bootstrap Token
- **Bootstrap Token**: Gates registration + recovery; 1 per server; never expires; environment variable
- **API Key**: Authenticates requests; up to 20 per tenant; can expire; created once, shown once, then hashed in DB

### Error Responses
- **401 Unauthorized**: Invalid/expired/revoked API key, missing bootstrap token, invalid bootstrap token, bad token format
- **403 Forbidden**: Suspended tenant, max keys reached, max tenants reached
- **429 Too Many Requests**: Rate limit exceeded (5/IP/hour or 20/global/hour); includes `Retry-After` header
- **422 Unprocessable Entity**: Database constraint violation (email duplicate, etc.); never leaks specific constraint to client
- **503 Service Unavailable**: Bootstrap token not configured or recovery temporarily unavailable

### Audit & Compliance
- All tenant/key operations are atomic (transactions with rollback)
- Activity log tracks key creation, deletion, tenant updates
- Timestamps (created_at, updated_at, last_used_at) stored as Unix seconds
- Email is never unique across tenants (multiple tenants can share same email)
- Tenant names are not unique (multiple tenants can share same name)
