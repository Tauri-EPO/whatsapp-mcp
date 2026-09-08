"""coverage(): archive boundaries, per-month counts and sync gaps from messages.db."""

import os
from pathlib import Path

import pytest

import media_inventory
import media_notes
import whatsapp
from chat_policy import ChatPolicy
from errors import ToolError
from tests.conftest import ALICE, BOB, DECOY, FAMILY

# Dense June, then a 14-day hole (2026-07-22 -> 2026-08-05), then July/August
# again. DECOY has metadata but no message at all.
MESSAGES = [
    ("m1", ALICE, "2026-06-07 09:00:00"),
    ("m2", BOB, "2026-06-07 10:30:00"),
    ("m3", FAMILY, "2026-06-08 11:00:00"),
    ("m4", ALICE, "2026-07-22 08:00:00"),
    ("m5", BOB, "2026-08-05 08:00:00"),
    ("m6", ALICE, "2026-08-05 20:00:00"),
]


@pytest.fixture
def archive(paired_dbs):
    with paired_dbs.messages() as conn:
        conn.executemany(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?, ?, ?, 'hi', ?, 0)",
            [(mid, chat, chat.split("@")[0], ts) for mid, chat, ts in MESSAGES],
        )
    return paired_dbs


def test_coverage_reports_boundaries_months_and_chats(archive):
    result = whatsapp.coverage()

    assert result["first_message_time"].startswith("2026-06-07 09:00:00")
    assert result["last_message_time"].startswith("2026-08-05 20:00:00")
    assert result["total_messages"] == 6
    assert result["chats_total"] == 4  # ALICE, BOB, FAMILY, DECOY
    assert result["chats_with_messages"] == 3
    assert result["chats_without_messages"] == 1
    assert result["messages_by_month"] == {"2026-06": 3, "2026-07": 1, "2026-08": 2}
    assert result["allow_list_applied"] is False
    assert "request_history" in result["hint"]


def test_coverage_finds_the_synthetic_hole(archive):
    result = whatsapp.coverage()

    assert result["gap_hours"] == 24.0
    assert result["gaps_truncated"] is False
    # Biggest first: the 44 days between June and July, the 14-day hole, then
    # the 24.5 h between the last June 7 message and June 8.
    assert [gap["from"][:10] for gap in result["gaps"]] == ["2026-06-08", "2026-07-22", "2026-06-07"]
    hole = result["gaps"][1]
    assert hole["from"].startswith("2026-07-22 08:00:00")
    assert hole["to"].startswith("2026-08-05 08:00:00")
    assert hole["hours"] == pytest.approx(336.0, abs=0.1)
    # The 1.5 h between the two June 7 messages, and the 12 h on August 5, are not gaps.
    assert all(gap["hours"] > 24 for gap in result["gaps"])


def test_coverage_gap_threshold_and_max_gaps(archive):
    hourly = whatsapp.coverage(gap_hours=1)
    assert len(hourly["gaps"]) == len(MESSAGES) - 1  # every consecutive pair is more than an hour apart
    assert hourly["gaps"][-1]["hours"] == pytest.approx(1.5, abs=0.01)

    capped = whatsapp.coverage(gap_hours=1, max_gaps=2)
    assert len(capped["gaps"]) == 2 and capped["gaps_truncated"] is True

    huge = whatsapp.coverage(gap_hours=24 * 365)
    assert huge["gaps"] == []


def test_coverage_honours_the_allow_list(archive, monkeypatch):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([ALICE]))
    result = whatsapp.coverage()

    assert result["allow_list_applied"] is True
    assert result["total_messages"] == 3
    assert result["chats_total"] == 1 and result["chats_without_messages"] == 0
    assert result["messages_by_month"] == {"2026-06": 1, "2026-07": 1, "2026-08": 1}
    # Alice's own gaps, not the archive-wide ones.
    assert result["gaps"][0]["from"].startswith("2026-06-07")


def test_coverage_empty_archive(paired_dbs):
    result = whatsapp.coverage()

    assert result["first_message_time"] is None and result["last_message_time"] is None
    assert result["total_messages"] == 0
    assert result["chats_with_messages"] == 0 and result["chats_without_messages"] == 4
    assert result["messages_by_month"] == {} and result["gaps"] == []


def test_coverage_rejects_bad_arguments(paired_dbs):
    for bad in (0, -1, "soon"):
        with pytest.raises(ToolError) as exc:
            whatsapp.coverage(gap_hours=bad)  # type: ignore[arg-type]
        assert exc.value.code == "invalid_argument"
    with pytest.raises(ToolError):
        whatsapp.coverage(max_gaps="all")  # type: ignore[arg-type]


