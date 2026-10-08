"""Actual cache sizes, safe paths and bounded pagination through real SQLite."""

import os

import pytest

import main
import media_inventory
import media_read
from errors import ToolError
from tests.conftest import ALICE
from tests.test_media_inventory import _insert


@pytest.mark.parametrize("sort", ["size", "date", "copies"])
def test_min_bytes_uses_cached_size_and_declared_uncached_fallback(paired_dbs, sort):
    directory = media_inventory.chat_media_dir(ALICE)
    os.makedirs(directory)
    values = {"NULL": None, "ZERO": 0, "SMALL": 1, "DECLARED": 9999999, "ABSENT": 9999999, "UNKNOWN": None}
    with paired_dbs.messages() as conn:
        for message_id, declared in values.items():
            _insert(conn, message_id, ALICE, "image", "2026-01-01 00:00:00+00:00", declared, None)
            if message_id in {"ABSENT", "UNKNOWN"}:
                continue
            size = 8 if message_id == "DECLARED" else 2048
            with open(os.path.join(directory, f"image_20260101_000000_{message_id}.jpg"), "wb") as file:
                file.write(b"x" * size)
    result = main.list_media(chat_jid=ALICE, min_bytes=1024, sort=sort)
    items = {item["message_id"]: item for item in result["items"]}
    assert set(items) == {"NULL", "ZERO", "SMALL", "ABSENT"}
    assert all(items[key]["cached_bytes"] == 2048 for key in ("NULL", "ZERO", "SMALL"))
    assert not items["ABSENT"]["cached"] and items["ABSENT"]["bytes"] == 9999999


def test_min_bytes_filtered_prefix_is_bounded_and_cursor_advances(paired_dbs, monkeypatch):
    # A small injected ceiling makes the bound easy to observe. No SQL/cache mock.
    monkeypatch.setattr(media_inventory, "MIN_BYTES_SCAN_LIMIT", 20)
    probes = []
    lookup = media_inventory._CacheIndex.lookup

    def counted_lookup(cache, chat, message_id):
        probes.append(message_id)
        return lookup(cache, chat, message_id)

    monkeypatch.setattr(media_inventory._CacheIndex, "lookup", counted_lookup)
    with paired_dbs.messages() as conn:
        for i in range(63):
            _insert(conn, f"ROW{i:03d}", ALICE, "image", "2026-01-01 00:00:00+00:00", None, None)
        _insert(conn, "TAIL", ALICE, "image", "2026-01-01 00:00:00+00:00", 2048, None)
    # Date sorting ensures the unknown-size prefix precedes the tail.
    cursor = None
    seen = []
    for _ in range(5):
        probes.clear()
        page = main.list_media(chat_jid=ALICE, min_bytes=1024, sort="date", cursor=cursor)
        assert len(probes) <= 20 + len(page["items"])
        seen.extend(item["message_id"] for item in page["items"])
        if not page["has_more"]:
            break
        assert page["next_cursor"] and page["next_cursor"] != cursor
        cursor = page["next_cursor"]
    assert seen == ["TAIL"] and not page["has_more"]


@pytest.mark.parametrize("sort", ["size", "date", "copies"])
@pytest.mark.parametrize("window", [20, 4096])
def test_numbered_pages_skip_qualifying_rows_without_duplicates(paired_dbs, monkeypatch, sort, window):
    monkeypatch.setattr(media_inventory, "MIN_BYTES_SCAN_LIMIT", window)
    directory = media_inventory.chat_media_dir(ALICE)
    os.makedirs(directory)
    with paired_dbs.messages() as conn:
        for i in range(63):
            _insert(conn, f"PREFIX{i:03d}", ALICE, "image", "2026-01-01 00:00:00+00:00", None, None)
        for message_id in ("TAIL1", "TAIL2"):
            _insert(conn, message_id, ALICE, "image", "2026-01-01 00:00:00+00:00", None, None)
            with open(os.path.join(directory, f"image_20260101_000000_{message_id}.jpg"), "wb") as file:
                file.write(b"x" * 2048)
    for page_number, expected in ((0, "TAIL1"), (1, "TAIL2")):
        cursor = None
        for _ in range(5):
            page = main.list_media(chat_jid=ALICE, min_bytes=1024, sort=sort, limit=1, page=page_number, cursor=cursor)
            if page["items"] or not page["has_more"]:
                break
            assert page["next_cursor"] and page["next_cursor"] != cursor
            cursor = page["next_cursor"]
        assert [item["message_id"] for item in page["items"]] == [expected]


