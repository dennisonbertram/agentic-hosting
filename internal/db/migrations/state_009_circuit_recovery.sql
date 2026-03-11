ALTER TABLE services ADD COLUMN circuit_retry_at INTEGER;
ALTER TABLE services ADD COLUMN circuit_open_count INTEGER NOT NULL DEFAULT 0;
