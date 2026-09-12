"""docker-compose.whisper.yml (one whisper shared by several stacks) stays in
step with the `whisper` profile in docker-compose.yml, and the override that
attaches a stack to it touches nothing but the bridge's networks.

Text-based on purpose: the repo has no YAML dependency, and the lines compared
are the ones an operator copies between the two files."""

from __future__ import annotations

import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]

STACK = (ROOT / "docker-compose.yml").read_text(encoding="utf-8")
SHARED = (ROOT / "docker-compose.whisper.yml").read_text(encoding="utf-8")
OVERRIDE = (ROOT / "docker-compose.shared-whisper.yml").read_text(encoding="utf-8")


def _one(pattern: str, text: str) -> str:
    found = re.findall(pattern, text, flags=re.M)
    assert len(found) == 1, f"expected one match for {pattern!r}, got {found}"
    return found[0]


def test_same_pinned_image():
    stack = _one(r"^\s+image: (ghcr\.io/ggml-org/whisper\.cpp:\S+)$", STACK)
    shared = _one(r"^\s+image: (ghcr\.io/ggml-org/whisper\.cpp:\S+)$", SHARED)
    assert stack == shared, "bump the whisper.cpp digest in both compose files"
    assert "@sha256:" in shared


def test_same_server_command_except_the_bind():
    stack = _one(r"^\s+exec /app/build/bin/whisper-server .*$", STACK).strip()
    shared = _one(r"^\s+exec /app/build/bin/whisper-server .*$", SHARED).strip()
    assert "--host 127.0.0.1" in stack, "the in-stack sidecar stays loopback-only"
    assert "--host 0.0.0.0" in shared, "the shared server listens on its private network"
    assert stack.replace("--host 127.0.0.1", "--host 0.0.0.0") == shared


def test_same_knobs_and_limits():
    for knob in ("WHISPER_MODEL_NAME", "WHISPER_LANGUAGE", "WHISPER_THREADS", "WHISPER_MEM_LIMIT", "WHISPER_CPUS"):
        # WHISPER_LANGUAGE is passed to the mcp service too, with the same default.
        stack = set(re.findall(rf"^\s+\S+: (\$\{{{knob}:-[^}}]*\}})$", STACK, flags=re.M))
        shared = set(re.findall(rf"^\s+\S+: (\$\{{{knob}:-[^}}]*\}})$", SHARED, flags=re.M))
        assert len(stack) == 1 and stack == shared, (
            f"{knob} default differs between the two compose files: {stack} vs {shared}"
        )


def test_shared_server_publishes_no_port_and_owns_the_network():
    assert not re.search(r"^\s+ports:", SHARED, flags=re.M), "whisper-server has no auth: never publish it"
    assert "name: whisper-shared" in SHARED
    assert "network_mode" not in SHARED


def test_override_only_attaches_the_bridge():
    services = re.findall(r"^  ([a-z]+):$", OVERRIDE, flags=re.M)
    assert services == ["bridge"], f"the override must touch only the bridge service, got {services}"
    assert re.search(r"^\s+- default$", OVERRIDE, flags=re.M), "keep the bridge on its own default network too"
    assert re.search(r"^\s+- whisper-shared$", OVERRIDE, flags=re.M)
    assert "external: true" in OVERRIDE
    code = "\n".join(line for line in OVERRIDE.splitlines() if not line.lstrip().startswith("#"))
    assert "WHISPER_URL" not in code, "WHISPER_URL comes from .env, like every other knob"


def test_docs_and_env_example_point_at_the_shared_file():
    for path in ("docs/DOCKER.md", ".env.example"):
        text = (ROOT / path).read_text(encoding="utf-8")
        assert "docker-compose.whisper.yml" in text, f"{path} does not mention the shared whisper"
        assert "docker-compose.shared-whisper.yml" in text, f"{path} does not mention the stack override"
