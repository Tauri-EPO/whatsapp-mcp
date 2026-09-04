"""Structured logs and a Prometheus text endpoint, without extra dependencies.

WHATSAPP_MCP_LOG_FORMAT=json switches the stderr log to one JSON object per
line ({"ts","level","logger","msg"}), which log shippers parse without regex.

/metrics (served by MetricsMiddleware in front of the MCP app, unauthenticated
like the bridge's /api/version: it exposes counts, never content) reports
tool calls and errors per tool, HTTP requests by status class, and process
uptime in the Prometheus text exposition format.
"""

from __future__ import annotations

import hmac
import json
import logging
import threading
import time
from collections import Counter
from collections.abc import Callable
from typing import Any

JSON_FORMAT_ENV = "WHATSAPP_MCP_LOG_FORMAT"
METRICS_TOKEN_ENV = "WHATSAPP_MCP_METRICS_TOKEN"


class JSONFormatter(logging.Formatter):
    """One JSON object per record; exceptions land in "exc"."""

    def format(self, record: logging.LogRecord) -> str:
        body: dict[str, Any] = {
            "ts": time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(record.created)) + f".{int(record.msecs):03d}Z",
            "level": record.levelname,
            "logger": record.name,
            "msg": record.getMessage(),
        }
        if record.exc_info:
            body["exc"] = self.formatException(record.exc_info)
        return json.dumps(body, ensure_ascii=False)


def log_formatter(value: str | None) -> logging.Formatter:
    """The stderr formatter for WHATSAPP_MCP_LOG_FORMAT (json | text, default text)."""
    if (value or "").strip().lower() == "json":
        return JSONFormatter()
    return logging.Formatter("%(asctime)s %(levelname)s %(name)s: %(message)s")


class Metrics:
    """Process-wide counters. Thread-safe; cheap enough to touch on every call."""

    def __init__(self, clock: Callable[[], float] = time.time) -> None:
        self._lock = threading.Lock()
        self._clock = clock
        self.started_at = clock()
        self.tool_calls: Counter[str] = Counter()
        self.tool_errors: Counter[tuple[str, str]] = Counter()
        self.tool_seconds: Counter[str] = Counter()
        self.http_requests: Counter[str] = Counter()  # by status class: 2xx, 4xx, 5xx

    def record_tool(self, name: str, seconds: float, error_code: str | None) -> None:
        with self._lock:
            self.tool_calls[name] += 1
            self.tool_seconds[name] += seconds
            if error_code:
                self.tool_errors[(name, error_code)] += 1

    def record_http(self, status: int) -> None:
        with self._lock:
            self.http_requests[f"{status // 100}xx"] += 1

    def render(self) -> str:
        """Prometheus text exposition (0.0.4)."""
        with self._lock:
            lines = [
                "# HELP whatsapp_mcp_uptime_seconds Seconds since the MCP server started.",
                "# TYPE whatsapp_mcp_uptime_seconds gauge",
                f"whatsapp_mcp_uptime_seconds {self._clock() - self.started_at:.0f}",
                "# HELP whatsapp_mcp_tool_calls_total Tool invocations by tool.",
                "# TYPE whatsapp_mcp_tool_calls_total counter",
                *[f'whatsapp_mcp_tool_calls_total{{tool="{t}"}} {n}' for t, n in sorted(self.tool_calls.items())],
                "# HELP whatsapp_mcp_tool_errors_total Tool invocations that returned the error envelope, by tool and code.",
                "# TYPE whatsapp_mcp_tool_errors_total counter",
                *[
                    f'whatsapp_mcp_tool_errors_total{{tool="{t}",code="{c}"}} {n}'
                    for (t, c), n in sorted(self.tool_errors.items())
                ],
                "# HELP whatsapp_mcp_tool_seconds_total Wall-clock seconds spent inside tools, by tool.",
                "# TYPE whatsapp_mcp_tool_seconds_total counter",
                *[
                    f'whatsapp_mcp_tool_seconds_total{{tool="{t}"}} {s:.3f}'
                    for t, s in sorted(self.tool_seconds.items())
                ],
                "# HELP whatsapp_mcp_http_requests_total HTTP requests on the MCP transport, by status class.",
                "# TYPE whatsapp_mcp_http_requests_total counter",
                *[
                    f'whatsapp_mcp_http_requests_total{{class="{k}"}} {n}'
                    for k, n in sorted(self.http_requests.items())
                ],
            ]
        return "\n".join(lines) + "\n"


metrics = Metrics()


class MetricsMiddleware:
    """Pure-ASGI: answers GET /metrics itself, counts everything else by status.

    With ``token`` set, /metrics needs ``Authorization: Bearer <token>`` (401
    otherwise); Prometheus supports this through ``bearer_token_file``. Without
    a token the endpoint is open, which is fine on loopback or a tailnet but
    not behind Tailscale Funnel (see docs/DOCKER.md).
    """

    def __init__(self, app: Any, registry: Metrics = metrics, path: str = "/metrics", token: str | None = None) -> None:
        self.app = app
        self.registry = registry
        self.path = path
        self.token = (token or "").strip() or None

    async def __call__(self, scope: dict[str, Any], receive: Any, send: Any) -> None:
        if scope.get("type") != "http":
            await self.app(scope, receive, send)
            return
        if scope.get("path") == self.path:
            if scope.get("method") != "GET":
                status, body = 405, b""
            elif not self._authorized(scope):
                status, body = 401, b""
            else:
                status, body = 200, self.registry.render().encode("utf-8")
            headers = [
                (b"content-type", b"text/plain; version=0.0.4; charset=utf-8"),
                (b"content-length", str(len(body)).encode()),
            ]
            if status == 401:
                headers.append((b"www-authenticate", b'Bearer realm="whatsapp-mcp-metrics"'))
            await send({"type": "http.response.start", "status": status, "headers": headers})
            await send({"type": "http.response.body", "body": body})
            return

        status_seen = {"status": 200}

        async def send_wrapper(message: dict[str, Any]) -> None:
            if message.get("type") == "http.response.start":
                status_seen["status"] = int(message.get("status", 200))
            await send(message)

        try:
            await self.app(scope, receive, send_wrapper)
        finally:
            self.registry.record_http(status_seen["status"])

    def _authorized(self, scope: dict[str, Any]) -> bool:
        if self.token is None:
            return True
        for name, value in scope.get("headers") or []:
            if name.lower() == b"authorization":
                presented = value.decode("latin-1").strip()
                if presented[:7].lower() == "bearer " and hmac.compare_digest(presented[7:].strip(), self.token):
                    return True
        return False


def metrics_enabled(value: str | None) -> bool:
    """WHATSAPP_MCP_METRICS (default on): 0/false/off disables the endpoint."""
    return (value or "").strip().lower() not in ("0", "false", "off", "no")
