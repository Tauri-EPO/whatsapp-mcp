"""A document answer starts with its identity, and WHATSAPP_MEDIA_INLINE pins its shape (#791).

Some clients materialise a BlobResourceContents as text and hand over a file
that only a sha256 check reveals as corrupted. Every mode is exercised over the
streamable-HTTP transport and the file is reassembled and hashed, because the
defect lives exactly where the in-process call cannot see it.
"""

import base64
import hashlib
import json

import pytest
from starlette.testclient import TestClient

import chat_policy
import main
import media_read
import tool_policy
import whatsapp
from strict_args import StrictArgumentServer
from tests.conftest import ALICE
from tests.test_read_media import _cache, _insert
from tests.test_read_media_text import make_pdf

PDF = make_pdf(["Ola mundo"], pad=1_500_000)  # two 1 MiB chunks, under the 2 MiB blob cap
ZIP = b"PK\x03\x04" + bytes(range(256)) * 4096  # nothing to extract from it
PDF_SHA = hashlib.sha256(PDF).hexdigest()
ZIP_SHA = hashlib.sha256(ZIP).hexdigest()


@pytest.fixture
def documents(paired_dbs, monkeypatch):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.load_chat_policy({}))
    monkeypatch.setattr(tool_policy, "_active", tool_policy.ToolPolicy())
    monkeypatch.setattr(tool_policy, "_runtime", False)
    monkeypatch.delenv(media_read.INLINE_ENV, raising=False)
    monkeypatch.delenv("WHATSAPP_MCP_OAUTH_ISSUER", raising=False)

    def no_bridge(*_args, **_kwargs):
        raise AssertionError("inline tests must never contact a paired account")

    monkeypatch.setattr(whatsapp, "_bridge_request", no_bridge)
    with paired_dbs.messages() as conn:
        _insert(conn, "PDF9", ALICE, "document", bytes.fromhex(PDF_SHA), "laudo.pdf", len(PDF))
        _insert(conn, "ZIP9", ALICE, "document", bytes.fromhex(ZIP_SHA), "bundle.zip", len(ZIP))
    _cache(ALICE, "document_20260904_100000_PDF9.pdf", PDF)
    _cache(ALICE, "document_20260904_100000_ZIP9.zip", ZIP)
    return paired_dbs


@pytest.fixture
def call(documents):
    """tools/call over streamable HTTP, answering the JSON-RPC result."""
    server = StrictArgumentServer("fake-inline-client")
    server.runtime_tool_policy = True
    server.tool()(main.read_media)
    app = main.build_http_app(server, "http", "fake-inline-token", stateless_http=True, json_response=True)
    headers = {"Authorization": "Bearer fake-inline-token", "Accept": "application/json, text/event-stream"}
    with TestClient(app, base_url="http://localhost:8000") as client:

        def _call(arguments):
            response = client.post(
                "/mcp",
                headers=headers,
                json={
                    "jsonrpc": "2.0",
                    "id": 1,
                    "method": "tools/call",
                    "params": {"name": "read_media", "arguments": {"chat_jid": ALICE, **arguments}},
                },
            )
            assert response.status_code == 200
            result = response.json()["result"]
            assert not result.get("isError"), result
            return result["content"]

        yield _call


def _reassemble(call, message_id, data=b"", offset=0):
    """Keep reading as_base64 chunks from ``offset`` and return the whole file."""
    while offset is not None:
        blocks = call({"message_id": message_id, "as_base64": True, "offset": offset})
        part = base64.b64decode(blocks[0]["text"], validate=True)
        meta = json.loads(blocks[-1]["text"])
        assert meta["chunk_sha256"] == hashlib.sha256(part).hexdigest()
        data += part
        offset = meta["next_offset"]
    return data


def _identity(block, sha256):
    assert block["type"] == "text"
    text = block["text"]
    assert sha256 in text and "as_base64=true" in text
    return text


