INSERT OR IGNORE INTO environment_templates (id, name, base_image, description, tools, memory_mb, cpu_millicores, disk_mb, egress_policy, created_at, updated_at)
VALUES
  ('tmpl_node', 'node', 'node:22-bookworm', 'Node.js 22 workspace', '["npm","yarn","npx"]', 1024, 1000, 2048, 'allow', strftime('%s','now'), strftime('%s','now')),
  ('tmpl_python', 'python', 'python:3.12-bookworm', 'Python 3.12 workspace', '["pip","pipenv","python3"]', 1024, 1000, 2048, 'allow', strftime('%s','now'), strftime('%s','now')),
  ('tmpl_go', 'go', 'golang:1.23-bookworm', 'Go 1.23 workspace', '["go"]', 1024, 2000, 2048, 'allow', strftime('%s','now'), strftime('%s','now'));
