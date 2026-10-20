# Operations Reference

## Idempotency

Add `Idempotency-Key: <uuid>` to **POST and PUT** requests to make retries safe. DELETE and PATCH are not covered. No PUT routes exist today.

```bash
-H "Idempotency-Key: $(uuidgen)"
```

Rules:
- Same key + same tenant + same method + same path + same body hash = replayed response
- Different body with the same key → **409**
- Only **2xx** responses are stored; failed requests re-run on retry
- Most important for: database creation and build triggers (the two slowest operations)
- Key scope: per-tenant, per-endpoint, per HTTP method
- Expiry: keys expire after **10 minutes** (`idempotencyTTL` in `internal/middleware/idempotency.go`)
- In-memory only (restart clears); max 50 keys per tenant (oldest evicted)
- `POST /v1/auth/keys` and `POST /v1/environments/{id}/exec` are excluded

## Rate Limits

| Scope | Limit |
|-------|-------|
| Per tenant | 100 req/s, burst 200 |
| Global | 500 req/s, burst 1000 |
| Tenant registration per IP | 5 per hour |
| Tenant registration global | 20 per hour |

On `429 Too Many Requests`: read the `Retry-After` header. If absent, back off exponentially (60s → 120s → 240s). Never hammer in a loop.

## Resource Limits

| Resource | Limit |
|----------|-------|
| Default memory / CPU (new tenant) | **2048 MB / 2 CPUs** from `tenant_quotas` (applied as container limits). Fallback 512 MB / 1 CPU only if the quota row cannot be loaded. Configurable per tenant; still bounded by the host. |
| Databases per tenant | 3 |
| API keys per tenant | 20 |
| Env vars per service | Schema default **50** (`max_env_vars_per_service`); a single POST may set at most **100** keys |
| Request body | 1 MB |
| Build log max size | 5 MB |
| Deploy timeout | 10 minutes |
| Database creation | Up to 30 seconds |

## Circuit Breaker

The circuit breaker opens after **5 crashes within 10 minutes**. When open:
- `status` stays `crashed`; `circuit_open` is a **boolean** (never a status string)
- Container is **stopped and removed**; `container_id` is cleared
- `/logs` returns 409 `service has no container`
- Reconciler will not restart it automatically

Recovery:
1. Investigate and fix the root cause (bad env var, OOM, crashing app). Use `GET /v1/services/$SERVICE_ID` (`last_error`) and `/deployments` — not `/logs`
2. Deploy a new image or fix config
3. Reset: `POST /v1/services/$SERVICE_ID/reset`
4. Redeploy: `POST /v1/services/$SERVICE_ID/redeploy` — **not** `/start` (start returns 409 because reset removed the container)
5. Watch for `running` status

**Do not loop-reset without fixing root cause.** The circuit breaker protects the server.

Auto-recovery: exponential backoff (30m → 1h → 4h) **closes the circuit and leaves the service stopped**. Nothing restarts it. Use `/redeploy` (or a new build). If it keeps crashing, the circuit re-opens.

## Disk Management

| Threshold | Effect |
|-----------|--------|
| 80% | Warning logged |
| 90% | New deployments blocked |

Check disk:
```bash
curl -s -H "Authorization: Bearer $AH_KEY" $AH_URL/v1/system/health/detailed | python3 -m json.tool
```

Free space:
```bash
ssh root@<server> "docker image prune -af && docker container prune -f"
```

## Reconciler

The reconciler runs every **30 seconds**. Docker itself restarts crashed containers (`unless-stopped`). The reconciler observes those restarts, counts crashes, and trips the breaker. Unhealthy containers are **restarted** (not stopped). A missing container on a `running` row is counted as a crash — it is not silently recreated.

Implication: `docker stop` on a tenant container is treated as a crash. Always use the API to manage service state.

## Garbage Collector

