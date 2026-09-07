import importlib

import pytest

import whatsapp
from errors import ToolError


class DummyResponse:
    def __init__(self, status_code=200, payload=None, text="OK"):
        self.status_code = status_code
        self._payload = payload or {"success": True, "message": "sent", "path": "/tmp/media.jpg"}
        self.text = text

    def json(self):
        return self._payload


def test_bridge_headers_uses_env_token(monkeypatch):
    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "env-token")

    assert whatsapp._bridge_headers() == {"Authorization": "Bearer env-token"}


def test_bridge_headers_falls_back_to_token_file(monkeypatch, tmp_path):
    token_file = tmp_path / ".bridge-token"
    token_file.write_text("file-token\n", encoding="utf-8")
    monkeypatch.delenv("WHATSAPP_BRIDGE_TOKEN", raising=False)
    monkeypatch.setattr(whatsapp, "_BRIDGE_TOKEN_PATH", str(token_file))

    assert whatsapp._bridge_headers() == {"Authorization": "Bearer file-token"}


def test_bridge_headers_reads_token_next_to_whatsmeow_db_path(monkeypatch, tmp_path):
    store_dir = tmp_path / "store"
    store_dir.mkdir()
    (store_dir / ".bridge-token").write_text("volume-token\n", encoding="utf-8")

    monkeypatch.delenv("WHATSAPP_BRIDGE_TOKEN", raising=False)
    monkeypatch.setenv("WHATSMEOW_DB_PATH", str(store_dir / "whatsapp.db"))

    try:
        importlib.reload(whatsapp)
        assert whatsapp._bridge_headers() == {"Authorization": "Bearer volume-token"}
    finally:
        monkeypatch.delenv("WHATSMEOW_DB_PATH", raising=False)
        importlib.reload(whatsapp)


def test_bridge_headers_prefers_env_over_token_file(monkeypatch, tmp_path):
    token_file = tmp_path / ".bridge-token"
    token_file.write_text("file-token\n", encoding="utf-8")
    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "env-token")
    monkeypatch.setattr(whatsapp, "_BRIDGE_TOKEN_PATH", str(token_file))

    assert whatsapp._bridge_headers() == {"Authorization": "Bearer env-token"}


def test_send_message_without_token_surfaces_bridge_401(monkeypatch, tmp_path):
    calls = []
    missing_token = tmp_path / "missing-token"
    monkeypatch.delenv("WHATSAPP_BRIDGE_TOKEN", raising=False)
    monkeypatch.setattr(whatsapp, "_BRIDGE_TOKEN_PATH", str(missing_token))

    def fake_post(url, json, headers=None, timeout=None):
        calls.append({"url": url, "json": json, "headers": headers})
        return DummyResponse(status_code=401, payload={"success": False}, text="Unauthorized")

    monkeypatch.setattr(whatsapp.bridge_http, "post", fake_post)

    with pytest.raises(ToolError) as exc:
        whatsapp.send_message("12025551234", "hello")

    assert exc.value.code == "internal"  # our own token rejected: configuration
    assert "Unauthorized" in exc.value.message
    assert calls[0]["headers"] == {}


@pytest.mark.parametrize(
    ("func_name", "args", "expected_suffix"),
    [
        ("send_message", ("12025551234", "hello"), "/send"),
        ("send_file", ("12025551234", "FILE"), "/send"),
        ("send_audio_message", ("12025551234", "FILE"), "/send"),
        ("download_media", ("msg-id", "12025551234@s.whatsapp.net"), "/download"),
        ("send_reaction", ("12025551234@s.whatsapp.net", "3AABCDEF01234567", "👍"), "/react"),
        ("mark_messages_read", (["3AABCDEF01234567"], "12025551234@s.whatsapp.net"), "/mark-read"),
    ],
)
def test_bridge_post_helpers_include_auth_headers(monkeypatch, tmp_path, func_name, args, expected_suffix):
    calls = []
    media_file = tmp_path / "voice.ogg"
    media_file.write_bytes(b"ogg")
    resolved_args = tuple(str(media_file) if arg == "FILE" else arg for arg in args)
    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "env-token")

    def fake_post(url, json, headers=None, timeout=None):
        calls.append({"url": url, "json": json, "headers": headers})
        return DummyResponse()

    monkeypatch.setattr(whatsapp.bridge_http, "post", fake_post)

    getattr(whatsapp, func_name)(*resolved_args)

    assert calls[0]["url"].endswith(expected_suffix)
    assert calls[0]["headers"] == {"Authorization": "Bearer env-token"}


