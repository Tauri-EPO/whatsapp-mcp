"""The bytes of one media message, in the answer instead of on the server's disk.

``download_media`` returns a **path**. That is the right answer for a client
running on the same filesystem as the bridge, and useless for the deployment
this fork is built for: an agent on another machine, reaching the MCP endpoint
over Tailscale, gets ``/app/store/5511…@s.whatsapp.net/image_20260904_100000_3EB0.jpg``
and has nothing to open it with (issue #285). It could see that a photo exists,
read its size and its notes, and never look at it.

``read_media`` closes that gap by returning MCP **content blocks**:

* an image as ``ImageContent`` — the model sees the picture, capped at 16 MiB;
* a small text-ish file (``text/*``, JSON, CSV) as text, capped at 1 MiB;
* a voice note as ``AudioContent``, the block the protocol has for audio;
* anything else — a PDF, a DOCX, a video, an archive — as an
  ``EmbeddedResource`` whose ``BlobResourceContents`` carries the file's real
  MIME type and a ``whatsapp://media/<chat_jid>/<message_id>`` URI, capped at
  2 MiB. That is what a client needs to hand a PDF to the model as a document
  (issue #367); the ``base64:<mime>:<bytes>`` text block it replaces was one
  every client had to decode itself, and none did;
* with ``as_text=True``, a PDF, DOCX or XLSX **read on this side** and returned
  as text (media_text.py), which is what makes a 5 MiB clinical PDF affordable.

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
from typing import Any
from urllib.parse import quote

from mcp_types import (
    AudioContent,
    BlobResourceContents,
    CallToolResult,
    ContentBlock,
    EmbeddedResource,
    ImageContent,
    TextContent,
)

import media_inventory
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

# The image types an MCP client can actually display. Anything else labelled
# image/* (TIFF, HEIC, ICO...) goes out as a resource: a block a client refuses
# to render fails the whole call, while the bytes at least arrive.
RENDERABLE_IMAGE_MIMES = frozenset({"image/jpeg", "image/png", "image/gif", "image/webp"})

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
)

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


def is_text_mime(mime: str) -> bool:
    return mime.startswith("text/") or mime in TEXT_MIMES


def hard_limit(mime: str, as_text: bool = False) -> int:
    """The ceiling for this type — and, with the branches below, what decides the block.

    Extraction has its own, much higher ceiling: what bounds ``as_text`` is the
    text it produces (media_text.MAX_TEXT_CHARS), not the size of the file it
    reads, which is the whole reason to extract instead of returning base64.
    """
    if as_text and mime in media_text.EXTRACTABLE_MIMES:
        return media_text.MAX_EXTRACT_BYTES
    if mime in RENDERABLE_IMAGE_MIMES:
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


def check_size(size: int, limit: int, mime: str) -> None:
    if size <= limit:
        return
    raise ToolError(
        "too_large",
        f"the file is {size} bytes and read_media returns at most {limit} for {mime}. "
        "download_media still returns the server-side path, which only helps a client sharing this filesystem",
        bytes=size,
        limit=limit,
    )


def _read(path: str, size: int) -> bytes:
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
    """The trailing block: what the file is, and what the agent already knows about it."""
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


def read_media(
    chat_jid: str,
    message_id: str,
    max_bytes: int = 0,
    as_text: bool = False,
    max_pages: int = 0,
) -> list[ContentBlock]:
    """The media of one message as content blocks. See the module docstring."""
    whatsapp._require_allowed(chat_jid)
    media_type, filename, reported, sha256 = _media_row(chat_jid, message_id)
    path = cached_path(chat_jid, message_id)
    if path is None:
        # Nothing is cached, so reading means a CDN transfer. Whether it may
        # happen at all comes first: "too_large, and download_media returns a
        # path instead" would recommend a tool this very policy took away.
        if not offers_download():
            raise download_denied("read_media")
        # Refuse a file the row already says is too big instead of paying for
        # it first. The authority is still the size on disk below; this only
        # skips the obvious no.
        if reported:
            expected = guess_mime(media_type, filename)
            check_size(reported, cap(max_bytes, hard_limit(expected, as_text)), expected)
        path = download_path(chat_jid, message_id)

    mime = guess_mime(media_type, filename, path)
    if mime.startswith("image/"):
        # The bytes decide. A name that promises a JPEG over a payload that is
        # not one must not produce an ImageContent: the client rejects a block
        # whose data disagrees with its type, so it goes out as an untyped
        # resource (application/octet-stream) instead.
        sniffed = sniff_image_mime(path)
        mime = sniffed or ("application/octet-stream" if mime in RENDERABLE_IMAGE_MIMES else mime)
    elif mime.startswith("audio/"):
        # And here too, for the same reason: the bridge calls every audio
        # message `.ogg`, so an MP3 would go out declared audio/ogg.
        sniffed = sniff_audio_mime(path)
        mime = sniffed or ("application/octet-stream" if mime in PLAYABLE_AUDIO_MIMES else mime)
    if as_text and not is_text_mime(mime):
        # Before the size check: "as_text does not apply to a video" is the
        # useful answer, not "that video is over the 2 MiB cap".
        media_text.require_extractable(mime)
    try:
        size = os.path.getsize(path)
    except OSError as exc:
        raise ToolError("internal", f"could not stat the cached file: {exc}") from exc
    check_size(size, cap(max_bytes, hard_limit(mime, as_text)), mime)

    blocks: list[ContentBlock]
    extra: dict[str, Any] = {}
    if as_text and not is_text_mime(mime):
        # A text file is already text: as_text changes nothing for it, and
        # asking for a JPEG as text is refused inside extract() rather than
        # silently answered with its bytes.
        blocks, extra = _extracted_blocks(path, mime, max_pages)
    elif mime in RENDERABLE_IMAGE_MIMES:
        data = _read(path, size)
        blocks = [ImageContent(type="image", data=base64.b64encode(data).decode("ascii"), mime_type=mime)]
    elif is_text_mime(mime):
        # replace, not strict: a mislabelled .txt must degrade to readable text
        # rather than fail the whole read.
        blocks = [text_block(_read(path, size).decode("utf-8", errors="replace"))]
    elif mime in PLAYABLE_AUDIO_MIMES:
        data = base64.b64encode(_read(path, size)).decode("ascii")
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
                    blob=base64.b64encode(_read(path, size)).decode("ascii"),
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
    return [*blocks, meta_block(sha256, mime, size, **extra)]
