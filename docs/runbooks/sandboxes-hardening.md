# Sandbox hardening: internet egress and hard disk limits

> **Status: both features are UNVERIFIED ON THE PRODUCTION HOST.** The code,
> unit tests and scripts are done, but nobody has yet run the
> [host verification checklist](#host-verification-checklist) on the real
> Hetzner host (gVisor netstack, DOCKER-USER, cgroup accounting). Do not
> enable either feature for real tenants until every checklist item passes,
> then remove this banner and the "unverified" notes in `docs-site/src/content/docs/api.md`
> and `.claude/skills/agentic-hosting/references/api-reference.md` in the same PR.

Disposable sandboxes ship with no internet access and a `/workspace` volume
with no size limit. Two operator opt-ins change that. Both are **off by
default**; with nothing configured, sandboxes behave exactly as before.

| Feature | Settings (`/etc/default/ah`) | What a tenant sees |
|---|---|---|
| Internet egress | `AH_SANDBOX_EGRESS_NETWORK=ah-sbx-egress` (or `--sandbox-egress-network`), optional `AH_SANDBOX_EGRESS_SUBNET`, the host firewall rules, and per tenant `sandbox_egress_allowed=1` | `egress: "internet"` creates work; otherwise 403 |
| Hard disk limits | `AH_SANDBOX_RUNTIME=runsc-sbx`, `AH_SANDBOX_WORKSPACE_TMPFS=true` | `disk_mb` on create; writes past it fail with ENOSPC; responses show `disk_mb` |

Commands below run on the production host as root (`sudo -i`) from the
repository checkout, unless they start with `ahc`, which runs anywhere with
`AH_URL` and `AH_KEY` set for a **test tenant**. Never connect to the retired
host `65.21.67.254`; verify the SSH host key of the current host as described
in `CLAUDE.md` before you connect.

---

## 1. Hard disk limits

### How it works

- `runsc-sbx` is a second Docker runtime: the same `runsc` binary started with
  `--overlay2=root:memory`. gVisor keeps the container's writable root
  filesystem layer in sandbox memory instead of on the host disk, so root
  filesystem writes are charged to the container's memory cgroup and bounded
  by `memory_mb`.
- With `AH_SANDBOX_WORKSPACE_TMPFS=true`, `/workspace` is a tmpfs of `disk_mb`
  (no Docker volume is created or removed). Writes beyond `disk_mb` fail with
  "No space left on device". The tmpfs is also sandbox memory, so `disk_mb`
  is bounded by `memory_mb`.
- `disk_mb` on create: 16 to min(`memory_mb`, tenant `max_sandbox_disk_mb`
  (default 1024, admin cap 8192)). Omitted: half of `memory_mb`. Without
  tmpfs workspaces `disk_mb` is rejected with 400 and reported as 0.
- Environments, services, databases and the warm pool are not affected.
  Sandboxes with these settings never use the warm pool.

### Enable

```bash
cd /path/to/agentic-hosting && git pull
sudo scripts/setup-linux.sh --sandbox-hardening --dry-run   # read the plan
sudo scripts/setup-linux.sh --sandbox-hardening --yes
```

`--sandbox-hardening` (existing install) merges this into
`/etc/docker/daemon.json` (backup `daemon.json.bak-<timestamp>`, other keys
untouched, validated with `dockerd --validate`):

```json
"runtimes": {
  "runsc":     {"path": "/usr/bin/runsc"},
  "runsc-sbx": {"path": "/usr/bin/runsc", "runtimeArgs": ["--overlay2=root:memory"]}
}
```

then runs `systemctl reload docker` (SIGHUP: running containers keep running),
checks `docker info` lists `runsc-sbx`, sets `AH_SANDBOX_RUNTIME=runsc-sbx` and
`AH_SANDBOX_WORKSPACE_TMPFS=true` in `/etc/default/ah`, rebuilds/reinstalls ah
as usual and restarts it.

Manual equivalent: edit `daemon.json` as above, `systemctl reload docker`,
`docker info --format '{{json .Runtimes}}'`, add the two lines to
`/etc/default/ah`, `systemctl restart ah`.

At startup ah refuses to start (`FATAL: AH_SANDBOX_RUNTIME=… is not registered
with Docker`) if the runtime is missing, and (outside `--dev`) if its binary
is not `runsc`. `AH_SANDBOX_RUNTIME=runc` requires `--dev`, like
`AH_CONTAINER_RUNTIME`. A registered runsc runtime without
`--overlay2=root:memory` only logs a warning.

Check the log:

```bash
journalctl -u ah -n 100 --no-pager | grep -E 'sandboxes:|AH_SANDBOX'
# want: sandboxes: egress disabled; runtime runsc-sbx; workspace tmpfs true
```

Optional per-tenant cap (admin token):

```bash
. /etc/default/ah
curl -s -X PATCH -H 'X-Forwarded-Proto: https' -H "X-Admin-Token: $AH_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' -d '{"max_sandbox_disk_mb":512}' \
  http://127.0.0.1:8080/v1/tenants/<tenant-id>/quotas
```

### Disable

Remove `AH_SANDBOX_RUNTIME` and `AH_SANDBOX_WORKSPACE_TMPFS` from
`/etc/default/ah` and `systemctl restart ah`. Existing tmpfs sandboxes keep
running until their TTL; new ones get a volume again. The `runsc-sbx` entry in
`daemon.json` is harmless and can stay.

---

## 2. Internet egress

### How it works

- ah creates one shared bridge network at startup (default name and interface
  `ah-sbx-egress`, subnet `AH_SANDBOX_EGRESS_SUBNET`, default `10.250.0.0/24`):
  not internal, inter-container traffic off, IPv6 off, label
  `ah.managed=sandbox-egress`. GC never removes it (it only touches
  `ah-tenant-*`, `ah-ingress-*` and `ah-dbnet-*`, and those prefixes are
  rejected as egress names). An existing network with that name but other
  settings is refused, not reused.
- An egress sandbox is attached **only** to this bridge, never to its tenant
  network, and never takes a warm-pool container.
- Host firewall rules (`docs/ops/sandbox-egress-firewall.sh`) limit the bridge
  to the public internet:
  - `DOCKER-USER -i ah-sbx-egress -j AH-SBX-EGRESS` at position 1. The chain
    returns ESTABLISHED,RELATED traffic, drops 10/8, 172.16/12, 192.168/16,
    100.64/10, 169.254/16 (Hetzner metadata), 127/8, 0/8, 224/4, 240/4 and
    each host public IPv4, and returns everything else to Docker's rules.
  - `INPUT -i ah-sbx-egress -j DROP` at position 1: no access to the ah API,
    sshd or anything else listening on the host.
  - `ip6tables` FORWARD and INPUT drops for the bridge (defence in depth).
- **IPv6** is disabled on the bridge (`EnableIPv6=false`), so sandboxes get no
  IPv6 address or route and the firewall only needs IPv4 rules. The ip6tables
  drops guard against the bridge ever being recreated with IPv6.
- **The egress gate.** `egress: "internet"` succeeds only if ah enabled the
  network at startup **and** the tenant has `sandbox_egress_allowed=1`.
  ah enables the network only if it can verify the firewall rules: ah runs as
  the unprivileged `ah` user, so a systemd drop-in, as root, reinstalls the
  rules and writes `/run/ah-sandbox-egress/firewall.rules` (`iptables -S`
  output) right before ah starts; ah checks that snapshot (root-owned, not
  group/world writable, at most 10 minutes old). Missing or out-of-order rules,
  a bad snapshot, a non-Linux host or a mismatched existing network log
  `WARNING: sandbox egress is DISABLED: <reason>` and every egress request gets
  403; the rest of ah runs normally.
  `AH_SANDBOX_EGRESS_SKIP_FIREWALL_CHECK=1` skips the check (only for hosts
  that filter the bridge some other way; it logs a loud warning).
- **DNS** is Docker's embedded resolver (127.0.0.11 inside the container
  network namespace), which forwards to the host's upstream resolvers from the
  host side, so the RFC 1918/loopback drops do not affect it. Whether it works
  under gVisor's netstack on this host is checklist item 4.
