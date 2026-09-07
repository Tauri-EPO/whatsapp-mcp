"""coverage(): archive boundaries, per-month counts and sync gaps from messages.db."""

import pytest

import whatsapp
from chat_policy import ChatPolicy
from errors import ToolError
from tests.conftest import ALICE, BOB, FAMILY

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


def test_coverage_tool_passes_arguments(monkeypatch):
    import main

    monkeypatch.setattr(main, "whatsapp_coverage", lambda gap_hours, max_gaps: {"g": gap_hours, "m": max_gaps})
    assert main.coverage() == {"g": 24.0, "m": 20}
    assert main.coverage(6, 5) == {"g": 6, "m": 5}


def test_coverage_database_error_is_internal(monkeypatch, paired_dbs):
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(paired_dbs.messages_db) + "/missing.db")
    with pytest.raises(ToolError) as exc:
        whatsapp.coverage()
    assert exc.value.code == "internal"
