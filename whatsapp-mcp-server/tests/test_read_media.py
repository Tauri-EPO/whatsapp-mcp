"""read_media returns the bytes as content blocks, capped, and only from the store."""

import base64
import json
import mimetypes
import os

import pytest

import chat_policy
import main
import media_inventory
import media_read
import media_text
import tool_policy
import whatsapp
from errors import ToolError
from tests.conftest import ALICE, BOB
from tool_policy import ALLOW_TOOLS_ENV, DENY_TOOLS_ENV, ToolPolicy
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


def _blob(block):
    """``(uri, mime, bytes)`` of an EmbeddedResource block."""
    assert block.type == "resource"
    return block.resource.uri, block.resource.mime_type, base64.b64decode(block.resource.blob)


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

    def test_a_pdf_comes_back_as_an_embedded_resource(self, store):
        """The block a client can hand to the model as a document (#367)."""
        _cache(ALICE, "document_20260904_100000_DOC1.pdf", b"%PDF-1.4 not really")
        blocks = media_read.read_media(ALICE, "DOC1")
        uri, mime, data = _blob(blocks[0])
        assert mime == "application/pdf" and data == b"%PDF-1.4 not really"
        assert uri == f"whatsapp://media/{ALICE}/DOC1"
        assert _meta(blocks)["mime"] == "application/pdf"

    def test_an_extractable_document_says_as_text_is_there(self, store):
        """A client that drops the resource must not leave the PDF looking empty."""
        _cache(ALICE, "document_20260904_100000_DOC1.pdf", b"%PDF-1.4 not really")
        blocks = media_read.read_media(ALICE, "DOC1")
        assert [block.type for block in blocks] == ["resource", "text", "text"]
        assert "as_text=true" in blocks[1].text and "application/pdf" in blocks[1].text

    def test_a_video_resource_carries_no_such_hint(self, store):
        """There is nothing to extract from an MP4, so nothing to advertise."""
        with store.messages() as conn:
            _insert(conn, "VID1", ALICE, "video", None, filename="clip.mp4")
        _cache(ALICE, "video_20260904_100000_VID1.mp4", b"\x00\x00\x00\x18ftypmp42")
        assert [block.type for block in media_read.read_media(ALICE, "VID1")] == ["resource", "text"]

    @pytest.mark.parametrize(
        ("name", "mime"),
        [
            ("contrato.docx", media_text.DOCX_MIME),
            ("leituras.xlsx", media_text.XLSX_MIME),
            ("clip.mp4", "video/mp4"),
        ],
    )
    def test_the_resource_declares_the_real_office_or_video_type(self, store, name, mime):
        with store.messages() as conn:
            _insert(conn, "OFF1", ALICE, "document", None, filename=name)
        _cache(ALICE, f"document_20260904_100000_OFF1{os.path.splitext(name)[1]}", b"payload")
        assert _blob(media_read.read_media(ALICE, "OFF1")[0])[1] == mime

    def test_a_voice_note_comes_back_as_audio_content(self, store):
        with store.messages() as conn:
            _insert(conn, "OGG1", ALICE, "audio", None, filename="nota.ogg")
        _cache(ALICE, "audio_20260904_100000_OGG1.ogg", b"OggS not really opus")
        blocks = media_read.read_media(ALICE, "OGG1")
        assert blocks[0].type == "audio" and blocks[0].mime_type == "audio/ogg"
        assert base64.b64decode(blocks[0].data) == b"OggS not really opus"

    def test_an_mp3_under_the_bridge_s_ogg_name_is_declared_audio_mpeg(self, store):
        """content.go names every audioMessage .ogg; an AudioContent must not lie."""
        with store.messages() as conn:
            _insert(conn, "MP31", ALICE, "audio", None, filename="audio_20260904_100000_MP31.ogg")
        _cache(ALICE, "audio_20260904_100000_MP31.ogg", b"ID3\x03\x00\x00\x00 not really mp3")
        blocks = media_read.read_media(ALICE, "MP31")
        assert blocks[0].type == "audio" and blocks[0].mime_type == "audio/mpeg"
        assert _meta(blocks)["mime"] == "audio/mpeg"

    def test_audio_bytes_matching_nothing_are_not_an_audio_block(self, store):
        """Same rule as the image branch: an unknown payload is never a typed block."""
        with store.messages() as conn:
            _insert(conn, "ODD1", ALICE, "audio", None, filename="nota.ogg")
        _cache(ALICE, "audio_20260904_100000_ODD1.ogg", b"not audio at all")
        assert _blob(media_read.read_media(ALICE, "ODD1")[0])[1] == "application/octet-stream"

    def test_a_flac_keeps_its_own_type_as_a_resource(self, store):
        """A type no client plays is still worth naming; only playable audio is a block."""
        with store.messages() as conn:
            _insert(conn, "FLA1", ALICE, "audio", None, filename="nota.ogg")
        _cache(ALICE, "audio_20260904_100000_FLA1.ogg", b"fLaC\x00\x00\x00\x22")
        assert _blob(media_read.read_media(ALICE, "FLA1")[0])[1] == "audio/flac"

    def test_an_audio_type_no_client_plays_stays_a_resource(self, store, monkeypatch):
        """AudioContent is for the types a client can play; the rest keep their bytes."""
        monkeypatch.setattr(media_read, "PLAYABLE_AUDIO_MIMES", frozenset())
        with store.messages() as conn:
            _insert(conn, "OGG2", ALICE, "audio", None, filename="nota.ogg")
        _cache(ALICE, "audio_20260904_100000_OGG2.ogg", b"OggS")
        assert _blob(media_read.read_media(ALICE, "OGG2")[0])[1] == "audio/ogg"

    def test_the_uri_survives_a_device_suffix_in_the_jid(self):
        """':' would otherwise read as the end of an authority, not part of a segment."""
        assert media_read.media_uri("5511999999999:12@s.whatsapp.net", "A/B") == (
            "whatsapp://media/5511999999999%3A12@s.whatsapp.net/A%2FB"
        )


