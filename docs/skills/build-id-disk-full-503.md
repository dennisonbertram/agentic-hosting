# Skill corrections: `build_id` and disk-full 503

Verification notes for the operator-skill patch applied 2026-10-21. The skill and `docs-site/CORRECTIONS.md` had drifted from `internal/` after #168.

## POST `/v1/services/{id}/builds` returns `build_id`

`internal/api/builds.go` `handleBuildCreate` writes:

```json
{"build_id": "<id>", "status": "...", "image": "..."}
```

The stored `builds.Build` type still serializes as `{"id": ...}` on list/get. Agents that parse the **create** body must use `build_id`, not `id`.

## Disk ≥90% is 503 + `Retry-After: 10` since #168

`apierr.Unavailable` is HTTP 503. `WriteAPIError` sets `Retry-After` to `apierr.RetryAfterSeconds` (`"10"`).

| Path | Message |
|------|---------|
| Builds (`StartBuild`) | `insufficient disk space for builds; try again later` |
| Databases, kanbans, deploy, restart, redeploy | `insufficient disk space; try again later` |

Service create is unchanged: 201, then the async `Deploy` marks `failed` and `last_error`.

## Incorrect assumption

The 2026-10-19 corrections table treated disk-full as an untyped 500. That was true before #168; it is not true on current `main`.
