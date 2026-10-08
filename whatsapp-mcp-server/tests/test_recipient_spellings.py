import json
from pathlib import Path

import pytest

import whatsapp
from chat_policy import ChatPolicy
from errors import ToolError
from phone import normalize_recipient


def test_bridge_and_mcp_share_spelling_contract():
    cases = json.loads((Path(__file__).parents[2] / "whatsapp-bridge/testdata/recipient_spellings.json").read_text())
    for case in cases:
        assert normalize_recipient(case["raw"]) == case["normalized"]


@pytest.mark.parametrize("tool", ["send_message", "send_file", "send_audio_message"])
@pytest.mark.parametrize("allowed", [False, True])
@pytest.mark.parametrize("recipient", ["5511999999999", "+55 (11) 99999-9999", "55.11.99999.9999"])
def test_normalized_policy_and_http_payload(monkeypatch, tmp_path, tool, allowed, recipient):
    monkeypatch.setattr(
        whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["5511999999999" if allowed else "5511888888888"])
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
    if allowed:
        assert getattr(whatsapp, tool)(*args)[0]
        assert len(calls) == 1
        assert calls[0]["recipient"] == "5511999999999"
    else:
        with pytest.raises(ToolError) as exc:
            getattr(whatsapp, tool)(*args)
        assert exc.value.code == "denied"
        assert calls == []
