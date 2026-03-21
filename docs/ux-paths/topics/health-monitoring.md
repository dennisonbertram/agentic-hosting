# Health Monitoring — UX Path Stories

Health monitoring provides two levels of system health visibility: a lightweight public endpoint for uptime checks (`GET /v1/system/health`) and a detailed authenticated endpoint for operational diagnostics (`GET /v1/system/health/detailed`). Responses are cached for 30 seconds to prevent system hammering.

## STORY-001: Basic Health Check — Uptime Probe

**Type**: short
**Persona**: External uptime monitoring service (e.g., UptimeRobot, Pingdom)
**Goal**: Verify the API server is responding to requests
**Preconditions**: Monitoring service configured with health endpoint URL

### Steps
1. Monitoring service sends unauthenticated request:
   ```
   GET /v1/system/health
   ```
2. Response (200 OK):
   ```json
   {
     "status": "ok"
   }
   ```
3. Response time: < 1ms (constant-time, no database calls)
4. Monitoring service records "UP" status
5. If no response within 5 seconds, monitoring service alerts on-call team

### Variations
- **Server overloaded**: Endpoint still responds quickly (no DB or Docker dependencies)
- **Database down**: Endpoint still returns 200 "ok" (does not check DB)
- **Docker down**: Endpoint still returns 200 "ok" (does not check Docker)

### Edge Cases
- **No authentication required**: This endpoint is public; any client can call it
- **DoS resistance**: Designed to be constant-time with no backend dependencies; safe to call at high frequency
- **Load balancer integration**: Can be used as Traefik health check for the `ah` process itself

---

## STORY-002: Detailed Health Check — Operational Diagnostics

**Type**: medium
**Persona**: Platform operator investigating system performance
**Goal**: Get comprehensive system health including Docker, gVisor, disk, and database status
**Preconditions**: Valid API key, operator has authentication credentials

### Steps
1. Operator sends authenticated request:
   ```
   GET /v1/system/health/detailed
   Authorization: Bearer <api-key>
   ```
2. Response (200 OK):
   ```json
   {
     "status": "ok",
     "docker_version": "24.0.7",
     "gvisor_version": "release-20231113.0",
     "disk": {
       "total_gb": 100.0,
       "free_gb": 45.2,
       "used_percent": 54.8
     },
     "database": "ok"
   }
   ```
3. Operator confirms:
   - Docker daemon is running (version reported)
   - gVisor (runsc) is installed and accessible
   - Disk usage is healthy (54.8% < 80% warning threshold)
   - Database (SQLite) is responding to pings

### Variations
- **Docker daemon down**: docker_version = "" or error, but status may be "degraded"
- **gVisor not installed**: gvisor_version = "" with fallback parsing
- **Database unreachable**: database = "error", overall status = "degraded"

### Edge Cases
- **30-second cache**: Response is cached for 30 seconds; rapid polling returns identical results
- **Authentication required**: Returns 401 without valid API key (prevents information leakage)
- **Command timeouts**: Docker version and gVisor version checks have 5-second timeouts each

---

## STORY-003: Degraded Health — Database Unavailable

**Type**: medium
**Persona**: On-call engineer responding to an alert
**Goal**: Diagnose why the system is reporting degraded status
**Preconditions**: SQLite database file is locked or corrupted

### Steps
1. Monitoring triggers alert: detailed health returns "degraded"
2. Engineer checks detailed health:
   ```
   GET /v1/system/health/detailed
   Authorization: Bearer <api-key>
   ```
3. Response:
   ```json
   {
     "status": "degraded",
     "docker_version": "24.0.7",
     "gvisor_version": "release-20231113.0",
     "disk": {
       "total_gb": 100.0,
       "free_gb": 45.2,
       "used_percent": 54.8
     },
     "database": "error: database is locked"
   }
   ```
4. Engineer identifies: database is locked (concurrent write contention in WAL mode)
5. Engineer checks for long-running transactions or stuck processes
6. Engineer restarts the service if necessary:
   ```bash
   systemctl restart paasd.service
   ```
7. Re-checks health endpoint: status returns to "ok"

### Variations
- **SQLite file deleted**: Database ping fails; status = "degraded"
- **Disk full**: Database writes fail; concurrent disk check shows 95%+ used

### Edge Cases
- **Basic health still returns "ok"**: The public `/v1/system/health` endpoint does not check the database; it always returns "ok" if the server process is running
- **Cache masks recovery**: If DB recovers within 30s of the degraded response, cached response still shows "degraded" until cache expires

---

## STORY-004: Disk Usage Warning Thresholds

**Type**: medium
**Persona**: Infrastructure engineer monitoring storage capacity
**Goal**: Understand disk space thresholds and their implications
**Preconditions**: Server running with disk at various utilization levels

