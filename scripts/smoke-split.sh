#!/usr/bin/env bash
# Real disposable networks/stores; builds nothing and never calls MCP tools.
# Compose >= 2.24.4. Set WHATSAPP_IMAGE_REGISTRY / WHATSAPP_IMAGE_TAG first.
set -euo pipefail
cd "$(dirname "$0")/.."
unset COMPOSE_FILE COMPOSE_ENV_FILES
export WHATSAPP_IMAGE_REGISTRY=${WHATSAPP_IMAGE_REGISTRY:-ghcr.io/tauri-epo}
export WHATSAPP_IMAGE_TAG=${WHATSAPP_IMAGE_TAG:-latest}
export WHATSAPP_OUTBOX="" WHATSAPP_EXPORT_DIR=""
export WHATSAPP_OPERATOR_TOKEN_FILE="" WHATSAPP_PUBLIC_URL=""
export WHATSAPP_PAIRING_STDOUT=false WHATSAPP_READ_ONLY=false
export WHATSAPP_ALLOW_TOOLS="" WHATSAPP_DENY_TOOLS="" WHATSAPP_ALLOWED_CHATS=""
export TRANSCRIBE_ON_INGEST=false WHATSAPP_MCP_OAUTH_ISSUER="" WHATSAPP_SNAPSHOT_DIR=""
run="wamcp-split-${$}"
export WHATSAPP_PROXY_NETWORK="$run-proxy" WHATSAPP_OPERATOR_NETWORK="$run-operator"
WHATSAPP_OPERATOR_TOKEN=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
export WHATSAPP_OPERATOR_TOKEN
export PROBE_ALIAS_A="$run-a" PROBE_ALIAS_B="$run-b"
export OPERATOR_ALIAS_A="$run-operator-a" OPERATOR_ALIAS_B="$run-operator-b"
export OPERATOR_PORT_A=8090 OPERATOR_PORT_B=8091
export PROBE_TOKEN_A=split-smoke-token-a-0123456789abcdef PROBE_TOKEN_B=split-smoke-token-b-0123456789abcdef
env_file=".split-smoke-env.$$"
: > "$env_file"
base=(--env-file "$env_file" -f docker-compose.yml)
split=(-f docker-compose.split.yml)
image="$WHATSAPP_IMAGE_REGISTRY/whatsapp-mcp-server:$WHATSAPP_IMAGE_TAG"
select_instance() {
  export COMPOSE_PROJECT_NAME="$run-$1" PROBE_INSTANCE="$1"
  export WHATSAPP_PROXY_ALIAS="$run-$1" WHATSAPP_OPERATOR_ALIAS="$run-operator-$1"
  export WHATSAPP_MCP_ALLOWED_HOSTS="$WHATSAPP_PROXY_ALIAS"
  if [ "$1" = a ]; then
    export WHATSAPP_MCP_TOKEN="$PROBE_TOKEN_A" WHATSAPP_OPERATOR_PORT=8090
    # Empty exercises the real generated .bridge-token handoff.
    export WHATSAPP_BRIDGE_TOKEN=""
  else
    export WHATSAPP_MCP_TOKEN="$PROBE_TOKEN_B" WHATSAPP_OPERATOR_PORT=8091
    export WHATSAPP_BRIDGE_TOKEN="split-bridge-b-0123456789abcdef"
  fi
  export WHISPER_URL=http://whisper:8178/inference
}
cleanup() {
  local rc=$?
  for instance in a b; do
    select_instance "$instance"
    docker rm -f "$run-whisper-$instance" >/dev/null 2>&1 || true
    if [ "$rc" -ne 0 ]; then
      docker compose "${base[@]}" -f docker-compose.proxy.yml -f docker-compose.operator.yml "${split[@]}" logs --tail 40 || true
    fi
    docker compose "${base[@]}" -f docker-compose.proxy.yml -f docker-compose.operator.yml "${split[@]}" down -v --remove-orphans || true
  done
  docker network rm "$WHATSAPP_PROXY_NETWORK" "$WHATSAPP_OPERATOR_NETWORK" || true
  rm -f "$env_file"
}
trap cleanup EXIT
docker network create "$WHATSAPP_PROXY_NETWORK"
docker network create "$WHATSAPP_OPERATOR_NETWORK"
for mode in split operator proxy combined reversed; do
  files=("${base[@]}")
  case "$mode" in
    operator) files+=(-f docker-compose.operator.yml) ;;
    proxy) files+=(-f docker-compose.proxy.yml) ;;
    combined) files+=(-f docker-compose.proxy.yml -f docker-compose.operator.yml) ;;
    reversed) files+=(-f docker-compose.operator.yml -f docker-compose.proxy.yml) ;;
  esac
  files+=("${split[@]}")
  select_instance a
  docker compose "${files[@]}" config --quiet
  echo "split compose config: $mode -> ok"
  [ "$mode" != reversed ] || continue
  stores=() outboxes=() tokens=()
  for instance in a b; do
    select_instance "$instance"
    docker compose "${files[@]}" up -d --no-build --wait --wait-timeout 120 bridge mcp
    bridge=$(docker compose "${files[@]}" ps -q bridge)
    mcp=$(docker compose "${files[@]}" ps -q mcp)
    for ctr in "$bridge" "$mcp"; do
      bindings=$(docker inspect -f '{{json .HostConfig.PortBindings}}' "$ctr")
      [ "$bindings" = null ] || [ "$bindings" = '{}' ]
    done
    # Prove distinct namespace and operator/proxy membership from the real containers.
    docker inspect "$bridge" "$mcp" | docker run --rm -i "$image" python -c '
