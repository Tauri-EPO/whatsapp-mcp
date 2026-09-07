"""Unknown tool arguments are refused, not dropped (issue #282).

Two halves: the SDK internals ``strict_args`` reads are pinned here so a bump
that moves them fails loudly, and the refusal itself is exercised through
``mcp.call_tool``, the entry point a client reaches.
"""

import json

import pytest
from mcp.server.mcpserver.exceptions import ToolError as SdkToolError
from mcp.server.mcpserver.tools.base import Tool
from mcp.server.mcpserver.utilities.func_metadata import ArgModelBase, func_metadata

import main
from observability import metrics
from strict_args import declared_arguments


def _envelope(result):
    """The error envelope a refusal carries, in both places a client may read it."""
    assert result.is_error is True
    text = json.loads(result.content[0].text)
    assert result.structured_content == text
    return text


# --- the SDK internals strict_args depends on ---------------------------------


def test_sdk_still_drops_unknown_arguments_without_the_override():
    """The reason this module exists: pydantic's default config ignores extras.

    If a future SDK makes the argument model strict, this fails and the override
    can go.
    """

    def sample(chat_jid: str, limit: int = 5) -> dict:
        return {}

    model = func_metadata(sample).arg_model
    assert issubclass(model, ArgModelBase)
    assert model.model_config.get("extra") in (None, "ignore")
    assert model.model_validate({"chat_jid": "x", "nope": 1}).model_dump_one_level() == {"chat_jid": "x", "limit": 5}


def test_declared_arguments_reads_the_tools_arg_model():
    tool = main.mcp._tool_manager.get_tool("list_chats")
    assert tool is not None
    assert declared_arguments(tool) == {
        "query",
        "limit",
        "page",
        "include_last_message",
        "sort_by",
        "cursor",
        "fields",
        "omit_nulls",
        "max_content_chars",
        "count_only",
    }


def test_declared_arguments_uses_the_alias_for_a_shadowing_name():
    """A parameter named like a BaseModel attribute is stored as field_<name>."""

    def sample(json: str = "") -> dict:
        return {}

    assert func_metadata(sample).arg_model.model_fields["field_json"].alias == "json"
    assert declared_arguments(Tool.from_function(sample)) == {"json"}


# --- the refusal --------------------------------------------------------------


async def test_unknown_argument_is_refused_with_the_valid_names(paired_dbs):
    result = await main.mcp.call_tool("list_chats", {"limit": 2, "feilds": ["jid"]})
    error = _envelope(result)["error"]
    assert error["code"] == "invalid_argument"
    assert "feilds" in error["message"]
    assert "fields" in error["message"] and "omit_nulls" in error["message"]


async def test_several_unknown_arguments_are_all_named(paired_dbs):
    result = await main.mcp.call_tool("list_chats", {"zzz": 1, "aaa": 2})
    assert "aaa, zzz" in _envelope(result)["error"]["message"]


async def test_a_refusal_is_counted_like_any_other_tool_error(paired_dbs):
    """The tool never runs, so @tool_errors cannot count it: /metrics would go blind."""
    calls = metrics.tool_calls["list_chats"]
    errors = metrics.tool_errors[("list_chats", "invalid_argument")]
    await main.mcp.call_tool("list_chats", {"feilds": ["jid"]})
    assert metrics.tool_calls["list_chats"] == calls + 1
    assert metrics.tool_errors[("list_chats", "invalid_argument")] == errors + 1


async def test_declared_arguments_still_run_the_tool(paired_dbs):
    result = await main.mcp.call_tool("list_chats", {"limit": 2, "fields": ["jid"]})
    assert result.is_error is not True
    rows = json.loads(result.content[0].text)["items"]
    assert rows and all(set(row) == {"jid"} for row in rows)


async def test_unknown_tool_is_still_the_sdks_error():
    with pytest.raises(SdkToolError):
        await main.mcp.call_tool("no_such_tool", {"whatever": 1})
