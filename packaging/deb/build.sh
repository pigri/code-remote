#!/bin/sh
# Build a code-remote .deb from pre-built binaries (no debhelper needed).
#
#   VERSION=1.2.3 ARCH=amd64 BIN_DIR=bin packaging/deb/build.sh [outdir]
#
# Defaults: ARCH=$(dpkg --print-architecture), BIN_DIR=bin, outdir=dist.
set -eu

VERSION="${VERSION:?set VERSION (e.g. 1.2.3)}"
ARCH="${ARCH:-$(dpkg --print-architecture 2>/dev/null || echo amd64)}"
BIN_DIR="${BIN_DIR:-bin}"
OUT="${1:-dist}"

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"

PKG="$(mktemp -d)"
trap 'rm -rf "$PKG"' EXIT
chmod 0755 "$PKG" # package root must be world-readable, not mktemp's 0700

# Binaries
for b in claude-remote-api crctl ngrok-forward; do
    install -D -m 0755 "$BIN_DIR/$b" "$PKG/usr/bin/$b"
done

# Per-user systemd units
for u in api synapse ngrok; do
    install -D -m 0644 "$HERE/systemd/code-remote-$u.service" \
        "$PKG/usr/lib/systemd/user/code-remote-$u.service"
done

# Shared: render script, Synapse config templates, env example
install -D -m 0755 "$REPO/deploy/render-config.sh" "$PKG/usr/share/code-remote/render-config.sh"
for f in config.yaml upstreams.yaml security_rules.yaml; do
    install -D -m 0644 "$REPO/deploy/synapse/$f" "$PKG/usr/share/code-remote/synapse/$f"
done
install -D -m 0644 "$REPO/deploy/.env.example" "$PKG/usr/share/code-remote/env.example"

# Control + maintainer scripts
install -d "$PKG/DEBIAN"
sed -e "s/__VERSION__/$VERSION/" -e "s/__ARCH__/$ARCH/" "$HERE/control.in" > "$PKG/DEBIAN/control"
install -m 0755 "$HERE/postinst" "$PKG/DEBIAN/postinst"

mkdir -p "$OUT"
DEB="$OUT/code-remote_${VERSION}_${ARCH}.deb"
dpkg-deb --root-owner-group --build "$PKG" "$DEB"
echo "built $DEB"
