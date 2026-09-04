#!/bin/sh
# Container healthcheck for the Go bridge.
#
# /api/health requires the bridge bearer token and answers 200 only while the
# WhatsApp connection is up (503 otherwise), so a failing check means either
# "REST not answering" or "WhatsApp disconnected / not paired yet".
set -eu

port="${WHATSAPP_BRIDGE_PORT:-8080}"
token="${WHATSAPP_BRIDGE_TOKEN:-}"
store="${WHATSAPP_STORE_DIR:-/app/store}"
if [ -z "$token" ] && [ -r "$store/.bridge-token" ]; then
    token="$(cat "$store/.bridge-token")"
fi

exec wget -q -O /dev/null -T 4 \
    --header="Authorization: Bearer ${token}" \
    "http://127.0.0.1:${port}/api/health"
