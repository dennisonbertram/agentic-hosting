# UX Path Catalog: Agentic Hosting

Generated: 2026-03-21
Total Stories: 148
Coverage: 42 / 44 features (95.5%)

## Summary

| Type | Count |
|------|-------|
| Short | 53 |
| Medium | 62 |
| Long | 10 |
| Untyped | 23 |

Note: 23 stories from service-lifecycle, database-provisioning, snapshots, kanban, activity-audit, and deployment-history topics did not include explicit type annotations. They have been classified based on step count and complexity (short: 1-5 steps, medium: 6-10 steps, long: 11+ steps or multi-phase).

## Coverage Matrix

| Feature Area | Stories | Gaps |
|---|---|---|
| System Health (`/health`) | STORY-119, STORY-125, STORY-126 | None |
| System Health Detailed (`/health/detailed`) | STORY-120, STORY-121, STORY-122, STORY-123, STORY-124, STORY-127 | None |
| Tenant Registration (`/tenants/register`) | STORY-001, STORY-002, STORY-004, STORY-005, STORY-010, STORY-011, STORY-012 | None |
| Tenant Read (`/tenant`) | STORY-004, STORY-008 | None |
| Tenant Update (`/tenant` PATCH) | STORY-008 | None |
| Tenant Delete/Suspend (`/tenant` DELETE) | STORY-009 | None |
| Tenant Usage (`/tenant/usage`) | STORY-004, STORY-044 | None |
| API Key Create (`/auth/keys` POST) | STORY-006, STORY-007, STORY-013, STORY-014, STORY-017 | None |
| API Key List (`/auth/keys` GET) | STORY-015, STORY-019 | None |
| API Key Revoke (`/auth/keys/{id}` DELETE) | STORY-016, STORY-024 | None |
| API Key Recovery (`/auth/recover`) | STORY-003, STORY-021, STORY-022, STORY-023 | None |
| Service Create (`/services` POST) | STORY-025, STORY-026, STORY-027, STORY-044, STORY-057 | None |
| Service List (`/services` GET) | STORY-041, STORY-148 | None |
| Service Get (`/services/{id}` GET) | STORY-028, STORY-041 | None |
| Service Delete (`/services/{id}` DELETE) | STORY-009 | None |
| Service Start/Stop/Restart | STORY-038, STORY-042 | None |
| Service Redeploy | STORY-050, STORY-137, STORY-138 | None |
| Service Reset (Circuit Breaker) | STORY-043, STORY-139 | None |
| Service Logs | STORY-039, STORY-048 | None |
| Service Deployments | STORY-025, STORY-136, STORY-141 | None |
| Env Vars Get (`/env`) | STORY-068 | None |
| Env Vars Set (`/env` POST) | STORY-063, STORY-064, STORY-066, STORY-073, STORY-074, STORY-076 | None |
| Env Vars Delete (`/env/{key}` DELETE) | STORY-067 | None |
| Builds Trigger (`/builds` POST) | STORY-053, STORY-054 | None |
| Builds List (`/builds` GET) | STORY-055, STORY-058 | None |
| Build Status (`/builds/{id}` GET) | STORY-055 | None |
| Build Logs (`/builds/{id}/logs`) | STORY-056, STORY-058 | None |
| Build Cancel (`/builds/{id}` DELETE) | STORY-059 | None |
| Database Create (`/databases` POST) | STORY-078, STORY-079 | None |
| Database List (`/databases` GET) | STORY-083 | None |
| Database Get (`/databases/{id}` GET) | STORY-080 | None |
| Database Delete (`/databases/{id}` DELETE) | STORY-084 | None |
| Database Connection String | STORY-081 | None |
| Snapshot Create (`/snapshots` POST) | STORY-093 | None |
| Snapshot List (`/snapshots` GET) | STORY-094 | None |
| Snapshot Get (`/snapshots/{id}` GET) | STORY-095 | None |
| Snapshot Delete (`/snapshots/{id}` DELETE) | STORY-097 | None |
| Kanban Create (`/kanban` POST) | STORY-108 | None |
| Kanban Get (`/kanban` GET) | STORY-111 | None |
| Kanban Admin Token (`/kanban/admin-token`) | STORY-110 | None |
| Kanban Delete (`/kanban` DELETE) | STORY-113 | None |
| Activity (`/activity` GET) | STORY-128, STORY-129, STORY-130, STORY-131 | None |
| Idempotency (SHA-256 body hash) | STORY-149 | None |
| Rate Limiting (429 + Retry-After) | STORY-010, STORY-137 | **No standalone story exercising per-tenant 100 req/s limit on non-registration endpoints** |

