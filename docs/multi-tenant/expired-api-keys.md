# Expired API keys and the 20-key cap

## What changed

`POST /v1/auth/keys`, `GET /v1/auth/keys`, `GET /v1/tenant/usage`, and `POST /v1/auth/recover` treat a key as occupying a slot only when it is still live:

```sql
revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)
```

Expired-but-unrevoked rows used to count toward the 20-key limit and still appeared in the list, even though auth already rejected them with 401. A tenant that minted short-lived keys could be locked out of replacements until someone revoked the dead rows.

Recover still auto-revokes the oldest **live** key when all 20 slots are in use. It no longer prefers an expired key first, because that revoke does not free a slot.

## Why

Auth middleware already excluded expired keys (`internal/middleware/auth.go`). The quota and list queries did not. Closing that gap matches the 401 behavior.

## Not changed

- Expired keys are not auto-deleted. They stay in `api_keys` until something revokes them (tenant delete, recover of a full live keyring, or an explicit revoke of that id).
- `RevokeOldestExpired` remains in `internal/db` for other callers; recover no longer uses it for quota relief.
