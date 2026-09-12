"""Inline media for send_file / send_audio_message: bytes handed to the tool
instead of a path on this host.

Agents often run on a different machine than the MCP server, and a
``media_path`` they cannot write to is no use to them (issue #423). With
``media_base64`` the bytes come in the tool call: they are decoded, bounded,
written under the outbox the bridge may read from, sent and removed. The
bridge is unchanged, it still receives a ``media_path`` confined to
``WHATSAPP_MEDIA_ROOTS``.

The directory is ``<first WHATSAPP_MEDIA_ROOTS entry>/.uploads``, or
``~/.local/share/whatsapp-mcp/outbox/.uploads`` when the variable is unset:
the bridge's own default root, so the two processes agree without further
configuration. Every upload gets a directory of its own
(``<utc timestamp>-<8 hex>/<filename>``) because WhatsApp shows a document's
base name to the recipient, so the name the agent chose has to survive.
"""

from __future__ import annotations

import base64
import binascii
import mimetypes
import os
import re
import secrets
import shutil
from datetime import UTC, datetime

from errors import ToolError

# Above this the tool refuses the payload before decoding it. On the http
# transport WHATSAPP_MCP_MAX_BODY_BYTES (4 MiB by default) cuts in first, at
# about three quarters of that in file bytes; this is the ceiling for stdio.
MAX_INLINE_BYTES = 64 * 1024 * 1024

# The bridge's default root (media_path.go, defaultOutboxSubpath).
DEFAULT_MEDIA_ROOT = os.path.join("~", ".local", "share", "whatsapp-mcp", "outbox")

UPLOADS_SUBDIR = ".uploads"

# Separators, Windows-reserved characters and control characters: the name is
# a base name inside a directory this module owns, nothing else.
_UNSAFE = re.compile(r'[\x00-\x1f\x7f/\\:*?"<>|]+')
_DATA_URL = re.compile(r"^data:[^,]*;base64,", re.IGNORECASE)


def media_root() -> str:
    """The first WHATSAPP_MEDIA_ROOTS entry, or the bridge's default outbox."""
    raw = (os.getenv("WHATSAPP_MEDIA_ROOTS") or "").strip()
    first = ""
    if raw:
        first = next((entry.strip() for entry in raw.split(os.pathsep) if entry.strip()), "")
    return os.path.abspath(os.path.expanduser(first or DEFAULT_MEDIA_ROOT))


def upload_dir() -> str:
    return os.path.join(media_root(), UPLOADS_SUBDIR)


def safe_filename(name: str, default: str) -> str:
    """A base name the filesystem and the recipient can both take.

    Separators are cut down to the last component, unsafe characters become
    ``_``, leading/trailing dots and spaces go, and the result is capped at
    200 characters with the extension kept. ``default`` when nothing is left.
    """
    base = os.path.basename((name or "").replace("\\", "/").strip())
    base = _UNSAFE.sub("_", base).strip(" .")
    if not base:
        return default
    if len(base) > 200:
        stem, ext = os.path.splitext(base)
        ext = ext[:16]
        base = stem[: 200 - len(ext)] + ext
    return base


def decode_inline(media_base64: str) -> bytes:
    """The bytes behind ``media_base64``: strict base64, a data URL prefix
    tolerated, whitespace ignored, size bounded before and after decoding."""
    if not isinstance(media_base64, str) or not media_base64.strip():
        raise ToolError("invalid_argument", "media_base64 is empty")
    text = _DATA_URL.sub("", media_base64.strip(), count=1)
    text = re.sub(r"\s+", "", text)
    approx = len(text) * 3 // 4
    if approx > MAX_INLINE_BYTES:
        raise ToolError(
            "invalid_argument",
            f"media_base64 decodes to about {approx} bytes; the limit is {MAX_INLINE_BYTES} "
            "(put the file on the server and use media_path for anything bigger)",
        )
    try:
        data = base64.b64decode(text, validate=True)
    except (binascii.Error, ValueError) as exc:
        raise ToolError("invalid_argument", f"media_base64 is not valid base64: {exc}") from exc
    if not data:
        raise ToolError("invalid_argument", "media_base64 decodes to zero bytes")
    if len(data) > MAX_INLINE_BYTES:
        raise ToolError(
            "invalid_argument", f"media_base64 decodes to {len(data)} bytes; the limit is {MAX_INLINE_BYTES}"
        )
    return data


def guess_mime(filename: str) -> str | None:
    return mimetypes.guess_type(filename)[0]


def write_inline(data: bytes, filename: str) -> str:
    """Write ``data`` as ``<upload_dir>/<stamp>-<hex>/<filename>`` and return
    that path. The directory is fresh, so the name never collides; the file
    lands through a ``.part`` rename, so the bridge never reads a half write."""
    root = upload_dir()
    os.makedirs(root, exist_ok=True)
    stamp = datetime.now(UTC).strftime("%Y%m%dT%H%M%SZ")
    folder = os.path.join(root, f"{stamp}-{secrets.token_hex(4)}")
    os.makedirs(folder)
    path = os.path.join(folder, filename)
    part = path + ".part"
    with open(part, "wb") as handle:
        handle.write(data)
    os.replace(part, path)
    return path


def discard(path: str) -> None:
    """Remove what write_inline or the audio conversion left under the upload
    directory: the per-upload folder, or a lone file directly in it. Anything
    elsewhere is not ours and stays. Never raises."""
    root = upload_dir()
    parent = os.path.dirname(os.path.abspath(path))
    try:
        if os.path.dirname(parent) == root:
            shutil.rmtree(parent, ignore_errors=True)
        elif parent == root and os.path.isfile(path):
            os.remove(path)
    except OSError:
        pass
