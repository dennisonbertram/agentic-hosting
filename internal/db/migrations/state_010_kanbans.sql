-- Kanban board instances (Vikunja per tenant)
CREATE TABLE IF NOT EXISTS kanbans (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    status TEXT NOT NULL DEFAULT 'provisioning',
    container_id TEXT,
    host TEXT DEFAULT '127.0.0.1',
    port INTEGER,
    admin_token_encrypted TEXT,
    url TEXT,
    volume_name TEXT,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE(tenant_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_kanbans_port_unique ON kanbans(port) WHERE status NOT IN ('failed');
