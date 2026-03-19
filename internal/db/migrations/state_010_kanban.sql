CREATE TABLE IF NOT EXISTS kanban_instances (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'provisioning',
  container_id TEXT,
  port INTEGER,
  url TEXT,
  api_token_encrypted TEXT NOT NULL,
  jwt_secret_encrypted TEXT NOT NULL,
  volume_name TEXT,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_kanban_tenant ON kanban_instances(tenant_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_kanban_tenant_unique ON kanban_instances(tenant_id) WHERE status != 'failed';
CREATE UNIQUE INDEX IF NOT EXISTS idx_kanban_port ON kanban_instances(port) WHERE status NOT IN ('failed');
