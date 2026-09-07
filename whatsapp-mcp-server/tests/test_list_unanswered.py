"""list_unanswered: chats whose newest stored message is inbound (issue #219).

The backlog list_unread cannot see: read on the phone, never replied to.
"""

import sqlite3
from datetime import datetime, timedelta

import pytest

import main
import whatsapp
from chat_policy import ChatPolicy
from errors import ToolError
from tests.conftest import MESSAGES_SCHEMA

WAITING = "5511111111111@s.whatsapp.net"  # read on the phone, never answered
ANSWERED = "5511222222222@s.whatsapp.net"  # I had the last word
REACTED = "5511333333333@s.whatsapp.net"  # I only reacted — still unanswered
GROUP = "120363000000000001@g.us"
FRESH = "5511444444444@s.whatsapp.net"  # they wrote minutes ago


def _stamp(**delta):
    return (datetime.now() - timedelta(**delta)).strftime("%Y-%m-%d %H:%M:%S")


@pytest.fixture
def db(tmp_path, monkeypatch):
    path = tmp_path / "messages.db"
    chats = [
        # jid, name, last_message_time, last_read_time
        (WAITING, "Alice", _stamp(days=2), _stamp(days=2)),  # marker past it: "read"
        (ANSWERED, "Bob", _stamp(days=3), None),
        (REACTED, "Carla", _stamp(days=4), _stamp(days=4)),
        (GROUP, "Neighbourhood", _stamp(days=5), None),
        (FRESH, "Dan", _stamp(minutes=30), None),
    ]
    messages = [
        # id, chat, timestamp, is_from_me, media_type, deleted_at
        ("w1", WAITING, _stamp(days=9), 1, None, None),
        ("w2", WAITING, _stamp(days=2), 0, None, None),
        ("b1", ANSWERED, _stamp(days=4), 0, None, None),
        ("b2", ANSWERED, _stamp(days=3), 1, None, None),
        ("c1", REACTED, _stamp(days=4), 0, None, None),
        ("c2", REACTED, _stamp(days=3), 1, "reaction", None),  # a reaction is not an answer
        ("c3", REACTED, _stamp(days=2), 1, None, _stamp(days=2)),  # revoked: not said any more
        ("g1", GROUP, _stamp(days=5), 0, None, None),
        ("d1", FRESH, _stamp(minutes=30), 0, None, None),
    ]
    with sqlite3.connect(path) as c:
        c.executescript(MESSAGES_SCHEMA)
        c.execute("ALTER TABLE chats ADD COLUMN last_read_time TIMESTAMP")
        c.executemany("INSERT INTO chats VALUES (?, ?, ?, ?)", chats)
        c.executemany(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, deleted_at)"
            " VALUES (?,?,?,?,?,?,?,?)",
            [(mid, chat, chat.split("@")[0], mid, ts, me, media, gone) for mid, chat, ts, me, media, gone in messages],
        )
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", str(tmp_path / "absent.db"))
    whatsapp._reset_schema_cache()
    whatsapp._reset_name_cache()
    return path


def _jids(result):
    return [item["jid"] for item in result["items"]]


def test_lists_chats_where_they_spoke_last(db):
    out = main.list_unanswered()
    assert _jids(out) == [FRESH, WAITING, REACTED, GROUP]  # newest inbound first
    assert ANSWERED not in _jids(out)  # I had the last word

    waiting = next(item for item in out["items"] if item["jid"] == WAITING)
    # Read on the phone, so list_unread would not show it: that is the point.
    assert waiting["unread"] is False
    assert waiting["last_message"] == "w2" and waiting["last_sender"] == WAITING.split("@")[0]
    assert waiting["last_inbound_time"] == waiting["last_message_time"].replace("T", " ")
    assert 47 < waiting["age_hours"] < 49


def test_reactions_and_revoked_messages_are_not_answers(db):
    """My reaction and my deleted message do not count as having replied."""
    assert REACTED in _jids(main.list_unanswered())


def test_exclude_groups(db):
    out = main.list_unanswered(exclude_groups=True)
    assert GROUP not in _jids(out)
    assert _jids(out) == [FRESH, WAITING, REACTED]


def test_min_age_hours_skips_live_conversations(db):
    assert FRESH not in _jids(main.list_unanswered(min_age_hours=24))
    assert _jids(main.list_unanswered(min_age_hours=24)) == [WAITING, REACTED, GROUP]
    # REACTED's last inbound is 4 days old, WAITING's only 2.
    assert _jids(main.list_unanswered(min_age_hours=72)) == [REACTED, GROUP]
    assert main.list_unanswered(min_age_hours=-1)["error"]["code"] == "invalid_argument"


def test_since_bounds_the_last_inbound_message(db):
    recent = (datetime.now() - timedelta(days=3)).isoformat()
    assert _jids(main.list_unanswered(since=recent)) == [FRESH, WAITING]
    assert main.list_unanswered(since="yesterday")["error"]["code"] == "invalid_argument"


def test_cursor_pagination_walks_every_chat_once(db):
    seen, cursor, pages = [], None, 0
    while True:
        page = main.list_unanswered(limit=2, cursor=cursor)
        seen.extend(_jids(page))
        pages += 1
        cursor = page["next_cursor"]
        if not cursor:
            assert page["has_more"] is False
            break
        assert pages < 10
    assert seen == [FRESH, WAITING, REACTED, GROUP]
    assert pages == 2

    assert main.list_unanswered(cursor="not-a-cursor")["error"]["code"] == "invalid_argument"
    # a cursor from another list tool is refused rather than silently misread
    other = main.list_chats(limit=1)["next_cursor"]
    assert main.list_unanswered(cursor=other)["error"]["code"] == "invalid_argument"


def test_include_last_message_false_drops_the_content(db):
    item = main.list_unanswered(include_last_message=False)["items"][0]
    assert item["last_message"] is None and item["last_sender"] is None
    assert item["last_is_from_me"] == 0 and item["last_inbound_time"] is not None


def test_respects_the_allow_list(db, monkeypatch):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["5511111111111"]))
    assert _jids(whatsapp.list_unanswered_page().to_dict()) == [WAITING]


def test_db_error_is_internal(db, monkeypatch):
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(db.parent / "missing" / "x.db"))
    whatsapp._reset_schema_cache()
    with pytest.raises(ToolError) as exc:
        whatsapp.list_unanswered()
    assert exc.value.code == "internal"


def test_age_hours_and_min_age_hours_use_one_clock(db):
    """A stored UTC offset must not make the reported age disagree with the filter (#257)."""
    odd = "5511555555555@s.whatsapp.net"
    with sqlite3.connect(db) as c:
        c.execute("INSERT INTO chats VALUES (?, 'Eve', NULL, NULL)", (odd,))
        c.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) "
            "VALUES ('e1', ?, 'eve', 'oi', ?, 0)",
            (odd, _stamp(hours=3) + "+05:00"),
        )
    item = next(i for i in main.list_unanswered()["items"] if i["jid"] == odd)
    assert 2.9 < item["age_hours"] < 3.1  # local wall time, the offset is not re-applied
    assert odd in _jids(main.list_unanswered(min_age_hours=2))  # ...and the SQL bound agrees
    assert odd not in _jids(main.list_unanswered(min_age_hours=4))
