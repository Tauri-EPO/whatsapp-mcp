"""The bytes of one media message, in the answer instead of on the server's disk.

``download_media`` returns a **path**. That is the right answer for a client
running on the same filesystem as the bridge, and useless for the deployment
this fork is built for: an agent on another machine, reaching the MCP endpoint
over Tailscale, gets ``/app/store/5511…@s.whatsapp.net/image_20260904_100000_3EB0.jpg``
and has nothing to open it with (issue #285). It could see that a photo exists,
read its size and its notes, and never look at it.

``read_media`` closes that gap by returning MCP **content blocks**:

* an image as ``ImageContent`` — the model sees the picture. The file may be up
  to 16 MiB, but what travels is a copy downscaled to ``max_edge`` and
  re-encoded (media_image.py), because a vision model resamples to ~1568 px
  before it looks at anything and most clients refuse a block above ~5 MB;
  ``max_edge=0`` sends the stored bytes instead;
* a small text-ish file (``text/*``, JSON, CSV) as text, capped at 1 MiB;
* a voice note as ``AudioContent``, the block the protocol has for audio;
* anything else — a PDF, a DOCX, a video, an archive — as an
  ``EmbeddedResource`` whose ``BlobResourceContents`` carries the file's real
  MIME type and a ``whatsapp://media/<chat_jid>/<message_id>`` URI, capped at
  2 MiB. That is what a client needs to hand a PDF to the model as a document
  (issue #367); the ``base64:<mime>:<bytes>`` text block it replaces was one
  every client had to decode itself, and none did;
* with ``as_text=True``, a PDF, DOCX or XLSX **read on this side** and returned
  as text (media_text.py), which is what makes a 5 MiB clinical PDF affordable;
* with ``as_images=True``, a PDF **rendered** here, one ``ImageContent`` per page
  behind a page marker (media_pdf.py) — the answer for a scan, which has no text
  layer for ``as_text`` to find and which a vision model can simply read.

Every answer ends with one JSON text block carrying ``sha256``, ``mime``,
``bytes`` and the ``notes`` already recorded for the file, so the loop
``list_media(has_notes=false)`` → ``read_media`` → ``annotate_media`` needs no
second call to learn the hash.

A file over the cap is refused with ``too_large`` naming the real size: an
answer the client cannot hold is worse than an error, and ``download_media``
is still there for a client that does share the filesystem.
"""

from __future__ import annotations

import base64
import functools
import json
import mimetypes
import os
import sqlite3
from collections.abc import Callable
from typing import Any, NamedTuple
from urllib.parse import quote

from mcp_types import (
    AudioContent,
    BlobResourceContents,
    CallToolResult,
    ContentBlock,
    EmbeddedResource,
    ImageContent,
    ResourceLink,
    TextContent,
)

import media_image
import media_inventory
import media_pdf
import media_text
import whatsapp
from errors import ToolError
from tool_policy import download_denied, offers_download
from untrusted import clean_untrusted, wrap_enabled, wrap_text

# Hard limits, and the default value of ``max_bytes``. An ImageContent is
# base64 in JSON, so 16 MiB of JPEG is ~21 MiB on the wire and a good deal more
# in the model's context; 2 MiB is what a generic blob is worth when the client
# can only hand it back to a tool.
MAX_IMAGE_BYTES = 16 * 1024 * 1024
MAX_BASE64_BYTES = 2 * 1024 * 1024
# Text is returned decoded, so the cap is the context it costs, not the wire.
MAX_TEXT_BYTES = 1024 * 1024

# Text-ish types returned as text rather than base64. ``text/*`` covers plain,
# csv, html and markdown; the rest are the structured formats that arrive as
# documents and read as text — an SVG among them, which is markup a model can
# read and not something a client can render as an image block.
TEXT_MIMES = frozenset(
    {
        "application/json",
        "application/xml",
        "application/csv",
        "application/x-ndjson",
        "application/x-yaml",
        "image/svg+xml",
    }
)

# The image types an MCP client can actually display: the ones that may travel
# as they are. A block a client refuses to render fails the whole call.
RENDERABLE_IMAGE_MIMES = frozenset({"image/jpeg", "image/png", "image/gif", "image/webp"})

# Every type read_media treats as a picture: the four above plus the ones
# media_image converts to JPEG/PNG on the way out (TIFF, BMP, HEIC). The
# converted ones only earn the image size cap on the path that converts them
# (``hard_limit``); everywhere else — ``max_edge=0``, ``resources/read`` — they
# are still bytes in a resource and keep the blob cap they had.
IMAGE_MIMES = RENDERABLE_IMAGE_MIMES | media_image.CONVERTIBLE_IMAGE_MIMES