- **Metering.** Network bytes of egress sandboxes are recorded every 60s
  (first sample is the baseline) and appear in
  `GET /v1/tenant/usage/metrics?kind=sandbox` (`network_rx_bytes`,
  `network_tx_bytes`). Watch them for scanning or spam.
- **Limits.** Egress sandboxes cannot reach anything on this host, including
  services hosted here through their public URL (the DNAT to Traefik lands in
  172.16/12), managed databases, or other sandboxes.

### Enable

1. Pick the subnet. It must not overlap host routes, Docker networks or the
   Docker address pools (bootstrap installs use `172.20.0.0/14`):

   ```bash
   ip -4 route
   docker network ls -q | xargs docker network inspect --format '{{.Name}} {{range .IPAM.Config}}{{.Subnet}} {{end}}'
   grep -o '"default-address-pools".*' /etc/docker/daemon.json
   ```

   The default `10.250.0.0/24` (253 sandboxes; the default host cap is 20) is
   fine unless one of those shows `10.250.`.

2. Install the firewall script and look at what it would do:

   ```bash
   install -o root -g root -m 0755 docs/ops/sandbox-egress-firewall.sh /usr/local/sbin/ah-sandbox-egress-firewall
   ah-sandbox-egress-firewall install --bridge ah-sbx-egress --dry-run
   ah-sandbox-egress-firewall install --bridge ah-sbx-egress
   ah-sandbox-egress-firewall status --bridge ah-sbx-egress     # exit 0 = complete
   ```

   `install` auto-detects the host's global IPv4 addresses (or pass
   `--host-ip 178.104.44.25`), is idempotent, and moves its two rules back to
   position 1 if something else (ufw/firewalld reload, fail2ban) inserted
   rules ahead of them. If `netfilter-persistent` is installed it saves the
   ruleset; note that this also saves Docker's own rules. The drop-in below
   re-applies the rules on every ah start anyway, so persistence is optional.

