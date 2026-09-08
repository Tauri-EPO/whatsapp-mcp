"""Images made affordable for the model: downscaled, upright, stripped, in a format clients render.

``read_media`` used to hand back the file as it sits on disk. For a photo that
is the wrong answer twice over. A vision model resamples anything above ~1568
pixels on the long edge before it looks at it, so a 4000 px phone photo pays
20-50x the payload for pixels nobody reads — and it pays it as base64 inside a
JSON response, over Tailscale, into a context window. Above ~5 MB most clients
refuse the block outright, so the biggest photos were exactly the ones that
failed (issue #368).

So the bytes are prepared here first:

* **downscaled** to fit ``max_edge`` on the long edge, never upscaled — a small
  image is left at its own size;
* **upright**: a phone writes the rotation into an EXIF tag rather than into
  the pixels, and a model that gets the tag stripped without the rotation
  applied sees the photo on its side;
* **stripped**: nothing is passed for ``exif`` or ``icc_profile`` on the way
  out, so the GPS coordinates, the camera serial and the timestamps a photo
  carries do not travel to the model with it;
* **re-encoded** as JPEG at ``quality``, or PNG when the image has an alpha
  channel (JPEG has none, and compositing transparency onto black turns a
  sticker into a black rectangle).

That last step is also what makes TIFF, BMP and HEIC readable at all: no MCP
client renders them, so they used to come back as a resource full of bytes the
model cannot open. Converted, they are ``ImageContent`` like any photo.

Two things this module deliberately does **not** do:

* **it never grows a file.** When the image is already in a format clients
  render, is under :data:`PASSTHROUGH_MAX_BYTES`, already fits ``max_edge``,
  carries no rotation and has no metadata to strip, :func:`render` returns
  ``data=None`` and the caller ships the original bytes: re-encoding a 68-byte
  PNG icon as JPEG costs quality and saves nothing. A phone photo always carries
  an EXIF block, so that path belongs to stickers, icons and screenshots;
* **it never touches the stored file.** The cache under ``store/<chat_jid>/``
  stays byte-for-byte what WhatsApp delivered, which is what the
  ``whatsapp://media/...`` resource serves and what ``sha256`` identifies.

``max_edge=0`` turns the whole module off for a call: the caller gets the file
as it is, which is the pre-#368 behaviour and the way to fetch an image whose
detail actually matters.

Anyone who can message the account chooses these files, so a decoded image is
bounded before it is decoded: :data:`MAX_PIXELS` is checked against the header,
which is all ``Image.open`` reads, and a file that lies about its dimensions
fails as bad input rather than as a memory error.
"""

from __future__ import annotations

import io
from typing import Any, NamedTuple

from PIL import Image, ImageOps

from errors import ToolError

# What a vision model actually consumes: Claude resamples to ~1568 px on the
# long edge, and the other hosted models are within a factor of two of that.
DEFAULT_MAX_EDGE = 1568
# A ceiling on the argument, not on the image: asking for 10^9 px must not turn
# into an attempt to encode one.
MAX_EDGE_LIMIT = 8192
DEFAULT_QUALITY = 85

# Decompression-bomb guard, checked against the header before any pixel is
# decoded. 64 MP is above every consumer camera (a 50 MP phone photo is
# ~8100x6100) and costs ~192 MB as RGB, which is what an image this size really
# means for a container that also holds two SQLite databases.
MAX_PIXELS = 64_000_000

# Above this, a file a client renders is re-encoded even though its pixels
# already fit: a photo at 1568 px and quality 85 is 200-600 KB, so anything past
# a megabyte has room to shrink, and that is the whole point of the change. Below
# it the file travels untouched, because a lossy round trip through JPEG would
# cost quality to save a few dozen KB.
PASSTHROUGH_MAX_BYTES = 1024 * 1024

JPEG_MIME = "image/jpeg"
PNG_MIME = "image/png"

# Image types Pillow decodes and no MCP client renders. Converted on the way
# out, so they arrive as a picture instead of as a blob; media_read adds them
# to the image tier for the size cap, because that is what they now are.
CONVERTIBLE_IMAGE_MIMES = frozenset(
    {
        "image/tiff",
        "image/bmp",
        "image/x-ms-bmp",
        "image/heic",
        "image/heif",
    }
)

