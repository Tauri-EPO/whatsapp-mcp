"""read_media returns the bytes as content blocks, capped, and only from the store."""

import base64
import json
import os

import pytest

import chat_policy
import main
import media_inventory
import media_read
import whatsapp
from errors import ToolError
from tests.conftest import ALICE, BOB
from untrusted import CLOSE_TAG, OPEN_TAG, WRAP_ENV

SHA_IMG = bytes.fromhex("aa" * 32)
SHA_DOC = bytes.fromhex("bb" * 32)
SHA_TXT = bytes.fromhex("cc" * 32)

# A one-pixel PNG: real bytes, so the mime guess and the base64 round trip are
# not tested against a placeholder.
PNG = base64.b64decode(
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
)


def _insert(conn, msg_id, chat, media_type, sha, filename=None, length=10):
    conn.execute(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, "
        "file_length, file_sha256) VALUES (?, ?, '5511888888888', '', '2026-09-04 10:00:00+00:00', 0, ?, ?, ?, ?)",
        (msg_id, chat, media_type, filename, length, sha),
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
    """One cached image, one cached text file, one document with no cached bytes."""
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.load_chat_policy({}))
    with paired_dbs.messages() as conn:
        _insert(conn, "IMG1", ALICE, "image", SHA_IMG)
        _insert(conn, "TXT1", ALICE, "document", SHA_TXT, filename="notes.txt")
        _insert(conn, "DOC1", ALICE, "document", SHA_DOC, filename="report.pdf")
        _insert(conn, "TIF1", ALICE, "document", None, filename="scan.tiff")
        _insert(conn, "GZ1", ALICE, "document", None, filename="notes.txt.gz")
        _insert(conn, "BIG1", ALICE, "video", None, filename="movie.mp4", length=200_000_000)
        conn.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) "
            "VALUES ('T1', ?, 'x', 'hi', '2026-09-04 09:00:00+00:00', 0)",
            (ALICE,),
        )
    # The bridge names every cached image .jpg whatever it holds, so this one
    # is a PNG behind a .jpg name on purpose.
    _cache(ALICE, "image_20260904_100000_IMG1.jpg", PNG)
    _cache(ALICE, "document_20260904_100000_TXT1.txt", "olá, mundo\n".encode())
    _cache(ALICE, "document_20260904_100000_TIF1.tiff", b"II* not really a tiff")
    _cache(ALICE, "document_20260904_100000_GZ1.gz", b"not really gzip")
    return paired_dbs


def _meta(blocks):
    return json.loads(blocks[-1].text)


class TestBlocks:
    def test_image_comes_back_as_image_content(self, store):
        blocks = media_read.read_media(ALICE, "IMG1")
        assert blocks[0].type == "image"
        assert blocks[0].mime_type == "image/png"
        assert base64.b64decode(blocks[0].data) == PNG

    def test_metadata_block_closes_the_annotate_loop(self, store):
        meta = _meta(media_read.read_media(ALICE, "IMG1"))
        assert meta == {
            "sha256": "aa" * 32,
            "mime": "image/png",
            "bytes": len(PNG),
            "truncated": False,
            "notes": {},
        }

    def test_metadata_carries_the_notes_already_written(self, store):
        import media_notes

        media_notes.annotate_media("aa" * 32, "summary", "a photo of the invoice")
        assert _meta(media_read.read_media(ALICE, "IMG1"))["notes"] == {"summary": "a photo of the invoice"}

    def test_text_file_comes_back_as_text(self, store):
        blocks = media_read.read_media(ALICE, "TXT1")
        assert blocks[0].type == "text" and blocks[0].text == "olá, mundo\n"
        assert _meta(blocks)["mime"] == "text/plain"

    def test_text_is_delimited_when_the_envelope_is_on(self, store, monkeypatch):
        monkeypatch.setenv(WRAP_ENV, "1")
        assert media_read.read_media(ALICE, "TXT1")[0].text == f"{OPEN_TAG}olá, mundo\n{CLOSE_TAG}"

    def test_other_types_come_back_as_base64_behind_a_header(self, store):
        _cache(ALICE, "document_20260904_100000_DOC1.pdf", b"%PDF-1.4 not really")
        blocks = media_read.read_media(ALICE, "DOC1")
        header, payload = blocks[0].text.split("\n", 1)
        assert header == "base64:application/pdf:19"
        assert base64.b64decode(payload) == b"%PDF-1.4 not really"


class TestTypes:
    def test_the_declared_type_comes_from_the_bytes_not_the_extension(self, store):
        """A PNG cached as .jpg must not be announced as image/jpeg: clients check."""
        blocks = media_read.read_media(ALICE, "IMG1")
        assert blocks[0].mime_type == "image/png"
        assert _meta(blocks)["mime"] == "image/png"

    def test_an_image_type_no_client_renders_falls_back_to_base64(self, store):
        """image/* is not enough: an ImageContent a client refuses fails the whole call."""
        blocks = media_read.read_media(ALICE, "TIF1")
        assert blocks[0].type == "text" and blocks[0].text.startswith("base64:image/tiff:")

    def test_a_compressed_text_file_is_not_decoded_as_text(self, store):
        """notes.txt.gz guesses as text/plain *with an encoding*; the bytes are an archive."""
        blocks = media_read.read_media(ALICE, "GZ1")
        assert blocks[0].text.startswith("base64:application/gzip:")


