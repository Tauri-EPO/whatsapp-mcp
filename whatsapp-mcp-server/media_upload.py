"""Media for send_file / send_audio_message: inline bytes or an HTTP upload
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
(``<utc timestamp>-<32 hex>/<filename>``) because WhatsApp shows a document's
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
import threading
from contextlib import contextmanager
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path

from errors import ToolError
from private_files import private_makedirs, private_open, tighten
from untrusted import sanitize_name

# Above this the tool refuses the payload before decoding it. On the http
# transport WHATSAPP_MCP_MAX_BODY_BYTES (4 MiB by default) cuts in first, at
# about three quarters of that in file bytes; this is the ceiling for stdio.
MAX_INLINE_BYTES = 64 * 1024 * 1024

# The bridge's default root (media_path.go, defaultOutboxSubpath).
DEFAULT_MEDIA_ROOT = os.path.join("~", ".local", "share", "whatsapp-mcp", "outbox")

UPLOADS_SUBDIR = ".uploads"
UPLOAD_TTL_S = 3600
MAX_OUTBOX_BYTES = 256 * 1024 * 1024
CAPACITY_QUANTUM = 8 * 1024 * 1024
UPLOAD_ID = re.compile(r"[0-9]{8}T[0-9]{6}Z-[0-9a-f]{32}\Z")
OWNED_FOLDER = re.compile(r"[0-9]{8}T[0-9]{6}Z-(?:[0-9a-f]{8}|[0-9a-f]{32})\Z")
CONVERTED_FILE = re.compile(r"tmp[^/\\]+\.ogg\Z")
_lock = threading.RLock()
_busy: set[str] = set()
_reserved: dict[str, int] = {}


def checked_upload_root() -> str:
    root = upload_dir()
    if not unlinked_path(root):
        raise ToolError("denied", "The uploads directory must not contain symlinks")
    return root


def unlinked_path(path: str | Path) -> bool:
    """The configured root is resolved; links beneath it are never followed."""
    return os.path.normcase(os.path.realpath(path)) == os.path.normcase(os.path.abspath(path))


def full_outbox() -> ToolError:
    return ToolError(
        "too_large",
        "The upload outbox is full; send existing uploads or retry after expiry",
        limit_bytes=MAX_OUTBOX_BYTES,
    )


def owned_entry(path: Path) -> bool:
    if OWNED_FOLDER.fullmatch(path.name):
        try:
            upload_expiry(path.name)
            return True
        except ValueError:
            return False
    return bool(CONVERTED_FILE.fullmatch(path.name))


def check_outbox_capacity(additional: int) -> int:
    """Walk sweepable files under _lock; receiving credits reserve unwritten bytes."""
    used = 0
    root = Path(checked_upload_root())
    if root.exists():
        for folder in list(root.iterdir()):
            if not owned_entry(folder) or not unlinked_path(folder):
                continue
            try:
                files = list(folder.iterdir()) if folder.is_dir() else [folder]
                for path in files:
                    if unlinked_path(path) and path.is_file():
                        try:
                            used += path.stat().st_size
                        except FileNotFoundError:
                            continue
            except FileNotFoundError:
                continue
    available = MAX_OUTBOX_BYTES - used - sum(_reserved.values())
    if additional > available:
        raise full_outbox()
    return available


@dataclass
class ReceivingUpload:
    folder: str
    limit: int
    complete: bool = False
    since_scan: int = 0

    @property
    def upload_id(self) -> str:
        return os.path.basename(self.folder)

    @property
    def pending(self) -> str:
        return os.path.join(self.folder, ".pending")

    def reserve(self) -> None:
        available = check_outbox_capacity(1)
        _reserved[self.upload_id] = min(CAPACITY_QUANTUM, self.limit, available)
        self.since_scan = 0

    def write(self, handle, chunk: bytes) -> None:
        # FileIO writes publish size immediately; no flush/stat walk per chunk.
        with _lock:
            pending = memoryview(chunk)
            while pending:
                if not _reserved[self.upload_id]:
                    if self.since_scan < CAPACITY_QUANTUM:
                        raise full_outbox()  # The previous walk found less than a quantum free.
                    self.reserve()
                piece = pending[: _reserved[self.upload_id]]
                written = handle.write(piece)
                if not written:
                    raise OSError("Upload write made no progress")
                _reserved[self.upload_id] -= written
                self.since_scan += written
                pending = pending[written:]

    def finish(self, filename: str, *, source: str | None = None) -> None:
        with _lock:
            os.replace(source or self.pending, os.path.join(self.folder, filename))
            self.complete = True


@contextmanager
def receiving_upload(limit: int = MAX_INLINE_BYTES):
    """Own a receiving lease and remove incomplete uploads on every exit path."""
    sweep_uploads()  # All transports, including inline stdio, reclaim old owned files.
    with _lock:
        upload = ReceivingUpload(create_upload(), limit)
        try:
            upload.reserve()
        except BaseException:
            shutil.rmtree(upload.folder, ignore_errors=True)
            raise
        _busy.add(upload.upload_id)
    try:
        yield upload
    finally:
        with _lock:
            if not upload.complete:
                shutil.rmtree(upload.folder, ignore_errors=True)
            _busy.discard(upload.upload_id)
            _reserved.pop(upload.upload_id, None)


def create_upload() -> str:
    root = checked_upload_root()
    private_makedirs(root)
    stamp = datetime.now(UTC).strftime("%Y%m%dT%H%M%SZ")
    folder = os.path.join(root, f"{stamp}-{secrets.token_hex(16)}")
    os.mkdir(folder, mode=0o700)
    if os.name == "posix":
        tighten(folder, 0o700, "upload directory")
    return folder


def upload_expiry(upload_id: str) -> float:
    return datetime.strptime(upload_id.split("-")[0], "%Y%m%dT%H%M%SZ").replace(tzinfo=UTC).timestamp() + UPLOAD_TTL_S


def sweep_uploads() -> None:
    """Bound abandoned uploads; never follow links or remove an active transfer."""
    try:
        root = checked_upload_root()
    except (ToolError, OSError):
        return
    with _lock:
        if not os.path.isdir(root):
            return
        try:
            entries = list(Path(root).iterdir())
        except OSError:
            return  # A best-effort sweep must not prevent startup or receiving.
        for entry in entries:
            if not owned_entry(entry) or entry.name in _busy:
                continue
            if not unlinked_path(entry):
                continue
            try:
                if entry.is_dir() and OWNED_FOLDER.fullmatch(entry.name):
                    if upload_expiry(entry.name) <= datetime.now(UTC).timestamp():
                        shutil.rmtree(entry, ignore_errors=True)
                elif entry.is_file() and CONVERTED_FILE.fullmatch(entry.name):
                    if entry.stat().st_mtime + UPLOAD_TTL_S <= datetime.now(UTC).timestamp():
                        entry.unlink()
            except (OSError, ValueError):
                continue


@contextmanager
def uploaded_path(upload_id: str, *, consume: bool = True):
    """Lease a completed HTTP upload; IDs are filenames generated here only."""
    from mcp_config import resolve_transport

    if resolve_transport(os.getenv("WHATSAPP_MCP_TRANSPORT")) == "stdio":
        raise ToolError("invalid_argument", "upload_id requires the http or sse transport; stdio has no /upload route")
    missing = ToolError("not_found", "Unknown or expired upload_id; upload the file again with POST /upload")
    with _lock:
        if not UPLOAD_ID.fullmatch(upload_id):
            raise missing
        if upload_id in _busy:
            raise ToolError("conflict", "upload_id is in use; retry the same ID shortly, without uploading again")
        try:
            expired = upload_expiry(upload_id) <= datetime.now(UTC).timestamp()
        except ValueError:
            raise missing from None
        if expired:
            raise missing
        folder = Path(checked_upload_root()) / upload_id
        if not folder.is_dir() or not unlinked_path(folder):
            raise missing
        files = list(folder.iterdir())
        if len(files) != 1 or files[0].name == ".pending" or not files[0].is_file():
            raise missing
        path = files[0]
        if not unlinked_path(path):
            raise missing
        _busy.add(upload_id)
    succeeded = False
    try:
        yield str(path)
        succeeded = True
    finally:
        with _lock:
            # An unavailable bridge or failed conversion can be retried.
            if consume and succeeded:
                discard(str(path))
            _busy.discard(upload_id)


# Separators, Windows-reserved characters and control characters: the name is
# a base name inside a directory this module owns, nothing else.
_UNSAFE = re.compile(r'[\x00-\x1f\x7f-\x9f/\\:*?"<>|]+')
_DATA_URL = re.compile(r"^data:[^,]*;base64,", re.IGNORECASE)


def configured_media_root() -> str:
    """The agreed path spelling, which may differ across filesystem namespaces."""
    raw = (os.getenv("WHATSAPP_MEDIA_ROOTS") or "").strip()
    first = ""
    if raw:
        first = next((entry.strip() for entry in raw.split(os.pathsep) if entry.strip()), "")
    return os.path.abspath(os.path.expanduser(first or DEFAULT_MEDIA_ROOT))


def media_root() -> str:
    """The physical first media root, used for local IO and confinement."""
    return os.path.realpath(configured_media_root())


def upload_dir() -> str:
    return os.path.join(media_root(), UPLOADS_SUBDIR)


def bridge_media_path(path: str, *, preview: bool = False) -> str:
    """Translate owned uploads only; preserve other caller-supplied paths.

    Local IO always uses the physical root. The bridge sees the agreed
    configured spelling, and links underneath .uploads are refused.
    """
    configured = configured_media_root()
    physical = media_root()
    uploads = os.path.join(physical, UPLOADS_SUBDIR)
    absolute = os.path.abspath(path)
    relative = None
    for root in (uploads, os.path.join(configured, UPLOADS_SUBDIR)):
        try:
            if os.path.normcase(os.path.commonpath([absolute, root])) == os.path.normcase(root):
                relative = os.path.relpath(absolute, root)
                break
        except ValueError:
            continue  # Different Windows drives: this is the caller's path.
    if relative is None:
        return path
    checked_upload_root()
    local = os.path.join(uploads, relative)
    if not unlinked_path(local):
        raise ToolError("denied", "Uploads must not contain symlinks")
    parts = Path(relative).parts
    owned = (len(parts) == 2 and owned_entry(Path(parts[0]))) or (
        len(parts) == 1 and bool(CONVERTED_FILE.fullmatch(parts[0]))
    )
    if preview and len(parts) == 2 and parts[0] == "<upload>":
        owned = True
    if not owned:
        return path
    translated = os.path.join(configured, UPLOADS_SUBDIR, relative)
    if os.path.normcase(os.path.realpath(configured)) != os.path.normcase(physical) or (
        os.path.normcase(os.path.realpath(translated)) != os.path.normcase(os.path.abspath(local))
    ):
        raise ToolError("denied", "The configured outbox no longer resolves to the same upload")
    return translated


def safe_filename(name: str, default: str) -> str:
    """A base name the filesystem and the recipient can both take.

    Separators are cut down to the last component, unsafe characters become
    ``_``, leading/trailing dots and spaces go, and the result is capped at
    200 UTF-8 bytes with the extension kept. ``default`` when nothing is left.
    """
    cleaned = sanitize_name(name or "", max_chars=max(1, len(name or ""))).replace("\u2028", "").replace("\u2029", "")
    base = os.path.basename(cleaned.replace("\\", "/").strip())
    base = _UNSAFE.sub("_", base).strip(" .")
    if not base:
        return default
    device = base.split(".", 1)[0].rstrip(" .").upper()
    if device in {"CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$"} or re.fullmatch(r"(?:COM|LPT)[0-9¹²³]", device):
        base = "_" + base
    if len(base.encode("utf-8")) > 200:
        stem, ext = os.path.splitext(base)
        ext = ext.encode("utf-8")[:16].decode("utf-8", errors="ignore")
        base = stem.encode("utf-8")[: 200 - len(ext.encode("utf-8"))].decode("utf-8", errors="ignore") + ext
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
    lands through a ``.pending`` rename, so the bridge never reads a half write."""
    with receiving_upload(limit=len(data)) as upload:
        path = os.path.join(upload.folder, filename)
        with private_open(upload.pending, "xb", buffering=0) as handle:
            upload.write(handle, data)
        upload.finish(filename)
    return path


def discard(path: str) -> None:
    """Remove what write_inline or the audio conversion left under the upload
    directory: the per-upload folder, or a lone file directly in it. Anything
    elsewhere is not ours and stays. Never raises."""
    with _lock:
        root = upload_dir()
        parent = os.path.dirname(os.path.abspath(path))
        try:
            if os.path.dirname(parent) == root:
                shutil.rmtree(parent, ignore_errors=True)
            elif parent == root and os.path.isfile(path):
                os.remove(path)
        except OSError:
            pass