def test_send_reaction_posts_correct_payload(monkeypatch):
    """send_reaction sends recipient, message_id, emoji, from_me, sender_jid to /react."""
    calls = []
    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "test-token")

    def fake_post(url, json, headers=None, timeout=None):
        calls.append({"url": url, "json": json, "headers": headers})
        return DummyResponse(payload={"ok": True})

    monkeypatch.setattr(whatsapp.bridge_http, "post", fake_post)

    success, msg = whatsapp.send_reaction(
        "12025551234@s.whatsapp.net",
        "3AABCDEF01234567",
        "👍",
        from_me=False,
        sender_jid="98765@s.whatsapp.net",
    )

    assert success is True
    assert len(calls) == 1
    assert calls[0]["url"].endswith("/react")
    payload = calls[0]["json"]
    assert payload["recipient"] == "12025551234@s.whatsapp.net"
    assert payload["message_id"] == "3AABCDEF01234567"
    assert payload["emoji"] == "👍"
    assert payload["from_me"] is False
    assert payload["sender_jid"] == "98765@s.whatsapp.net"
    assert calls[0]["headers"] == {"Authorization": "Bearer test-token"}


def test_send_reaction_empty_emoji_sends_removal(monkeypatch):
    """An empty emoji string is forwarded as-is (reaction removal)."""
    calls = []
    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "test-token")

    def fake_post(url, json, headers=None, timeout=None):
        calls.append({"url": url, "json": json})
        return DummyResponse(payload={"ok": True})

    monkeypatch.setattr(whatsapp.bridge_http, "post", fake_post)

    success, _ = whatsapp.send_reaction("12025551234@s.whatsapp.net", "3AABCDEF01234567", "")

    assert success is True
    assert calls[0]["json"]["emoji"] == ""


def test_send_reaction_missing_recipient_returns_error():
    """send_reaction returns failure without calling the bridge when recipient is empty."""
    with pytest.raises(ToolError) as exc:
        whatsapp.send_reaction("", "3AABCDEF01234567", "👍")
    assert exc.value.code == "invalid_argument" and "chat_jid" in exc.value.message


def test_send_reaction_missing_message_id_returns_error():
    """send_reaction returns failure without calling the bridge when message_id is empty."""
    with pytest.raises(ToolError) as exc:
        whatsapp.send_reaction("12025551234@s.whatsapp.net", "", "👍")
    assert exc.value.code == "invalid_argument" and "message_id" in exc.value.message


def test_mark_messages_read_posts_correct_payload(monkeypatch):
    calls = []
    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "test-token")

    def fake_post(url, json, headers=None, timeout=None):
        calls.append({"url": url, "json": json, "headers": headers})
        return DummyResponse(payload={"success": True, "message": "Messages marked as read"})

    monkeypatch.setattr(whatsapp.bridge_http, "post", fake_post)

    result = whatsapp.mark_messages_read(
        [" 3AABCDEF01234567 ", "3AABCDEF76543210"],
        "120363012345678901@g.us",
        sender_jid="15551234567@s.whatsapp.net",
        timestamp="2026-08-11T18:30:00Z",
    )

    assert result["success"] is True
    assert result["message"] == "Messages marked as read"
    assert calls == [
        {
            "url": f"{whatsapp.WHATSAPP_API_BASE_URL}/mark-read",
            "json": {
                "message_ids": ["3AABCDEF01234567", "3AABCDEF76543210"],
                "chat_jid": "120363012345678901@g.us",
                "sender_jid": "15551234567@s.whatsapp.net",
                "timestamp": "2026-08-11T18:30:00Z",
            },
            "headers": {"Authorization": "Bearer test-token"},
        }
    ]


@pytest.mark.parametrize(
    ("message_ids", "chat_jid", "sender_jid", "expected_message"),
    [
        ([], "12025551234@s.whatsapp.net", "", "message ID"),
        ([""], "12025551234@s.whatsapp.net", "", "message ID"),
        (["3AABCDEF01234567"], "", "", "chat_jid"),
        (["3AABCDEF01234567"], "120363012345678901@g.us", "", "sender_jid"),
    ],
)
def test_mark_messages_read_validates_input(message_ids, chat_jid, sender_jid, expected_message):
    with pytest.raises(ToolError) as exc:
        whatsapp.mark_messages_read(message_ids, chat_jid, sender_jid)

    assert exc.value.code == "invalid_argument"
    assert expected_message in exc.value.message


def test_mark_whole_chat_read_posts_up_to_without_ids(monkeypatch):
    """message_ids=None marks the whole chat read; the counts come back to the caller."""
    calls = []
    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "test-token")

    def fake_post(url, json, headers=None, timeout=None):
        calls.append(json)
        return DummyResponse(
            payload={
                "success": True,
                "message": "Marked 1240 message(s) from 3 sender(s) as read",
                "messages": 1240,
                "senders": 3,
                "batches": 13,
                "truncated": True,
            }
        )

    monkeypatch.setattr(whatsapp.bridge_http, "post", fake_post)

    result = whatsapp.mark_messages_read(
        None,
        "120363012345678901@g.us",
        up_to="2026-09-07T12:00:00Z",
    )

    assert calls == [{"chat_jid": "120363012345678901@g.us", "up_to": "2026-09-07T12:00:00Z"}]
    assert result == {
        "success": True,
        "message": "Marked 1240 message(s) from 3 sender(s) as read",
        "messages": 1240,
        "senders": 3,
        "batches": 13,
        "truncated": True,
    }


