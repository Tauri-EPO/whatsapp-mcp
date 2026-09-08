#!/usr/bin/env bash
# Post-deploy smoke test for the compose stack.
#
#   scripts/smoke.sh                 # check the stack described by ./.env
#   scripts/smoke.sh --wait 90       # poll up to 90 s for the bridge to come up first
#   scripts/smoke.sh --url https://box.tailnet.ts.net   # MCP endpoint as clients see it
#   scripts/smoke.sh --project whatsapp-mcp             # stack owned by a manager (Komodo,
#                                    #   Portainer): reach the containers by compose label
#                                    #   instead of reading ./.env, root-owned over there
#   scripts/smoke.sh --mcp-token XYZ # MCP bearer token when ./.env is unreadable; the
#                                    #   value shows up in `ps`, so prefer
#                                    #   WHATSAPP_MCP_TOKEN=... scripts/smoke.sh ...
#
# Steps, in order:
#   1. bridge  GET /api/health   (inside the container: the bridge listens on loopback)
#   2. bridge  GET /api/ready    200 = paired and connected, 503 = waiting for the QR scan
#   3. mcp     GET /metrics      (skipped when WHATSAPP_MCP_METRICS=false; a 404 is a
#                                warning, not a failure: a proxy may route /mcp only)
#   4. mcp     POST /mcp initialize with the bearer token -> 200 + mcp-session-id
#   5. whisper on 127.0.0.1:8178 from inside the mcp container (skipped when this
#                                deployment has no whisper sidecar and no WHISPER_URL)
#
# Without --project the script drives `docker compose` in the repository directory.
# With it -- or when that directory has no usable stack -- it resolves the containers
# through `docker ps --filter label=com.docker.compose.project=<name>` and reads the
# tokens from the container environment, so a root-owned .env is never needed.
#
# Exit codes: 0 everything answers, 2 stack is up but not paired yet (scan the QR
# in `docker compose logs -f bridge`), 1 something is broken (the failing step
# says what to look at). Needs docker and curl on the host.
set -uo pipefail

cd "$(git rev-parse --show-toplevel 2>/dev/null || dirname "$0")/." || exit 1

WAIT=0
URL=""
PROJECT=""
MCP_TOKEN_ARG=""
# `shift 2` on a flag given without its value shifts nothing and returns 1, which
# would spin this loop forever: ask for the value explicitly.
need_value() { [ "$2" -ge 2 ] || { echo "$1 needs a value" >&2; exit 1; }; }
while [ $# -gt 0 ]; do
  case "$1" in
    --wait) need_value "$1" $#; WAIT="$2"; shift 2 ;;
    --url) need_value "$1" $#; URL="$2"; shift 2 ;;
    --project) need_value "$1" $#; PROJECT="$2"; shift 2 ;;
    --mcp-token) need_value "$1" $#; MCP_TOKEN_ARG="$2"; shift 2 ;;
    -h|--help) awk 'NR>1 && /^#/ {sub(/^# ?/, ""); print; next} NR>1 {exit}' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 1 ;;
  esac
done

# .env is KEY=VALUE lines (see .env.example); export them for the defaults below.
# A stack deployed by Komodo/Portainer keeps it root-owned: skip it there and read
# the tokens from the containers instead (--project, or the fallback below).
if [ -z "$PROJECT" ] && [ -r .env ]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env
  set +a
elif [ -z "$PROJECT" ] && [ -e .env ]; then
  echo "note: ./.env is not readable; reading the tokens from the containers instead"
fi
URL="${URL%/}"   # the default needs the containers; it is built after "containers"

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
step()  { printf '\n== %s\n' "$*"; }
fail()  { red "FAIL: $1"; [ -n "${2:-}" ] && echo "      $2"; exit 1; }

compose() { docker compose "$@"; }

# MODE=compose: `docker compose` here reads ./.env and ./docker-compose.yml.
# MODE=project: the stack belongs to a manager; reach the containers by label.
MODE=compose
BRIDGE_CTR=""
MCP_CTR=""

container_of() { # $1 service, $2 project (default $PROJECT) -> container name, empty when not running
  docker ps --filter "label=com.docker.compose.project=${2:-$PROJECT}" \
            --filter "label=com.docker.compose.service=$1" \
            --format '{{.Names}}' 2>/dev/null | head -n1
}

