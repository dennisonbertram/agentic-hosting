# Environment Variable Management — UX Path Stories

Environment variable management enables tenants to securely configure services with secrets and configuration values. Variables are encrypted at rest with AES-256-GCM, masked by default in API responses, and applied to containers on restart. Each service supports up to 100 variables with key validation and forbidden key protections.

## STORY-001: Set Database Connection String

**Type**: short
**Persona**: AI agent developer connecting a service to its database
**Goal**: Configure DATABASE_URL environment variable for a running service
**Preconditions**: Service "chatbot-api" running, Postgres database provisioned and ready

### Steps
1. Developer retrieves database connection string:
   ```
   GET /v1/databases/{dbID}/connection-string
   ```
   Response:
   ```json
   {
     "connection_string": "postgres://ah:7f3a9d2e1b4c@127.0.0.1:5456/ah?sslmode=disable"
   }
   ```
2. Developer sets the environment variable:
   ```
   POST /v1/services/{serviceID}/env
   {
     "DATABASE_URL": "postgres://ah:7f3a9d2e1b4c@127.0.0.1:5456/ah?sslmode=disable"
   }
   ```
3. Response (200 OK):
   ```json
   {
     "DATABASE_URL": "********"
   }
   ```
   Note: Value is masked in the response
4. Developer restarts the service to apply:
   ```
   POST /v1/services/{serviceID}/restart
   ```
5. Service container starts with DATABASE_URL injected as an environment variable
6. Application connects to the database successfully

### Variations
- **Multiple variables at once**: Can set multiple key-value pairs in a single POST
- **Existing variable**: Overwrites the previous value (upsert via INSERT ... ON CONFLICT)

### Edge Cases
- **Restart required**: Setting env vars does NOT automatically restart the container; values only apply on next start
- **Value encrypted at rest**: Stored encrypted with AES-256-GCM in the database; decrypted only when container starts

---

## STORY-002: Configure API Keys as Environment Variables

**Type**: short
**Persona**: ML engineer configuring LLM provider credentials
**Goal**: Inject OPENAI_API_KEY and ANTHROPIC_API_KEY into a service
**Preconditions**: Service exists and is running

### Steps
1. Engineer sets multiple API keys:
   ```
   POST /v1/services/{serviceID}/env
   {
     "OPENAI_API_KEY": "sk-proj-abc123...",
     "ANTHROPIC_API_KEY": "sk-ant-xyz789...",
     "LOGLEVEL": "info"
   }
   ```
2. Response (200 OK):
   ```json
   {
     "OPENAI_API_KEY": "********",
     "ANTHROPIC_API_KEY": "********",
     "LOGLEVEL": "********"
   }
   ```
3. All values are masked in the response (even non-sensitive ones like LOGLEVEL)
4. Engineer restarts service to apply new credentials
5. Service now has access to both LLM providers

### Variations
- **Partial update**: Only the specified keys are updated; existing keys are preserved
- **Same key set twice**: Second POST overwrites the first value

### Edge Cases
- **All values masked equally**: No distinction between sensitive and non-sensitive values in masked response
- **Audit logging**: All env var set operations are logged with tenant ID and service ID (but not values)

---

## STORY-003: Reveal Hidden Environment Values for Debugging

**Type**: short
**Persona**: DevOps engineer debugging connection failures
**Goal**: Verify the actual values stored for environment variables
**Preconditions**: Service has env vars set, engineer suspects a typo in DATABASE_URL

### Steps
1. Engineer retrieves env vars without reveal (default):
   ```
   GET /v1/services/{serviceID}/env
   ```
   Response:
   ```json
   {
     "DATABASE_URL": "********",
     "OPENAI_API_KEY": "********",
     "LOGLEVEL": "********"
   }
   ```
   Keys are visible but values are masked
2. Engineer retrieves with reveal:
   ```
   GET /v1/services/{serviceID}/env?reveal=true
   ```
   Response:
   ```json
   {
     "DATABASE_URL": "postgres://ah:7f3a9d2e1b4c@127.0.0.1:5456/ah?sslmode=disable",
     "OPENAI_API_KEY": "sk-proj-abc123...",
     "LOGLEVEL": "info"
   }
   ```