## Duplicate & Overlap Analysis

The following stories cover substantially similar ground across topics. They are NOT removed from the catalog (each topic file retains its stories) but are flagged here for test planning efficiency.

| Duplicate Group | Stories | Notes |
|---|---|---|
| API key creation workflow | STORY-006 (tenant-onboarding), STORY-013 (api-key-mgmt) | Both cover creating a new key with optional expiry. STORY-006 includes rotation; STORY-013 is the canonical key-creation story. |
| API key recovery | STORY-003 (tenant-onboarding), STORY-021 (api-key-mgmt) | Nearly identical recovery flow. STORY-003 is from the onboarding perspective; STORY-021 from key-management. |
| API key rotation | STORY-006 (tenant-onboarding), STORY-024 (api-key-mgmt) | Both cover create-new-then-revoke-old. STORY-024 is more detailed with monitoring period. |
| Multi-key / per-pipeline keys | STORY-007 (tenant-onboarding), STORY-019 (api-key-mgmt) | Both cover creating service-specific keys. STORY-007 focuses on blast radius; STORY-019 on last_used_at auditing. |
| Email enumeration prevention | STORY-003 variations (tenant-onboarding), STORY-023 (api-key-mgmt) | STORY-023 is a dedicated adversarial story; STORY-003 mentions it in variations. |
| Env var restart semantics | STORY-050 (service-lifecycle), STORY-069 (env-mgmt) | Both cover "set env, restart to apply." STORY-050 is lifecycle-focused; STORY-069 is env-focused. |
| Env var lifecycle across snapshots | STORY-074 (env-mgmt), STORY-098 (snapshots) | Both cover env vars captured in snapshots and restored. |
| Deploy from snapshot | STORY-027 (service-creation), STORY-096 (snapshots) | Both cover `POST /v1/services?from_snapshot=`. STORY-027 is brief; STORY-096 details decryption. |
| Deploy queue backpressure | STORY-029 (service-creation), STORY-144 (deployment-history) | Both cover queue full / 503 scenarios. |
| Circuit breaker + redeploy | STORY-031 (service-creation), STORY-040 (service-lifecycle), STORY-139 (deployment-history) | Three stories on circuit breaker open blocking operations. |
| Redeploy vs restart semantics | STORY-050 (service-lifecycle), STORY-137 (deployment-history) | Both explain redeploy = restart alias. |
| Build log streaming | STORY-056 (build-pipeline), STORY-134 (activity-audit) | STORY-056 is the canonical build-log stream; STORY-134 uses activity as a lightweight alternative. |
| Stale provisioning reconciliation | STORY-087 (database), STORY-116 (kanban) | Both cover ReconcileStale() on service restart for their respective resource types. |

## Story Dependency Graph

