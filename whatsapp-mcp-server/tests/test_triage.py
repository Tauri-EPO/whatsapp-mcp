"""Triage state (issue #287): handled, snoozed and muted chats leave list_unanswered.

The loop the tool is for: list_unanswered says who is waiting, mark_handled /
snooze record what was decided, and the next run reflects it — until the other
side speaks again, which is the one thing that must always bring a chat back.
"""

import sqlite3
from datetime import UTC, datetime, timedelta

import pytest

import main
import notes
import triage
import whatsapp
from chat_policy import ChatPolicy
from tests.conftest import MESSAGES_SCHEMA

ALICE = "5511111111111@s.whatsapp.net"
BOB = "5511222222222@s.whatsapp.net"
CARLA = "5511333333333@s.whatsapp.net"
VIVO = "5511444444444@s.whatsapp.net"  # the service notice nobody is waiting on
BOB_LID = "231241139937355@lid"


def _stamp(**delta):
    """A past instant in the spelling the bridge stores: UTC, to the second."""
    return (datetime.now(UTC) - timedelta(**delta)).strftime("%Y-%m-%d %H:%M:%S+00:00")


@pytest.fixture
def store(tmp_path, monkeypatch):
    """Four chats, all waiting: they spoke last and nobody answered."""
    path = tmp_path / "messages.db"
    chats = [(ALICE, "Alice"), (BOB, "Bob"), (CARLA, "Carla"), (VIVO, "Vivo")]
    messages = [
        ("a1", ALICE, _stamp(days=2), "preciso do orçamento", None),
        ("b1", BOB, _stamp(days=3), "e aí, decidiu?", None),
        ("c1", CARLA, _stamp(days=4), "Ok", None),
        ("v1", VIVO, _stamp(days=5), "sua fatura chegou", None),
    ]
    with sqlite3.connect(path) as c:
        c.executescript(MESSAGES_SCHEMA)
        c.execute("ALTER TABLE chats ADD COLUMN last_read_time TIMESTAMP")
        c.executemany(
            "INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)",
            [(jid, name, _stamp(days=1)) for jid, name in chats],
        )
        c.executemany(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type)"
            " VALUES (?,?,?,?,?,0,?)",
            [(mid, chat, chat.split("@")[0], text, ts, media) for mid, chat, ts, text, media in messages],
        )
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", str(tmp_path / "absent.db"))
    whatsapp._reset_schema_cache()
    whatsapp._reset_name_cache()
    return path


def _speak(store_path, chat_jid, message_id, at):
    """They write again — the event that must undo `handled_at`."""
    with sqlite3.connect(store_path) as c:
        c.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?,?,?,?,?,0)",
            (message_id, chat_jid, chat_jid.split("@")[0], "ainda estou esperando", at),
        )


def _jids(**kwargs):
    return [item["jid"] for item in main.list_unanswered(**kwargs)["items"]]


def _count(**kwargs):
    return main.list_unanswered(count_only=True, **kwargs)["count"]


class TestMarkHandled:
    def test_handled_chat_leaves_the_list_and_the_count(self, store):
        assert set(_jids()) == {ALICE, BOB, CARLA, VIVO}

        out = main.mark_handled(ALICE)
        assert out["success"] and out["chat_jid"] == ALICE and out["logged"] is False
        assert out["handled_at"].endswith("+00:00")

        assert ALICE not in _jids()
        assert _count() == 3
        # Nothing was hidden from the archive itself.
        assert ALICE in [chat["jid"] for chat in main.list_chats()["items"]]

    def test_a_new_inbound_message_brings_it_back(self, store):
        handled = main.mark_handled(ALICE)["handled_at"]
        assert ALICE not in _jids()

        # Stamped after the mark: that is what "they wrote again" looks like.
        _speak(store, ALICE, "a2", whatsapp.timestamp_bound(whatsapp.parse_db_time(handled) + timedelta(minutes=1)))
        assert ALICE in _jids()
        assert _count() == 4

    def test_an_older_message_does_not(self, store):
        """Only a message the mark did not cover reopens the chat."""
        main.mark_handled(ALICE)
        _speak(store, ALICE, "a0", _stamp(days=9))
        assert ALICE not in _jids()

    def test_hide_handled_false_shows_the_whole_backlog(self, store):
        main.mark_handled(ALICE)
        assert ALICE in _jids(hide_handled=False)
        assert _count(hide_handled=False) == 4

    def test_the_note_is_readable_and_clearable(self, store):
        written = main.mark_handled(ALICE)
        assert main.get_notes("chat", ALICE)["notes"]["handled_at"]["value"] == written["handled_at"]

        main.annotate("chat", ALICE, "handled_at", "")
        assert ALICE in _jids()

    def test_the_optional_note_lands_in_the_log(self, store):
        out = main.mark_handled(ALICE, note="liguei, ela manda a nota")
        assert out["logged"] is True
        log = main.get_notes("chat", ALICE)["notes"]["log"]["value"]
        assert log == f"{out['handled_at'][:10]}: liguei, ela manda a nota"

        main.mark_handled(ALICE, note="segunda tentativa")
        assert main.get_notes("chat", ALICE)["notes"]["log"]["value"].count("\n") == 1

    def test_an_unreadable_note_never_hides_a_chat(self, store):
        """A hand-written handled_at nobody can parse is ignored, not guessed at."""
        main.annotate("chat", ALICE, "handled_at", "ontem à tarde")
        assert ALICE in _jids()

    def test_the_allow_list_is_enforced(self, store, monkeypatch):
        policy = ChatPolicy.from_entries([BOB])
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", policy)
        monkeypatch.setattr(notes, "CHAT_POLICY", policy)
        assert main.mark_handled(ALICE)["error"]["code"] == "denied"
        assert main.snooze(ALICE, "2099-01-01")["error"]["code"] == "denied"


