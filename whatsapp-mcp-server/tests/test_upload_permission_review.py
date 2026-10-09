"""Uploads stay usable on writable filesystems that refuse chmod/fchmod."""

import errno
import logging
import os
from pathlib import Path

import pytest

import chat_policy
import media_upload
import private_files
import whatsapp
from tests.conftest import ALICE
from tests.test_http_upload import HEADERS, client
from tests.test_send_inline import DummyResponse


@pytest.mark.skipif(os.name != "posix", reason="POSIX permissions")
@pytest.mark.usefixtures("auth_runtime_store")
def test_inline_and_http_uploads_continue_when_directory_chmod_is_unsupported(tmp_path, monkeypatch, caplog):
    outbox = tmp_path / "outbox"
    root = outbox / ".uploads"
    root.mkdir(parents=True)
    monkeypatch.setenv("WHATSAPP_MEDIA_ROOTS", str(outbox))
    monkeypatch.setenv("WHATSAPP_MCP_TRANSPORT", "http")
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.load_chat_policy({}))
    private_files._warn_permissions.cache_clear()
    original_mkdir = os.mkdir

    def mkdir_without_modes(path, mode=0o777, **kwargs):
        return original_mkdir(path, 0o755, **kwargs)

    def denied(*_args, **_kwargs):
        raise PermissionError(errno.EPERM, "synthetic private path")

    monkeypatch.setattr(os, "mkdir", mkdir_without_modes)
    monkeypatch.setattr(os, "chmod", denied)
    monkeypatch.setattr(os, "fchmod", denied)
    received = []

    def bridge(_method, _endpoint, **kwargs):
        received.append(Path(kwargs["json"]["media_path"]).read_bytes())
        return DummyResponse()

    monkeypatch.setattr(whatsapp, "_bridge_request", bridge)
    with caplog.at_level(logging.WARNING, logger="whatsapp_mcp"):
        inline = media_upload.write_inline(b"inline-synthetic", "report.pdf")
        assert Path(inline).read_bytes() == b"inline-synthetic"
        media_upload.discard(inline)
        with client(stateless_http=True, json_response=True) as c:
            response = c.post("/upload", content=b"http-synthetic", headers=HEADERS)
            assert response.status_code == 200
            whatsapp.send_file(ALICE, upload_id=response.json()["upload_id"])
    assert received == [b"http-synthetic"] and list(root.iterdir()) == []
    assert sum("upload directory" in row.message for row in caplog.records) == 1
    assert all(
        str(tmp_path) not in row.message and "synthetic private path" not in row.message for row in caplog.records
    )
