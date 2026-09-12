"""scripts/smoke.sh drives the two stack shapes we deploy.

The script is the post-deploy check an operator runs by hand (AGENTS.md section 4,
step 11) and CI runs against the real compose stack. Here it runs against fake
`docker` and `curl` executables on PATH, which is enough to pin the branching:
the compose-in-this-directory path CI depends on, and the managed-stack path
(Komodo, Portainer) where `./.env` is root-owned and everything has to come from
the containers.
"""

from __future__ import annotations

import os
import re
import shutil
import stat
import subprocess
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
SMOKE = ROOT / "scripts" / "smoke.sh"

BASH = shutil.which("bash")
pytestmark = pytest.mark.skipif(BASH is None, reason="needs bash to run scripts/smoke.sh")

FAKE_DOCKER = r"""#!/usr/bin/env bash
printf '%s\n' "$*" >>"$FAKE_DOCKER_LOG"

if [ "$1" = "compose" ] && [ "$2" = "ps" ]; then
  case "$*" in
    *" -q "*) printf '%s\n' "${FAKE_BRIDGE_ID:-}" ;;   # compose ps -q bridge
    *) printf '%s' "${FAKE_COMPOSE_PS:-}" ;;
  esac
  exit 0
fi
if [ "$1" = "compose" ] && [ "$2" = "ls" ]; then
  printf '%s\n' "${FAKE_COMPOSE_LS:-[]}"
  exit 0
fi
if [ "$1" = "ps" ]; then
  case "$*" in
    *com.docker.compose.service=bridge*)  printf '%s\n' "${FAKE_BRIDGE_CTR:-}" ;;
    *com.docker.compose.service=mcp*)     printf '%s\n' "${FAKE_MCP_CTR:-}" ;;
    *com.docker.compose.service=whisper*) printf '%s\n' "${FAKE_WHISPER_CTR:-}" ;;
    *) printf '  bridge: running (Up 3 minutes)\n  mcp: running (Up 3 minutes)\n' ;;
  esac
  exit 0
fi
if [ "$1" = "inspect" ]; then   # inspect -f <template> <container>
  case "$3" in
    *.Id*)
      # The bridge itself, or whatever whisper's namespace points at: an id that
      # no longer exists is an error, like the real docker.
      case "$4" in
        "${FAKE_BRIDGE_CTR:-@no-bridge@}"|"${FAKE_BRIDGE_ID:-@no-bridge@}")
          printf '%s\n' "${FAKE_BRIDGE_ID:-}" ;;
        *)
          [ -n "${FAKE_NETNS_RESOLVES_TO:-}" ] || { echo "No such object: $4" >&2; exit 1; }
          printf '%s\n' "$FAKE_NETNS_RESOLVES_TO" ;;
      esac ;;
    *HostConfig.NetworkMode*)       printf '%s\n' "${FAKE_WHISPER_NETNS:-}" ;;
    *com.docker.compose.project*)   printf '%s\n' "${FAKE_COMPOSE_PROJECT:-}" ;;
    *) echo "fake docker: unexpected inspect template: $3" >&2; exit 99 ;;
  esac
  exit 0
fi
if [ "$1" = "port" ]; then
  [ -n "${FAKE_PUBLISHED_PORT:-}" ] || exit 1
  printf '0.0.0.0:%s\n' "$FAKE_PUBLISHED_PORT"
  exit 0
fi

if [ "$1" = "compose" ] && [ "$2" = "exec" ]; then
  shift 4            # compose exec -T <service>
elif [ "$1" = "exec" ]; then
  shift 2            # exec <container>
else
  echo "fake docker: unexpected command: $*" >&2
  exit 99
fi

cmd="$1"; shift
case "$cmd" in
  printenv)
    case "$1" in
      WHATSAPP_BRIDGE_TOKEN)      value="${FAKE_ENV_BRIDGE_TOKEN:-}" ;;
      WHATSAPP_MCP_TOKEN)         value="${FAKE_ENV_MCP_TOKEN:-}" ;;
      WHATSAPP_MCP_METRICS)       value="${FAKE_ENV_MCP_METRICS:-}" ;;
      WHATSAPP_MCP_METRICS_TOKEN) value="${FAKE_ENV_MCP_METRICS_TOKEN:-}" ;;
      WHISPER_URL)                value="${FAKE_ENV_WHISPER_URL:-}" ;;
      *) value="" ;;
    esac
    [ -n "$value" ] || exit 1
    printf '%s\n' "$value"
    exit 0 ;;
  cat)
    [ -n "${FAKE_TOKEN_FILE:-}" ] || exit 1
    printf '%s\n' "$FAKE_TOKEN_FILE"
    exit 0 ;;
  python)   # the whisper probe, run inside the mcp container
    [ "${FAKE_WHISPER_REACHABLE:-yes}" = "yes" ] || {
      echo "<urlopen error [Errno 111] Connection refused>"; exit 1; }
    exit 0 ;;
  wget)
    case "$*" in
      *"Bearer ${FAKE_EXPECT_TOKEN}"*) ;;
      *) echo "wget: server returned error: HTTP/1.1 401 Unauthorized" >&2; exit 1 ;;
    esac
    for url in "$@"; do :; done
    case "$url" in
      */api/health) printf '%s\n' "${FAKE_HEALTH_BODY}"; exit 0 ;;
      */api/ready)
        [ "${FAKE_READY:-200}" = "200" ] && { echo ok; exit 0; }
        echo "wget: server returned error: HTTP/1.1 ${FAKE_READY} Service Unavailable" >&2
        exit 1 ;;
      *) echo "fake docker: unexpected url: $url" >&2; exit 99 ;;
    esac ;;
  *) echo "fake docker: unexpected exec: $cmd $*" >&2; exit 99 ;;
esac
"""