class TestResolution:
    def test_uncached_media_is_downloaded_through_the_bridge(self, store, monkeypatch):
        calls = []

        def fake_download(message_id, chat_jid):
            calls.append((message_id, chat_jid))
            return _cache(chat_jid, "document_20260904_100000_DOC1.pdf", b"%PDF-1.4")

        monkeypatch.setattr(media_read.whatsapp, "download_media", fake_download)
        blocks = media_read.read_media(ALICE, "DOC1")
        assert calls == [("DOC1", ALICE)]
        assert blocks[0].text.startswith("base64:application/pdf:8\n")

    def test_cached_media_never_calls_the_bridge(self, store, monkeypatch):
        def explode(*_args, **_kwargs):
            raise AssertionError("the bridge must not be called for a cached file")

        monkeypatch.setattr(media_read.whatsapp, "download_media", explode)
        assert media_read.read_media(ALICE, "IMG1")[0].type == "image"

    def test_an_oversized_file_is_refused_before_it_is_fetched(self, store, monkeypatch):
        """The row already knows the size: no 200 MB transfer to then say too_large."""

        def explode(*_args, **_kwargs):
            raise AssertionError("nothing this big should be downloaded first")

        monkeypatch.setattr(media_read.whatsapp, "download_media", explode)
        with pytest.raises(ToolError) as exc:
            media_read.read_media(ALICE, "BIG1")
        assert exc.value.code == "too_large" and exc.value.extra["bytes"] == 200_000_000

    def test_a_path_outside_the_store_is_refused(self, store, monkeypatch, tmp_path):
        # tmp_path *is* the store here (messages.db lives in it), so the file
        # the bridge points at has to be one level up to be outside it.
        outside = tmp_path.parent / "elsewhere.pdf"
        outside.write_bytes(b"%PDF-1.4")
        monkeypatch.setattr(media_read.whatsapp, "download_media", lambda *_: str(outside))
        with pytest.raises(ToolError) as exc:
            media_read.read_media(ALICE, "DOC1")
        assert exc.value.code == "denied"

    @pytest.mark.parametrize("target", ["outside", "bridge-token"])
    def test_a_symlink_leaving_the_chat_directory_is_refused(self, store, tmp_path, target):
        """Not just outside the store: the store root holds .bridge-token too."""
        secret = tmp_path.parent / "secret.txt" if target == "outside" else tmp_path / ".bridge-token"
        secret.write_text("token")
        link = os.path.join(media_inventory.chat_media_dir(ALICE), "document_20260904_100000_DOC1.pdf")
        try:
            os.symlink(secret, link)
        except (OSError, NotImplementedError):
            pytest.skip("this platform does not allow creating symlinks here")
        with pytest.raises(ToolError) as exc:
            media_read.read_media(ALICE, "DOC1")
        assert exc.value.code == "denied"


class TestRefusals:
    def test_a_file_over_the_cap_is_too_large(self, store, monkeypatch):
        monkeypatch.setattr(media_read, "MAX_IMAGE_BYTES", 10)
        with pytest.raises(ToolError) as exc:
            media_read.read_media(ALICE, "IMG1")
        assert exc.value.code == "too_large"
        assert exc.value.extra == {"bytes": len(PNG), "limit": 10}
        assert "download_media" in exc.value.message

    def test_max_bytes_lowers_the_cap_but_cannot_raise_it(self, store):
        with pytest.raises(ToolError) as exc:
            media_read.read_media(ALICE, "IMG1", max_bytes=10)
        assert exc.value.extra["limit"] == 10
        assert media_read.cap(1_000_000_000, media_read.MAX_IMAGE_BYTES) == media_read.MAX_IMAGE_BYTES
        assert media_read.cap(0, media_read.MAX_IMAGE_BYTES) == media_read.MAX_IMAGE_BYTES

    def test_unknown_message_is_not_found(self, store):
        with pytest.raises(ToolError) as exc:
            media_read.read_media(ALICE, "NOPE")
        assert exc.value.code == "not_found"

    def test_a_text_message_has_no_media(self, store):
        with pytest.raises(ToolError) as exc:
            media_read.read_media(ALICE, "T1")
        assert exc.value.code == "invalid_argument"

    def test_a_denied_chat_is_refused_before_anything_is_read(self, store, monkeypatch):
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.ChatPolicy.from_entries([BOB]))
        out = main.read_media(chat_jid=ALICE, message_id="IMG1")
        assert out.structured_content["error"]["code"] == "denied"


class TestTool:
    def test_the_tool_returns_the_blocks_unchanged(self, store):
        blocks = main.read_media(chat_jid=ALICE, message_id="IMG1")
        assert [block.type for block in blocks] == ["image", "text"]

    def test_a_failure_answers_the_envelope_on_both_channels(self, store):
        """No output schema means the SDK cannot build this one: content_tool does."""
        out = main.read_media(chat_jid=ALICE, message_id="NOPE")
        assert out.is_error is True
        assert out.structured_content["error"]["code"] == "not_found"
        assert json.loads(out.content[0].text)["error"]["code"] == "not_found"

    def test_the_sdk_publishes_it_as_unstructured_content(self):
        """The return annotation is what makes the SDK emit blocks instead of JSON."""
        tool = main.mcp._tool_manager.get_tool("read_media")
        assert tool is not None and tool.fn_metadata.output_schema is None