# The audio types that go out as ``AudioContent``. WhatsApp voice notes are all
# Opus in an Ogg container; the other three are what a forwarded audio file
# turns out to be. Anything else (FLAC, AMR, a container we do not recognise)
# takes the resource path with its real type, which is the honest answer for a
# payload no client is going to play.
#
# ``transcribe_audio`` remains the tool for a voice note the *model* has to
# understand: no model reads Opus, and a transcript is text, cached and
# searchable. This block is for the client and the human behind it.
PLAYABLE_AUDIO_MIMES = frozenset({"audio/ogg", "audio/mpeg", "audio/mp4", "audio/wav"})

# Scheme of the URI that names one message's media. It is the identity the
# EmbeddedResource below carries, so a client can tell two attachments apart
# and match a block it already holds to the row it came from.
MEDIA_URI_PREFIX = "whatsapp://media/"

# The bridge names every cached image `.jpg` whatever it actually is
# (whatsapp-bridge/media.go) and `messages` has no mime column, so the
# extension is not evidence: the first bytes are. A declared type that
# disagrees with the payload is rejected by strict clients.
IMAGE_SIGNATURES: tuple[tuple[bytes, str], ...] = (
    (b"\x89PNG\r\n\x1a\n", "image/png"),
    (b"\xff\xd8\xff", "image/jpeg"),
    (b"GIF87a", "image/gif"),
    (b"GIF89a", "image/gif"),
    (b"II*\x00", "image/tiff"),
    (b"MM\x00*", "image/tiff"),
)

# HEIC/HEIF is ISO base media like MP4 — `....ftyp<brand>` — so only the brand
# tells a photo from a video. The `he*` brands are what an iPhone writes; the
# two generic ones are what a converter writes.
HEIF_BRANDS = {
    b"heic": "image/heic",
    b"heix": "image/heic",
    b"hevc": "image/heic",
    b"hevx": "image/heic",
    b"heim": "image/heif",
    b"heis": "image/heif",
    b"mif1": "image/heif",
    b"msf1": "image/heif",
}

# Same problem, worse: the bridge names *every* audioMessage `.ogg`
# (whatsapp-bridge/content.go), so an MP3 a contact attached from WhatsApp's
# audio picker arrives with an `.ogg` name and no mime column to contradict it.
# Declaring that AudioContent as audio/ogg would be a block whose data
# disagrees with its type, which is the one thing a strict client refuses
# outright. The first bytes decide here too.
AUDIO_SIGNATURES: tuple[tuple[bytes, str], ...] = (
    (b"OggS", "audio/ogg"),
    (b"ID3", "audio/mpeg"),
    (b"fLaC", "audio/flac"),
    (b"#!AMR", "audio/amr"),
)

# The extensions read_media branches on, resolved here rather than by
# mimetypes. Its table is the platform's: python:3.13-slim has no
# /etc/mime.types, so `.docx`, `.xlsx` and `.ogg` all come back as None there
# (as_text would then refuse the very documents it exists for), while Windows
# answers from the registry and maps `.csv` to application/vnd.ms-excel. What a
# WhatsApp attachment is must not depend on which of those is running.
EXTENSION_MIME = {
    ".pdf": media_text.PDF_MIME,
    ".docx": media_text.DOCX_MIME,
    ".xlsx": media_text.XLSX_MIME,
    ".txt": "text/plain",
    ".md": "text/markdown",
    ".csv": "text/csv",
    ".tsv": "text/tab-separated-values",
    ".json": "application/json",
    ".xml": "application/xml",
    ".jpg": "image/jpeg",
    ".jpeg": "image/jpeg",
    ".png": "image/png",
    ".gif": "image/gif",
    ".webp": "image/webp",
    ".svg": "image/svg+xml",
    ".tif": "image/tiff",
    ".tiff": "image/tiff",
    ".bmp": "image/bmp",
    ".heic": "image/heic",
    ".heif": "image/heif",
    ".ogg": "audio/ogg",
    ".opus": "audio/ogg",
    ".mp3": "audio/mpeg",
    ".m4a": "audio/mp4",
    # CPython's built-in table says audio/x-wav and Windows says audio/wav.
    ".wav": "audio/wav",
    ".mp4": "video/mp4",
    ".zip": "application/zip",
}