```
STORY-001 (Register tenant)
 +-- STORY-004 (First-time setup: register -> read tenant -> check quotas)
 |    +-- STORY-008 (Update tenant name)
 |    +-- STORY-009 (Suspend tenant)
 +-- STORY-013 (Create API key)
 |    +-- STORY-014 (Create key with expiry)
 |    +-- STORY-015 (List keys)
 |    +-- STORY-016 (Revoke key)
 |    +-- STORY-017 (Hit 20-key limit)
 |    +-- STORY-019 (Last-used tracking)
 |    +-- STORY-024 (Multi-key rotation)
 +-- STORY-025 (Deploy service from image)
 |    +-- STORY-026 (Deploy with inline env vars)
 |    +-- STORY-028 (Monitor deployment polling)
 |    +-- STORY-038 (Start/stop/restart)
 |    +-- STORY-039 (Log streaming)
 |    +-- STORY-040 (Circuit breaker)
 |    +-- STORY-043 (Manual circuit reset)
 |    +-- STORY-048 (Log tail/pagination)
 |    +-- STORY-050 (Redeploy vs rebuild)
 |    +-- STORY-063 (Set env var)
 |    |    +-- STORY-066 (Update env var)
 |    |    +-- STORY-067 (Delete env var)
 |    |    +-- STORY-068 (Reveal env values)
 |    |    +-- STORY-069 (Restart-required semantics)
 |    +-- STORY-093 (Create snapshot)
 |    |    +-- STORY-094 (List snapshots)
 |    |    +-- STORY-095 (Get snapshot details)
 |    |    +-- STORY-096 (Restore from snapshot)
 |    |    +-- STORY-097 (Delete snapshot)
 |    +-- STORY-053 (Build from GitHub)
 |    |    +-- STORY-055 (Poll build status)
 |    |    +-- STORY-056 (Stream build logs)
 |    |    +-- STORY-057 (Build completion + auto-deploy)
 |    |    +-- STORY-059 (Cancel build)
 |    +-- STORY-136 (View deployment history)
 |    +-- STORY-137 (Redeploy for env changes)
 +-- STORY-078 (Provision Postgres)
 |    +-- STORY-079 (Provision Redis)
 |    +-- STORY-080 (Poll database status)
 |    +-- STORY-081 (Retrieve connection string)
 |    +-- STORY-082 (Wire DB to service via env var)
 |    +-- STORY-083 (List databases)
 |    +-- STORY-084 (Delete database)
 +-- STORY-108 (Provision kanban)
 |    +-- STORY-109 (Access kanban web UI)
 |    +-- STORY-110 (Retrieve admin token)
 |    +-- STORY-111 (Check kanban status)
 |    +-- STORY-113 (Delete kanban)
 |    +-- STORY-115 (Agent task coordination)
 +-- STORY-128 (Activity: verify account)
 |    +-- STORY-130 (Debug deployment via activity)
 +-- STORY-119 (Basic health check)
      +-- STORY-120 (Detailed health check)
```

## All Stories

### 1. Tenant Onboarding & Registration (12 stories)

| ID | Title | Type | Summary |
|---|---|---|---|
| STORY-001 | New Developer Registers First Tenant | short | Register tenant with bootstrap token, receive initial API key |
| STORY-002 | Operator Enables Open Registration | short | Start server with `--dev --open-registration` for local dev |
| STORY-003 | Developer Recovers Lost API Key | short | Use `/auth/recover` with email + bootstrap token to regain access |
| STORY-004 | First-Time Setup: Registration to Quota Check | medium | End-to-end: register, read tenant info, check quotas |
| STORY-005 | Operator Diagnoses Registration Failure | medium | Troubleshoot 422 errors via server logs and DB inspection |
| STORY-006 | Tenant Admin Creates Rotation Keys | medium | Create new key with expiry, test it, revoke old key |
| STORY-007 | Multi-Team Collaboration Keys | medium | Create namespaced API keys per CI/CD pipeline |
| STORY-008 | Tenant Name Update After Registration | medium | PATCH tenant name, verify updated_at changes |
| STORY-009 | Tenant Suspension and Cleanup | medium | DELETE tenant: revoke keys, stop containers, suspend account |
| STORY-010 | Operator Audits Rate Limiting | medium | Script 6 registrations from one IP; 6th gets 429 |
| STORY-011 | Bootstrap Token Rotation | long | Rotate bootstrap token in env file, restart service |
| STORY-012 | Enterprise Multi-Region Tenant Setup | long | Register same tenant across 3 regional servers; discover isolation |

### 2. API Key Management (12 stories)

