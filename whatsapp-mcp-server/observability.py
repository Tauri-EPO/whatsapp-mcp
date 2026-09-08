"""Structured logs and a Prometheus text endpoint, without extra dependencies.

WHATSAPP_MCP_LOG_FORMAT=json switches the stderr log to one JSON object per
line ({"ts","level","logger","msg"}), which log shippers parse without regex.

/metrics (served by MetricsMiddleware in front of the MCP app, unauthenticated
like the bridge's /api/version: it exposes counts, never content) reports
tool calls and errors per tool, a per-tool latency histogram, HTTP requests by
status class, and process uptime in the Prometheus text exposition format.
"""

from __future__ import annotations

import bisect
import hmac
import json
import logging
import threading
import time
from collections import Counter, defaultdict
from collections.abc import Callable
from typing import Any

JSON_FORMAT_ENV = "WHATSAPP_MCP_LOG_FORMAT"
METRICS_TOKEN_ENV = "WHATSAPP_MCP_METRICS_TOKEN"

# Upper bounds (seconds) of the tool-duration histogram, from a cached DB read
# to the slowest call this server can make: the tail reaches WHISPER_TIMEOUT_S
# (300 s by default) for transcribe_audio and the 120 s bridge transfer for a
# media upload or download, so the bounds go that far — a tool whose every call
# lands in +Inf has no quantile at all. Fixed: a bucket set that changes
# between deploys makes histogram_quantile() lie across the change.
DURATION_BUCKETS: tuple[float, ...] = (
    0.005,
    0.025,
    0.1,
    0.25,
    0.5,
    1.0,
    2.5,
    5.0,
    10.0,
    30.0,
    60.0,
    120.0,
    300.0,
)


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
    """Process-wide counters. Thread-safe; cheap enough to touch on every call.

    Every label is drawn from a closed set: the tool name (the only names
    reaching ``record_tool`` are the decorated tool functions of ``@tool_errors``
    and the registered tools ``strict_args`` refuses arguments for), the error
    codes of the envelope, the histogram bounds and the HTTP status class. Chat
    JIDs, message IDs and query text never become labels, so the series count
    is bounded by the tool list however much traffic the server sees.
    """

    def __init__(self, clock: Callable[[], float] = time.time) -> None:
        self._lock = threading.Lock()
        self._clock = clock
        self.started_at = clock()
        self.tool_calls: Counter[str] = Counter()
        self.tool_errors: Counter[tuple[str, str]] = Counter()
        self.tool_seconds: dict[str, float] = defaultdict(float)
        # Per tool, one slot per bucket plus a last slot for everything above
        # the largest bound; rendered cumulatively (Prometheus "le" semantics).
        self.tool_duration_buckets: dict[str, list[int]] = defaultdict(lambda: [0] * (len(DURATION_BUCKETS) + 1))
        self.http_requests: Counter[str] = Counter()  # by status class: 2xx, 4xx, 5xx

    def record_tool(self, name: str, seconds: float, error_code: str | None) -> None:
        """Count one finished call (successful or not) and place its duration."""
        slot = bisect.bisect_left(DURATION_BUCKETS, seconds)
        with self._lock:
            self.tool_calls[name] += 1
            self.tool_seconds[name] += seconds
            self.tool_duration_buckets[name][slot] += 1
            if error_code:
                self.tool_errors[(name, error_code)] += 1

    def record_http(self, status: int) -> None:
        with self._lock:
            self.http_requests[f"{status // 100}xx"] += 1

    def _duration_lines(self) -> list[str]:
        """The histogram series, cumulative per tool. Caller holds the lock."""
        lines: list[str] = []
        for tool, slots in sorted(self.tool_duration_buckets.items()):
            running = 0
            for bound, count in zip(DURATION_BUCKETS, slots[:-1], strict=True):
                running += count
                lines.append(f'whatsapp_mcp_tool_duration_seconds_bucket{{tool="{tool}",le="{bound:g}"}} {running}')
            running += slots[-1]
            lines.append(f'whatsapp_mcp_tool_duration_seconds_bucket{{tool="{tool}",le="+Inf"}} {running}')
            lines.append(
                f'whatsapp_mcp_tool_duration_seconds_sum{{tool="{tool}"}} {self.tool_seconds.get(tool, 0.0):.3f}'
            )
            lines.append(f'whatsapp_mcp_tool_duration_seconds_count{{tool="{tool}"}} {running}')
        return lines

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
                "# HELP whatsapp_mcp_tool_duration_seconds Wall-clock duration of tool invocations, by tool.",
                "# TYPE whatsapp_mcp_tool_duration_seconds histogram",
                *self._duration_lines(),
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
