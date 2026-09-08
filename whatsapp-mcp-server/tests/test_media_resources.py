"""whatsapp://media/<chat>/<id>: the same bytes as read_media, behind the same gates."""

import os

import pytest
from mcp.server.mcpserver.exceptions import ResourceError, ResourceNotFoundError

import chat_policy
import main
import media_inventory
import media_read
import media_resource
import tool_policy
import whatsapp
from tests.conftest import ALICE, BOB
from tool_policy import ToolPolicy

PDF = b"%PDF-1.4 not really a pdf"


def _insert(conn, msg_id, chat, media_type, filename=None, length=10):
    conn.execute(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, "
        "file_length) VALUES (?, ?, '5511888888888', '', '2026-09-04 10:00:00+00:00', 0, ?, ?, ?)",
        (msg_id, chat, media_type, filename, length),
    )


def _cache(chat, name, data: bytes) -> str:
    directory = media_inventory.chat_media_dir(chat)
    os.makedirs(directory, exist_ok=True)
    path = os.path.join(directory, name)
    with open(path, "wb") as handle:
        handle.write(data)
    return path


@pytest.fixture
def store(paired_dbs, monkeypatch):
    """A cached PDF, a cached text file, and a document with no bytes on disk."""
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.load_chat_policy({}))
    with paired_dbs.messages() as conn:
        _insert(conn, "DOC1", ALICE, "document", "report.pdf", length=len(PDF))
        _insert(conn, "TXT1", ALICE, "document", "notes.txt")
        _insert(conn, "GONE", ALICE, "document", "missing.pdf")
        _insert(conn, "BIG1", ALICE, "video", "movie.mp4", length=200_000_000)
        conn.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) "
            "VALUES ('T1', ?, 'x', 'hi', '2026-09-04 09:00:00+00:00', 0)",
            (ALICE,),
        )
    _cache(ALICE, "document_20260904_100000_DOC1.pdf", PDF)
    _cache(ALICE, "document_20260904_100000_TXT1.txt", "olá, mundo\n".encode())
    yield paired_dbs
    tool_policy.set_active_policy(None)


def _uri(message_id: str, chat_jid: str = ALICE) -> str:
    return media_read.media_uri(chat_jid, message_id)


class TestUri:
    def test_the_uri_round_trips(self):
        jid, message_id = "5511999999999:12@s.whatsapp.net", "3EB0/A"
        assert media_resource.parse_media_uri(media_read.media_uri(jid, message_id)) == (jid, message_id)

    @pytest.mark.parametrize(
        "uri",
        [
            "https://example.com/a/b",
            "whatsapp://media/only-one-segment",
            "whatsapp://media/a/b/c",
            "whatsapp://media//b",
        ],
    )
    def test_anything_else_is_not_ours(self, uri):
        assert media_resource.parse_media_uri(uri) is None


class TestRead:
    async def test_the_bytes_are_the_same_bytes_read_media_returns(self, store):
        contents = list(await main.mcp.read_resource(_uri("DOC1")))
        assert len(contents) == 1
        assert contents[0].content == PDF
        assert contents[0].mime_type == "application/pdf"
        block = main.read_media(chat_jid=ALICE, message_id="DOC1")[0]
        assert block.resource.uri == _uri("DOC1")

    async def test_a_text_file_comes_back_as_text(self, store):
        contents = list(await main.mcp.read_resource(_uri("TXT1")))
        assert contents[0].content == "olá, mundo\n"
        assert contents[0].mime_type == "text/plain"

    async def test_an_uncached_file_is_fetched_through_the_bridge(self, store, monkeypatch):
        calls = []

        def fake_download(message_id, chat_jid):
            calls.append((message_id, chat_jid))
            return _cache(chat_jid, "document_20260904_100000_GONE.pdf", PDF)

        monkeypatch.setattr(whatsapp, "download_media", fake_download)
        contents = list(await main.mcp.read_resource(_uri("GONE")))
        assert calls == [("GONE", ALICE)] and contents[0].content == PDF

    async def test_a_uri_we_do_not_serve_is_still_the_sdk_s_answer(self, store):
        with pytest.raises(ResourceError):
            await main.mcp.read_resource("https://example.com/nope")

    async def test_the_template_is_advertised(self, store):
        templates = await main.mcp.list_resource_templates()
        ours = [item for item in templates if item.uri_template.startswith("whatsapp://media/")]
        assert len(ours) == 1
        assert ours[0].uri_template == "whatsapp://media/{chat_jid}/{message_id}"


