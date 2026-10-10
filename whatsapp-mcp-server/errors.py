"""One error shape for every tool.

Success returns the payload the tool documents. Failure returns::

    {"error": {"code": "<code>", "message": "<human readable>"}}

codes: ``not_found`` (the chat/message/contact does not exist in the archive),
``rate_limited`` (send budget exhausted; stop and report instead of retrying,
with ``retry_after_s`` and the limit name),

``transcription_quota_exceeded`` (the audio does not fit the capped monthly quota),
``denied`` (WHATSAPP_ALLOWED_CHATS blocks the target), ``bridge_unavailable``
(the bridge REST API could not be reached or answered 5xx),
``media_unavailable`` (the bytes are not cached here and nothing can fetch them:
the sender's phone no longer has them, or the message was stored without the CDN
fields — retrying will not help),
``media_refused`` (the message identity cannot safely name a cache file;
retrying this row will not help, but other copies remain fetchable),
``invalid_argument`` (bad input), ``conflict`` (the target changed since the
caller read it; re-read and retry), ``too_large`` (the answer would not fit: the
payload carries the real size and the limit that was applied), ``internal``
(unexpected failure, details in the server log).

An agent that sees an unexpected empty result should call ``bridge_status``;
an unreadable database is reported as ``internal``, never as an empty account.
"""

from __future__ import annotations

import functools
import inspect
import json
import logging
import time
from collections.abc import Callable
from typing import Any

from mcp_types import CallToolResult, TextContent

MEDIA_REFUSED_CODE = "media_refused"

ERROR_CODES = (
    "rate_limited",
    "transcription_quota_exceeded",
    "not_found",
    "denied",
    "bridge_unavailable",
    "media_unavailable",
    MEDIA_REFUSED_CODE,
    "invalid_argument",
    "conflict",
    "too_large",
    "internal",
)

logger = logging.getLogger("whatsapp_mcp")


class ToolError(Exception):
    """Raised anywhere below a tool to produce the error envelope."""

    # Internal fetch receipt, never serialized: None means not attempted.
    _media_refusal_recorded: bool | None = None

    def __init__(self, code: str, message: str, **extra: Any) -> None:
        if code not in ERROR_CODES:
            raise ValueError(f"unknown error code {code!r}")
        super().__init__(message)
        self.code = code
        self.message = message
        self.extra = extra

    def to_dict(self) -> dict[str, Any]:
        body: dict[str, Any] = {"error": {"code": self.code, "message": self.message}}
        body.update(self.extra)
        return body


def error(code: str, message: str, **extra: Any) -> dict[str, Any]:
    """Build the envelope without raising."""
    return ToolError(code, message, **extra).to_dict()


def tool_error_result(envelope: dict[str, Any]) -> CallToolResult:
    """Carry the common failure on both MCP channels, with the error flag set."""
    return CallToolResult(
        content=[TextContent(type="text", text=json.dumps(envelope, ensure_ascii=False))],
        structured_content=envelope,
        is_error=True,
    )


def structured_errors(fn: Callable[..., Any]) -> Callable[..., Any]:
    """Convert failure envelopes before the SDK validates the success schema.

    A list-returning tool's success schema cannot accept an error dictionary.
    The explicit error result bypasses success validation and preserves its code;
    dict-returning tools and content-block tools use the same representation.
    Return annotations, argument signatures and successful values stay intact.
    """

    def convert(result: Any) -> Any:
        if isinstance(result, dict) and "error" in result:
            return tool_error_result(result)
        return result

    if inspect.iscoroutinefunction(fn):

        @functools.wraps(fn)
        async def async_wrapper(*args: Any, **kwargs: Any) -> Any:
            return convert(await fn(*args, **kwargs))

        return async_wrapper

    @functools.wraps(fn)
    def wrapper(*args: Any, **kwargs: Any) -> Any:
        return convert(fn(*args, **kwargs))

    return wrapper


def tool_errors(fn: Callable[..., Any]) -> Callable[..., Any]:
    """Decorator for MCP tools: map exceptions to the envelope.

    ToolError → its envelope; ValueError → invalid_argument; anything else →
    internal (logged with traceback, message kept generic).
    """

    @functools.wraps(fn)
    def wrapper(*args: Any, **kwargs: Any) -> Any:
        from observability import mcp_metrics_owned, metrics  # local import avoids a module cycle

        started = time.monotonic()
        code: str | None = None
        try:
            return fn(*args, **kwargs)
        except ToolError as exc:
            code = exc.code
            return exc.to_dict()
        except ValueError as exc:
            code = "invalid_argument"
            return error("invalid_argument", str(exc))
        except Exception as exc:  # noqa: BLE001 - last line of defence for a tool call
            code = "internal"
            logger.exception("%s failed", fn.__name__)
            return error("internal", f"{type(exc).__name__}: {exc}")
        finally:
            if not mcp_metrics_owned.get():
                metrics.record_tool(fn.__name__, time.monotonic() - started, code)

    return wrapper
