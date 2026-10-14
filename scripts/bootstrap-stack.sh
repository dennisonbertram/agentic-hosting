#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

# Verified upstream artifacts; review this table when refreshing the installer.
readonly DOCKER_KEY_FINGERPRINT=9DC858229FC7DD38854AE2D88D81803C0EBFCD88
readonly GO_VERSION=1.27.2
readonly GO_SHA256_AMD64=ecbadb99091a3f46e31f5f934b068b1864eafa7995211b39eaddf76996045fe5
readonly GO_SHA256_ARM64=94f3e30b8e374bc285e7dadc11e0865726b9bc6e85b841ccceaabc0214c6b7c8
readonly NIXPACKS_VERSION=1.41.0
readonly NIXPACKS_SHA256_AMD64=0f55de7874507b9cf7502113120bd96f2ab6979f78d10eaf2eb2ade9207b3af6
readonly NIXPACKS_SHA256_ARM64=912bd02dd2bb6f9c3a9ed965fe8a68b4aa318dc7a2546e2eca6f2806a894ba39
readonly TRAEFIK_DIGEST_AMD64=sha256:5902d68e00708015736416b62faa49d23a2e1d4415649e5831d20b79ba301508
readonly TRAEFIK_DIGEST_ARM64=sha256:575fa15b135078fe5e50aa847987d96dbddd7b093c172429618404df73f3fa7c
readonly REGISTRY_DIGEST_AMD64=sha256:46faa9a1ae6813194b53921a370f2f4f8c5e1aae228a89bceafef5847a6a3278
readonly REGISTRY_DIGEST_ARM64=sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373
readonly NGINX_DIGEST_AMD64=sha256:a60ab79b8d1cbc6c0860ca9829908c5e7e83ed887034e778fc7adf0b1bfe5e47
readonly NGINX_DIGEST_ARM64=sha256:4870c12cd2ca986de501a804b4f506ad3875a0b1874940ba0a2c7f763f1855b2
readonly INGRESS_SUBNET=172.24.0.0/24 INGRESS_GATEWAY=172.24.0.1 TRAEFIK_IP=172.24.0.2 REGISTRY_IP=172.24.0.3 WEBSITE_IP=172.24.0.4
domain= email= skip_website=0 dry_run=0 self_test=0 external_tls_terminated=0
repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

