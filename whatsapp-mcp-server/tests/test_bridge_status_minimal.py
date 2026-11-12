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

MINIMAL_OK = {"ok": True, "paired": True, "connected": True, "transcription_configured": True}


class FakeStatus:
    """Stands in for whatsapp.bridge_status, recording which shape was asked for."""

    def __init__(self, full):
        self.full, self.calls = full, []

    def __call__(self, minimal=False):
        self.calls.append(minimal)
        return dict(self.full)


@pytest.fixture
def full(monkeypatch):
    fake = FakeStatus(FULL_OK)
    monkeypatch.setattr(main, "whatsapp_bridge_status", fake)
    monkeypatch.setattr(main, "transcription_configured", lambda: True)
    monkeypatch.delenv(main.STATUS_DETAIL_ENV, raising=False)
    return fake


def test_default_is_minimal_hides_internals_and_asks_for_the_cheap_shape(full):
    out = main.bridge_status()
    assert out == MINIMAL_OK
    assert not INTERNAL_KEYS & out.keys()
    assert full.calls == [True]


def test_detail_returns_the_full_payload(full):
    assert main.bridge_status(detail=True) == FULL_OK
    assert full.calls == [False]


@pytest.mark.parametrize("value", ["false", "0", "off", "No"])
def test_deployment_can_refuse_detail(full, monkeypatch, value):
    monkeypatch.setenv(main.STATUS_DETAIL_ENV, value)
    out = main.bridge_status(detail=True)
    assert not INTERNAL_KEYS & out.keys()
    assert out["ok"] is True and out["detail"] == "unavailable on this deployment"
    assert main.STATUS_DETAIL_ENV not in out["detail"]
    assert main.bridge_status() == MINIMAL_OK
    assert full.calls == [True, True]


@pytest.mark.parametrize("value", ["", "  ", "true", "1"])
def test_detail_allowed_by_default_and_when_true(full, monkeypatch, value):
    monkeypatch.setenv(main.STATUS_DETAIL_ENV, value)
    assert main.bridge_status(detail=True) == FULL_OK


def test_malformed_switch_is_rejected(monkeypatch):
    monkeypatch.setenv(main.STATUS_DETAIL_ENV, "treu")
    with pytest.raises(ValueError):
        main._status_detail_allowed()


def _minimal_for(monkeypatch, full_status):
    monkeypatch.setattr(main, "whatsapp_bridge_status", FakeStatus(full_status))
    monkeypatch.setattr(main, "transcription_configured", lambda: False)
    monkeypatch.delenv(main.STATUS_DETAIL_ENV, raising=False)
    return main.bridge_status()


def test_restriction_and_pairing_state_with_a_generic_reason(monkeypatch):
    out = _minimal_for(
        monkeypatch,
        {
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
    assert out == {
        "ok": False,
        "paired": True,
        "connected": False,
        "reason": "WhatsApp account restricted (temporarily_banned); the operator must act",
        "pairing_state": "passkey_required",
        "restriction": {"kind": "temporarily_banned", "expires_at": "2026-11-12T10:00:00Z"},
        "transcription_configured": False,
    }


@pytest.mark.parametrize(
    "full_status,reason",
    [
        (
            {"ok": False, "reason": "bridge unreachable at http://bridge.internal:8080/api: [Errno 111] refused"},
            "bridge unreachable",
        ),
        (
            {
                "ok": False,
                "paired": False,
                "connected": False,
                "reason": "Restart the bridge or POST /operator/v1/pairing/restart for a new QR",
            },
            "not paired with a phone yet; the operator must pair it",
        ),
        (
            {"ok": False, "paired": False, "connected": False, "pairing_state": "expired", "reason": "x"},
            "WhatsApp pairing in progress; the operator must complete it",
        ),
        (
            {"ok": False, "paired": True, "connected": False, "reason": "bridge is paired but disconnected"},
            "paired but disconnected from WhatsApp; it reconnects automatically",
        ),
    ],
)
def test_minimal_reason_never_carries_urls_exceptions_or_operator_paths(monkeypatch, full_status, reason):
    out = _minimal_for(monkeypatch, full_status)
    assert out["reason"] == reason
    for leak in ("http://", "Errno", "/operator/", "bridge.internal"):
        assert leak not in out["reason"]


def test_non_blocking_problem_is_not_a_restriction(monkeypatch):
    out = _minimal_for(
        monkeypatch,
        {"ok": True, "paired": True, "connected": True, "connection_problem": {"kind": "stream_replaced", "code": 0}},
    )
    assert "restriction" not in out and "reason" not in out


def test_minimal_shape_makes_no_probe_and_reads_no_notes(monkeypatch, tmp_path):
    """End to end through whatsapp.bridge_status(minimal=True) with the bridge unreachable."""

    def forbidden(*_a, **_k):
        raise AssertionError("the minimal answer must not probe or read notes.db")

    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", str(tmp_path / "whatsapp.db"))
    monkeypatch.setattr(whatsapp, "WHATSAPP_API_BASE_URL", "http://127.0.0.1:9/api")
    monkeypatch.setattr(whatsapp.time, "sleep", lambda _s: None)
    monkeypatch.setattr(whatsapp, "_whisper_status", forbidden)
    monkeypatch.setattr(whatsapp, "_endpoint_cert_status", forbidden)
    monkeypatch.setattr(whatsapp, "owner_identity", forbidden)
    monkeypatch.delenv("WHATSAPP_PUBLIC_URL", raising=False)
    monkeypatch.delenv(main.STATUS_DETAIL_ENV, raising=False)
    out = main.bridge_status()
    assert out["ok"] is False and out["reason"] == "bridge unreachable"
    assert not INTERNAL_KEYS & out.keys()
    assert isinstance(out["transcription_configured"], bool)


def test_transcription_configured_reads_configuration_only():
    import transcribe

    assert transcribe.transcription_configured({}) is False
    assert transcribe.transcription_configured({"WHISPER_URL": "http://whisper.example:8080/inference"}) is True
    assert transcribe.transcription_configured({"WHISPER_BIN": "/usr/bin/whisper-cli"}) is True
