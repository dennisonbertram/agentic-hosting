-- Add last_error column to services for deploy error surfacing.
ALTER TABLE services ADD COLUMN last_error TEXT NOT NULL DEFAULT '';
