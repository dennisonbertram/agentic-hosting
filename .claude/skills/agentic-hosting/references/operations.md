# Operations Reference

## Idempotency

Add `Idempotency-Key: <uuid>` to any mutating request to make it safe to retry on network timeout:

```bash
-H "Idempotency-Key: $(uuidgen)"
```

Rules:
- Same key + same tenant + same endpoint = same result, no duplicate resource created
- Most important for: database creation and build triggers (the two slowest operations)
- Key scope: per-tenant, per-endpoint, per HTTP method
- Expiry: keys expire after 24 hours

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
| Databases per tenant | 3 |
| API keys per tenant | 20 |
| Env vars per service | 100 |
| Request body | 1 MB |
| Build log max size | 5 MB |
| Deploy timeout | 10 minutes |
| Database creation | Up to 30 seconds |

## Circuit Breaker

The circuit breaker opens after **5 crashes within 10 minutes**. When open:
- Service status becomes `circuit_open`
- Container is stopped
- Reconciler will not restart it automatically

Recovery:
1. Investigate and fix the root cause (bad env var, OOM, crashing app)
2. Deploy a new image or fix config
3. Reset: `POST /v1/services/$SERVICE_ID/reset`
4. Start: `POST /v1/services/$SERVICE_ID/start`
5. Watch for `running` status

**Do not loop-reset without fixing root cause.** The circuit breaker protects the server.

Auto-recovery: the server has exponential backoff auto-recovery (30m → 1h → 4h). This resets the circuit and attempts restart automatically — but only if the app actually runs successfully. If it keeps crashing, the circuit re-opens.

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

The reconciler runs every **30 seconds**. It scans all services and ensures desired state matches actual Docker state. If a service should be `running` but its container is gone, the reconciler restarts it within 30 seconds.

Implication: manually stopping a Docker container outside the API will cause it to restart within 30 seconds. Always use the API to manage service state.

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
| `500 Internal Server Error` | Server bug | Check server logs: `journalctl -u paasd -n 50` |
| `503 Service Unavailable` | Disk >90% or Docker/gVisor unavailable | Check `/v1/system/health/detailed` |

## Pagination

List endpoints accept `limit` and `offset` query parameters:

```bash
# First page of 20
curl -s -H "Authorization: Bearer $AH_KEY" "$AH_URL/v1/services?limit=20&offset=0"

# Next page
curl -s -H "Authorization: Bearer $AH_KEY" "$AH_URL/v1/services?limit=20&offset=20"
```

Default limit is 50; max is 200 per request.

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

# Update tenant name/email
curl -s -X PATCH $AH_URL/v1/tenant \
  -H "Authorization: Bearer $AH_KEY" -H "Content-Type: application/json" \
  -d '{"name":"new-name"}'

# Delete tenant (irreversible — removes all services, databases, keys)
curl -s -X DELETE $AH_URL/v1/tenant -H "Authorization: Bearer $AH_KEY"
```

## Server Maintenance (requires SSH)

```bash
# Restart paasd daemon
ssh root@<server> "systemctl restart paasd"

# Watch live logs
ssh root@<server> "journalctl -u paasd -f"

# Check what's running
ssh root@<server> "docker ps --format 'table {{.Names}}\t{{.Status}}\t{{.Ports}}'"

# Check migrations applied
ssh root@<server> "sqlite3 /var/lib/paasd/paasd.db 'SELECT name FROM schema_migrations ORDER BY name;'"
```

## Backup

```bash
# Backup the state database (contains all tenant/service/database metadata)
ssh root@<server> "/agentic-hosting/bin/paasd backup /var/lib/paasd/paasd.db"
# Output: /var/lib/paasd/backups/paasd-<timestamp>.db

# The master key must also be backed up separately — without it, all
# encrypted database credentials become unrecoverable
ssh root@<server> "cat /var/lib/paasd/master.key"
# Store this securely off-server
```
