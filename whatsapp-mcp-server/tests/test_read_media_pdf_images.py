"""read_media(as_images=True): a scanned PDF rendered here, one picture per page (#368)."""

import base64
import io
import json
import os

import pytest
from PIL import Image

import chat_policy
import main
import media_image
import media_inventory
import media_pdf
import media_read
import whatsapp
from errors import ToolError
from tests.conftest import ALICE
from tests.test_read_media_text import make_pdf


def _insert(conn, msg_id, filename="laudo.pdf"):
    conn.execute(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, "
        "file_length) VALUES (?, ?, '5511888888888', '', '2026-09-04 10:00:00+00:00', 0, 'document', ?, 0)",
        (msg_id, ALICE, filename),
    )


def _cache(name: str, data: bytes) -> str:
    directory = media_inventory.chat_media_dir(ALICE)
    os.makedirs(directory, exist_ok=True)
    path = os.path.join(directory, name)
    with open(path, "wb") as handle:
        handle.write(data)
    return path


def _meta(blocks):
    return json.loads(blocks[-1].text)


def _images(blocks):
    return [block for block in blocks if block.type == "image"]


def _markers(blocks):
    return [block.text for block in blocks[:-1] if block.type == "text"]


def _size(block) -> tuple[int, int]:
    with Image.open(io.BytesIO(base64.b64decode(block.data))) as image:
        return image.size


def _encrypted_pdf() -> bytes:
    from pypdf import PdfWriter

    writer = PdfWriter()
    writer.add_blank_page(width=200, height=200)
    writer.encrypt("segredo")
    buffer = io.BytesIO()
    writer.write(buffer)
    return buffer.getvalue()


@pytest.fixture
def scans(paired_dbs, monkeypatch):
    """A three-page PDF and a seven-page one, both cached."""
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.load_chat_policy({}))
    with paired_dbs.messages() as conn:
        _insert(conn, "PDF3")
        _insert(conn, "PDF7")
        _insert(conn, "IMG1", filename="foto.jpg")
    _cache("document_20260904_100000_PDF3.pdf", make_pdf(["uma", "duas", "tres"]))
    _cache("document_20260904_100000_PDF7.pdf", make_pdf([f"pagina {n}" for n in range(1, 8)]))
    return paired_dbs