def test_coverage_window_scopes_counts_and_gaps(archive):
    result = whatsapp.coverage(after="2026-07-01")

    assert result["total_messages"] == 3  # m4, m5, m6
    assert result["first_message_time"].startswith("2026-07-22 08:00:00")
    assert result["last_message_time"].startswith("2026-08-05 20:00:00")
    assert result["messages_by_month"] == {"2026-07": 1, "2026-08": 2}
    # FAMILY and DECOY hold nothing inside the window, so they count as missing.
    assert result["chats_with_messages"] == 2 and result["chats_without_messages"] == 2
    # The June artefacts are outside the window; what is left is the 14-day
    # hole and the three empty weeks between the bound and the first message.
    assert [(gap["from"][:10], gap["to"][:10]) for gap in result["gaps"]] == [
        ("2026-07-01", "2026-07-22"),
        ("2026-07-22", "2026-08-05"),
    ]
    assert result["gaps"][1]["hours"] == pytest.approx(336.0, abs=0.1)
    assert result["scope"] == {"after": "2026-07-01 00:00:00+00:00", "before": None, "chat_jid": None}

    bounded = whatsapp.coverage(after="2026-07-01", before="2026-08-05 12:00:00")
    assert bounded["total_messages"] == 2 and bounded["messages_by_month"] == {"2026-07": 1, "2026-08": 1}


def test_coverage_window_inside_an_outage_reports_the_whole_window(archive):
    """A window with no message in it is one gap, not an empty gap list.

    Pairing stored messages alone can only see holes *between* two of them, so
    a window that falls entirely inside the 14-day outage would answer "no
    gaps" about a period holding nothing at all.
    """
    result = whatsapp.coverage(after="2026-07-23", before="2026-08-04")

    assert result["total_messages"] == 0
    assert len(result["gaps"]) == 1
    assert result["gaps"][0]["from"].startswith("2026-07-23")
    assert result["gaps"][0]["to"].startswith("2026-08-04")
    assert result["gaps"][0]["hours"] == pytest.approx(288.0, abs=0.1)


def test_coverage_gaps_are_unchanged_without_a_window(archive):
    """The bounds are the only new points: an unbounded scan still pairs messages only."""
    assert [gap["from"][:10] for gap in whatsapp.coverage()["gaps"]] == [
        "2026-06-08",
        "2026-07-22",
        "2026-06-07",
    ]


def test_coverage_hint_stops_claiming_never_synced_under_a_window(archive):
    """The unbounded reading of first_message_time is false once a bound is set."""
    unbounded = whatsapp.coverage()
    assert "were never synced either" in unbounded["hint"]

    windowed = whatsapp.coverage(after="2026-07-01")
    assert "were never synced either" not in windowed["hint"]
    assert "anything before first_message_time" not in windowed["hint"]
    assert "describe that window alone" in windowed["hint"]
    assert "request_history" in windowed["hint"]


def test_coverage_per_chat_scopes_everything(archive):
    result = whatsapp.coverage(chat_jid=ALICE)

    assert result["total_messages"] == 3
    assert result["first_message_time"].startswith("2026-06-07 09:00:00")
    assert result["last_message_time"].startswith("2026-08-05 20:00:00")
    assert result["chats_total"] == 1 and result["chats_without_messages"] == 0
    # Alice's own silences, not the archive's.
    assert [gap["from"][:10] for gap in result["gaps"]] == ["2026-06-07", "2026-07-22"]
    assert result["scope"]["chat_jid"] and ALICE in result["scope"]["chat_jid"]

    both = whatsapp.coverage(chat_jid=[ALICE, FAMILY])
    assert both["total_messages"] == 4 and both["chats_total"] == 2


def test_coverage_by_chat_orders_the_backfill_queue(archive):
    result = whatsapp.coverage(by_chat=True)

    assert result["by_chat"] is True and result["chats_total"] == 4
    assert result["has_more"] is False and result["next_cursor"] is None
    # No messages first, then the stub-only chat, then by first_message_time desc.
    assert [item["chat_jid"] for item in result["items"]] == [DECOY, FAMILY, BOB, ALICE]
    decoy, family, _bob, alice = result["items"]
    assert decoy["messages"] == 0 and decoy["stub_only"] is False
    assert decoy["first_message_time"] is None and decoy["last_message_time"] is None
    assert family["messages"] == 1 and family["stub_only"] is True and family["name"] == "Family"
    assert alice["messages"] == 3 and alice["stub_only"] is False
    assert alice["first_message_time"].startswith("2026-06-07 09:00:00")
    assert alice["last_message_time"].startswith("2026-08-05 20:00:00")
    assert "request_history" in result["hint"]


