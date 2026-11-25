"""get_profile shape, allow-list, and the local profile photo read_media opens."""

import base64
import os

import pytest

import main
import media_inventory
import media_read
import whatsapp
from chat_policy import ChatPolicy
from errors import ToolError
from tests.conftest import ALICE
from tests.test_read_media import PNG
from untrusted import CLOSE_TAG, OPEN_TAG, WRAP_ENV

PHONE = "5511999999999"


def _payload(**extra):
    body = {
        "success": True,
        "jid": ALICE,
        "query": ALICE,
        "on_whatsapp": True,
        "about": "hello\u202e",
        "about_status": "ok",
        "picture_id": "pic-1",
        "picture_status": "ok",
        "picture_chat_jid": ALICE,
        "picture_message_id": "profile-picture",
        "push_name": "Alice",
        "push_name_source": "local_store",
        "is_business": True,
        "verified_name": "Alice",
        "categories": ["Shop"],
        "email": "alice@example.test",
        "rate_limited": False,
    }
    body.update(extra)
    return body


def test_profile_shape_and_untrusted_about(monkeypatch):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.unrestricted())
    calls = []

    def request(method, path, **kwargs):
        calls.append((method, path, kwargs))
        return _payload()

    monkeypatch.setattr(whatsapp, "_bridge_request", request)
    monkeypatch.setattr(whatsapp, "_bridge_json", lambda response: response)
    monkeypatch.setenv(WRAP_ENV, "1")
    result = main.get_profile("+55 11 99999-9999")
    assert calls == [("GET", "/profile", {"params": {"jid": PHONE}})]
    assert result["on_whatsapp"] is True
    assert result["picture_message_id"] == "profile-picture"
    assert result["push_name"] == "Alice"
    assert result["push_name_source"] == "local_store"
    assert result["verified_name"] == "Alice"
    assert result["about"] == f"{OPEN_TAG}hello{CLOSE_TAG}"
    monkeypatch.setenv(WRAP_ENV, "0")
    plain = main.get_profile(PHONE)
    assert plain["about"] == "hello"


def test_profile_allow_list_precedes_bridge(monkeypatch):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["5511888888888"]))
    monkeypatch.setattr(whatsapp, "_bridge_request", lambda *args, **kwargs: pytest.fail("bridge reached"))
    with pytest.raises(ToolError, match="allow-list"):
        whatsapp.get_profile(PHONE)


def test_profile_rechecks_the_resolved_jid(monkeypatch):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([PHONE]))

    def request(method, path, **kwargs):
        return _payload(jid="5511888888888@s.whatsapp.net")

    monkeypatch.setattr(whatsapp, "_bridge_request", request)
    monkeypatch.setattr(whatsapp, "_bridge_json", lambda response: response)
    with pytest.raises(ToolError, match="allow-list"):
        whatsapp.get_profile(PHONE)


def test_profile_refuses_a_group(monkeypatch):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.unrestricted())
    monkeypatch.setattr(whatsapp, "_bridge_request", lambda *args, **kwargs: pytest.fail("bridge reached"))
    with pytest.raises(ToolError, match="phone"):
        whatsapp.get_profile("120363000000000001@g.us")


def test_read_media_opens_a_profile_picture_without_a_message_row(paired_dbs, monkeypatch):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.unrestricted())
    directory = media_inventory.chat_media_dir(ALICE)
    os.makedirs(directory, exist_ok=True)
    name = "image_20260904_100000_profile-picture.jpg"
    with open(os.path.join(directory, name), "wb") as handle:
        handle.write(PNG)
    monkeypatch.setattr(whatsapp, "download_media", lambda *args, **kwargs: pytest.fail("cdn download"))
    blocks = media_read.read_media(ALICE, "profile-picture")
    assert blocks[0].type == "image"
    assert base64.b64decode(blocks[0].data) == PNG
    with pytest.raises(ToolError) as missing:
        media_read.read_media(ALICE, "profile-picture-missing")
    assert missing.value.code == "not_found"
    os.remove(os.path.join(directory, name))
    with pytest.raises(ToolError) as gone:
        media_read.read_media(ALICE, "profile-picture")
    assert gone.value.code == "not_found"
