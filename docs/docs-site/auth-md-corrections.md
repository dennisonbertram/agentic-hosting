# auth.md corrections (post 06 fact-check)

`docs-site/src/content/docs/auth.md` was aligned to the live code:

| Stale claim | Code |
|-------------|------|
| HMAC of the full key with the master key | HMAC of the **secret only** with `apiKeyHashKey` |
| Creating a key invalidates the tenant auth cache | `handleKeyCreate` does not call the invalidator |
| Revocation can lag 30s | Revoke and tenant delete evict the cache; only out-of-band DB edits lag |
| Recover / reactivate / quotas share the registration limiter | Separate `adminLimiter` (same numbers) |
| Recover always just mints | Full live keyring auto-revokes oldest live key (`warning` / `revoked_key_id`) |
| `ahc key revoke <id>` | Requires `--confirm` |

`CORRECTIONS.md` hashing row was updated to match.
