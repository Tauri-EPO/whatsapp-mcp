"""MCP argument refusals, schema-compatible errors and result-shape diagnostics.

The SDK drops unknown argument keys and converts tool exceptions into text.
This server keeps the shared errors.py envelope, while omitting structured
error content when the advertised success schema would reject it. SDK input
validation, deliberate execution errors and output-conversion crashes retain
their distinct meanings. Successful results and unknown-name errors stay on
the SDK path.

SDK internals used here are pinned by the strict-argument and SDK-client tests.
"""

from __future__ import annotations

import json
import logging
import time
from collections.abc import Callable
from typing import Any

from jsonschema.validators import validator_for
from mcp.server.mcpserver import Context, MCPServer
from mcp.server.mcpserver.exceptions import ToolError as SDKToolError
from mcp.server.mcpserver.exceptions import UnexpectedToolError
from mcp.server.mcpserver.tools.base import Tool
from mcp_types import CallToolResult, InputRequiredResult, TextContent
from pydantic import ValidationError
from referencing import Registry
from referencing.exceptions import Unresolvable

from errors import ERROR_CODES, ToolError, error, structured_errors, tool_error_result
from observability import mcp_metrics_owned, metrics
from tool_policy import active_policy

logger = logging.getLogger("whatsapp_mcp")


def declared_arguments(tool: Tool) -> set[str]:
    """Wire argument names, including aliases for fields shadowing BaseModel."""
    return {field.alias or name for name, field in tool.fn_metadata.arg_model.model_fields.items()}


def _refusal(name: str, unknown: list[str], declared: set[str]) -> CallToolResult:
    valid = ", ".join(sorted(declared)) or "(no arguments)"
    return tool_error_result(
        ToolError(
            "invalid_argument",
            f"{name} has no argument(s) {', '.join(unknown)}; it accepts: {valid}",
        ).to_dict()
    )


def _error_code(result: CallToolResult | InputRequiredResult | None) -> str | None:
    if not isinstance(result, CallToolResult) or not result.is_error:
        return None
    payload = result.structured_content
    if payload is None:
        for block in result.content:
            if isinstance(block, TextContent):
                try:
                    payload = json.loads(block.text)
                except ValueError:
                    continue
                break
    envelope = payload.get("error") if isinstance(payload, dict) else None
    code = envelope.get("code") if isinstance(envelope, dict) else None
    return code if code in ERROR_CODES else "internal"


def _schema_compatible_error(tool: Tool, result: CallToolResult) -> CallToolResult:
    schema = tool.output_schema
    if not result.is_error or result.structured_content is None or schema is None:
        return result
    try:
        compatible = validator_for(schema)(schema, registry=Registry()).is_valid(result.structured_content)
    except Unresolvable:
        compatible = False
    if compatible:
        return result
    # Some clients validate structuredContent even on isError results. A text
    # error is accepted; an envelope cannot satisfy a required successful list.
    return result.model_copy(update={"structured_content": None})


class StrictArgumentServer(MCPServer[Any]):
    """Keep tool failures readable across clients with different schema checks."""

    runtime_tool_policy = False

    async def list_tools(self):
        tools = await super().list_tools()
        if self.runtime_tool_policy:
            policy = active_policy()
            tools = [tool for tool in tools if policy.allows(tool.name)]
        return tools

    def add_tool(self, fn: Callable[..., Any], *args: Any, **kwargs: Any) -> None:
        super().add_tool(structured_errors(fn), *args, **kwargs)

    async def call_tool(
        self, name: str, arguments: dict[str, Any], context: Context[Any, Any] | None = None
    ) -> CallToolResult | InputRequiredResult:
        started = time.monotonic()
        tool = self._tool_manager.get_tool(name)
        result: CallToolResult | InputRequiredResult | None = None
        token = mcp_metrics_owned.set(True)
        try:
            result = await self._call_tool_result(name, arguments, context, tool)
            if isinstance(result, CallToolResult) and tool is not None:
                result = _schema_compatible_error(tool, result)
            return result
        finally:
            mcp_metrics_owned.reset(token)
            elapsed = time.monotonic() - started
            if tool is not None:
                metrics.record_tool(name, elapsed, _error_code(result))
            if logger.isEnabledFor(logging.INFO):
                is_tool_result = isinstance(result, CallToolResult)
                size = len(result.model_dump_json(by_alias=True, exclude_none=True).encode("utf-8")) if result else 0
                logger.info(
                    "tool_result tool=%s duration_ms=%.3f result_kind=%s is_error=%s "
                    "has_structured_content=%s content_blocks=%s result_bytes=%d",
                    name if tool is not None else "<unknown>",
                    elapsed * 1000,
                    type(result).__name__ if result is not None else "raised",
                    result.is_error if is_tool_result else None,
                    result.structured_content is not None if is_tool_result else False,
                    len(result.content) if is_tool_result else 0,
                    size,
                )

    async def _call_tool_result(
        self, name: str, arguments: dict[str, Any], context: Context[Any, Any] | None, tool: Tool | None
    ) -> CallToolResult | InputRequiredResult:
        if tool is None:
            return await super().call_tool(name, arguments, context)
        if self.runtime_tool_policy:
            try:
                policy = active_policy()
                if not policy.allows(name):
                    return tool_error_result(ToolError("denied", policy.denial_message(name)).to_dict())
            except ToolError as exc:
                return tool_error_result(exc.to_dict())
        declared = declared_arguments(tool)
        unknown = sorted(set(arguments) - declared)
        if unknown:
            return _refusal(name, unknown, declared)
        try:
            result = await super().call_tool(name, arguments, context)
        except UnexpectedToolError as exc:
            logger.exception("%s failed during SDK execution or output conversion", name)
            return tool_error_result(error("internal", str(exc)))
        except SDKToolError as exc:
            # The SDK's input-model failure is distinct from a deliberate tool
            # or nested resource error. Preserve the latter's original message.
            code = "invalid_argument" if isinstance(exc.__cause__, ValidationError) else "internal"
            return tool_error_result(error(code, str(exc)))
        if isinstance(result, CallToolResult) and result.structured_content is None:
            if result.is_error:
                message = "\n".join(block.text for block in result.content if isinstance(block, TextContent))
                return tool_error_result(error("internal", message or f"Error executing tool {name}"))
            if tool.output_schema is not None:
                logger.error("%s returned an unstructured MCP tool result", name)
                return tool_error_result(error("internal", f"Error executing tool {name}"))
        return result
