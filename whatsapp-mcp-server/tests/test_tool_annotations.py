"""Every registered tool carries complete MCP annotations (issue #851).

Connector directories refuse a server whose tools lack a ``title`` and the
read-only / destructive hints, and a client without them falls back to the spec
defaults (destructive, open world). The check walks the tools the server really
registered, so a tool added without annotations fails here, and it stays in step
with the enforced policy: an annotation may be more cautious than
``@mutating_tool``, never less.

The second half covers the split of ``get_group_invite_link``: the read keeps the
old name and is annotated read-only, revoking the link is
``reset_group_invite_link``, and allow/deny lists naming the old tool keep the
power they had before the split.
"""

from __future__ import annotations

import json
from typing import Any

import pytest
from mcp.server.mcpserver import MCPServer
from mcp_types import ToolAnnotations

import main
import tool_policy
import whatsapp
from tool_policy import READ_ONLY_ENV, ToolPolicy, mutating_tools, registered_tool_names

# Mutating in the policy sense yet annotated read-only, each with its reason.
READ_ONLY_DESPITE_POLICY = {
    # Only reads the current link; read-only mode still blocks it because a
    # link is group access that can leak (tool_policy.py docstring).
    "get_group_invite_link",
}


def annotation_problems(server: Any) -> dict[str, list[str]]:
    """Tool name -> what its annotations are missing; empty when all are complete."""
    problems: dict[str, list[str]] = {}
    for tool in server._tool_manager.list_tools():
        ann: ToolAnnotations | None = tool.annotations
        missing: list[str] = []
        if ann is None:
            missing.append("annotations")
        else:
            if not (ann.title or "").strip():
                missing.append("title")
            if ann.read_only_hint is None:
                missing.append("readOnlyHint")
            if ann.read_only_hint is False:
                if ann.destructive_hint is None:
                    missing.append("destructiveHint")
                if ann.idempotent_hint is None:
                    missing.append("idempotentHint")
            if ann.open_world_hint is None:
                missing.append("openWorldHint")
        if missing:
            problems[tool.name] = missing
    return problems


def _annotations() -> dict[str, ToolAnnotations]:
    return {tool.name: tool.annotations for tool in main.mcp._tool_manager.list_tools() if tool.annotations is not None}


def test_every_registered_tool_is_fully_annotated():
    assert len(registered_tool_names(main.mcp)) >= 60
    assert annotation_problems(main.mcp) == {}


def test_the_checker_catches_a_tool_without_annotations():
    """The guard itself: a tool added bare, or half-annotated, is reported."""
    server = MCPServer("annotation-check")

    def bare() -> str:
        return ""

    def half() -> str:
        return ""

    def complete() -> str:
        return ""

    server.add_tool(bare)
    server.add_tool(half, annotations=ToolAnnotations(title="Half", read_only_hint=False))
    server.add_tool(complete, annotations=main._reads("Complete", open_world=False))
    assert annotation_problems(server) == {
        "bare": ["annotations"],
        "half": ["destructiveHint", "idempotentHint", "openWorldHint"],
    }


def test_mutating_tools_are_never_annotated_read_only():
    annotations = _annotations()
    wrong = sorted(
        name
        for name in mutating_tools()
        if annotations[name].read_only_hint is not False and name not in READ_ONLY_DESPITE_POLICY
    )
    assert wrong == []
    # The exception list stays exact: each entry is mutating and annotated read-only.
    for name in READ_ONLY_DESPITE_POLICY:
        assert name in mutating_tools()
        assert annotations[name].read_only_hint is True


def test_local_writes_are_not_annotated_read_only():
    """Notes, triage state and exports write local state the user relies on."""
    annotations = _annotations()
    for name in (
        "annotate",
        "annotate_media",
        "compact",
        "clear_media_refusal",
        "mark_handled",
        "snooze",
        "export_messages",
        "download_media",  # Returns a persisted path callers can depend on, not just bytes.
        "transcribe_audio",
    ):
        assert annotations[name].read_only_hint is False, name


def test_sends_are_destructive_and_open_world():
    """A sent message cannot be taken back from the recipient."""
    annotations = _annotations()
    for name in ("send_message", "send_file", "send_audio_message", "forward_message"):
        ann = annotations[name]
        assert (ann.read_only_hint, ann.destructive_hint, ann.open_world_hint) == (False, True, True), name


@pytest.mark.parametrize(
    "name",
    [
        "export_messages",  # No out_path: each later call can create another dated file.
        "snooze",  # Repeated values still add note versions and refresh the triage anchor.
        "transcribe_audio",  # force=True repeats metered inference and transcript writes.
    ],
)
def test_repeatable_writes_do_not_claim_idempotency(name):
    assert _annotations()[name].idempotent_hint is False