3. Install the systemd drop-in and set the network:

   ```bash
   install -d -m 0755 /etc/systemd/system/ah.service.d
   install -o root -g root -m 0644 deploy/ah-sandbox-egress.conf /etc/systemd/system/ah.service.d/sandbox-egress.conf
   sed -i '/^AH_SANDBOX_EGRESS_NETWORK=/d;/^AH_SANDBOX_EGRESS_SUBNET=/d' /etc/default/ah
   printf 'AH_SANDBOX_EGRESS_NETWORK=ah-sbx-egress\nAH_SANDBOX_EGRESS_SUBNET=10.250.0.0/24\n' >> /etc/default/ah
   systemctl daemon-reload && systemctl restart ah
   journalctl -u ah -n 100 --no-pager | grep -E 'sandboxes:|sandbox egress'
   # want: sandboxes: egress enabled on ah-sbx-egress (10.250.0.0/24); host firewall rules verified (snapshot /run/ah-sandbox-egress/firewall.rules)
   ```

   If you change the network name, change `--bridge` in the drop-in too.

4. Allow a tenant (admin token). Start with a test tenant only:

   ```bash
   . /etc/default/ah
   curl -s -X PATCH -H 'X-Forwarded-Proto: https' -H "X-Admin-Token: $AH_ADMIN_TOKEN" \
     -H 'Content-Type: application/json' -d '{"sandbox_egress_allowed":1}' \
     http://127.0.0.1:8080/v1/tenants/<tenant-id>/quotas
   ```

### Disable

```bash
sed -i '/^AH_SANDBOX_EGRESS_NETWORK=/d;/^AH_SANDBOX_EGRESS_SUBNET=/d' /etc/default/ah
rm /etc/systemd/system/ah.service.d/sandbox-egress.conf
systemctl daemon-reload && systemctl restart ah
# after the last egress sandbox has expired:
docker network rm ah-sbx-egress
ah-sandbox-egress-firewall remove --bridge ah-sbx-egress
```

Setting `sandbox_egress_allowed` back to 0 stops new egress sandboxes for one
tenant; existing ones keep their access until they expire or are deleted.

---

## Host verification checklist

Run every item after enabling, and again after Docker, gVisor or kernel
upgrades. Record the date, versions (`docker version --format '{{.Server.Version}}'`,
`runsc --version | head -1`, `uname -r`) and results in the PR that removes
the "unverified" banner. Items marked **(must fail)** pass when the command
fails or times out.

