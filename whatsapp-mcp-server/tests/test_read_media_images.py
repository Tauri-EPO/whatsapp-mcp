"""read_media downscales, rotates and converts images before they travel (#368)."""

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
import media_read
import whatsapp
from errors import ToolError
from tests.conftest import ALICE

SHA_PHOTO = bytes.fromhex("dd" * 32)


def _photo(width: int, height: int, mode: str = "RGB", fmt: str = "JPEG", **save: object) -> bytes:
    """A real encoded image, built from Pillow's own gradients so the fixture stays cheap."""
    bands = [
        Image.linear_gradient("L"),
        Image.radial_gradient("L"),
        Image.linear_gradient("L").transpose(Image.Transpose.ROTATE_90),
    ]
    image = Image.merge("RGB", bands).resize((width, height))
    if mode != "RGB":
        image = image.convert(mode)
    buffer = io.BytesIO()
    image.save(buffer, format=fmt, **save)
    return buffer.getvalue()


def _insert(conn, msg_id, media_type="image", sha=SHA_PHOTO, filename=None, length=None):
    conn.execute(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, "
        "file_length, file_sha256) VALUES (?, ?, '5511888888888', '', '2026-09-04 10:00:00+00:00', 0, ?, ?, ?, ?)",
        (msg_id, ALICE, media_type, filename, length, sha),
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


def _payload(block) -> bytes:
    assert block.type == "image"
    return base64.b64decode(block.data)


def _size(block) -> tuple[int, int]:
    with Image.open(io.BytesIO(_payload(block))) as image:
        return image.size


@pytest.fixture
def photos(paired_dbs, monkeypatch):
    """One oversized JPEG photo, cached under the .jpg name the bridge writes."""
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.load_chat_policy({}))
    with paired_dbs.messages() as conn:
        _insert(conn, "BIGJPG")
    _cache("image_20260904_100000_BIGJPG.jpg", _photo(4000, 3000, quality=95))
    return paired_dbs


class TestDownscale:
    def test_a_4000px_photo_comes_back_small_enough_for_a_model(self, photos):
        blocks = media_read.read_media(ALICE, "BIGJPG")
        meta = _meta(blocks)
        assert _size(blocks[0]) == (1568, 1176)
        assert meta["resized"] is True
        assert (meta["width"], meta["height"]) == (1568, 1176)
        assert meta["mime"] == "image/jpeg" and meta["original_mime"] == "image/jpeg"
        # The point of the whole change: the payload is a fraction of the file,
        # and the file's own size is still reported so the agent can ask for it.
        assert meta["bytes"] < meta["original_bytes"] // 4
        assert meta["original_bytes"] == os.path.getsize(
            os.path.join(media_inventory.chat_media_dir(ALICE), "image_20260904_100000_BIGJPG.jpg")
        )

    def test_max_edge_zero_returns_the_stored_bytes(self, photos):
        original = open(
            os.path.join(media_inventory.chat_media_dir(ALICE), "image_20260904_100000_BIGJPG.jpg"), "rb"
        ).read()
        blocks = media_read.read_media(ALICE, "BIGJPG", max_edge=0)
        assert _payload(blocks[0]) == original
        # No re-encoding happened, so nothing is claimed about width or height.
        assert "resized" not in _meta(blocks)

    def test_max_edge_chooses_the_size(self, photos):
        assert _size(media_read.read_media(ALICE, "BIGJPG", max_edge=512)[0]) == (512, 384)

    def test_a_small_image_is_never_upscaled_nor_re_encoded(self, paired_dbs, monkeypatch):
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.load_chat_policy({}))
        with paired_dbs.messages() as conn:
            _insert(conn, "SMALL")
        small = _photo(40, 30)
        _cache("image_20260904_100000_SMALL.jpg", small)
        blocks = media_read.read_media(ALICE, "SMALL")
        assert _payload(blocks[0]) == small
        meta = _meta(blocks)
        assert (meta["width"], meta["height"], meta["resized"]) == (40, 30, False)
        assert meta["bytes"] == meta["original_bytes"] == len(small)

    def test_a_heavy_file_is_re_encoded_even_when_its_pixels_already_fit(self, paired_dbs, monkeypatch):
        """A 1200 px PNG screenshot can still be megabytes; that is what this is for."""
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.load_chat_policy({}))
        monkeypatch.setattr(media_image, "PASSTHROUGH_MAX_BYTES", 1000)
        with paired_dbs.messages() as conn:
            _insert(conn, "HEAVY")
        heavy = _photo(1200, 800, fmt="PNG")
        _cache("image_20260904_100000_HEAVY.png", heavy)
        blocks = media_read.read_media(ALICE, "HEAVY")
        meta = _meta(blocks)
        assert (meta["width"], meta["height"], meta["resized"]) == (1200, 800, False)
        assert meta["mime"] == "image/jpeg" and meta["original_mime"] == "image/png"
        assert meta["bytes"] < meta["original_bytes"] == len(heavy)

    def test_the_stored_file_is_never_touched(self, photos):
        path = os.path.join(media_inventory.chat_media_dir(ALICE), "image_20260904_100000_BIGJPG.jpg")
        before = open(path, "rb").read()
        media_read.read_media(ALICE, "BIGJPG")
        assert open(path, "rb").read() == before

    def test_quality_moves_the_payload_size(self, photos):
        low = _meta(media_read.read_media(ALICE, "BIGJPG", quality=20))["bytes"]
        high = _meta(media_read.read_media(ALICE, "BIGJPG", quality=95))["bytes"]
        assert low < high

    def test_the_resource_link_still_describes_the_stored_file(self, photos):
        """resources/read serves the original bytes, so the link must not claim the payload's."""
        meta = _meta(media_read.read_media(ALICE, "BIGJPG"))
        assert meta["resource_link"]["mimeType"] == "image/jpeg"
        assert meta["resource_link"]["size"] == meta["original_bytes"]