class TestTypes:
    def test_the_declared_type_comes_from_the_bytes_not_the_extension(self, store):
        """A PNG cached as .jpg must not be announced as image/jpeg: clients check."""
        blocks = media_read.read_media(ALICE, "IMG1")
        assert blocks[0].mime_type == "image/png"
        assert _meta(blocks)["mime"] == "image/png"

    def test_an_image_type_no_client_renders_falls_back_to_a_resource(self, store):
        """image/* is not enough: an ImageContent a client refuses fails the whole call."""
        assert _blob(media_read.read_media(ALICE, "TIF1")[0])[1] == "image/tiff"

    def test_the_types_do_not_depend_on_the_platform_s_mime_table(self, monkeypatch):
        """python:3.13-slim has no /etc/mime.types: .docx, .xlsx and .ogg are None there."""
        bare = mimetypes.MimeTypes(filenames=())
        assert bare.guess_type("a.docx") == (None, None)
        monkeypatch.setattr(media_read.mimetypes, "guess_type", bare.guess_type)
        assert media_read.guess_mime("document", "contrato.docx") == media_text.DOCX_MIME
        assert media_read.guess_mime("document", "leituras.xlsx") == media_text.XLSX_MIME
        assert media_read.guess_mime("audio", "nota.ogg") == "audio/ogg"
        # CPython's built-in table answers audio/x-wav for .wav, which is not a
        # type PLAYABLE_AUDIO_MIMES knows: the block would change per platform.
        assert bare.guess_type("a.wav") == ("audio/x-wav", None)
        assert media_read.guess_mime("audio", "nota.wav") == "audio/wav"
        # ...and not on Windows either, where the registry calls a CSV a spreadsheet.
        assert media_read.guess_mime("document", "planilha.csv") == "text/csv"

    def test_a_name_promising_an_image_over_other_bytes_is_not_an_image_block(self, store):
        """A client rejects a block whose data disagrees with its declared type."""
        _cache(ALICE, "image_20260904_100000_IMG1.jpg", b"%PDF-1.4 not an image at all")
        _uri, mime, data = _blob(media_read.read_media(ALICE, "IMG1")[0])
        assert mime == "application/octet-stream" and data == b"%PDF-1.4 not an image at all"

    def test_a_compressed_text_file_is_not_decoded_as_text(self, store):
        """notes.txt.gz guesses as text/plain *with an encoding*; the bytes are an archive."""
        assert _blob(media_read.read_media(ALICE, "GZ1")[0])[1] == "application/gzip"


class TestResolution:
    def test_uncached_media_is_downloaded_through_the_bridge(self, store, monkeypatch):
        calls = []

        def fake_download(message_id, chat_jid):
            calls.append((message_id, chat_jid))
            return _cache(chat_jid, "document_20260904_100000_DOC1.pdf", b"%PDF-1.4")

        monkeypatch.setattr(media_read.whatsapp, "download_media", fake_download)
        blocks = media_read.read_media(ALICE, "DOC1")
        assert calls == [("DOC1", ALICE)]
        assert _blob(blocks[0])[2] == b"%PDF-1.4"

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

    def test_a_resource_block_survives_the_decorators_whole(self, store, monkeypatch):
        """clean_untrusted walks dicts and lists; a content block must pass through untouched."""
        monkeypatch.setenv(WRAP_ENV, "1")
        _cache(ALICE, "document_20260904_100000_DOC1.pdf", b"%PDF-1.4 not really")
        blocks = main.read_media(chat_jid=ALICE, message_id="DOC1")
        assert [block.type for block in blocks] == ["resource", "text", "text"]
        assert _blob(blocks[0])[2] == b"%PDF-1.4 not really"

    async def test_the_sdk_ships_the_resource_block_over_the_wire(self, store):
        """The strict-args server still counts and converts a block-returning call."""
        _cache(ALICE, "document_20260904_100000_DOC1.pdf", b"%PDF-1.4 not really")
        result = await main.mcp.call_tool("read_media", {"chat_jid": ALICE, "message_id": "DOC1"})
        assert [block.type for block in result.content] == ["resource", "text", "text"]
        assert result.content[0].resource.mime_type == "application/pdf"

    def test_the_sdk_publishes_it_as_unstructured_content(self):
        """The return annotation is what makes the SDK emit blocks instead of JSON."""
        tool = main.mcp._tool_manager.get_tool("read_media")
        assert tool is not None and tool.fn_metadata.output_schema is None


