"""The runtime receiver's allowed_chats is a webhook-only list (issue #846).

The MCP side sees only its mode and entry count, and the list never restricts
what the tools list or read: the bridge keeps it in process memory, outside
messages.db and the chat policy every tool applies.
"""

import json
from contextlib import closing

import httpx
import pytest

import main
import tool_policy
import whatsapp
from tests.conftest import ALICE, FAMILY
from tests.test_webhook_tools import VIEW
from tool_policy import ToolPolicy

PRIVATE = ("120363000000000001", "5511999999999", "@g.us", "@s.whatsapp.net")


@pytest.fixture(autouse=True)
def isolated(monkeypatch):
    monkeypatch.setattr(tool_policy, "_active", ToolPolicy())
    monkeypatch.setattr(tool_policy, "_runtime", False)
    monkeypatch.delenv(main.STATUS_DETAIL_ENV, raising=False)
    monkeypatch.delenv("WHATSAPP_ALLOWED_CHATS", raising=False)
    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "fake-bridge-token-issue-846")


def serve(monkeypatch, view):
    def fake(url, **_kwargs):
        if url.endswith("/health"):
            return httpx.Response(200, json={"paired": True, "connected": True, "webhook": view})
        if url.endswith("/webhook"):
            return httpx.Response(200, json=view)
        return httpx.Response(503, json={})

    monkeypatch.setattr(whatsapp.bridge_http, "get", fake)


@pytest.mark.parametrize(
    "bridge,want",
    [
        ({"mode": "list", "entries": 2}, {"mode": "list", "entries": 2}),
        ({"mode": "all", "entries": 0}, {"mode": "all", "entries": 0}),
        ({"mode": "list", "entries": 0}, {"mode": "list", "entries": 0}),
        # Anything beyond the mode and count is rebuilt away, never passed on.
        ({"mode": "list", "entries": 1, "jids": [FAMILY]}, {"mode": "list", "entries": 1}),
        ({"mode": FAMILY, "entries": 1}, None),
        ({"mode": "list", "entries": [FAMILY]}, None),
        ({"mode": "list", "entries": "1"}, None),
        ({"mode": "list", "entries": True}, None),
        ({"mode": "list", "entries": -1}, None),
        ([FAMILY], None),
        (FAMILY, None),
    ],
)
def test_views_carry_mode_and_count_only(monkeypatch, bridge, want):
    serve(monkeypatch, {**VIEW, "allowed_chats": bridge})
    monkeypatch.setenv(main.STATUS_DETAIL_ENV, "true")
    for view in (main.get_webhook(), main.bridge_status(detail=True)["webhook"]):
        assert view.get("allowed_chats") == want
        assert {k: v for k, v in view.items() if k != "allowed_chats"} == VIEW
        assert not any(value in json.dumps(view) for value in PRIVATE)


def test_compact_views_omit_the_list(monkeypatch):
    serve(monkeypatch, {**VIEW, "allowed_chats": {"mode": "list", "entries": 2}})
    assert "allowed_chats" not in main.bridge_status()["webhook"]
    monkeypatch.setenv(main.STATUS_DETAIL_ENV, "false")
    assert "allowed_chats" not in main.get_webhook()
    assert "allowed_chats" not in main.bridge_status(detail=True)["webhook"]


def test_older_bridge_without_the_field_still_works(monkeypatch):
    serve(monkeypatch, VIEW)
    assert main.get_webhook() == VIEW


def test_tools_still_list_and_read_chats_outside_allowed_chats(monkeypatch, paired_dbs):
    with closing(paired_dbs.messages()) as conn, conn:
        conn.executemany(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?, ?, ?, ?, ?, 0)",
            [
                ("fake-in", ALICE, "5511999999999", "inside the receiver list", "2026-09-04 10:00:00"),
                ("fake-out", FAMILY, "5511999999999", "outside the receiver list", "2026-09-04 10:00:00"),
            ],
        )
    # The receiver delivers only one chat; the agent still sees every chat.
    serve(monkeypatch, {**VIEW, "allowed_chats": {"mode": "list", "entries": 1}})
    assert main.get_webhook()["allowed_chats"] == {"mode": "list", "entries": 1}
    chats = {row["jid"] for row in main.list_chats()["items"]}
    assert {ALICE, FAMILY} <= chats
    contents = [row["content"] for row in main.list_messages(chat_jid=FAMILY, include_context=False)["items"]]
    assert contents == ["outside the receiver list"]
    assert main.get_chat(FAMILY)["jid"] == FAMILY