class TestAlphaAndOrientation:
    def test_a_transparent_image_stays_png(self, paired_dbs, monkeypatch):
        """JPEG has no alpha channel: a sticker would come back on a black square."""
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.load_chat_policy({}))
        with paired_dbs.messages() as conn:
            _insert(conn, "ALPHA", media_type="sticker")
        _cache("sticker_20260904_100000_ALPHA.png", _photo(2000, 2000, mode="RGBA", fmt="PNG"))
        blocks = media_read.read_media(ALICE, "ALPHA")
        assert blocks[0].mime_type == "image/png"
        with Image.open(io.BytesIO(_payload(blocks[0]))) as image:
            assert image.mode == "RGBA" and image.size == (1568, 1568)

    def test_an_exif_rotated_photo_comes_back_upright(self, paired_dbs, monkeypatch):
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.load_chat_policy({}))
        with paired_dbs.messages() as conn:
            _insert(conn, "ROT")
        # Orientation 6: "rotate 90° clockwise to display", which is what a phone
        # held sideways writes instead of rotating the pixels.
        exif = Image.Exif()
        exif[media_image.ORIENTATION_TAG] = 6
        _cache("image_20260904_100000_ROT.jpg", _photo(400, 200, exif=exif.tobytes()))
        blocks = media_read.read_media(ALICE, "ROT")
        # Landscape on disk, portrait once the tag is applied — and re-encoded
        # even though it fits max_edge, or the tag would be lost with the pixels
        # left as they were.
        assert _size(blocks[0]) == (200, 400)
        meta = _meta(blocks)
        assert (meta["width"], meta["height"], meta["resized"]) == (200, 400, False)

    def test_the_camera_metadata_does_not_travel_with_the_photo(self, photos):
        """GPS, serial numbers and timestamps stay on the server."""
        exif = Image.Exif()
        exif[media_image.ORIENTATION_TAG] = 1
        exif[0x010F] = "SecretCamera"
        _cache("image_20260904_100000_BIGJPG.jpg", _photo(2000, 1000, exif=exif.tobytes()))
        payload = _payload(media_read.read_media(ALICE, "BIGJPG")[0])
        assert b"SecretCamera" not in payload
        with Image.open(io.BytesIO(payload)) as image:
            assert dict(image.getexif()) == {}

    def test_a_small_photo_with_exif_is_re_encoded_rather_than_passed_through(self, paired_dbs, monkeypatch):
        """The cheap path must not become a hole in "metadata is stripped"."""
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.load_chat_policy({}))
        with paired_dbs.messages() as conn:
            _insert(conn, "GPS")
        exif = Image.Exif()
        exif[0x010F] = "SecretCamera"
        small = _photo(400, 300, exif=exif.tobytes())
        assert b"SecretCamera" in small and len(small) < media_image.PASSTHROUGH_MAX_BYTES
        _cache("image_20260904_100000_GPS.jpg", small)
        payload = _payload(media_read.read_media(ALICE, "GPS")[0])
        assert payload != small and b"SecretCamera" not in payload

    def test_a_colour_profile_does_not_survive_the_png_branch_either(self, paired_dbs, monkeypatch):
        """Pillow's PNG writer copies icc_profile from the source image's info."""
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.load_chat_policy({}))
        with paired_dbs.messages() as conn:
            _insert(conn, "ICC", media_type="sticker")
        profile = b"fake icc profile " * 100
        _cache("sticker_20260904_100000_ICC.png", _photo(2000, 1500, mode="RGBA", fmt="PNG", icc_profile=profile))
        payload = _payload(media_read.read_media(ALICE, "ICC")[0])
        assert profile not in payload
        with Image.open(io.BytesIO(payload)) as image:
            assert "icc_profile" not in image.info


