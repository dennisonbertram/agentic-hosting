-- Distinguish sandbox usage samples from service samples. Existing rows are
-- service usage.
ALTER TABLE usage_events ADD COLUMN resource_kind TEXT NOT NULL DEFAULT 'service';
ALTER TABLE usage_daily ADD COLUMN resource_kind TEXT NOT NULL DEFAULT 'service';