# A name like `notes.txt.gz` guesses as text/plain *with an encoding*: the bytes
# on disk are an archive, and decoding them as UTF-8 would hand the model
# mojibake presented as the document.
ENCODING_MIME = {
    "gzip": "application/gzip",
    "bzip2": "application/x-bzip2",
    "xz": "application/x-xz",
    "compress": "application/x-compress",
    "br": "application/x-brotli",
}

# What a media_type is when the filename says nothing (a document with no
# extension, an image cached before extensions were kept).
TYPE_FALLBACK_MIME = {
    "image": "image/jpeg",
    "sticker": "image/webp",
    "video": "video/mp4",
    "audio": "audio/ogg",
    "document": "application/octet-stream",
}


def media_uri(chat_jid: str, message_id: str) -> str:
    """``whatsapp://media/<chat_jid>/<message_id>`` — the URI of one message's media.

    Both halves are percent-encoded, so the URI is exactly two segments under
    the prefix and splitting it back into a JID and an id needs no knowledge of
    what either may contain (a slash in a message id, a device suffix in a
    JID). ``@`` is left readable: it is legal in a path segment, and the JID is
    the half of the URI a human recognises.
    """
    return f"{MEDIA_URI_PREFIX}{quote(chat_jid, safe='@')}/{quote(message_id, safe='')}"


def resource_link(chat_jid: str, message_id: str, name: str, mime: str, size: int | None) -> dict[str, Any]:
    """The link a client follows to fetch these bytes when *it* decides to.

    The wire form of ``mcp_types.ResourceLink`` (``type``, ``uri``, ``name``,
    ``mimeType``, ``size``) rather than the model itself, because it travels
    inside JSON — a ``list_media`` row, the metadata block of ``read_media`` —
    and not as a content block of its own. Dumped from the model so the keys
    stay the protocol's if the SDK renames one.

    ``name`` is a *display* label, which is why it is spelled ``name`` and not
    ``filename``: ``@untrusted_content`` sanitises it (invisible characters
    out, length capped) while the row's own ``filename`` is deliberately left
    byte-for-byte, because that one is matched against the file on disk. The
    two can therefore differ for a sender who put a bidi override in a
    filename, which is the direction to differ in.
    """
    return ResourceLink(
        type="resource_link",
        uri=media_uri(chat_jid, message_id),
        name=name,
        mime_type=mime,
        size=size,
    ).model_dump(by_alias=True, exclude_none=True, mode="json")


def _media_row(chat_jid: str, message_id: str) -> tuple[str, str | None, int | None, str | None]:
    """``(media_type, filename, file_length, sha256)`` for one message row, or an error.

    Reading is addressed by chat, so the chat allow-list is the whole
    visibility check here: ``media_notes`` needs ``visible_hashes`` because it
    is addressed by content hash and must not confirm that a file exists in a
    chat the agent may not see.
    """
    try:
        conn = whatsapp._connect_messages_db()
        try:
            row = conn.execute(
                "SELECT media_type, filename, file_length, lower(hex(file_sha256)) "
                "FROM messages WHERE id = ? AND chat_jid = ?",
                (message_id, chat_jid),
            ).fetchone()
        finally:
            conn.close()
    except sqlite3.Error as exc:
        raise ToolError("internal", f"database error: {exc}") from exc
    if row is None:
        raise ToolError("not_found", f"no message {message_id} in {chat_jid}")
    media_type = (row[0] or "").strip()
    if media_type not in media_inventory.MEDIA_TYPES:
        raise ToolError(
            "invalid_argument",
            f"message {message_id} carries no media (media_type={media_type or 'text'}); "
            f"read_media reads {', '.join(media_inventory.MEDIA_TYPES)} rows",
        )
    # SQLite's hex(NULL) is '', not NULL: a row without a content hash has to be
    # reported as null, the way download_media and list_media report it.
    return media_type, row[1], int(row[2]) if row[2] else None, row[3] or None


def _in_chat_dir(chat_jid: str, path: str) -> str:
    """``path`` resolved and proven to be in this chat's media directory.

    ``read_media`` never takes a path from the caller, but the one it works
    with comes from another process (the bridge's ``/api/download`` answer) or
    from a directory listing. The check is the chat's own directory rather than
    the store root because the root also holds ``.bridge-token``, the two
    databases and the exports: a symlink planted in the cache must not turn
    "read this message's media" into "read the bridge's bearer token".
    """
    real = os.path.realpath(path)
    root = os.path.realpath(media_inventory.chat_media_dir(chat_jid))
    if not real.startswith(root + os.sep):
        raise ToolError("denied", "the media file resolves outside this chat's directory in the media store")
    return real