FAKE_CURL = r"""#!/usr/bin/env bash
printf '%s\n' "$*" >>"$FAKE_CURL_LOG"
out=/dev/null; hdr=/dev/null; url=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -D) hdr="$2"; shift 2 ;;
    -H|-d|-w) shift 2 ;;
    -*) shift ;;
    *) url="$1"; shift ;;
  esac
done
case "$url" in
  */metrics)
    code="${FAKE_METRICS_CODE:-200}"
    if [ "$code" = "200" ]; then
      printf 'whatsapp_mcp_uptime_seconds 12\nwhatsapp_mcp_tool_calls_total 3\n' >"$out"
    else
      : >"$out"
    fi
    printf '%s' "$code"; exit 0 ;;
  */mcp)
    code="${FAKE_INIT_CODE:-200}"
    if [ "$code" = "200" ]; then
      printf 'HTTP/1.1 200 OK\r\nmcp-session-id: 0123\r\n\r\n' >"$hdr"
      printf '{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"whatsapp","version":"9.9.9"}}}' >"$out"
    else
      : >"$hdr"; : >"$out"
    fi
    printf '%s' "$code"; exit 0 ;;
  *) echo "fake curl: unexpected url: $url" >&2; exit 99 ;;
esac
"""

HEALTHY = '{"status":"ok","connected":true,"paired":true}'
UNPAIRED = '{"status":"awaiting_pairing","connected":false,"paired":false}'


