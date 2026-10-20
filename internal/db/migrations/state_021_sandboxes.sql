-- Disposable sandboxes are rows in environments with kind='sandbox'. Existing
-- rows default to kind='environment' so environment behaviour is unchanged.
ALTER TABLE environments ADD COLUMN kind TEXT NOT NULL DEFAULT 'environment';
ALTER TABLE environments ADD COLUMN source_type TEXT NOT NULL DEFAULT 'template';
ALTER TABLE environments ADD COLUMN source_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE environments ADD COLUMN image_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE environments ADD COLUMN run_mode TEXT NOT NULL DEFAULT 'idle';
ALTER TABLE environments ADD COLUMN memory_mb INTEGER NOT NULL DEFAULT 0;
ALTER TABLE environments ADD COLUMN cpu_millicores INTEGER NOT NULL DEFAULT 0;
ALTER TABLE environments ADD COLUMN egress TEXT NOT NULL DEFAULT 'none';
ALTER TABLE environments ADD COLUMN max_expires_at INTEGER;
CREATE INDEX IF NOT EXISTS idx_environments_kind_status ON environments(kind, status, lease_expires_at);

ALTER TABLE tenant_quotas ADD COLUMN max_sandboxes INTEGER NOT NULL DEFAULT 5;
ALTER TABLE tenant_quotas ADD COLUMN max_sandbox_ttl_seconds INTEGER NOT NULL DEFAULT 14400;
ALTER TABLE tenant_quotas ADD COLUMN max_sandbox_lifetime_seconds INTEGER NOT NULL DEFAULT 86400;
ALTER TABLE tenant_quotas ADD COLUMN sandbox_egress_allowed INTEGER NOT NULL DEFAULT 0;