class TestConversion:
    @pytest.mark.parametrize(
        ("name", "fmt", "stored_mime"),
        [("scan.tiff", "TIFF", "image/tiff"), ("plan.bmp", "BMP", "image/bmp")],
    )
    def test_a_format_no_client_renders_comes_back_as_an_image(self, paired_dbs, monkeypatch, name, fmt, stored_mime):
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.load_chat_policy({}))
        with paired_dbs.messages() as conn:
            _insert(conn, "CONV", media_type="document", filename=name)
        _cache(f"document_20260904_100000_CONV{os.path.splitext(name)[1]}", _photo(300, 200, fmt=fmt))
        blocks = media_read.read_media(ALICE, "CONV")
        assert blocks[0].type == "image" and blocks[0].mime_type == "image/jpeg"
        meta = _meta(blocks)
        assert meta["mime"] == "image/jpeg" and meta["original_mime"] == stored_mime
        assert (meta["width"], meta["height"]) == (300, 200)

    def test_the_same_file_is_a_resource_with_max_edge_zero(self, paired_dbs, monkeypatch):
        """max_edge=0 means "the bytes as stored", which no client renders — so, a resource."""
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.load_chat_policy({}))
        with paired_dbs.messages() as conn:
            _insert(conn, "CONV", media_type="document", filename="scan.tiff")
        _cache("document_20260904_100000_CONV.tiff", _photo(300, 200, fmt="TIFF"))
        blocks = media_read.read_media(ALICE, "CONV", max_edge=0)
        assert blocks[0].type == "resource" and blocks[0].resource.mime_type == "image/tiff"

    def test_a_heic_photo_is_decoded_and_converted(self, paired_dbs, monkeypatch):
        """The one format Pillow needs a plugin for; the plugin is registered on demand."""
        pillow_heif = pytest.importorskip("pillow_heif")
        pillow_heif.register_heif_opener()
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.load_chat_policy({}))
        monkeypatch.setattr(media_image, "_heif_registered", False)
        with paired_dbs.messages() as conn:
            _insert(conn, "HEIC", media_type="document", filename="IMG_0042.heic")
        _cache("document_20260904_100000_HEIC.heic", _photo(2000, 1000, fmt="HEIF"))
        blocks = media_read.read_media(ALICE, "HEIC")
        assert blocks[0].type == "image" and blocks[0].mime_type == "image/jpeg"
        meta = _meta(blocks)
        # The type came from the ISO brand, not from the name (the bridge
        # renames every cached image `.jpg`).
        assert meta["original_mime"] == "image/heic"
        assert (meta["width"], meta["height"], meta["resized"]) == (1568, 784, True)

    def test_the_image_cap_applies_only_where_the_conversion_happens(self):
        """A 9 MB TIFF is a photo when it is downscaled and a blob when it is not.

        Nothing converts it without `max_edge` — a `max_edge=0` read,
        `resources/read`, a `list_media` link — and 12 MB of base64 in a block no
        client renders is the failure this change exists to remove.
        """
        for mime in ("image/tiff", "image/heic", "image/bmp"):
            assert media_read.hard_limit(mime) == media_read.MAX_BASE64_BYTES
            assert media_read.hard_limit(mime, max_edge=1568) == media_read.MAX_IMAGE_BYTES
        # The four a client renders keep the image cap on every path: they are
        # never converted, only ever downscaled.
        assert media_read.hard_limit("image/jpeg") == media_read.MAX_IMAGE_BYTES

    def test_a_tiff_over_the_blob_cap_is_refused_with_max_edge_zero(self, paired_dbs, monkeypatch):
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.load_chat_policy({}))
        monkeypatch.setattr(media_read, "MAX_BASE64_BYTES", 500)
        with paired_dbs.messages() as conn:
            _insert(conn, "BIGTIF", media_type="document", filename="scan.tiff")
        _cache("document_20260904_100000_BIGTIF.tiff", _photo(300, 200, fmt="TIFF"))
        with pytest.raises(ToolError) as exc:
            media_read.read_media(ALICE, "BIGTIF", max_edge=0)
        assert exc.value.code == "too_large"
        # The same file converts fine at the default max_edge.
        assert media_read.read_media(ALICE, "BIGTIF")[0].type == "image"

    def test_an_animated_gif_comes_back_as_its_first_frame(self, paired_dbs, monkeypatch):
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.load_chat_policy({}))
        with paired_dbs.messages() as conn:
            _insert(conn, "ANIM", filename="loop.gif")
        # Visibly different frames: Pillow's GIF writer drops a frame identical
        # to the one before it, and three shades of the default palette are not.
        frames = [Image.new("RGB", (2000, 1000), color=(index * 90, 20, 200)).convert("P") for index in range(3)]
        buffer = io.BytesIO()
        frames[0].save(buffer, format="GIF", save_all=True, append_images=frames[1:])
        _cache("image_20260904_100000_ANIM.gif", buffer.getvalue())
        blocks = media_read.read_media(ALICE, "ANIM")
        assert blocks[0].mime_type == "image/jpeg" and _size(blocks[0]) == (1568, 784)
        # ...and says so, or a still frame reads exactly like a still image.
        assert _meta(blocks)["original_frames"] == 3

    def test_a_still_image_carries_no_frame_count(self, photos):
        assert "original_frames" not in _meta(media_read.read_media(ALICE, "BIGJPG"))