class Stack:
    """A copy of smoke.sh in tmp_path plus fake docker/curl on PATH."""

    def __init__(self, tmp_path: Path) -> None:
        self.dir = tmp_path
        self.script = tmp_path / "smoke.sh"
        self.script.write_bytes(SMOKE.read_bytes())
        bindir = tmp_path / "bin"
        bindir.mkdir()
        for name, body in (("docker", FAKE_DOCKER), ("curl", FAKE_CURL)):
            path = bindir / name
            path.write_text(body, encoding="utf-8", newline="\n")
            path.chmod(path.stat().st_mode | stat.S_IEXEC | stat.S_IXGRP | stat.S_IXOTH)
        self.docker_log = tmp_path / "docker.log"
        self.curl_log = tmp_path / "curl.log"
        self.env = {
            **os.environ,
            "PATH": str(bindir) + os.pathsep + os.environ.get("PATH", ""),
            # tmp_path is not a repository; make sure the script does not find one above it.
            "GIT_CEILING_DIRECTORIES": str(tmp_path),
            "FAKE_DOCKER_LOG": str(self.docker_log),
            "FAKE_CURL_LOG": str(self.curl_log),
            "FAKE_HEALTH_BODY": HEALTHY,
            "FAKE_EXPECT_TOKEN": "bridge-token-0123456789",
        }
        for leaked in (
            "WHATSAPP_BRIDGE_TOKEN",
            "WHATSAPP_MCP_TOKEN",
            "WHATSAPP_MCP_METRICS",
            "WHISPER_URL",
        ):
            self.env.pop(leaked, None)

    def run(self, *args: str, **fakes: str) -> subprocess.CompletedProcess[str]:
        assert BASH is not None
        return subprocess.run(
            [BASH, str(self.script), *args],
            cwd=self.dir,
            env={**self.env, **fakes},
            capture_output=True,
            text=True,
            timeout=60,  # a flag parsed wrong used to spin forever
        )

    def docker_calls(self) -> str:
        return self.docker_log.read_text(encoding="utf-8") if self.docker_log.exists() else ""

    def curl_calls(self) -> str:
        return self.curl_log.read_text(encoding="utf-8") if self.curl_log.exists() else ""


@pytest.fixture
def stack(tmp_path: Path) -> Stack:
    return Stack(tmp_path)


def test_compose_mode_still_reads_dot_env(stack: Stack) -> None:
    """The path CI exercises: a stack in this directory, token from ./.env."""
    (stack.dir / ".env").write_text(
        "WHATSAPP_BRIDGE_TOKEN=bridge-token-0123456789\nWHATSAPP_MCP_PORT=8443\n",
        encoding="utf-8",
        newline="\n",
    )
    result = stack.run(FAKE_COMPOSE_PS="  bridge: running healthy\n  mcp: running healthy\n")

    assert result.returncode == 0, result.stdout + result.stderr
    assert "using WHATSAPP_BRIDGE_TOKEN from the environment" in result.stdout
    assert "http://127.0.0.1:8443/mcp" in stack.curl_calls()  # port from .env
    assert "compose exec -T bridge wget" in stack.docker_calls()
    assert not re.search(r"^(exec|port) ", stack.docker_calls(), re.M)  # no container-name path


def test_compose_mode_unpaired_exits_2(stack: Stack) -> None:
    """CI asserts exit 2 on the unpaired stack; keep that contract."""
    result = stack.run(
        FAKE_COMPOSE_PS="  bridge: running healthy\n",
        FAKE_HEALTH_BODY=UNPAIRED,
        FAKE_READY="503",
        FAKE_ENV_BRIDGE_TOKEN="bridge-token-0123456789",
    )

    assert result.returncode == 2, result.stdout + result.stderr
    assert "not paired yet" in result.stdout


def test_project_mode_talks_to_the_containers(stack: Stack) -> None:
    """--project: no ./.env at all, the bridge token comes from the container."""
    result = stack.run(
        "--project",
        "whatsapp-mcp",
        "--url",
        "https://box.tailnet.ts.net",
        FAKE_BRIDGE_CTR="whatsapp-mcp-bridge-1",
        FAKE_MCP_CTR="whatsapp-mcp-mcp-1",
        FAKE_ENV_BRIDGE_TOKEN="bridge-token-0123456789",
        FAKE_ENV_MCP_TOKEN="mcp-token-from-container",
    )

    assert result.returncode == 0, result.stdout + result.stderr
    assert "using WHATSAPP_BRIDGE_TOKEN from the bridge container" in result.stdout
    calls = stack.docker_calls()
    assert "exec whatsapp-mcp-bridge-1 wget" in calls
    assert "compose ps" not in calls  # never touches the unreadable directory
    assert "Bearer mcp-token-from-container" in stack.curl_calls()