def test_coverage_by_chat_applies_the_window(archive):
    result = whatsapp.coverage(by_chat=True, after="2026-07-01")

    by_jid = {item["chat_jid"]: item for item in result["items"]}
    assert by_jid[FAMILY]["messages"] == 0  # its only message is before the window
    assert by_jid[ALICE]["first_message_time"].startswith("2026-07-22")
    assert by_jid[ALICE]["messages"] == 2
    # A window narrows the counts but cannot invent a stub: Bob has two stored
    # messages and only one inside it, which is not "never synced".
    assert by_jid[BOB]["messages"] == 1 and by_jid[BOB]["stub_only"] is False
    # ...while the chat that really is a stub keeps the flag even though the
    # window empties it.
    assert by_jid[FAMILY]["stub_only"] is True
    # The order is about the whole history, so the window does not move it.
    assert [item["chat_jid"] for item in result["items"]] == [DECOY, FAMILY, BOB, ALICE]


def test_coverage_by_chat_ranks_on_the_whole_history_not_the_window(paired_dbs):
    """A window scopes the counts; it must not decide who needs backfilling.

    Alice synced back to 2019, Bob's history starts this month and Family's
    starts later still, so Family is the one missing the most. The three chats
    are laid out so that both wrong rankings answer something else: ordering by
    the in-window first message gives Alice, Bob, Family, and ordering by a raw
    message count instead of the saturating "none / one / more" probe gives
    Bob, Family, Alice.
    """
    rows = [(f"a{i}", ALICE, f"2019-01-{i + 1:02d} 09:00:00") for i in range(20)]
    rows += [
        ("a99", ALICE, "2026-09-06 12:00:00"),  # latest in-window first message
        ("b1", BOB, "2026-09-01 09:00:00"),  # second-latest overall start
        ("b2", BOB, "2026-09-06 10:00:00"),
        ("b3", BOB, "2026-09-06 11:00:00"),
        ("f1", FAMILY, "2026-09-03 08:00:00"),  # latest overall start
        ("f2", FAMILY, "2026-09-06 07:00:00"),
        ("f3", FAMILY, "2026-09-06 08:00:00"),
        ("f4", FAMILY, "2026-09-06 09:00:00"),
        ("f5", FAMILY, "2026-09-07 08:00:00"),
    ]
    with paired_dbs.messages() as conn:
        conn.executemany(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?, ?, ?, 'hi', ?, 0)",
            [(mid, chat, chat.split("@")[0], ts) for mid, chat, ts in rows],
        )

    result = whatsapp.coverage(by_chat=True, after="2026-09-05", chat_jid=[ALICE, BOB, FAMILY])
    by_jid = {item["chat_jid"]: item for item in result["items"]}
    assert [item["chat_jid"] for item in result["items"]] == [FAMILY, BOB, ALICE]
    # Alice's 21 stored messages and Family's 5 rank alike — both are "more
    # than a stub" — and only where their history starts separates them.
    assert by_jid[ALICE]["messages"] == 1 and by_jid[ALICE]["stub_only"] is False
    assert by_jid[FAMILY]["messages"] == 4 and by_jid[BOB]["messages"] == 2


def test_coverage_by_chat_cursor_ignores_the_order_of_the_chat_list(archive):
    page = whatsapp.coverage(by_chat=True, limit=1, chat_jid=[ALICE, BOB])
    resumed = whatsapp.coverage(by_chat=True, limit=1, chat_jid=[BOB, ALICE], cursor=page["next_cursor"])

    assert [item["chat_jid"] for item in resumed["items"]] == [ALICE]


def test_coverage_by_chat_pages_with_a_cursor(archive):
    first = whatsapp.coverage(by_chat=True, limit=2)
    assert [item["chat_jid"] for item in first["items"]] == [DECOY, FAMILY]
    assert first["has_more"] is True and first["next_cursor"]

    second = whatsapp.coverage(by_chat=True, limit=2, cursor=first["next_cursor"])
    assert [item["chat_jid"] for item in second["items"]] == [BOB, ALICE]
    assert second["has_more"] is False and second["next_cursor"] is None

    # A cursor cannot be replayed against another scope, which would skip chats.
    with pytest.raises(ToolError) as exc:
        whatsapp.coverage(by_chat=True, limit=2, cursor=first["next_cursor"], after="2026-07-01")
    assert exc.value.code == "invalid_argument"

    with pytest.raises(ToolError) as exc:
        whatsapp.coverage(cursor=first["next_cursor"])
    assert exc.value.code == "invalid_argument"


