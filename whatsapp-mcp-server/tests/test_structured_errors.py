"""Drive schema-bearing tools through the SDK client, including failure conversion."""

import inspect
import json
import logging
from contextlib import asynccontextmanager

import anyio
import pytest
from jsonschema.validators import validator_for
from mcp.client.session import ClientSession
from mcp.server.mcpserver.exceptions import ResourceError, UnexpectedResourceError
from mcp.server.mcpserver.exceptions import ToolError as SDKToolError
from mcp.shared.memory import create_client_server_memory_streams
from mcp_types import CallToolResult, TextContent

import main
import observability
import strict_args
from errors import ToolError, tool_errors
from strict_args import StrictArgumentServer
from tests.conftest import ALICE

SCHEMA_TOOLS = [tool for tool in main.mcp._tool_manager.list_tools() if tool.output_schema is not None]


@asynccontextmanager
async def _sdk_client(mcp_server=main.mcp):
    server = mcp_server._lowlevel_server
    async with create_client_server_memory_streams() as (client_streams, server_streams):
        async with anyio.create_task_group() as tasks:
            tasks.start_soon(server.run, *server_streams, server.create_initialization_options())
            async with ClientSession(*client_streams) as client:
                await client.initialize()
                await client.list_tools()
                yield client
            tasks.cancel_scope.cancel()


@pytest.fixture
def sdk_client():
    return _sdk_client


def _sample(schema, root):
    if "$ref" in schema:
        schema = root["$defs"][schema["$ref"].rsplit("/", 1)[-1]]
    if "enum" in schema:
        return schema["enum"][0]
    if "anyOf" in schema:
        return _sample(next(part for part in schema["anyOf"] if part.get("type") != "null"), root)
    kind = schema.get("type")
    if kind == "array":
        return [_sample(schema.get("items", {}), root)]
    if kind == "object":
        return {}
    if kind == "integer":
        return 1
    if kind == "number":
        return 1.0
    if kind == "boolean":
        return False
    return "probe"


def _arguments(tool):
    schema = tool.parameters
    return {key: _sample(schema["properties"][key], schema) for key in schema.get("required", [])}


def _assert_error(result, code, schema=None):
    assert result.is_error is True
    envelope = json.loads(result.content[0].text)
    assert envelope["error"]["code"] == code
    if result.structured_content is not None:
        assert envelope == result.structured_content
        if schema is not None:
            validator_for(schema)(schema).validate(result.structured_content)
    else:
        assert schema is not None
        assert not validator_for(schema)(schema).is_valid(envelope)


def _denied(*args, **kwargs):
    raise ToolError("denied", "synthetic archive refusal")


def _invalid(*args, **kwargs):
    raise ValueError("synthetic invalid argument")


def _crash(*args, **kwargs):
    raise RuntimeError("synthetic body failure")


def _plain_text(*args, **kwargs):
    return "synthetic unexpected text result"


@pytest.mark.parametrize("tool", SCHEMA_TOOLS, ids=lambda tool: tool.name)
@pytest.mark.parametrize(
    "body,code",
    [(_denied, "denied"), (_invalid, "invalid_argument"), (_crash, "internal"), (_plain_text, "internal")],
    ids=["domain", "value", "unexpected", "text-instead-of-schema"],
)
async def test_every_schema_tool_preserves_error_channels(sdk_client, monkeypatch, tool, body, code):
    # Inject beneath the real decorators and registration adapter, preserving
    # the SDK's validated arguments and original schema. No tool effects run.
    original_body = inspect.unwrap(tool.fn)
    assert original_body.__closure__ is None
    monkeypatch.setattr(original_body, "__code__", body.__code__)
    async with sdk_client() as client:
        result = await client.call_tool(tool.name, _arguments(tool))
    _assert_error(result, code, client._tool_output_schemas[tool.name])


@pytest.mark.parametrize("tool", SCHEMA_TOOLS, ids=lambda tool: tool.name)
async def test_every_schema_tool_rejects_bad_input_structurally(sdk_client, tool):
    arguments = _arguments(tool)
    name = next(iter(tool.parameters["properties"]), "undeclared_probe")
    arguments[name] = {"not": "the declared argument type"}
    async with sdk_client() as client:
        result = await client.call_tool(tool.name, arguments)
    _assert_error(result, "invalid_argument", client._tool_output_schemas[tool.name])


@pytest.mark.parametrize("tool", SCHEMA_TOOLS, ids=lambda tool: tool.name)
async def test_every_schema_tool_rejects_unknown_arguments_structurally(sdk_client, tool):
    async with sdk_client() as client:
        result = await client.call_tool(tool.name, {**_arguments(tool), "undeclared_probe": True})
    _assert_error(result, "invalid_argument", client._tool_output_schemas[tool.name])