class TestParse:
    @pytest.mark.parametrize(
        ("value", "mode"), [(None, "resource"), ("", "resource"), (" Base64 ", "base64"), ("text", "text")]
    )
    def test_the_three_shapes_and_the_default(self, value, mode):
        assert media_read.parse_inline_env(value) == mode

    def test_an_unknown_shape_stops_startup(self):
        with pytest.raises(ValueError, match=media_read.INLINE_ENV):
            media_read.parse_inline_env("blob")

    def test_in_process_an_unknown_shape_falls_back_to_no_blob(self, monkeypatch):
        monkeypatch.setenv(media_read.INLINE_ENV, "blob")
        assert media_read.inline_mode() == "base64"


class TestOverTheWire:
    def test_resource_puts_the_identity_before_the_blob(self, call):
        blocks = call({"message_id": "PDF9"})
        assert [block["type"] for block in blocks] == ["text", "resource", "text"]
        assert "as_text=true" in _identity(blocks[0], PDF_SHA)
        blob = base64.b64decode(blocks[1]["resource"]["blob"], validate=True)
        assert blocks[1]["resource"]["mimeType"] == "application/pdf"
        assert hashlib.sha256(blob).hexdigest() == PDF_SHA
        assert json.loads(blocks[2]["text"])["sha256"] == PDF_SHA

    def test_a_file_with_nothing_to_extract_does_not_advertise_as_text(self, call):
        blocks = call({"message_id": "ZIP9"})
        assert [block["type"] for block in blocks] == ["text", "resource", "text"]
        assert "as_text" not in _identity(blocks[0], ZIP_SHA)

    def test_base64_sends_no_blob_and_the_chunks_rebuild_the_file(self, call, monkeypatch):
        monkeypatch.setenv(media_read.INLINE_ENV, "base64")
        blocks = call({"message_id": "PDF9"})
        assert [block["type"] for block in blocks] == ["text", "text", "text"]
        _identity(blocks[0], PDF_SHA)
        first = base64.b64decode(blocks[1]["text"], validate=True)
        meta = json.loads(blocks[2]["text"])
        assert meta["offset"] == 0 and meta["next_offset"] == len(first) == media_read.DEFAULT_CHUNK_BYTES
        rebuilt = _reassemble(call, "PDF9", first, meta["next_offset"])
        assert hashlib.sha256(rebuilt).hexdigest() == PDF_SHA

    def test_text_sends_the_extracted_text_and_no_blob(self, call, monkeypatch):
        monkeypatch.setenv(media_read.INLINE_ENV, "text")
        blocks = call({"message_id": "PDF9"})
        assert "resource" not in {block["type"] for block in blocks}
        _identity(blocks[0], PDF_SHA)
        assert any("Ola mundo" in block["text"] for block in blocks[1:-1])
        assert json.loads(blocks[-1]["text"])["sha256"] == PDF_SHA
        assert hashlib.sha256(_reassemble(call, "PDF9")).hexdigest() == PDF_SHA

    def test_text_falls_back_to_base64_for_a_file_with_no_text(self, call, monkeypatch):
        monkeypatch.setenv(media_read.INLINE_ENV, "text")
        blocks = call({"message_id": "ZIP9"})
        assert [block["type"] for block in blocks] == ["text", "text", "text"]
        _identity(blocks[0], ZIP_SHA)
        first = base64.b64decode(blocks[1]["text"], validate=True)
        rebuilt = _reassemble(call, "ZIP9", first, json.loads(blocks[2]["text"])["next_offset"])
        assert rebuilt == ZIP

    @pytest.mark.parametrize("mode", ["base64", "text"])
    def test_explicit_as_text_and_images_are_untouched_by_the_shape(self, call, monkeypatch, mode):
        """The setting only replaces the resource; what an agent asked for still wins."""
        monkeypatch.setenv(media_read.INLINE_ENV, mode)
        blocks = call({"message_id": "PDF9", "as_text": True})
        assert any("Ola mundo" in block["text"] for block in blocks if block["type"] == "text")
        assert "resource" not in {block["type"] for block in blocks}