| ID | Title | Type | Summary |
|---|---|---|---|
| STORY-013 | Create First API Key After Registration | short | Generate additional key for CI/CD use |
| STORY-014 | Create API Key with Expiration | short | Set time-limited key for a contractor (30-day expiry) |
| STORY-015 | List Active API Keys | short | View all keys with prefix, last_used_at, and expiry info |
| STORY-016 | Revoke a Compromised Key | short | Immediately revoke leaked key; cache eviction confirmed |
| STORY-017 | Hit the 20-Key Limit | short | Attempt 21st key creation; get 403; revoke one; retry |
| STORY-018 | Use an Expired API Key | short | Request with expired key returns 401 "key expired" |
| STORY-019 | Last-Used Tracking and Sampling | medium | Audit key usage via last_used_at; flag unused keys for revocation |
| STORY-020 | Auth Cache Behavior Under Load | medium | Understand LRU cache hits/misses, 30s TTL, revocation eviction |
| STORY-021 | Recovery via Bootstrap Token | medium | Regain access after all keys revoked using email + bootstrap token |
| STORY-022 | Recovery Blocked by Full Keyring | medium | Recovery fails at 20 keys; requires operator DB intervention |
| STORY-023 | Email Enumeration Prevention | medium | Attacker probes recovery endpoint; identical 401 for all failures |
| STORY-024 | Multi-Key Rotation Workflow | long | Zero-downtime quarterly key rotation with parallel monitoring |

### 3. Service Creation & Deployment (15 stories)

| ID | Title | Type | Summary |
|---|---|---|---|
| STORY-025 | Deploy Service from Docker Hub Image | short | Create service from image, poll until running, verify URL |
| STORY-026 | Deploy with Inline Environment Variables | short | Create service with env map; verify env persistence |
| STORY-027 | Create from Snapshot to Clone Environment | short | Fork production to staging via `from_snapshot` query param |
| STORY-028 | Monitor Deployment Progress with Polling | short | Poll service status every 2s until deploying -> running |
| STORY-029 | Handle Deploy Queue Backpressure | medium | 25 concurrent deploys; first 5 run, 15 queue, 5 rejected (503) |
| STORY-030 | Deploy Local Registry Image | short | Deploy from `127.0.0.1:5000/` (Nixpacks build output) |
| STORY-031 | Recover from Failed Deployment | medium | Diagnose failed deploy via logs, quotas; restart or recreate |
| STORY-032 | Understand Service Status Lifecycle | short | Map deploying/running/stopped/failed/crashed status transitions |
| STORY-033 | Deploy with Custom Port | short | Two services both using port 8080; Traefik routes by hostname |
| STORY-034 | Validate Image References | short | Test allowed (Docker Hub, localhost:5000) vs blocked image sources |
| STORY-035 | Quota Enforcement on Service Creation | medium | Create 5 services (max quota), attempt 6th (409), delete one, retry |
| STORY-036 | Deployment Failure Diagnostics | medium | Decision tree: disk full, bad image, OOM, network error |
| STORY-037 | Multi-Deployment Concurrency Limits | long | Deploy 100 services across 5 tenants with 5 global concurrent slots |
| STORY-052 | Service URL Routing (Prod vs Localhost) | short | URL assignment with baseDomain vs empty (localhost mode) |
| STORY-051 | Deployment via Snapshot (Advanced Cloning) | long | Snapshot prod, create 3 variants, each runs independently |

### 4. Service Lifecycle Control (13 stories)

| ID | Title | Type | Summary |
|---|---|---|---|
| STORY-038 | Basic Start, Stop, Restart Workflow | medium | Stop for maintenance, restart, verify idempotency |
| STORY-039 | Log Streaming with Follow Mode | medium | Stream logs in real-time; chunked transfer; follow=true |
| STORY-040 | Circuit Breaker Triggering (5 Crashes) | long | 5 crashes in 10 min opens circuit; blocks restarts for 30 min |
| STORY-041 | Circuit Breaker Recovery and Backoff | long | Escalating recovery: 30min -> 1hr -> 4hr backoff |
| STORY-042 | Health Check Failure Recovery | medium | Unhealthy container detected by reconciler; restarted |
| STORY-043 | Manual Circuit Reset | medium | Fix bug, rebuild image, reset circuit, restart service |
| STORY-044 | Redeploy vs Rebuild Semantics | medium | Redeploy = restart with current env; rebuild = new image from git |
| STORY-045 | Deploy Timeout and Reconciliation | medium | Service stuck deploying >10 min; reconciler marks failed |
| STORY-046 | Crash Detection and Reconciliation | medium | Container crashes (OOM); reconciler detects exit code 137 |
| STORY-047 | Service Creation with Async Deployment | medium | POST returns immediately; deployment runs in background goroutine |
| STORY-048 | Log Streaming with Tail and Pagination | medium | Retrieve last N lines; follow with initial tail; 10k line cap |
| STORY-049 | Service Recovery After Network Partition | medium | Docker daemon unreachable; reconciler skips (no false crash) |
| STORY-050 | Environment Variable Restart Semantics | medium | Set env, verify stored but not applied, restart to apply |