def test_schema_tool_inventory_is_not_disabled_to_hide_failures():
    assert len(SCHEMA_TOOLS) >= 47
    assert {"list_messages", "list_unanswered", "search_contacts", "search_notes", "search_media_notes"} <= {
        tool.name for tool in SCHEMA_TOOLS
    }


@pytest.fixture
def fts_archive(paired_dbs):
    conn = paired_dbs.messages()
    try:
        with conn:
            conn.execute(
                "INSERT INTO messages (id, chat_jid, sender, sender_server, content, timestamp, is_from_me) "
                "VALUES (?, ?, ?, ?, ?, ?, ?)",
                (
                    "sdk-message",
                    ALICE,
                    ALICE.split("@")[0],
                    "s.whatsapp.net",
                    "Alice reunião",
                    "2026-10-08 10:00:00",
                    0,
                ),
            )
            conn.execute(
                "CREATE VIRTUAL TABLE messages_fts USING fts5(content, content='messages', "
                "content_rowid='rowid', tokenize='unicode61 remove_diacritics 2')"
            )
            conn.execute("INSERT INTO messages_fts(messages_fts) VALUES('rebuild')")
    finally:
        conn.close()
    return paired_dbs


@pytest.mark.parametrize("fields", [None, ["jid", "name", "last_message", "last_inbound_time", "age_hours", "unread"]])
async def test_owner_unanswered_shapes_retain_page_schema(sdk_client, fts_archive, fields):
    arguments = {"since": "2026-09-17T00:00:00", "limit": 60, "exclude_groups": True, "omit_nulls": True}
    if fields is not None:
        arguments["fields"] = fields
    async with sdk_client() as client:
        result = await client.call_tool("list_unanswered", arguments)
    assert not result.is_error
    page = result.structured_content
    assert page is not None and set(page) == {"items", "next_cursor", "has_more"}
    assert page["items"][0]["jid"] == ALICE
    if fields is not None:
        assert set(page["items"][0]) <= set(fields)


@pytest.mark.parametrize("query,expected", [("Alice OR Bob OR reuniao OR reunião", True), ("Alice OR (", False)])
async def test_real_fts_and_projection_remain_structured(sdk_client, fts_archive, query, expected):
    async with sdk_client() as client:
        result = await client.call_tool(
            "list_messages",
            {
                "query": query,
                "after": "2026-09-17T00:00:00",
                "limit": 30,
                "include_context": False,
                "fields": ["id", "content"],
                "omit_nulls": True,
                "max_content_chars": 250,
            },
        )
    assert not result.is_error
    page = result.structured_content
    assert page is not None and set(page) == {"items", "next_cursor", "has_more"}
    assert bool(page["items"]) is expected
    if expected:
        assert page["items"] == [{"id": "sdk-message", "content": "Alice reunião"}]


async def test_async_tool_registration_preserves_success_and_errors():
    server = StrictArgumentServer("async-errors")

    @server.tool()
    async def sample(fail: bool = False) -> list[dict]:
        if fail:
            return {"error": {"code": "denied", "message": "synthetic refusal"}}
        return [{"value": "ok"}]

    success = await server.call_tool("sample", {})
    assert success.structured_content == {"result": [{"value": "ok"}]}
    failure = await server.call_tool("sample", {"fail": True})
    _assert_error(failure, "denied", server._tool_manager.get_tool("sample").output_schema)


@pytest.mark.parametrize("name", ["list_messages", "list_unanswered"])
async def test_invalid_projection_is_a_real_tool_error(sdk_client, fts_archive, name):
    async with sdk_client() as client:
        result = await client.call_tool(name, {"fields": ["invented_field"]})
    _assert_error(result, "invalid_argument")


@pytest.mark.parametrize("name", ["list_messages", "list_unanswered"])
async def test_unreadable_sqlite_archive_is_a_structured_error(sdk_client, paired_dbs, name):
    conn = paired_dbs.messages()
    try:
        with conn:
            conn.execute("DROP TABLE messages")
    finally:
        conn.close()
    async with sdk_client() as client:
        result = await client.call_tool(name, {})
    _assert_error(result, "internal")


async def test_explicit_unstructured_failure_gets_the_common_envelope():
    server = StrictArgumentServer("explicit-error")

    @server.tool()
    def sample() -> dict:
        return CallToolResult(content=[TextContent(type="text", text="synthetic failure")], is_error=True)

    result = await server.call_tool("sample", {})
    _assert_error(result, "internal")
    assert json.loads(result.content[0].text)["error"]["message"] == "synthetic failure"


async def test_schema_without_output_model_is_a_tool_error_through_sdk():
    server = StrictArgumentServer("schema-only")

    @server.tool()
    def sample() -> dict[str, str]:
        return {"value": "ok"}

    tool = server._tool_manager.get_tool("sample")
    assert tool.output_schema is not None
    tool.fn_metadata.output_model = None
    async with _sdk_client(server) as client:
        result = await client.call_tool("sample", {})
    _assert_error(result, "internal", client._tool_output_schemas["sample"])