3. Engineer spots the typo: port should be 5457, not 5456
4. Engineer fixes the value with a new POST

### Variations
- **No reveal parameter**: Defaults to masked values (safe for logging and display)
- **reveal=false**: Same as omitting the parameter; values masked

### Edge Cases
- **Audit trail**: Reveal operations are logged for security compliance
- **Decryption on-demand**: Values decrypted from AES-256-GCM only when reveal=true requested
- **No caching of plaintext**: Decrypted values are not cached; each reveal triggers fresh decryption

---

## STORY-004: Update an Existing Environment Variable

**Type**: short
**Persona**: Operator rotating a database password
**Goal**: Change DATABASE_URL to point to new credentials
**Preconditions**: Service has DATABASE_URL already set

### Steps
1. Operator sets the updated value:
   ```
   POST /v1/services/{serviceID}/env
   {
     "DATABASE_URL": "postgres://ah:newsecret999@127.0.0.1:5456/ah?sslmode=disable"
   }
   ```
2. Response (200 OK):
   ```json
   {
     "DATABASE_URL": "********"
   }
   ```
3. Old value is overwritten (INSERT ... ON CONFLICT UPDATE)
4. Operator verifies with reveal:
   ```
   GET /v1/services/{serviceID}/env?reveal=true
   ```
   Confirms new value is stored
5. Operator restarts service to apply the change

### Variations
- **Multiple updates**: Can update several variables in a single POST request
- **Value unchanged**: If same value is posted, upsert still succeeds (idempotent)

### Edge Cases
- **Transactional update**: SetEnv uses database transactions; partial failures roll back all changes
- **Running container sees old value**: Until restart, the container has the old environment

---

## STORY-005: Delete a Deprecated Environment Variable

**Type**: short
**Persona**: Developer removing an unused feature flag
**Goal**: Remove LEGACY_MODE env var from a service
**Preconditions**: Service has LEGACY_MODE="true" set

### Steps
1. Developer deletes the variable:
   ```
   DELETE /v1/services/{serviceID}/env/LEGACY_MODE
   ```
2. Response: 204 No Content
3. Variable is removed from the database
4. Developer verifies:
   ```
   GET /v1/services/{serviceID}/env
   ```
   LEGACY_MODE no longer appears in the response
5. Developer restarts service; container no longer has LEGACY_MODE in its environment

### Variations
- **Variable not found**: Returns 404 "environment variable not found"
- **Delete and re-create**: Can set the same variable again after deletion

### Edge Cases
- **Audit logging**: Deletion is logged with tenant ID, service ID, and variable key (but not value)
- **Running container**: Container still has the old variable until restart

---

## STORY-006: Hit the 100-Variable Limit

**Type**: medium
**Persona**: Platform team with complex service configuration
**Goal**: Understand variable limits when configuring a service with many settings
**Preconditions**: Service already has 98 environment variables

### Steps
1. Developer sets 3 more variables:
   ```
   POST /v1/services/{serviceID}/env
   {
     "NEW_VAR_99": "value99",
     "NEW_VAR_100": "value100",
     "NEW_VAR_101": "value101"
   }
   ```
2. Response (400 Bad Request):
   ```json
   {
     "error": "maximum environment variables exceeded (max 100)"
   }
   ```
3. None of the variables are set (transaction rolled back)
4. Developer removes unused variables to make room
5. Retries with 2 variables instead of 3; succeeds

### Variations
- **Exactly 100 variables**: Adding 100th succeeds; 101st fails
- **Updating existing variables**: Does NOT count as new; can update all 100 without hitting limit

### Edge Cases
- **Atomic enforcement**: Limit checked within transaction; concurrent requests cannot bypass it
- **Count includes all keys**: Both new and existing keys count toward the total

---

## STORY-007: Restart-Required Semantics

**Type**: medium
**Persona**: New developer who expects hot-reload of configuration
**Goal**: Understand why env var changes don't take effect immediately
**Preconditions**: Service running with LOGLEVEL="info"

