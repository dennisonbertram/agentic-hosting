# HKDF Key-Separation Scheme

**Status:** Draft
**Issue:** #8
**Date:** 2026-03-20

---

## 1. Problem Statement

Today every encrypted column in the database uses the same 32-byte master key
directly with AES-256-GCM. This means:

1. **No key separation** -- a compromise of one ciphertext class (e.g. env var
   values) gives an attacker the ability to decrypt every other class (DB
   passwords, connection strings, kanban admin tokens).
2. **No ciphertext versioning** -- there is no way to distinguish the encryption
   scheme used for a given ciphertext, making future key rotation or algorithm
   changes dangerous.
3. **HMAC keys are unseparated** -- `HashAPIKey` uses the raw master key as the
   HMAC secret, so the AES encryption key and HMAC authentication key are the
   same bytes.

## 2. Goals

- Derive purpose-specific 256-bit subkeys from the master key using HKDF-SHA256
  (RFC 5869).
- Tag every ciphertext with a version byte so `Decrypt` can distinguish legacy
  (v0) from new (v1+) formats.
- Provide a backwards-compatible migration path with zero downtime.
- Make rollback safe: the system can read both formats at any point.

## 3. HKDF Derivation

We use `golang.org/x/crypto/hkdf` with SHA-256.

```
IKM  = master key (32 bytes, loaded from disk at startup)
salt = nil           (master key already has full entropy)
info = purpose string (see table below)
L    = 32 bytes      (AES-256 key size)
```

The `info` parameter is a fixed ASCII string unique to each purpose. It MUST NOT
contain variable data (no tenant IDs, no row IDs). Purpose strings are
compile-time constants in `internal/crypto/purposes.go`.

### Why no salt?

The master key is generated from `crypto/rand` (256 bits of entropy). HKDF's
salt is most useful when the input keying material has non-uniform entropy (e.g.
a Diffie-Hellman shared secret). With a uniformly random IKM the extract step
is effectively a no-op, so a nil salt is safe and avoids an extra secret to
manage.

## 4. Purpose Table

| Purpose constant | `info` string | Used by | Call sites |
|---|---|---|---|
| `PurposeEnvVarEncryption` | `"ah/v1/env-var-encryption"` | Encrypting service environment variable values | `services.go:452` (Create), `services.go:1398` (UpdateEnvVars) |
| `PurposeEnvVarDecryption` | (same key as above) | Decrypting service environment variable values | `services.go:1431` (GetEnvVars), `snapshots.go:304` (snapshot restore) |
| `PurposeDBPasswordEncryption` | `"ah/v1/db-password-encryption"` | Encrypting provisioned database passwords | `databases.go:167` (Create) |
| `PurposeDBConnStringEncryption` | `"ah/v1/db-connstr-encryption"` | Encrypting provisioned database connection strings | `databases.go:192` (Create) |
| `PurposeDBConnStringDecryption` | (same key as above) | Decrypting provisioned database connection strings | `databases.go:373` (GetConnectionString) |
| `PurposeKanbanTokenEncryption` | `"ah/v1/kanban-token-encryption"` | Encrypting kanban admin passwords | `kanbans.go:299` (Create) |
| `PurposeKanbanTokenDecryption` | (same key as above) | Decrypting kanban admin passwords | `kanbans.go:374` (GetAdminToken) |
| `PurposeAPIKeyHMAC` | `"ah/v1/api-key-hmac"` | HMAC-SHA256 of API key secrets | `auth.go:83`, `tenants.go:248`, `recovery.go:121` |

Note: Encryption and decryption for a given data class use the **same** derived
key (the separation is between data classes, not between encrypt/decrypt). The
table lists both directions for traceability.

### Detailed Call-Site Inventory

#### `internal/services/services.go`

| Line | Function | Direction | Current code |
|---|---|---|---|
| 452 | `Create` | encrypt | `crypto.Encrypt([]byte(v), m.masterKey)` -- env var values |
| 1398 | `UpdateEnvVars` | encrypt | `crypto.Encrypt([]byte(v), m.masterKey)` -- env var values |
| 1431 | `GetEnvVars` | decrypt | `crypto.Decrypt(encrypted, m.masterKey)` -- env var values |

