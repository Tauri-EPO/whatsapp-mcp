"""Scanned PDF pages as pictures, for ``read_media(as_images=True)``.

``as_text`` (media_text.py) reads a PDF's text layer, and says so plainly when
there is none: a scan is photographed paper, and there is no OCR on this server.
That answer is honest and useless — the clinical reports, the receipts and the
signed contracts that motivated issue #285 are exactly the documents somebody
photographed, and an agent with a vision model behind it can *read* a picture of
a page. It just never got one.

So ``as_images`` renders the pages with PDFium (pypdfium2) and returns them as
``ImageContent`` blocks, one per page, each preceded by a marker naming the page
so the model knows what it is looking at. ``first_page`` walks a document longer
than one answer can hold: pages 1-5, then 6-10, and the marker keeps saying which
page of how many each picture is.

**Prefer ``as_text`` when the PDF has a text layer.** A page of text costs a few
kilobytes as text and a few hundred as a picture, the words are exact rather
than read off pixels, and 40 pages fit in one answer. ``as_images`` is for the
document ``as_text`` cannot read, and the two are mutually exclusive so the
choice is deliberate.

Everything is bounded, because anyone who can message the account chooses the
file: :data:`MAX_PAGES_LIMIT` pages per call, :data:`MAX_EDGE` pixels on a
page's long edge whatever ``max_edge`` asks for, and
:data:`MAX_TOTAL_BYTES` of rendered image across the whole answer. Hitting any
of them sets ``truncated``.

**One render at a time.** PDFium is not thread-safe — pypdfium2 says so in as
many words — and the MCP SDK runs a sync tool on a worker thread, so two agents
calling ``read_media(as_images=True)`` at once would drive it concurrently: not
a ``ToolError`` but corruption or a segfault taking the whole server process
with it, on bytes a stranger chose. :data:`_pdfium_lock` serialises the open,
the render and the close. It also bounds the peak memory, which is one page
bitmap (tens of MB at 4096 px) rather than one per worker thread.
"""

from __future__ import annotations

import threading
from typing import Any, NamedTuple

import media_image
import media_text
from errors import ToolError

# A scan is heavy: five pages of JPEG at 1568 px is already ~1.5 MB of base64,
# where five pages of text are a few kilobytes. So the default is small and the
# agent raises it once it knows the document is worth it.
DEFAULT_PAGES = 5
MAX_PAGES_LIMIT = 20

# PDFium renders at 72 dpi times ``scale``, so the scale comes from the page's
# own size in points. This ceiling is on the *rendered* long edge: a page beyond
# it carries no more detail a model can use, and an A0 poster asked for at 8192
# would be half a gigapixel.
MAX_EDGE = 4096

# The whole answer, across every page. Twenty pages that each happen to be a
# dense colour scan would otherwise be tens of megabytes of base64 in one
# result — the failure this half of #368 exists to avoid, arriving page by page.
MAX_TOTAL_BYTES = 8 * 1024 * 1024

# PDFium is not thread-safe and the MCP SDK calls a sync tool on a worker
# thread; see the module docstring. Held across the whole open/render/close.
_pdfium_lock = threading.Lock()


class RenderedPage(NamedTuple):
    """One rendered page, carrying the number it has in the document.

    The number is the source page, not the position in ``pages``: a page the
    renderer had to skip must not shift the ones after it, or the marker names
    the wrong page and every quotation from it is wrong.
    """

    number: int
    image: media_image.Rendered


class RenderedPages(NamedTuple):
    """The pages that were rendered, and what was left out.

    ``next_page`` is the first page this call did not attempt — what
    ``first_page`` should be on the next one — and equals ``pages_total + 1``
    when there is nothing after. ``truncated`` covers both kinds of gap: pages
    left for a later call, and pages the renderer could not draw (``failed``).
    """

    pages: list[RenderedPage]
    pages_total: int
    truncated: bool
    first_page: int = 1
    next_page: int = 1
    failed: tuple[int, ...] = ()


def page_limit(max_pages: Any) -> int:
    """``max_pages`` as a positive count; 0 or unset means :data:`DEFAULT_PAGES`."""
    try:
        value = int(max_pages or 0)
    except (TypeError, ValueError) as exc:
        raise ToolError("invalid_argument", f"max_pages must be an integer, got {max_pages!r}") from exc
    if value <= 0:
        value = DEFAULT_PAGES
    return min(value, MAX_PAGES_LIMIT)


