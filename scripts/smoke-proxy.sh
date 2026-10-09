#!/usr/bin/env bash
# Disposable two-project proof of proxy-only and proxy+operator overrides.
# Builds no images: set WHATSAPP_IMAGE_REGISTRY / WHATSAPP_IMAGE_TAG first.
# Requires Docker Compose >= 2.24.0; never uses a real store or calls MCP tools.
set -euo pipefail
cd "$(dirname "$0")/.."

# Ignore a checkout's .env; every volume/network below belongs to this run.
unset COMPOSE_FILE COMPOSE_ENV_FILES
export WHATSAPP_IMAGE_REGISTRY=${WHATSAPP_IMAGE_REGISTRY:-ghcr.io/tauri-epo}
export WHATSAPP_IMAGE_TAG=${WHATSAPP_IMAGE_TAG:-latest}
export WHATSAPP_OUTBOX="" WHATSAPP_MCP_PORT=8000
export WHATSAPP_BRIDGE_BIND=127.0.0.1 WHATSAPP_OPERATOR_BIND=""
export WHATSAPP_OPERATOR_TOKEN_FILE="" WHATSAPP_PUBLIC_URL=""
export WHATSAPP_PAIRING_STDOUT=false WHATSAPP_READ_ONLY=false
export WHATSAPP_ALLOW_TOOLS="" WHATSAPP_DENY_TOOLS="" WHATSAPP_ALLOWED_CHATS=""
export WHISPER_URL="" TRANSCRIBE_ON_INGEST=false
export WHATSAPP_MCP_OAUTH_ISSUER="" WHATSAPP_SNAPSHOT_DIR=""
run="wamcp-proxy-${$}"
export WHATSAPP_PROXY_NETWORK="$run-proxy"
export WHATSAPP_OPERATOR_NETWORK="$run-operator"
export WHATSAPP_OPERATOR_PORT=8091
WHATSAPP_OPERATOR_TOKEN=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
export WHATSAPP_OPERATOR_TOKEN
export PROBE_ALIAS_A="$run-a" PROBE_ALIAS_B="$run-b"
export PROBE_TOKEN_A=proxy-smoke-token-a-0123456789abcdef
export PROBE_TOKEN_B=proxy-smoke-token-b-0123456789abcdef
env_file=".proxy-smoke-env.$$"
: > "$env_file"
base=(--env-file "$env_file" -f docker-compose.yml)
proxy=(-f docker-compose.proxy.yml)
operator=(-f docker-compose.operator.yml)

select_instance() {
  export COMPOSE_PROJECT_NAME="$run-$1"
  export WHATSAPP_PROXY_ALIAS="$run-$1"
  export WHATSAPP_OPERATOR_ALIAS="$run-operator-$1"
  export WHATSAPP_MCP_ALLOWED_HOSTS="$WHATSAPP_PROXY_ALIAS"
  if [ "$1" = a ]; then
    export WHATSAPP_MCP_TOKEN="$PROBE_TOKEN_A"
  else
    export WHATSAPP_MCP_TOKEN="$PROBE_TOKEN_B"
  fi
  export WHATSAPP_BRIDGE_TOKEN="bridge-smoke-$1-0123456789abcdef"
}
cleanup() {
  local rc=$?
  for instance in a b; do
    select_instance "$instance"
    if [ "$rc" -ne 0 ]; then
      docker compose "${base[@]}" "${proxy[@]}" "${operator[@]}" logs --tail 50 || true
    fi
    docker compose "${base[@]}" "${proxy[@]}" "${operator[@]}" down -v --remove-orphans || true
  done
  docker network rm "$WHATSAPP_PROXY_NETWORK" "$WHATSAPP_OPERATOR_NETWORK" || true
  rm -f "$env_file"
}
trap cleanup EXIT
docker network create "$WHATSAPP_PROXY_NETWORK"
docker network create "$WHATSAPP_OPERATOR_NETWORK"
select_instance a
for combination in base operator proxy combined reversed; do
  files=("${base[@]}")
  case "$combination" in
    operator) files+=("${operator[@]}") ;;
    proxy) files+=("${proxy[@]}") ;;
    combined) files+=("${proxy[@]}" "${operator[@]}") ;;
    reversed) files+=("${operator[@]}" "${proxy[@]}") ;;
  esac
  docker compose "${files[@]}" config --quiet
  echo "compose config: $combination -> ok"
done

for mode in proxy combined; do
  files=("${base[@]}" "${proxy[@]}")
  [ "$mode" = proxy ] || files+=("${operator[@]}")
  for instance in a b; do
    select_instance "$instance"
    docker compose "${files[@]}" up -d --no-build --wait --wait-timeout 120 bridge mcp
    echo "$mode: $COMPOSE_PROJECT_NAME running (no per-instance port setting)"
    docker compose "${files[@]}" ps
    bridge=$(docker compose "${files[@]}" ps -q bridge)
    mcp=$(docker compose "${files[@]}" ps -q mcp)
    for ctr in "$bridge" "$mcp"; do
      bindings=$(docker inspect -f '{{json .HostConfig.PortBindings}}' "$ctr")
      [ "$bindings" = null ] || [ "$bindings" = '{}' ]
    done
    echo "$WHATSAPP_PROXY_ALIAS: published host ports -> none"
    # Actual uid-1000 writes, then bridge reads the bytes through its mount.
    docker exec "$mcp" python -c 'import os,pathlib; assert os.getuid()==1000; p=pathlib.Path("/app/outbox/proxy-smoke.txt"); p.write_text("fake outbox payload"); print("MCP uid=1000: named outbox write -> ok")'
    docker exec "$bridge" sh -c 'test "$(id -u)" = 1000 && test "$(cat /app/outbox/proxy-smoke.txt)" = "fake outbox payload" && echo "bridge uid=1000: same named outbox bytes -> ok"'
    # Registered-recipient network lookup is skipped for a fake group JID.
    docker exec -i "$mcp" python - <<'PY'
import json, os, urllib.request
req = urllib.request.Request("http://127.0.0.1:8080/api/send", data=json.dumps({"recipient":"120363000000000001@g.us", "media_path":"/app/outbox/proxy-smoke.txt", "dry_run":True}).encode(), headers={"Authorization":"Bearer " + os.environ["WHATSAPP_BRIDGE_TOKEN"], "Content-Type":"application/json"})
with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(req, timeout=5) as r:
    assert r.status == 200 and json.load(r)["dry_run"] is True
print("REST /api/send named outbox dry_run -> 200 (no WhatsApp send)")
PY
    if [ "$mode" = combined ]; then
      state=$(docker exec "$bridge" whatsapp-bridge --operator-status)
      echo "$WHATSAPP_OPERATOR_ALIAS:8091 private operator -> $state"
      case "$state" in starting|awaiting_qr|expired) ;; *) exit 1 ;; esac
    fi
  done
  echo "$mode: probe from independent container on proxy network"
  docker run --rm -i --network "$WHATSAPP_PROXY_NETWORK" \
    -e PROBE_ALIAS_A -e PROBE_ALIAS_B -e PROBE_TOKEN_A -e PROBE_TOKEN_B \
    "$WHATSAPP_IMAGE_REGISTRY/whatsapp-mcp-server:$WHATSAPP_IMAGE_TAG" \
    python - < scripts/proxy-probe.py
  for instance in a b; do
    select_instance "$instance"
    docker compose "${files[@]}" down -v --remove-orphans
  done
done
echo 'proxy smoke -> PASS (two projects, both override combinations, unpaired)'