#### `internal/databases/databases.go`

| Line | Function | Direction | Current code |
|---|---|---|---|
| 167 | `Create` | encrypt | `crypto.Encrypt([]byte(password), m.masterKey)` -- DB password |
| 192 | `Create` | encrypt | `crypto.Encrypt([]byte(connStr), m.masterKey)` -- connection string |
| 373 | `GetConnectionString` | decrypt | `crypto.Decrypt(connStrEnc, m.masterKey)` -- connection string |

Note: `password_encrypted` is written at line 167 but never read back via
`crypto.Decrypt` in production code (the password is embedded in the connection
string). It exists as a recovery-only column. It still needs a separate purpose
key so it cannot be confused with a connection-string ciphertext.

#### `internal/kanbans/kanbans.go`

| Line | Function | Direction | Current code |
|---|---|---|---|
| 299 | `Create` | encrypt | `crypto.Encrypt([]byte(adminPassword), m.masterKey)` -- admin token |
| 374 | `GetAdminToken` | decrypt | `crypto.Decrypt(tokenEnc.String, m.masterKey)` -- admin token |

#### `internal/snapshots/snapshots.go`

| Line | Function | Direction | Current code |
|---|---|---|---|
| 304 | `decryptEnvVars` | decrypt | `crypto.Decrypt(cipherHex, m.masterKey)` -- env var values |

#### `internal/api/*.go` (HMAC, not AES)

| Line | File | Current code |
|---|---|---|
| 83 | `auth.go` | `crypto.HashAPIKey(apiKey, s.masterKey)` |
| 248 | `tenants.go` | `crypto.HashAPIKey(apiKey, s.masterKey)` |
| 121 | `recovery.go` | `crypto.HashAPIKey(apiKey, s.masterKey)` |

## 5. Ciphertext Versioning

### Current (legacy) format -- version 0

```
hex( nonce || AES-GCM-ciphertext )
```

The output of `crypto.Encrypt` today is a hex-encoded blob whose first 12 bytes
(24 hex chars) are the GCM nonce. There is no version marker.

### New format -- version 1

```
hex( 0x01 || purpose_tag[4] || nonce[12] || AES-GCM-ciphertext )
```

| Field | Size | Description |
|---|---|---|
| Version byte | 1 byte | `0x01` -- identifies the HKDF scheme. |
| Purpose tag | 4 bytes | First 4 bytes of `SHA-256(info)`. Allows `Decrypt` to verify the caller passed the correct purpose. Acts as a guardrail, not a security boundary. |
| Nonce | 12 bytes | Random AES-GCM nonce. |
| Ciphertext + tag | variable | AES-256-GCM authenticated ciphertext (includes 16-byte auth tag). |

### How `Decrypt` distinguishes legacy vs new

After hex-decoding, inspect the first byte:

1. **If `raw[0] == 0x01`**: parse as v1. Extract purpose tag (bytes 1-4), nonce
   (bytes 5-16), ciphertext (bytes 17+). Derive the subkey for the given
   purpose, verify the purpose tag matches, then decrypt.
2. **Otherwise**: treat as v0 (legacy). The first 12 bytes are the nonce, the
   rest is the ciphertext. Decrypt using the raw master key directly.

This works because AES-GCM nonces are random, and the probability of a random
nonce starting with `0x01` is ~1/256 (0.4%). However, we do NOT rely on this
for correctness -- on v0 fallback we simply attempt decryption with the master
key, and if the GCM authentication tag fails, we return an error. The version
byte is an optimization that lets us pick the right key on the first try in
>99.6% of cases; for the rare legacy ciphertext whose nonce happens to start
with `0x01`, the v1 parse will fail the purpose-tag check or GCM auth, and we
fall back to v0.

**Full Decrypt logic (pseudocode):**