class TestImplicitDownloadPolicy:
    """Taking download_media off the list stops the fetch these tools make (#350)."""

    @pytest.fixture(autouse=True)
    def _never_fetches(self, monkeypatch):
        def explode(*args, **kwargs):
            raise AssertionError("the tool policy should have stopped this fetch")

        monkeypatch.setattr(whatsapp, "download_media", explode)
        monkeypatch.setattr(main, "whatsapp_download_media", explode)
        monkeypatch.setattr(main, "load_whisper_config", lambda: "cfg")
        monkeypatch.setattr(
            main,
            "transcribe_file",
            lambda path, language=None, config=None: {"text": "olá", "language": "pt", "backend": "server"},
        )
        yield
        tool_policy.set_active_policy(None)

    def _denied(self, envelope):
        assert envelope["code"] == "denied"
        assert "download_media" in envelope["message"]
        return envelope["message"]

    def test_read_media_refuses_a_file_that_is_not_cached(self, store):
        tool_policy.set_active_policy(ToolPolicy(deny=frozenset({"download_media"})))
        out = main.read_media(chat_jid=ALICE, message_id="DOC1")
        assert out.is_error is True
        assert DENY_TOOLS_ENV in self._denied(out.structured_content["error"])

    def test_read_media_still_reads_the_cached_bytes(self, store):
        tool_policy.set_active_policy(ToolPolicy(deny=frozenset({"download_media"})))
        assert [block.type for block in main.read_media(chat_jid=ALICE, message_id="IMG1")] == ["image", "text"]

    def test_an_allow_list_without_download_media_refuses_the_same_way(self, store):
        tool_policy.set_active_policy(ToolPolicy(allow=frozenset({"read_media", "transcribe_audio"})))
        out = main.read_media(chat_jid=ALICE, message_id="DOC1")
        assert ALLOW_TOOLS_ENV in self._denied(out.structured_content["error"])

    def test_transcribe_audio_refuses_a_voice_note_that_is_not_cached(self, store):
        tool_policy.set_active_policy(ToolPolicy(deny=frozenset({"download_media"})))
        self._denied(main.transcribe_audio(chat_jid=ALICE, message_id="DOC1")["error"])

    def test_transcribe_audio_reads_the_cached_file(self, store):
        tool_policy.set_active_policy(ToolPolicy(deny=frozenset({"download_media"})))
        out = main.transcribe_audio(chat_jid=ALICE, message_id="TXT1")
        assert out["success"] and out["text"] == "olá"
        assert out["file_path"].endswith("document_20260904_100000_TXT1.txt")

    def test_a_denied_chat_still_wins_over_the_cache(self, store, monkeypatch):
        """The cached path is not a way around WHATSAPP_ALLOWED_CHATS."""
        tool_policy.set_active_policy(ToolPolicy(deny=frozenset({"download_media"})))
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.ChatPolicy.from_entries([BOB]))
        out = main.transcribe_audio(chat_jid=ALICE, message_id="TXT1")
        assert out["error"]["code"] == "denied" and "download_media" not in out["error"]["message"]

    def test_a_bad_id_is_still_a_bad_id_and_not_a_refusal(self, store):
        """The row is read first: "denied" must not hide a typo or a text message."""
        tool_policy.set_active_policy(ToolPolicy(deny=frozenset({"download_media"})))
        assert main.transcribe_audio(chat_jid=ALICE, message_id="NOPE")["error"]["code"] == "not_found"
        assert main.transcribe_audio(chat_jid=ALICE, message_id="T1")["error"]["code"] == "invalid_argument"
        assert main.read_media(chat_jid=ALICE, message_id="NOPE").structured_content["error"]["code"] == "not_found"

    def test_an_oversized_uncached_file_is_denied_not_pointed_at_download_media(self, store):
        """too_large advises download_media, which is exactly what is disabled here."""
        tool_policy.set_active_policy(ToolPolicy(deny=frozenset({"download_media"})))
        out = main.read_media(chat_jid=ALICE, message_id="BIG1")
        self._denied(out.structured_content["error"])

    def test_the_fetch_happens_as_before_when_the_policy_allows_it(self, store, monkeypatch):
        """The whole point of the gate is that nothing changes without it."""
        tool_policy.set_active_policy(ToolPolicy())
        monkeypatch.setattr(
            whatsapp,
            "download_media",
            lambda mid, chat: _cache(chat, f"document_20260904_100000_{mid}.pdf", b"%PDF-1.4 not really"),
        )
        blocks = main.read_media(chat_jid=ALICE, message_id="DOC1")
        assert _blob(blocks[0])[2] == b"%PDF-1.4 not really"
