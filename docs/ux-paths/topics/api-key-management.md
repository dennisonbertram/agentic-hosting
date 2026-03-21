# API Key Management — UX Path Stories

API key management enables tenants to create, list, revoke, and recover API keys. Keys use HMAC-SHA256 hashing, support optional expiration (up to 10 years), and are cached in an LRU auth cache with 30-second TTL. Each tenant can hold up to 20 active keys.

## STORY-001: Create First API Key After Registration

**Type**: short
**Persona**: New tenant admin
**Goal**: Generate an additional API key for CI/CD pipeline use
**Preconditions**: Tenant registered, initial API key from registration available

### Steps
1. Admin calls `POST /v1/auth/keys` with body:
   ```json
   {
     "name": "ci-deploy-key"
   }
   ```
2. Server generates a new key pair: 32-char hex keyID + 64-char hex secret
3. Response (201 Created):
   ```json
   {
     "id": "a1b2c3d4e5f67890a1b2c3d4e5f67890",
     "name": "ci-deploy-key",
     "api_key": "a1b2c3d4e5f67890a1b2c3d4e5f67890.secretabcdef1234567890abcdef1234567890abcdef1234567890abcdef12345678",
     "prefix": "a1b2c3d4",
     "created_at": 1742560000,
     "expires_at": null
   }
   ```
4. Admin stores the full `api_key` value securely (it will never be shown again)
5. Key is immediately usable for all authenticated endpoints

### Variations
- **No name provided**: Server assigns a default name
- **Duplicate name**: Allowed; names are not unique

### Edge Cases
- **Key displayed only once**: After creation, only the 8-char prefix is shown in list operations
- **HMAC storage**: Secret is hashed with HMAC-SHA256 before storage; raw secret cannot be recovered from database

---

## STORY-002: Create API Key with Expiration

**Type**: short
**Persona**: Security-conscious tenant admin
**Goal**: Generate a time-limited key for a contractor
**Preconditions**: Tenant has at least one active key

### Steps
1. Admin calls `POST /v1/auth/keys` with body:
   ```json
   {
     "name": "contractor-30d",
     "expires_in": 2592000
   }
   ```
   (expires_in = 30 days in seconds)
2. Response (201 Created):
   ```json
   {
     "id": "b2c3d4e5f6789012b2c3d4e5f6789012",
     "name": "contractor-30d",
     "api_key": "b2c3d4e5f6789012b2c3d4e5f6789012.secretxyz...",
     "prefix": "b2c3d4e5",
     "created_at": 1742560000,
     "expires_at": 1745152000
   }
   ```
3. Key works normally until `expires_at` timestamp
4. After expiry, any request with this key returns 401 "key expired"

### Variations
- **expires_in = 0**: Returns 400 "expires_in must be positive"
- **expires_in > 10 years (315,360,000s)**: Returns 400 "expires_in exceeds maximum (10 years)"
- **No expires_in**: Key never expires (null expires_at)

### Edge Cases
- **Expiry during request**: In-flight request completes; next request fails with 401
- **Auth cache and expiry**: Cache checks expiry on every lookup, even for cached entries (30s TTL does not bypass expiry)

---

## STORY-003: List Active API Keys

**Type**: short
**Persona**: Tenant admin auditing key usage
**Goal**: View all API keys for the tenant with usage information
**Preconditions**: Tenant has 3 active keys

### Steps
1. Admin calls `GET /v1/auth/keys`
2. Response (200 OK):
   ```json
   [
     {
       "id": "a1b2c3d4e5f67890...",
       "name": "default",
       "prefix": "a1b2c3d4",
       "created_at": 1742560000,
       "last_used_at": 1742650000,
       "expires_at": null
     },
     {
       "id": "b2c3d4e5f6789012...",
       "name": "ci-deploy-key",
       "prefix": "b2c3d4e5",
       "created_at": 1742560100,
       "last_used_at": 1742649500,
       "expires_at": null
     },
     {
       "id": "c3d4e5f678901234...",
       "name": "contractor-30d",
       "prefix": "c3d4e5f6",
       "created_at": 1742560200,
       "last_used_at": null,
       "expires_at": 1745152000
     }
   ]
   ```
3. Admin notes the prefix (first 8 chars) identifies each key
4. Full secrets are never returned in list operations

### Variations
- **No keys exist**: Returns empty array `[]`
- **Revoked keys**: Not included in list (filtered out)

### Edge Cases
- **last_used_at is null**: Key was created but never used for any request
- **last_used_at sampling**: Usage timestamps update at most every 5 minutes to reduce DB write load

---

## STORY-004: Revoke a Compromised API Key

**Type**: short
**Persona**: Tenant admin responding to a security incident
**Goal**: Immediately revoke a leaked API key
**Preconditions**: Tenant has identified key prefix "b2c3d4e5" as compromised

### Steps
1. Admin lists keys to find the full key ID: `GET /v1/auth/keys`
2. Admin calls `DELETE /v1/auth/keys/b2c3d4e5f6789012b2c3d4e5f6789012`
3. Response: 204 No Content
4. Key is immediately revoked:
   - `revoked_at` timestamp set in database
   - Key evicted from auth cache immediately
   - Any request using this key returns 401 "key revoked"