```go
func DecryptV(ciphertextHex string, masterKey []byte, purpose Purpose) ([]byte, error) {
    raw, _ := hex.DecodeString(ciphertextHex)

    if len(raw) > 17 && raw[0] == 0x01 {
        // Try v1 first
        purposeTag := raw[1:5]
        if matchesPurpose(purposeTag, purpose) {
            subkey := hkdfDerive(masterKey, purpose)
            nonce  := raw[5:17]
            ct     := raw[17:]
            plain, err := aesGCMOpen(subkey, nonce, ct)
            if err == nil {
                return plain, nil
            }
            // Fall through to v0 attempt
        }
    }

    // v0 legacy: nonce is first 12 bytes, rest is ciphertext
    if len(raw) < 12 {
        return nil, errors.New("ciphertext too short")
    }
    nonce := raw[:12]
    ct    := raw[12:]
    return aesGCMOpen(masterKey, nonce, ct)
}
```

## 6. Worked Examples

### Example A: Decrypting a legacy (v0) ciphertext

Suppose we stored an environment variable value before the HKDF migration:

```
Stored hex: "a1b2c3d4e5f6a7b8c9d0e1f2<...gcm_ciphertext...>"
```

1. Hex-decode -> `raw` bytes.
2. `raw[0] == 0xa1` which is != `0x01`, so we skip the v1 path.
3. Parse as v0: `nonce = raw[:12]`, `ciphertext = raw[12:]`.
4. Call `aes.NewCipher(masterKey[:32])` -> GCM -> `Open(nonce, ciphertext)`.
5. Return plaintext.

### Example B: Decrypting a new (v1) ciphertext

After migration, a new env var value is encrypted with the HKDF scheme:

```
Stored hex: "01<purpose_tag_4bytes><nonce_12bytes><gcm_ciphertext>"
```

1. Hex-decode -> `raw` bytes.
2. `raw[0] == 0x01` -> v1 path.
3. Extract `purposeTag = raw[1:5]`. Verify it matches
   `SHA256("ah/v1/env-var-encryption")[:4]`.
4. Extract `nonce = raw[5:17]`, `ciphertext = raw[17:]`.
5. Derive subkey: `hkdf.New(sha256.New, masterKey, nil, []byte("ah/v1/env-var-encryption"))`.
   Read 32 bytes.
6. Call `aes.NewCipher(subkey)` -> GCM -> `Open(nonce, ciphertext)`.
7. Return plaintext.

### Example C: A v0 ciphertext whose nonce starts with 0x01 (edge case)

```
Stored hex: "01<random_11_nonce_bytes><gcm_ciphertext>"
```

1. Hex-decode -> `raw` bytes.
2. `raw[0] == 0x01` -> try v1 path.
3. Extract `purposeTag = raw[1:5]`. It does NOT match the expected purpose tag
   (it is random nonce bytes, not a SHA-256 prefix).
4. Skip v1 path, fall through to v0.
5. Parse as v0: `nonce = raw[:12]`, `ciphertext = raw[12:]`.
6. Decrypt with master key -> success.

If by extraordinary coincidence the random bytes also match the purpose tag, the
GCM `Open` call will fail because the derived subkey is different from the
master key. We then fall through to v0 and succeed.

## 7. HMAC Key Separation

`HashAPIKey` currently uses the master key directly as the HMAC secret. After
migration it will use a derived key:

```go
hmacKey := hkdfDerive(masterKey, PurposeAPIKeyHMAC)
// hmacKey is used in hmac.New(sha256.New, hmacKey)
```

**HMAC migration is NOT backwards-compatible** -- changing the key changes every
hash output. This requires re-hashing all stored API key hashes in a single
transaction. See the migration strategy (Phase 2) below.

## 8. Migration Strategy

### Phase 0: Ship the code (read v0+v1, write v0)

1. Add `internal/crypto/hkdf.go` with `DeriveKey(masterKey, purpose)`.
2. Add `internal/crypto/versioned.go` with `EncryptV` / `DecryptV`.
3. Update all `Decrypt` call sites to use `DecryptV` (can read both v0 and v1).
4. **Keep all `Encrypt` call sites writing v0 format** (no behavior change yet).
5. Deploy. Verify everything still works. This is a no-op deploy from the
   data perspective -- no ciphertext changes, no HMAC changes.

### Phase 1: Write v1, read v0+v1