### Steps
1. Developer changes log level:
   ```
   POST /v1/services/{serviceID}/env
   {"LOGLEVEL": "debug"}
   ```
2. Response indicates success
3. Developer checks application logs -- still showing "info" level
4. Developer is confused; queries env:
   ```
   GET /v1/services/{serviceID}/env?reveal=true
   ```
   Shows LOGLEVEL="debug" (new value is stored)
5. Developer realizes: **env vars are stored but not applied until restart**
6. Developer restarts:
   ```
   POST /v1/services/{serviceID}/restart
   ```
7. After restart, application logs show "debug" level

### Variations
- **Multiple changes before restart**: All changes accumulate; single restart applies all
- **Restart fails**: If container fails to start, env vars remain stored but unapplied

### Edge Cases
- **Container crash**: If container crashes and restarts (via Docker restart policy), new env vars ARE loaded from database on restart
- **No auto-restart**: The API deliberately does not auto-restart to prevent unexpected downtime

---

## STORY-008: Audit Trail of Environment Changes

**Type**: medium
**Persona**: Security officer auditing configuration changes
**Goal**: Track who changed what environment variables and when
**Preconditions**: Service has been configured multiple times over several weeks

### Steps
1. Officer queries activity log:
   ```
   GET /v1/activity?limit=200
   ```
2. Finds env-related events:
   - `env.set` events with timestamps and service IDs
   - `env.deleted` events with key names (not values)
3. Officer can trace configuration changes chronologically
4. Cross-references with deployment history to understand when changes were applied (via restart)

### Variations
- **No env values logged**: Only key names appear in audit trail; values are never logged for security
- **Bulk set**: Single POST with 5 variables creates one audit event

### Edge Cases
- **Reveal operations**: Logged separately to track who accessed plaintext values
- **Automated changes**: CI/CD pipeline changes are logged with the pipeline's API key ID

---

## STORY-009: Port Configuration via PORT Environment Variable

**Type**: short
**Persona**: Developer configuring application listen port
**Goal**: Set the PORT env var that the application uses to bind
**Preconditions**: Service created with port 8080

### Steps
1. Developer sets PORT explicitly:
   ```
   POST /v1/services/{serviceID}/env
   {"PORT": "3000"}
   ```
2. Response: 200 OK
3. Developer restarts service
4. Container receives PORT=3000 in its environment
5. Application binds to port 3000 inside the container
6. Traefik routes external traffic to the container's internal port

### Variations
- **PORT conflicts with service port**: If service was created with port 8080 but PORT env var is 3000, the application binds to 3000; Traefik adjusts routing accordingly
- **No PORT set**: Application uses its default port; service port field determines Traefik routing

### Edge Cases
- **PORT is not forbidden**: Unlike LD_PRELOAD, PORT is a valid env var name
- **Numeric validation**: PORT value is a string; application is responsible for parsing

---

## STORY-010: Invalid Key Validation — Reserved and Forbidden Keys

**Type**: short
**Persona**: Developer attempting to set a forbidden environment variable
**Goal**: Understand which keys are blocked and why
**Preconditions**: Service exists

### Steps
1. Attempt to set LD_PRELOAD:
   ```
   POST /v1/services/{serviceID}/env
   {"LD_PRELOAD": "/tmp/malicious.so"}
   ```
   Response (400 Bad Request):
   ```json
   {
     "error": "forbidden environment variable key: LD_PRELOAD"
   }
   ```
2. Attempt to set LD_LIBRARY_PATH:
   ```json
   {"LD_LIBRARY_PATH": "/tmp/evil"}
   ```
   Response: 400 "forbidden environment variable key: LD_LIBRARY_PATH"

3. Attempt to set PATH:
   ```json
   {"PATH": "/tmp/evil:/usr/bin"}
   ```
   Response: 400 "forbidden environment variable key: PATH"

4. Attempt invalid key format:
   ```json
   {"123_INVALID": "value"}
   ```
   Response: 400 "invalid environment variable key"