def test_coverage_by_chat_rejects_a_mangled_cursor_offset(archive):
    """A cursor is caller-supplied: a bad offset is invalid_argument, not internal."""
    scope = {"after": None, "before": None, "chat_jid": None}
    fingerprint = whatsapp._coverage_fingerprint(scope)
    for bad in ("2", -1, {"a": 1}, None):
        cursor = whatsapp.encode_cursor({"k": "coverage_by_chat", "o": bad, "s": fingerprint})
        with pytest.raises(ToolError) as exc:
            whatsapp.coverage(by_chat=True, cursor=cursor)
        assert exc.value.code == "invalid_argument"


def test_coverage_validates_limit_even_without_by_chat(archive):
    """The one argument that would otherwise vanish silently along with by_chat."""
    for bad in ("lots", None):
        with pytest.raises(ToolError) as exc:
            whatsapp.coverage(limit=bad)  # type: ignore[arg-type]
        assert exc.value.code == "invalid_argument"


def test_coverage_by_chat_honours_the_allow_list(archive, monkeypatch):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([ALICE]))
    result = whatsapp.coverage(by_chat=True)

    assert [item["chat_jid"] for item in result["items"]] == [ALICE]
    assert result["chats_total"] == 1 and result["allow_list_applied"] is True

    with pytest.raises(ToolError) as exc:
        whatsapp.coverage(chat_jid=BOB)
    assert exc.value.code == "denied"


def test_coverage_tool_passes_arguments(monkeypatch):
    import main

    monkeypatch.setattr(main, "whatsapp_coverage", lambda **kwargs: kwargs)
    assert main.coverage() == {
        "gap_hours": 24.0,
        "max_gaps": 20,
        "after": None,
        "before": None,
        "chat_jid": None,
        "by_chat": False,
        "cursor": None,
        "limit": 50,
    }
    passed = main.coverage(6, 5, after="2026-07-01", chat_jid=[ALICE], by_chat=True, limit=10)
    assert passed["gap_hours"] == 6 and passed["max_gaps"] == 5
    assert passed["after"] == "2026-07-01" and passed["chat_jid"] == [ALICE]
    assert passed["by_chat"] is True and passed["limit"] == 10


def test_coverage_database_error_is_internal(monkeypatch, paired_dbs):
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(paired_dbs.messages_db) + "/missing.db")
    with pytest.raises(ToolError) as exc:
        whatsapp.coverage()
    assert exc.value.code == "internal"


# --- the audio block (issue #334) ---------------------------------------------
#
# Alice has four inbound voice notes, three of them cached; Bob has one, never
# cached. One of Alice's carries a transcript note, another a transcript_error.
# An outbound voice note and a deleted one are in the table so the counts have
# something to exclude.
AUDIO = [
    ("a1", ALICE, "2026-06-07 09:05:00", 0, None, True),
    ("a2", ALICE, "2026-07-22 08:05:00", 0, None, True),
    ("a3", ALICE, "2026-08-05 08:05:00", 0, None, True),
    ("a4", ALICE, "2026-08-05 09:05:00", 0, None, False),
    ("b1", BOB, "2026-08-05 10:05:00", 0, None, False),
    ("o1", ALICE, "2026-08-05 11:05:00", 1, None, True),
    ("d1", ALICE, "2026-08-05 12:05:00", 0, "2026-08-06 10:00:00", True),
]
AUDIO_SHA = {message_id: f"{index + 1:02x}" * 32 for index, (message_id, *_rest) in enumerate(AUDIO)}


def _audio_filename(message_id: str) -> str:
    """The bridge's name for a cached voice note: audio_<date>_<time>_<id>.ogg."""
    return f"audio_20260805_0900_{message_id}.ogg"