### 5. Build Pipeline (12 stories)

| ID | Title | Type | Summary |
|---|---|---|---|
| STORY-053 | Basic Build from GitHub Repository | short | Trigger Nixpacks build from public GitHub repo |
| STORY-054 | Build with Custom Git Ref | short | Build from branch, tag, or full SHA |
| STORY-055 | Polling Build Status | short | Poll build status every 5s: pending -> running -> succeeded |
| STORY-056 | Streaming Build Logs in Real Time | medium | Stream chunked build logs with follow=true |
| STORY-057 | Build Completion and Auto-Deploy | medium | Build succeeds, image pushed, service auto-deployed |
| STORY-058 | Build Failure Diagnosis | medium | Check failed build logs; fix repo; retrigger |
| STORY-059 | Cancel a Running Build | short | DELETE build; process terminated; status = cancelled |
| STORY-060 | Queue Full -- Backpressure Scenario | medium | 22 builds queued; build 22 rejected (503) |
| STORY-061 | Per-Tenant Build Concurrency | short | Second build queues behind first (1 concurrent per tenant) |
| STORY-062 | SSRF Protection -- Blocked Git URLs | short | Private IPs, localhost, unapproved hosts all rejected (400) |
| STORY-075 | Invalid Git URL Validation | short | Empty URL, malformed, FTP, credentials-in-URL all rejected |
| STORY-076 | Build Timeout -- 20-Minute Limit | medium | Build exceeds 20 min; process killed; status = failed |

### 6. Environment Variable Management (12 stories)

| ID | Title | Type | Summary |
|---|---|---|---|
| STORY-063 | Set Database Connection String | short | Set DATABASE_URL, restart to apply, verify connectivity |
| STORY-064 | Configure API Keys as Env Vars | short | Inject OPENAI_API_KEY + ANTHROPIC_API_KEY; all values masked |
| STORY-065 | Reveal Hidden Env Values for Debugging | short | GET env with reveal=true to see decrypted values |
| STORY-066 | Update an Existing Env Var | short | Overwrite DATABASE_URL via upsert; verify with reveal |
| STORY-067 | Delete a Deprecated Env Var | short | DELETE env var by key; verify removed from list |
| STORY-068 | Hit the 100-Variable Limit | medium | Attempt to exceed 100 vars; entire request rolled back |
| STORY-069 | Restart-Required Semantics | medium | Set env, observe old config active, restart, observe new config |
| STORY-070 | Audit Trail of Env Changes | medium | Query activity log for env.set and env.deleted events |
| STORY-071 | Port Configuration via PORT Env Var | short | Set PORT=3000; container binds to 3000; Traefik adjusts |
| STORY-072 | Reserved and Forbidden Keys | short | LD_PRELOAD, PATH blocked; invalid key format rejected |
| STORY-073 | Bulk Env Configuration for New Service | medium | Set 8 vars in single POST; atomic transaction |
| STORY-074 | Env Vars Across Service Lifecycle | long | Create with env, add more, snapshot, update, restore, delete |

### 7. Database Provisioning (15 stories)

