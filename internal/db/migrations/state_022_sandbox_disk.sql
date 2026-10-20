-- Phase 2 sandbox hardening: a sandbox's /workspace can be a tmpfs of
-- disk_mb (0 = the named workspace volume, no hard size limit). The tenant
-- cap bounds disk_mb; it is also bounded by the sandbox's memory_mb because
-- the tmpfs is charged to the memory cgroup.
ALTER TABLE environments ADD COLUMN disk_mb INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tenant_quotas ADD COLUMN max_sandbox_disk_mb INTEGER NOT NULL DEFAULT 1024;
