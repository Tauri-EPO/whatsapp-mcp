"""Runtime webhook status uses bridge REST only; detail never widens credentials."""

import json

import httpx
import pytest

import main
import whatsapp


def view(configured=False, enabled=False, reason="runtime webhook disabled", **extra):
    return {
        "configured": configured,
        "enabled": enabled,
        "consecutive_failures": 0,
        "reason": reason,
        "url": "",
        "last_success": None,
        "queued": 0,
        "dropped": 0,
        **extra,
    }


STATES = [
    view(),
    view(reason="runtime webhook has no destination"),
    view(True, True, "", url="https://example.com/…abcd", last_success="2026-11-17T12:00:00Z", queued=2),
    view(True, False, "http_503", url="https://example.com/…abcd", consecutive_failures=50, dropped=3),
]
COMPACT_FIELDS = ("configured", "enabled", "consecutive_failures", "reason")


@pytest.fixture(autouse=True)
def isolated(monkeypatch, tmp_path):
    monkeypatch.delenv(main.STATUS_DETAIL_ENV, raising=False)
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", str(tmp_path / "whatsapp.db"))


def bridge(monkeypatch, webhook, paired=True, connected=True):
    calls = []

    def get(url, **kwargs):
        calls.append(url)
        if url.endswith("/health"):
            payload = {"paired": paired, "connected": connected}
            if webhook is not None:
                payload["webhook"] = webhook
            return httpx.Response(200, json=payload)
        if url.endswith("/version") or url.endswith("/me"):
            return httpx.Response(503, json={})
        raise AssertionError(f"unexpected endpoint: {url}")

    monkeypatch.setattr(whatsapp.bridge_http, "get", get)
    return calls


@pytest.mark.parametrize("webhook", STATES)
@pytest.mark.parametrize("detail,allowed", [(False, True), (True, True), (True, False)])
@pytest.mark.parametrize("paired,connected", [(True, True), (True, False), (False, False)])
def test_webhook_states_and_detail_policy(monkeypatch, webhook, detail, allowed, paired, connected):
    calls = bridge(monkeypatch, webhook, paired, connected)
    monkeypatch.setenv(main.STATUS_DETAIL_ENV, str(allowed))
    out = main.bridge_status(detail=detail)
    assert out["webhook"] == (webhook if detail and allowed else {key: webhook[key] for key in COMPACT_FIELDS})
    assert out["ok"] is (paired and connected)
    assert "error" not in out
    assert all("/operator/" not in url for url in calls)
    if not detail or not allowed:
        assert len(calls) == 1 and calls[0].endswith("/health")
        assert "url" not in out["webhook"]


@pytest.mark.parametrize("detail", [False, True])
def test_older_bridge_returns_explicit_unknown_webhook(monkeypatch, detail):
    bridge(monkeypatch, None)
    out = main.bridge_status(detail=detail)
    assert out["ok"] is True
    assert out["webhook"] == whatsapp._webhook_status(None)
    assert out["webhook"]["configured"] is None
    assert "bridge did not return the view" in out["webhook"]["reason"]


@pytest.mark.parametrize("detail", [False, True])
def test_unreachable_bridge_returns_explicit_unknown_webhook(monkeypatch, detail):
    def down(*_args, **_kwargs):
        raise httpx.ConnectError("refused")

    monkeypatch.setattr(whatsapp.bridge_http, "get", down)
    monkeypatch.setattr(whatsapp.time, "sleep", lambda _: None)
    out = main.bridge_status(detail=detail)
    assert out["ok"] is False
    assert out["webhook"]["configured"] is None
    assert "bridge unreachable" in out["webhook"]["reason"]
    assert "error" not in out


@pytest.mark.parametrize("detail", [False, True])
def test_extra_operator_fields_never_reach_tool(monkeypatch, detail):
    sanitized = STATES[2]
    bridge(
        monkeypatch,
        {
            **sanitized,
            "secret": "fake-secret",
            "authorization": "Bearer fake-auth",
            "secret_fingerprint": "fake-fingerprint",
        },
    )
    out = main.bridge_status(detail=detail)
    serialized = json.dumps(out)
    for private in ("fake-secret", "Bearer fake-auth", "fake-fingerprint", "secret_fingerprint", "authorization"):
        assert private not in serialized
    assert out["webhook"] == (sanitized if detail else {key: sanitized[key] for key in COMPACT_FIELDS})


@pytest.mark.parametrize("payload", [{}, {"configured": True}, "invalid"])
def test_incomplete_view_is_explicitly_unavailable(monkeypatch, payload):
    bridge(monkeypatch, payload)
    assert main.bridge_status()["webhook"] == whatsapp._webhook_status(None)
