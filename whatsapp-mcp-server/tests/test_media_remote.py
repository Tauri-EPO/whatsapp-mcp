"""The remote reader uses the HTTP path and never needs an S3 credential."""

import hashlib
import os

import httpx
import pytest

import media_remote
import whatsapp
from errors import ToolError

CHAT = "5511999999999@s.whatsapp.net"


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
        assert open(path, "rb").read() == payload
    assert not os.path.exists(path)
    assert seen[0].url.params["chat_jid"] == CHAT
    with pytest.raises(ToolError, match="limit"):
        with media_remote.local_file(media_remote.uri(CHAT, "REMOTE1"), len(payload) - 1):
            pytest.fail("oversize file was exposed")
    with pytest.raises(ToolError, match="SHA256"):
        with media_remote.local_file(media_remote.uri(CHAT, "REMOTE1"), len(payload), "00" * 32):
            pytest.fail("mismatching file was exposed")