def first_page(value: Any) -> int:
    """``first_page`` as a 1-based page number; 0 or unset means the first page."""
    try:
        number = int(value or 0)
    except (TypeError, ValueError) as exc:
        raise ToolError("invalid_argument", f"first_page must be an integer, got {value!r}") from exc
    if number < 0:
        raise ToolError("invalid_argument", f"first_page counts from 1 (got {number})")
    return max(number, 1)


def require_renderable(mime: str) -> None:
    """Refuse ``as_images`` for anything that is not a PDF, before a file is opened."""
    if mime != media_text.PDF_MIME:
        raise ToolError(
            "invalid_argument",
            f"as_images renders the pages of a PDF; {mime} has none. "
            "An image already comes back as an image, a DOCX or XLSX reads with as_text, "
            "and a voice note with transcribe_audio",
        )


def _scale(width_pt: float, height_pt: float, max_edge: int) -> float:
    """PDFium's render scale: 1.0 is 72 dpi, so the target edge decides it."""
    longest = max(width_pt, height_pt)
    if longest <= 0:
        raise ToolError("invalid_argument", "this PDF declares a page with no size")
    return min(max_edge, MAX_EDGE) / longest


def render_pages(path: str, first: int, wanted: int, max_edge: int, quality: int) -> RenderedPages:
    """``wanted`` pages of the PDF at ``path`` from page ``first``, as encoded image bytes.

    A file PDFium will not open — a broken download, a password-protected
    document — is ``invalid_argument`` naming what happened, not a crash: it is
    bad input, and the agent can act on "this PDF is encrypted". So is a
    ``first`` past the last page, because an empty answer would read like an
    empty document.

    A single page PDFium cannot draw is *skipped*, not fatal: the pages around
    it are already rendered and worth returning, and the ones that failed come
    back in ``failed`` so nobody mistakes the gap for a page that was not there.
    """
    import pypdfium2

    pages: list[RenderedPage] = []
    failed: list[int] = []
    used = 0
    # PDFium is not thread-safe (module docstring): the whole open/render/close
    # is one critical section, not three.
    with _pdfium_lock:
        try:
            document = pypdfium2.PdfDocument(path)
        except pypdfium2.PdfiumError as exc:
            raise ToolError("invalid_argument", f"this PDF could not be opened: {exc}") from exc
        except Exception as exc:  # noqa: BLE001 - a malformed file is bad input, not a server fault
            raise ToolError("invalid_argument", f"this PDF could not be read: {type(exc).__name__}: {exc}") from exc
        try:
            total = len(document)
            if first > total:
                raise ToolError(
                    "invalid_argument",
                    f"this PDF has {total} page(s); first_page={first} is past the end",
                )
            stop = min(total, first - 1 + wanted)
            for index in range(first - 1, stop):
                try:
                    page = document[index]
                    width_pt, height_pt = page.get_size()
                    # pypdfium2 annotates `scale` as int; it multiplies the
                    # page's size in points by it, and a fractional scale is the
                    # only way to hit a target edge (1568/842 renders 1109x1568).
                    scale = _scale(width_pt, height_pt, max_edge)
                    image = page.render(scale=scale).to_pil()  # pyright: ignore[reportArgumentType]
                    data, mime = media_image.encode(image, quality)
                except Exception:  # noqa: BLE001 - one bad page must not lose the good ones
                    failed.append(index + 1)
                    continue
                if used + len(data) > MAX_TOTAL_BYTES and pages:
                    # Never return zero pages over the budget: one page is what
                    # makes the answer worth anything, and the caller is told
                    # the rest was cut.
                    stop = index
                    break
                used += len(data)
                pages.append(RenderedPage(index + 1, media_image.Rendered(data, mime, image.width, image.height, True)))
        finally:
            document.close()
    if not pages and failed:
        # Every page we were allowed to draw failed: that is a broken document,
        # and an answer with no picture in it is not one.
        raise ToolError(
            "invalid_argument",
            f"none of the {len(failed)} page(s) asked for could be rendered; this PDF is damaged",
        )
    return RenderedPages(pages, total, stop < total or bool(failed), first, stop + 1, tuple(failed))