inspect_f() { # $1 container, $2 Go template -> the value, empty when it cannot be read
  [ -n "$1" ] || return 0
  docker inspect -f "$2" "$1" 2>/dev/null </dev/null | tr -d '\r\n'
}

detect_project() { # names of the running compose projects that look like this stack
  # `docker compose ls --format json` prints one JSON array of flat objects:
  # one object per line (portably, no sed newline escapes), then keep the ones
  # whose name or config path mentions whatsapp-mcp.
  docker compose ls --format json 2>/dev/null \
    | grep -o '{[^{}]*}' \
    | grep -i 'whatsapp-mcp' \
    | sed -n 's/.*"Name":"\([^"]*\)".*/\1/p'
}

published_port() { # host port mapped to container port $1, empty when unmapped
  local ctr port
  for ctr in "$MCP_CTR" "$BRIDGE_CTR"; do   # compose puts the mcp in the bridge netns
    [ -n "$ctr" ] || continue
    port=$(docker port "$ctr" "$1/tcp" 2>/dev/null | sed -n 's/.*:\([0-9]\{1,5\}\)$/\1/p' | head -n1)
    [ -n "$port" ] && { printf '%s' "$port"; return 0; }
  done
}

bridge_exec() { # run a command inside the bridge container
  if [ "$MODE" = "project" ]; then
    docker exec "$BRIDGE_CTR" "$@"
  else
    compose exec -T bridge "$@"
  fi
}

mcp_exec() { # run a command inside the mcp container
  if [ "$MODE" = "project" ]; then
    docker exec "$MCP_CTR" "$@"
  else
    compose exec -T mcp "$@"
  fi
}

mcp_env() { # $1 variable name -> its value inside the mcp container (project mode only)
  [ "$MODE" = "project" ] && [ -n "$MCP_CTR" ] || return 0
  docker exec "$MCP_CTR" printenv "$1" 2>/dev/null </dev/null | tr -d '\r\n'
}

bridge_get() { # $1 path -> body in BRIDGE_BODY, HTTP status in BRIDGE_STATUS
  # Results go through globals on purpose: `x=$(bridge_get ...)` would run the
  # function in a subshell and lose the status.
  # busybox wget: exit 0 with the body on a 2xx; on other statuses it prints
  # "server returned error: HTTP/1.1 503 Service Unavailable" and exits 1.
  local out rc
  out=$(bridge_exec wget -qO- --header "Authorization: Bearer ${TOKEN}" "http://127.0.0.1:8080$1" 2>&1 </dev/null)
  rc=$?
  BRIDGE_BODY=""
  if [ "$rc" -eq 0 ]; then
    BRIDGE_STATUS=200
    BRIDGE_BODY="$out"
  else
    BRIDGE_STATUS=$(printf '%s\n' "$out" | sed -n 's/.*HTTP\/[0-9.]* \([0-9][0-9][0-9]\).*/\1/p' | tail -n1)
    [ -n "$BRIDGE_STATUS" ] || BRIDGE_STATUS="no response ($(printf '%s' "$out" | head -c 120))"
  fi
}

step "containers"
ps_out=""
if [ -n "$PROJECT" ]; then
  MODE=project
elif ps_out=$(compose ps --format '  {{.Service}}: {{.State}} {{.Health}}' 2>/dev/null) &&
     printf '%s' "$ps_out" | grep -q '[^[:space:]]'; then
  : # a stack of our own in this directory
else
  # Nothing here: either docker compose could not read this directory (root-owned
  # .env) or the stack lives elsewhere. Fall back to the one running compose
  # project that looks like ours.
  candidates=$(detect_project)
  found=$(printf '%s\n' "$candidates" | grep -c '[^[:space:]]')
  if [ "$found" = "1" ]; then
    PROJECT=$(printf '%s\n' "$candidates" | head -n1)
    MODE=project
    echo "  no stack in $(pwd); using the running compose project '$PROJECT'"
  elif [ "$found" = "0" ]; then
    fail "docker compose reports no containers" "run: docker compose up -d --build, or point at a managed stack: scripts/smoke.sh --project <name>"
  else
    fail "several compose projects match whatsapp-mcp" "pick one: scripts/smoke.sh --project <name> ($(printf '%s' "$candidates" | tr '\n' ' '))"
  fi