def cached_path(chat_jid: str, message_id: str) -> str | None:
    """The message's file on disk, or None when nothing is cached for it.

    Looked up by message id (issue #318): reading one file must not stat every
    other file in the chat, and the answer is the directory as it is now, not a
    memoised scan — the bytes are opened right after this.
    """
    name = media_inventory.lookup_cached_name(chat_jid, message_id)
    if name is None:
        return None
    return _in_chat_dir(chat_jid, os.path.join(media_inventory.chat_media_dir(chat_jid), name))


def download_path(chat_jid: str, message_id: str) -> str:
    """Fetch the bytes through the bridge and return where they landed.

    The fetch is ``download_media`` under another name, so a policy that does
    not offer that tool refuses here too (issue #350). Only a message with
    nothing cached reaches this function, so the refusal never touches media
    the store already holds.
    """
    if not offers_download():
        raise download_denied("read_media")
    path = whatsapp.download_media(message_id, chat_jid)
    if not path:
        raise ToolError("internal", "the bridge reported success without a file path")
    return _in_chat_dir(chat_jid, path)


def cached_only_path(chat_jid: str, message_id: str, caller: str) -> str:
    """The cached file of one message, for a caller that may not fetch it.

    ``transcribe_audio`` lets the bridge answer from its own cache, so with
    ``download_media`` off the list it needs this local lookup instead (issue
    #350). The chat allow-list still applies and the file still has to resolve
    inside the chat's directory; a message whose bytes are not here is refused
    with ``denied`` naming ``download_media``, never fetched.

    The row is read first, so a mistyped id or a text message answers
    ``not_found`` / ``invalid_argument`` as it does when fetching is allowed:
    "the policy forbids this" would send the agent away from a mistake it could
    have corrected.
    """
    whatsapp._require_allowed(chat_jid)
    _media_row(chat_jid, message_id)
    path = cached_path(chat_jid, message_id)
    if path is None:
        raise download_denied(caller)
    return path


def guess_mime(media_type: str, filename: str | None, path: str = "") -> str:
    """MIME type from the cached name, then the sender's name, then the media type."""
    for candidate in (path, filename or ""):
        if not candidate:
            continue
        extension = os.path.splitext(candidate)[1].lower()
        if extension in EXTENSION_MIME:
            return EXTENSION_MIME[extension]
        guessed, encoding = mimetypes.guess_type(candidate)
        if encoding:
            return ENCODING_MIME.get(encoding, "application/octet-stream")
        if guessed:
            return guessed
    return TYPE_FALLBACK_MIME.get(media_type, "application/octet-stream")


def sniff_image_mime(path: str) -> str | None:
    """The image type the first bytes say it is, or None for anything else."""
    try:
        with open(path, "rb") as handle:
            head = handle.read(16)
    except OSError:
        return None
    for signature, mime in IMAGE_SIGNATURES:
        if head.startswith(signature):
            return mime
    if head.startswith(b"RIFF") and head[8:12] == b"WEBP":
        return "image/webp"
    if head[4:8] == b"ftyp":
        return HEIF_BRANDS.get(head[8:12])
    if head.startswith(b"BM") and len(head) >= 6:
        # "BM" is two bytes, far too weak for a table whose job is to catch
        # payloads that do not match their name — and a false positive here is
        # not a mislabelled block but a failed call, since an image type goes
        # to a decoder. The header's own file-size field has to agree with the
        # file on disk before this counts as a BMP.
        try:
            if int.from_bytes(head[2:6], "little") == os.path.getsize(path):
                return "image/bmp"
        except OSError:
            return None
    return None


def sniff_audio_mime(path: str) -> str | None:
    """The audio type the first bytes say it is, or None for anything else."""
    try:
        with open(path, "rb") as handle:
            head = handle.read(16)
    except OSError:
        return None
    for signature, mime in AUDIO_SIGNATURES:
        if head.startswith(signature):
            return mime
    if head.startswith(b"RIFF") and head[8:12] == b"WAVE":
        return "audio/wav"
    # ISO base media: `....ftyp<brand>`, which is M4A here and also how an
    # MP4 video starts — the caller only asks about a file the name calls audio.
    if head[4:8] == b"ftyp":
        return "audio/mp4"
    # An MP3 without an ID3 tag starts straight at a frame header: 11 set bits.
    if len(head) > 1 and head[0] == 0xFF and head[1] & 0xE0 == 0xE0:
        return "audio/mpeg"
    return None


