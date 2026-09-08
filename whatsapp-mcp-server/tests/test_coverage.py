"""coverage(): archive boundaries, per-month counts and sync gaps from messages.db."""

import pytest

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
    # Only the 14-day hole survives; the June artefacts are outside the window.
    assert len(result["gaps"]) == 1
    assert result["gaps"][0]["hours"] == pytest.approx(336.0, abs=0.1)
    assert result["scope"] == {"after": "2026-07-01 00:00:00+00:00", "before": None, "chat_jid": None}

    bounded = whatsapp.coverage(after="2026-07-01", before="2026-08-05 12:00:00")
    assert bounded["total_messages"] == 2 and bounded["messages_by_month"] == {"2026-07": 1, "2026-08": 1}


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

    Alice synced back to 2019 and said one thing this week; Bob's stored
    history only starts this month. Bob is the one missing history, whatever
    window the caller asked about.
    """
    rows = [(f"a{i}", ALICE, f"2019-01-{i + 1:02d} 09:00:00") for i in range(20)]
    rows += [
        ("a99", ALICE, "2026-09-06 09:00:00"),
        ("b1", BOB, "2026-09-01 09:00:00"),
        ("b2", BOB, "2026-09-06 10:00:00"),
        ("b3", BOB, "2026-09-06 11:00:00"),
    ]
    with paired_dbs.messages() as conn:
        conn.executemany(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?, ?, ?, 'hi', ?, 0)",
            [(mid, chat, chat.split("@")[0], ts) for mid, chat, ts in rows],
        )

    result = whatsapp.coverage(by_chat=True, after="2026-09-05", chat_jid=[ALICE, BOB])
    by_jid = {item["chat_jid"]: item for item in result["items"]}
    assert [item["chat_jid"] for item in result["items"]] == [BOB, ALICE]
    # ...even though Alice holds fewer messages inside the window than Bob.
    assert by_jid[ALICE]["messages"] == 1 and by_jid[BOB]["messages"] == 2
    assert by_jid[ALICE]["stub_only"] is False


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
