ALTER TABLE services ADD COLUMN dns_label TEXT NOT NULL DEFAULT '';
DROP INDEX IF EXISTS idx_services_tenant_dns_label;
CREATE UNIQUE INDEX IF NOT EXISTS idx_services_dns_label
  ON services(dns_label) WHERE dns_label != '';
