# Mac mini demo: first run

This setup runs the same agentic-hosting application as the server install, inside a native ARM64 Ubuntu 24.04 Lima VM on Apple Silicon. Docker and gVisor stay inside the VM. Lima does not mount the Mac home directory into it, and only the guest's HTTP ingress is forwarded to host loopback at `127.0.0.1:18080`. This is a separate demo setup path; it does not change the Hetzner/server install.

For public URLs, keep the Mac powered on and awake, keep it connected to the network, keep the Lima VM running, and leave the ngrok agent running. If the Mac sleeps, loses its network connection, or the tunnel stops, public requests will fail. This guide does not configure automatic startup or change macOS power settings.

## Before you start

- An Apple Silicon Mac running macOS, with enough free disk space for VM and container images. The VM profile uses 4 CPUs, 8 GiB memory, and an 80 GiB virtual disk; Docker images and application state consume host disk space as they grow.
- Lima installed on the Mac. If you use Homebrew, install it with `brew install lima`; otherwise follow [Lima's installation guide](https://lima-vm.io/docs/installation/). The setup uses `limactl validate`, `limactl start`, and `limactl shell`; see the [Lima CLI reference](https://lima-vm.io/docs/).
- A committed checkout of this repository. The bootstrap wrapper copies the committed `HEAD` into the VM; uncommitted changes are not included.
- A domain you intend to use as the base domain, for example `demo.example.com`. Choose it before bootstrapping; the installer uses it to generate service URLs. You can configure ngrok later.
- When you are ready to expose the demo, the ngrok CLI installed and authenticated, plus an agent config with an exact endpoint for `api.<domain>` and a reserved wildcard `*.apps.<domain>`. Follow [ngrok's setup guide](https://ngrok.com/docs/getting-started/) to install and authenticate it. Wildcard domains must be reserved before ngrok can serve a wildcard endpoint. Confirm domain eligibility and any account charges in your ngrok account; this guide does not create domains or change billing. See [ngrok wildcard endpoints](https://ngrok.com/docs/gateway/endpoints/cloud-endpoints/http/#wildcard-endpoints), [custom domains](https://ngrok.com/docs/gateway/domains/custom-domains/), and [agent config v3](https://ngrok.com/docs/agent/config/v3/).

The ngrok account and tunnel do not need to be ready for the local VM health check. Pick the final domain first; configure ngrok after the application is healthy locally.

## Get the committed setup

The Mac mini setup is currently on the local `feat/macmini-ngrok-setup` branch and has not been published to the repository remote. Until it is published, use an existing checkout that contains `scripts/bootstrap-macmini.sh` and `deploy/lima-macmini.yaml`. After that branch or its replacement is published, clone the published ref:

```sh
git clone --branch feat/macmini-ngrok-setup \
  https://github.com/dennisonbertram/agentic-hosting.git agentic-hosting
cd agentic-hosting
```

Before creating the VM, make sure the commit you intend to install is checked out. Uncommitted changes are excluded because the wrapper stages `git archive HEAD`. Replace the branch name in the clone command if a different release branch or tag is published for the demo.

## Create and install the VM

From the repository root, create and start the named VM:

```sh
scripts/bootstrap-macmini.sh create
```

The wrapper validates the profile before creating the VM. The profile pins an official Ubuntu 24.04 ARM64 cloud image by SHA-256 and uses Apple's Virtualization.framework. It configures 4 CPUs, 8 GiB memory, and an 80 GiB virtual disk. Leave enough free host storage for Docker images and app data. The VM has no shared host mounts.

Install the application inside the fresh VM. Replace `demo.example.com` with the base domain you chose above; omit `*.`:

```sh
scripts/bootstrap-macmini.sh bootstrap --domain demo.example.com --dry-run
scripts/bootstrap-macmini.sh bootstrap --domain demo.example.com
```

The dry run checks the requested domain and VM state without staging source or running the installer. The install configures Docker's `runsc` runtime, API, constrained relay, and HTTP ingress for a trusted TLS tunnel. It sets `AH_EXTERNAL_TLS_TERMINATED=true`; the tunnel terminates public HTTPS while the app keeps generating HTTPS URLs. The optional website container is skipped. Forwarded protocol is constrained to the existing local relay.

### Check the local API before configuring ngrok

This check uses the intended API hostname in the HTTP `Host` header and the loopback-only forwarded port. It works before ngrok authentication, endpoints, or DNS are ready. Replace the domain with the same value used for bootstrap:

```sh
DOMAIN=demo.example.com
curl --fail --show-error --write-out '\nHTTP %{http_code}\n' \
  -H "Host: api.$DOMAIN" http://127.0.0.1:18080/v1/system/health
```

Replace the example domain with the same base domain used at bootstrap. Expect HTTP 200 and `{"status":"ok"}`. This checks local routing only; it does not test public DNS or TLS.

If ngrok is not ready yet, you can continue with either deployment example below. They call the API inside the VM; the printed HTTPS URLs become publicly reachable only after the ngrok endpoints and DNS are active.

If the wrapper says the VM already exists, inspect it with `scripts/bootstrap-macmini.sh status`; use `start` if it is stopped, not `create`. Bootstrap is deliberately fresh-VM only. If it exits after creating guest state, preserve the VM and do not rerun bootstrap or delete state to force a retry. Inspect service state and logs without printing the root-only environment file:

```sh
limactl shell ah-macmini sudo systemctl --no-pager --full status docker ah ah-api-proxy ah-build-broker
limactl shell ah-macmini sudo journalctl --no-pager -n 100 -u docker -u ah -u ah-api-proxy -u ah-build-broker
```

Keep the first error output and VM state for diagnosis. There is no generic safe rerun command after a partial bootstrap.

## Connect ngrok when ready

When your ngrok account and domain are ready, add the two endpoint entries from [`deploy/ngrok-macmini.example.yaml`](../../deploy/ngrok-macmini.example.yaml) to your existing authenticated v3 agent config. Replace `example.com` with your base domain. Keep the existing `agent` authentication settings and other endpoints; the example intentionally contains no credentials and must not replace your config file.

Both endpoints use the same local upstream:

- `https://api.demo.example.com` serves the platform API.
- `https://*.apps.demo.example.com` serves deployed tenant services.

The wildcard is scoped to `apps.<domain>` because the application generates service URLs as `<service>.apps.<domain>`. It covers one service-name label at that level. The API endpoint is configured separately as the exact `api.<domain>` hostname.

If your config is in ngrok's default location, check it and start only these named endpoints. Leave the ngrok process running while the demo is public:

```sh
ngrok config check
ngrok start ah-api ah-services
```

For a non-default config location, pass its path to both commands with `--config /path/to/ngrok.yml`. If ngrok reports that an endpoint domain is not reserved or not available, resolve that in your account before retrying; do not paste authentication tokens into the repository or shell history.

Check the public API route:

```sh
curl --fail --show-error --write-out '\nHTTP %{http_code}\n' \
  https://api.demo.example.com/v1/system/health
```

Replace the example hostname with `api.<your-domain>`. When this public HTTPS request returns HTTP 200 with `status=ok`, it verifies the API's public route through ngrok. The earlier loopback check is not proof of public TLS or DNS.

## Register a demo tenant and deploy a sample

Run this command from your Mac. It reads the bootstrap token from the guest's root-only environment file, registers a demo tenant, and deploys `traefik/whoami:v1.11.0` on port 80. The image works with the platform's read-only root filesystem and dropped capabilities. The script saves the API key in a root-only guest file, does not print it, and reuses it on reruns. It polls for up to six minutes, prints the service error if deployment fails, and prints the URL after the service reaches `running`:

```sh
DOMAIN=demo.example.com
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

Open the printed HTTPS URL in a browser after confirming the public API URL works. A successful request verifies the public service route through ngrok. During isolated VM testing, the service route returned HTTP 200 and reported `X-Forwarded-Proto: https` through host loopback; that local result does not verify public DNS or ngrok ingress. If the script times out or reports a deployment failure, rerunning it checks the saved service instead of creating a duplicate. If you intentionally delete the demo service outside this script, remove its saved ID marker before rerunning so the script can find or recreate the service:

```sh
limactl shell ah-macmini sudo rm -f /root/.ah-macmini-demo-service-id
```

## Optional: deploy from Git

This uses the API key saved by the preceding demo registration step and builds a pinned public sample commit. It creates a separate `git-demo` service on port 3333, starts a Git build, waits for the build and service to finish, then checks the local route. Each run starts a new build. The request config contains the key in a mode-600 temporary file; the key is not printed or passed on the curl command line.

```sh
limactl shell ah-macmini sudo bash -s <<'GUEST'
set -Eeuo pipefail
api=http://127.0.0.1:8080
key_file=/root/.ah-macmini-demo-api-key
[[ -s $key_file ]] || { echo 'Run the demo registration step first.' >&2; exit 1; }
auth=$(mktemp)
services=$(mktemp)
service=$(mktemp)
build=$(mktemp)
trap 'rm -f "$auth" "$services" "$service" "$build"' EXIT
chmod 600 "$auth" "$services" "$service" "$build"
api_key=$(< "$key_file")
printf 'header = "X-Forwarded-Proto: https"\nheader = "Authorization: Bearer %s"\n' "$api_key" > "$auth"
unset api_key
curl --fail --silent --show-error --config "$auth" "$api/v1/services" > "$services"
service_id=$(jq -r '[.[] | select(.name == "git-demo")][0].id // empty' "$services")
if [[ -z $service_id ]]; then
  curl --fail --silent --show-error --config "$auth" -H 'Content-Type: application/json' \
    -d '{"name":"git-demo","image":"traefik/whoami:v1.11.0","port":3333}' \
    "$api/v1/services" > "$service"
  service_id=$(jq -er '.id' "$service")
fi
curl --fail --silent --show-error --config "$auth" -H 'Content-Type: application/json' \
  -d '{"source_type":"git","source_url":"https://github.com/railwayapp-templates/expressjs.git","source_ref":"d3a671d7a13de103bed6ba7d19ef20a21a4b11f7"}' \
  "$api/v1/services/$service_id/builds" > "$build"
build_id=$(jq -er '.build_id' "$build")
build_status=
for attempt in {1..900}; do
  curl --fail --silent --show-error --config "$auth" \
    "$api/v1/services/$service_id/builds/$build_id" > "$build"
  build_status=$(jq -er '.status' "$build")
  case "$build_status" in
    succeeded) break ;;
    failed|cancelled) printf 'Git build %s ended as %s. Inspect its build logs before retrying.\n' "$build_id" "$build_status" >&2; exit 1 ;;
  esac
  sleep 2