def declared_mime(media_type: str, filename: str | None, path: str = "") -> str:
    """The type this file goes out as: the name's guess, corrected by the first bytes.

    One answer for every caller, so a ``list_media`` link, the block
    ``read_media`` returns and ``resources/read`` on the same URI cannot
    disagree about what the file is — the mislabelling the sniffing exists to
    prevent, arriving through a different door.

    Only images and audio are sniffed, because those are the two the bridge
    renames (`.jpg` and `.ogg` for everything of that kind). Without ``path``
    — a row whose bytes are not cached — the name is all there is.
    """
    mime = guess_mime(media_type, filename, path)
    if not path:
        return mime
    # A name that promises a type the payload does not have must not produce a
    # typed block: the client rejects it, and a rejected block fails the whole
    # call. An unrecognised payload behind such a name is untyped instead — and
    # that is also what keeps a decoder from ever being pointed at it, since
    # every image type media_image opens got there by matching a signature.
    if mime.startswith("image/"):
        return sniff_image_mime(path) or ("application/octet-stream" if mime in IMAGE_MIMES else mime)
    if mime.startswith("audio/"):
        return sniff_audio_mime(path) or ("application/octet-stream" if mime in PLAYABLE_AUDIO_MIMES else mime)
    return mime


def is_text_mime(mime: str) -> bool:
    return mime.startswith("text/") or mime in TEXT_MIMES


def hard_limit(mime: str, as_text: bool = False, max_edge: int = 0, as_images: bool = False) -> int:
    """The ceiling for this type — and, with the branches below, what decides the block.

    Extraction has its own, much higher ceiling: what bounds ``as_text`` is the
    text it produces (media_text.MAX_TEXT_CHARS), not the size of the file it
    reads, which is the whole reason to extract instead of returning base64.

    ``max_edge`` matters for the same reason in the other direction. A big JPEG
    is affordable because what travels is a downscaled copy; a TIFF is only
    affordable on the path that makes one of it. Without ``max_edge`` — a
    ``max_edge=0`` call, a ``resources/read``, a ``list_media`` link — those
    types are what they were before #368: bytes in a resource, capped like any
    other blob, rather than 12 MB of base64 in a block no client renders.
    """
    if (as_text and mime in media_text.EXTRACTABLE_MIMES) or (as_images and mime == media_text.PDF_MIME):
        # Rendering is bounded by the pages it draws, not by the file it reads:
        # a 30 MiB scan is five JPEGs either way.
        return media_text.MAX_EXTRACT_BYTES
    if mime in RENDERABLE_IMAGE_MIMES or (max_edge and mime in media_image.CONVERTIBLE_IMAGE_MIMES):
        return MAX_IMAGE_BYTES
    if is_text_mime(mime):
        return MAX_TEXT_BYTES
    return MAX_BASE64_BYTES


def cap(requested: int | None, hard: int) -> int:
    """``max_bytes`` clamped to the hard limit; 0 or unset means the limit itself."""
    try:
        value = int(requested or 0)
    except (TypeError, ValueError) as exc:
        raise ToolError("invalid_argument", f"max_bytes must be an integer, got {requested!r}") from exc
    if value <= 0:
        return hard
    return min(value, hard)


def check_size(size: int, limit: int, mime: str, caller: str = "read_media") -> None:
    if size <= limit:
        return
    raise ToolError(
        "too_large",
        f"the file is {size} bytes and {caller} returns at most {limit} for {mime}. "
        "download_media still returns the server-side path, which only helps a client sharing this filesystem",
        bytes=size,
        limit=limit,
    )


def read_capped(path: str, size: int) -> bytes:
    """Read the file, at most the size the cap was checked against."""
    try:
        with open(path, "rb") as handle:
            return handle.read(size)
    except OSError as exc:
        raise ToolError("internal", f"could not read the cached file: {exc}") from exc


def text_block(text: str) -> TextContent:
    """A text block holding somebody else's words, delimited when the envelope is on."""
    return TextContent(type="text", text=wrap_text(text) if wrap_enabled() else text)