Runs every 5 minutes. Cleans up:
- Dangling Docker images
- Stopped containers for deleted services
- Orphaned volumes

Deletion via the API is soft-delete — the GC handles actual container/volume cleanup asynchronously.

## Full Error Reference

| Status Code | Meaning | Fix |
|-------------|---------|-----|
| `400 Bad Request` | Malformed JSON or missing required field | Check request body |
| `401 Unauthorized` | Missing, expired, or revoked API key | Check `Authorization: Bearer keyid.secret` format |
| `403 Forbidden` | Valid key but wrong tenant for resource | Check you're using the right key |
| `404 Not Found` | Resource doesn't exist | List first, then operate on specific IDs |
| `409 Conflict` | Duplicate name or idempotency conflict | Check if resource already exists |
| `422 Unprocessable Entity` | Validation failed | Read error message; common: name already taken, bad email format |
| `429 Too Many Requests` | Rate limited | Respect `Retry-After` header; exponential backoff |
| `500 Internal Server Error` | Untyped errors (`{"error":"internal error"}`) | Check `/v1/system/health/detailed?fresh=true` and `journalctl -u ah -n 50` |
| `503 Service Unavailable` | Docker/build/metering/registration unavailable (manager nil); disk ≥90% on build, DB create, kanban create, deploy and restart/redeploy (`{"error":"insufficient disk space; try again later"}`, `Retry-After: 10`); deploy queue full | Respect `Retry-After`; check `/v1/system/health/detailed?fresh=true` |

## Pagination

List endpoints accept `limit` and `offset` query parameters:

```bash
# First page of 20
curl -s -H "Authorization: Bearer $AH_KEY" "$AH_URL/v1/services?limit=20&offset=0"

# Next page
curl -s -H "Authorization: Bearer $AH_KEY" "$AH_URL/v1/services?limit=20&offset=20"
```

Default limit is **100** (`parsePagination`); max is 200 per request. Activity feed defaults to **50**.

## Activity Feed

Synthetic event log across all tenant resources:

```bash
curl -s -H "Authorization: Bearer $AH_KEY" "$AH_URL/v1/activity?limit=50" | python3 -m json.tool
```

Events include: service created/started/stopped/failed, database created/deleted, builds started/succeeded/failed, circuit breaker open/closed.

## Tenant Management

```bash
# Get tenant info
curl -s -H "Authorization: Bearer $AH_KEY" $AH_URL/v1/tenant | python3 -m json.tool

# Get usage stats
curl -s -H "Authorization: Bearer $AH_KEY" $AH_URL/v1/tenant/usage | python3 -m json.tool

# Update tenant name (email is not updatable)
curl -s -X PATCH $AH_URL/v1/tenant \
  -H "Authorization: Bearer $AH_KEY" -H "Content-Type: application/json" \
  -d '{"name":"new-name"}'

# Delete tenant (irreversible — removes all services, databases, keys)
curl -s -X DELETE $AH_URL/v1/tenant -H "Authorization: Bearer $AH_KEY"
```

## Server Maintenance (requires SSH)

```bash
# Restart ah daemon
ssh root@<server> "systemctl restart ah"

# Watch live logs
ssh root@<server> "journalctl -u ah -f"

# Check what's running
ssh root@<server> "docker ps --format 'table {{.Names}}\t{{.Status}}\t{{.Ports}}'"

# Check migrations applied
ssh root@<server> "sqlite3 /var/lib/ah/ah.db 'SELECT name FROM schema_migrations ORDER BY name;'"
```

## Backup

```bash
# Backup the state + metering databases (VACUUM INTO, gzip, keep last 10)
ssh root@<server> "ah backup"
# Output: /var/lib/ah/backups/ah-<timestamp>.db.gz (and metering sibling)

# The master key must also be backed up separately — without it, all
# encrypted database credentials become unrecoverable
ssh root@<server> "cat /var/lib/ah/master.key"
# Store this securely off-server
```