| ID | Title | Type | Summary |
|---|---|---|---|
| STORY-078 | Provision a Postgres Database | medium | Create Postgres DB; health check via protocol probe; status = ready |
| STORY-079 | Provision a Redis Database | medium | Create Redis cache; RESP PING health check; connection string |
| STORY-080 | Poll Database Status Until Ready | short | Poll every 2s until provisioning -> ready |
| STORY-081 | Retrieve Connection String | short | GET decrypted connection string (AES-256-GCM) |
| STORY-082 | Wire Database to Service via Env Var | medium | Get connection string, set as env var, restart service |
| STORY-083 | List All Databases for Tenant | short | Paginated list; secrets excluded; newest first |
| STORY-084 | Delete Database and Wipe Volume | medium | Stop container, zero-wipe volume, remove record |
| STORY-085 | Hit Quota Limit While Provisioning | short | 6th database returns 429 (max 5); failed DBs don't count |
| STORY-086 | Async Provisioning and Concurrent Creates | medium | 3 concurrent creates; atomic port allocation; all succeed |
| STORY-087 | Stale Provisioning Databases on Restart | medium | ReconcileStale() cleans up stuck-provisioning DBs on startup |
| STORY-088 | Verify Database Connectivity from Service | short | Python service connects to Postgres on tenant bridge network |
| STORY-089 | Encryption at Rest for DB Credentials | medium | AES-256-GCM encryption of passwords and connection strings |
| STORY-090 | Monitor Database Resource Usage | short | Shared tenant quota pool; no per-DB resource profiles |
| STORY-091 | Recover from Orphaned Docker Resources | medium | GC scans for containers with no DB record; cleans up in 5 min |
| STORY-092 | Database Type Selection and Constraints | short | Postgres vs Redis: images, ports, auth, connection string formats |

### 8. Snapshots & Service Templating (15 stories)

| ID | Title | Type | Summary |
|---|---|---|---|
| STORY-093 | Create Snapshot from Running Service | medium | Tag Docker image, encrypt env vars, store snapshot record |
| STORY-094 | List Snapshots with Pagination | short | Paginated list (default 100, max 200); ordered by created_at |
| STORY-095 | Get Snapshot Details | short | Full metadata including image_ref, port, resource_config |
| STORY-096 | Restore Service from Snapshot | medium | Create service from snapshot; env vars decrypted and restored |
| STORY-097 | Delete a Snapshot | short | Remove Docker image tag, delete DB record |
| STORY-098 | Snapshot with Encrypted Env Vars | medium | AES-256-GCM encryption of env vars in snapshot blob |
| STORY-099 | Multi-Environment Forking: Staging to Prod | medium | Snapshot staging, review, fork to production |
| STORY-100 | Rapid Dev Environment Setup (Onboarding) | medium | New team member creates service from "dev-env-complete" snapshot |
| STORY-101 | Disaster Recovery from Snapshot | medium | Delete crashed service, restore from last-known-good snapshot |
| STORY-102 | Service Templating for Team Onboarding | medium | Architect creates template snapshot; team spins up 3 services |
| STORY-103 | CI/CD Integration: Automated Snapshot on Build | medium | Pipeline builds, tests, snapshots on success for rollback |
| STORY-104 | A/B Testing via Snapshot Variant | medium | Clone baseline service, toggle feature flag, split traffic |
| STORY-105 | Compliance: Snapshot Retention Policy | long | Daily snapshots for 7-year audit trail; S3 cold storage |
| STORY-106 | Cost Optimization: Snapshot Sharing | medium | Shared base images; Docker layer deduplication saves 80% storage |
| STORY-107 | Snapshot Resource Tracking | medium | Resource config (CPU/memory) captured and restored from snapshot |

### 9. Kanban Board Integration (15 stories)