class TestSnooze:
    def test_snoozed_until_the_date_then_back(self, store):
        out = main.snooze(BOB, (datetime.now(UTC) + timedelta(days=2)).date().isoformat())
        assert out["success"] and out["snooze_until"].endswith("00:00:00+00:00")
        assert BOB not in _jids()
        assert _count() == 3

        # The date passes: nothing runs, the chat simply stops being skipped.
        main.annotate("chat", BOB, "snooze_until", whatsapp.timestamp_bound(datetime.now(UTC) - timedelta(minutes=1)))
        assert BOB in _jids()
        assert _count() == 4

    def test_a_message_after_the_snooze_lifts_it(self, store):
        """ "Chase on Thursday" must not bury "urgente, me liga"."""
        main.snooze(BOB, (datetime.now(UTC) + timedelta(days=30)).isoformat())
        assert BOB not in _jids()

        snoozed_at = main.get_notes("chat", BOB)["notes"]["snooze_until"]["updated_at"]
        _speak(store, BOB, "b2", whatsapp.timestamp_bound(whatsapp.parse_db_time(snoozed_at) + timedelta(minutes=1)))
        assert BOB in _jids()

    def test_mark_handled_drops_a_pending_snooze(self, store):
        main.snooze(BOB, (datetime.now(UTC) + timedelta(days=30)).isoformat())
        out = main.mark_handled(BOB)
        assert out["snooze_cleared"] is True
        assert "snooze_until" not in main.get_notes("chat", BOB)["notes"]

        # Without the clearing, this chat would stay hidden for the next month.
        main.annotate("chat", BOB, "handled_at", "")
        assert BOB in _jids()

    def test_nothing_to_clear_is_reported_as_such(self, store):
        assert main.mark_handled(BOB)["snooze_cleared"] is False

    def test_include_snoozed_shows_them_anyway(self, store):
        main.snooze(BOB, (datetime.now(UTC) + timedelta(days=2)).isoformat())
        assert BOB in _jids(include_snoozed=True)

    @pytest.mark.parametrize("until", ["", "quinta-feira", "2026-13-45", "2020-01-01"])
    def test_unusable_dates_are_refused(self, store, until):
        assert main.snooze(BOB, until)["error"]["code"] == "invalid_argument"
        assert BOB in _jids()  # and nothing was written

    def test_a_naive_timestamp_is_read_as_utc(self, store):
        moment = (datetime.now(UTC) + timedelta(hours=6)).replace(tzinfo=None)
        out = main.snooze(BOB, moment.isoformat())
        assert out["snooze_until"] == whatsapp.timestamp_bound(moment)


class TestMute:
    def test_muted_chats_stay_out(self, store):
        main.annotate("chat", VIVO, "mute", "yes")
        assert VIVO not in _jids()
        assert _count() == 3
        assert VIVO in _jids(exclude_muted=False)

    def test_only_a_truthy_value_mutes(self, store):
        main.annotate("chat", VIVO, "mute", "no")
        assert VIVO in _jids()

    def test_a_new_message_does_not_lift_it(self, store):
        """The one filter that is meant to be permanent."""
        main.annotate("chat", VIVO, "mute", "yes")
        _speak(store, VIVO, "v2", whatsapp.timestamp_bound(datetime.now(UTC)))
        assert VIVO not in _jids()

    def test_a_mute_note_does_not_hide_the_chat_from_reads(self, store):
        main.annotate("chat", VIVO, "mute", "true")
        assert VIVO in [chat["jid"] for chat in main.list_chats()["items"]]
        assert main.get_chat(VIVO)["jid"] == VIVO