class TestRefusals:
    def test_a_corrupt_image_is_a_clear_error_and_not_a_crash(self, photos):
        """Right magic, broken body: the decoder fails and the agent is told how to proceed."""
        _cache("image_20260904_100000_BIGJPG.jpg", b"\xff\xd8\xff\xe0 truncated before any scan")
        with pytest.raises(ToolError) as exc:
            media_read.read_media(ALICE, "BIGJPG")
        assert exc.value.code == "invalid_argument"
        assert "max_edge=0" in exc.value.message

    def test_a_decompression_bomb_is_refused_before_it_is_decoded(self, photos, monkeypatch):
        monkeypatch.setattr(media_image, "MAX_PIXELS", 1000)
        with pytest.raises(ToolError) as exc:
            media_read.read_media(ALICE, "BIGJPG")
        assert exc.value.code == "too_large"
        assert exc.value.extra == {"pixels": 4000 * 3000, "limit": 1000}

    def test_a_negative_max_edge_is_refused_before_anything_is_read(self, photos, monkeypatch):
        def explode(*_args, **_kwargs):
            raise AssertionError("a bad argument must not cost a database read")

        monkeypatch.setattr(media_read, "resolve_media", explode)
        with pytest.raises(ToolError) as exc:
            media_read.read_media(ALICE, "BIGJPG", max_edge=-5)
        assert exc.value.code == "invalid_argument"

    def test_the_argument_bounds(self):
        assert media_image.edge_limit(0) == 0
        assert media_image.edge_limit(10**9) == media_image.MAX_EDGE_LIMIT
        assert media_image.quality_limit(0) == media_image.DEFAULT_QUALITY
        for bad in (-1, 101):
            with pytest.raises(ToolError):
                media_image.quality_limit(bad)
        with pytest.raises(ToolError):
            media_image.edge_limit("big")


class TestTool:
    def test_the_tool_downscales_too(self, photos):
        blocks = main.read_media(chat_jid=ALICE, message_id="BIGJPG")
        assert [block.type for block in blocks] == ["image", "text"]
        assert _size(blocks[0]) == (1568, 1176)

    def test_a_bad_quality_answers_the_error_envelope(self, photos):
        out = main.read_media(chat_jid=ALICE, message_id="BIGJPG", quality=200)
        assert out.is_error is True
        assert out.structured_content["error"]["code"] == "invalid_argument"
