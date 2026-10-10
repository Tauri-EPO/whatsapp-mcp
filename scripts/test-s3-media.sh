#!/usr/bin/env bash
# Real S3 -> bridge bearer HTTP -> Python MCP consumers, without a paired phone.
set -euo pipefail
[[ $(uname -s) == Linux ]] || { echo 'Run this proof on Linux (CI or a container).' >&2; exit 1; }
cd "$(dirname "$0")/.."
scratch=$(mktemp -d)
container="wamcp-minio-proof-$$"
cleanup() {
  docker rm -f "$container" >/dev/null 2>&1 || true
  rm -rf "$scratch"
}
trap cleanup EXIT
release=RELEASE.2025-09-07T16-13-09Z
curl --fail --silent --show-error --location \
  "https://github.com/minio/minio/releases/download/$release/minio.linux-amd64.$release" \
  --output "$scratch/minio"
printf '%s  %s\n' 7c5bd8512c6e966455b1d198209358b2d191c77a83ab377c4073281065fb855f "$scratch/minio" | sha256sum --check
chmod 755 "$scratch/minio"
docker run -d --name "$container" -p 127.0.0.1::9000 \
  -e MINIO_ROOT_USER=wamcp-test-access -e MINIO_ROOT_PASSWORD=wamcp-test-secret-sentinel \
  -v "$scratch/minio:/minio:ro" golang:1.27-alpine /minio server /data >/dev/null
address=$(docker port "$container" 9000/tcp | head -n 1)
export WAMCP_TEST_MINIO_ENDPOINT="http://$address"
ready=false
for _ in $(seq 1 30); do
  if curl --noproxy '*' --fail --silent "$WAMCP_TEST_MINIO_ENDPOINT/minio/health/ready" >/dev/null; then ready=true; break; fi
  sleep 1
done
if [[ $ready != true ]]; then docker logs "$container"; exit 1; fi
export WAMCP_TEST_MCP_PYTHON="$(pwd)/whatsapp-mcp-server/.venv/bin/python"
test -x "$WAMCP_TEST_MCP_PYTHON"
cd whatsapp-bridge
go test -count=1 -timeout 10m -v -run 'TestMinIO|TestS3Config' ./...
