CREATE TABLE IF NOT EXISTS snapshots (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id),
  service_id TEXT NOT NULL REFERENCES services(id),
  name TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  image_ref TEXT NOT NULL,
  env_encrypted TEXT NOT NULL DEFAULT '',
  db_dump_path TEXT NOT NULL DEFAULT '',
  resource_config TEXT NOT NULL DEFAULT '{}',
  port INTEGER NOT NULL DEFAULT 8000,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_snapshots_tenant ON snapshots(tenant_id);
CREATE INDEX IF NOT EXISTS idx_snapshots_service ON snapshots(service_id);
