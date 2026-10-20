# Sandbox hardening: internet egress and hard disk limits

> **Status: both features are UNVERIFIED ON THE PRODUCTION HOST.** The code,
> unit tests and scripts are done, but nobody has yet run the
> [host verification checklist](#host-verification-checklist) on the real
> Hetzner host (gVisor netstack, DOCKER-USER, cgroup accounting). Do not
> enable egress or `runsc-sbx` for real tenants until every checklist item
> passes. The tmpfs workspace is now on by default (an unbounded volume let
> one sandbox fill the host disk); run checklist items 16 to 18 after the
> upgrade, and set `AH_SANDBOX_WORKSPACE_TMPFS=false` if item 18 fails. Once
> everything passes, remove this banner and the "unverified" notes in `docs-site/src/content/docs/api.md`
> and `.claude/skills/agentic-hosting/references/api-reference.md` in the same PR.

Disposable sandboxes ship with no internet access and a tmpfs `/workspace`
bounded by `disk_mb`. Internet egress and the memory-backed sandbox runtime
are operator opt-ins, **off by default**. The tmpfs workspace is **on by
default** (`AH_SANDBOX_WORKSPACE_TMPFS=true`); an operator can set it to
`false` to go back to a `/workspace` volume with no size limit.

| Feature | Settings (`/etc/default/ah`) | What a tenant sees |
|---|---|---|
| Internet egress | `AH_SANDBOX_EGRESS_NETWORK=ah-sbx-egress` (or `--sandbox-egress-network`), optional `AH_SANDBOX_EGRESS_SUBNET`, the host firewall rules, and per tenant `sandbox_egress_allowed=1` | `egress: "internet"` creates work; otherwise 403 |
| Hard disk limits | `AH_SANDBOX_WORKSPACE_TMPFS` (default `true`), plus `AH_SANDBOX_RUNTIME=runsc-sbx` for the root filesystem | `disk_mb` on create; writes past it fail with ENOSPC; responses show `disk_mb` |
| Image pull limits | `AH_SANDBOX_MAX_CONCURRENT_PULLS` (default 2), `AH_SANDBOX_MAX_IMAGE_MB` (default 4096) | creates wait for a pull slot; larger `image` sources get 400 `image too large` |

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
- With `AH_SANDBOX_WORKSPACE_TMPFS=true` (the default), `/workspace` is a
  tmpfs of `disk_mb` (no Docker volume is created or removed). Writes beyond `disk_mb` fail with
  "No space left on device". The tmpfs is also sandbox memory, so `disk_mb`
  is bounded by `memory_mb`.
- `disk_mb` on create: 16 to min(`memory_mb`, tenant `max_sandbox_disk_mb`
  (default 1024, admin cap 8192)). Omitted: half of `memory_mb`. Without
  tmpfs workspaces `disk_mb` is rejected with 400 and reported as 0.
- Environments, services, databases and the warm pool are not affected.
  Sandboxes with these settings never use the warm pool.
- Without `runsc-sbx`, the container's writable root filesystem is still on
  the host disk with no hard limit. The disk watchdog below is the backstop.

### Disk watchdog and image pulls

These are always on; sandboxes are disposable.

- **Disk watchdog.** On every pass (every 30s) the sandbox reaper runs the
  sandbox disk check on the data dir (`AH_DATA_DIR`) and the Docker root
  (`AH_DOCKER_DATA_DIR`). While either is at the 90% block threshold (the
  same threshold that refuses creates, deploys and builds) it deletes running
  sandboxes through the normal teardown, largest `disk_mb` first (oldest
  first among equals), re-checking after each, until usage is below 90% or
  none are left. Each deletion logs
  `AUDIT: action=sandbox.disk_pressure_deleted tenant=… sandbox=… disk_mb=…`.
  Environments, services and databases are never touched.
- **Image pulls.** Every sandbox image pull waits for one of
  `AH_SANDBOX_MAX_CONCURRENT_PULLS` host-wide slots (default 2). After pulling
  an `image` source, ah reads the image size; over `AH_SANDBOX_MAX_IMAGE_MB`
  (default 4096) the create fails with 400 `image too large` and the image is
  removed if that create pulled it. The disk check runs again after every
  pull; a pull that took the disk to 90% fails the create with 503.
- **Template images** are used from the local image store without contacting
  the registry (they are pulled only when missing), so template sandboxes
  keep working when Docker Hub rate-limits or is down. Pre-pull them, and
  pin them, after changing a template. `image` sources are always pulled:
  that pull is what stops a tenant from running an image that exists only on
  this host.

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

`runsc-sbx` gets the same path and `runtimeArgs` as `runsc` (for example
`--platform=kvm`), with any `--overlay2` value replaced by
`--overlay2=root:memory`. An existing `runsc-sbx` entry that differs is left
unchanged with a warning; fix it by hand if needed.

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

Optional per-tenant cap (admin token). The header is read from a process
substitution so the token never appears in `curl`'s argv (visible in `ps`):

```bash
. /etc/default/ah
curl -s -X PATCH -H 'X-Forwarded-Proto: https' -H @<(printf 'X-Admin-Token: %s\n' "$AH_ADMIN_TOKEN") \
  -H 'Content-Type: application/json' -d '{"max_sandbox_disk_mb":512}' \
  http://127.0.0.1:8080/v1/tenants/<tenant-id>/quotas
```

### Disable

Remove `AH_SANDBOX_RUNTIME` from `/etc/default/ah` and `systemctl restart ah`
to go back to the container runtime. To give sandboxes a `/workspace` volume
with no size limit again, set `AH_SANDBOX_WORKSPACE_TMPFS=false` (removing the
line keeps the tmpfs default). Existing tmpfs sandboxes keep running until
their TTL. The `runsc-sbx` entry in `daemon.json` is harmless and can stay.

### Capacity planning

Keep `max_sandboxes` times the number of tenants allowed sandboxes at or
below `AH_MAX_SANDBOXES_TOTAL` (default 20; per-tenant default
`max_sandboxes` 5). The host cap is shared first come, first served: with the
defaults, four tenants can hold every slot for up to their maximum lifetime
(`max_sandbox_lifetime_seconds`, default 24h) and every other tenant gets
`host sandbox capacity reached; try again later`. Lower `max_sandboxes` per
tenant (admin `PATCH /v1/tenants/<id>/quotas`) or raise the host cap (if the
host has the memory and disk for it) when you add tenants.

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
  - These rules only see forwarded traffic while Docker manages iptables with
    its iptables firewall backend, which puts `FORWARD -j DOCKER-USER` first.
    With `"firewall-backend": "nftables"` or `"iptables": false` in
    `daemon.json` nothing jumps to `DOCKER-USER`; ah then finds no
    `FORWARD -j DOCKER-USER` rule and keeps egress disabled.
  - Every reinstall (each ah start) first puts temporary
    `ah-sbx-egress-guard` drops at position 1, so running egress sandboxes
    are cut off, not unfiltered, while the chain is rebuilt; established
    connections may see a brief stall. If install fails part-way the guards
    stay and the bridge has no network at all until install succeeds.
- **Egress sandboxes never restart on their own.** Their containers have
  Docker restart policy `no`: after a host reboot or a dockerd restart they
  stay stopped (so none runs before the rules are reinstalled), the
  reconciler marks them stopped and the reaper deletes them after the grace
  period. Tenants create new ones.
- **Startup with egress disabled deletes egress sandboxes.** If ah starts with
  egress disabled (not configured, or the rules could not be verified), it
  deletes every existing sandbox with egress `internet` before serving
  requests (`AUDIT: action=sandbox.egress_disabled_delete`).
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
  network namespace). It forwards from the host namespace only when the
  upstream is a loopback address (the systemd-resolved stub); otherwise it
  forwards from the container's namespace, through the bridge and
  `DOCKER-USER`, so `AH-SBX-EGRESS` sees the queries. The host's upstream
  resolvers must therefore be public addresses (Hetzner's are). On a host
  whose resolvers are in 10/8, 172.16/12, 100.64/10 or 169.254/16 (common in
  cloud VPCs) egress sandboxes get no DNS unless an explicit allow rule for
  those resolvers is placed before the drops (needs a review). Whether DNS
  works under gVisor's netstack on this host is checklist item 5.
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
   re-applies the rules on every ah start. Persistence is not what keeps a
   reboot safe: egress sandbox containers have restart policy `no`, so after
   a reboot none runs until ah (and its drop-in) has started and a tenant
   creates a new one.

   > **ah checks the egress firewall only at startup.** It does not notice
   > rules that disappear while it runs. After **any** firewall reload or
   > change, anything that flushes or reorders `INPUT`, `FORWARD` or
   > `DOCKER-USER` (ufw/firewalld reload, `iptables-restore`,
   > `netfilter-persistent reload`, fail2ban restart, a Docker restart with
   > firewalld), run `systemctl restart ah`. That reinstalls the rules and
   > re-checks them; until then running egress sandboxes may be unfiltered.

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
   curl -s -X PATCH -H 'X-Forwarded-Proto: https' -H @<(printf 'X-Admin-Token: %s\n' "$AH_ADMIN_TOKEN") \
     -H 'Content-Type: application/json' -d '{"sandbox_egress_allowed":1}' \
     http://127.0.0.1:8080/v1/tenants/<tenant-id>/quotas
   ```

### Disable

```bash
sed -i '/^AH_SANDBOX_EGRESS_NETWORK=/d;/^AH_SANDBOX_EGRESS_SUBNET=/d' /etc/default/ah
systemctl restart ah     # starts with egress disabled and deletes every egress sandbox
journalctl -u ah -n 100 --no-pager | grep -E 'egress_disabled_delete|egress sandbox'
# only once no egress sandbox is left (the rules must outlive the last one):
docker ps -q --filter network=ah-sbx-egress     # want: nothing
rm /etc/systemd/system/ah.service.d/sandbox-egress.conf
systemctl daemon-reload
docker network rm ah-sbx-egress
ah-sandbox-egress-firewall remove --bridge ah-sbx-egress
```

If the restart logged `not every egress sandbox could be deleted`, wait for
the reaper (it retries every 30s) and repeat the `docker ps` check before
removing the rules.

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
   `iptables -S FORWARD | head -2` shows `-A FORWARD -j DOCKER-USER` first;
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
If item 18 or 19 shows no cgroup charge, set `AH_SANDBOX_WORKSPACE_TMPFS=false`:
the tmpfs would then be unaccounted host memory.
