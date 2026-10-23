# Service subdomains are server-wide

## Decision

Keep host-wide unique `dns_label` values and document that. Do not namespace hostnames per tenant.

Public URLs are `{dns_label}.{base-domain}` (for example `web.apps.example.com`). The unique index is on `services.dns_label` (`state_011` dropped the old per-tenant index). A second tenant that wants `web` gets **409**.

The less breaking route:

- `{label}.{tenant}.{base}` needs a two-level wildcard (`*.*.apps.example.com`). Install and docs use `*.apps.example.com`.
- Prefixing `{tenant}-{label}` would change every existing public URL.

IDs, containers, networks and images stay tenant-scoped. Only the public hostname is first-come across the host. Agents should prefix service names with a project or tenant slug if they need a private namespace.

The 409 text now says labels are unique across the whole server so agents do not retry the same name.

## Not changed

- URL scheme
- Reserved labels
- Localhost mode (`http://{serviceID}.localhost`, no label claimed)
