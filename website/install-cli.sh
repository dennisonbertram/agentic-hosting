#!/usr/bin/env bash
# install-cli.sh — Install the ahc CLI for agentic-hosting
#
# Usage:
#   curl -fsSL https://agentic.hosting/install-cli.sh | bash
#
# The script detects your OS and architecture, downloads the correct binary
# from the latest GitHub release, and installs it to /usr/local/bin/ahc
# (or ~/.local/bin/ahc if /usr/local/bin is not writable).

set -euo pipefail

REPO="dennisonbertram/agentic-hosting"
BINARY="ahc"
GITHUB_API="https://api.github.com/repos/${REPO}/releases/latest"

# ---- detect OS and arch ----

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"

case "$OS" in
  linux)  OS="linux" ;;
  darwin) OS="darwin" ;;
  *)
    echo "Unsupported OS: $OS" >&2
    echo "Please download manually from https://github.com/${REPO}/releases" >&2
    exit 1
    ;;
esac

case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *)
    echo "Unsupported architecture: $ARCH" >&2
    echo "Please download manually from https://github.com/${REPO}/releases" >&2
    exit 1
    ;;
esac

# ---- resolve install directory ----

if [ -w "/usr/local/bin" ]; then
  INSTALL_DIR="/usr/local/bin"
else
  INSTALL_DIR="${HOME}/.local/bin"
  mkdir -p "$INSTALL_DIR"
fi

# ---- fetch latest release version ----

echo "Fetching latest ${BINARY} release..."

if command -v curl >/dev/null 2>&1; then
  LATEST_VERSION="$(curl -fsSL "${GITHUB_API}" | grep '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/')"
elif command -v wget >/dev/null 2>&1; then
  LATEST_VERSION="$(wget -qO- "${GITHUB_API}" | grep '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/')"
else
  echo "Error: curl or wget is required to install ahc" >&2
  exit 1
fi

if [ -z "$LATEST_VERSION" ]; then
  echo "Error: Could not determine latest release version." >&2
  echo "Check https://github.com/${REPO}/releases for the latest version." >&2
  exit 1
fi

echo "Installing ${BINARY} ${LATEST_VERSION} (${OS}/${ARCH})..."

# ---- build download URL ----

ARCHIVE_NAME="${BINARY}_${OS}_${ARCH}"
if [ "$OS" = "windows" ]; then
  ARCHIVE_NAME="${ARCHIVE_NAME}.zip"
else
  ARCHIVE_NAME="${ARCHIVE_NAME}.tar.gz"
fi

DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${LATEST_VERSION}/${ARCHIVE_NAME}"

# ---- download and extract ----

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

if command -v curl >/dev/null 2>&1; then
  curl -fsSL "$DOWNLOAD_URL" -o "${TMP_DIR}/${ARCHIVE_NAME}"
else
  wget -qO "${TMP_DIR}/${ARCHIVE_NAME}" "$DOWNLOAD_URL"
fi

if [ "$OS" = "windows" ]; then
  unzip -q "${TMP_DIR}/${ARCHIVE_NAME}" -d "$TMP_DIR"
else
  tar -xzf "${TMP_DIR}/${ARCHIVE_NAME}" -C "$TMP_DIR"
fi

# ---- install binary ----

chmod +x "${TMP_DIR}/${BINARY}"
mv "${TMP_DIR}/${BINARY}" "${INSTALL_DIR}/${BINARY}"

# ---- verify ----

echo ""
echo "Installed: ${INSTALL_DIR}/${BINARY}"

if ! command -v ahc >/dev/null 2>&1; then
  echo ""
  echo "NOTE: ${INSTALL_DIR} is not in your PATH."
  echo "Add it by running:"
  echo "  echo 'export PATH=\"${INSTALL_DIR}:\$PATH\"' >> ~/.bashrc && source ~/.bashrc"
  echo ""
fi

# ---- getting started ----

echo ""
echo "Getting started:"
echo ""
echo "  1. Configure your API endpoint:"
echo "       ahc configure --url https://your-ah-instance.example.com --key YOUR_API_KEY"
echo ""
echo "  2. Create a tenant account:"
echo "       ahc register --name \"My Project\" --email you@example.com"
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
