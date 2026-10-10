"""Original bytes survive text-only MCP clients, in bounded, verifiable ranges."""

import base64
import builtins
import hashlib
import json
from io import BytesIO
from pathlib import Path

import pytest
from pypdf import PdfReader, PdfWriter
from starlette.testclient import TestClient

import chat_policy
import main
import media_read
import tool_policy
import whatsapp
from errors import ToolError
from strict_args import StrictArgumentServer
from tests.conftest import ALICE, BOB
from tests.test_read_media import _cache, _insert


@pytest.fixture
def attachment(paired_dbs, monkeypatch):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.load_chat_policy({}))
    monkeypatch.setattr(tool_policy, "_active", tool_policy.ToolPolicy(read_only=True))
    monkeypatch.setattr(tool_policy, "_runtime", False)
    monkeypatch.setenv("WHATSAPP_WRAP_UNTRUSTED", "1")
    monkeypatch.delenv("WHATSAPP_MCP_OAUTH_ISSUER", raising=False)

    def no_bridge(*_args, **_kwargs):
        raise AssertionError("chunk tests must never contact a paired account")

    monkeypatch.setattr(whatsapp, "_bridge_request", no_bridge)
    writer = PdfWriter()
    writer.add_blank_page(width=612, height=792)
    writer.add_metadata({"/Padding": "0123456789abcdef" * 144000})
    buffer = BytesIO()
    writer.write(buffer)
    data = buffer.getvalue()  # A valid PDF > 2 MiB
    digest = hashlib.sha256(data).digest()
    with paired_dbs.messages() as conn:
        _insert(conn, "CHUNK1", ALICE, "document", digest, "report.pdf", len(data))
        conn.execute("CREATE TABLE runtime_settings(key TEXT PRIMARY KEY,value TEXT,updated_at TEXT,version INTEGER)")
    path = Path(_cache(ALICE, "document_20260904_100000_CHUNK1.pdf", data))
    return paired_dbs, data, path


def chunk(offset=0, **kwargs):
    blocks = main.read_media(ALICE, "CHUNK1", as_base64=True, offset=offset, **kwargs)
    assert [block.type for block in blocks] == ["text", "text"]
    data = base64.b64decode(blocks[0].text, validate=True)
    meta = json.loads(blocks[1].text)
    assert meta["offset"] == offset
    assert meta["bytes"] == meta["returned_length"] == len(data)
    assert meta["chunk_sha256"] == hashlib.sha256(data).hexdigest()
    return data, meta


def test_default_last_and_eof_chunks(attachment):
    _, original, _ = attachment
    data, meta = chunk()
    assert data == original[:1048576]
    assert meta["next_offset"] == 1048576
    assert meta["total_size"] == len(original)
    assert meta["sha256"] == hashlib.sha256(original).hexdigest()
    data, meta = chunk(len(original) - 7)
    assert data == original[-7:] and meta["next_offset"] is None
    data, meta = chunk(len(original))
    assert data == b"" and meta["next_offset"] is None


@pytest.mark.parametrize(
    "kwargs",
    [
        {"offset": -1},
        {"offset": True},
        {"offset": 1.5},
        {"length": 0},
        {"length": -1},
        {"length": True},
        {"length": 1.5},
        {"length": media_read.MAX_CHUNK_BYTES + 1},
        {"as_text": True},
        {"as_images": True},
    ],
)
def test_invalid_arguments_before_io(monkeypatch, kwargs):
    def no_io(*_args, **_kwargs):
        raise AssertionError("invalid chunk arguments must be refused before I/O")

    monkeypatch.setattr(media_read, "resolve_media", no_io)
    with pytest.raises(ToolError) as exc:
        media_read.read_media(ALICE, "CHUNK1", as_base64=True, **kwargs)
    assert exc.value.code == "invalid_argument"


def test_offset_beyond_size(attachment):
    out = main.read_media(ALICE, "CHUNK1", as_base64=True, offset=len(attachment[1]) + 1)
    assert out.is_error
    assert out.structured_content["error"]["code"] == "invalid_argument"


def test_minimum_and_maximum_length_and_empty_file(attachment):
    store, original, path = attachment
    assert chunk(length=1)[0] == original[:1]
    assert chunk(length=media_read.MAX_CHUNK_BYTES)[0] == original
    path.write_bytes(b"")
    with store.messages() as conn:
        conn.execute("UPDATE messages SET file_sha256=? WHERE id='CHUNK1'", (hashlib.sha256(b"").digest(),))
    assert chunk()[0] == b""


