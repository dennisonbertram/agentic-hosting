-- Server-held secrets, encrypted under the master key. Currently holds the
-- API-key HMAC key ("api_key_hmac"), so rotating the master key no longer
-- invalidates API key hashes. Rows are created lazily at startup.
CREATE TABLE IF NOT EXISTS server_secrets (
    name TEXT PRIMARY KEY,
    value_encrypted TEXT NOT NULL,
    created_at INTEGER NOT NULL
);
