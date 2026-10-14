---
title: Env vars & secrets
description: How service environment variables are validated, encrypted, merged, revealed, and denied.
section: Use
order: 8
---

# Env vars and secrets

Service env lives in `service_env`, encrypted with **AES-256-GCM**. The API never writes plaintext to SQLite.

## Set (merge, not replace)

```
POST /v1/services/{id}/env
{"DATABASE_URL":"...","NODE_ENV":"production"}
```

This is an **upsert**. Keys omitted from the body are left alone. There is no replace-all PATCH.

- Max **100 keys per request**
- Quota `max_env_vars_per_service` defaults to **50**
- Response notes that a **restart recreates** the container

You can also pass `"env"` on `POST /v1/services` at create time. Same validation.

## Get

```
GET /v1/services/{id}/env              # values masked
GET /v1/services/{id}/env?reveal=true  # plaintext — AUDIT logged
```

## Delete one key

```
DELETE /v1/services/{id}/env/{key}
```

## Validation (`services.ValidateEnvVars`)

| Rule | Value |
|------|--------|
| Key pattern | `^[a-zA-Z_][a-zA-Z0-9_]{0,127}$` |
| Value size | ≤ 32,768 bytes |
| Null bytes | rejected |
| Denied keys | `PATH`, `LD_PRELOAD`, `LD_LIBRARY_PATH` (**case-insensitive**) |

Denied keys return 400 with the key name as sent (`env var "path" is not allowed`).

## Master key

`/var/lib/ah/master.key` is 32 raw bytes encoded as 64 hex chars. Lose it and encrypted env + DB passwords are unrecoverable.

Rotate:

```bash
ah rotate-key --new-key-file /path/to/new.key
```

`cmd/ah/rotate_key.go` re-encrypts stored secrets. Back up the old key until you confirm services still start.

## Instant environment files

Workspace environments are **not** the same as service env vars.

- `POST /v1/environments/{id}/sync` clones `{"git_url":"https://...","git_ref":"HEAD"}` into the workspace (HTTPS only)
- There is **no** `GET/PUT /v1/environments/{id}/files` route in `setupRoutes` (older build-log text is stale)

Exec is `POST /v1/environments/{id}/exec` with `{"command":["ls","-la"],"work_dir":"/workspace","timeout":60}`. Exec is **not** idempotent (intentionally excluded from the idempotency middleware group). Timeout max 300s, default 60s.
