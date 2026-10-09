#!/bin/sh
# Container healthcheck for the Go bridge.
#
# /api/health (bearer token required) answers 200 as soon as the REST server
# is up, even while waiting for the QR scan; the body carries connected/paired.
# Use /api/ready when you need "WhatsApp is connected" semantics.
set -eu

port="${WHATSAPP_BRIDGE_PORT:-8080}"
bind="${WHATSAPP_BRIDGE_BIND:-127.0.0.1}"
bind=$(printf '%s' "$bind" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//;s/^\[//;s/\]$//')
bind=${bind:-127.0.0.1}
case "$bind" in
    0.0.0.0) bind=127.0.0.1 ;;
    ::) bind='[::1]' ;;
    *:*) bind="[$bind]" ;;
esac
token="${WHATSAPP_BRIDGE_TOKEN:-}"
store="${WHATSAPP_STORE_DIR:-/app/store}"
if [ -z "$token" ] && [ -r "$store/.bridge-token" ]; then
    token="$(cat "$store/.bridge-token")"
fi

# This probe is always local: never send its bearer token to an HTTP proxy.
exec wget -Y off -q -O /dev/null -T 4 \
    --header="Authorization: Bearer ${token}" \
    "http://${bind}:${port}/api/health"
