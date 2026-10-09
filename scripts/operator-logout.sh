#!/usr/bin/env bash
# OPERATOR_TOKEN=<separate secret> scripts/operator-logout.sh http://127.0.0.1:8090 idle
set -euo pipefail
url=${1:?Usage: operator-logout.sh OPERATOR_URL [exit|idle]}
after=${2:-exit}
case "$after" in exit|idle) ;; *) echo 'after must be exit or idle' >&2; exit 2 ;; esac
token=${OPERATOR_TOKEN:?Set OPERATOR_TOKEN to the separate operator secret}
case "$token" in *[!a-zA-Z0-9_-]*) echo 'Invalid operator token encoding' >&2; exit 2 ;; esac
# Pass the header through stdin so credentials are absent from process argv.
printf 'header = "Authorization: Bearer %s"\n' "$token" |
  curl --config - --fail-with-body --silent --show-error --max-time 15 \
    -X POST -H 'Content-Type: application/json' \
    --data "{\"after\":\"$after\"}" "${url%/}/operator/v1/logout"
