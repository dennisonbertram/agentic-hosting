-- Add max_builds_concurrent and max_env_vars_per_service columns to tenant_quotas.
-- These support the operator quota update endpoint (issue #136).
ALTER TABLE tenant_quotas ADD COLUMN max_builds_concurrent INTEGER NOT NULL DEFAULT 3;
ALTER TABLE tenant_quotas ADD COLUMN max_env_vars_per_service INTEGER NOT NULL DEFAULT 50;