5. All other keys continue working normally

### Variations
- **Key not found**: Returns 404 "key not found"
- **Key already revoked**: Returns 204 (idempotent)
- **Revoking own key**: Allowed; admin must use another key for subsequent requests

### Edge Cases
- **Cache eviction timing**: Key is evicted from LRU cache immediately upon revocation; no ~30s delay
- **Concurrent requests with revoked key**: Request in-flight may complete; next request fails

---

## STORY-005: Hit the 20-Key Limit

**Type**: short
**Persona**: Tenant admin managing many integrations
**Goal**: Understand key creation limits
**Preconditions**: Tenant has 20 active API keys (the maximum)

### Steps
1. Admin calls `POST /v1/auth/keys` with body `{"name": "key-21"}`
2. Response (403 Forbidden):
   ```json
   {
     "error": "maximum API keys reached; revoke unused keys first"
   }
   ```
3. Admin lists keys: `GET /v1/auth/keys` -- sees 20 keys
4. Admin revokes an unused key: `DELETE /v1/auth/keys/{oldKeyID}`
5. Admin retries: `POST /v1/auth/keys` with body `{"name": "key-21"}` -- succeeds (201 Created)

### Variations
- **Revoked keys don't count**: Only active (non-revoked) keys count toward the 20-key limit

### Edge Cases
- **Concurrent creation**: Two requests racing to create the 20th key; SQL constraint ensures at most one succeeds

---

## STORY-006: Use an Expired API Key

**Type**: short
**Persona**: Contractor whose temporary key has expired
**Goal**: Understand why requests are failing
**Preconditions**: Key was created with 30-day expiry; 31 days have passed

### Steps
1. Contractor sends any authenticated request with expired key
2. Response (401 Unauthorized):
   ```json
   {
     "error": "key expired"
   }
   ```
3. Contractor contacts tenant admin for a new key
4. Admin creates a new key with fresh expiry

### Variations
- **Key expired 1 second ago**: Still rejected; expiry is checked at exact timestamp
- **Revoked vs expired**: Different error messages ("key revoked" vs "key expired")

### Edge Cases
- **Auth cache behavior**: Even cached keys are checked for expiry on every auth lookup; cache TTL (30s) does not bypass expiry checking

---

## STORY-007: Last-Used Tracking and Sampling

**Type**: medium
**Persona**: Security auditor reviewing key activity
**Goal**: Identify unused API keys that should be revoked
**Preconditions**: Tenant has 5 keys with varying usage patterns

### Steps
1. Auditor lists keys: `GET /v1/auth/keys`
2. Examines `last_used_at` for each key:
   - "default": last_used_at = 1742650000 (used today)
   - "ci-key": last_used_at = 1742560000 (used 1 day ago)
   - "old-key": last_used_at = 1740000000 (not used in 30 days)
   - "test-key": last_used_at = null (never used)
   - "monitoring": last_used_at = 1742649900 (used today)
3. Auditor flags "old-key" and "test-key" for revocation
4. Revokes both: `DELETE /v1/auth/keys/{id}` for each

### Variations
- **Sampling interval**: last_used_at updates at most every 5 minutes per key to reduce DB writes
- **High-frequency key**: Key used 1000 times/minute; last_used_at updates only every 5 minutes

### Edge Cases
- **Exact timing**: If key was used 4 minutes ago, last_used_at may still show the previous 5-minute sample
- **Cache hit without DB update**: Auth cache serves requests without touching DB; last_used_at only updates on cache miss or sampling interval

---

## STORY-008: Auth Cache Behavior Under Load

**Type**: medium
**Persona**: Performance engineer analyzing auth latency
**Goal**: Understand how the auth cache accelerates key validation
**Preconditions**: Auth cache configured with 30s TTL and 5000-entry LRU

### Steps
1. First request with key "abc123.secret456":
   - Cache miss: key not in LRU cache
   - DB lookup: HMAC-SHA256 hash computed, compared against stored hash
   - Key validated; cached in LRU with 30s TTL
   - Response time: ~5ms (DB roundtrip)
2. Second request (within 30s) with same key:
   - Cache hit: key found in LRU
   - Expiry re-checked (even on cache hit)
   - No DB lookup needed
   - Response time: ~0.1ms
3. After 30 seconds, cache entry expires:
   - Next request triggers fresh DB lookup
   - Re-cached for another 30 seconds
4. If key is revoked while cached:
   - Revocation immediately evicts the entry
   - Next request triggers DB lookup, finds revoked key, returns 401

### Variations
- **Cache full (5000 entries)**: LRU evicts least-recently-used entry; next request for evicted key triggers DB lookup
- **Multiple keys from same tenant**: Each key is cached independently

### Edge Cases
- **Race between revocation and cache**: Revocation explicitly evicts from cache; no window of vulnerability

---

## STORY-009: Recovery via Bootstrap Token When All Keys Lost