def meta_block(sha256: str | None, mime: str, size: int, **extra: Any) -> TextContent:
    """The trailing block: what the file is, and what the agent already knows about it.

    ``mime`` and ``bytes`` describe what came back, so a branch that re-encoded
    the file (the image one) overrides them through ``extra`` and adds
    ``original_mime`` / ``original_bytes`` for the file on disk. ``sha256`` is
    always the stored file's, because that is what ``annotate_media`` keys on.
    """
    notes: dict[str, str] = {}
    if sha256:
        from media_notes import fetch_notes

        notes = fetch_notes([sha256]).get(sha256, {})
    # `truncated` is always false while the only ways out are the whole file or
    # `too_large`; the key is part of the shape the as_text extraction fills in.
    payload: dict[str, Any] = {"sha256": sha256, "mime": mime, "bytes": size, "truncated": False, **extra}
    payload["notes"] = notes
    return TextContent(
        type="text",
        text=json.dumps(clean_untrusted(payload, wrap=wrap_enabled()), ensure_ascii=False),
    )


def content_tool(fn: Callable[..., Any]) -> Callable[..., Any]:
    """Make a block-returning tool report a failure on both channels.

    A tool whose return annotation is content blocks publishes no output schema
    (``func_metadata`` stops at ``_returns_content``), so the SDK has nowhere to
    put the envelope ``@tool_errors`` builds: a client reading
    ``structured_content`` and ignoring the text — the case
    ``strict_args._refusal`` is written for — would see a failure as an empty
    result. The envelope becomes the ``CallToolResult`` the SDK would otherwise
    have built, with ``is_error`` set, which a bare list of blocks cannot carry
    either. Goes *above* ``@tool_errors``::

        @mcp.tool()
        @content_tool
        @tool_errors
        @untrusted_content
        def read_media(...): ...
    """

    @functools.wraps(fn)
    def wrapper(*args: Any, **kwargs: Any) -> Any:
        result = fn(*args, **kwargs)
        if isinstance(result, dict) and "error" in result:
            return CallToolResult(
                content=[TextContent(type="text", text=json.dumps(result, ensure_ascii=False))],
                structured_content=result,
                is_error=True,
            )
        return result

    return wrapper


def _extracted_blocks(path: str, mime: str, max_pages: int) -> tuple[list[ContentBlock], dict[str, Any]]:
    """The document's text, one block per page/table/sheet, and what to report about it."""
    result = media_text.extract(path, mime, max_pages)
    blocks: list[ContentBlock] = [text_block(section) for section in result.sections]
    if result.note:
        # Written here, not by whoever sent the file: it stays outside the
        # untrusted envelope, and an empty answer never reads like an empty
        # document.
        blocks.append(TextContent(type="text", text=result.note))
    return blocks, {"pages_total": result.units_total, "truncated": result.truncated}


class ResolvedMedia(NamedTuple):
    """What :func:`resolve_media` proved about one message's media."""

    path: str
    mime: str
    size: int
    sha256: str | None
    filename: str | None


def resolve_media(
    chat_jid: str,
    message_id: str,
    caller: str = "read_media",
    max_bytes: int = 0,
    as_text: bool = False,
    max_edge: int = 0,
    as_images: bool = False,
) -> ResolvedMedia:
    """The file behind one message, proven readable, or the refusal.

    Every gate a read passes, in the order it has to pass them, so a resource
    read (media_resource.py) is the same read as ``read_media``: the chat
    allow-list, the row, the implicit-download policy of issue #350, the proof
    that the path resolves inside this chat's directory (inside
    ``cached_path`` / ``download_path``), and the cap for the resolved type.
    ``caller`` only names the tool in the refusal an agent reads; ``as_text``,
    ``as_images`` and ``max_edge`` only tell ``hard_limit`` which ceiling
    applies, because a file read as text, as rendered pages or as a downscaled
    picture is not the size it costs.
    """
    whatsapp._require_allowed(chat_jid)
    media_type, filename, reported, sha256 = _media_row(chat_jid, message_id)
    path = cached_path(chat_jid, message_id)
    if path is None:
        # Nothing is cached, so reading means a CDN transfer. Whether it may
        # happen at all comes first: "too_large, and download_media returns a
        # path instead" would recommend a tool this very policy took away.
        if not offers_download():
            raise download_denied(caller)
        # Refuse a file the row already says is too big instead of paying for
        # it first. The authority is still the size on disk below; this only
        # skips the obvious no.
        if reported:
            expected = guess_mime(media_type, filename)
            check_size(reported, cap(max_bytes, hard_limit(expected, as_text, max_edge, as_images)), expected, caller)
        path = download_path(chat_jid, message_id)

    mime = declared_mime(media_type, filename, path)
    if as_images:
        # Before the size check: "as_images does not apply to a video" is the
        # useful answer, not "that video is over the 2 MiB cap".
        media_pdf.require_renderable(mime)
    elif as_text and not is_text_mime(mime):
        media_text.require_extractable(mime)
    try:
        size = os.path.getsize(path)
    except OSError as exc:
        raise ToolError("internal", f"could not stat the cached file: {exc}") from exc
    check_size(size, cap(max_bytes, hard_limit(mime, as_text, max_edge, as_images)), mime, caller)
    return ResolvedMedia(path, mime, size, sha256, filename)


