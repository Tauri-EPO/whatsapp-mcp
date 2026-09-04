"""One error shape for every tool.

Success returns the payload the tool documents. Failure returns::

    {"error": {"code": "<code>", "message": "<human readable>"}}

codes: ``not_found`` (the chat/message/contact does not exist in the archive),
``denied`` (WHATSAPP_ALLOWED_CHATS blocks the target), ``bridge_unavailable``
(the bridge REST API could not be reached or answered 5xx), ``invalid_argument``
(bad input), ``internal`` (unexpected failure, details in the server log).

An agent that sees an unexpected empty result should call ``bridge_status``;
an unreadable database is reported as ``internal``, never as an empty account.
"""

from __future__ import annotations

import functools
import logging
import time
from collections.abc import Callable
from typing import Any

ERROR_CODES = ("not_found", "denied", "bridge_unavailable", "invalid_argument", "internal")

logger = logging.getLogger("whatsapp_mcp")


class ToolError(Exception):
    """Raised anywhere below a tool to produce the error envelope."""

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


def tool_errors(fn: Callable[..., Any]) -> Callable[..., Any]:
    """Decorator for MCP tools: map exceptions to the envelope.

    ToolError → its envelope; ValueError → invalid_argument; anything else →
    internal (logged with traceback, message kept generic).
    """

    @functools.wraps(fn)
    def wrapper(*args: Any, **kwargs: Any) -> Any:
        from observability import metrics  # local import: observability imports nothing from here

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
            metrics.record_tool(fn.__name__, time.monotonic() - started, code)

    return wrapper