class TestPages:
    def test_every_page_comes_back_as_a_picture_behind_its_marker(self, scans):
        blocks = media_read.read_media(ALICE, "PDF3", as_images=True)
        assert [block.type for block in blocks] == ["text", "image", "text", "image", "text", "image", "text"]
        assert _markers(blocks) == [
            "--- page 1 of 3 (rendered image) ---",
            "--- page 2 of 3 (rendered image) ---",
            "--- page 3 of 3 (rendered image) ---",
        ]
        assert all(block.mime_type == "image/jpeg" for block in _images(blocks))
        meta = _meta(blocks)
        assert (meta["pages_total"], meta["pages_rendered"], meta["truncated"]) == (3, 3, False)
        assert meta["image_bytes"] > 0 and meta["mime"] == "application/pdf"

    def test_the_pages_are_really_rendered_at_max_edge(self, scans):
        """The fixture's pages are 200x200 points, so the long edge is the target."""
        block = _images(media_read.read_media(ALICE, "PDF3", as_images=True))[0]
        assert max(_size(block)) == media_image.DEFAULT_MAX_EDGE
        smaller = _images(media_read.read_media(ALICE, "PDF3", as_images=True, max_edge=400))[0]
        assert max(_size(smaller)) == 400

    def test_the_rendered_edge_is_capped_whatever_max_edge_asks(self, scans):
        block = _images(media_read.read_media(ALICE, "PDF3", as_images=True, max_edge=8192))[0]
        assert max(_size(block)) == media_pdf.MAX_EDGE

    def test_max_edge_zero_still_renders(self, scans):
        """as_images was asked for pictures; "the stored bytes" is the opposite request."""
        blocks = media_read.read_media(ALICE, "PDF3", as_images=True, max_edge=0)
        assert len(_images(blocks)) == 3

    def test_max_pages_caps_the_answer_and_says_truncated(self, scans):
        blocks = media_read.read_media(ALICE, "PDF3", as_images=True, max_pages=2)
        assert len(_images(blocks)) == 2
        assert _markers(blocks)[0] == "--- page 1 of 3 (rendered image) ---"
        meta = _meta(blocks)
        assert (meta["pages_total"], meta["pages_rendered"], meta["truncated"]) == (3, 2, True)
        # ...and names the call that fetches the rest, or the pages are unreachable.
        assert "first_page=3" in _markers(blocks)[-1]

    def test_first_page_walks_a_document_longer_than_one_answer(self, scans):
        blocks = media_read.read_media(ALICE, "PDF7", as_images=True, first_page=6)
        assert len(_images(blocks)) == 2
        assert _markers(blocks)[:2] == [
            "--- page 6 of 7 (rendered image) ---",
            "--- page 7 of 7 (rendered image) ---",
        ]
        meta = _meta(blocks)
        assert (meta["first_page"], meta["pages_rendered"], meta["truncated"]) == (6, 2, False)

    def test_a_first_page_past_the_end_says_so(self, scans):
        with pytest.raises(ToolError) as exc:
            media_read.read_media(ALICE, "PDF3", as_images=True, first_page=10)
        assert exc.value.code == "invalid_argument" and "3 page" in exc.value.message

    def test_first_page_is_one_based_and_validated(self):
        assert media_pdf.first_page(0) == 1
        assert media_pdf.first_page(7) == 7
        with pytest.raises(ToolError):
            media_pdf.first_page(-1)
        with pytest.raises(ToolError):
            media_pdf.first_page("segunda")

    def test_a_page_the_renderer_cannot_draw_does_not_lose_the_others(self, scans, monkeypatch):
        """Pages are independent: one broken image stream must not cost the whole scan."""
        real_encode = media_image.encode
        calls = {"n": 0}

        def flaky(image, quality):
            calls["n"] += 1
            if calls["n"] == 2:
                raise ValueError("broken content stream")
            return real_encode(image, quality)

        monkeypatch.setattr(media_image, "encode", flaky)
        blocks = media_read.read_media(ALICE, "PDF3", as_images=True)
        assert len(_images(blocks)) == 2
        meta = _meta(blocks)
        assert meta["pages_failed"] == [2] and meta["truncated"] is True
        assert _markers(blocks)[:2] == [
            "--- page 1 of 3 (rendered image) ---",
            "--- page 3 of 3 (rendered image) ---",
        ]

    def test_a_document_where_every_page_fails_is_an_error(self, scans, monkeypatch):
        def explode(image, quality):
            raise ValueError("broken content stream")

        monkeypatch.setattr(media_image, "encode", explode)
        with pytest.raises(ToolError) as exc:
            media_read.read_media(ALICE, "PDF3", as_images=True)
        assert exc.value.code == "invalid_argument" and "damaged" in exc.value.message

    def test_the_renderer_is_serialised(self):
        """PDFium is not thread-safe and the SDK runs sync tools on a thread pool."""
        import threading

        assert isinstance(media_pdf._pdfium_lock, type(threading.Lock()))

    def test_the_default_is_five_pages_not_twenty(self, scans):
        """A page is a few hundred KB here and a few KB with as_text; the default is small."""
        meta = _meta(media_read.read_media(ALICE, "PDF7", as_images=True))
        assert (meta["pages_total"], meta["pages_rendered"], meta["truncated"]) == (7, media_pdf.DEFAULT_PAGES, True)

    def test_the_total_payload_is_bounded_and_never_empty(self, scans, monkeypatch):
        monkeypatch.setattr(media_pdf, "MAX_TOTAL_BYTES", 1)
        blocks = media_read.read_media(ALICE, "PDF3", as_images=True)
        assert len(_images(blocks)) == 1
        assert _meta(blocks)["truncated"] is True

    def test_a_pdf_over_the_blob_cap_is_still_renderable(self, scans, monkeypatch):
        """The same trade as as_text: the file's size stops mattering once we read it here."""
        monkeypatch.setattr(media_read, "MAX_BASE64_BYTES", 500)
        with pytest.raises(ToolError) as exc:
            media_read.read_media(ALICE, "PDF3")
        assert exc.value.code == "too_large"
        assert len(_images(media_read.read_media(ALICE, "PDF3", as_images=True))) == 3


