CREATE TABLE IF NOT EXISTS environments (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    template_id TEXT NOT NULL DEFAULT 'default',
    status TEXT NOT NULL DEFAULT 'creating',
    container_id TEXT DEFAULT '',
    volume_name TEXT DEFAULT '',
    lease_expires_at INTEGER,
    lease_duration_seconds INTEGER NOT NULL DEFAULT 3600,
    last_activity_at INTEGER,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_environments_tenant ON environments(tenant_id);
CREATE INDEX IF NOT EXISTS idx_environments_status ON environments(status);
CREATE INDEX IF NOT EXISTS idx_environments_lease ON environments(lease_expires_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_environments_tenant_name ON environments(tenant_id, name);

CREATE TABLE IF NOT EXISTS environment_templates (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    base_image TEXT NOT NULL DEFAULT 'ubuntu:24.04',
    description TEXT DEFAULT '',
    tools TEXT DEFAULT '[]',
    memory_mb INTEGER NOT NULL DEFAULT 512,
    cpu_millicores INTEGER NOT NULL DEFAULT 1000,
    disk_mb INTEGER NOT NULL DEFAULT 1024,
    egress_policy TEXT NOT NULL DEFAULT 'block',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

INSERT OR IGNORE INTO environment_templates (id, name, base_image, description, tools, memory_mb, cpu_millicores, disk_mb, egress_policy, created_at, updated_at)
VALUES ('tmpl_default', 'default', 'ubuntu:24.04', 'Default Ubuntu workspace', '[]', 512, 1000, 1024, 'allow', strftime('%s','now'), strftime('%s','now'));

ALTER TABLE tenant_quotas ADD COLUMN max_environments INTEGER NOT NULL DEFAULT 2;