def test_annotations_reach_the_wire_in_camel_case():
    wire = _annotations()["reset_group_invite_link"].model_dump(by_alias=True, exclude_none=True)
    assert wire == {
        "title": "Reset group invite link",
        "readOnlyHint": False,
        "destructiveHint": True,
        "idempotentHint": False,
        "openWorldHint": True,
    }


# --- get_group_invite_link / reset_group_invite_link ---------------------------

GROUP = "120363000000000001@g.us"


@pytest.fixture(autouse=True)
def _reset_active_policy():
    tool_policy.set_active_policy(ToolPolicy())
    yield
    tool_policy.set_active_policy(None)


class _Reply:
    status_code = 200
    text = ""

    def json(self) -> dict[str, Any]:
        return {"success": True, "link": "https://chat.whatsapp.com/ExampleInvite"}


@pytest.fixture
def bridge_calls(monkeypatch, auth_runtime_store):
    """Bridge POSTs recorded instead of sent; the permission store is the empty fixture one."""
    calls: list[tuple[str, dict[str, Any]]] = []

    def fake_post(url: str, json: Any = None, headers: Any = None, timeout: Any = None) -> _Reply:
        calls.append((url.rsplit("/api", 1)[-1], json))
        return _Reply()

    monkeypatch.setattr(whatsapp.bridge_http, "post", fake_post)
    monkeypatch.setattr(whatsapp, "_read_bridge_token", lambda: "t" * 32)
    return calls


def test_the_read_has_no_reset_argument():
    tool = main.mcp._tool_manager.get_tool("get_group_invite_link")
    assert tool is not None
    assert set(tool.parameters["properties"]) == {"chat_jid"}


def test_the_read_never_resets(bridge_calls):
    assert main.get_group_invite_link(GROUP)["link"].startswith("https://chat.whatsapp.com/")
    assert bridge_calls == [("/group/invite", {"group_jid": GROUP, "reset": False})]


def test_the_reset_tool_resets(bridge_calls):
    assert main.reset_group_invite_link(GROUP)["success"] is True
    assert bridge_calls == [("/group/invite", {"group_jid": GROUP, "reset": True})]


async def test_old_reset_argument_points_to_the_new_tool(bridge_calls):
    result = await main.mcp.call_tool("get_group_invite_link", {"chat_jid": GROUP, "reset": True})
    assert result.is_error is True
    error = json.loads(result.content[0].text)["error"]
    assert error["code"] == "invalid_argument"
    assert "reset_group_invite_link" in error["message"]
    assert bridge_calls == []


@pytest.mark.parametrize(
    ("policy", "read", "reset"),
    [
        # Lists written before the split keep exactly their power.
        (ToolPolicy(deny=frozenset({"get_group_invite_link"})), False, False),
        (ToolPolicy(allow=frozenset({"get_group_invite_link"})), True, True),
        (ToolPolicy(allow=frozenset({"list_chats"})), False, False),
        # The new name narrows only the reset.
        (ToolPolicy(deny=frozenset({"reset_group_invite_link"})), True, False),
        (ToolPolicy(allow=frozenset({"reset_group_invite_link"})), False, True),
        # Read-only blocks both, whatever the allow-list says.
        (ToolPolicy(read_only=True, allow=frozenset({"get_group_invite_link"})), False, False),
    ],
)
def test_lists_naming_the_old_tool_bind_the_new_one(policy, read, reset):
    assert policy.allows("get_group_invite_link") is read
    assert policy.allows("reset_group_invite_link") is reset


def test_old_deny_refuses_the_reset_call(bridge_calls):
    tool_policy.set_active_policy(ToolPolicy(deny=frozenset({"get_group_invite_link"})))
    error = main.reset_group_invite_link(GROUP)["error"]
    assert error["code"] == "denied"
    assert "get_group_invite_link" in error["message"]
    assert bridge_calls == []


def test_read_only_refuses_both_calls(bridge_calls):
    tool_policy.set_active_policy(ToolPolicy(read_only=True))
    for call in (main.get_group_invite_link, main.reset_group_invite_link):
        error = call(GROUP)["error"]
        assert error["code"] == "denied" and READ_ONLY_ENV in error["message"]
    assert bridge_calls == []
    assert {"get_group_invite_link", "reset_group_invite_link"} <= set(ToolPolicy(read_only=True).blocked())


def test_the_new_name_is_a_valid_list_entry():
    ToolPolicy(deny=frozenset({"reset_group_invite_link"})).validate(registered_tool_names(main.mcp))