| ID | Title | Type | Summary |
|---|---|---|---|
| STORY-108 | First-Time Kanban Provisioning | medium | Create Vikunja kanban; auto-setup admin, project, 4 buckets |
| STORY-109 | Accessing Kanban via Web UI | short | Log into Vikunja at tenant subdomain; TLS via Traefik |
| STORY-110 | Retrieving Admin Token Later | short | GET /kanban/admin-token returns decrypted password |
| STORY-111 | Checking Kanban Status After Creation | short | GET /kanban returns status, URL, timestamps |
| STORY-112 | Attempting to Create Second Kanban | short | Returns 409 Conflict; one-per-tenant constraint |
| STORY-113 | Deleting a Kanban Board | medium | Stop container, remove volume, delete record |
| STORY-114 | Kanban Provisioning Timeout | medium | Health check fails after 60s; status = failed; delete and retry |
| STORY-115 | Using Kanban for Agent Task Coordination | medium | Orchestrator creates tasks; agents query and update via Vikunja API |
| STORY-116 | Kanban Stale Reconciliation at Startup | medium | ReconcileStale() cleans up stuck-provisioning kanbans |
| STORY-117 | Port Collision Handling | medium | Concurrent provisions; mutex + UNIQUE constraint; retry loop |
| STORY-118 | Kanban During Tenant Suspension | medium | StopForTenant stops container but preserves volume and record |
| STORY-077 | Disk Space Check Before Provisioning | short | Provision blocked if `/var/lib/ah` < 10% free |
| STORY-125 | Kanban URL and DNS Routing | short | Wildcard DNS + Traefik Host routing to container |
| STORY-126 | Kanban Credentials Encryption | medium | AES-256-GCM encryption of admin password; master key protection |
| STORY-127 | Concurrent Kanban Creations (Multi-Tenant) | medium | 50 parallel provisions; mutex + UNIQUE constraint; all succeed |

### 10. Health Monitoring & System Status (10 stories)

| ID | Title | Type | Summary |
|---|---|---|---|
| STORY-119 | Basic Health Check -- Uptime Probe | short | Unauthenticated GET returns `{"status":"ok"}` in <1ms |
| STORY-120 | Detailed Health -- Operational Diagnostics | medium | Docker version, gVisor version, disk usage, DB status |
| STORY-121 | Degraded Health -- Database Unavailable | medium | Database locked; status = degraded; restart to recover |
| STORY-122 | Disk Usage Warning Thresholds | medium | 50% normal -> 82% warning -> 93% blocks provisioning |
| STORY-123 | Docker and gVisor Version Verification | short | Check runtime versions after CVE announcement |
| STORY-124 | Automated Health Check Integration | medium | Prometheus scrape + Grafana dashboard + alert rules |
| STORY-125 | Health Check During System Startup | short | Systemd readiness probe; connection refused then 200 OK |
| STORY-126 | Health Check Failure Cascade Analysis | long | Docker down + disk full + DB locked; triage and recover |
| STORY-127 | Health During Tenant Provisioning | medium | Monitor disk/Docker while 3 databases provisioning |
| STORY-128 | Basic vs Detailed Health Comparison | short | Decision matrix for which endpoint to use |

### 11. Activity & Audit Trail (15 stories)

| ID | Title | Type | Summary |
|---|---|---|---|
| STORY-128 | Verify Account Created | short | Query activity after registration; see tenant.created event |
| STORY-129 | 30-Second Status Check | short | Quick activity poll for recent events across all resources |
| STORY-130 | Debug a Deployment Failure | medium | Trace service.created -> build.queued -> build.failed in activity |
| STORY-131 | Compliance: List All Key Rotations | medium | Filter activity for api_key.revoked events over 90 days |
| STORY-132 | Circuit Breaker Activation Investigation | medium | Trace service.running -> service.failed -> circuit_open in activity |
| STORY-133 | Database Provisioning Workflow Tracking | medium | Poll activity for database.created -> database.ready events |
| STORY-134 | Build Pipeline Monitoring via Activity | medium | Track build.queued -> build.started -> build.succeeded |
| STORY-135 | Key Revocation Verification | medium | Confirm api_key.created and api_key.revoked events in activity |
| STORY-136 | Cross-Resource Activity Timeline | medium | Interleaved timeline: service + database + build events |
| STORY-137 | Rate Limit Troubleshooting | short | Activity endpoint itself respects 100 req/s tenant limit |
| STORY-138 | Tenant Suspension Audit | medium | Find tenant.suspended event timestamp for billing cutoff |
| STORY-139 | Large-Scale Environment Discovery | medium | New SRE scans activity to understand tenant resource layout |
| STORY-140 | Transient Failure vs Circuit Breaker Diagnosis | medium | Distinguish auto-recovery cycles from stuck circuit in activity |
| STORY-141 | Security Event Forensics | medium | Trace suspicious api_key.created event for incident response |
| STORY-142 | Integration Test: All Event Types | medium | Create service, build, DB, key, revoke; verify all event types |

