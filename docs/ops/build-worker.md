# Production build worker

Production Git builds are coordinated by the `ah` API process and run in a
separate `ah-builder` systemd service. The API writes a bounded request and
log file under `/var/lib/ah-builds/<build-id>`, then asks the root-owned
`ah-build-broker.service` over `/run/ah-build-broker/broker.sock` to start,
stop, or inspect the fixed `ah-build@<build-id>.service` unit. The broker accepts
only generated 32-character lowercase hexadecimal build IDs and these three
operations; it does not accept commands, unit properties, paths, or environment
values from the API.

The build root must be owned by `ah:ah-build` with mode `0710`. The API account
must belong to `ah-build`; `ah-builder` must belong to `ah-build` and `docker`.
The API creates each build directory as `ah:ah-build` with mode `02770`, and
creates the request file and log with modes `0640` and `0660`. Keep this root
separate from `/var/lib/ah`, which contains daemon secrets. The broker runtime
directory is root:`ah` mode `0750`, and the socket is root:`ah` mode `0660`.

Install `ah-build-broker.service` and `ah-build@.service`, enable only the
broker service, and make `ah.service` start after and want the broker. The
worker template is started per build by the broker and should not be enabled as
a standalone unit. It uses `AH_BUILD_DIR=/var/lib/ah-builds` and
`AH_NIXPACKS_PATH=/usr/local/bin/nixpacks`; the API's `AH_BUILD_DIR` must match.
If the build root or broker socket is customized, update the API and worker
configuration, the worker's `ReadWritePaths`, and the broker's fixed socket
path together.

The worker unit applies a 2 GiB memory limit, 200% CPU quota, and 20-minute
runtime limit to the `ah-builder` service cgroup. Git sources must use HTTPS
without credentials and one of the configured Git hosts (`github.com`,
`gitlab.com`, `bitbucket.org`, `sr.ht`, or `codeberg.org`). The worker streams
sanitized build output into the shared log; the API streams that file to the
existing build log callback. Neither the broker nor worker receives the API's
secret-bearing environment file.

The worker needs access to `/run/docker.sock` to push images. Membership in the
Docker group grants broad control of the host Docker daemon and is effectively
root-equivalent on typical installations. The cgroup limits apply to the
`ah-builder` service process tree; containers created by the host Docker daemon
may run in Docker-managed cgroups outside those limits. This setup preserves
the existing Docker-based build path and does not provide daemonless build
isolation.

Local Go tests exercise validation, Unix-socket protocol framing, bounded
cancellation, artifact permissions, and the broker's fixed command mapping.
They do not prove that a fresh Linux host has the expected systemd, Docker,
group, or filesystem configuration. Verify a real Git build and cancellation
on the provisioned Hetzner host before treating production builds as restored.
