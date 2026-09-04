import pytest

import whatsapp
from chat_policy import ChatPolicy
from errors import ToolError

GROUP = "120363000000000001@g.us"


class Resp:
    def __init__(self, status=200, payload=None, text=""):
        self.status_code = status
        self._payload = payload
        self.text = text

    def json(self):
        if self._payload is None:
            raise ValueError("no json")
        return self._payload


def test_get_group_members_happy_path(monkeypatch):
    calls = []

    def fake_get(url, params=None, headers=None, timeout=None):
        calls.append((url, params, headers, timeout))
        return Resp(
            payload={
                "success": True,
                "group_jid": GROUP,
                "name": "Obra",
                "members": [
                    {
                        "jid": "5511999999999@s.whatsapp.net",
                        "phone_number": "5511999999999",
                        "name": "Enrico",
                        "is_admin": True,
                    },
                    {"jid": "777@lid", "phone_number": "5511888888888", "is_admin": False},
                    {"jid": "888@lid", "is_admin": False},
                ],
            }
        )

    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "test-token-0123456789")
    monkeypatch.setattr(whatsapp.bridge_http, "get", fake_get)

    result = whatsapp.get_group_members(GROUP)

    assert result["success"] is True
    assert calls[0][0].endswith("/group/members")
    assert calls[0][1] == {"jid": GROUP}
    assert calls[0][2] == {"Authorization": "Bearer test-token-0123456789"}
    assert [m["display"] for m in result["members"]] == ["Enrico", "5511888888888", "888@lid"]


def test_get_group_members_rejects_non_group(monkeypatch):
    monkeypatch.setattr(whatsapp.bridge_http, "get", lambda *a, **k: pytest.fail("bridge called"))
    with pytest.raises(ToolError, match="Not a group JID") as exc:
        whatsapp.get_group_members("5511999999999@s.whatsapp.net")
    assert exc.value.code == "invalid_argument"


def test_get_group_members_respects_policy(monkeypatch):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["5511999999999"]))
    monkeypatch.setattr(whatsapp.bridge_http, "get", lambda *a, **k: pytest.fail("bridge called"))
    with pytest.raises(ToolError, match="WHATSAPP_ALLOWED_CHATS") as exc:
        whatsapp.get_group_members(GROUP)
    assert exc.value.code == "denied"


def test_get_group_members_bridge_error(monkeypatch):
    monkeypatch.setattr(
        whatsapp.bridge_http, "get", lambda *a, **k: Resp(502, {"success": False, "message": "not connected"})
    )
    with pytest.raises(ToolError, match="not connected") as exc:
        whatsapp.get_group_members(GROUP)
    assert exc.value.code == "bridge_unavailable"


def test_get_group_members_non_json_error(monkeypatch):
    monkeypatch.setattr(whatsapp.bridge_http, "get", lambda *a, **k: Resp(500, None, "boom"))
    with pytest.raises(ToolError, match="boom") as exc:
        whatsapp.get_group_members(GROUP)
    assert exc.value.code == "bridge_unavailable"
