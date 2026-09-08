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
* anything else base64-encoded in a text block behind a
  ``base64:<mime>:<bytes>`` header line, capped at 2 MiB.

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

from mcp_types import CallToolResult, ContentBlock, ImageContent, TextContent

import media_inventory
import whatsapp
from errors import ToolError
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
# image/* (TIFF, HEIC, ICO...) takes the base64 path: a block a client refuses
# to render fails the whole call, while base64 at least arrives.
RENDERABLE_IMAGE_MIMES = frozenset({"image/jpeg", "image/png", "image/gif", "image/webp"})

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
    return media_type, row[1], int(row[2]) if row[2] else None, row[3]


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
    """The message's file on disk, or None when nothing is cached for it."""
    cached = media_inventory.scan_chat_cache(chat_jid).get(message_id)
    if cached is None:
        return None
    return _in_chat_dir(chat_jid, os.path.join(media_inventory.chat_media_dir(chat_jid), cached.name))


def download_path(chat_jid: str, message_id: str) -> str:
    """Fetch the bytes through the bridge and return where they landed."""
    path = whatsapp.download_media(message_id, chat_jid)
    if not path:
        raise ToolError("internal", "the bridge reported success without a file path")
    return _in_chat_dir(chat_jid, path)


def guess_mime(media_type: str, filename: str | None, path: str = "") -> str:
    """MIME type from the cached name, then the sender's name, then the media type."""
    for candidate in (path, filename or ""):
        if not candidate:
            continue
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


def is_text_mime(mime: str) -> bool:
    return mime.startswith("text/") or mime in TEXT_MIMES


def hard_limit(mime: str) -> int:
    """The ceiling for this type — and, with the branches below, what decides the block."""
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


def read_media(chat_jid: str, message_id: str, max_bytes: int = 0) -> list[ContentBlock]:
    """The media of one message as content blocks. See the module docstring."""
    whatsapp._require_allowed(chat_jid)
    media_type, filename, reported, sha256 = _media_row(chat_jid, message_id)
    path = cached_path(chat_jid, message_id)
    if path is None:
        # Nothing is cached, so reading means a CDN transfer: refuse a file the
        # row already says is too big instead of paying for it first. The
        # authority is still the size on disk below; this only skips the
        # obvious no.
        if reported:
            expected = guess_mime(media_type, filename)
            check_size(reported, cap(max_bytes, hard_limit(expected)), expected)
        path = download_path(chat_jid, message_id)

    mime = guess_mime(media_type, filename, path)
    if mime.startswith("image/"):
        mime = sniff_image_mime(path) or mime
    try:
        size = os.path.getsize(path)
    except OSError as exc:
        raise ToolError("internal", f"could not stat the cached file: {exc}") from exc
    check_size(size, cap(max_bytes, hard_limit(mime)), mime)

    blocks: list[ContentBlock]
    if mime in RENDERABLE_IMAGE_MIMES:
        data = _read(path, size)
        blocks = [ImageContent(type="image", data=base64.b64encode(data).decode("ascii"), mime_type=mime)]
    elif is_text_mime(mime):
        # replace, not strict: a mislabelled .txt must degrade to readable text
        # rather than fail the whole read.
        blocks = [text_block(_read(path, size).decode("utf-8", errors="replace"))]
    else:
        encoded = base64.b64encode(_read(path, size)).decode("ascii")
        # The header line is what tells a model the rest of the block is not
        # prose: decode it, or hand the whole block to something that can.
        blocks = [TextContent(type="text", text=f"base64:{mime}:{size}\n{encoded}")]
    return [*blocks, meta_block(sha256, mime, size)]
