"""bridge_status().whisper: can this deployment transcribe at all?

The capability is read from the environment plus one cheap liveness check, and
it must never be able to break bridge_status itself.
"""

import httpx
import pytest

import main
import transcribe
import whatsapp


class TestDescribeStatus:
    def test_unconfigured_reports_nothing_usable(self):
        assert transcribe.describe_status({}) == {
            "configured": False,
            "backend": None,
            "reachable": None,
            "model": None,
            "on_ingest": False,
        }

    def test_server_backend_probes_the_configured_url(self):
        probed = []

        def probe(url):
            probed.append(url)
            return True

        status = transcribe.describe_status(
            {"WHISPER_URL": "http://whisper:8178/inference", "TRANSCRIBE_ON_INGEST": "1"}, probe=probe
        )

        assert status["configured"] is True
        assert status["backend"] == "url"
        assert status["reachable"] is True
        assert status["on_ingest"] is True
        assert probed == ["http://whisper:8178/inference"]

    def test_server_backend_down_is_configured_but_unreachable(self):
        status = transcribe.describe_status({"WHISPER_URL": "http://whisper:8178/inference"}, probe=lambda url: False)
        assert status["configured"] is True and status["reachable"] is False

    def test_url_wins_over_bin(self):
        status = transcribe.describe_status(
            {"WHISPER_URL": "http://127.0.0.1:8178/inference", "WHISPER_BIN": "/usr/bin/whisper-cli"},
            probe=lambda url: True,
        )
        assert status["backend"] == "url"

    def test_cli_backend_ready_when_binary_and_model_exist(self, tmp_path):
        binary = tmp_path / "whisper-cli"
        binary.write_bytes(b"#!/bin/sh\n")
        model = tmp_path / "ggml-small.bin"
        model.write_bytes(b"ggml")

        status = transcribe.describe_status({"WHISPER_BIN": str(binary), "WHISPER_MODEL": str(model)})

        assert status["backend"] == "bin"
        assert status["reachable"] is True
        assert status["model"] == str(model)

    def test_cli_backend_without_model_is_unreachable(self, tmp_path):
        binary = tmp_path / "whisper-cli"
        binary.write_bytes(b"#!/bin/sh\n")

        status = transcribe.describe_status({"WHISPER_BIN": str(binary)})

        assert status["configured"] is True and status["reachable"] is False and status["model"] is None

    def test_cli_backend_with_missing_binary_is_unreachable(self, tmp_path):
        model = tmp_path / "ggml-small.bin"
        model.write_bytes(b"ggml")

        status = transcribe.describe_status({"WHISPER_BIN": str(tmp_path / "absent"), "WHISPER_MODEL": str(model)})

        assert status["configured"] is True and status["reachable"] is False

    def test_on_ingest_needs_a_backend_to_be_running(self):
        # The worker refuses to start without one, whatever the variable says.
        status = transcribe.describe_status({"TRANSCRIBE_ON_INGEST": "1"})
        assert status["configured"] is False and status["on_ingest"] is False

    def test_unreadable_on_ingest_value_does_not_lose_the_block(self):
        status = transcribe.describe_status(
            {"WHISPER_URL": "http://whisper:8178/inference", "TRANSCRIBE_ON_INGEST": "maybe"},
            probe=lambda url: True,
        )
        assert status["configured"] is True and status["on_ingest"] is False


class TestProbeServer:
    def test_any_http_answer_means_reachable(self, monkeypatch):
        seen = {}

        def fake_head(url, timeout=None):
            seen["url"], seen["timeout"] = url, timeout
            return object()

        monkeypatch.setattr(transcribe.httpx, "head", fake_head)

        assert transcribe.probe_server("http://whisper:8178/inference") is True
        # The configured URL, path included, and no body either way.
        assert seen["url"] == "http://whisper:8178/inference"
        assert seen["timeout"] == transcribe.STATUS_PROBE_TIMEOUT_S

    def test_transport_failure_means_unreachable(self, monkeypatch):
        def fake_head(url, timeout=None):
            raise httpx.ConnectError("refused")

        monkeypatch.setattr(transcribe.httpx, "head", fake_head)

        assert transcribe.probe_server("http://whisper:8178/inference") is False

    @pytest.mark.parametrize("url", ["not-a-url", "http://[unbalanced/inference", ""])
    def test_an_unusable_url_never_calls_out(self, monkeypatch, url):
        def fake_head(url, timeout=None):
            raise AssertionError("probe should not leave the process")

        monkeypatch.setattr(transcribe.httpx, "head", fake_head)

        assert transcribe.probe_server(url) is False


class Resp:
    def __init__(self, status, payload):
        self.status_code = status
        self._payload = payload
        self.text = ""

    def json(self):
        return self._payload


def _healthy(monkeypatch):
    def fake(url, **kwargs):
        if url.endswith("/health"):
            return Resp(200, {"status": "ok", "connected": True, "paired": True})
        return Resp(500, {})

    monkeypatch.setattr(whatsapp.bridge_http, "get", fake)


class TestBridgeStatusIntegration:
    def test_reports_the_whisper_block(self, monkeypatch):
        _healthy(monkeypatch)
        monkeypatch.setenv("WHISPER_URL", "http://whisper:8178/inference")
        monkeypatch.setattr(transcribe.httpx, "head", lambda url, timeout=None: object())

        out = main.bridge_status()

        assert out["ok"] is True
        assert out["whisper"]["backend"] == "url" and out["whisper"]["reachable"] is True

    def test_a_broken_probe_does_not_break_the_status(self, monkeypatch):
        _healthy(monkeypatch)

        def boom(*args, **kwargs):
            raise RuntimeError("probe exploded")

        monkeypatch.setattr(transcribe, "describe_status", boom)

        out = main.bridge_status()

        assert out["ok"] is True
        assert out["whisper"]["configured"] is False and "probe exploded" in out["whisper"]["error"]

    def test_reported_even_when_the_bridge_is_unreachable(self, monkeypatch):
        def down(url, **kwargs):
            raise httpx.ConnectError("refused")

        monkeypatch.setattr(whatsapp.bridge_http, "get", down)
        monkeypatch.setattr(whatsapp.time, "sleep", lambda s: None)
        monkeypatch.delenv("WHISPER_URL", raising=False)
        monkeypatch.delenv("WHISPER_BIN", raising=False)

        out = main.bridge_status()

        assert out["ok"] is False and out["whisper"]["configured"] is False
