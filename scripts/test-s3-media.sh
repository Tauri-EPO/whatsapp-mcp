#!/usr/bin/env bash
# Real S3 -> bridge bearer HTTP -> Python MCP consumers, without a paired phone.
set -euo pipefail
[[ $(uname -s) == Linux ]] || { echo 'Run this proof on Linux (CI or a container).' >&2; exit 1; }
cd "$(dirname "$0")/.."
scratch=$(mktemp -d)
container="wamcp-minio-proof-$$"
active_pid=
active_label=
bounded() {
  local label=$1 seconds=$2 status
  shift 2
  active_label=$label
  echo "Waiting for $label (limit ${seconds}s)" >&2
  timeout --kill-after=5s "${seconds}s" "$@" &
  active_pid=$!
  # wait is interruptible by Bash traps; a foreground external command is not.
  if wait "$active_pid"; then status=0; else status=$?; fi
  active_pid=
  if (( status != 0 )); then
    echo "Failed waiting for $label (exit $status, limit ${seconds}s)" >&2
  fi
  return "$status"
}
interrupted() {
  local signal=$1 status=$2
  trap '' INT TERM
  echo "Interrupted waiting for $active_label ($signal)" >&2
  if [[ -n $active_pid ]]; then
    # GNU timeout owns this process group and forwards to its children. Its
    # kill-after bound also handles a child that ignores the forwarded signal.
    kill -s "$signal" -- "-$active_pid" 2>/dev/null || kill -s "$signal" "$active_pid" 2>/dev/null || true
    wait "$active_pid" 2>/dev/null || true
    active_pid=
  fi
  exit "$status"
}
cleanup() {
  local status=$?
  trap - EXIT
  if bounded 'MinIO container lookup' 5 docker inspect "$container" >/dev/null 2>&1; then
    if (( status != 0 )); then
      bounded 'MinIO failure logs' 5 docker logs --tail 80 "$container" >&2 || true
    fi
    bounded 'MinIO container cleanup' 15 docker rm -f "$container" >/dev/null || true
  fi
  bounded 'scratch cleanup' 10 rm -rf "$scratch" || true
  exit "$status"
}
trap cleanup EXIT
trap 'interrupted INT 130' INT
trap 'interrupted TERM 143' TERM
release=RELEASE.2025-09-07T16-13-09Z
bounded 'MinIO binary download' 90 curl --fail --silent --show-error --location \
  --connect-timeout 10 --max-time 85 \
  "https://github.com/minio/minio/releases/download/$release/minio.linux-amd64.$release" \
  --output "$scratch/minio"
printf '%s  %s\n' 7c5bd8512c6e966455b1d198209358b2d191c77a83ab377c4073281065fb855f "$scratch/minio" | sha256sum --check
chmod 755 "$scratch/minio"
bounded 'MinIO container startup' 30 docker run -d --name "$container" -p 127.0.0.1::9000 \
  -e MINIO_ROOT_USER=wamcp-test-access -e MINIO_ROOT_PASSWORD=wamcp-test-secret-sentinel \
  -v "$scratch/minio:/minio:ro" golang:1.27-alpine /minio server /data >/dev/null
bounded 'MinIO published port' 10 docker port "$container" 9000/tcp >"$scratch/port"
address=
IFS= read -r address <"$scratch/port" || true
[[ -n "$address" ]] || { echo 'MinIO published no port' >&2; exit 1; }
export WAMCP_TEST_MINIO_ENDPOINT="http://$address"
echo 'Waiting for MinIO readiness (limit 40s; each HTTP call at most 3s)' >&2
ready=false
deadline=$((SECONDS + 40))
while (( SECONDS < deadline )); do
  if curl --noproxy '*' --connect-timeout 2 --max-time 3 --fail --silent \
    "$WAMCP_TEST_MINIO_ENDPOINT/minio/health/ready" >/dev/null; then ready=true; break; fi
  sleep 1
done
if [[ $ready != true ]]; then echo 'Timed out waiting for MinIO readiness' >&2; exit 1; fi
export WAMCP_TEST_MCP_PYTHON="$(pwd)/whatsapp-mcp-server/.venv/bin/python"
test -x "$WAMCP_TEST_MCP_PYTHON"
cd whatsapp-bridge
# The outer process-group timeout also bounds bridge httptest startup, Python
# MCP children and their shutdown; kill-after handles a child ignoring TERM.
bounded 'S3 bridge/MCP integration and shutdown' 420 \
  go test -count=1 -timeout 6m -v -run 'TestMinIO|TestS3Config' ./...
