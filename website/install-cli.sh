#!/usr/bin/env bash
# install-cli.sh — Install the ahc CLI for agentic-hosting
#
# Usage:
#   curl -fsSL https://agentic.hosting/install-cli.sh | bash
#
# The script detects your OS and architecture, downloads the correct archive
# from the latest GitHub release, verifies it against the release's
# checksums.txt, and installs it to /usr/local/bin/ahc (or ~/.local/bin/ahc
# if /usr/local/bin is not writable).
#
# Everything runs inside main(), called on the last line, so a download that
# is cut off mid-stream cannot execute a partial script.

set -euo pipefail

REPO="dennisonbertram/agentic-hosting"
BINARY="ahc"
GITHUB_API="https://api.github.com/repos/${REPO}/releases/latest"

fetch() { # fetch <url> [output-file]; prints to stdout without a file
  if command -v curl >/dev/null 2>&1; then
    if [ $# -ge 2 ]; then curl -fsSL "$1" -o "$2"; else curl -fsSL "$1"; fi
  elif command -v wget >/dev/null 2>&1; then
    if [ $# -ge 2 ]; then wget -qO "$2" "$1"; else wget -qO- "$1"; fi
  else
    echo "Error: curl or wget is required to install ${BINARY}" >&2
    return 1
  fi
}

sha256_of() { # sha256_of <file>
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    echo "Error: sha256sum or shasum is required to verify the download" >&2
    return 1
  fi
}

main() {
  # ---- detect OS and arch ----

  local os arch
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch="$(uname -m)"

  case "$os" in
    linux)  os="linux" ;;
    darwin) os="darwin" ;;
    *)
      echo "Unsupported OS: $os" >&2
      echo "Please download manually from https://github.com/${REPO}/releases" >&2
      exit 1
      ;;
  esac

  case "$arch" in
    x86_64|amd64) arch="amd64" ;;
    arm64|aarch64) arch="arm64" ;;
    *)
      echo "Unsupported architecture: $arch" >&2
      echo "Please download manually from https://github.com/${REPO}/releases" >&2
      exit 1
      ;;
  esac

  # ---- resolve install directory ----

  local install_dir
  if [ -w "/usr/local/bin" ]; then
    install_dir="/usr/local/bin"
  else
    install_dir="${HOME}/.local/bin"
    mkdir -p "$install_dir"
  fi

  # ---- fetch latest release version ----

  echo "Fetching latest ${BINARY} release..."

  local version
  version="$(fetch "${GITHUB_API}" | grep '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/')"
  if [ -z "$version" ]; then
    echo "Error: Could not determine latest release version." >&2
    echo "Check https://github.com/${REPO}/releases for the latest version." >&2
    exit 1
  fi

  echo "Installing ${BINARY} ${version} (${os}/${arch})..."

  # ---- download archive and checksums from the same release ----

  local archive="${BINARY}_${os}_${arch}.tar.gz"
  local base_url="https://github.com/${REPO}/releases/download/${version}"

  local tmp_dir
  tmp_dir="$(mktemp -d)"
  trap 'rm -rf "$tmp_dir"' EXIT

  fetch "${base_url}/${archive}" "${tmp_dir}/${archive}"
  fetch "${base_url}/checksums.txt" "${tmp_dir}/checksums.txt"

  # ---- verify checksum ----

  local expected actual
  expected="$(awk -v f="$archive" '$2 == f {print $1}' "${tmp_dir}/checksums.txt")"
  if [ -z "$expected" ]; then
    echo "Error: ${archive} is not listed in checksums.txt for ${version}." >&2
    exit 1
  fi
  actual="$(sha256_of "${tmp_dir}/${archive}")"
  if [ "$expected" != "$actual" ]; then
    echo "Error: checksum mismatch for ${archive}." >&2
    echo "  expected: ${expected}" >&2
    echo "  actual:   ${actual}" >&2
    exit 1
  fi
  echo "Checksum verified."

  # ---- extract and install ----

  tar -xzf "${tmp_dir}/${archive}" -C "$tmp_dir"
  chmod +x "${tmp_dir}/${BINARY}"
  mv "${tmp_dir}/${BINARY}" "${install_dir}/${BINARY}"

  # ---- verify ----

  echo ""
  echo "Installed: ${install_dir}/${BINARY}"

  if ! command -v ahc >/dev/null 2>&1; then
    echo ""
    echo "NOTE: ${install_dir} is not in your PATH."
    echo "Add it by running:"
    echo "  echo 'export PATH=\"${install_dir}:\$PATH\"' >> ~/.bashrc && source ~/.bashrc"
    echo ""
  fi

  # ---- getting started ----

  echo ""
  echo "Getting started:"
  echo ""
  echo "  1. Configure your API endpoint:"
  echo "       ahc configure --url https://your-ah-instance.example.com --key YOUR_API_KEY"
  echo ""
  echo "  2. Create a tenant account (needs a bootstrap token from your operator):"
  echo "       ahc register \"My Project\" you@example.com --bootstrap-token <token> --save"
  echo ""
  echo "  3. Deploy your first service:"
  echo "       ahc deploy nginx:alpine my-site --port 80"
  echo "       ahc deploy https://github.com/org/repo my-app --port 3000"
  echo ""
  echo "  4. Create an instant environment:"
  echo "       ahc env create dev --template tmpl-golang"
  echo "       ahc env exec dev -- go version"
  echo ""
  echo "  Run 'ahc --help' to see all available commands."
  echo ""
  echo "Documentation: https://agentic.hosting/docs"
}

main "$@"
