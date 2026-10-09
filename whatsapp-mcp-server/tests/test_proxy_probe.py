"""A DNS failure, timeout or open socket must never count as refusal evidence."""

from __future__ import annotations

import errno
import io
import runpy
import urllib.error
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]


@pytest.mark.parametrize("socket_error", [errno.ECONNREFUSED, errno.ETIMEDOUT, -2, None])
def test_probe_requires_connection_refusal(monkeypatch: pytest.MonkeyPatch, socket_error: int | None) -> None:
    module = runpy.run_path(str(ROOT / "scripts" / "proxy-probe.py"))
    probe = module["probe"]

    class Response(io.BytesIO):
        status = 200
        headers = {"mcp-session-id": "fake-session"}

    def fake_request(alias: str, token: str) -> Response:
        if token != "own-token":
            raise urllib.error.HTTPError("http://example:8000/mcp", 401, "unauthorized", {}, None)
        return Response(b'{"result":{"serverInfo":{"name":"whatsapp"}}}')

    def connect(*args, **kwargs):
        if socket_error is not None:
            raise OSError(socket_error, "fake socket failure")
        return io.BytesIO()

    monkeypatch.setitem(probe.__globals__, "request", fake_request)
    monkeypatch.setattr(module["socket"], "create_connection", connect)
    if socket_error == errno.ECONNREFUSED:
        probe("example", "own-token", "other-token")
    else:
        with pytest.raises(AssertionError):
            probe("example", "own-token", "other-token")
