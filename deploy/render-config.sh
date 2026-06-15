#!/bin/sh
# Render runtime Synapse config from the templates next to this script into the
# deploy dir, substituting the real domain (NGROK_DOMAIN, from .env) into the
# upstreams host — Synapse routes by Host header, so it must match the live
# ngrok domain. Run by the synapse systemd unit's ExecStartPre.
#
# Works both in-repo (templates in ./synapse) and when packaged
# (/usr/share/code-remote/{render-config.sh,synapse/}).
set -eu

TPL="${CODE_REMOTE_TEMPLATES:-$(cd "$(dirname "$0")/synapse" && pwd)}"
RUN="${CODE_REMOTE_RUN:-$HOME/.local/share/code-remote}"
: "${NGROK_DOMAIN:?NGROK_DOMAIN not set (load .env)}"

mkdir -p "$RUN"
cp "$TPL/security_rules.yaml" "$RUN/security_rules.yaml"
sed "s#/etc/synapse/upstreams.yaml#$RUN/upstreams.yaml#" \
    "$TPL/config.yaml" > "$RUN/config.yaml"
sed "s/your-domain.ngrok.dev/$NGROK_DOMAIN/" \
    "$TPL/upstreams.yaml" > "$RUN/upstreams.yaml"