@pytest.fixture
def audio_archive(archive):
    """The text archive plus the voice notes, their bytes and two notes."""
    with archive.messages() as conn:
        conn.executemany(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, "
            "file_sha256, filename, deleted_at) VALUES (?, ?, ?, '', ?, ?, 'audio', ?, ?, ?)",
            [
                (
                    message_id,
                    chat,
                    chat.split("@")[0],
                    timestamp,
                    from_me,
                    bytes.fromhex(AUDIO_SHA[message_id]),
                    _audio_filename(message_id),
                    deleted_at,
                )
                for message_id, chat, timestamp, from_me, deleted_at, _cached in AUDIO
            ],
        )
    for message_id, chat, _ts, _from_me, _deleted, cached in AUDIO:
        if not cached:
            continue
        directory = media_inventory.chat_media_dir(chat)
        os.makedirs(directory, exist_ok=True)
        Path(directory, _audio_filename(message_id)).write_bytes(b"opus")
    media_inventory.forget_cached_names()
    media_notes.annotate_media(AUDIO_SHA["a1"], media_notes.TRANSCRIPT_KEY, "oi tudo bem")
    media_notes.annotate_media(AUDIO_SHA["a2"], media_notes.TRANSCRIPT_ERROR_KEY, "ffmpeg said no")
    return archive


def test_coverage_audio_counts_the_backlog(audio_archive):
    audio = whatsapp.coverage()["audio"]

    # a1..a4 and b1: the outbound one and the deleted one are not work.
    assert audio["messages"] == 5
    assert audio["cached"] == 3  # a1, a2, a3
    assert audio["cached_examined"] == 5  # nothing was cut, so the cached counts are exact
    assert audio["transcribed"] == 1
    assert audio["errors"] == 1
    assert audio["backlog"] == 3  # a3, a4, b1
    # Only a3: a1 and a2 are on disk but already handled, a4 and b1 are not on disk.
    assert audio["backlog_cached"] == 1


def test_coverage_audio_ignores_rows_without_a_content_hash(audio_archive):
    """No hash, no transcript note ever: counting it would leave a backlog nothing can drain."""
    with audio_archive.messages() as conn:
        conn.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type) "
            "VALUES ('nohash', ?, ?, '', '2026-08-05 13:05:00', 0, 'audio')",
            (ALICE, ALICE),
        )
    audio = whatsapp.coverage()["audio"]
    assert audio["messages"] == 5 and audio["backlog"] == 3


def test_coverage_audio_is_scoped_by_the_window(audio_archive):
    audio = whatsapp.coverage(after="2026-08-01")["audio"]

    assert audio["messages"] == 3  # a3, a4, b1
    assert audio["cached"] == 1 and audio["cached_examined"] == 3
    assert audio["transcribed"] == 0 and audio["errors"] == 0
    assert audio["backlog"] == 3 and audio["backlog_cached"] == 1


def test_coverage_audio_is_scoped_by_chat(audio_archive):
    assert whatsapp.coverage(chat_jid=BOB)["audio"] == {
        "messages": 1,
        "cached": 0,
        "cached_examined": 1,
        "transcribed": 0,
        "errors": 0,
        "unavailable": 0,
        "backlog": 1,
        "backlog_cached": 0,
    }


def test_coverage_audio_honours_the_allow_list(audio_archive, monkeypatch):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([BOB]))
    audio = whatsapp.coverage()["audio"]

    assert audio["messages"] == 1 and audio["cached"] == 0
    assert audio["transcribed"] == 0


def test_coverage_audio_without_a_notes_db(archive):
    """No transcription has ever run here: every hash is untranscribed, nothing errors."""
    with archive.messages() as conn:
        conn.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, file_sha256) "
            "VALUES ('a1', ?, ?, '', '2026-08-05 08:05:00', 0, 'audio', ?)",
            (ALICE, ALICE, bytes.fromhex("aa" * 32)),
        )
    assert not os.path.exists(media_notes.notes_db_path())
    assert whatsapp.coverage()["audio"] == {
        "messages": 1,
        "cached": 0,
        "cached_examined": 1,
        "transcribed": 0,
        "errors": 0,
        "unavailable": 0,
        "backlog": 1,
        "backlog_cached": 0,
    }


def test_coverage_audio_on_an_archive_without_voice_notes(archive):
    assert whatsapp.coverage()["audio"]["messages"] == 0
    assert whatsapp.coverage()["audio"]["backlog"] == 0


def test_coverage_hint_names_the_backlog_only_when_there_is_one(audio_archive):
    hint = whatsapp.coverage()["hint"]
    assert "3 of the 5 voice notes" in hint and "1 of them with their bytes" in hint
    assert "TRANSCRIBE_ON_INGEST" in hint
    # Everything transcribed: the sentence would be noise.
    for message_id in ("a3", "a4", "b1"):
        media_notes.annotate_media(AUDIO_SHA[message_id], media_notes.TRANSCRIPT_KEY, "ok")
    assert "voice notes" not in whatsapp.coverage()["hint"]


