CREATE TABLE IF NOT EXISTS deployments (
  id TEXT PRIMARY KEY,
  service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
  tenant_id TEXT NOT NULL,
  build_id TEXT DEFAULT NULL,
  image TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  trigger TEXT NOT NULL DEFAULT 'manual',
  container_id TEXT DEFAULT '',
  error_message TEXT DEFAULT '',
  started_at INTEGER NOT NULL,
  completed_at INTEGER,
  created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_deployments_service ON deployments(service_id);
CREATE INDEX IF NOT EXISTS idx_deployments_tenant ON deployments(tenant_id);
CREATE INDEX IF NOT EXISTS idx_deployments_service_created ON deployments(service_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_deployments_status ON deployments(status);