def test_mark_whole_chat_read_defaults_to_now(monkeypatch):
    """Without up_to the bridge decides the cut-off, so no bound is sent."""
    calls = []
    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "test-token")
    monkeypatch.setattr(
        whatsapp.bridge_http,
        "post",
        lambda url, json, headers=None, timeout=None: (
            calls.append(json)
            or DummyResponse(payload={"success": True, "message": "ok", "messages": 2, "senders": 1, "batches": 1})
        ),
    )

    result = whatsapp.mark_messages_read(None, "12025551234@s.whatsapp.net")

    assert calls == [{"chat_jid": "12025551234@s.whatsapp.net"}]
    assert result["messages"] == 2 and result["truncated"] is False


@pytest.mark.parametrize(
    ("kwargs", "expected_message"),
    [
        ({"message_ids": None, "sender_jid": "15551234567@s.whatsapp.net"}, "sender_jid"),
        ({"message_ids": ["3AABCDEF01234567"], "up_to": "2026-09-07T12:00:00Z"}, "up_to_timestamp"),
        ({"message_ids": []}, "message ID"),
    ],
)
def test_mark_messages_read_rejects_mixed_forms(monkeypatch, kwargs, expected_message):
    """The two forms do not mix, and an empty list is never "the whole chat"."""

    def unexpected_post(*_args, **_kwargs):
        raise AssertionError("the bridge must not be called")

    monkeypatch.setattr(whatsapp.bridge_http, "post", unexpected_post)

    with pytest.raises(ToolError) as exc:
        whatsapp.mark_messages_read(chat_jid="12025551234@s.whatsapp.net", **kwargs)

    assert exc.value.code == "invalid_argument"
    assert expected_message in exc.value.message


def test_send_message_with_quoted_reply_includes_quote_fields(monkeypatch):
    """send_message passes quoted_message_id, quoted_sender_jid, quoted_content to /api/send."""
    calls = []
    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "test-token")

    def fake_post(url, json, headers=None, timeout=None):
        calls.append({"url": url, "json": json, "headers": headers})
        return DummyResponse()

    monkeypatch.setattr(whatsapp.bridge_http, "post", fake_post)

    success, _, _ = whatsapp.send_message(
        "12025551234@s.whatsapp.net",
        "Great point!",
        quoted_message_id="3AORIGINAL0000001",
        quoted_sender_jid="99887766@s.whatsapp.net",
        quoted_content="original text",
    )

    assert success is True
    payload = calls[0]["json"]
    assert payload["recipient"] == "12025551234@s.whatsapp.net"
    assert payload["message"] == "Great point!"
    assert payload["quoted_message_id"] == "3AORIGINAL0000001"
    assert payload["quoted_sender_jid"] == "99887766@s.whatsapp.net"
    assert payload["quoted_content"] == "original text"
    assert calls[0]["headers"] == {"Authorization": "Bearer test-token"}


def test_send_message_without_quote_omits_quote_fields(monkeypatch):
    """send_message without a quoted_message_id does not include quote fields."""
    calls = []
    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "test-token")

    def fake_post(url, json, headers=None, timeout=None):
        calls.append({"url": url, "json": json})
        return DummyResponse()

    monkeypatch.setattr(whatsapp.bridge_http, "post", fake_post)

    whatsapp.send_message("12025551234@s.whatsapp.net", "Hello!")

    payload = calls[0]["json"]
    assert "quoted_message_id" not in payload
    assert "quoted_sender_jid" not in payload
    assert "quoted_content" not in payload


def test_send_message_with_mentions_includes_mentions_field(monkeypatch):
    """send_message passes the mentions list to /api/send."""
    calls = []
    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "test-token")

    def fake_post(url, json, headers=None, timeout=None):
        calls.append({"url": url, "json": json})
        return DummyResponse()

    monkeypatch.setattr(whatsapp.bridge_http, "post", fake_post)

    success, _, _ = whatsapp.send_message(
        "123456789@g.us",
        "thanks @12025551234!",
        mentions=["12025551234"],
    )

    assert success is True
    payload = calls[0]["json"]
    assert payload["mentions"] == ["12025551234"]


def test_send_message_without_mentions_omits_mentions_field(monkeypatch):
    """send_message without mentions does not include the mentions field."""
    calls = []
    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "test-token")

    def fake_post(url, json, headers=None, timeout=None):
        calls.append({"url": url, "json": json})
        return DummyResponse()

    monkeypatch.setattr(whatsapp.bridge_http, "post", fake_post)

    whatsapp.send_message("12025551234@s.whatsapp.net", "Hello!")

    payload = calls[0]["json"]
    assert "mentions" not in payload
