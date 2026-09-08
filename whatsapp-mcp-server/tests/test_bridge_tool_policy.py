"""The bridge's tool -> endpoint map stays in step with the MCP tool names.

``WHATSAPP_ALLOW_TOOLS`` / ``WHATSAPP_DENY_TOOLS`` are written once and handed to
both containers, so both sides must accept exactly the same names: the bridge
stops at startup on a name it does not know (whatsapp-bridge/tool_policy.go), and
a tool renamed or added here without touching that file would turn a valid list
into a startup failure — or, worse, leave a new mutating endpoint unclassified.
"""

from __future__ import annotations

import re
from pathlib import Path

import main
from tool_policy import mutating_tools, registered_tool_names

GO_SOURCE = Path(__file__).resolve().parents[2] / "whatsapp-bridge" / "tool_policy.go"


def _go_block(name: str) -> str:
    """The body of a top-level `var <name> = ...{ ... }` declaration."""
    text = GO_SOURCE.read_text(encoding="utf-8")
    match = re.search(rf"^var {name} = [^{{]*\{{\n(.*?)^\}}", text, flags=re.M | re.S)
    assert match, f"{name} not found in {GO_SOURCE.name}"
    return match.group(1)


def _quoted(block: str) -> set[str]:
    return set(re.findall(r'"([^"]+)"', block))


def go_endpoint_tools() -> dict[str, set[str]]:
    """endpointTools: REST path -> the tool names that call it."""
    mapping: dict[str, set[str]] = {}
    for path, tools in re.findall(r'"(/api/[^"]+)":\s*\{([^}]*)\}', _go_block("endpointTools")):
        mapping[path] = _quoted(tools)
    return mapping


def go_unenforced_tools() -> set[str]:
    return _quoted(_go_block("unenforcedTools"))


def test_bridge_knows_exactly_the_registered_tool_names():
    mapped = set().union(*go_endpoint_tools().values())
    assert mapped | go_unenforced_tools() == set(registered_tool_names(main.mcp))


def test_endpoint_map_covers_every_mutating_tool():
    """A tool with a WhatsApp side effect reaches it through one of these paths."""
    mapped = set().union(*go_endpoint_tools().values())
    assert mapped == set(mutating_tools())


def test_unenforced_tools_have_no_endpoint():
    assert not (go_unenforced_tools() & set(mutating_tools()))


def test_send_tools_share_one_endpoint():
    """The bridge check is endpoint-granular; this is the case that shows it."""
    assert go_endpoint_tools()["/api/send"] == {"send_message", "send_file", "send_audio_message"}


def test_the_download_endpoint_is_not_endpoint_enforced():
    """`/api/download` is a read endpoint: it stays open however the lists are set.

    Denying `download_media` removes the tool and the implicit fetches
    `read_media` / `transcribe_audio` make (media_read.py, main.py), but the
    bridge must keep serving `/api/download`, exactly as it keeps serving
    `/api/poll` in read-only mode — a per-endpoint 403 there would take the
    fetch away from tools the same list still allows (issue #350).
    """
    assert "/api/download" not in go_endpoint_tools()
    assert {"download_media", "read_media", "transcribe_audio"} <= go_unenforced_tools()