class TestClosingMessages:
    def test_a_bare_acknowledgement_is_not_someone_waiting(self, store):
        assert CARLA in _jids()  # "Ok" still counts by default
        assert CARLA not in _jids(ignore_closing_messages=True)
        assert _count(ignore_closing_messages=True) == 3

    def test_a_real_message_stays(self, store):
        kept = _jids(ignore_closing_messages=True)
        assert ALICE in kept and BOB in kept

    @pytest.mark.parametrize("text", ["ok", "OK!", " obrigado ", "valeu", "blz", "thanks", "👍", "🙏"])
    def test_the_published_list(self, store, text):
        with sqlite3.connect(store) as c:
            c.execute("UPDATE messages SET content = ? WHERE chat_jid = ?", (text, ALICE))
        assert ALICE not in _jids(ignore_closing_messages=True)

    def test_a_sticker_closes_too(self, store):
        with sqlite3.connect(store) as c:
            c.execute("UPDATE messages SET media_type = 'sticker', content = '' WHERE chat_jid = ?", (ALICE,))
        assert ALICE not in _jids(ignore_closing_messages=True)
        assert ALICE in _jids()


class TestPagination:
    def test_pages_and_count_agree_with_the_filter(self, store):
        main.mark_handled(BOB)
        main.annotate("chat", VIVO, "mute", "yes")

        seen: list[str] = []
        cursor = None
        while True:
            page = main.list_unanswered(limit=1, cursor=cursor)
            seen += [item["jid"] for item in page["items"]]
            cursor = page["next_cursor"]
            if not cursor:
                break
        # Newest inbound first, and a hidden chat never occupies a slot in a page.
        assert seen == [ALICE, CARLA]
        assert _count() == len(seen)


class TestLidSpellings:
    """The chat is stored as `<lid>@lid`; annotate writes its notes under the phone JID."""

    @pytest.fixture(autouse=True)
    def lid_chat(self, paired_dbs):
        with paired_dbs.messages() as c:
            c.execute(
                "INSERT INTO chats (jid, name, last_message_time) VALUES (?, 'Bob', ?)", (BOB_LID, _stamp(days=1))
            )
            c.execute(
                "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)"
                " VALUES ('l1', ?, ?, 'oi', ?, 0)",
                (BOB_LID, BOB_LID.split("@")[0], _stamp(days=1)),
            )
        return paired_dbs

    def _legacy_note(self, key, value, updated_at="2020-01-01T00:00:00+00:00"):
        """A note written under the LID spelling, before the map learned the pair."""
        conn = notes._connect(create=True)
        assert conn is not None
        with conn:
            conn.execute(
                "INSERT INTO notes (target_type, target_id, key, value, updated_at, source, version)"
                " VALUES ('chat', ?, ?, ?, ?, 'legacy', 1)",
                (BOB_LID, key, value, updated_at),
            )
        conn.close()

    def test_a_note_stored_under_the_phone_jid_hides_the_lid_chat(self):
        assert BOB_LID in _jids()

        out = main.mark_handled(BOB_LID)
        assert out["chat_jid"].endswith("@s.whatsapp.net")  # stored under the phone form
        assert BOB_LID not in _jids()

    def test_the_phone_spelling_wins_over_a_note_left_under_the_lid(self):
        """A stale note must not outrank the current one just because rows come back in that order."""
        main.annotate("chat", BOB_LID, "mute", "no")  # canonicalised to the phone JID
        self._legacy_note("mute", "yes")
        assert BOB_LID in _jids()

    def test_clearing_a_note_beats_a_live_row_under_the_older_spelling(self):
        """The delete lands on the phone JID; the chat must come back all the same."""
        self._legacy_note("handled_at", _stamp(minutes=5))
        assert BOB_LID not in _jids()

        assert main.annotate("chat", BOB_LID, "handled_at", "")["deleted"] is True
        assert main.get_notes("chat", BOB_LID)["notes"] == {}
        assert BOB_LID in _jids()


class TestNoNotes:
    def test_an_untouched_store_is_unaffected(self, store):
        """Defaults are on, so this is the guarantee they rest on."""
        assert set(_jids()) == {ALICE, BOB, CARLA, VIVO}
        assert _count() == 4
        assert triage.install_filter(sqlite3.connect(":memory:"), True, True, False) == ("", [])