1. Switch all `Encrypt` call sites to use `EncryptV` (writes v1 format).
2. Deploy. New rows get v1 ciphertext; old rows still have v0. Both are
   readable because `DecryptV` handles both.
3. Run a background migration job (`ah migrate-crypto`) that:
   - Reads each encrypted column.
   - Calls `DecryptV` (which handles v0 or v1).
   - Re-encrypts with `EncryptV` (v1 format).
   - Updates the row.
   - Processes in batches of 100 with 50ms sleep between batches to avoid
     holding the WAL writer lock.
4. After migration completes, all rows are v1.

### Phase 2: Migrate HMAC hashes

HMAC re-keying cannot be done incrementally (we cannot read the original API key
from the hash). Instead:

1. Add a `key_hash_v1` column to `api_keys` alongside the existing `key_hash`.
2. On every successful authentication, compute the v1 HMAC and populate
   `key_hash_v1` if it is NULL (we have the plaintext key in memory during
   auth).
3. After a soak period (e.g. 30 days), any key that still has `key_hash_v1 =
   NULL` has not been used in a month. Force-rotate those keys (issue new
   credentials to the tenant).
4. Once all rows have `key_hash_v1`, switch auth to read `key_hash_v1` and drop
   the old column.

### Phase 3: Remove v0 support

1. Verify no v0 ciphertexts remain:
   ```sql
   -- Env vars: check first byte of hex (first 2 chars)
   SELECT COUNT(*) FROM service_env_vars
   WHERE substr(value_encrypted, 1, 2) != '01';

   -- Databases
   SELECT COUNT(*) FROM databases
   WHERE substr(password_encrypted, 1, 2) != '01'
      OR substr(connection_string_encrypted, 1, 2) != '01';

   -- Kanbans
   SELECT COUNT(*) FROM kanbans
   WHERE admin_token_encrypted IS NOT NULL
     AND substr(admin_token_encrypted, 1, 2) != '01';
   ```
2. If counts are zero, remove the v0 fallback from `DecryptV`.
3. Remove the old `Encrypt` / `Decrypt` functions (or mark them as deprecated
   and unexported).

## 9. Rollout Order

| Step | Change | Risk | Rollback |
|---|---|---|---|
| 1 | Add `hkdf.go`, `versioned.go`, purpose constants. No call-site changes. | None -- dead code. | Revert commit. |
| 2 | Replace `Decrypt` calls with `DecryptV`. Still writing v0. | Low -- `DecryptV` v0 path is identical to old `Decrypt`. | Revert commit; no data change. |
| 3 | Replace `Encrypt` calls with `EncryptV`. New rows written as v1. | Medium -- new ciphertext format. | Revert commit; old code can still read v1 rows because `DecryptV` was already deployed in step 2. **If step 2 is also reverted**, v1 rows become unreadable -- see rollback section. |
| 4 | Run `ah migrate-crypto` to re-encrypt existing rows as v1. | Medium -- bulk write. | Restore from pre-migration SQLite backup. |
| 5 | Add `key_hash_v1` column, start dual-writing HMAC hashes. | Low -- additive schema change. | Drop column. |
| 6 | Switch auth to `key_hash_v1`. | Medium -- auth path change. | Revert to reading `key_hash`. |
| 7 | Remove v0 fallback code. | Low -- cleanup. | Re-add v0 fallback if a missed row surfaces. |

## 10. Rollback Behavior

### Rolling back after step 2 only (DecryptV deployed, still writing v0)

No data has changed. Reverting to the old `Decrypt` function works immediately.

### Rolling back after step 3 (writing v1, some rows are v1)

Two sub-cases:

**A. Step 2 is still deployed (DecryptV in place):** Safe. `DecryptV` reads
both v0 and v1. Simply revert the encrypt-side change to go back to writing v0.
Existing v1 rows remain readable.

**B. Full revert (steps 2 AND 3 reverted):** The old `Decrypt` cannot read v1
ciphertexts. Recovery options:
1. Re-deploy step 2 (just the `DecryptV` reader).
2. Or run a one-off script that re-encrypts v1 rows back to v0 using the master
   key (this is always possible because `DeriveKey` is deterministic from the
   same master key).