@pytest.mark.parametrize("failure", [SDKToolError, ResourceError, UnexpectedResourceError])
async def test_sdk_execution_errors_keep_their_message_and_are_not_bad_input(failure):
    server = StrictArgumentServer("execution-error")

    @server.tool()
    def sample() -> dict:
        raise failure("synthetic nested failure")

    async with _sdk_client(server) as client:
        result = await client.call_tool("sample", {})
    _assert_error(result, "internal", client._tool_output_schemas["sample"])
    assert "synthetic nested failure" in json.loads(result.content[0].text)["error"]["message"]


async def test_required_properties_error_uses_only_text_and_preserves_both_blocks():
    server = StrictArgumentServer("required-properties")

    @server.tool()
    def sample() -> dict:
        return CallToolResult(
            content=[TextContent(type="text", text="synthetic first"), TextContent(type="text", text="second")],
            is_error=True,
        )

    server._tool_manager.get_tool("sample").fn_metadata.output_schema = {
        "type": "object",
        "required": ["successful_value"],
        "properties": {"successful_value": {"type": "string"}},
    }
    async with _sdk_client(server) as client:
        result = await client.call_tool("sample", {})
    _assert_error(result, "internal", client._tool_output_schemas["sample"])
    assert result.structured_content is None
    assert json.loads(result.content[0].text)["error"]["message"] == "synthetic first\nsecond"


@pytest.fixture
def isolated_metrics(monkeypatch):
    registry = observability.Metrics()
    monkeypatch.setattr(observability, "metrics", registry)
    monkeypatch.setattr(strict_args, "metrics", registry)
    return registry


async def test_output_conversion_failure_counts_once_as_internal(isolated_metrics):
    server = StrictArgumentServer("conversion-metrics")

    @server.tool()
    @tool_errors
    def sample() -> dict[str, str]:
        return "synthetic bad result"

    result = await server.call_tool("sample", {})
    _assert_error(result, "internal", server._tool_manager.get_tool("sample").output_schema)
    assert isolated_metrics.tool_calls == {"sample": 1}
    assert isolated_metrics.tool_errors == {("sample", "internal"): 1}
    assert sum(isolated_metrics.tool_duration_buckets["sample"]) == 1
    assert not observability.mcp_metrics_owned.get()


async def test_metrics_context_is_isolated_and_direct_python_calls_still_count(isolated_metrics):
    server = StrictArgumentServer("concurrent-metrics")

    @server.tool()
    @tool_errors
    def sample(fail: bool = False) -> dict:
        if fail:
            raise ToolError("denied", "synthetic refusal")
        return {"ok": True}

    async with anyio.create_task_group() as tasks:
        for fail in [False, True] * 8:
            tasks.start_soon(server.call_tool, "sample", {"fail": fail})
    assert isolated_metrics.tool_calls == {"sample": 16}
    assert isolated_metrics.tool_errors == {("sample", "denied"): 8}
    assert sum(isolated_metrics.tool_duration_buckets["sample"]) == 16
    assert not observability.mcp_metrics_owned.get()
    sample()
    assert isolated_metrics.tool_calls == {"sample": 17}


@pytest.mark.parametrize("mode", ["success", "domain", "input", "unknown-argument", "unknown-tool"])
async def test_one_info_diagnostic_records_actual_shape_without_args_or_content(caplog, mode):
    server = StrictArgumentServer("diagnostic")

    @server.tool()
    @tool_errors
    def sample(secret: str = "synthetic-secret", fail: bool = False) -> dict:
        if fail:
            raise ToolError("denied", "synthetic-private-content")
        return {"private": "synthetic-private-content", "accent": "reunião"}

    args = {"secret": "synthetic-secret", "fail": mode == "domain"}
    if mode == "input":
        args["secret"] = {}
    elif mode == "unknown-argument":
        args["unexpected"] = "synthetic-secret"
    result = None
    with caplog.at_level(logging.INFO, logger="whatsapp_mcp"):
        if mode == "unknown-tool":
            with pytest.raises(SDKToolError):
                await server.call_tool("synthetic-secret", args)
        else:
            result = await server.call_tool("sample", args)
    lines = [record.getMessage() for record in caplog.records if record.levelno == logging.INFO]
    assert len(lines) == 1
    line = lines[0]
    assert line.startswith("tool_result tool=" + ("<unknown>" if result is None else "sample"))
    assert "synthetic-secret" not in line and "synthetic-private-content" not in line
    assert "duration_ms=" in line
    if result is not None:
        assert f"is_error={result.is_error}" in line
        assert f"has_structured_content={result.structured_content is not None}" in line
        assert f"content_blocks={len(result.content)}" in line
        assert f"result_bytes={len(result.model_dump_json(by_alias=True, exclude_none=True).encode('utf-8'))}" in line
    else:
        assert "result_kind=raised" in line and "result_bytes=0" in line
