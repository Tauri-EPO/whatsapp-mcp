"""delete_messages_for_me: dry run from messages.db, confirmed payload, refusals."""

import sqlite3

import pytest

import main
import tool_policy
import whatsapp
from chat_policy import ChatPolicy
from tool_policy import ToolPolicy

pytestmark = pytest.mark.usefixtures("auth_runtime_store")

CHAT = "5511999999999@s.whatsapp.net"


class Response:
    def __init__(self, payload, status=200):
        self.status_code = status
        self.text = ""
        self._payload = payload

    def json(self):
        return self._payload


@pytest.fixture
def archive(tmp_path, monkeypatch):
    path = tmp_path / "messages.db"
    conn = sqlite3.connect(path)
    conn.execute(
        "CREATE TABLE messages (id TEXT, chat_jid TEXT, is_from_me BOOLEAN, timestamp TEXT, media_type TEXT, "
        "PRIMARY KEY (id, chat_jid))"
    )
    conn.executemany(
        "INSERT INTO messages VALUES (?, ?, ?, ?, ?)",
        [
            ("IN1", CHAT, 0, "2026-01-01 12:00:00+00:00", None),
            ("OUT1", CHAT, 1, "2026-01-01 12:01:00+00:00", "image"),
            ("ELSE", "5511888888888@s.whatsapp.net", 0, "2026-01-01 12:00:00+00:00", None),
        ],
    )
    conn.commit()
    conn.close()
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    return path


def test_dry_run_reads_archive_and_sends_nothing(monkeypatch, archive):
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda *a, **kw: pytest.fail("bridge called"))
    result = main.delete_messages_for_me(CHAT, ["IN1", "OUT1", "ELSE", "NOPE"])
    assert result["dry_run"] is True
    assert [row["message_id"] for row in result["would_delete"]] == ["IN1", "OUT1"]
    assert result["would_delete"][1]["is_from_me"] is True
    assert result["not_found_in_this_chat_spelling"] == ["ELSE", "NOPE"]


def test_confirmed_payload(monkeypatch):
    seen = []
    monkeypatch.setattr(whatsapp, "_read_bridge_token", lambda: "t" * 32)
    monkeypatch.setattr(
        whatsapp.bridge_http,
        "post",
        lambda url, **kw: seen.append((url, kw)) or Response({"success": True, "deleted_for_me": 2}),
    )
    assert main.delete_messages_for_me(" " + CHAT, [" IN1", "OUT1"], confirm=True)["deleted_for_me"] == 2
    assert seen[0][0].endswith("/api/delete-for-me")
    assert seen[0][1]["json"] == {"chat_jid": CHAT, "message_ids": ["IN1", "OUT1"], "confirm": True}
    assert seen[0][1]["timeout"] >= 60


@pytest.mark.parametrize(
    "chat,ids",
    [
        ("120363000000000001@g.us", ["M1"]),
        ("", ["M1"]),
        ("5511999999999", ["M1"]),
        (CHAT, []),
        (CHAT, ["M1", "M1"]),
        (CHAT, [" "]),
        (CHAT, [f"M{i}" for i in range(201)]),
        (CHAT, "M1"),
    ],
)
def test_refusals(monkeypatch, chat, ids):
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda *a, **kw: pytest.fail("bridge called"))
    assert main.delete_messages_for_me(chat, ids, confirm=True)["error"]["code"] == "invalid_argument"


def test_allow_list(monkeypatch):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["5511888888888"]))
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda *a, **kw: pytest.fail("bridge called"))
    assert main.delete_messages_for_me(CHAT, ["M1"], confirm=True)["error"]["code"] == "denied"


@pytest.mark.parametrize(
    "policy",
    [
        ToolPolicy(read_only=True),
        ToolPolicy(allow=frozenset({"delete_message"})),
        ToolPolicy(deny=frozenset({"delete_messages_for_me"})),
    ],
)
def test_tool_policy(monkeypatch, policy):
    monkeypatch.setattr(tool_policy, "_active", policy)
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda *a, **kw: pytest.fail("bridge called"))
    assert main.delete_messages_for_me(CHAT, ["M1"], confirm=True)["error"]["code"] == "denied"


@pytest.mark.parametrize("status,code", [(404, "not_found"), (408, "bridge_unavailable"), (422, "invalid_argument")])
def test_bridge_errors(monkeypatch, status, code):
    monkeypatch.setattr(whatsapp, "_read_bridge_token", lambda: "t" * 32)
    monkeypatch.setattr(
        whatsapp.bridge_http,
        "post",
        lambda *a, **kw: Response({"success": False, "error": {"code": code, "message": "refused"}}, status),
    )
    assert main.delete_messages_for_me(CHAT, ["M1"], confirm=True)["error"]["code"] == code


def test_annotated_destructive():
    tool = main.mcp._tool_manager.get_tool("delete_messages_for_me")
    assert tool.annotations is not None and tool.annotations.destructive_hint is True