class TestRefusals:
    def test_as_text_and_as_images_cannot_be_combined(self, scans):
        with pytest.raises(ToolError) as exc:
            media_read.read_media(ALICE, "PDF3", as_text=True, as_images=True)
        assert exc.value.code == "invalid_argument"
        assert "as_text" in exc.value.message and "as_images" in exc.value.message

    def test_the_refusal_costs_no_database_read(self, scans, monkeypatch):
        def explode(*_args, **_kwargs):
            raise AssertionError("two contradictory arguments must be refused before any I/O")

        monkeypatch.setattr(media_read, "resolve_media", explode)
        with pytest.raises(ToolError):
            media_read.read_media(ALICE, "PDF3", as_text=True, as_images=True)

    def test_as_images_on_anything_but_a_pdf_is_refused(self, scans):
        _cache("document_20260904_100000_IMG1.jpg", b"\xff\xd8\xff\xe0 not a pdf")
        with pytest.raises(ToolError) as exc:
            media_read.read_media(ALICE, "IMG1", as_images=True)
        assert exc.value.code == "invalid_argument"
        assert "as_text" in exc.value.message and "transcribe_audio" in exc.value.message

    def test_an_encrypted_pdf_says_so(self, scans):
        _cache("document_20260904_100000_PDF3.pdf", _encrypted_pdf())
        with pytest.raises(ToolError) as exc:
            media_read.read_media(ALICE, "PDF3", as_images=True)
        assert exc.value.code == "invalid_argument"
        assert "password" in exc.value.message.lower()

    def test_a_corrupt_pdf_is_the_caller_s_problem_not_an_internal_error(self, scans):
        _cache("document_20260904_100000_PDF3.pdf", b"%PDF-1.4 and then nothing useful")
        with pytest.raises(ToolError) as exc:
            media_read.read_media(ALICE, "PDF3", as_images=True)
        assert exc.value.code == "invalid_argument"

    def test_the_page_limit_is_validated_and_capped(self):
        assert media_pdf.page_limit(0) == media_pdf.DEFAULT_PAGES
        assert media_pdf.page_limit(10_000) == media_pdf.MAX_PAGES_LIMIT
        with pytest.raises(ToolError):
            media_pdf.page_limit("todas")

    def test_as_images_still_obeys_the_chat_allow_list(self, scans, monkeypatch):
        from tests.conftest import BOB

        monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.ChatPolicy.from_entries([BOB]))
        out = main.read_media(chat_jid=ALICE, message_id="PDF3", as_images=True)
        assert out.structured_content["error"]["code"] == "denied"


class TestTool:
    def test_the_tool_renders_the_pages(self, scans):
        blocks = main.read_media(chat_jid=ALICE, message_id="PDF3", as_images=True, max_pages=1)
        # marker, page, "there is more", metadata
        assert [block.type for block in blocks] == ["text", "image", "text", "text"]
        assert _meta(blocks)["pages_rendered"] == 1

    def test_the_contradiction_answers_the_envelope_on_both_channels(self, scans):
        out = main.read_media(chat_jid=ALICE, message_id="PDF3", as_text=True, as_images=True)
        assert out.is_error is True
        assert out.structured_content["error"]["code"] == "invalid_argument"

    async def test_the_sdk_ships_the_page_images_over_the_wire(self, scans):
        result = await main.mcp.call_tool(
            "read_media", {"chat_jid": ALICE, "message_id": "PDF3", "as_images": True, "max_pages": 3}
        )
        assert [block.type for block in result.content] == ["text", "image", "text", "image", "text", "image", "text"]

    def test_the_no_text_layer_note_points_at_as_images(self, scans):
        """as_text on a scan has to name the tool that can actually read it."""
        _cache("document_20260904_100000_PDF3.pdf", make_pdf(["", "", ""]))
        blocks = media_read.read_media(ALICE, "PDF3", as_text=True)
        note = "\n".join(block.text for block in blocks[:-1])
        assert "as_images=true" in note
