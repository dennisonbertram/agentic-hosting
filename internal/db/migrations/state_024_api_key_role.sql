-- Minimal key roles: owner can revoke keys and suspend the tenant; member cannot.
-- Existing keys stay owner so current tenants do not lose admin.
ALTER TABLE api_keys ADD COLUMN role TEXT NOT NULL DEFAULT 'owner';