# HEIC/HEIF is the one format Pillow itself does not read; pillow-heif
# registers a plugin for it. Loaded on first use rather than at import, so an
# archive of JPEGs never pays for libheif.
HEIF_MIMES = frozenset({"image/heic", "image/heif"})
_heif_registered = False

# EXIF tag 0x0112: the rotation the camera recorded instead of applying.
ORIENTATION_TAG = 0x0112

# The `info` keys that hold metadata rather than picture: the EXIF block, the
# colour profile, XMP and the Photoshop/IPTC resource block. Their presence is
# what makes a file worth re-encoding even when its pixels are already fine —
# see :func:`_carries_metadata`. GIF's own `duration`/`background`/`version` are
# deliberately not here: they describe the image, not whoever made it.
METADATA_KEYS = ("exif", "icc_profile", "xmp", "XML:com.adobe.xmp", "photoshop")


class Rendered(NamedTuple):
    """What :func:`render` made of one image.

    ``data`` is ``None`` when the file's own bytes are already the best answer;
    ``width`` / ``height`` describe what the model will see either way,
    ``resized`` says whether the pixels were scaled down, and ``frames`` is how
    many the source held — more than one means an animation was flattened to
    its first frame.
    """

    data: bytes | None
    mime: str
    width: int
    height: int
    resized: bool
    frames: int = 1


def edge_limit(max_edge: Any) -> int:
    """``max_edge`` as a pixel count; 0 means "return the file untouched"."""
    try:
        value = int(max_edge or 0)
    except (TypeError, ValueError) as exc:
        raise ToolError("invalid_argument", f"max_edge must be an integer, got {max_edge!r}") from exc
    if value < 0:
        raise ToolError("invalid_argument", f"max_edge cannot be negative (got {value}); 0 means the original bytes")
    return min(value, MAX_EDGE_LIMIT) if value else 0


def quality_limit(quality: Any) -> int:
    """``quality`` as a JPEG quality; 0 or unset means :data:`DEFAULT_QUALITY`."""
    try:
        value = int(quality or 0)
    except (TypeError, ValueError) as exc:
        raise ToolError("invalid_argument", f"quality must be an integer, got {quality!r}") from exc
    if value == 0:
        return DEFAULT_QUALITY
    if not 1 <= value <= 100:
        raise ToolError("invalid_argument", f"quality must be between 1 and 100 (got {value}); 0 means 85")
    return value


def _register_heif() -> None:
    global _heif_registered
    if _heif_registered:
        return
    try:
        import pillow_heif
    except ImportError as exc:  # pragma: no cover - the dependency is pinned in pyproject.toml
        raise ToolError("internal", f"HEIC support is not installed on this server: {exc}") from exc
    # register_heif_opener is re-exported without `__all__`, so pyright calls it private.
    pillow_heif.register_heif_opener()  # pyright: ignore[reportPrivateImportUsage]
    _heif_registered = True


def _guard_pixels(width: int, height: int) -> None:
    if width * height > MAX_PIXELS:
        raise ToolError(
            "too_large",
            f"this image is {width}x{height} pixels, more than read_media decodes ({MAX_PIXELS}). "
            "Call again with max_edge=0 to get its bytes without decoding them",
            pixels=width * height,
            limit=MAX_PIXELS,
        )


def _has_alpha(image: Image.Image) -> bool:
    """Whether losing the alpha channel would change what the image looks like."""
    return image.mode in {"RGBA", "LA", "PA"} or (image.mode == "P" and "transparency" in image.info)


def _carries_metadata(image: Image.Image) -> bool:
    """Whether this file holds anything that must not reach the model with the picture.

    The passthrough below exists so a small icon is not re-encoded for nothing;
    it must not become a hole in "metadata is stripped". A phone photo always
    carries an EXIF block (GPS, camera serial, the timestamp), so the file that
    would most benefit from travelling untouched is exactly the one that has to
    be re-encoded — and a sticker or a screenshot, which carries none of it,
    still takes the cheap path.
    """
    return any(key in image.info for key in METADATA_KEYS) or bool(image.getexif())


