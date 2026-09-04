"""bridge_status: reachable/paired/connected/version, never raises."""

import httpx

import main
import whatsapp


class Resp:
    def __init__(self, status, payload):
        self.status_code = status
        self._payload = payload
        self.text = ""

    def json(self):
        return self._payload


def _get(mapping):
    def fake(url, **kwargs):
        for suffix, resp in mapping.items():
            if url.endswith(suffix):
                return resp
        raise AssertionError(url)

    return fake


def test_ok_when_paired_and_connected(monkeypatch):
    monkeypatch.setattr(
        whatsapp.bridge_http,
        "get",
        _get(
            {
                "/health": Resp(
                    200,
                    {
                        "status": "ok",
                        "connected": True,
                        "paired": True,
                        "uptime_seconds": 42,
                        "store_bytes": 10,
                        "media_bytes": 4,
                        "media_files": 1,
                    },
                ),
                "/version": Resp(
                    200, {"version": "dev", "commit": "abc", "go": "go1.26", "whatsmeow": "v0", "fts5": True}
                ),
            }
        ),
    )
    out = main.bridge_status()
    assert out["ok"] is True and out["connected"] and out["paired"] and out["uptime_seconds"] == 42
    assert out["version"]["commit"] == "abc" and out["version"]["fts5"] is True
    assert out["media_files"] == 1


def test_awaiting_pairing_reports_reason(monkeypatch):
    monkeypatch.setattr(
        whatsapp.bridge_http,
        "get",
        _get(
            {
                "/health": Resp(200, {"status": "awaiting_pairing", "connected": True, "paired": False}),
                "/version": Resp(500, {}),
            }
        ),
    )
    out = main.bridge_status()
    assert out["ok"] is False and "QR" in out["reason"] and "version" not in out


def test_unreachable_never_raises(monkeypatch):
    def down(url, **kwargs):
        raise httpx.ConnectError("refused")

    monkeypatch.setattr(whatsapp.bridge_http, "get", down)
    monkeypatch.setattr(whatsapp.time, "sleep", lambda s: None)
    out = main.bridge_status()
    assert out["ok"] is False and "unreachable" in out["reason"] and "error" not in out
