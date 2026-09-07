"""Refuse tool arguments the tool never declared.

The SDK builds one pydantic model per tool (``FuncMetadata.arg_model``) whose
config is pydantic's default ``extra="ignore"``, so an invented or misspelled
key is dropped before the function runs. ``list_chats(fields=["jid","name"])``
therefore returned full rows and reported no error at all: an agent that
believes it asked for a projection gets everything and only notices by measuring
the payload. ``MCPServer`` exposes no strict switch, so the check lives here —
one ``call_tool`` override that compares the incoming keys with the ones the
tool declares and answers the ``invalid_argument`` envelope naming both the
unknown keys and the valid names.

The refusal is returned rather than raised so it reaches the model as the same
``{"error": {"code": ..., "message": ...}}`` shape every other failure uses
(``errors.py``); ``is_error`` is set because the tool never ran.

The SDK internals this leans on — ``_tool_manager.get_tool``,
``Tool.fn_metadata.arg_model.model_fields`` and the alias rule below — are
pinned by ``tests/test_strict_args.py``, so an SDK bump that moves them fails
there instead of silently restoring the permissive behaviour.
"""

from __future__ import annotations

import json
from typing import Any

from mcp.server.mcpserver import Context, MCPServer
from mcp.server.mcpserver.tools.base import Tool
from mcp_types import CallToolResult, InputRequiredResult, TextContent

from errors import ToolError
from observability import metrics


def declared_arguments(tool: Tool) -> set[str]:
    """The argument names ``tool`` accepts on the wire.

    A parameter whose name shadows a ``BaseModel`` attribute (``json``,
    ``copy``, ``schema``…) is stored as ``field_<name>`` with the original kept
    as the field alias, so the wire name is the alias whenever there is one.
    """
    return {field.alias or name for name, field in tool.fn_metadata.arg_model.model_fields.items()}


def _refusal(name: str, unknown: list[str], declared: set[str]) -> CallToolResult:
    """The ``invalid_argument`` envelope for a call carrying undeclared keys."""
    valid = ", ".join(sorted(declared)) or "(no arguments)"
    envelope = ToolError(
        "invalid_argument",
        f"{name} has no argument(s) {', '.join(unknown)}; it accepts: {valid}",
    ).to_dict()
    # The tool never ran, so @tool_errors never saw the call: count it here or a
    # client looping on a typo produces no signal at all on /metrics.
    metrics.record_tool(name, 0.0, "invalid_argument")
    return CallToolResult(
        content=[TextContent(type="text", text=json.dumps(envelope, ensure_ascii=False))],
        # Also structured, like every other envelope this server returns: a
        # client that reads structured_content and ignores the text block must
        # not see an empty error for exactly the typo this check is here to name.
        structured_content=envelope,
        is_error=True,
    )


class StrictArgumentServer(MCPServer[Any]):
    """``MCPServer`` that refuses a call carrying arguments the tool does not declare."""

    async def call_tool(
        self, name: str, arguments: dict[str, Any], context: Context[Any, Any] | None = None
    ) -> CallToolResult | InputRequiredResult:
        tool = self._tool_manager.get_tool(name)
        if tool is not None:
            declared = declared_arguments(tool)
            unknown = sorted(set(arguments) - declared)
            if unknown:
                return _refusal(name, unknown, declared)
        # An unknown tool name stays the SDK's error, not ours.
        return await super().call_tool(name, arguments, context)