@pytest.mark.parametrize("kind", ["file_link", "chat_link", "nested", "directory", "control"])
def test_inventory_refuses_unsafe_cache_entries(paired_dbs, tmp_path, monkeypatch, kind):
    directory = media_inventory.chat_media_dir(ALICE)
    name = "image_20260101_000000_REFUSED.jpg"
    with paired_dbs.messages() as conn:
        _insert(conn, "REFUSED", ALICE, "image", "2026-01-01 00:00:00+00:00", None, None)
    target = tmp_path / "target"
    target.mkdir()
    (target / name).write_bytes(b"x" * 2048)
    try:
        if kind == "chat_link":
            os.symlink(target, directory, target_is_directory=True)
        else:
            os.makedirs(directory)
            path = os.path.join(directory, name)
            if kind == "file_link":
                os.symlink(target / name, path)
            elif kind == "nested":
                os.makedirs(os.path.join(directory, "nested"))
                with open(os.path.join(directory, "nested", name), "wb") as file:
                    file.write(b"x" * 2048)
            elif kind == "directory":
                os.mkdir(path)
            else:
                with open(os.path.join(directory, "image_20260101_000000_REFUSED\n.jpg"), "wb") as file:
                    file.write(b"x" * 2048)
    except OSError as exc:
        if kind in {"file_link", "chat_link", "control"} and os.name == "nt":
            pytest.skip(f"platform cannot create this fixture: {exc}")
        raise
    page = main.list_media(chat_jid=ALICE)
    assert not page["items"][0]["cached"]
    assert main.list_media(chat_jid=ALICE, min_bytes=1024)["items"] == []
    assert media_inventory.scan_chat_cache(ALICE) == {}
    assert media_inventory.lookup_cached_name(ALICE, "REFUSED") is None
    if kind in {"file_link", "chat_link", "directory"}:

        def unexpected_download(*_args, **_kwargs):
            raise AssertionError("an unsafe local cache must be denied before a download")

        monkeypatch.setattr(media_read.whatsapp, "download_media", unexpected_download)
        with pytest.raises(ToolError) as error:
            media_read.read_media(ALICE, "REFUSED")
        assert error.value.code == "denied"


def test_purge_continuation_reaches_bridge_and_preserves_unknown_count(monkeypatch):
    from tests.test_purge_media import BRIDGE_OK, CHAT, _bridge

    calls = _bridge(monkeypatch, {**BRIDGE_OK, "remaining": -1, "next_cursor": "next", "examined": 100000})
    result = main.purge_media(chat_jid=CHAT, cursor="previous", summary_only=True)
    assert calls[0][1] == {"chat_jid": CHAT, "dry_run": True, "cursor": "previous"}
    assert result["remaining"] == -1 and result["next_cursor"] == "next" and result["examined"] == 100000
    assert "items" not in result
    assert (
        main.purge_media(items=[{"message_id": "A", "chat_jid": CHAT}], cursor="previous")["error"]["code"]
        == "invalid_argument"
    )
    assert len(calls) == 1


def test_purge_explicit_item_limit_refuses_before_bridge(monkeypatch):
    from tests.test_purge_media import BRIDGE_OK, CHAT, _bridge

    calls = _bridge(monkeypatch, BRIDGE_OK)
    item = {"message_id": "A", "chat_jid": CHAT}
    result = main.purge_media(items=[item] * 1001)
    assert result["error"]["code"] == "invalid_argument"
    assert "1000" in result["error"]["message"]
    assert calls == []
    assert main.purge_media(items=[item] * 1000)["success"]
    assert len(calls) == 1 and len(calls[0][1]["items"]) == 1000


def test_part_document_inventory_uses_completed_cache_only(paired_dbs):
    with paired_dbs.messages() as conn:
        _insert(conn, "PART1", ALICE, "document", "2026-01-01 00:00:00+00:00", None, None)
        conn.execute("UPDATE messages SET filename='model.part' WHERE id='PART1' AND chat_jid=?", (ALICE,))
    directory = media_inventory.chat_media_dir(ALICE)
    os.makedirs(directory)
    complete = "document_20260101_000000_PART1.part.bin"
    for name in (complete, complete + ".part", "document_20260101_000000_LEGACY.part"):
        with open(os.path.join(directory, name), "wb") as file:
            file.write(b"completed bytes")
    assert set(media_inventory.scan_chat_cache(ALICE)) == {"PART1"}
    assert media_inventory.lookup_cached_name(ALICE, "PART1") == complete
    result = main.list_media(chat_jid=ALICE, min_bytes=1)
    assert len(result["items"]) == 1 and result["items"][0]["cached"]
    assert result["items"][0]["cached_file"] == complete