select_architecture() {
  case "$1" in
    amd64)
      go_arch=amd64 nixpacks_arch=x86_64-unknown-linux-musl
      go_sha256=$GO_SHA256_AMD64 nixpacks_sha256=$NIXPACKS_SHA256_AMD64
      traefik_digest=$TRAEFIK_DIGEST_AMD64 registry_digest=$REGISTRY_DIGEST_AMD64 nginx_digest=$NGINX_DIGEST_AMD64
      ;;
    arm64)
      go_arch=arm64 nixpacks_arch=aarch64-unknown-linux-musl
      go_sha256=$GO_SHA256_ARM64 nixpacks_sha256=$NIXPACKS_SHA256_ARM64
      traefik_digest=$TRAEFIK_DIGEST_ARM64 registry_digest=$REGISTRY_DIGEST_ARM64 nginx_digest=$NGINX_DIGEST_ARM64
      ;;
    *) die "unsupported Ubuntu architecture: $1 (supported: amd64, arm64)" ;;
  esac
}
valid_domain() {
  local domain=$1 label
  local -a labels
  [[ ${#domain} -le 253 && $domain =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$ ]] || return 1
  IFS=. read -r -a labels <<< "$domain"
  for label in "${labels[@]}"; do ((${#label} <= 63)) || return 1; done
}

usage() { cat <<'EOF'
Usage: sudo scripts/bootstrap-stack.sh --domain DOMAIN [--acme-email EMAIL] [--external-tls-terminated] [--skip-website] [--dry-run]
       scripts/bootstrap-stack.sh --self-test
Runs on a fresh Ubuntu 24.04 amd64 or arm64 host from this repository checkout. DNS can be
configured afterward; public ACME certificates issue after DNS points here.
EOF
}
die() { printf 'bootstrap: %s\n' "$*" >&2; exit 1; }
log() { printf 'bootstrap: %s\n' "$*"; }
cidr_overlaps() {
  python3 - "$1" "${@:2}" <<'PY'
import ipaddress
import sys
try:
    target = ipaddress.ip_network(sys.argv[1], strict=False)
    others = [ipaddress.ip_network(s, strict=False) for s in sys.argv[2:] if s and s != "default"]
except ValueError as exc:
    print(f"invalid network data: {exc}", file=sys.stderr)
    sys.exit(2)
sys.exit(0 if any(target.overlaps(other) for other in others) else 1)
PY
}
test_preflight() {
  cidr_overlaps 172.24.0.0/24 172.24.0.0/24 || die 'self-test did not detect identical subnet'
  if cidr_overlaps 172.24.0.0/24 172.24.1.0/24; then die 'self-test falsely detected adjacent subnet'; else [[ $? -eq 1 ]] || die 'self-test rejected valid subnet input'; fi
  if cidr_overlaps 172.24.0.0/24 172.16.0.0/12; then :; else die 'self-test missed overlapping supernet'; fi
  log 'CIDR overlap self-test passed (identical, adjacent, overlapping supernet)'
  select_architecture amd64
  [[ $go_arch == amd64 && $nixpacks_arch == x86_64-unknown-linux-musl ]] || die 'amd64 artifact selection failed'
  select_architecture arm64
  [[ $go_arch == arm64 && $nixpacks_arch == aarch64-unknown-linux-musl ]] || die 'arm64 artifact selection failed'
  log 'architecture artifact selection self-test passed (amd64, arm64)'
  valid_domain example.com || die 'valid domain self-test failed'
  if valid_domain "$(printf 'a%.0s' {1..64}).example"; then die 'invalid label accepted'; fi
  log 'domain label length self-test passed'
}
while (($#)); do
  case "$1" in
    --domain) (($# >= 2)) || die '--domain requires a value'; domain=$2; shift 2 ;;
    --acme-email) (($# >= 2)) || die '--acme-email requires a value'; email=$2; shift 2 ;;
    --skip-website) skip_website=1; shift ;;
    --external-tls-terminated) external_tls_terminated=1; shift ;;
    --dry-run) dry_run=1; shift ;;
    --self-test) self_test=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done
if ((self_test)); then test_preflight; exit 0; fi
[[ $EUID -eq 0 ]] || die 'run as root'
valid_domain "$domain" || die 'provide a lowercase DNS domain with valid labels (each label must be at most 63 characters)'
email_pattern='^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$'
if ((external_tls_terminated == 0)); then
  [[ $email =~ $email_pattern ]] || die 'provide a simple email address safe for the generated YAML'
else
  [[ -z $email ]] || die '--acme-email is not used with --external-tls-terminated'
fi
[[ -f $repo_root/go.mod && -f $repo_root/cmd/ah/main.go ]] || die 'run from an agentic-hosting source checkout'

preflight() {
  [[ $(. /etc/os-release; printf '%s %s' "${ID:-}" "${VERSION_ID:-}") == 'ubuntu 24.04' ]] || die 'only Ubuntu 24.04 is supported'
  select_architecture "$(dpkg --print-architecture)"
  [[ ! -e /var/lib/ah ]] || die '/var/lib/ah exists; refusing to overwrite persistent state'
  [[ ! -e /var/lib/ah-builds && ! -e /var/lib/traefik && ! -e /etc/traefik ]] || die 'existing ah build or Traefik state/config found'
  [[ ! -e /etc/default/ah && ! -e /etc/default/ah-api-proxy ]] || die 'ah environment file already exists'
  [[ ! -e /etc/systemd/system/ah.service && ! -e /etc/systemd/system/ah-api-proxy.service && ! -e /etc/systemd/system/ah-build-broker.service && ! -e /etc/systemd/system/ah-build@.service ]] || die 'ah systemd unit already exists'
  [[ ! -e /etc/docker/daemon.json ]] || die 'Docker daemon config exists; refusing unsafe replacement'
  [[ ! -e /usr/local/go && ! -e /usr/local/bin/ah && ! -e /usr/local/bin/nixpacks ]] || die 'existing Go, ah, or Nixpacks installation found'
  [[ ! -e /etc/apt/sources.list.d/docker.sources && ! -e /etc/apt/sources.list.d/gvisor.list ]] || die 'existing Docker or gVisor apt source found'
  for identity in ah ah-builder ah-proxy ah-build; do
    getent passwd "$identity" >/dev/null && die "user $identity already exists" || :
    getent group "$identity" >/dev/null && die "group $identity already exists" || :
  done
  if command -v ss >/dev/null 2>&1; then
    for port in 80 443 5000 8080 8081; do ss -H -lnt "sport = :$port" | grep -q . && die "TCP port $port is already in use" || :; done
  fi
  if command -v docker >/dev/null 2>&1; then
    if docker ps -q | grep -q .; then die 'running Docker containers found; refusing to restart a nonempty Docker host'; fi
    docker network inspect traefik-public >/dev/null 2>&1 && die 'Docker network traefik-public exists'
    docker volume inspect ah-registry >/dev/null 2>&1 && die 'Docker volume ah-registry exists'
    for n in ah-registry paas-traefik ah-website; do docker ps -a --format '{{.Names}}' | grep -qx "$n" && die "container $n exists" || :; done
    docker_subnets=$(docker network ls -q | xargs -r docker network inspect --format '{{range .IPAM.Config}}{{.Subnet}}{{"\n"}}{{end}}') || die 'could not inspect Docker networks'
    mapfile -t docker_subnet_list <<< "$docker_subnets"
    if cidr_overlaps "$INGRESS_SUBNET" "${docker_subnet_list[@]}"; then die "Docker network overlaps $INGRESS_SUBNET"; else [[ $? -eq 1 ]] || die 'could not parse Docker subnet data'; fi
  fi
  route_prefixes=$(ip -4 route show | awk '{print $1}') || die 'could not inspect host routes'
  mapfile -t host_route_list <<< "$route_prefixes"
  if cidr_overlaps "$INGRESS_SUBNET" "${host_route_list[@]}"; then die "host route overlaps $INGRESS_SUBNET"; else [[ $? -eq 1 ]] || die 'could not parse host route data'; fi
}
preflight
if ((dry_run)); then log "preflight passed for $domain; ingress $INGRESS_SUBNET, Traefik $TRAEFIK_IP"; exit 0; fi

export DEBIAN_FRONTEND=noninteractive
log 'installing system prerequisites and Docker'
apt-get update
apt-get install -y --no-install-recommends ca-certificates curl git gnupg jq socat xz-utils build-essential pkg-config libsqlite3-dev
install -d -m 0755 /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /tmp/docker.asc
actual_fingerprint=$(gpg --show-keys --with-colons /tmp/docker.asc | awk -F: '$1=="fpr" {print $10; exit}')
[[ $actual_fingerprint == "$DOCKER_KEY_FINGERPRINT" ]] || die 'Docker signing key fingerprint mismatch'
gpg --dearmor --yes -o /etc/apt/keyrings/docker.gpg /tmp/docker.asc; chmod 0644 /etc/apt/keyrings/docker.gpg; rm -f /tmp/docker.asc
cat > /etc/apt/sources.list.d/docker.sources <<'EOF'
Types: deb
URIs: https://download.docker.com/linux/ubuntu
Suites: noble
Components: stable
Architectures: $(dpkg --print-architecture)
Signed-By: /etc/apt/keyrings/docker.gpg
EOF
apt-get update
apt-get install -y --no-install-recommends docker-ce=5:29.9.0-1~ubuntu.24.04~noble docker-ce-cli=5:29.9.0-1~ubuntu.24.04~noble containerd.io=2.4.2-1~ubuntu.24.04~noble docker-buildx-plugin=0.38.0-1~ubuntu.24.04~noble docker-compose-plugin=5.6.0-1~ubuntu.24.04~noble

log 'installing gVisor from its signed apt repository'
curl -fsSL https://gvisor.dev/archive.key -o /tmp/gvisor.asc
gpg --dearmor --yes -o /etc/apt/keyrings/gvisor.gpg /tmp/gvisor.asc; chmod 0644 /etc/apt/keyrings/gvisor.gpg; rm -f /tmp/gvisor.asc
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/gvisor.gpg] https://storage.googleapis.com/gvisor/releases release main" > /etc/apt/sources.list.d/gvisor.list
apt-get update; apt-get install -y --no-install-recommends runsc=20261005.0

tmpdir=$(mktemp -d); trap 'rm -rf "$tmpdir"' EXIT
log 'installing verified Go and Nixpacks binaries'
curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${go_arch}.tar.gz" -o "$tmpdir/go.tgz"
echo "$go_sha256  $tmpdir/go.tgz" | sha256sum -c - >/dev/null || die 'Go checksum mismatch'
tar -C /usr/local -xzf "$tmpdir/go.tgz"
curl -fsSL "https://github.com/railwayapp/nixpacks/releases/download/v${NIXPACKS_VERSION}/nixpacks-v${NIXPACKS_VERSION}-${nixpacks_arch}.tar.gz" -o "$tmpdir/nixpacks.tgz"
echo "$nixpacks_sha256  $tmpdir/nixpacks.tgz" | sha256sum -c - >/dev/null || die 'Nixpacks checksum mismatch'
tar -tzf "$tmpdir/nixpacks.tgz" | grep -Fxq nixpacks || die 'Nixpacks archive layout mismatch'
tar -C "$tmpdir" -xzf "$tmpdir/nixpacks.tgz" nixpacks
install -m 0755 "$tmpdir/nixpacks" /usr/local/bin/nixpacks

log 'creating service users and persistent paths'
getent group ah >/dev/null || groupadd --system ah
getent group ah-builder >/dev/null || groupadd --system ah-builder
getent group ah-proxy >/dev/null || groupadd --system ah-proxy
getent group ah-build >/dev/null || groupadd --system ah-build
id ah >/dev/null 2>&1 || useradd --system --gid ah --home-dir /var/lib/ah --shell /usr/sbin/nologin ah
id ah-builder >/dev/null 2>&1 || useradd --system --gid ah-builder --groups docker,ah-build --home-dir /var/lib/ah-builds --shell /usr/sbin/nologin ah-builder
id ah-proxy >/dev/null 2>&1 || useradd --system --gid ah-proxy --no-create-home --shell /usr/sbin/nologin ah-proxy
usermod -aG docker,ah-build ah
install -d -o ah -g ah -m 0750 /var/lib/ah /var/lib/ah/backups /etc/traefik/dynamic
install -d -o ah -g ah-build -m 0710 /var/lib/ah-builds
install -d -o root -g root -m 0755 /etc/traefik
install -d -o 65532 -g 65532 -m 0700 /var/lib/traefik/certs
umask 077
master_key=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
bootstrap_token=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
printf '%s\n' "$master_key" > /var/lib/ah/master.key
chown ah:ah /var/lib/ah/master.key; chmod 0600 /var/lib/ah/master.key
printf 'AH_BOOTSTRAP_TOKEN=%s\nAH_BASE_DOMAIN=apps.%s\nAH_NIXPACKS_PATH=/usr/local/bin/nixpacks\nAH_BUILD_DIR=/var/lib/ah-builds\n' "$bootstrap_token" "$domain" > /etc/default/ah
if ((external_tls_terminated)); then printf 'AH_EXTERNAL_TLS_TERMINATED=true\n' >> /etc/default/ah; fi
chmod 0600 /etc/default/ah; unset master_key bootstrap_token

log 'configuring Docker runsc and ingress bridge'
cat > /etc/docker/daemon.json <<'EOF'
{"runtimes":{"runsc":{"path":"/usr/bin/runsc"}},"default-address-pools":[{"base":"172.20.0.0/14","size":24}]}
EOF
systemctl enable --now docker; systemctl restart docker
docker network create --driver bridge --subnet "$INGRESS_SUBNET" --gateway "$INGRESS_GATEWAY" --opt com.docker.network.bridge.name=br-ah-ingress traefik-public >/dev/null
docker volume create ah-registry >/dev/null

log 'building and installing API'
cd "$repo_root"
PATH=/usr/local/go/bin:$PATH CGO_ENABLED=1 go build -trimpath -ldflags='-s -w' -o "$tmpdir/ah" ./cmd/ah
install -o root -g root -m 0755 "$tmpdir/ah" /usr/local/bin/ah
install -o root -g root -m 0644 "$repo_root/deploy/ah.service" /etc/systemd/system/ah.service
install -o root -g root -m 0644 "$repo_root/deploy/ah-api-proxy.service" /etc/systemd/system/ah-api-proxy.service
install -o root -g root -m 0644 "$repo_root/deploy/ah-build-broker.service" /etc/systemd/system/ah-build-broker.service
install -o root -g root -m 0644 "$repo_root/deploy/ah-build@.service" /etc/systemd/system/ah-build@.service
printf 'AH_PROXY_BIND=%s\nAH_PROXY_PORT=8081\nAH_TRAEFIK_IP=%s\n' "$INGRESS_GATEWAY" "$TRAEFIK_IP" > /etc/default/ah-api-proxy
chmod 0644 /etc/default/ah-api-proxy
systemctl daemon-reload; systemctl enable --now ah-api-proxy.service ah-build-broker.service

log 'writing Traefik config with dashboard and Docker provider disabled'
cat > /etc/traefik/traefik.yml <<EOF
api:
  dashboard: false
log:
  level: INFO
providers:
  file:
    directory: /etc/traefik/dynamic
    watch: true
entryPoints:
  web: {address: ":8080"}
  websecure: {address: ":8443"}
EOF
if ((external_tls_terminated == 0)); then
  cat >> /etc/traefik/traefik.yml <<EOF
certificatesResolvers:
  letsencrypt:
    acme:
      email: "$email"
      storage: /etc/traefik/certs/acme.json
      httpChallenge: {entryPoint: web}
EOF
fi
chown root:ah /etc/traefik/traefik.yml; chmod 0640 /etc/traefik/traefik.yml
if ((external_tls_terminated)); then
  cat > /etc/traefik/dynamic/00-platform.yml <<EOF
http:
  routers:
    api-http:
      rule: "Host(\`api.$domain\`) || (Host(\`$domain\`) && PathPrefix(\`/v1/\`))"
      priority: 100
      entryPoints: [web]
      middlewares: [external-https-proto]
      service: ah-api
  middlewares:
    external-https-proto:
      headers:
        customRequestHeaders:
          X-Forwarded-Proto: https
  services:
    ah-api:
      loadBalancer:
        servers: [{url: "http://ah-api-relay:8081"}]
EOF
else
cat > /etc/traefik/dynamic/00-platform.yml <<EOF
http:
  routers:
    api-https:
      rule: "Host(\`api.$domain\`) || (Host(\`$domain\`) && PathPrefix(\`/v1/\`))"
      priority: 100
      entryPoints: [websecure]
      service: ah-api
      tls: {certResolver: letsencrypt}
    api-http:
      rule: "Host(\`api.$domain\`) || (Host(\`$domain\`) && PathPrefix(\`/v1/\`))"
      priority: 100
      entryPoints: [web]
      middlewares: [https-redirect]
      service: ah-api
  middlewares:
    https-redirect:
      redirectScheme: {scheme: https, permanent: true}
  services:
    ah-api:
      loadBalancer:
        servers: [{url: "http://ah-api-relay:8081"}]
EOF
fi
if ((skip_website == 0)); then
  if ((external_tls_terminated)); then
    cat > /etc/traefik/dynamic/10-website.yml <<EOF
http:
  routers:
    website-http:
      rule: "Host(\`$domain\`) || Host(\`www.$domain\`)"
      entryPoints: [web]
      service: ah-website
  services:
    ah-website:
      loadBalancer:
        servers: [{url: "http://ah-website:80"}]
EOF
  else
  cat > /etc/traefik/dynamic/10-website.yml <<EOF
http:
  routers:
    website-https:
      rule: "Host(\`$domain\`) || Host(\`www.$domain\`)"
      entryPoints: [websecure]
      service: ah-website
      tls: {certResolver: letsencrypt}
    website-http:
      rule: "Host(\`$domain\`) || Host(\`www.$domain\`)"
      entryPoints: [web]
      middlewares: [https-redirect]
      service: ah-website
  services:
    ah-website:
      loadBalancer:
        servers: [{url: "http://ah-website:80"}]
EOF
  fi
  chown root:ah /etc/traefik/dynamic/10-website.yml; chmod 0640 /etc/traefik/dynamic/10-website.yml
fi
chown root:ah /etc/traefik/dynamic/00-platform.yml; chmod 0640 /etc/traefik/dynamic/00-platform.yml
install -o 65532 -g 65532 -m 0600 /dev/null /var/lib/traefik/certs/acme.json

if ((skip_website == 0)); then
  grep -qx 'FROM nginx:alpine' "$repo_root/website/Dockerfile" || die 'website Dockerfile base changed; update its pinned digest before retrying'
  sed 's|^FROM nginx:alpine$|FROM nginx@'"$nginx_digest"'|' "$repo_root/website/Dockerfile" > "$tmpdir/Dockerfile.website"
  docker build -t ah-website:bootstrap -f "$tmpdir/Dockerfile.website" "$repo_root/website"
  docker run -d --name ah-website --restart unless-stopped --network traefik-public --ip "$WEBSITE_IP" ah-website:bootstrap >/dev/null
fi
docker run -d --name ah-registry --restart unless-stopped --network traefik-public --ip "$REGISTRY_IP" -p 127.0.0.1:5000:5000 -v ah-registry:/var/lib/registry registry@${registry_digest} >/dev/null
traefik_ports=(-p 80:8080)
if ((external_tls_terminated == 0)); then traefik_ports+=(-p 443:8443); fi
docker run -d --name paas-traefik --restart unless-stopped --network traefik-public --ip "$TRAEFIK_IP" --add-host "ah-api-relay:$INGRESS_GATEWAY" "${traefik_ports[@]}" -v /etc/traefik/traefik.yml:/etc/traefik/traefik.yml:ro -v /etc/traefik/dynamic:/etc/traefik/dynamic:ro -v /var/lib/traefik/certs:/etc/traefik/certs traefik@${traefik_digest} --configFile=/etc/traefik/traefik.yml >/dev/null
systemctl enable --now ah.service
for _ in {1..40}; do curl -fsS -H 'X-Forwarded-Proto: https' http://127.0.0.1:8080/v1/system/health >/dev/null && break; sleep 1; done
curl -fsS -H 'X-Forwarded-Proto: https' http://127.0.0.1:8080/v1/system/health >/dev/null || { journalctl -u ah.service -n 80 --no-pager; die 'API health check failed'; }

log 'stack installed; public certificates require DNS pointing at this host'
log 'bootstrap token is stored in root-only /etc/default/ah; retrieve it through an approved secure channel'
log "after DNS and ACME certificate issuance, verify: curl --fail --show-error https://api.$domain/v1/system/health"
log 'build broker and worker template are installed; git-based service builds still need a live smoke test'