done
[[ $build_status == succeeded ]] || { printf 'Git build %s timed out (last status: %s).\n' "$build_id" "$build_status" >&2; exit 1; }
service_status=
for attempt in {1..180}; do
  curl --fail --silent --show-error --config "$auth" \
    "$api/v1/services/$service_id" > "$service"
  service_status=$(jq -er '.status' "$service")
  case "$service_status" in
    running) break ;;
    failed) printf 'Service failed: %s\n' "$(jq -r '.last_error // "no detail available"' "$service")" >&2; exit 1 ;;
  esac
  sleep 2
done
[[ $service_status == running ]] || { printf 'Service did not reach running state (last status: %s).\n' "$service_status" >&2; exit 1; }
service_url=$(jq -er '.url' "$service")
printf 'build_id=%s\nservice_id=%s\nservice_status=%s\nurl=%s\n' \
  "$build_id" "$service_id" "$service_status" "$service_url"
GUEST
curl --fail --silent --show-error --write-out '\nHTTP %{http_code}\n' \
  -H "Host: git-demo.apps.$DOMAIN" http://127.0.0.1:18080/
```

Change `DOMAIN` to the same base domain used during bootstrap. The guest prints the service URL returned by the API; compare its hostname with `DOMAIN` before running the final curl. That curl runs on the Mac because Lima forwards host loopback into the guest; it checks the local route without using public DNS. Expect HTTP 200 and `{"body":"Hello world!"}`. After configuring ngrok, open the printed service URL in a browser; the local check does not verify public DNS or TLS.

## Operate the demo

```sh
scripts/bootstrap-macmini.sh status
scripts/bootstrap-macmini.sh stop
scripts/bootstrap-macmini.sh start
```

Stopping the VM leaves its data on disk. Starting it again restores the installed stack; restart the named ngrok endpoints separately. The setup does not change the Mac's Docker Desktop context, ngrok credentials, or ngrok account configuration.

## Troubleshooting

- If the local health request cannot connect, run `scripts/bootstrap-macmini.sh status`. If the VM is stopped, run `scripts/bootstrap-macmini.sh start`; if it is running, inspect the service status and journal commands above.
- If local health works but the public API does not, leave ngrok running and check the two endpoint names, upstream `127.0.0.1:18080`, exact API domain, wildcard reservation, and public DNS in your ngrok account. Do not use `curl -k` to treat an invalid public certificate as success.
- If tenant registration fails, read the error from the command and confirm the guest is healthy. The endpoint is rate-limited; do not repeatedly submit registration attempts or print the root-only `/etc/default/ah` file.
- If the service reports `failed`, the deploy command prints its `last_error`. Keep the VM and service record while diagnosing; do not bootstrap again to retry.

## Verification status

The ARM64 setup uses the official [Go 1.27.2 ARM64 archive](https://go.dev/dl/?mode=json) and the `aarch64-unknown-linux-musl` asset from the [Nixpacks v1.41.0 release](https://github.com/railwayapp/nixpacks/releases/tag/v1.41.0), each verified by SHA-256. The installer uses the exact Docker package versions in the [signed Ubuntu Noble ARM64 repository](https://download.docker.com/linux/ubuntu/dists/noble/stable/binary-arm64/Packages.gz) and `runsc=20261005.0` from the [gVisor ARM64 repository](https://storage.googleapis.com/gvisor/releases/dists/release/main/binary-arm64/Packages.gz). Container images are selected by pinned digest for each supported architecture. The ARM64 Traefik and Registry images were pulled during the VM test; the optional Nginx website container was skipped and its ARM64 image was not runtime-tested.

The profile and bootstrap were exercised on an Apple Silicon Mac running macOS 15.7.7 with Lima 2.2.1. The exact Lima profile validated and booted, and the full Ubuntu ARM64 bootstrap completed after fixing Docker apt source architecture rendering. A `runsc` container ran `hello-world`; the deployed `traefik/whoami:v1.11.0` container reported `linux/arm64` and runtime `runsc`. The API returned HTTP 200 with `status=ok` through host `127.0.0.1:18080`. The demo service returned HTTP 200 through the same loopback ingress and reported `X-Forwarded-Proto: https`. A guest canary listener on port 28177 was unreachable from the host, confirming that the Lima profile did not forward that listener.

After stopping and starting the VM, the API health request and demo service HTTP request passed again with the same results. The temporary QA VM was deleted after verification. `ngrok config check` passed with ngrok 3.39.11 against the credential-free example, without accessing an account or starting a tunnel. The integrated `go test ./... -timeout 180s` and `go build ./...` checks passed; script-level checks cover architecture selection, rendered Docker source entries, domain and VM-name validation, Lima instance-list parsing, and mocked wrapper commands.

One Git build using the pinned `railwayapp-templates/expressjs` commit above also passed on this ARM64 guest. The generated service image was `linux/arm64`, ran with `runsc`, reached `running`, and returned HTTP 200 with `{"body":"Hello world!"}` through the local Host-header route. This verifies one pinned sample repository, not arbitrary Git projects. Public ngrok DNS and TLS ingress have not been tested, and no ngrok account configuration or tunnel was changed.