def test_only_requested_range_is_read_when_archive_has_hash(attachment, monkeypatch):
    _, original, path = attachment
    real_open = builtins.open
    reads, seeks = [], []

    class ObservedFile:
        def __enter__(self):
            self.handle = real_open(path, "rb")
            return self

        def __exit__(self, *_args):
            self.handle.close()

        def fileno(self):
            return self.handle.fileno()

        def seek(self, offset):
            seeks.append(offset)
            return self.handle.seek(offset)

        def read(self, size):
            reads.append(size)
            return self.handle.read(size)

    monkeypatch.setattr(media_read, "open", lambda *_args: ObservedFile(), raising=False)
    assert chunk(1048576, length=123)[0] == original[1048576:1048699]
    assert seeks == [1048576] and reads == [123]


def test_legacy_missing_hash_is_streamed(attachment):
    store, original, _ = attachment
    with store.messages() as conn:
        conn.execute("UPDATE messages SET file_sha256=NULL WHERE id='CHUNK1'")
    assert chunk(length=13)[1]["sha256"] == hashlib.sha256(original).hexdigest()


def test_implicit_download_denied_and_local_bytes_still_readable(attachment, monkeypatch):
    monkeypatch.setattr(tool_policy, "_active", tool_policy.ToolPolicy(deny=frozenset({"download_media"})))
    assert chunk(length=3)[0] == attachment[1][:3]
    attachment[2].unlink()
    out = main.read_media(ALICE, "CHUNK1", as_base64=True)
    assert out.is_error and out.structured_content["error"]["code"] == "denied"


@pytest.mark.parametrize("policy", ["allowed", "chat_denied", "tool_denied"])
def test_http_text_only_client_reassembles_and_enforces_policy(attachment, monkeypatch, tmp_path, policy):
    _, original, path = attachment
    if policy == "chat_denied":
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.load_chat_policy({"WHATSAPP_ALLOWED_CHATS": BOB}))
    if policy == "tool_denied":
        monkeypatch.setattr(tool_policy, "_active", tool_policy.ToolPolicy(deny=frozenset({"read_media"})))
    server = StrictArgumentServer("fake-chunk-client")
    server.runtime_tool_policy = True
    server.tool()(main.read_media)
    server.tool()(main.list_media)
    app = main.build_http_app(server, "http", "fake-chunk-token", stateless_http=True, json_response=True)
    before = set(path.parent.iterdir())
    headers = {"Authorization": "Bearer fake-chunk-token", "Accept": "application/json, text/event-stream"}
    with TestClient(app, base_url="http://localhost:8000") as client:

        def call(name, arguments):
            response = client.post(
                "/mcp",
                headers=headers,
                json={
                    "jsonrpc": "2.0",
                    "id": 1,
                    "method": "tools/call",
                    "params": {"name": name, "arguments": arguments},
                },
            )
            assert response.status_code == 200
            return response.json()["result"]

        if policy != "allowed":
            result = call("read_media", {"chat_jid": ALICE, "message_id": "CHUNK1", "as_base64": True})
            assert result["isError"] and result["structuredContent"]["error"]["code"] == "denied"
            return
        inventory = call("list_media", {"chat_jid": ALICE})["structuredContent"]
        expected = inventory["items"][0]["sha256"]
        offset = 0
        destination = tmp_path / "agent-copy.pdf"
        chunks = 0
        with destination.open("wb") as output:
            while True:
                result = call(
                    "read_media", {"chat_jid": ALICE, "message_id": "CHUNK1", "as_base64": True, "offset": offset}
                )
                assert not result.get("isError")
                blocks = result["content"]
                assert [block["type"] for block in blocks] == ["text", "text"]
                data = base64.b64decode(blocks[0]["text"], validate=True)
                meta = json.loads(blocks[1]["text"])
                assert meta["offset"] == offset and meta["returned_length"] == len(data)
                assert meta["chunk_sha256"] == hashlib.sha256(data).hexdigest()
                assert meta["sha256"] == expected and meta["total_size"] == len(original)
                output.write(data)
                chunks += 1
                if meta["next_offset"] is None:
                    break
                offset = meta["next_offset"]
        assert chunks == 3
        assert destination.read_bytes() == original
        assert hashlib.sha256(destination.read_bytes()).hexdigest() == expected
        assert len(PdfReader(destination).pages) == 1
    assert set(path.parent.iterdir()) == before
    assert tool_policy.active_policy().read_only
