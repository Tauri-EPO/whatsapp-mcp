"""bridge_status default shape is minimal; detail=true is opt-in and can be refused (issue #831)."""

import pytest

import main
import whatsapp

FULL_OK = {
    "ok": True,
    "paired": True,
    "connected": True,
    "bridge_url": "http://bridge.internal:8080/api",
    "uptime_seconds": 42,
    "store_bytes": 10,
    "send_usage": {"sends_today": 3},
    "whisper": {"configured": True, "backend": "url", "model": "large"},
    "version": {"version": "dev", "commit": "abc"},
    "owner": {"jid": "5511999999999@s.whatsapp.net", "phone": "5511999999999", "lid": "100000000000001"},
    "transcription_usage": {"seconds": 60, "minutes": 1.0},
}

INTERNAL_KEYS = {
    "bridge_url",
    "uptime_seconds",
    "store_bytes",
    "send_usage",
    "whisper",
    "version",
    "owner",
    "transcription_usage",
    "connection_problem",
    "pairing_action",
}


@pytest.fixture
def full(monkeypatch):
    payload = dict(FULL_OK)
    monkeypatch.setattr(main, "whatsapp_bridge_status", lambda: dict(payload))
    monkeypatch.delenv(main.STATUS_DETAIL_ENV, raising=False)
    return payload


def test_default_is_minimal_and_hides_internals(full):
    out = main.bridge_status()
    assert out == {"ok": True, "paired": True, "connected": True}
    assert not INTERNAL_KEYS & out.keys()


def test_detail_returns_the_full_payload(full):
    assert main.bridge_status(detail=True) == FULL_OK


@pytest.mark.parametrize("value", ["false", "0", "off", "No"])
def test_operator_can_refuse_detail(full, monkeypatch, value):
    monkeypatch.setenv(main.STATUS_DETAIL_ENV, value)
    out = main.bridge_status(detail=True)
    assert not INTERNAL_KEYS & out.keys()
    assert out["ok"] is True and out["detail"].startswith("unavailable")
    assert main.bridge_status() == {"ok": True, "paired": True, "connected": True}


@pytest.mark.parametrize("value", ["", "  ", "true", "1"])
def test_detail_allowed_by_default_and_when_true(full, monkeypatch, value):
    monkeypatch.setenv(main.STATUS_DETAIL_ENV, value)
    assert main.bridge_status(detail=True) == FULL_OK


def test_malformed_switch_is_rejected(monkeypatch):
    monkeypatch.setenv(main.STATUS_DETAIL_ENV, "treu")
    with pytest.raises(ValueError):
        main._status_detail_allowed()


def test_not_ok_keeps_reason_pairing_state_and_restriction(monkeypatch):
    monkeypatch.setattr(
        main,
        "whatsapp_bridge_status",
        lambda: {
            "ok": False,
            "paired": True,
            "connected": False,
            "reason": "WhatsApp connection problem: temporarily_banned; see docs/TROUBLESHOOTING.md",
            "pairing_state": "passkey_required",
            "connection_problem": {
                "kind": "temporarily_banned",
                "code": 402,
                "expires_at": "2026-11-12T10:00:00Z",
                "since": "2026-11-11T10:00:00Z",
                "temp_ban_reason": 101,
            },
            "bridge_url": "http://bridge.internal:8080/api",
        },
    )
    out = main.bridge_status()
    assert out == {
        "ok": False,
        "paired": True,
        "connected": False,
        "reason": "WhatsApp connection problem: temporarily_banned; see docs/TROUBLESHOOTING.md",
        "pairing_state": "passkey_required",
        "restriction": {"kind": "temporarily_banned", "expires_at": "2026-11-12T10:00:00Z"},
    }


def test_unreachable_bridge_minimal_shape(monkeypatch):
    monkeypatch.setattr(
        main,
        "whatsapp_bridge_status",
        lambda: {"ok": False, "reason": "bridge unreachable", "bridge_url": "http://x/api"},
    )
    assert main.bridge_status() == {"ok": False, "paired": False, "connected": False, "reason": "bridge unreachable"}


def test_non_blocking_problem_is_not_a_restriction(monkeypatch):
    monkeypatch.setattr(
        main,
        "whatsapp_bridge_status",
        lambda: {
            "ok": True,
            "paired": True,
            "connected": True,
            "connection_problem": {"kind": "stream_replaced", "code": 0},
        },
    )
    assert "restriction" not in main.bridge_status()


def test_real_full_status_projects_without_internals(monkeypatch, tmp_path):
    """End to end through whatsapp.bridge_status with the bridge unreachable."""
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", str(tmp_path / "whatsapp.db"))
    monkeypatch.setattr(whatsapp, "WHATSAPP_API_BASE_URL", "http://127.0.0.1:9/api")
    out = main.bridge_status()
    assert out["ok"] is False and out["reason"]
    assert not INTERNAL_KEYS & out.keys()
