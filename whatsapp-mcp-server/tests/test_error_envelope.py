"""Every tool answers failures with {"error": {"code", "message"}}."""

import pytest

import main
from errors import ERROR_CODES, ToolError, error, tool_errors


def test_envelope_shape():
    body = error("not_found", "nope", chat_jid="x@s.whatsapp.net")
    assert body == {"error": {"code": "not_found", "message": "nope"}, "chat_jid": "x@s.whatsapp.net"}
    with pytest.raises(ValueError):
        ToolError("bogus", "x")


def test_decorator_maps_exceptions():
    @tool_errors
    def raises_tool():
        raise ToolError("denied", "no")

    @tool_errors
    def raises_value():
        raise ValueError("bad input")

    @tool_errors
    def raises_other():
        raise RuntimeError("boom")

    @tool_errors
    def ok():
        return {"fine": True}

    assert raises_tool() == {"error": {"code": "denied", "message": "no"}}
    assert raises_value() == {"error": {"code": "invalid_argument", "message": "bad input"}}
    out = raises_other()
    assert out["error"]["code"] == "internal" and "RuntimeError" in out["error"]["message"]
    assert ok() == {"fine": True}


def test_every_tool_is_wrapped():
    tools = [
        name
        for name in dir(main)
        if callable(getattr(main, name)) and getattr(getattr(main, name), "__wrapped__", None)
    ]
    assert "list_messages" in tools and "send_message" in tools and "transcribe_audio" in tools
    assert len(tools) >= 19


def test_tools_report_not_found_and_denied(monkeypatch):
    monkeypatch.setattr(main, "whatsapp_get_chat", lambda *a, **k: None)
    assert main.get_chat("x@s.whatsapp.net")["error"]["code"] == "not_found"

    def denied(*a, **k):
        raise ToolError("denied", "not in WHATSAPP_ALLOWED_CHATS")

    monkeypatch.setattr(main, "whatsapp_send_message", denied)
    out = main.send_message("x@s.whatsapp.net", "hi")
    assert out["error"]["code"] == "denied"

    monkeypatch.setattr(
        main,
        "whatsapp_send_message",
        lambda *a, **k: (True, "sent", {"message_id": "ABC", "chat_jid": "x@s.whatsapp.net"}),
    )
    assert main.send_message("x@s.whatsapp.net", "hi") == {
        "success": True,
        "message": "sent",
        "message_id": "ABC",
        "chat_jid": "x@s.whatsapp.net",
    }


def test_codes_are_documented():
    assert set(ERROR_CODES) == {"not_found", "denied", "bridge_unavailable", "invalid_argument", "internal"}
