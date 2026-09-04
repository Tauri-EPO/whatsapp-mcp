from datetime import datetime

import pytest

import whatsapp
from chat_policy import ChatPolicy
from whatsapp import Message, msg_to_dict

CHAT = "120363000000000001@g.us"


class Resp:
    def __init__(self, status=200, payload=None):
        self.status_code = status
        self._payload = payload

    def json(self):
        if self._payload is None:
            raise ValueError("no json")
        return self._payload


def _msg(media_type, filename):
    return Message(
        timestamp=datetime(2024, 1, 1, 12, 0, 0),
        sender="111@s.whatsapp.net",
        content="x",
        is_from_me=False,
        chat_jid=CHAT,
        id="m1",
        media_type=media_type,
        filename=filename,
    )


def test_poll_vote_exposes_poll_message_id_not_filename():
    d = msg_to_dict(_msg("poll_vote", "POLL1"), include_sender_name=False)
    assert d["poll_message_id"] == "POLL1"
    assert d["filename"] is None
    assert d["reaction_to_message_id"] is None


def test_poll_creation_row_is_plain():
    d = msg_to_dict(_msg("poll", None), include_sender_name=False)
    assert d["media_type"] == "poll" and d["poll_message_id"] is None and d["filename"] is None


def test_get_poll_results_calls_bridge(monkeypatch):
    calls = []

    def fake_get(url, params=None, headers=None, timeout=None):
        calls.append((url, params))
        return Resp(payload={"success": True, "question": "Almoço?", "total_voters": 2, "options": [], "votes": []})

    monkeypatch.setattr(whatsapp.requests, "get", fake_get)
    result = whatsapp.get_poll_results("POLL1", CHAT)
    assert result["success"] is True and result["question"] == "Almoço?"
    assert calls[0][0].endswith("/poll") and calls[0][1] == {"message_id": "POLL1", "chat_jid": CHAT}


def test_get_poll_results_errors(monkeypatch):
    monkeypatch.setattr(whatsapp.requests, "get", lambda *a, **k: Resp(404, {"success": False, "message": "no poll"}))
    assert whatsapp.get_poll_results("X", CHAT) == {"success": False, "message": "no poll"}
    monkeypatch.setattr(whatsapp.requests, "get", lambda *a, **k: pytest.fail("bridge called"))
    assert whatsapp.get_poll_results("", CHAT)["success"] is False
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["5511999999999"]))
    assert "WHATSAPP_ALLOWED_CHATS" in whatsapp.get_poll_results("POLL1", CHAT)["message"]


def test_target_message_id_preferred_over_legacy_filename():
    legacy = _msg("reaction", "OLD")
    assert msg_to_dict(legacy, include_sender_name=False)["reaction_to_message_id"] == "OLD"
    migrated = _msg("poll_vote", "OLD")
    migrated.target_message_id = "NEW"
    d = msg_to_dict(migrated, include_sender_name=False)
    assert d["poll_message_id"] == "NEW" and d["target_message_id"] == "NEW" and d["filename"] is None
    plain = _msg("image", "photo.jpg")
    d = msg_to_dict(plain, include_sender_name=False)
    assert d["filename"] == "photo.jpg" and d["target_message_id"] is None
