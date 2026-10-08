import json
from pathlib import Path

import pytest

import whatsapp
from chat_policy import ChatPolicy
from errors import ToolError
from phone import normalize_recipient


def test_bridge_and_mcp_share_spelling_contract():
    cases = json.loads(
        (Path(__file__).parents[2] / "whatsapp-bridge/testdata/recipient_spellings.json").read_text(encoding="utf-8")
    )
    for case in cases:
        if case.get("invalid"):
            with pytest.raises(ToolError) as exc:
                normalize_recipient(case["raw"])
            assert exc.value.code == "invalid_argument"
        else:
            assert normalize_recipient(case["raw"]) == case["normalized"]


@pytest.mark.parametrize("tool", ["send_message", "send_file", "send_audio_message", "forward_message"])
@pytest.mark.parametrize("allowed", [False, True])
@pytest.mark.parametrize("recipient", ["5511999999999", "+55 (11) 99999-9999", "55.11.99999.9999"])
def test_normalized_policy_and_http_payload(monkeypatch, tmp_path, tool, allowed, recipient):
    monkeypatch.setattr(
        whatsapp,
        "CHAT_POLICY",
        ChatPolicy.from_entries(["120363000000000001@g.us", "5511999999999" if allowed else "5511888888888"]),
    )
    calls = []

    class Response:
        status_code = 200

        def json(self):
            return {"success": True, "message": "sent"}

    def post(url, **kwargs):
        calls.append(kwargs["json"])
        return Response()

    monkeypatch.setattr(whatsapp.bridge_http, "post", post)
    media = tmp_path / "voice.ogg"
    media.write_bytes(b"fake audio")
    args = (recipient, "hello") if tool == "send_message" else (recipient, str(media))
    if tool == "forward_message":
        args = ("120363000000000001@g.us", "MSG1", recipient)
    if allowed:
        result = getattr(whatsapp, tool)(*args)
        assert result["success"] if tool == "forward_message" else result[0]
        assert len(calls) == 1
        assert calls[0]["to_chat_jid" if tool == "forward_message" else "recipient"] == "5511999999999"
    else:
        with pytest.raises(ToolError) as exc:
            getattr(whatsapp, tool)(*args)
        assert exc.value.code == "denied"
        assert calls == []


@pytest.mark.parametrize("tool", ["send_message", "send_file", "send_audio_message", "forward_message"])
@pytest.mark.parametrize("invalid", [False, True])
def test_refused_recipient_has_no_http_effect(monkeypatch, tool, invalid):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["+55 11 99999-9999"]))
    calls = []
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda *args, **kwargs: calls.append(kwargs))
    recipient = "12025551234-1612345678" if invalid else "+55 11 99999-9999"
    args = (recipient, "unused")
    if tool == "forward_message":
        monkeypatch.setattr(
            whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["120363000000000001@g.us", "+55 11 99999-9999"])
        )
        args = ("120363000000000001@g.us", "MSG1", recipient)
    with pytest.raises(ToolError) as exc:
        getattr(whatsapp, tool)(*args)
    assert exc.value.code == ("invalid_argument" if invalid else "denied")
    assert calls == []


@pytest.mark.parametrize(
    "raw,want",
    [(" 120363000000000001@g.us ", "120363000000000001@g.us"), ("5511999999999\n", "5511999999999")],
)
def test_forward_trims_destination_before_policy_and_http(monkeypatch, raw, want):
    source = "120363000000000001@g.us"
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([source, "5511999999999"]))
    posted = []

    class Response:
        status_code = 200

        def json(self):
            return {"success": True, "chat_jid": want}

    def post(url, **kwargs):
        posted.append(kwargs["json"])
        return Response()

    monkeypatch.setattr(whatsapp.bridge_http, "post", post)
    result = whatsapp.forward_message(source, "MSG1", raw)
    assert result["chat_jid"] == want
    assert posted == [{"chat_jid": source, "message_id": "MSG1", "to_chat_jid": want}]
