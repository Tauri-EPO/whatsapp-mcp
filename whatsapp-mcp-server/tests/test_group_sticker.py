import base64
import sqlite3
from pathlib import Path

import pytest
from starlette.applications import Starlette
from starlette.testclient import TestClient

import main
import media_upload
import tool_policy
import whatsapp
from chat_policy import ChatPolicy
from http_upload import UploadApp
from untrusted import clean_untrusted

GROUP = "120363000000000001@g.us"
WEBP = b"RIFF\x16\x00\x00\x00WEBPVP8X\x0a\x00\x00\x00" + bytes(10)


class Response:
    status_code = 200

    def json(self):
        return {"success": True, "message": "sent", "members": []}


@pytest.fixture
def bridge(monkeypatch, tmp_path):
    monkeypatch.setenv("WHATSAPP_MEDIA_ROOTS", str(tmp_path / "outbox"))
    monkeypatch.setenv("WHATSAPP_MCP_TRANSPORT", "http")
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([]))
    calls = []

    def request(method, endpoint, **kwargs):
        payload = kwargs.get("json", {})
        if endpoint == "/send":
            calls.append((payload, Path(payload["media_path"]).read_bytes()))
        return Response()

    monkeypatch.setattr(whatsapp, "_bridge_request", request)
    tool_policy.set_active_policy(tool_policy.ToolPolicy())
    yield calls
    tool_policy.set_active_policy(None)


@pytest.mark.parametrize("source", ["path", "inline", "upload"])
def test_sticker_sources_reach_bridge_and_cleanup(source, bridge, tmp_path):
    kwargs = {}
    if source == "path":
        path = tmp_path / "sticker.webp"
        path.write_bytes(WEBP)
        kwargs["media_path"] = str(path)
    elif source == "inline":
        kwargs.update(media_base64=base64.b64encode(WEBP).decode(), filename="sticker.webp")
    else:
        # Real HTTP upload path, then the same MCP tool and fake bridge.
        with TestClient(UploadApp(Starlette(), None)) as client:
            response = client.post("/upload", content=WEBP, headers={"X-Filename": "sticker.webp"})
            assert response.status_code == 200
            kwargs["upload_id"] = response.json()["upload_id"]
    result = main.send_file(GROUP, as_sticker=True, **kwargs)
    assert result["success"] is True
    payload, data = bridge[0]
    assert payload["as_sticker"] is True and data == WEBP
    assert not payload.get("message")
    if source != "path":
        assert not Path(payload["media_path"]).exists()


@pytest.mark.parametrize("data,caption", [(b"JPEG renamed to webp", ""), (WEBP, "caption")])
@pytest.mark.parametrize("source", ["path", "inline"])
def test_sticker_invalid_never_uploads(data, caption, source, bridge, tmp_path, monkeypatch):
    def unexpected(*args, **kwargs):
        pytest.fail("inline write occurred before validation")

    monkeypatch.setattr(media_upload, "write_inline", unexpected)
    if source == "path":
        path = tmp_path / "fake.webp"
        path.write_bytes(data)
        kwargs = {"media_path": str(path)}
    else:
        kwargs = {"media_base64": base64.b64encode(data).decode(), "filename": "fake.webp"}
    result = main.send_file(GROUP, caption=caption, as_sticker=True, **kwargs)
    assert result["error"]["code"] == "invalid_argument"
    assert bridge == []


def test_sticker_default_and_preview(bridge, tmp_path):
    path = tmp_path / "photo.webp"
    path.write_bytes(WEBP)
    whatsapp.send_file(GROUP, str(path))
    assert "as_sticker" not in bridge[0][0]
    _, _, preview = whatsapp.send_file(GROUP, str(path), as_sticker=True, dry_run=True)
    assert preview["payload"]["as_sticker"] is True
    assert len(bridge) == 1


def test_permission_code_reaches_mcp(bridge, tmp_path, monkeypatch):
    class Denied(Response):
        status_code = 403

        def json(self):
            return {"error": {"code": "group_admin_required", "message": "This group is restricted to admins"}}

    monkeypatch.setattr(whatsapp, "_bridge_request", lambda *a, **k: Denied())
    path = tmp_path / "sticker.webp"
    path.write_bytes(WEBP)
    result = main.send_file(GROUP, str(path), as_sticker=True)
    assert result["error"]["code"] == "group_admin_required"
    assert "admins" in result["error"]["message"]


def test_group_settings_reads_unknown_and_observed(paired_dbs, monkeypatch):
    conn = sqlite3.connect(whatsapp.MESSAGES_DB_PATH)
    fields = ("announce", "locked", "join_approval_required", "topic")
    for setting in fields:
        conn.execute(f"ALTER TABLE chats ADD COLUMN group_{setting}")
        conn.execute(f"ALTER TABLE chats ADD COLUMN group_{setting}_updated_at TIMESTAMP")
    conn.commit()
    result = main.get_chat(GROUP)
    assert all(value is None for value in result["group_settings"].values())
    stamp = "2026-10-14 12:00:00+00:00"
    conn.execute(
        "UPDATE chats SET group_announce=1, group_locked=0, group_join_approval_required=1, group_topic=?, group_topic_updated_at=? WHERE jid=?",
        ("A\u200b description\nline", stamp, GROUP),
    )
    conn.commit()
    conn.close()
    result = main.get_chat(GROUP, fields=["jid", "group_settings"])
    settings = result["group_settings"]
    assert settings["announce"] is True
    assert settings["locked"] is False
    assert settings["join_approval_required"] is True
    assert settings["topic"] == "A description\nline"
    assert settings["topic_updated_at"] == stamp
    assert settings["announce_updated_at"] is None
    monkeypatch.setattr(whatsapp, "_bridge_request", lambda *a, **k: Response())
    members = main.list_group_members(GROUP)
    assert members["group_settings"] == settings
    wrapped = clean_untrusted({"group_settings": {"topic": "A\u200b description"}}, wrap=True)
    assert wrapped["group_settings"]["topic"] == "<untrusted>A description</untrusted>"