def test_project_mode_falls_back_to_the_token_file(stack: Stack) -> None:
    result = stack.run(
        "--project",
        "whatsapp-mcp",
        FAKE_BRIDGE_CTR="whatsapp-mcp-bridge-1",
        FAKE_MCP_CTR="whatsapp-mcp-mcp-1",
        FAKE_TOKEN_FILE="bridge-token-0123456789",
    )

    assert result.returncode == 0, result.stdout + result.stderr
    assert "store/.bridge-token" in result.stdout
    assert "exec whatsapp-mcp-bridge-1 cat /app/store/.bridge-token" in stack.docker_calls()


def test_project_mode_reports_a_missing_bridge_container(stack: Stack) -> None:
    result = stack.run("--project", "whatsapp-mcp", FAKE_BRIDGE_CTR="")

    assert result.returncode == 1
    assert "no running bridge container" in result.stdout


def test_mcp_token_flag_wins(stack: Stack) -> None:
    result = stack.run(
        "--project",
        "whatsapp-mcp",
        "--mcp-token",
        "token-from-the-flag",
        FAKE_BRIDGE_CTR="whatsapp-mcp-bridge-1",
        FAKE_MCP_CTR="whatsapp-mcp-mcp-1",
        FAKE_ENV_BRIDGE_TOKEN="bridge-token-0123456789",
        FAKE_ENV_MCP_TOKEN="mcp-token-from-container",
    )

    assert result.returncode == 0, result.stdout + result.stderr
    assert "Bearer token-from-the-flag" in stack.curl_calls()


@pytest.mark.parametrize("matching_first", [True, False])
def test_autodetects_the_single_matching_project(stack: Stack, matching_first: bool) -> None:
    """No stack in this directory (root-owned .env, or a plain clone): find the project.

    The matching project is tried in both array positions: the name must come from
    the object that matched, not from the last one in the list.
    """
    ours = '{"Name":"whatsapp","Status":"running(2)","ConfigFiles":"/etc/komodo/stacks/whatsapp-mcp/compose.yaml"}'
    other = '{"Name":"media","Status":"running(2)","ConfigFiles":"/etc/komodo/stacks/media/compose.yaml"}'
    listing = [ours, other] if matching_first else [other, ours]
    result = stack.run(
        FAKE_COMPOSE_PS="",
        FAKE_COMPOSE_LS="[" + ",".join(listing) + "]",
        FAKE_BRIDGE_CTR="whatsapp-bridge-1",
        FAKE_MCP_CTR="whatsapp-mcp-1",
        FAKE_ENV_BRIDGE_TOKEN="bridge-token-0123456789",
    )

    assert result.returncode == 0, result.stdout + result.stderr
    assert "using the running compose project 'whatsapp'" in result.stdout
    assert "exec whatsapp-bridge-1 wget" in stack.docker_calls()


def test_ambiguous_projects_ask_for_the_flag(stack: Stack) -> None:
    result = stack.run(
        FAKE_COMPOSE_PS="",
        FAKE_COMPOSE_LS=(
            '[{"Name":"whatsapp-mcp","ConfigFiles":"/etc/komodo/stacks/whatsapp-mcp/compose.yaml"},'
            '{"Name":"whatsapp-mcp-staging","ConfigFiles":"/srv/whatsapp-mcp/compose.yaml"}]'
        ),
    )

    assert result.returncode == 1
    assert "--project" in result.stdout


def test_no_stack_anywhere_still_says_what_to_run(stack: Stack) -> None:
    result = stack.run(FAKE_COMPOSE_PS="", FAKE_COMPOSE_LS="[]")

    assert result.returncode == 1
    assert "docker compose reports no containers" in result.stdout


