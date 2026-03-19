-- Dev environments: sandboxed development containers with shell access
CREATE TABLE IF NOT EXISTS environments (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'creating',
  base_image TEXT NOT NULL,
  container_id TEXT DEFAULT '',
  volume_name TEXT NOT NULL,
  idle_timeout_sec INTEGER NOT NULL DEFAULT 1800,
  last_activity_at INTEGER,
  last_error TEXT DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_environments_tenant ON environments(tenant_id);
CREATE INDEX IF NOT EXISTS idx_environments_status ON environments(status);

ALTER TABLE tenant_quotas ADD COLUMN max_environments INTEGER NOT NULL DEFAULT 3;
