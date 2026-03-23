CREATE TABLE IF NOT EXISTS warm_pool (
    id TEXT PRIMARY KEY,
    template_id TEXT NOT NULL,
    container_id TEXT NOT NULL,
    volume_name TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'ready',
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_warm_pool_template_status ON warm_pool(template_id, status);