def test_metrics_404_is_a_warning_not_a_failure(stack: Stack) -> None:
    """Tailscale Serve can map /mcp only; that must not fail the deploy check."""
    result = stack.run(
        "--project",
        "whatsapp-mcp",
        "--url",
        "https://box.tailnet.ts.net",
        FAKE_BRIDGE_CTR="whatsapp-mcp-bridge-1",
        FAKE_MCP_CTR="whatsapp-mcp-mcp-1",
        FAKE_ENV_BRIDGE_TOKEN="bridge-token-0123456789",
        FAKE_METRICS_CODE="404",
    )

    assert result.returncode == 0, result.stdout + result.stderr
    assert "warning: 404" in result.stdout
    assert "FAIL" not in result.stdout


@pytest.mark.parametrize("disabled", ["false", "off", "0", "no", "FALSE"])
def test_metrics_disabled_in_the_container_is_skipped(stack: Stack, disabled: str) -> None:
    """Same off switches as observability.metrics_enabled, so a 404 is not misread."""
    result = stack.run(
        "--project",
        "whatsapp-mcp",
        FAKE_BRIDGE_CTR="whatsapp-mcp-bridge-1",
        FAKE_MCP_CTR="whatsapp-mcp-mcp-1",
        FAKE_ENV_BRIDGE_TOKEN="bridge-token-0123456789",
        FAKE_ENV_MCP_METRICS=disabled,
    )

    assert result.returncode == 0, result.stdout + result.stderr
    assert f"skipped (WHATSAPP_MCP_METRICS={disabled})" in result.stdout


def test_metrics_failure_still_fails(stack: Stack) -> None:
    result = stack.run(
        "--project",
        "whatsapp-mcp",
        FAKE_BRIDGE_CTR="whatsapp-mcp-bridge-1",
        FAKE_MCP_CTR="whatsapp-mcp-mcp-1",
        FAKE_ENV_BRIDGE_TOKEN="bridge-token-0123456789",
        FAKE_METRICS_CODE="502",
    )

    assert result.returncode == 1
    assert "/metrics -> 502" in result.stdout


def test_project_mode_uses_the_published_port_when_no_url_is_given(stack: Stack) -> None:
    """.env holds WHATSAPP_MCP_PORT and is unreachable here; docker knows the mapping."""
    result = stack.run(
        "--project",
        "whatsapp-mcp",
        FAKE_BRIDGE_CTR="whatsapp-mcp-bridge-1",
        FAKE_MCP_CTR="whatsapp-mcp-mcp-1",
        FAKE_ENV_BRIDGE_TOKEN="bridge-token-0123456789",
        FAKE_PUBLISHED_PORT="8443",
    )

    assert result.returncode == 0, result.stdout + result.stderr
    assert "http://127.0.0.1:8443/mcp" in stack.curl_calls()


def test_project_mode_warns_about_a_missing_mcp_container(stack: Stack) -> None:
    result = stack.run(
        "--project",
        "whatsapp-mcp",
        "--url",
        "https://box.tailnet.ts.net",
        FAKE_BRIDGE_CTR="whatsapp-mcp-bridge-1",
        FAKE_MCP_CTR="",
        FAKE_ENV_BRIDGE_TOKEN="bridge-token-0123456789",
    )

    assert result.returncode == 0, result.stdout + result.stderr
    assert "no running mcp container" in result.stdout


BRIDGE_ID = "b" * 64


def _with_whisper(stack: Stack, **fakes: str) -> subprocess.CompletedProcess[str]:
    """A managed stack whose whisper sidecar is up (the `whisper` profile)."""
    return stack.run(
        "--project",
        "whatsapp-mcp",
        "--url",
        "https://box.tailnet.ts.net",
        FAKE_BRIDGE_CTR="whatsapp-mcp-bridge-1",
        FAKE_MCP_CTR="whatsapp-mcp-mcp-1",
        FAKE_WHISPER_CTR="whatsapp-mcp-whisper-1",
        FAKE_BRIDGE_ID=BRIDGE_ID,
        FAKE_ENV_BRIDGE_TOKEN="bridge-token-0123456789",
        **fakes,
    )