### 12. Deployment History & Redeploy (15 stories)

| ID | Title | Type | Summary |
|---|---|---|---|
| STORY-143 | View Deployment History After Creation | short | Query deployments endpoint after service creation |
| STORY-144 | Restart vs Redeploy Terminology | medium | Redeploy is explicit alias for restart; no build triggered |
| STORY-145 | Redeploy After Env Var Changes | medium | Update env, redeploy, verify new config active |
| STORY-146 | Deployment During Circuit Breaker Open | medium | Redeploy blocked (409); reset circuit; retry succeeds |
| STORY-147 | Redeploy Fails Due to Disk Space | medium | Disk at 91%; redeploy returns 503 |
| STORY-148 | Deployment History for Long-Running Service | medium | Current limitation: only latest deployment record returned |
| STORY-149 | Redeploy During Container Crash Recovery | medium | No container exists; must full-deploy instead of redeploy |
| STORY-150 | Redeploy with Port Preservation | short | Port config preserved through redeploy; routing uninterrupted |
| STORY-151 | Redeploy Blocked by Queue Overflow | medium | 50 services queued; additional redeploy gets "queue full" |
| STORY-152 | Redeploy Preserves Env Vars | medium | Verify env vars persist through redeploy cycle |
| STORY-153 | Redeploy Timeline with Concurrent Ops | medium | Sub-3-second redeploy: stop, remove, create, start |
| STORY-154 | Redeploy Failure with Cleanup Verification | medium | Partial failure leaves service in recoverable state |
| STORY-155 | Deployment Status Across Multiple Services | medium | List all services; batch redeploy crashed ones |
| STORY-156 | Idempotent Redeploy with Retry | medium | SHA-256 body hash idempotency; network retry safe |
| STORY-157 | Redeploy with Service Name Update | medium | PATCH name then redeploy; DNS label updated by Traefik |

## Gaps & Recommendations

### Covered but Limited

1. **Deployment history table** -- STORY-148 explicitly notes the dedicated deployments table is not yet implemented (issue #6). Current deployment history returns only the latest record derived from `service.updated_at`. Stories in this topic assume the current implementation but flag the limitation.

2. **Per-tenant rate limiting on general endpoints** -- Registration rate limiting (5/IP/hour) is well-covered (STORY-010), but no standalone story exercises the per-tenant 100 req/s limit on service/database/build endpoints. STORY-137 (activity) touches on it briefly. **Recommendation: Add a story for rate-limit behavior on high-frequency API calls.**

### Not Covered

3. **Reconciler behavior (30s loop)** -- Several stories mention the reconciler indirectly (STORY-045, STORY-046, STORY-087, STORY-116) but no story explicitly tests the reconciler's full 30-second cycle from an external API perspective. The reconciler is internal; coverage is implicit.

4. **Garbage collector (5-min loop)** -- STORY-091 covers orphaned database cleanup. No story explicitly covers orphaned service container cleanup or dangling image pruning. **Recommendation: Add a story for GC of orphaned service containers.**

5. **Custom domain support** -- Discovery lists Traefik reverse proxy with TLS, and the CHANGELOG mentions custom domains in v0.4.0, but no endpoint appears in the Feature Map for custom domain management. If custom domain APIs exist, they need story coverage.

6. **Supervisory dashboard** -- Discovery notes the "optional supervisory dashboard is served separately." No stories cover dashboard interaction. This is likely intentional (dashboard is out of scope for API UX paths).

7. **Metering database** -- Discovery lists a separate metering database (`metering.db`) with 1 migration, but no API endpoints expose metering data and no stories cover it. If metering becomes user-facing, stories are needed.

8. **Service PATCH (update)** -- No endpoint for `PATCH /v1/services/{serviceID}` appears in the Feature Map, but STORY-157 references `PATCH /services/{serviceID}` for name updates. If this endpoint exists, it needs explicit coverage. If not, STORY-157 should be flagged as aspirational.