def _image_blocks(found: ResolvedMedia, max_edge: int, quality: int) -> tuple[list[ContentBlock], dict[str, Any]]:
    """The picture, sized for a model, and what the metadata block should say about it.

    The overrides matter: ``mime`` and ``bytes`` describe the payload that
    actually travelled (JPEG, downscaled), while ``original_mime`` and
    ``original_bytes`` describe the file in the store — what ``sha256``
    identifies, what ``resource_link`` points at and what ``resources/read``
    serves, none of which this re-encoding touches.
    """
    rendered = media_image.render(
        found.path,
        found.mime,
        max_edge,
        quality,
        # The stored bytes may travel only when a client renders that type and
        # the file is small enough that re-encoding it would not pay for itself.
        passthrough=found.mime in RENDERABLE_IMAGE_MIMES and found.size <= media_image.PASSTHROUGH_MAX_BYTES,
    )
    data = read_capped(found.path, found.size) if rendered.data is None else rendered.data
    blocks: list[ContentBlock] = [
        ImageContent(type="image", data=base64.b64encode(data).decode("ascii"), mime_type=rendered.mime)
    ]
    extra: dict[str, Any] = {
        "mime": rendered.mime,
        "bytes": len(data),
        "original_bytes": found.size,
        "original_mime": found.mime,
        "width": rendered.width,
        "height": rendered.height,
        "resized": rendered.resized,
    }
    if rendered.frames > 1:
        # An animation flattened to frame 0. Said out loud, because a still
        # frame reads exactly like a still image and the agent would never
        # think to ask for the rest with max_edge=0.
        extra["original_frames"] = rendered.frames
    return blocks, extra


def _page_image_blocks(
    found: ResolvedMedia, first: int, max_pages: int, max_edge: int, quality: int
) -> tuple[list[ContentBlock], dict[str, Any]]:
    """A scanned PDF as one picture per page, each behind the marker that names it.

    The marker is its own text block because an image block carries no caption:
    without it a model handed five pictures cannot say which page it is quoting,
    nor that pages 6 to 40 are still waiting behind ``first_page=6``.
    """
    result = media_pdf.render_pages(found.path, first, media_pdf.page_limit(max_pages), max_edge, quality)
    blocks: list[ContentBlock] = []
    for page in result.pages:
        # Written here, not by whoever sent the file, so these stay outside the
        # untrusted envelope.
        marker = f"--- page {page.number} of {result.pages_total} (rendered image) ---"
        blocks.append(TextContent(type="text", text=marker))
        assert page.image.data is not None  # render_pages always encodes
        blocks.append(
            ImageContent(
                type="image", data=base64.b64encode(page.image.data).decode("ascii"), mime_type=page.image.mime
            )
        )
    if result.next_page <= result.pages_total:
        # The one line that makes the rest of the document reachable: a model
        # holding five pictures has no other way to learn the argument.
        blocks.append(
            TextContent(
                type="text",
                text=(
                    f"[{result.pages_total} pages in this PDF; call "
                    f"read_media(as_images=true, first_page={result.next_page}) for the next ones.]"
                ),
            )
        )
    extra: dict[str, Any] = {
        "pages_total": result.pages_total,
        "first_page": result.first_page,
        "next_page": result.next_page,
        "pages_rendered": len(result.pages),
        "truncated": result.truncated,
        "image_bytes": sum(len(page.image.data or b"") for page in result.pages),
    }
    if result.failed:
        # A page PDFium could not draw is a gap in what the model sees; unnamed,
        # it reads as a page that was never there.
        extra["pages_failed"] = list(result.failed)
    return blocks, extra


