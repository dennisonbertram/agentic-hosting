CREATE TABLE IF NOT EXISTS environment_previews (
    id TEXT PRIMARY KEY,
    environment_id TEXT NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    tenant_id TEXT NOT NULL,
    name TEXT NOT NULL,
    port INTEGER NOT NULL,
    dns_label TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_env_previews_env ON environment_previews(environment_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_env_previews_dns ON environment_previews(dns_label);
CREATE UNIQUE INDEX IF NOT EXISTS idx_env_previews_env_name ON environment_previews(environment_id, name);