fi
if [ "$MODE" = "project" ]; then
  BRIDGE_CTR=$(container_of bridge)
  [ -n "$BRIDGE_CTR" ] || fail "no running bridge container in compose project '$PROJECT'" "docker ps --filter label=com.docker.compose.project=$PROJECT"
  MCP_CTR=$(container_of mcp)
  [ -n "$MCP_CTR" ] || echo "  warning: no running mcp container in project '$PROJECT' (steps 3 and 4 will only reach it if it runs elsewhere)"
  docker ps --filter "label=com.docker.compose.project=$PROJECT" \
    --format '  {{.Label "com.docker.compose.service"}}: {{.State}} ({{.Status}})'
else
  printf '%s\n' "$ps_out"
fi

# The optional `whisper` sidecar joins the bridge's network namespace, which
# Docker resolves to a container id when whisper is created. A `docker compose
# up` whose whisper profile is not active recreates the bridge and never touches
# whisper: it stays Up, attached to a namespace whose container is gone, and
# nothing reaches it again (issue #415). Classify it here, where the containers
# are resolved; step 5 reports it, after the checks this was run for.
WHISPER_CTR=""
WHISPER_STATE=absent   # absent | attached | orphaned | elsewhere | unknown
WHISPER_DETAIL=""
if [ "$MODE" = "project" ]; then
  BRIDGE_ID=$(inspect_f "$BRIDGE_CTR" '{{.Id}}')
  WHISPER_CTR=$(container_of whisper)
else
  # Compose mode spells the project name nowhere: read it off the bridge
  # container, whose id the namespace comparison needs anyway.
  BRIDGE_ID=$(compose ps -q bridge 2>/dev/null | head -n1)
  compose_project=$(inspect_f "$BRIDGE_ID" '{{index .Config.Labels "com.docker.compose.project"}}')
  [ -n "$compose_project" ] && WHISPER_CTR=$(container_of whisper "$compose_project")
fi
if [ -n "$WHISPER_CTR" ]; then
  whisper_netns=$(inspect_f "$WHISPER_CTR" '{{.HostConfig.NetworkMode}}')
  whisper_target=${whisper_netns#container:}
  if [ -z "$whisper_netns" ] || [ -z "$BRIDGE_ID" ]; then
    WHISPER_STATE=unknown
    WHISPER_DETAIL="could not read the network mode of $WHISPER_CTR, or the bridge container id"
  elif [ "$whisper_netns" = "$whisper_target" ]; then
    WHISPER_STATE=elsewhere
    WHISPER_DETAIL="$WHISPER_CTR runs with network mode '$whisper_netns', not the bridge namespace"
  elif [ "$(inspect_f "$whisper_target" '{{.Id}}')" = "$BRIDGE_ID" ]; then
    # Resolved through docker, so an id, a short id or a name all work; a
    # namespace whose container is gone inspects to nothing and fails here.
    WHISPER_STATE=attached
  else
    WHISPER_STATE=orphaned
    WHISPER_DETAIL="$WHISPER_CTR -> $whisper_netns, current bridge ${BRIDGE_ID:0:12}"
  fi
fi

# ./.env carries WHATSAPP_MCP_PORT in compose mode; in project mode it is out of
# reach and the published port is a compose mapping, not a container variable, so
# ask docker for it (the mcp shares the bridge's network namespace).
if [ -z "$URL" ]; then
  if [ "$MODE" = "project" ]; then
    MCP_PORT=$(published_port 8000)
  fi
  URL="http://127.0.0.1:${MCP_PORT:-${WHATSAPP_MCP_PORT:-8000}}"
fi

step "bridge token"
TOKEN="${WHATSAPP_BRIDGE_TOKEN:-}"
if [ -n "$TOKEN" ]; then
  echo "  using WHATSAPP_BRIDGE_TOKEN from the environment"
else
  TOKEN=$(bridge_exec printenv WHATSAPP_BRIDGE_TOKEN 2>/dev/null </dev/null | tr -d '\r\n')
  if [ -n "$TOKEN" ]; then
    echo "  using WHATSAPP_BRIDGE_TOKEN from the bridge container"
  else
    TOKEN=$(bridge_exec cat /app/store/.bridge-token 2>/dev/null </dev/null | tr -d '\r\n')
    [ -n "$TOKEN" ] || fail "no WHATSAPP_BRIDGE_TOKEN and no /app/store/.bridge-token yet" "is the bridge running? docker compose logs bridge"
    echo "  using the token generated by the bridge (store/.bridge-token)"
  fi
fi

step "1. bridge /api/health"
deadline=$(( $(date +%s) + WAIT ))
while :; do
  bridge_get /api/health
  body="$BRIDGE_BODY"
  [ "${BRIDGE_STATUS:-}" = "200" ] && break
  if [ "$(date +%s)" -ge "$deadline" ]; then
    fail "bridge /api/health answered '${BRIDGE_STATUS:-no response}'" "docker compose logs --tail 50 bridge"
  fi
  sleep 3
done
echo "  $body"
case "$body" in
  *'"status":"ok"'*) PAIRED=yes ;;
  *) PAIRED=no ;;
