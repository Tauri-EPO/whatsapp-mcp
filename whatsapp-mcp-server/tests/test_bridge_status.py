"""bridge_status: reachable/paired/connected/version, never raises."""

import httpx
import pytest

import main
import whatsapp


@pytest.fixture(autouse=True)
def _no_local_device(monkeypatch, tmp_path):
    """No whatsapp.db in reach, so the identity below is the bridge's answer."""
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", str(tmp_path / "whatsapp.db"))


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
                "/me": Resp(
                    200,
                    {
                        "phone_jid": "5511999999999@s.whatsapp.net",
                        "phone": "5511999999999",
                        "lid_jid": "158883943301358@lid",
                        "lid": "158883943301358",
                    },
                ),
            }
        ),
    )
    out = main.bridge_status()
    assert out["ok"] is True and out["connected"] and out["paired"] and out["uptime_seconds"] == 42
    assert out["version"]["commit"] == "abc" and out["version"]["fts5"] is True
    assert out["media_files"] == 1
    # Who the account is: an agent needs the LID to recognise a mention of itself.
    assert out["owner"] == {
        "jid": "5511999999999@s.whatsapp.net",
        "phone": "5511999999999",
        "lid": "158883943301358",
    }


def test_awaiting_pairing_reports_reason(monkeypatch):
    monkeypatch.setattr(
        whatsapp.bridge_http,
        "get",
        _get(
            {
                "/health": Resp(200, {"status": "awaiting_pairing", "connected": True, "paired": False}),
                "/version": Resp(500, {}),
                "/me": Resp(503, {"message": "WhatsApp account is not paired yet"}),
            }
        ),
    )
    out = main.bridge_status()
    # No identity before pairing: the key is absent rather than null.
    assert out["ok"] is False and "QR" in out["reason"] and "version" not in out and "owner" not in out


def test_unreachable_never_raises(monkeypatch):
    def down(url, **kwargs):
        raise httpx.ConnectError("refused")

    monkeypatch.setattr(whatsapp.bridge_http, "get", down)
    monkeypatch.setattr(whatsapp.time, "sleep", lambda s: None)
    out = main.bridge_status()
    assert out["ok"] is False and "unreachable" in out["reason"] and "error" not in out
    assert "owner" not in out