def read_media(
    chat_jid: str,
    message_id: str,
    max_bytes: int = 0,
    as_text: bool = False,
    max_pages: int = 0,
    max_edge: int = media_image.DEFAULT_MAX_EDGE,
    quality: int = media_image.DEFAULT_QUALITY,
    as_images: bool = False,
    first_page: int = 1,
) -> list[ContentBlock]:
    """The media of one message as content blocks. See the module docstring."""
    if as_text and as_images:
        raise ToolError(
            "invalid_argument",
            "as_text and as_images are two ways to read the same document and cannot be combined: "
            "as_text reads the text layer (cheap, exact, many pages), as_images renders the pages as "
            "pictures for a scan that has no text layer. Pick one",
        )
    # Argument validation before any I/O: "max_edge cannot be negative" must not
    # arrive after a CDN transfer paid for it.
    edge = media_image.edge_limit(max_edge)
    encode_quality = media_image.quality_limit(quality)
    page_one = media_pdf.first_page(first_page) if as_images else 1
    if as_images:
        media_pdf.page_limit(max_pages)
    found = resolve_media(
        chat_jid, message_id, max_bytes=max_bytes, as_text=as_text, max_edge=edge, as_images=as_images
    )
    path, mime, size, sha256 = found.path, found.mime, found.size, found.sha256

    blocks: list[ContentBlock]
    extra: dict[str, Any] = {}
    if as_images:
        # The type was proven a PDF in resolve_media; max_edge=0 still renders,
        # because "the stored bytes" is what as_images was asked *not* to give.
        blocks, extra = _page_image_blocks(
            found, page_one, max_pages, edge or media_image.DEFAULT_MAX_EDGE, encode_quality
        )
    elif as_text and not is_text_mime(mime):
        # A text file is already text: as_text changes nothing for it, and
        # asking for a JPEG as text is refused inside extract() rather than
        # silently answered with its bytes.
        blocks, extra = _extracted_blocks(path, mime, max_pages)
    elif mime in IMAGE_MIMES and edge:
        blocks, extra = _image_blocks(found, edge, encode_quality)
    elif mime in RENDERABLE_IMAGE_MIMES:
        # max_edge=0: the file as it is stored, which is what an agent asks for
        # when the detail matters more than the payload.
        data = read_capped(path, size)
        blocks = [ImageContent(type="image", data=base64.b64encode(data).decode("ascii"), mime_type=mime)]
    elif is_text_mime(mime):
        # replace, not strict: a mislabelled .txt must degrade to readable text
        # rather than fail the whole read.
        blocks = [text_block(read_capped(path, size).decode("utf-8", errors="replace"))]
    elif mime in PLAYABLE_AUDIO_MIMES:
        data = base64.b64encode(read_capped(path, size)).decode("ascii")
        blocks = [AudioContent(type="audio", data=data, mime_type=mime)]
    else:
        # A resource, not a text block: the bytes keep their real type, so a
        # client that knows what application/pdf is can hand it to the model as
        # a document instead of showing it a header line and 2 MiB of base64.
        blocks = [
            EmbeddedResource(
                type="resource",
                resource=BlobResourceContents(
                    uri=media_uri(chat_jid, message_id),
                    mime_type=mime,
                    blob=base64.b64encode(read_capped(path, size)).decode("ascii"),
                ),
            )
        ]
        if mime in media_text.EXTRACTABLE_MIMES:
            # A client that does not open resources shows the model the
            # metadata block and nothing else, and an unread document reads
            # exactly like an empty one. This line is written here, not by
            # whoever sent the file, so it stays outside the untrusted
            # envelope; it is the only thing that makes that case recoverable.
            blocks.append(
                TextContent(
                    type="text",
                    text=(
                        f"[{mime}, {size} bytes, returned as a resource. If you cannot read its "
                        f"contents, call read_media(as_text=true) and the text will be extracted here.]"
                    ),
                )
            )
    # The link is redundant with the bytes above and cheap; it is what lets a
    # client (or the agent, on a second pass) come back for the same file
    # through resources/read instead of paying for another tool call. Only when
    # that read would actually succeed: with as_text a 5 MiB PDF is answered
    # here and refused there (a resource is never extracted, so its ceiling is
    # the 2 MiB one), and a link to bytes the server would refuse is worse than
    # no link.
    if size <= hard_limit(mime):
        extra["resource_link"] = resource_link(
            chat_jid, message_id, found.filename or os.path.basename(path), mime, size
        )
    # The stored file's type and size, except where a branch re-encoded it: the
    # metadata block describes the payload that travelled, and puts the file's
    # own numbers under `original_mime` / `original_bytes`.
    payload_mime = extra.pop("mime", mime)
    payload_bytes = extra.pop("bytes", size)
    return [*blocks, meta_block(sha256, payload_mime, payload_bytes, **extra)]
