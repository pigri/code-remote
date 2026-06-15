#!/bin/sh
# Install the latest code-remote .deb from GitHub Releases.
#   curl -fsSL https://raw.githubusercontent.com/pigri/code-remote/main/install.sh | sh
set -eu

REPO="pigri/code-remote"

arch="$(uname -m)"
case "$arch" in
    x86_64)        deb_arch=amd64 ;;
    aarch64|arm64) deb_arch=arm64 ;;
    *) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac

ver="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
        | grep -m1 '"tag_name"' | sed -E 's/.*"v?([^"]+)".*/\1/')"
[ -n "$ver" ] || { echo "could not determine latest release" >&2; exit 1; }

deb="code-remote_${ver}_${deb_arch}.deb"
url="https://github.com/$REPO/releases/download/v${ver}/${deb}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "Downloading code-remote ${ver} (${deb_arch})..."
curl -fSL "$url" -o "$tmp/$deb"

echo "Installing (sudo)..."
sudo dpkg -i "$tmp/$deb" || sudo apt-get install -y -f

echo "Installed. Try: crctl ls"
