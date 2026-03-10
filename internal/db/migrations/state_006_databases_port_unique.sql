CREATE UNIQUE INDEX IF NOT EXISTS idx_databases_port_unique ON databases(port) WHERE status NOT IN ('failed');
