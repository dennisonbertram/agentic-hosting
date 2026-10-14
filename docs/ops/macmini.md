# Mac mini demo setup

This path runs the same agentic-hosting Go application in a native ARM64 Ubuntu 24.04 virtual machine on an Apple Silicon Mac mini. Docker and gVisor run inside that VM, so tenant workloads keep the existing `runsc` isolation. Lima does not mount the Mac home directory into the VM. The only forwarded listener is the VM's HTTP ingress on host loopback `127.0.0.1:18080`.

This is a demo host. The Mac mini must stay powered on and connected to the network, the Lima VM must be running, and the ngrok agent must remain connected for public URLs to work. This guide does not set up a paid ngrok account or create domains. Before using custom domains, the operator needs an ngrok endpoint for the exact `api.<domain>` name and a reserved wildcard `*.apps.<domain>` for service names. Ngrok requires wildcard domains to be reserved before a public wildcard endpoint can use them; account eligibility and any charges depend on the operator's ngrok plan. See [ngrok wildcard endpoints](https://ngrok.com/docs/gateway/endpoints/cloud-endpoints/http/#wildcard-endpoints) and [custom domains](https://ngrok.com/docs/gateway/domains/custom-domains/).

## Prerequisites

- An Apple Silicon Mac mini with macOS and enough free disk space for the Ubuntu VM and container images.
- Homebrew and Lima. Install Lima with `brew install lima` or follow [Lima's installation guide](https://lima-vm.io/docs/installation/).
- An ngrok account and CLI with authentication already configured using [ngrok's official setup instructions](https://ngrok.com/docs/getting-started/).
- A domain such as `demo.example.com`, with both `api.demo.example.com` and `*.apps.demo.example.com` already reserved/configured as ngrok endpoints. Do not enter a wildcard in the `--domain` argument.
- A committed checkout of this repository on the Mac mini. The wrapper copies committed source into the VM; uncommitted files are not copied.

## Create and install the VM

From the repository root, check the Lima profile before creating a VM:

```sh
limactl validate deploy/lima-macmini.yaml
```

Create and start a VM named `ah-macmini`:

```sh
scripts/bootstrap-macmini.sh create
```

The profile pins an official Ubuntu 24.04 ARM64 cloud image by SHA-256. Lima uses Apple's Virtualization.framework and a native ARM64 guest. To use another VM name, pass `--name ah-demo` to each wrapper command.

Install the application and stack inside the fresh VM:

```sh
scripts/bootstrap-macmini.sh bootstrap --domain demo.example.com --dry-run
scripts/bootstrap-macmini.sh bootstrap --domain demo.example.com
```

The installer configures Docker's `runsc` runtime, installs the API and constrained relay, and configures Traefik to receive HTTP from the local tunnel. It sets `AH_EXTERNAL_TLS_TERMINATED=true`; ngrok handles public HTTPS while the app keeps generating HTTPS URLs. The optional website container is skipped. The API still trusts forwarded protocol only after traffic reaches it through the existing local relay.

The installer is intentionally fresh-VM only. If it stops after creating state, keep the VM and its disks. Do not run bootstrap again or delete its state to force a rerun. Use `scripts/bootstrap-macmini.sh status` and inspect the relevant service logs with `limactl shell ah-macmini sudo journalctl -u ah -u ah-api-proxy -u ah-build-broker --no-pager -n 100`; resolve the first failure before taking recovery steps.

## Configure and start ngrok

Copy the two endpoint entries from [`deploy/ngrok-macmini.example.yaml`](../../deploy/ngrok-macmini.example.yaml) into the existing authenticated ngrok agent config. Replace `example.com` with the parent domain you configured. Keep any existing authentication settings and other endpoints; the example intentionally contains no credentials and is not meant to replace your config file.

Both endpoints use the same local upstream:

- `https://api.demo.example.com` serves the platform API.
- `https://*.apps.demo.example.com` serves deployed tenant services.

The wildcard is scoped to `apps.<domain>` because the application generates service URLs under `AH_BASE_DOMAIN=apps.<domain>`. The exact API endpoint is separate so the setup does not depend on wildcard matching of nested hostnames. Ngrok routes to the most specific matching online endpoint.

Start only these named endpoints in a terminal and leave it open:

```sh
ngrok start ah-api ah-services
```

Check the public API route:

```sh
curl --fail --show-error https://api.demo.example.com/v1/system/health
```

It should return the API health response. This command has not been run against a real Mac mini or ngrok account as part of preparing this guide.

## Register a demo tenant and deploy a sample

The following guest command registers a tenant using the bootstrap token already stored in the VM's root-only environment file, then deploys `traefik/whoami:v1.11.0`. The image serves HTTP on port 80 and works with the platform's read-only root filesystem and dropped capabilities. It stores the returned API key in a root-only file for this demo, reuses it on reruns, and never prints it. The polling loop waits up to six minutes, reports deployment failures, and prints the public URL only after the service reports `running`:

```sh
limactl shell ah-macmini sudo bash -s <<'GUEST'
set -Eeuo pipefail
set -a
. /etc/default/ah
set +a
registration=$(mktemp)
service=$(mktemp)
service_list=$(mktemp)
register_config=$(mktemp)
auth_config=$(mktemp)
api_key_tmp=$(mktemp)
service_id_tmp=$(mktemp)
trap 'rm -f "$registration" "$service" "$service_list" "$register_config" "$auth_config" "$api_key_tmp" "$service_id_tmp"' EXIT
chmod 600 "$register_config" "$auth_config" "$api_key_tmp" "$service_id_tmp"
api_key_file=/root/.ah-macmini-demo-api-key
service_id_file=/root/.ah-macmini-demo-service-id
if [[ ! -s $api_key_file ]]; then
  printf 'header = "X-Forwarded-Proto: https"\nheader = "X-Bootstrap-Token: %s"\n' "$AH_BOOTSTRAP_TOKEN" > "$register_config"
  curl --fail --silent --show-error --config "$register_config" \
    -H 'Content-Type: application/json' \
    -d '{"name":"mac-mini-demo","email":"demo@example.com"}' \
    http://127.0.0.1:8080/v1/tenants/register > "$registration"
  jq -er '.api_key' "$registration" > "$api_key_tmp"
  chmod 600 "$api_key_tmp"
  mv "$api_key_tmp" "$api_key_file"
fi
api_key=$(< "$api_key_file")
printf 'header = "X-Forwarded-Proto: https"\nheader = "Authorization: Bearer %s"\n' "$api_key" > "$auth_config"
unset api_key
if [[ -s $service_id_file ]]; then
  service_id=$(< "$service_id_file")
else
  curl --fail --silent --show-error --config "$auth_config" \
    http://127.0.0.1:8080/v1/services > "$service_list"
  service_id=$(jq -r '[.[] | select(.name == "demo-web")][0].id // empty' "$service_list")
fi
if [[ -z ${service_id:-} ]]; then
  curl --fail --silent --show-error --config "$auth_config" \
    -H 'Content-Type: application/json' \
    -d '{"name":"demo-web","image":"traefik/whoami:v1.11.0","port":80}' \
    http://127.0.0.1:8080/v1/services > "$service"
  service_id=$(jq -er '.id' "$service")
  printf '%s\n' "$service_id" > "$service_id_tmp"
  chmod 600 "$service_id_tmp"
  mv "$service_id_tmp" "$service_id_file"
fi
for attempt in {1..180}; do
  curl --fail --silent --show-error --config "$auth_config" \
    "http://127.0.0.1:8080/v1/services/$service_id" > "$service"
  status=$(jq -er '.status' "$service")
  case "$status" in
    running)
      jq -r '"service_id=\(.id)\nstatus=\(.status)\nurl=\(.url)"' "$service"
      exit 0
      ;;
    failed)
      printf 'Service deployment failed. API detail: %s\n' "$(jq -r '.last_error // "no detail available"' "$service")" >&2
      exit 1
      ;;
  esac
  sleep 2
done
printf 'Service did not reach running state within six minutes (last status: %s). Inspect the service through the API before retrying.\n' "$status" >&2
exit 1
GUEST
```

Open the returned HTTPS URL in a browser. A browser request to the service exercises both ngrok and the service route. Do not treat successful unit tests or a successful VM build as proof of this public path; the public request remains to be run on the demo Mac mini.

## Operate the demo

```sh
scripts/bootstrap-macmini.sh status
scripts/bootstrap-macmini.sh stop
scripts/bootstrap-macmini.sh start
```

Stopping the VM leaves its data on disk. Starting it again restores the installed stack. Restart the named ngrok endpoints separately after starting the VM. `create` refuses to replace an existing Lima instance; use `start` to resume an existing VM. The setup does not modify the Mac's Docker Desktop context, ngrok credentials, or ngrok account configuration.

## What has been verified

The ARM64 setup uses the official [Go 1.27.2 ARM64 archive](https://go.dev/dl/?mode=json) and the `aarch64-unknown-linux-musl` asset from the [Nixpacks v1.41.0 release](https://github.com/railwayapp/nixpacks/releases/tag/v1.41.0), each verified by SHA-256. The installer uses the exact Docker package versions in the [signed Ubuntu Noble ARM64 repository](https://download.docker.com/linux/ubuntu/dists/noble/stable/binary-arm64/Packages.gz) and `runsc=20261005.0` from the [gVisor ARM64 repository](https://storage.googleapis.com/gvisor/releases/dists/release/main/binary-arm64/Packages.gz). The Traefik, Registry, and Nginx pins are multi-architecture image index digests; the indexes were checked for both amd64 and ARM64 manifests in the official Docker Hub registry.

Script-level checks cover architecture selection, domain and VM-name validation, Lima instance-list parsing, and mocked create/start/stop/status/bootstrap commands. The Lima profile still needs validation with Lima on the target Mac mini, and the full VM bootstrap, ngrok connection, public API health request, and sample service browser request remain unverified until an operator runs them.