esac

step "2. bridge /api/ready"
bridge_get /api/ready
case "${BRIDGE_STATUS:-}" in
  200) green "  connected to WhatsApp" ;;
  503) echo "  503: not connected (unpaired, or reconnecting)"; PAIRED=no ;;
  *) fail "unexpected /api/ready status '${BRIDGE_STATUS:-none}'" ;;
esac

step "3. mcp /metrics ($URL/metrics)"
# In project mode ./.env is out of reach: ask the container for the two knobs.
[ -n "${WHATSAPP_MCP_METRICS:-}" ] || WHATSAPP_MCP_METRICS=$(mcp_env WHATSAPP_MCP_METRICS)
[ -n "${WHATSAPP_MCP_METRICS_TOKEN:-}" ] || WHATSAPP_MCP_METRICS_TOKEN=$(mcp_env WHATSAPP_MCP_METRICS_TOKEN)
# Same off switches as the server (observability.metrics_enabled): 0/false/off/no.
case "$(printf '%s' "${WHATSAPP_MCP_METRICS:-true}" | tr '[:upper:]' '[:lower:]')" in
  0|false|off|no) metrics_off=yes ;;
  *) metrics_off=no ;;
esac
if [ "$metrics_off" = "yes" ]; then
  echo "  skipped (WHATSAPP_MCP_METRICS=${WHATSAPP_MCP_METRICS:-false})"
else
  while :; do
    # WHATSAPP_MCP_METRICS_TOKEN (optional) gates /metrics on its own bearer.
    metrics_auth=()
    [ -n "${WHATSAPP_MCP_METRICS_TOKEN:-}" ] && metrics_auth=(-H "Authorization: Bearer ${WHATSAPP_MCP_METRICS_TOKEN}")
    # curl already prints 000 as %{http_code} when it cannot connect; assign, do
    # not append, or the status becomes "000000" and no branch below matches it.
    code=$(curl -sS -o /tmp/wamcp-metrics.$$ -w '%{http_code}' "${metrics_auth[@]}" "$URL/metrics" 2>/dev/null) || code="${code:-000}"
    # 404: the endpoint answers but does not route /metrics, so waiting is pointless.
    if [ "$code" = "200" ] || [ "$code" = "404" ] || [ "$(date +%s)" -ge "$deadline" ]; then break; fi
    sleep 3 # the mcp container starts after the bridge is healthy
  done
  if [ "$code" = "200" ] && grep -q '^whatsapp_mcp_uptime_seconds' /tmp/wamcp-metrics.$$; then
    green "  ok ($(grep -c '^whatsapp_mcp_' /tmp/wamcp-metrics.$$) series)"
  elif [ "$code" = "404" ]; then
    echo "  warning: 404. Either this endpoint routes /mcp only (a Tailscale Serve mapping does that: scrape the MCP port itself) or the MCP server has metrics disabled."
  else
    rm -f /tmp/wamcp-metrics.$$
    fail "GET $URL/metrics -> $code" "is the MCP port published (WHATSAPP_MCP_BIND / WHATSAPP_MCP_PORT) and the mcp container healthy?"
  fi
  rm -f /tmp/wamcp-metrics.$$
fi

step "4. mcp initialize ($URL/mcp)"
MCP_TOKEN="${MCP_TOKEN_ARG:-${WHATSAPP_MCP_TOKEN:-}}"
[ -n "$MCP_TOKEN" ] || MCP_TOKEN=$(mcp_env WHATSAPP_MCP_TOKEN)
MCP_TOKEN="${MCP_TOKEN:-$TOKEN}"
hdrs=/tmp/wamcp-init-h.$$
body=/tmp/wamcp-init-b.$$
while :; do
  code=$(curl -sS -o "$body" -D "$hdrs" -w '%{http_code}' \
    -H "Authorization: Bearer ${MCP_TOKEN}" \
    -H 'Accept: application/json, text/event-stream' -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}' \
    "$URL/mcp" 2>/dev/null) || code="${code:-000}"
  [ "$code" != "000" ] || [ "$(date +%s)" -ge "$deadline" ] && break
  sleep 3