import json,sys
b,m=json.load(sys.stdin)
assert b["NetworkSettings"]["SandboxKey"] != m["NetworkSettings"]["SandboxKey"]
bn,mn=set(b["NetworkSettings"]["Networks"]),set(m["NetworkSettings"]["Networks"])
assert not any(n.endswith("-proxy") for n in bn)
assert not any(n.endswith("-operator") for n in mn)
assert any(n.endswith("_agent") for n in bn & mn)
print("distinct namespaces; bridge off proxy; MCP off operator; shared agent -> PASS")'
    agent_net="$COMPOSE_PROJECT_NAME-agent"
    # Compose names networks with underscores; inspect the actual configured agent.
    agent_net=$(docker inspect -f '{{range $n,$v := .NetworkSettings.Networks}}{{println $n}}{{end}}' "$bridge" | tr -d '\r' | grep '_agent$')
    [ "$(docker network inspect -f '{{.Internal}}' "$agent_net")" = true ]
    docker run -d --name "$run-whisper-$instance" --network "${COMPOSE_PROJECT_NAME}_default" \
      --network-alias whisper "$image" python -m http.server 8178 >/dev/null
    docker exec -i -e PROBE_ROLE=mcp -e PROBE_INSTANCE "$mcp" python - < scripts/split-probe.py
    docker exec "$bridge" sh -c 'test "$(id -u)" = 1000 && test "$(cat /app/outbox/.uploads/split-smoke.txt)" = "$1"' sh "$instance"
    stores+=("$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/app/store"}}{{.Name}}{{end}}{{end}}' "$bridge")")
    outboxes+=("$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/app/outbox"}}{{.Name}}{{end}}{{end}}' "$bridge")")
    tokens+=("$(docker exec "$mcp" python -c 'import hashlib,whatsapp;print(hashlib.sha256(whatsapp._bridge_headers()["Authorization"].encode()).hexdigest())')")
  done
  [ "${stores[0]}" != "${stores[1]}" ] && [ "${outboxes[0]}" != "${outboxes[1]}" ] && [ "${tokens[0]}" != "${tokens[1]}" ]
  echo "$mode: two stores, bridge tokens and named outboxes distinct; no host ports -> PASS"
  if [ "$mode" = proxy ] || [ "$mode" = combined ]; then
    docker run --rm -i --network "$WHATSAPP_PROXY_NETWORK" \
      -e PROBE_ALIAS_A -e PROBE_ALIAS_B -e PROBE_TOKEN_A -e PROBE_TOKEN_B \
      "$image" python - < scripts/proxy-probe.py
  fi
  if [ "$mode" = operator ] || [ "$mode" = combined ]; then
    docker run --rm -i --network "$WHATSAPP_OPERATOR_NETWORK" -e PROBE_ROLE=operator \
      -e OPERATOR_ALIAS_A -e OPERATOR_ALIAS_B -e OPERATOR_PORT_A -e OPERATOR_PORT_B \
      -e WHATSAPP_OPERATOR_TOKEN "$image" python - < scripts/split-probe.py
  fi
  for instance in a b; do
    select_instance "$instance"
    docker rm -f "$run-whisper-$instance" >/dev/null
    docker compose "${files[@]}" down -v --remove-orphans
  done
done
echo 'split smoke -> PASS (four real topology combinations, two instances each, unpaired)'