### Variations
- **Valid key pattern**: Must match `^[a-zA-Z_][a-zA-Z0-9_]{0,127}$`
- **Key too long (>128 chars)**: Rejected by regex validation
- **Forbidden keys list**: LD_PRELOAD, LD_LIBRARY_PATH, PATH (security-critical dynamic linker variables)

### Edge Cases
- **Case sensitivity**: "ld_preload" (lowercase) is checked case-insensitively against forbidden list
- **Value size limit**: Each value capped at 32KB; oversized values rejected with 400

---

## STORY-011: Bulk Environment Configuration for New Service

**Type**: medium
**Persona**: DevOps engineer setting up a new microservice
**Goal**: Configure all environment variables in a single request
**Preconditions**: New service created, no env vars set yet

### Steps
1. Engineer prepares full configuration:
   ```
   POST /v1/services/{serviceID}/env
   {
     "DATABASE_URL": "postgres://ah:pass@127.0.0.1:5456/ah",
     "REDIS_URL": "redis://:pass@127.0.0.1:6380/0",
     "OPENAI_API_KEY": "sk-proj-abc123",
     "LOG_LEVEL": "info",
     "MAX_WORKERS": "4",
     "CORS_ORIGINS": "https://app.example.com",
     "JWT_SECRET": "supersecretjwtkey123",
     "SENTRY_DSN": "https://abc@sentry.io/123"
   }
   ```
2. Response: 200 OK with all keys masked
3. Engineer verifies count:
   ```
   GET /v1/services/{serviceID}/env
   ```
   Shows 8 keys, all masked
4. Engineer starts the service:
   ```
   POST /v1/services/{serviceID}/restart
   ```
5. All 8 variables injected into container environment

### Variations
- **Atomic operation**: All variables set in a single transaction; if any fails, none are applied
- **Idempotent**: Running the same POST again updates nothing (values unchanged)

### Edge Cases
- **Mixed valid and invalid**: If one key is forbidden (e.g., LD_PRELOAD), entire request rejected
- **Empty value**: Setting `{"KEY": ""}` is valid; stores an empty string

---

## STORY-012: Environment Variables Across Service Lifecycle

**Type**: long
**Persona**: Platform engineer managing a service through its full lifecycle
**Goal**: Track how env vars behave across creation, updates, restarts, snapshots, and deletion
**Preconditions**: Service "ml-api" running with 5 env vars

### Steps
1. **Initial setup**: Service created with inline env vars:
   ```
   POST /v1/services
   {"name": "ml-api", "image": "ml:v1", "port": 8000, "env": {"MODEL": "gpt-4", "TEMP": "0.7"}}
   ```
   Env vars stored encrypted in database

2. **Add more vars**: Developer adds credentials:
   ```
   POST /v1/services/{serviceID}/env
   {"API_KEY": "sk-abc", "DB_URL": "postgres://...", "CACHE_TTL": "300"}
   ```
   Total: 5 env vars

3. **Snapshot captures env**: Developer snapshots the service:
   ```
   POST /v1/services/{serviceID}/snapshots
   {"name": "ml-api-stable"}
   ```
   All 5 env vars encrypted and stored in snapshot

4. **Update env var**: Developer changes model:
   ```
   POST /v1/services/{serviceID}/env
   {"MODEL": "gpt-4-turbo"}
   ```
   Restart to apply

5. **Restore from snapshot**: Create new service from snapshot:
   ```
   POST /v1/services?from_snapshot=snap-id
   {"name": "ml-api-staging"}
   ```
   New service gets original 5 env vars (MODEL="gpt-4", not "gpt-4-turbo")

6. **Delete service**: `DELETE /v1/services/{serviceID}`
   Env vars are deleted with the service record

### Variations
- **Env vars survive restart**: Container restart reloads current values from database
- **Env vars in snapshot**: Snapshot captures the values at snapshot time, not current values

### Edge Cases
- **Snapshot with updated vars**: Snapshot does not auto-update when vars change; captures point-in-time state
- **Deletion is permanent**: No way to recover env vars after service deletion (unless a snapshot exists)