### Steps
1. **Normal state** (disk at 50%):
   ```json
   {
     "disk": {"total_gb": 100.0, "free_gb": 50.0, "used_percent": 50.0}
   }
   ```
   All operations proceed normally.

2. **Warning threshold** (disk at 82%):
   ```json
   {
     "disk": {"total_gb": 100.0, "free_gb": 18.0, "used_percent": 82.0}
   }
   ```
   Health status still "ok" but operator should take action.
   Service deployments still allowed but risk running out of space.

3. **Critical threshold** (disk at 93%):
   ```json
   {
     "disk": {"total_gb": 100.0, "free_gb": 7.0, "used_percent": 93.0}
   }
   ```
   Database provisioning blocked: "disk check: device at 93% capacity".
   Service deploys blocked: "disk check: insufficient space".
   Existing services continue running.

4. **Recovery**: Engineer cleans up old images and volumes:
   ```bash
   docker system prune -f
   ```
   Disk drops to 60%; operations resume.

### Variations
- **Disk path**: Health check measures `/var/lib/ah` specifically using `syscall.Statfs()`
- **Docker storage**: Docker's `/var/lib/docker` may fill separately; not checked by this endpoint

### Edge Cases
- **80% warning vs 95% block**: Warning is advisory; 95% blocks new deployments and database provisioning
- **No alerts built-in**: Health endpoint reports numbers; alerting must be configured externally (e.g., Prometheus scraping)

---

## STORY-005: Docker and gVisor Version Verification

**Type**: short
**Persona**: Security officer verifying runtime versions after a CVE
**Goal**: Confirm Docker and gVisor are patched to non-vulnerable versions
**Preconditions**: CVE announced for Docker < 24.0.8

### Steps
1. Officer checks versions:
   ```
   GET /v1/system/health/detailed
   Authorization: Bearer <api-key>
   ```
2. Response includes:
   ```json
   {
     "docker_version": "24.0.7",
     "gvisor_version": "release-20231113.0"
   }
   ```
3. Officer identifies Docker 24.0.7 is vulnerable (need 24.0.8+)
4. Officer schedules Docker upgrade with infrastructure team
5. After upgrade, re-checks: `docker_version: "24.0.8"` -- patched

### Variations
- **Docker not running**: docker_version field is empty or contains error message
- **gVisor not found**: gvisor_version field is empty; gVisor check uses `runsc --version`

### Edge Cases
- **Version check commands**: Docker uses `docker version --format "{{.Server.Version}}"` with 5s timeout; gVisor uses `runsc --version` with fallback parsing
- **Cached versions**: Versions are cached for 30 seconds; upgrade won't show immediately

---

## STORY-006: Automated Health Check Integration

**Type**: medium
**Persona**: DevOps engineer setting up monitoring pipeline
**Goal**: Integrate health endpoints with Prometheus and Grafana
**Preconditions**: Prometheus configured, Grafana dashboards available

### Steps
1. Engineer configures Prometheus scrape job:
   - Target: `https://api.agentic.io/v1/system/health/detailed`
   - Interval: 30 seconds (matches cache TTL)
   - Authorization header with API key
2. Prometheus scrapes the endpoint every 30 seconds
3. Metrics extracted:
   - `ah_health_status` (ok=1, degraded=0)
   - `ah_disk_used_percent` (54.8)
   - `ah_disk_free_gb` (45.2)
4. Grafana dashboard shows:
   - Disk usage trend over time
   - Health status history
   - Docker/gVisor version changes
5. Alert rules configured:
   - disk_used_percent > 80 -> warning notification
   - disk_used_percent > 90 -> critical page
   - health_status = degraded -> immediate page

### Variations
- **Basic endpoint for simple uptime**: Use `/v1/system/health` (no auth needed) for basic up/down monitoring
- **Detailed endpoint for metrics**: Use `/v1/system/health/detailed` for disk and version metrics

### Edge Cases
- **30-second cache alignment**: If Prometheus scrapes every 30s and cache TTL is 30s, each scrape may get a fresh or cached result depending on timing
- **Rate limiting**: Health endpoints are not rate-limited separately; covered by tenant API rate limit (100 req/s)

---

## STORY-007: Health Check During System Startup

**Type**: short
**Persona**: Systemd service manager verifying process readiness
**Goal**: Confirm `ah` process is ready to serve requests after restart
**Preconditions**: `paasd.service` restarting after upgrade

### Steps
1. Systemd restarts the service
2. Readiness probe polls basic health:
   ```
   GET /v1/system/health
   ```
3. First few attempts: connection refused (server not yet listening)
4. After ~2 seconds: 200 OK `{"status": "ok"}`
5. Systemd marks the service as "active (running)"
6. Traefik begins routing traffic to the process

### Variations
- **Slow database migrations**: Server may start HTTP listener before migrations complete; basic health returns "ok" but detailed health may show database issues
- **Port already in use**: Server fails to start; health check never succeeds