done
case "$code" in
  200)
    if grep -qi '^mcp-session-id:' "$hdrs"; then
      version=$(tr -d '\n' <"$body" | sed -n 's/.*"serverInfo":{[^}]*"version":"\([^"]*\)".*/\1/p')
      green "  ok (server version ${version:-unknown})"
    else
      rm -f "$hdrs" "$body"; fail "200 without mcp-session-id header" "is $URL really the MCP server?"
    fi ;;
  401) rm -f "$hdrs" "$body"; fail "401 Unauthorized" "token mismatch: pass --mcp-token, or line up WHATSAPP_MCP_TOKEN (the bridge token when unset) with what the mcp container loaded; restart mcp after changing it" ;;
  421) rm -f "$hdrs" "$body"; fail "421 Misdirected Request" "the Host you used (${URL#*://}) is not in WHATSAPP_MCP_ALLOWED_HOSTS" ;;
  000) rm -f "$hdrs" "$body"; fail "no answer from $URL/mcp" "port not published, wrong URL, or the mcp container is down (docker compose logs --tail 50 mcp)" ;;
  *) rm -f "$hdrs" "$body"; fail "unexpected status $code from $URL/mcp" ;;
esac
rm -f "$hdrs" "$body"

# The mcp container reaches whisper over the shared namespace, so the compose
# profile is the only thing that can serve this address. Nothing configured and
# no container: this deployment does not transcribe, and step 5 is skipped.
[ -n "${WHISPER_URL:-}" ] || WHISPER_URL=$(mcp_env WHISPER_URL)
case "${WHISPER_URL:-}" in
  *//127.0.0.1:8178/*|*//localhost:8178/*) whisper_expected=yes ;;
  *) whisper_expected=no ;;
esac

if [ "$WHISPER_STATE" != "absent" ] || [ "$whisper_expected" = "yes" ]; then
  step "5. whisper"
  case "$WHISPER_STATE" in
    orphaned)
      fail "whisper is attached to a bridge container that no longer exists; run: docker compose --profile whisper up -d --force-recreate whisper" \
           "$WHISPER_DETAIL" ;;
    absent)
      echo "  warning: WHISPER_URL is $WHISPER_URL but this stack runs no whisper container; the profile was dropped (COMPOSE_PROFILES=whisper)" ;;
    elsewhere|unknown)
      echo "  skipped: $WHISPER_DETAIL" ;;
    *)
      if [ "$MODE" = "project" ] && [ -z "$MCP_CTR" ]; then
        echo "  skipped: no mcp container to probe from"
      else
        # The mcp image is python:3.13-slim: python is the only HTTP client it
        # has. Any HTTP answer proves the server is there; whisper-server has no
        # health route, so the status code itself says nothing.
        reached=no
        while :; do
          if out=$(mcp_exec python -c 'import sys, urllib.error, urllib.request

try:
    urllib.request.urlopen("http://127.0.0.1:8178/", timeout=5)
except urllib.error.HTTPError:
    pass
except Exception as exc:
    print(exc)
    sys.exit(1)
' 2>&1 </dev/null); then
            reached=yes
            break
          fi
          # A first start downloads the ggml model before it listens at all.
          [ "$(date +%s)" -ge "$deadline" ] && break
          sleep 3
        done
        if [ "$reached" = "yes" ]; then
          green "  reachable on 127.0.0.1:8178 ($WHISPER_CTR)"
        else
          fail "whisper is running but does not answer on 127.0.0.1:8178 (${out:-no output})" \
               "a first start downloads the ggml model: docker compose logs --tail 50 whisper"
        fi
      fi ;;
  esac
fi

echo
if [ "$PAIRED" = "yes" ]; then
  green "All good: bridge paired and connected, MCP endpoint answering."
  exit 0
fi
echo "Stack is up but WhatsApp is not paired yet: run 'docker compose logs -f bridge' and scan the QR code."
exit 2
