"""JSON log formatter, tool metrics and the /metrics ASGI endpoint."""

import asyncio
import json
import logging

import main
import observability
from errors import ToolError, tool_errors
from observability import JSONFormatter, Metrics, MetricsMiddleware, log_formatter, metrics_enabled


def test_json_formatter_line():
    rec = logging.LogRecord("whatsapp_mcp", logging.WARNING, __file__, 1, "hello %s", ("x",), None)
    out = json.loads(JSONFormatter().format(rec))
    assert out["level"] == "WARNING" and out["logger"] == "whatsapp_mcp" and out["msg"] == "hello x"
    assert out["ts"].endswith("Z")
    assert isinstance(log_formatter("json"), JSONFormatter)
    assert not isinstance(log_formatter(None), JSONFormatter)


def test_tool_decorator_records_metrics(monkeypatch):
    reg = Metrics()
    monkeypatch.setattr(observability, "metrics", reg)

    @tool_errors
    def fine():
        return {"ok": True}

    @tool_errors
    def denied():
        raise ToolError("denied", "no")

    fine()
    fine()
    denied()
    text = reg.render()
    assert 'whatsapp_mcp_tool_calls_total{tool="fine"} 2' in text
    assert 'whatsapp_mcp_tool_calls_total{tool="denied"} 1' in text
    assert 'whatsapp_mcp_tool_errors_total{tool="denied",code="denied"} 1' in text
    assert "whatsapp_mcp_uptime_seconds" in text


def _run(app, scope):
    sent = []

    async def receive():
        return {"type": "http.request", "body": b"", "more_body": False}

    async def send(message):
        sent.append(message)

    asyncio.run(app(scope, receive, send))
    return sent


def test_metrics_endpoint_and_status_counting():
    reg = Metrics()

    async def inner(scope, receive, send):
        await send({"type": "http.response.start", "status": 401, "headers": []})
        await send({"type": "http.response.body", "body": b"nope"})

    app = MetricsMiddleware(inner, reg)
    _run(app, {"type": "http", "method": "POST", "path": "/mcp"})
    sent = _run(app, {"type": "http", "method": "GET", "path": "/metrics"})
    start, body = sent[0], sent[1]
    assert start["status"] == 200
    assert dict(start["headers"])[b"content-type"].startswith(b"text/plain; version=0.0.4")
    text = body["body"].decode()
    assert 'whatsapp_mcp_http_requests_total{class="4xx"} 1' in text
    assert int(dict(start["headers"])[b"content-length"]) == len(body["body"])
    sent = _run(app, {"type": "http", "method": "POST", "path": "/metrics"})
    assert sent[0]["status"] == 405 and sent[1]["body"] == b""


def test_metrics_toggle_and_app_wiring(monkeypatch):
    assert metrics_enabled(None) and metrics_enabled("1") and not metrics_enabled("off") and not metrics_enabled("0")

    class FakeServer:
        def streamable_http_app(self, **kw):
            async def app(scope, receive, send):
                await send({"type": "http.response.start", "status": 200, "headers": []})
                await send({"type": "http.response.body", "body": b""})

            return app

    monkeypatch.delenv("WHATSAPP_MCP_METRICS", raising=False)
    app = main.build_http_app(FakeServer(), "http", None)
    assert isinstance(app, MetricsMiddleware)
    monkeypatch.setenv("WHATSAPP_MCP_METRICS", "off")
    assert not isinstance(main.build_http_app(FakeServer(), "http", None), MetricsMiddleware)