Setup (on a workstation with `ahc`, test tenant allowed egress, and on the
host as root where noted):

```bash
ahc sandbox create egress-a  --image curlimages/curl:8.10.1 --egress internet --ttl 30m
ahc sandbox create egress-b  --image curlimages/curl:8.10.1 --egress internet --ttl 30m
ahc sandbox create no-egress --image curlimages/curl:8.10.1 --ttl 30m
ahc sandbox create disk      --image alpine:3.20 --memory 512 --disk 64 --ttl 30m
for s in egress-a egress-b no-egress disk; do ahc --json sandbox get $s | jq -r '"\(.name) \(.id) egress=\(.egress) disk_mb=\(.disk_mb)"'; done
```

`curlimages/curl` is Alpine based, so it has `nslookup`, `timeout` and `ip`.
Container names on the host are `ah-env-<tenant-id>-<sandbox-id>`.

**Startup and wiring**

1. ah log shows both features active:
   `journalctl -u ah -n 200 --no-pager | grep -E 'sandboxes:'` lists
   `egress enabled on ah-sbx-egress ... verified (snapshot ...)` and
   `runtime runsc-sbx; workspace tmpfs true`.
2. Egress bridge settings (host):
   `docker network inspect ah-sbx-egress --format '{{.Internal}} {{.EnableIPv6}} {{index .Options "com.docker.network.bridge.enable_icc"}} {{index .Options "com.docker.network.bridge.name"}} {{index .Labels "ah.managed"}}'`
   → `false false false ah-sbx-egress sandbox-egress`.
3. Firewall complete and first in line (host):
   `ah-sandbox-egress-firewall status --bridge ah-sbx-egress; echo rc=$?` → `rc=0`;
   `iptables -S DOCKER-USER | head -2` and `iptables -S INPUT | head -2` show the `ah-sbx-egress` rules first.
4. Container wiring (host):
   `for n in egress-a no-egress disk; do id=$(ahc --json sandbox get $n | jq -r .id); docker ps --filter "label=ah.environment=$id" --format '{{.Names}}' | xargs docker inspect --format '{{.Name}} runtime={{.HostConfig.Runtime}} net={{.HostConfig.NetworkMode}} mounts={{range .Mounts}}{{.Type}}:{{.Destination}} {{end}}'; done`
   → `egress-a` on `ah-sbx-egress` only, `no-egress` on `ah-tenant-<tenant>`, all `runtime=runsc-sbx`, `disk` has `tmpfs:/workspace` and no volume.

**Egress**

5. DNS works inside gVisor on the egress bridge:
   `ahc sandbox exec egress-a -- nslookup example.com` → an answer (exit 0).
6. Internet works with egress:
   `ahc sandbox exec egress-a -- curl -sS -m 10 -o /dev/null -w '%{http_code}\n' http://1.1.1.1` → `301` (or 200);
   `ahc sandbox exec egress-a -- curl -sS -m 10 -o /dev/null -w '%{http_code}\n' https://example.com` → `200`.
7. **(must fail)** Internet without egress:
   `ahc sandbox exec no-egress -- curl -sS -m 10 http://1.1.1.1` → exit non-zero;
   `ahc sandbox exec no-egress -- nslookup example.com` → no answer.
8. **(must fail)** Cloud metadata:
   `ahc sandbox exec egress-a -- curl -sS -m 5 http://169.254.169.254/hetzner/v1/metadata` → timeout (exit 28).
9. **(must fail)** RFC 1918 and CGNAT (try a gateway that exists on this host, e.g. docker0 and the egress gateway):
   `for ip in 172.17.0.1 10.250.0.1 10.0.0.1 192.168.0.1 100.64.0.1; do ahc sandbox exec egress-a -- curl -sS -m 5 -o /dev/null http://$ip/ && echo "REACHED $ip"; done` → no `REACHED`.
10. **(must fail)** The host: its API, SSH and public ports:
    `for t in 10.250.0.1:8080 10.250.0.1:22 178.104.44.25:22 178.104.44.25:443 178.104.44.25:8080; do ahc sandbox exec egress-a -- curl -sS -m 5 -o /dev/null telnet://$t && echo "REACHED $t"; done` → no `REACHED`.
