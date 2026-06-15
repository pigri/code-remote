#!/bin/sh
# Render runtime Synapse config from the repo templates into the deploy dir,
# substituting the real domain (NGROK_DOMAIN, from .env) into the upstreams
# host — Synapse routes by Host header, so it must match the live ngrok domain.
# Run by the synapse systemd unit's ExecStartPre (NGROK_DOMAIN comes from the
# unit's EnvironmentFile).
set -eu

REPO="$(cd "$(dirname "$0")/.." && pwd)"
RUN="${CODE_REMOTE_RUN:-$HOME/.local/share/code-remote}"
: "${NGROK_DOMAIN:?NGROK_DOMAIN not set (load .env)}"

mkdir -p "$RUN"
cp "$REPO/deploy/synapse/security_rules.yaml" "$RUN/security_rules.yaml"
sed "s#/etc/synapse/upstreams.yaml#$RUN/upstreams.yaml#" \
    "$REPO/deploy/synapse/config.yaml" > "$RUN/config.yaml"
sed "s/your-domain.ngrok.dev/$NGROK_DOMAIN/" \
    "$REPO/deploy/synapse/upstreams.yaml" > "$RUN/upstreams.yaml"
