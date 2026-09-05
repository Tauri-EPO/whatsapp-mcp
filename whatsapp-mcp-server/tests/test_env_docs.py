"""Every environment variable the code reads is documented, everywhere it should be.

AGENTS.md section 5 asks for four places per variable: AGENTS.md section 7,
docs/CONFIGURATION.md, .env.example and (when a container needs it) the compose
passthrough. This test turns that rule into a check so a new knob cannot drift.
"""

from __future__ import annotations

import re
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]

PREFIXES = ("WHATSAPP_", "WHATSMEOW_", "WHISPER_", "WEBHOOK_", "FFMPEG_", "FORWARD_SELF")

# Consumed by compose / the shell only; no process reads them, so AGENTS.md
# section 7 lists them in its footnote instead of the table.
COMPOSE_ONLY = {
    "WHATSAPP_MCP_BIND",
    "WHATSAPP_OUTBOX",
    "WHATSAPP_IMAGE_TAG",
    "WHATSAPP_IMAGE_REGISTRY",
    "WHISPER_MODEL_NAME",
    "WHISPER_THREADS",
    "WHISPER_MEM_LIMIT",
    "WHISPER_CPUS",
}
# Set by the runtime or the image, never by an operator.
INTERNAL = {"WHATSAPP_MCP_VERSION"}


def _read(rel: str) -> str:
    return (ROOT / rel).read_text(encoding="utf-8")


def code_variables() -> set[str]:
    """Names a process actually reads: os.getenv-style calls, *Env constants, getEnvBool, whisper get()."""
    found: set[str] = set()
    py_patterns = [
        r'os\.(?:getenv|environ\.get)\(\s*"([A-Z0-9_]+)"',
        r'os\.environ\[\s*"([A-Z0-9_]+)"',
        r'^[A-Z_]*ENV(?:_[A-Z]+)?\s*=\s*"([A-Z0-9_]+)"',  # JSON_FORMAT_ENV = "..."
        r'get\(\s*"(WHISPER_[A-Z0-9_]+)"',  # transcribe.load_config
        r'install_stdio_parent_watchdog\(\s*"([A-Z0-9_]+)"',
    ]
    for py in (ROOT / "whatsapp-mcp-server").glob("*.py"):
        text = py.read_text(encoding="utf-8")
        for pat in py_patterns:
            found |= set(re.findall(pat, text, flags=re.M))
    go_patterns = [
        r'os\.(?:Getenv|LookupEnv)\(\s*"([A-Z0-9_]+)"',
        r'getEnv\w*\(\s*"([A-Z0-9_]+)"',  # getEnvBool("WEBHOOK_ENABLED", true)
        r'^\s*(?:const\s+)?\w*[eE]nv\w*\s*=\s*"([A-Z0-9_]+)"',  # const logLevelEnv = "..."
    ]
    for go in (ROOT / "whatsapp-bridge").glob("*.go"):
        if go.name.endswith("_test.go"):
            continue
        text = go.read_text(encoding="utf-8")
        for pat in go_patterns:
            found |= set(re.findall(pat, text, flags=re.M))
    return {v for v in found if v.startswith(PREFIXES)} - INTERNAL


def _first_column_names(markdown: str) -> set[str]:
    """Backticked names in the first cell of every table row (handles `A` / `B` cells)."""
    names: set[str] = set()
    for cell in re.findall(r"^\| ([^|]+)\|", markdown, flags=re.M):
        names |= set(re.findall(r"`([A-Z][A-Z0-9_]+)`", cell))
    return names


def agents_table() -> set[str]:
    text = _read("AGENTS.md")
    section = text.split("## 7. Environment variables", 1)[1].split("\n## 8.", 1)[0]
    return _first_column_names(section)


def configuration_doc() -> set[str]:
    return _first_column_names(_read("docs/CONFIGURATION.md"))


def env_example() -> set[str]:
    return set(re.findall(r"^#?\s*([A-Z][A-Z0-9_]+)=", _read(".env.example"), flags=re.M))


def compose_passthrough() -> set[str]:
    return set(re.findall(r"^\s+([A-Z][A-Z0-9_]+):\s", _read("docker-compose.yml"), flags=re.M))


def test_code_variables_are_documented_in_agents_and_configuration():
    code = code_variables()
    assert len(code) >= 30, code  # sanity: the regexes still find the knobs
    missing_agents = sorted(code - agents_table() - COMPOSE_ONLY)
    missing_config = sorted(code - configuration_doc() - COMPOSE_ONLY)
    assert not missing_agents, f"read by code but absent from AGENTS.md section 7: {missing_agents}"
    assert not missing_config, f"read by code but absent from docs/CONFIGURATION.md: {missing_config}"


def test_documented_variables_exist_in_code_and_env_example():
    code = code_variables()
    documented = agents_table() | configuration_doc()
    stale = sorted(v for v in documented if v.startswith(PREFIXES) and v not in code and v not in COMPOSE_ONLY)
    assert not stale, f"documented but no process reads them: {stale}"
    missing_example = sorted(v for v in documented | COMPOSE_ONLY if v.startswith(PREFIXES) and v not in env_example())
    assert not missing_example, f"missing from .env.example: {missing_example}"


def test_compose_passes_through_what_containers_need():
    compose = compose_passthrough()
    # Every variable compose hands to a container is read by some process
    # (or is a documented compose-only knob); otherwise it is dead config.
    unknown = sorted(v for v in compose if v.startswith(PREFIXES) and v not in code_variables() | COMPOSE_ONLY)
    assert not unknown, f"compose passes variables no process reads: {unknown}"


@pytest.mark.parametrize("name", sorted(COMPOSE_ONLY))
def test_compose_only_knobs_are_used_by_compose_or_env(name):
    text = _read("docker-compose.yml") + _read(".env.example")
    assert name in text, f"{name} is listed as compose-only but neither compose nor .env.example mentions it"
