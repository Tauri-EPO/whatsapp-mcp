"""purge_media: request shape sent to the bridge, dry-run default, allow-list, error mapping."""

import main
import whatsapp
from chat_policy import ChatPolicy

CHAT = "5511888888888@s.whatsapp.net"
OTHER = "120363000000000001@g.us"


class Resp:
    def __init__(self, status=200, payload=None, text=""):
        self.status_code = status
        self._payload = payload
        self.text = text

    def json(self):
        if self._payload is None:
            raise ValueError("no json")
        return self._payload


def _bridge(monkeypatch, payload=None, status=200, text=""):
    calls = []

    def fake_post(url, json=None, headers=None, timeout=None):
        calls.append((url, json))
        return Resp(status=status, payload=payload, text=text)

    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "test-token-0123456789")
    monkeypatch.setattr(whatsapp.bridge_http, "post", fake_post)
    return calls


BRIDGE_OK = {
    "success": True,
    "message": "Dry run: 1 cached file(s), 4096 bytes would be removed; repeat with dry_run=false to purge",
    "dry_run": True,
    "matched": 2,
    "purged_files": 1,
    "purged_bytes": 4096,
    "truncated": False,
    "items": [
        {"message_id": "A", "chat_jid": CHAT, "purged": True, "bytes": 4096, "file": "video_x_A.mp4"},
        {"message_id": "B", "chat_jid": CHAT, "purged": False, "bytes": 0, "reason": "not cached"},
    ],
}


def test_items_form_is_dry_run_by_default(monkeypatch):
    calls = _bridge(monkeypatch, BRIDGE_OK)
    out = main.purge_media(items=[{"message_id": " A ", "chat_jid": CHAT}, {"message_id": "B", "chat_jid": CHAT}])
    assert calls[0][0].endswith("/media/purge")
    assert calls[0][1] == {
        "dry_run": True,
        "items": [{"message_id": "A", "chat_jid": CHAT}, {"message_id": "B", "chat_jid": CHAT}],
    }
    assert out["success"] and out["dry_run"] is True and out["purged_files"] == 1 and out["purged_bytes"] == 4096
    assert out["matched"] == 2 and out["items"][1]["reason"] == "not cached" and "dry_run=false" in out["message"]


def test_criteria_form_sends_only_set_fields(monkeypatch):
    calls = _bridge(monkeypatch, {**BRIDGE_OK, "dry_run": False, "truncated": True})
    out = main.purge_media(chat_jid=CHAT, older_than_days=30, media_type="video", dry_run=False)
    assert calls[0][1] == {"dry_run": False, "chat_jid": CHAT, "older_than_days": 30, "media_type": "video"}
    assert out["dry_run"] is False and out["truncated"] is True

    calls.clear()
    main.purge_media(min_bytes=1_000_000)
    assert calls[0][1] == {"dry_run": True, "min_bytes": 1_000_000}


def test_items_win_over_criteria(monkeypatch):
    calls = _bridge(monkeypatch, BRIDGE_OK)
    main.purge_media(items=[{"message_id": "A", "chat_jid": CHAT}], chat_jid=OTHER, older_than_days=5)
    assert "older_than_days" not in calls[0][1] and "chat_jid" not in calls[0][1]


def test_validation(monkeypatch):
    calls = _bridge(monkeypatch, BRIDGE_OK)
    assert main.purge_media()["error"]["code"] == "invalid_argument"
    assert main.purge_media(dry_run=False)["error"]["code"] == "invalid_argument"
    assert main.purge_media(items=[{"message_id": "A"}])["error"]["code"] == "invalid_argument"
    assert main.purge_media(items=["A"])["error"]["code"] == "invalid_argument"
    assert main.purge_media(media_type="hologram")["error"]["code"] == "invalid_argument"
    assert main.purge_media(older_than_days=-1)["error"]["code"] == "invalid_argument"
    assert calls == []  # nothing reached the bridge


def test_allow_list_blocks_before_the_bridge(monkeypatch):
    calls = _bridge(monkeypatch, BRIDGE_OK)
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([CHAT]))
    assert main.purge_media(chat_jid=OTHER)["error"]["code"] == "denied"
    assert main.purge_media(items=[{"message_id": "A", "chat_jid": OTHER}])["error"]["code"] == "denied"
    assert calls == []
    assert main.purge_media(chat_jid=CHAT)["success"]
    assert calls[0][1] == {"dry_run": True, "chat_jid": CHAT}


def test_bridge_errors_are_surfaced(monkeypatch):
    _bridge(monkeypatch, {"success": False, "message": "chat x is not in WHATSAPP_ALLOWED_CHATS"}, status=403)
    out = main.purge_media(chat_jid=CHAT)
    assert out["error"]["code"] == "denied" and "WHATSAPP_ALLOWED_CHATS" in out["error"]["message"]

    _bridge(monkeypatch, None, status=503, text="bridge down")
    assert main.purge_media(chat_jid=CHAT)["error"]["code"] == "bridge_unavailable"