def test_orphaned_whisper_names_the_recreate_command(stack: Stack) -> None:
    """A redeploy without the profile leaves whisper in the old bridge namespace.

    The namespace names a container docker no longer knows, so the fake refuses
    to inspect it, exactly like the real one.
    """
    result = _with_whisper(stack, FAKE_WHISPER_NETNS="container:" + "d" * 64)

    assert result.returncode == 1, result.stdout + result.stderr
    assert "whisper is attached to a bridge container that no longer exists" in result.stdout
    assert "docker compose --profile whisper up -d --force-recreate whisper" in result.stdout
    assert "python" not in stack.docker_calls()  # no point probing a dead namespace
    # An optional sidecar must not swallow the checks the operator ran this for.
    assert "4. mcp initialize" in result.stdout


def test_whisper_in_the_bridge_namespace_is_probed_from_the_mcp_container(stack: Stack) -> None:
    result = _with_whisper(stack, FAKE_WHISPER_NETNS="container:" + BRIDGE_ID)

    assert result.returncode == 0, result.stdout + result.stderr
    assert "5. whisper" in result.stdout
    assert "reachable on 127.0.0.1:8178 (whatsapp-mcp-whisper-1)" in result.stdout
    assert "exec whatsapp-mcp-mcp-1 python" in stack.docker_calls()


@pytest.mark.parametrize("target", ["whatsapp-mcp-bridge-1", BRIDGE_ID[:12]])
def test_whisper_attached_by_name_or_short_id_is_resolved_through_docker(stack: Stack, target: str) -> None:
    """`container:<name>` and `container:<short id>` both name the live bridge."""
    result = _with_whisper(
        stack,
        FAKE_WHISPER_NETNS="container:" + target,
        FAKE_NETNS_RESOLVES_TO=BRIDGE_ID,
    )

    assert result.returncode == 0, result.stdout + result.stderr
    assert "FAIL" not in result.stdout
    assert "reachable on 127.0.0.1:8178" in result.stdout


def test_unreachable_whisper_fails(stack: Stack) -> None:
    """The profile is active, so silence on 8178 is a broken deployment."""
    result = _with_whisper(
        stack,
        FAKE_WHISPER_NETNS="container:" + BRIDGE_ID,
        FAKE_WHISPER_REACHABLE="no",
    )

    assert result.returncode == 1, result.stdout + result.stderr
    assert "does not answer on 127.0.0.1:8178" in result.stdout
    assert "Connection refused" in result.stdout


def test_no_whisper_container_skips_the_check(stack: Stack) -> None:
    """The shape CI smokes: compose stack in this directory, no whisper profile."""
    (stack.dir / ".env").write_text("WHATSAPP_BRIDGE_TOKEN=bridge-token-0123456789\n", encoding="utf-8", newline="\n")
    result = stack.run(
        FAKE_COMPOSE_PS="  bridge: running healthy\n  mcp: running healthy\n",
        FAKE_BRIDGE_ID=BRIDGE_ID,
        FAKE_COMPOSE_PROJECT="whatsapp-mcp",
        FAKE_WHISPER_CTR="",
    )

    assert result.returncode == 0, result.stdout + result.stderr
    assert "5. whisper" not in result.stdout
    calls = stack.docker_calls()
    assert "compose ps -q bridge" in calls  # the project name comes off the bridge
    assert "python" not in calls


def test_whisper_configured_but_not_running_is_reported(stack: Stack) -> None:
    """WHISPER_URL kept, container gone: transcription is dead just the same."""
    result = stack.run(
        "--project",
        "whatsapp-mcp",
        "--url",
        "https://box.tailnet.ts.net",
        FAKE_BRIDGE_CTR="whatsapp-mcp-bridge-1",
        FAKE_MCP_CTR="whatsapp-mcp-mcp-1",
        FAKE_BRIDGE_ID=BRIDGE_ID,
        FAKE_ENV_BRIDGE_TOKEN="bridge-token-0123456789",
        FAKE_WHISPER_CTR="",
        FAKE_ENV_WHISPER_URL="http://127.0.0.1:8178/inference",
    )

    assert result.returncode == 0, result.stdout + result.stderr
    assert "5. whisper" in result.stdout
    assert "runs no whisper container" in result.stdout
    assert "COMPOSE_PROFILES=whisper" in result.stdout