def _encode(image: Image.Image, quality: int) -> tuple[bytes, str]:
    """The image as the smallest of the two formats every client renders.

    What the camera wrote into the file does not travel with the picture. The
    JPEG writer takes its EXIF and profile from ``encoderinfo`` only, so
    passing neither is enough; the PNG writer falls back to the *source*
    image's ``info`` for both, and ``Image.convert`` copies ``info`` along, so
    there the two have to be silenced explicitly.
    """
    buffer = io.BytesIO()
    if _has_alpha(image):
        image.convert("RGBA").save(buffer, format="PNG", optimize=True, icc_profile=None, exif=None)
        return buffer.getvalue(), PNG_MIME
    if image.mode not in {"RGB", "L"}:
        # P (a GIF or a palette PNG), CMYK, I;16 from a scanner: JPEG writes
        # none of them.
        image = image.convert("RGB")
    image.save(buffer, format="JPEG", quality=quality, optimize=True, progressive=True)
    return buffer.getvalue(), JPEG_MIME


def render(path: str, mime: str, max_edge: int, quality: int, passthrough: bool) -> Rendered:
    """The image at ``path``, sized and encoded for a model.

    ``passthrough`` says whether the caller may ship the source bytes as they
    are: the type is one a client renders *and* the file is small enough that
    re-encoding it would not pay (:data:`PASSTHROUGH_MAX_BYTES`). Even then the
    bytes only travel when there is nothing to do to them — they already fit
    ``max_edge``, they are upright, and they carry no metadata to strip. Without
    it — TIFF, BMP, HEIC, or a 4 MB PNG screenshot — the encode happens whatever
    the pixel size.

    A file Pillow cannot decode is ``invalid_argument``, not a crash and not a
    silent fallback: an image block whose payload is not an image fails the
    whole call at the client, and the message names ``max_edge=0`` as the way
    to get the bytes anyway.
    """
    if mime in HEIF_MIMES:
        _register_heif()
    try:
        with Image.open(path) as opened:
            width, height = opened.size
            _guard_pixels(width, height)
            # An animated GIF or a multi-page TIFF: frame 0 is what Image.open
            # positions on, and one frame is what an image block can carry. The
            # count is reported so a flattened animation is not silent.
            frames = int(getattr(opened, "n_frames", 1) or 1)
            rotated = opened.getexif().get(ORIENTATION_TAG, 1) not in {0, 1}
            resized = max(width, height) > max_edge
            if passthrough and not resized and not rotated and not _carries_metadata(opened):
                return Rendered(None, mime, width, height, False, frames)
            image = opened
            if resized:
                if image.mode in {"P", "1"}:
                    # Pillow resamples a palette image with NEAREST whatever
                    # filter it is given, so a GIF or a palette PNG has to leave
                    # its palette first or it comes back visibly blocky.
                    image = image.convert("RGBA" if _has_alpha(image) else "RGB")
                # Before the transpose, not after: thumbnail() calls draft() on
                # a JPEG, which decodes at a reduced DCT scale instead of full
                # resolution. Rotating first would have forced the full decode
                # plus a full-size copy — hundreds of megabytes for a photo at
                # the MAX_PIXELS ceiling, on a box with no memory limit. The
                # target box is square, so the order does not change the result.
                image.thumbnail((max_edge, max_edge), Image.Resampling.LANCZOS)
            if rotated:
                image = ImageOps.exif_transpose(image) or image
            width, height = image.size
            data, out_mime = _encode(image, quality)
    except ToolError:
        raise
    except Image.DecompressionBombError as exc:
        raise ToolError("too_large", f"this image is too large to decode: {exc}", limit=MAX_PIXELS) from exc
    except Exception as exc:  # noqa: BLE001 - a corrupt file is bad input, not a server fault
        raise ToolError(
            "invalid_argument",
            f"this image could not be decoded: {type(exc).__name__}: {exc}. "
            "Call again with max_edge=0 to get the bytes as they are stored",
        ) from exc
    return Rendered(data, out_mime, width, height, resized, frames)
