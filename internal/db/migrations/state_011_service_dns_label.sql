ALTER TABLE services ADD COLUMN dns_label TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_services_tenant_dns_label
  ON services(tenant_id, dns_label) WHERE dns_label != '';