@pytest.mark.parametrize("flag", ["--project", "--url", "--wait", "--mcp-token"])
def test_a_flag_without_its_value_exits_instead_of_spinning(stack: Stack, flag: str) -> None:
    result = stack.run(flag)

    assert result.returncode == 1
    assert f"{flag} needs a value" in result.stderr


# A whisper shared between stacks (docker-compose.whisper.yml) is not a container
# of this project: step 5 follows WHISPER_URL to it instead of assuming the sidecar.


def _with_shared_whisper(stack: Stack, **fakes: str) -> subprocess.CompletedProcess[str]:
    """A managed stack whose WHISPER_URL names the shared server, no sidecar here."""
    return stack.run(
        "--project",
        "whatsapp-mcp",
        "--url",
        "https://box.tailnet.ts.net",
        FAKE_BRIDGE_CTR="whatsapp-mcp-bridge-1",
        FAKE_MCP_CTR="whatsapp-mcp-mcp-1",
        FAKE_BRIDGE_ID=BRIDGE_ID,
        FAKE_ENV_BRIDGE_TOKEN="bridge-token-0123456789",
        FAKE_WHISPER_CTR="",
        FAKE_ENV_WHISPER_URL="http://whisper:8178/inference",
        **fakes,
    )


def test_shared_whisper_is_probed_at_its_url(stack: Stack) -> None:
    result = _with_shared_whisper(stack)

    assert result.returncode == 0, result.stdout + result.stderr
    assert "5. whisper" in result.stdout
    assert "reachable on whisper:8178 (shared" in result.stdout
    assert "runs no whisper container" not in result.stdout
    calls = stack.docker_calls()
    assert "exec whatsapp-mcp-mcp-1 python" in calls
    assert " whisper:8178\n" in calls  # the address is an argument of the probe, not baked into it


def test_shared_whisper_down_fails_the_check(stack: Stack) -> None:
    """A URL that names a server nobody answers at is a broken deployment, not a skip."""
    result = _with_shared_whisper(stack, FAKE_WHISPER_REACHABLE="no")

    assert result.returncode == 1, result.stdout + result.stderr
    assert "the shared whisper does not answer on whisper:8178" in result.stdout
    assert "Connection refused" in result.stdout
    assert "docker-compose.whisper.yml" in result.stdout


def test_compose_mode_follows_the_shared_url_from_dot_env(stack: Stack) -> None:
    (stack.dir / ".env").write_text(
        "WHATSAPP_BRIDGE_TOKEN=bridge-token-0123456789\nWHISPER_URL=http://whisper:8178/inference\n",
        encoding="utf-8",
        newline="\n",
    )
    result = stack.run(
        FAKE_COMPOSE_PS="  bridge: running healthy\n  mcp: running healthy\n",
        FAKE_BRIDGE_ID=BRIDGE_ID,
        FAKE_COMPOSE_PROJECT="whatsapp-mcp",
        FAKE_WHISPER_CTR="",
    )

    assert result.returncode == 0, result.stdout + result.stderr
    assert "reachable on whisper:8178 (shared" in result.stdout
    assert "compose exec -T mcp python" in stack.docker_calls()


def test_sidecar_left_running_while_the_url_points_at_the_shared_server(stack: Stack) -> None:
    """The profile was not dropped when the stack moved to the shared whisper: say so."""
    result = _with_whisper(
        stack,
        FAKE_WHISPER_NETNS="container:" + BRIDGE_ID,
        FAKE_ENV_WHISPER_URL="http://whisper:8178/inference",
    )

    assert result.returncode == 0, result.stdout + result.stderr
    assert (
        "reachable on whisper:8178 (shared; whatsapp-mcp-whisper-1 in this project is not the one in use)"
        in result.stdout
    )
    assert " 127.0.0.1:8178\n" not in stack.docker_calls()
