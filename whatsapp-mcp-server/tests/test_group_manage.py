"""Group management tools: payloads, validation, allow-list, bridge errors."""

import pytest

import main
import whatsapp
from chat_policy import ChatPolicy
from errors import ToolError

GROUP = "120363000000000001@g.us"


class Resp:
    def __init__(self, status=200, payload=None):
        self.status_code = status
        self._payload = payload if payload is not None else {"success": True}
        self.text = ""

    def json(self):
        return self._payload


@pytest.fixture
def calls(monkeypatch):
    seen = []

    def fake_post(url, json=None, headers=None, timeout=None):
        seen.append((url.rsplit("/api", 1)[-1], json))
        if url.endswith("/group/invite"):
            return Resp(payload={"success": True, "link": "https://chat.whatsapp.com/ABC"})
        return Resp()

    monkeypatch.setattr(whatsapp.requests, "post", fake_post)
    monkeypatch.setattr(whatsapp, "_read_bridge_token", lambda: "t" * 32)
    return seen


def test_participants_payload(calls):
    out = main.manage_group_participants(GROUP, "Add", ["+5511999999999", " 5511888888888@s.whatsapp.net "])
    assert out["success"] is True
    assert calls[-1] == (
        "/group/participants",
        {"group_jid": GROUP, "action": "add", "participants": ["+5511999999999", "5511888888888@s.whatsapp.net"]},
    )


def test_participants_validation(calls):
    assert (
        main.manage_group_participants("5511999999999@s.whatsapp.net", "add", ["x"])["error"]["code"]
        == "invalid_argument"
    )
    assert main.manage_group_participants(GROUP, "kick", ["x"])["error"]["code"] == "invalid_argument"
    assert main.manage_group_participants(GROUP, "add", [" "])["error"]["code"] == "invalid_argument"
    assert calls == []


def test_update_invite_leave_typing(calls):
    assert main.update_group(GROUP, name="Família")["success"] is True
    assert calls[-1] == ("/group/subject", {"group_jid": GROUP, "name": "Família"})
    assert main.update_group(GROUP, description="")["success"] is True
    assert calls[-1][1] == {"group_jid": GROUP, "description": ""}
    assert main.update_group(GROUP)["error"]["code"] == "invalid_argument"

    out = main.get_group_invite_link(GROUP, reset=True)
    assert out["link"].startswith("https://chat.whatsapp.com/") and calls[-1][1] == {"group_jid": GROUP, "reset": True}

    assert main.leave_group(GROUP)["success"] is True and calls[-1][0] == "/group/leave"

    assert main.send_typing("5511999999999")["success"] is True
    assert calls[-1] == ("/typing", {"recipient": "5511999999999", "is_typing": True})
    assert main.send_typing("", False)["error"]["code"] == "invalid_argument"


def test_allow_list_blocks_before_bridge(monkeypatch):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["5511999999999"]))
    monkeypatch.setattr(whatsapp.requests, "post", lambda *a, **k: pytest.fail("bridge called"))
    for fn, args in (
        (whatsapp.manage_group_participants, (GROUP, "add", ["5511999999999"])),
        (whatsapp.update_group, (GROUP, "x", None)),
        (whatsapp.get_group_invite_link, (GROUP,)),
        (whatsapp.leave_group, (GROUP,)),
        (whatsapp.send_typing, (GROUP,)),
    ):
        with pytest.raises(ToolError) as exc:
            fn(*args)
        assert exc.value.code == "denied", fn.__name__


def test_bridge_refusal_maps_to_code(monkeypatch):
    monkeypatch.setattr(whatsapp, "_read_bridge_token", lambda: "t" * 32)
    monkeypatch.setattr(
        whatsapp.requests, "post", lambda *a, **k: Resp(502, {"success": False, "message": "not connected"})
    )
    out = main.leave_group(GROUP)
    assert out["error"]["code"] == "bridge_unavailable" and "not connected" in out["error"]["message"]