**Recommendation:** Never revert step 2 while v1 data exists in the database.
The rollout order guarantees step 2 is deployed before any v1 data is written.

### Rolling back after step 4 (bulk migration complete)

All rows are v1. Reverting step 3 (encrypt side) is safe because step 2
(`DecryptV`) still reads v1. To fully revert to v0, restore the pre-migration
SQLite backup.

### Rolling back HMAC migration (steps 5-6)

If step 6 is reverted (switch auth back to `key_hash`), the old hashes are
still present and correct. The `key_hash_v1` column can be dropped later.

## 11. New File Layout

```
internal/crypto/
  crypto.go         -- existing Encrypt/Decrypt (kept for v0 compat, eventually deprecated)
  hkdf.go           -- DeriveKey(masterKey, purpose) -> 32-byte subkey
  purposes.go       -- purpose constants (PurposeEnvVarEncryption, etc.)
  versioned.go      -- EncryptV(plaintext, masterKey, purpose) / DecryptV(ciphertextHex, masterKey, purpose)
  versioned_test.go -- round-trip tests, legacy compat tests, purpose-tag mismatch tests
```

## 12. API Surface Changes

```go
// purposes.go
type Purpose string

const (
    PurposeEnvVarEncryption     Purpose = "ah/v1/env-var-encryption"
    PurposeDBPasswordEncryption Purpose = "ah/v1/db-password-encryption"
    PurposeDBConnStrEncryption  Purpose = "ah/v1/db-connstr-encryption"
    PurposeKanbanTokenEncryption Purpose = "ah/v1/kanban-token-encryption"
    PurposeAPIKeyHMAC           Purpose = "ah/v1/api-key-hmac"
)

// hkdf.go
func DeriveKey(masterKey []byte, purpose Purpose) ([]byte, error)

// versioned.go
func EncryptV(plaintext []byte, masterKey []byte, purpose Purpose) (string, error)
func DecryptV(ciphertextHex string, masterKey []byte, purpose Purpose) ([]byte, error)
func DeriveHMACKey(masterKey []byte) ([]byte, error)  // convenience for PurposeAPIKeyHMAC
```

## 13. Security Considerations

- **Purpose tags are not a security boundary.** They prevent accidental
  cross-purpose decryption (developer error), not adversarial attacks. The real
  isolation comes from HKDF producing independent keys for each purpose.
- **No tenant-scoped keys.** HKDF `info` uses only purpose strings, not tenant
  IDs. Tenant isolation is enforced at the SQL layer (every query includes
  `WHERE tenant_id = ?`). Adding tenant-scoped keys would require per-tenant key
  material management, which is out of scope for this iteration.
- **Master key rotation** is not covered by this design. HKDF subkey derivation
  is deterministic, so rotating the master key requires re-encrypting all data
  (same as today). A future iteration can add a `key_id` field to support
  multiple master keys.
- **The v0 fallback MUST be time-bounded.** After Phase 3 verification queries
  confirm zero v0 rows, the fallback code should be removed to reduce attack
  surface (an attacker who obtains the master key could craft a v0 ciphertext to
  bypass purpose separation).

## 14. Open Questions

1. **Should `password_encrypted` in the databases table use the same purpose as
   `connection_string_encrypted`?** Currently they are separate purposes. The
   password column is write-only in production (never decrypted). If we want to
   minimize purpose proliferation we could share the key, but separate purposes
   are safer.
   **Decision:** Keep them separate. The cost is negligible (one more HKDF
   derivation cached at startup) and it follows the principle of least privilege.

2. **Should HKDF-derived keys be cached?** Deriving a key costs one
   HKDF-Expand call (~1 microsecond). For hot paths (env var decryption during
   container start), caching avoids repeated derivation.
   **Decision:** Yes -- derive all subkeys once at startup and store them in a
   `KeyRing` struct passed to each manager. This also makes testing easier
   (inject a mock key ring).

3. **Should we add a `crypto_version` column to each table instead of embedding
   the version in the ciphertext?** A column would be more explicit but requires
   schema changes to every encrypted table.
   **Decision:** Embed in the ciphertext. It is self-contained, requires no
   schema changes, and survives data export/import.