def test_coverage_audio_bounds_the_cache_scan(audio_archive, monkeypatch):
    """With the row ceiling at 1, the cached counts are floors over the newest row."""
    monkeypatch.setattr(whatsapp, "COVERAGE_AUDIO_MAX_ROWS", 1)
    audio = whatsapp.coverage()["audio"]

    assert audio["messages"] == 5  # the counts stay exact
    assert audio["cached_examined"] == 1  # b1, the newest, is not cached
    assert audio["cached"] == 0 and audio["backlog_cached"] == 0
    assert audio["transcribed"] == 1 and audio["errors"] == 1


def test_coverage_audio_bounds_the_directories_it_reads(audio_archive, monkeypatch):
    """With the chat ceiling at 1, only the busiest chat's directory is read."""
    monkeypatch.setattr(whatsapp, "COVERAGE_AUDIO_MAX_CHATS", 1)
    audio = whatsapp.coverage()["audio"]

    assert audio["messages"] == 5
    assert audio["cached_examined"] == 4 and audio["cached"] == 3  # Alice's four rows, Bob's dropped


def test_coverage_audio_reads_each_chat_directory_once(audio_archive, monkeypatch):
    """One listing per chat with audio in scope — never one per row (issue #318)."""
    listed: list[str] = []
    real = media_inventory.list_chat_names

    def counting(chat_jid: str) -> dict[str, str]:
        listed.append(chat_jid)
        return real(chat_jid)

    monkeypatch.setattr(media_inventory, "list_chat_names", counting)
    monkeypatch.setattr(media_inventory, "cached_names", counting)
    whatsapp.coverage()
    assert sorted(listed) == sorted({ALICE, BOB})


def test_coverage_audio_counts_a_row_with_both_notes_once(audio_archive):
    """A hash the worker failed on and an agent then transcribed by hand carries both notes."""
    media_notes.annotate_media(AUDIO_SHA["a3"], media_notes.TRANSCRIPT_KEY, "oi")
    media_notes.annotate_media(AUDIO_SHA["a3"], media_notes.TRANSCRIPT_ERROR_KEY, "ffmpeg said no")
    audio = whatsapp.coverage()["audio"]

    assert audio["transcribed"] == 2 and audio["errors"] == 2  # a3 is in both
    # ... but it is one row: a4 and b1 are what is left, not one of them.
    assert audio["backlog"] == 2


def test_coverage_audio_spends_the_row_ceiling_on_the_backlog(audio_archive, monkeypatch):
    """The newest rows are the transcribed ones; the ceiling must not go on them.

    The ingest worker transcribes newest-first, so on a real backlog the newest
    voice notes are exactly the ones already done. Ordering the scan by time
    alone would report backlog_cached: 0 on an archive whose backlog is on disk.
    """
    with audio_archive.messages() as conn:
        conn.executemany(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, "
            "file_sha256, filename) VALUES (?, ?, ?, '', ?, 0, 'audio', ?, ?)",
            [
                (message_id, ALICE, ALICE, timestamp, bytes.fromhex(sha), _audio_filename(message_id))
                for message_id, timestamp, sha in [
                    ("n1", "2026-09-01 08:00:00", "f1" * 32),
                    ("n2", "2026-09-01 09:00:00", "f2" * 32),
                ]
            ],
        )
    directory = media_inventory.chat_media_dir(ALICE)
    for message_id, sha in (("n1", "f1" * 32), ("n2", "f2" * 32)):
        Path(directory, _audio_filename(message_id)).write_bytes(b"opus")
        media_notes.annotate_media(sha, media_notes.TRANSCRIPT_KEY, "ok")
    media_inventory.forget_cached_names()

    monkeypatch.setattr(whatsapp, "COVERAGE_AUDIO_MAX_ROWS", 3)
    audio = whatsapp.coverage()["audio"]

    assert audio["messages"] == 7 and audio["backlog"] == 3  # a3, a4, b1
    # The three examined rows are the backlog, not n2/n1/b1: a3's bytes are here.
    assert audio["cached_examined"] == 3
    assert audio["backlog_cached"] == 1 and audio["cached"] == 1


def test_coverage_by_chat_has_no_audio_block(audio_archive):
    """The per-chat queue answers a different question and pays for no scan."""
    assert "audio" not in whatsapp.coverage(by_chat=True)