11. **(must fail)** Other containers: pick a running service container of **another** tenant and a managed database (host):
    `docker inspect --format '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}' <container>`; then
    `ahc sandbox exec egress-a -- curl -sS -m 5 -o /dev/null http://<ip>:<port>` → timeout.
12. **(must fail)** Another egress sandbox:
    `IP_B=$(ahc sandbox exec egress-b -- sh -c "ip -4 -o addr show eth0 | awk '{print \$4}' | cut -d/ -f1")`;
    `ahc sandbox exec egress-a -- curl -sS -m 5 -o /dev/null http://$IP_B:80` → timeout or refused without reaching it (no listener: start one with `ahc sandbox exec egress-b -- sh -c 'nohup nc -lk -p 8000 >/dev/null 2>&1 &'` and use port 8000; must still time out).
13. **(must fail)** IPv6:
    `ahc sandbox exec egress-a -- sh -c 'ip -6 addr; ip -6 route'` → no global address or default route;
    `ahc sandbox exec egress-a -- curl -6 -sS -m 5 https://ipv6.google.com` → fails.
14. Gate (API): with a tenant whose `sandbox_egress_allowed` is 0,
    `ahc sandbox create x --egress internet` → 403 `not enabled for this tenant`.
15. Metering: after 3 minutes and some traffic from `egress-a`,
    `curl -s "$AH_URL/v1/tenant/usage/metrics?kind=sandbox" -H "Authorization: Bearer $AH_KEY" | jq '.[] | select(.network_tx_bytes > 0)'`
    shows the `egress-a` ID (adjust to the response shape).

**Disk limits**

16. tmpfs size and no volume:
    `ahc sandbox exec disk -- df -m /workspace` → `tmpfs` of ~64 MB;
    host: `docker volume ls --format '{{.Name}}' | grep "ah-env-$(ahc --json sandbox get disk | jq -r .id)"` → nothing.
17. **(must fail)** Writes beyond `disk_mb`:
    `ahc sandbox exec disk -- dd if=/dev/zero of=/workspace/fill bs=1M count=100` → `No space left on device` after ~64 MB.
18. Memory cgroup is charged for the tmpfs (host). Find the cgroup and read it before and after writing 48 MB:

    ```bash
    CID=$(docker ps --no-trunc -q --filter "label=ah.environment=$(ahc --json sandbox get disk | jq -r .id)")
    CG=$(find /sys/fs/cgroup -type d -name "*${CID}*" | head -1); echo "$CG"
    cat "$CG/memory.current"
    ahc sandbox exec disk -- sh -c 'rm -f /workspace/fill; dd if=/dev/zero of=/workspace/m bs=1M count=48'
    cat "$CG/memory.current"     # want: about 48 MB (50331648 bytes) higher
    ```
19. Root filesystem writes are memory-backed, not on the host disk (host):

    ```bash
    UPPER=$(docker inspect --format '{{.GraphDriver.Data.UpperDir}}' "$CID"); du -sm "$UPPER"
    ahc sandbox exec disk -- dd if=/dev/zero of=/root/big bs=1M count=100
    du -sm "$UPPER"              # want: unchanged (data is in sandbox memory)
    cat "$CG/memory.current"     # want: about 100 MB higher
    ```
20. **(must fail)** Root filesystem writes past `memory_mb`:
    `ahc sandbox exec disk -- dd if=/dev/zero of=/root/huge bs=1M count=700` → fails (ENOSPC/ENOMEM) or the sandbox is OOM-killed
    (`docker inspect --format '{{.State.OOMKilled}}' "$CID"` → `true`, sandbox status `stopped`); the host stays healthy and its free disk is unchanged.

Cleanup: `for s in egress-a egress-b no-egress disk; do ahc sandbox delete $s --confirm; done`.

If any **must fail** item succeeds, disable egress immediately (see Disable),
keep the banner, and file the finding. If item 5 (DNS) fails, egress cannot be
used as is: confirm with `ahc sandbox exec egress-a -- cat /etc/resolv.conf`
and record it; do not switch the resolver to a public IP without a review.
If item 18 or 19 shows no cgroup charge, keep `AH_SANDBOX_WORKSPACE_TMPFS`
off: the tmpfs would then be unaccounted host memory.
