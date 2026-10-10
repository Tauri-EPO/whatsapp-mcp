"""The remote reader uses the HTTP path and never needs an S3 credential."""

import hashlib
import os

import httpx
import pytest

import media_remote
import whatsapp
from errors import ToolError

CHAT = "5511999999999@s.whatsapp.net"


@pytest.mark.parametrize("code", ["media_unavailable", "media_refused", "bridge_unavailable"])
def test_stream_errors_preserve_permanent_codes_without_remote_diagnostics(monkeypatch, code):
    def handle(request):
        return httpx.Response(502, json={"error": {"code": code, "message": "fake-private-sentinel"}})

    monkeypatch.setattr(whatsapp, "bridge_http", httpx.Client(transport=httpx.MockTransport(handle)))
    with pytest.raises(media_remote.MediaReadError) as failure:
        with media_remote.local_file(media_remote.uri(CHAT, "REMOTE1"), 1024):
            pytest.fail("error became media bytes")
    assert failure.value.code == code
    assert "fake-private-sentinel" not in str(failure.value)


def test_stream_error_body_is_bounded(monkeypatch):
    def handle(request):
        return httpx.Response(502, content=b"x" * 9000)

    monkeypatch.setattr(whatsapp, "bridge_http", httpx.Client(transport=httpx.MockTransport(handle)))
    with pytest.raises(media_remote.MediaReadError) as failure:
        with media_remote.local_file(media_remote.uri(CHAT, "REMOTE1"), 1024):
            pytest.fail("oversized error became media bytes")
    assert failure.value.code == "bridge_unavailable"


def test_identity_batches_are_bounded_and_deduplicated(monkeypatch):
    batches = []

    def handle(request):
        ids = request.url.params.get_list("message_id")
        assert request.url.params["chat_jid"] == CHAT and 0 < len(ids) <= 256
        assert "cursor" not in request.url.params
        batches.append(ids)
        return httpx.Response(
            200, json={"items": [{"message_id": message_id, "bytes": 4} for message_id in ids], "next_cursor": ""}
        )

    monkeypatch.setattr(whatsapp, "bridge_http", httpx.Client(transport=httpx.MockTransport(handle)))
    ids = [f"BATCH{index}" for index in range(513)]
    result = media_remote.lookup_many(CHAT, [*ids, ids[0]])
    assert list(result) == ids and [len(batch) for batch in batches] == [256, 256, 1]
    assert media_remote.lookup_many(CHAT, []) == {} and len(batches) == 3


def test_lookup_requests_one_identity_without_chat_scan(monkeypatch):
    seen = []

    def handle(request):
        seen.append(request)
        assert request.url.params["message_id"] == "REMOTE1"
        return httpx.Response(200, json={"items": [{"message_id": "REMOTE1", "bytes": 4}], "next_cursor": ""})

    monkeypatch.setattr(whatsapp, "bridge_http", httpx.Client(transport=httpx.MockTransport(handle)))
    assert media_remote.lookup(CHAT, "REMOTE1")["bytes"] == 4
    assert len(seen) == 1


def test_cached_send_validates_with_one_identity_lookup(monkeypatch):
    monkeypatch.setenv("WHATSAPP_MEDIA_BACKEND", "s3")
    seen = []

    def handle(request):
        seen.append(request)
        assert request.url.params["message_id"] == "REMOTE1"
        return httpx.Response(200, json={"items": [{"message_id": "REMOTE1", "bytes": 4}], "next_cursor": ""})

    monkeypatch.setattr(whatsapp, "bridge_http", httpx.Client(transport=httpx.MockTransport(handle)))
    success, _, _ = whatsapp.send_file(CHAT, media_remote.uri(CHAT, "REMOTE1"), dry_run=True)
    assert success and len(seen) == 1


@pytest.mark.parametrize("backend,want", [("local", False), ("", False), ("s3", True), (" s3 ", True)])
def test_backend_matches_bridge_whitespace_parsing(monkeypatch, backend, want):
    monkeypatch.setenv("WHATSAPP_MEDIA_BACKEND", backend)
    assert media_remote.enabled() is want


def test_bounded_remote_spool_cleanup_and_hash(monkeypatch):
    payload = b"remote media bytes"
    seen = []

    def handle(request):
        seen.append(request)
        return httpx.Response(200, content=payload)

    monkeypatch.setattr(whatsapp, "bridge_http", httpx.Client(transport=httpx.MockTransport(handle)))
    with media_remote.local_file(
        media_remote.uri(CHAT, "REMOTE1"), len(payload), hashlib.sha256(payload).hexdigest()
    ) as path:
        with open(path, "rb") as source:
            assert source.read() == payload
    assert not os.path.exists(path)
    assert seen[0].url.params["chat_jid"] == CHAT
    with pytest.raises(ToolError, match="limit"):
        with media_remote.local_file(media_remote.uri(CHAT, "REMOTE1"), len(payload) - 1):
            pytest.fail("oversize file was exposed")
    with pytest.raises(ToolError, match="SHA256"):
        with media_remote.local_file(media_remote.uri(CHAT, "REMOTE1"), len(payload), "00" * 32):
            pytest.fail("mismatching file was exposed")
