"""Own-profile write tools: arguments, annotations and a single 429."""

from __future__ import annotations

import pytest

import main
import tool_policy
import whatsapp
from tool_policy import ToolPolicy

pytestmark = pytest.mark.usefixtures("auth_runtime_store")


class Resp:
    def __init__(self, status=200, payload=None):
        self.status_code = status
        self._payload = payload if payload is not None else {"success": True}
        self.text = ""
        self.headers = {}

    def json(self):
        return self._payload


@pytest.fixture
def calls(monkeypatch):
    seen = []

    def fake_post(url, json=None, headers=None, timeout=None):
        seen.append((url.rsplit("/api", 1)[-1], json))
        return Resp(payload={"success": True, "picture_id": "pic-640", "removed": True, "text": "ok", "name": "Alice"})

    monkeypatch.setattr(whatsapp.bridge_http, "post", fake_post)
    monkeypatch.setattr(whatsapp, "_read_bridge_token", lambda: "t" * 32)
    return seen


def _tool(name):
    tool = main.mcp._tool_manager.get_tool(name)
    assert tool is not None
    return tool


def test_profile_tools_declare_their_arguments_and_are_writes():
    photo = _tool("set_profile_photo")
    assert set(photo.parameters["properties"]) == {"path"}
    assert photo.parameters["required"] == ["path"]
    assert photo.annotations.read_only_hint is False
    assert photo.annotations.destructive_hint is True
    assert photo.annotations.idempotent_hint is False
    assert photo.annotations.open_world_hint is True

    remove = _tool("remove_profile_photo")
    assert remove.parameters["properties"] == {}
    assert remove.annotations.destructive_hint is True
    assert remove.annotations.idempotent_hint is True
    assert remove.annotations.open_world_hint is True

    about = _tool("set_about")
    assert set(about.parameters["properties"]) == {"text"}
    assert about.parameters["required"] == ["text"]
    assert about.annotations.idempotent_hint is True
    assert about.annotations.destructive_hint is True
    assert about.annotations.open_world_hint is True

    name = _tool("set_display_name")
    assert set(name.parameters["properties"]) == {"name"}
    assert name.parameters["required"] == ["name"]
    assert name.annotations.idempotent_hint is True
    assert name.annotations.destructive_hint is True
    assert name.annotations.open_world_hint is True


def test_profile_tools_post_once(calls):
    assert main.set_profile_photo("  /tmp/photo.jpg  ")["picture_id"] == "pic-640"
    assert main.remove_profile_photo()["removed"] is True
    assert main.set_about("out of office")["text"] == "ok"
    assert main.set_display_name("Alice")["name"] == "Alice"
    assert calls == [
        ("/profile/photo", {"path": "/tmp/photo.jpg"}),
        ("/profile/photo/remove", {}),
        ("/profile/about", {"text": "out of office"}),
        ("/profile/name", {"name": "Alice"}),
    ]


def test_profile_rate_limit_is_reported_once(monkeypatch):
    seen = []

    def fake_post(url, json=None, headers=None, timeout=None):
        seen.append(url)
        return Resp(429, {"message": "rate-overlimit"})

    monkeypatch.setattr(whatsapp.bridge_http, "post", fake_post)
    monkeypatch.setattr(whatsapp, "_read_bridge_token", lambda: "t" * 32)
    result = main.set_about("hello")
    assert result["error"]["code"] == "rate_limited"
    assert "Send limit" not in result["error"]["message"]
    assert "retry" in result["error"]["message"]
    assert len(seen) == 1


def test_deny_list_blocks_profile_writes(monkeypatch):
    def explode(*args, **kwargs):
        raise AssertionError("a denied profile tool reached the bridge")

    monkeypatch.setattr(whatsapp.bridge_http, "post", explode)
    tool_policy.set_active_policy(
        ToolPolicy(deny=frozenset({"set_profile_photo", "remove_profile_photo", "set_about", "set_display_name"}))
    )
    try:
        for result in (
            main.set_profile_photo("/tmp/photo.jpg"),
            main.remove_profile_photo(),
            main.set_about("hello"),
            main.set_display_name("Alice"),
        ):
            assert result["error"]["code"] == "denied"
    finally:
        tool_policy.set_active_policy(None)