### Edge Cases
- **Startup ordering**: Basic health is available as soon as HTTP listener starts; detailed health requires Docker daemon and database to be ready
- **Migration failures**: If database migration fails, server may exit; health check connection refused

---

## STORY-008: Health Check Failure Cascade Analysis

**Type**: long
**Persona**: SRE investigating a complete system failure
**Goal**: Understand the failure cascade when multiple components fail simultaneously
**Preconditions**: Docker daemon crashed, disk at 98%, database locked

### Steps
1. SRE receives multiple alerts simultaneously
2. Checks basic health:
   ```
   GET /v1/system/health
   ```
   Response: 200 OK `{"status": "ok"}` -- process is alive
3. Checks detailed health:
   ```
   GET /v1/system/health/detailed
   Authorization: Bearer <api-key>
   ```
   Response:
   ```json
   {
     "status": "degraded",
     "docker_version": "",
     "gvisor_version": "release-20231113.0",
     "disk": {
       "total_gb": 100.0,
       "free_gb": 2.0,
       "used_percent": 98.0
     },
     "database": "error: database is locked"
   }
   ```
4. SRE identifies three issues:
   - Docker daemon not responding (empty version)
   - Disk nearly full (98% used)
   - Database locked (likely due to disk pressure)
5. Recovery plan:
   a. Free disk space first (root cause likely cascading to other failures)
   b. Restart Docker daemon
   c. Verify database recovers with free disk space
6. SRE executes recovery:
   ```bash
   docker system prune -af    # free Docker disk space
   systemctl restart docker    # restart Docker daemon
   systemctl restart paasd     # restart ah service (clears DB locks)
   ```
7. Re-check detailed health: all components "ok"

### Variations
- **Only Docker down**: Services can't deploy but existing containers keep running
- **Only disk full**: Deployments and database provisioning blocked; existing services still serve traffic
- **Only database locked**: API calls that write to DB fail; reads may succeed if cached

### Edge Cases
- **Cache during recovery**: Cached "degraded" response persists for up to 30s after fix; don't assume fix failed based on first check after remediation
- **Partial recovery**: Docker restarted but disk still full; deployments still fail even though Docker is "ok"

---

## STORY-009: Health Monitoring During Tenant Provisioning

**Type**: medium
**Persona**: Tenant admin provisioning multiple databases simultaneously
**Goal**: Monitor system health while heavy provisioning is in progress
**Preconditions**: Three databases being provisioned concurrently

### Steps
1. Tenant triggers 3 database provisions simultaneously
2. Checks health during provisioning:
   ```
   GET /v1/system/health/detailed
   ```
   Response shows:
   - disk used_percent rising (new volumes being created)
   - Docker version present (daemon handling container creation)
   - Database "ok" (SQLite handling writes)
3. After provisioning completes:
   - Disk usage increased by ~500MB per database
   - All databases in "ready" state
   - Health still "ok"

### Variations
- **Provisioning pushes disk past 80%**: Warning threshold crossed during provisioning; existing provisions complete but new ones may be blocked
- **Docker daemon overwhelmed**: Multiple container creations may slow Docker; version check may timeout (5s)

### Edge Cases
- **Health cache masks brief spikes**: If disk briefly crosses 95% during volume creation but drops back below within 30s, the spike may not be captured in health response

---

## STORY-010: Comparing Basic vs Detailed Health Endpoints

**Type**: short
**Persona**: New API consumer learning the platform
**Goal**: Understand which health endpoint to use for different purposes
**Preconditions**: API access available

### Steps
1. **Basic endpoint** (`GET /v1/system/health`):
   - No authentication required
   - Returns only `{"status": "ok"}`
   - No database, Docker, or disk checks
   - Constant-time response (< 1ms)
   - Use for: uptime monitoring, load balancer health checks, external status pages

2. **Detailed endpoint** (`GET /v1/system/health/detailed`):
   - Requires valid API key (`Authorization: Bearer <key>`)
   - Returns Docker version, gVisor version, disk usage, database status
   - Checks Docker daemon, gVisor binary, filesystem, SQLite
   - Cached for 30 seconds
   - Use for: operational dashboards, capacity planning, incident diagnostics, version auditing

### Decision Matrix
| Use Case | Endpoint | Auth Required |
|---|---|---|
| Uptime ping | `/v1/system/health` | No |
| Load balancer probe | `/v1/system/health` | No |
| Disk monitoring | `/v1/system/health/detailed` | Yes |
| Version auditing | `/v1/system/health/detailed` | Yes |
| Incident diagnosis | `/v1/system/health/detailed` | Yes |
| Status page | `/v1/system/health` | No |
| Capacity planning | `/v1/system/health/detailed` | Yes |

### Edge Cases
- **Unauthorized detailed check**: Returns 401; does not leak any system information
- **Basic endpoint during total failure**: Returns "ok" as long as the Go process is running, even if everything else is broken