class TestDenials:
    """Every refusal read_media makes, made again on the resource."""

    async def test_a_chat_outside_the_allow_list(self, store, monkeypatch):
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.ChatPolicy.from_entries([BOB]))
        with pytest.raises(ResourceError) as exc:
            await main.mcp.read_resource(_uri("DOC1"))
        assert str(exc.value).startswith("denied:")

    async def test_an_unknown_message_is_not_found(self, store):
        with pytest.raises(ResourceNotFoundError):
            await main.mcp.read_resource(_uri("NOPE"))

    async def test_a_text_message_carries_no_media(self, store):
        with pytest.raises(ResourceError) as exc:
            await main.mcp.read_resource(_uri("T1"))
        assert str(exc.value).startswith("invalid_argument:")

    async def test_a_file_over_the_cap_is_too_large(self, store, monkeypatch):
        def explode(*_args, **_kwargs):
            raise AssertionError("nothing this big should be fetched first")

        monkeypatch.setattr(whatsapp, "download_media", explode)
        with pytest.raises(ResourceError) as exc:
            await main.mcp.read_resource(_uri("BIG1"))
        assert str(exc.value).startswith("too_large:")

    async def test_an_uncached_file_when_download_media_is_denied(self, store, monkeypatch):
        def explode(*_args, **_kwargs):
            raise AssertionError("the tool policy should have stopped this fetch")

        monkeypatch.setattr(whatsapp, "download_media", explode)
        tool_policy.set_active_policy(ToolPolicy(deny=frozenset({"download_media"})))
        with pytest.raises(ResourceError) as exc:
            await main.mcp.read_resource(_uri("GONE"))
        assert str(exc.value).startswith("denied:") and "download_media" in str(exc.value)

    async def test_a_cached_file_is_still_readable_with_download_media_denied(self, store):
        tool_policy.set_active_policy(ToolPolicy(deny=frozenset({"download_media"})))
        assert list(await main.mcp.read_resource(_uri("DOC1")))[0].content == PDF

    async def test_a_symlink_out_of_the_chat_directory(self, store, tmp_path):
        secret = tmp_path / ".bridge-token"
        secret.write_text("token")
        link = os.path.join(media_inventory.chat_media_dir(ALICE), "document_20260904_100000_GONE.pdf")
        try:
            os.symlink(secret, link)
        except (OSError, NotImplementedError):
            pytest.skip("this platform does not allow creating symlinks here")
        with pytest.raises(ResourceError) as exc:
            await main.mcp.read_resource(_uri("GONE"))
        assert str(exc.value).startswith("denied:")


class TestListMediaLinks:
    def test_every_row_carries_a_link_to_its_bytes(self, store):
        items = main.list_media(chat_jid=ALICE)["items"]
        links = {item["message_id"]: item["resource_link"] for item in items}
        assert links["DOC1"] == {
            "type": "resource_link",
            "uri": _uri("DOC1"),
            "name": "report.pdf",
            "mimeType": "application/pdf",
            "size": len(PDF),
        }

    def test_the_row_shape_is_otherwise_unchanged(self, store):
        item = next(item for item in main.list_media(chat_jid=ALICE)["items"] if item["message_id"] == "DOC1")
        assert item["filename"] == "report.pdf" and item["cached"] is True
        assert item["bytes"] == len(PDF) and item["sha256"] is None

    def test_a_row_with_nothing_cached_still_gets_a_link(self, store):
        item = next(item for item in main.list_media(chat_jid=ALICE)["items"] if item["message_id"] == "GONE")
        assert item["cached"] is False
        assert item["resource_link"]["uri"] == _uri("GONE")
        assert item["resource_link"]["mimeType"] == "application/pdf"