**Type**: medium
**Persona**: Tenant admin who accidentally revoked all API keys
**Goal**: Regain access to tenant account using recovery endpoint
**Preconditions**: All API keys revoked; admin has bootstrap token and registration email

### Steps
1. Admin calls `POST /v1/auth/recover` with body:
   ```json
   {
     "email": "ops@myagents.ai",
     "bootstrap_token": "a1b2c3d4e5f6789012345678abcdef1234567890abcdef1234567890abcdef12"
   }
   ```
2. Server validates bootstrap token and looks up tenant by email
3. Response (201 Created):
   ```json
   {
     "id": "recovery_key_id_here",
     "key": "recovery_key_id_here.newsecrettoken123456789...",
     "name": "recovery-20260321",
     "created_at": 1742560000
   }
   ```
4. Recovery key is auto-named `recovery-YYYYMMDD` for audit trail
5. Key is immediately usable; admin can now create additional keys

### Variations
- **Wrong email**: Returns 401 "invalid bootstrap token or email" (no email enumeration)
- **Wrong bootstrap token**: Returns 401 "invalid bootstrap token or email" (indistinguishable)
- **Tenant suspended**: Returns 403 "tenant account is not active"

### Edge Cases
- **Rate limiting**: Same limits as registration (5/IP/hour, 20/global/hour)
- **Response timing**: Identical latency for wrong email vs wrong token (prevents timing attacks)
- **Multiple recoveries**: Each creates a new key; no deduplication

---

## STORY-010: Recovery Blocked by Full Keyring

**Type**: medium
**Persona**: Tenant admin trying recovery with 20 active keys
**Goal**: Understand why recovery fails when at key limit
**Preconditions**: Tenant has 20 active keys; admin lost track of which keys are active

### Steps
1. Admin calls `POST /v1/auth/recover` with valid credentials
2. Response (403 Forbidden):
   ```json
   {
     "error": "maximum API keys reached; revoke unused keys first"
   }
   ```
3. Admin realizes they still have active keys (just lost the secrets)
4. Admin must contact platform operator to revoke keys via direct DB access
5. After keys are revoked, recovery succeeds

### Variations
- **19 keys active**: Recovery succeeds (creates 20th key)
- **20 keys but some expired**: Expired keys still count toward limit until explicitly revoked

### Edge Cases
- **Circular dependency**: Admin can't revoke keys (no valid key to authenticate) and can't recover (at limit). Requires operator intervention.

---

## STORY-011: Email Enumeration Prevention

**Type**: medium
**Persona**: Attacker probing the recovery endpoint
**Goal**: Attempt to discover valid tenant email addresses
**Preconditions**: Attacker has the bootstrap token but not tenant emails

### Steps
1. Attacker calls `POST /v1/auth/recover` with guessed email:
   ```json
   {
     "email": "admin@target.com",
     "bootstrap_token": "valid_token_here..."
   }
   ```
2. Response (401 Unauthorized):
   ```json
   {
     "error": "invalid bootstrap token or email"
   }
   ```
3. Attacker tries another email -- same 401 response
4. Attacker tries with wrong bootstrap token -- same 401 response
5. No way to distinguish between "email not found" and "wrong token"
6. Response timing is identical for all failure cases

### Variations
- **Rate limiting kicks in**: After 5 attempts from same IP, returns 429 regardless
- **Valid email + wrong token**: Same 401 as wrong email + valid token

### Edge Cases
- **Timing side-channel**: Server uses constant-time comparison for both email lookup and token validation
- **Error message wording**: Identical string for all failure modes by design

---

## STORY-012: Multi-Key Rotation Workflow

**Type**: long
**Persona**: DevOps engineer performing quarterly key rotation
**Goal**: Rotate production API key with zero downtime
**Preconditions**: Production service uses key "prod-key-q4" for all API calls

### Steps
1. Engineer creates new key:
   ```
   POST /v1/auth/keys
   {"name": "prod-key-q1-2026", "expires_in": 7776000}
   ```
   Response: 201, new key returned
2. Engineer updates CI/CD pipeline with new key
3. Engineer verifies new key works:
   ```
   GET /v1/tenant
   Authorization: Bearer <new-key>
   ```
   Response: 200 OK
4. Engineer monitors both keys in parallel for 24 hours
5. Engineer checks `last_used_at` on old key:
   ```
   GET /v1/auth/keys
   ```
   Confirms old key is no longer in use (last_used_at stopped updating)
6. Engineer revokes old key:
   ```
   DELETE /v1/auth/keys/<old-key-id>
   ```
   Response: 204 No Content
7. Engineer verifies old key is dead:
   ```
   GET /v1/tenant
   Authorization: Bearer <old-key>
   ```
   Response: 401 "key revoked"
8. Rotation complete; new key is sole production credential

### Variations
- **Forgot to update all services**: Old key revoked but some services still use it; they start failing with 401
- **Both keys used simultaneously**: Both authenticate independently; no interference

### Edge Cases
- **Key rotation during deployment**: Deploy uses old key for initial request, new key for subsequent; both work
- **Rollback after rotation**: If new key has issues, cannot un-revoke old key; must create yet another key
