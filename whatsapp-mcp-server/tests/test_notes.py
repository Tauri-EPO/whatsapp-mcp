"""notes.db: versioned agent notes about chats, contacts, messages and media."""

import os
import sqlite3

import pytest

import chat_policy
import main
import media_inventory
import media_notes
import notes
import untrusted
import whatsapp
from tests.conftest import ALICE, BOB, BOB_LID, FAMILY

SHA_A = "aa" * 32  # photo in Alice's chat
MSG = f"{ALICE}/MSG1"


@pytest.fixture
def notes_store(paired_dbs):
    with paired_dbs.messages() as c:
        c.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, "
            "file_length, file_sha256) VALUES ('IMG1', ?, 'x', '', '2026-09-01 10:00:00', 0, 'image', 10, ?)",
            (ALICE, bytes.fromhex(SHA_A)),
        )
        c.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) "
            "VALUES ('MSG1', ?, 'x', 'oi', '2026-09-01 11:00:00', 0)",
            (ALICE,),
        )
    return paired_dbs


class TestWriteRead:
    def test_reads_never_create_the_file(self, notes_store):
        assert main.get_notes("chat", ALICE) == {"target_type": "chat", "target_id": ALICE, "notes": {}}
        assert main.search_notes("anything") == []
        assert not os.path.exists(media_notes.notes_db_path())

    @pytest.mark.parametrize(
        ("target_type", "target_id"),
        [("chat", FAMILY), ("contact", BOB), ("message", MSG)],
    )
    def test_write_then_read(self, notes_store, target_type, target_id):
        out = main.annotate(target_type, target_id, "label", "patient")
        assert out["success"] and out["key"] == "label" and out["version"] == 1
        assert "replaced" not in out
        assert out["updated_at"].endswith("+00:00")

        got = main.get_notes(target_type, target_id)
        assert got["target_type"] == target_type and got["target_id"] == target_id
        assert got["notes"] == {"label": {"value": "patient", "updated_at": out["updated_at"]}}

    def test_the_file_lands_in_the_store_in_wal_mode(self, notes_store):
        main.annotate("chat", ALICE, "importance", "5")
        path = media_notes.notes_db_path()
        assert os.path.dirname(path) == media_inventory.media_root()
        with sqlite3.connect(path) as c:
            assert c.execute("PRAGMA journal_mode").fetchone()[0] == "wal"
            assert c.execute("SELECT COUNT(*) FROM notes").fetchone()[0] == 1

    def test_keys_are_independent(self, notes_store):
        main.annotate("chat", ALICE, "label", "family")
        main.annotate("chat", ALICE, "importance", "5")
        assert sorted(main.get_notes("chat", ALICE)["notes"]) == ["importance", "label"]

    def test_targets_do_not_bleed_into_each_other(self, notes_store):
        main.annotate("chat", ALICE, "label", "chat note")
        main.annotate("contact", ALICE, "label", "contact note")
        assert main.get_notes("chat", ALICE)["notes"]["label"]["value"] == "chat note"
        assert main.get_notes("contact", ALICE)["notes"]["label"]["value"] == "contact note"


class TestVersioning:
    def test_a_replaced_value_comes_back_in_the_write(self, notes_store):
        first = main.annotate("contact", BOB, "summary", "accountant")
        second = main.annotate("contact", BOB, "summary", "accountant, chase on the 10th")
        assert second["replaced"] == "accountant"
        assert second["replaced_at"] == first["updated_at"]
        assert second["version"] == 2

    def test_history_keeps_every_version(self, notes_store):
        main.annotate("contact", BOB, "summary", "accountant")
        main.annotate("contact", BOB, "summary", "accountant, chase on the 10th")
        got = main.get_notes("contact", BOB, include_history=True)
        assert got["notes"]["summary"]["value"] == "accountant, chase on the 10th"
        assert [entry["version"] for entry in got["history"]] == [2, 1]
        assert [entry["value"] for entry in got["history"]][-1] == "accountant"
        assert {entry["source"] for entry in got["history"]} == {"set"}

    def test_history_is_absent_unless_asked(self, notes_store):
        main.annotate("contact", BOB, "summary", "accountant")
        assert "history" not in main.get_notes("contact", BOB)

    def test_append_adds_a_line(self, notes_store):
        main.annotate("chat", ALICE, "log", "2026-09-01: called", mode="append")
        out = main.annotate("chat", ALICE, "log", "2026-09-02: no answer", mode="append")
        assert out["value"] == "2026-09-01: called\n2026-09-02: no answer"
        assert out["source"] == "append"
        assert main.get_notes("chat", ALICE)["notes"]["log"]["value"] == out["value"]

    def test_append_needs_something_to_append(self, notes_store):
        assert main.annotate("chat", ALICE, "log", "", mode="append")["error"]["code"] == "invalid_argument"

    def test_delete_is_a_tombstone_the_history_survives(self, notes_store):
        main.annotate("chat", ALICE, "mute", "yes")
        out = main.annotate("chat", ALICE, "mute", "")
        assert out["deleted"] is True and out["replaced"] == "yes"
        assert main.get_notes("chat", ALICE)["notes"] == {}
        history = main.get_notes("chat", ALICE, include_history=True)["history"]
        assert [entry["value"] for entry in history] == ["", "yes"]
        # Writing again continues the version count instead of resurrecting v1.
        assert main.annotate("chat", ALICE, "mute", "no")["version"] == 3

    def test_deleting_what_is_not_there_writes_nothing(self, notes_store):
        assert main.annotate("chat", ALICE, "mute", "")["deleted"] is False
        assert main.get_notes("chat", ALICE, include_history=True)["history"] == []


class TestOptimisticLocking:
    def test_matching_stamp_writes(self, notes_store):
        first = main.annotate("chat", ALICE, "label", "patient")
        out = main.annotate("chat", ALICE, "label", "patient, dentist", if_unchanged_since=first["updated_at"])
        assert out["success"] and out["version"] == 2

    def test_stale_stamp_is_a_conflict(self, notes_store):
        main.annotate("chat", ALICE, "label", "patient")
        out = main.annotate("chat", ALICE, "label", "supplier", if_unchanged_since="2020-01-01T00:00:00+00:00")
        assert out["error"]["code"] == "conflict"
        # Refused, not partially applied.
        assert main.get_notes("chat", ALICE)["notes"]["label"]["value"] == "patient"
        assert main.get_notes("chat", ALICE, include_history=True)["history"] == [
            {
                "key": "label",
                "value": "patient",
                "updated_at": main.get_notes("chat", ALICE)["notes"]["label"]["updated_at"],
                "source": "set",
                "version": 1,
            }
        ]

    def test_a_stamp_on_a_missing_note_is_a_conflict(self, notes_store):
        out = main.annotate("chat", ALICE, "label", "x", if_unchanged_since="2026-09-01T00:00:00+00:00")
        assert out["error"]["code"] == "conflict"


class TestIdentity:
    def test_a_contact_note_is_found_under_both_spellings(self, notes_store):
        written = main.annotate("contact", f"{BOB_LID}@lid", "label", "supplier")
        # Stored under the phone form, which is what the LID map resolves to.
        assert written["target_id"] == BOB
        assert main.get_notes("contact", BOB)["notes"]["label"]["value"] == "supplier"
        assert main.get_notes("contact", f"{BOB_LID}@lid")["notes"]["label"]["value"] == "supplier"
        assert main.get_notes("contact", BOB.split("@")[0])["notes"]["label"]["value"] == "supplier"

    def test_digits_alone_are_not_an_identity(self, notes_store):
        """Without a map entry, <n>@lid and <n>@s.whatsapp.net are different people."""
        main.annotate("contact", "999888777@lid", "label", "unknown caller")
        assert main.get_notes("contact", "999888777@s.whatsapp.net")["notes"] == {}
        assert main.get_notes("contact", "999888777@lid")["notes"]["label"]["value"] == "unknown caller"

    def test_an_unmapped_lid_stays_a_lid(self, notes_store):
        out = main.annotate("contact", "999888777@lid", "label", "unknown caller")
        assert out["target_id"] == "999888777@lid"

    @pytest.mark.parametrize("jid", ["120363000000000001@g.us", "123456@newsletter", "status@broadcast"])
    def test_other_servers_are_left_alone(self, notes_store, jid):
        """Only phone/LID spell one identity two ways; a newsletter is not a DM."""
        assert main.annotate("chat", jid, "label", "channel")["target_id"] == jid
        assert main.get_notes("chat", jid)["notes"]["label"]["value"] == "channel"
        assert main.get_notes("chat", f"{jid.split('@')[0]}@s.whatsapp.net")["notes"] == {}

    def test_a_note_written_before_the_lid_map_learned_the_pair(self, notes_store):
        """The canonical spelling wins the read, and the version count carries over."""
        with notes_store.whatsmeow() as c:
            c.execute("DELETE FROM whatsmeow_lid_map")
        whatsapp._reset_name_cache()
        main.annotate("contact", f"{BOB_LID}@lid", "label", "unknown caller")

        with notes_store.whatsmeow() as c:
            c.execute("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", (BOB_LID, BOB.split("@")[0]))
        whatsapp._reset_name_cache()
        out = main.annotate("contact", f"{BOB_LID}@lid", "label", "supplier")
        assert out["target_id"] == BOB and out["version"] == 2
        assert main.get_notes("contact", BOB)["notes"]["label"]["value"] == "supplier"
        assert [hit["target_id"] for hit in main.search_notes("caller")] == []
        assert [hit["target_id"] for hit in main.search_notes("supplier")] == [BOB]

    def test_message_ids_are_scoped_to_their_chat(self, notes_store):
        main.annotate("message", MSG, "summary", "asked about the quote")
        assert main.get_notes("message", f"{FAMILY}/MSG1")["notes"] == {}
        assert main.get_notes("message", MSG)["notes"]["summary"]["value"] == "asked about the quote"


class TestMediaAlias:
    def test_media_targets_share_the_hash_keyed_store(self, notes_store):
        out = main.annotate("media", SHA_A, "summary", "beach photo")
        assert out["target_type"] == "media" and out["target_id"] == SHA_A
        assert main.get_media_notes(SHA_A)["notes"]["summary"]["value"] == "beach photo"
        got = main.get_notes("media", SHA_A)
        assert got["notes"]["summary"]["value"] == "beach photo"
        assert [m["message_id"] for m in got["messages"]] == ["IMG1"]

    def test_annotate_media_is_visible_through_get_notes(self, notes_store):
        main.annotate_media(SHA_A, "keep", "yes")
        assert main.get_notes("media", SHA_A)["notes"]["keep"]["value"] == "yes"
        history = main.get_notes("media", SHA_A, include_history=True)["history"]
        assert [entry["key"] for entry in history] == ["keep"]

    def test_a_transcript_written_through_annotate_still_reaches_the_index(self, notes_store):
        main.annotate("media", SHA_A, "transcript", "manda o orcamento da reforma")
        assert [sha for sha, _score in media_notes.transcript_matches("orcamento")] == [SHA_A]

    def test_media_reports_what_it_replaced(self, notes_store):
        main.annotate("media", SHA_A, "summary", "beach photo")
        out = main.annotate("media", SHA_A, "summary", "beach photo, 2019")
        assert out["replaced"] == "beach photo"

    def test_media_append_and_delete(self, notes_store):
        main.annotate("media", SHA_A, "log", "seen once", mode="append")
        assert main.annotate("media", SHA_A, "log", "seen twice", mode="append")["value"] == "seen once\nseen twice"
        assert main.annotate("media", SHA_A, "log", "")["deleted"] is True

    def test_an_invisible_hash_is_not_found(self, notes_store):
        assert main.annotate("media", "ee" * 32, "summary", "x")["error"]["code"] == "not_found"


class TestSearch:
    @pytest.fixture(autouse=True)
    def _written(self, notes_store):
        main.annotate("contact", BOB, "label", "accountant")
        main.annotate("chat", FAMILY, "label", "family group")
        main.annotate("message", MSG, "summary", "accountant asked for the invoice")
        main.annotate("media", SHA_A, "summary", "invoice from the accountant")

    def test_across_every_target_type(self):
        hits = main.search_notes("accountant")
        assert {hit["target_type"] for hit in hits} == {"contact", "message", "media"}

    def test_filter_by_target_type(self):
        assert [hit["target_id"] for hit in main.search_notes("accountant", target_type="contact")] == [BOB]
        assert [hit["target_id"] for hit in main.search_notes("accountant", target_type="media")] == [SHA_A]

    def test_filter_by_key(self):
        assert [hit["target_type"] for hit in main.search_notes("accountant", key="label")] == ["contact"]

    def test_newest_first_and_limited(self):
        assert len(main.search_notes("accountant", limit=1)) == 1

    def test_a_replaced_value_is_no_longer_found(self):
        main.annotate("contact", BOB, "label", "supplier")
        assert main.search_notes("accountant", target_type="contact") == []
        assert [hit["target_id"] for hit in main.search_notes("supplier")] == [BOB]

    def test_a_deleted_value_is_no_longer_found(self):
        main.annotate("chat", FAMILY, "label", "")
        assert main.search_notes("family group") == []

    def test_empty_query_is_refused(self):
        assert main.search_notes("  ")["error"]["code"] == "invalid_argument"


class TestAllowList:
    def test_chat_and_message_targets_obey_the_allow_list(self, notes_store, monkeypatch):
        main.annotate("chat", FAMILY, "label", "family group")
        main.annotate("chat", ALICE, "label", "patient")
        main.annotate("message", MSG, "summary", "asked about the quote")

        policy = chat_policy.load_chat_policy({"WHATSAPP_ALLOWED_CHATS": ALICE})
        for module in (whatsapp, media_inventory, media_notes, notes):
            monkeypatch.setattr(module, "CHAT_POLICY", policy)

        assert main.annotate("chat", FAMILY, "label", "x")["error"]["code"] == "denied"
        assert main.get_notes("chat", FAMILY)["error"]["code"] == "denied"
        assert main.annotate("message", f"{FAMILY}/MSG1", "summary", "x")["error"]["code"] == "denied"
        assert main.get_notes("chat", ALICE)["notes"]["label"]["value"] == "patient"
        assert {hit["target_id"] for hit in main.search_notes("a")} == {ALICE, MSG}

    def test_the_allow_list_matches_the_spelling_the_archive_uses(self, notes_store, monkeypatch):
        """A DM listed under its LID is allowed under its LID, note included."""
        policy = chat_policy.load_chat_policy({"WHATSAPP_ALLOWED_CHATS": f"{BOB_LID}@lid"})
        for module in (whatsapp, media_inventory, media_notes, notes):
            monkeypatch.setattr(module, "CHAT_POLICY", policy)
        written = main.annotate("chat", f"{BOB_LID}@lid", "label", "supplier")
        assert written["success"] and written["target_id"] == BOB
        # The same conversation under the spelling the note was stored with.
        assert main.get_notes("chat", BOB)["notes"]["label"]["value"] == "supplier"
        assert [hit["target_id"] for hit in main.search_notes("supplier")] == [BOB]
        assert main.annotate("chat", ALICE, "label", "x")["error"]["code"] == "denied"

    def test_the_denial_names_the_spelling_the_caller_used(self, notes_store, monkeypatch):
        policy = chat_policy.load_chat_policy({"WHATSAPP_ALLOWED_CHATS": ALICE})
        for module in (whatsapp, media_inventory, media_notes, notes):
            monkeypatch.setattr(module, "CHAT_POLICY", policy)
        message = main.annotate("chat", f"{BOB_LID}@lid", "label", "x")["error"]["message"]
        assert f"{BOB_LID}@lid" in message and BOB not in message

    def test_contacts_are_filtered_like_search_contacts_filters_them(self, notes_store, monkeypatch):
        """A contact JID is spelled like its direct chat, so the allow-list reaches it."""
        main.annotate("contact", BOB, "label", "supplier")
        policy = chat_policy.load_chat_policy({"WHATSAPP_ALLOWED_CHATS": ALICE})
        for module in (whatsapp, media_inventory, media_notes, notes):
            monkeypatch.setattr(module, "CHAT_POLICY", policy)
        assert main.get_notes("contact", BOB)["error"]["code"] == "denied"
        assert main.annotate("contact", BOB, "label", "x")["error"]["code"] == "denied"
        assert main.search_notes("supplier") == []


class TestValidation:
    @pytest.mark.parametrize(
        ("args", "kwargs"),
        [
            (("group", ALICE, "label", "x"), {}),
            (("chat", "   ", "label", "x"), {}),
            (("chat", ALICE, "", "x"), {}),
            (("chat", ALICE, "k" * 65, "x"), {}),
            (("message", ALICE, "label", "x"), {}),
            (("message", f"{ALICE}/", "label", "x"), {}),
            (("media", "not-a-hash", "label", "x"), {}),
            (("chat", ALICE, "label", "x"), {"mode": "merge"}),
        ],
    )
    def test_bad_input_is_invalid_argument(self, notes_store, args, kwargs):
        assert main.annotate(*args, **kwargs)["error"]["code"] == "invalid_argument"

    def test_an_oversize_value_is_refused(self, notes_store):
        out = main.annotate("chat", ALICE, "summary", "x" * (media_notes.MAX_VALUE_BYTES + 1))
        assert out["error"]["code"] == "invalid_argument"
        assert main.get_notes("chat", ALICE)["notes"] == {}

    def test_append_cannot_grow_past_the_cap(self, notes_store):
        main.annotate("chat", ALICE, "log", "x" * (media_notes.MAX_VALUE_BYTES - 10))
        out = main.annotate("chat", ALICE, "log", "y" * 20, mode="append")
        assert out["error"]["code"] == "invalid_argument"
        assert "compact()" in out["error"]["message"]

    def test_unknown_target_type_in_search(self, notes_store):
        assert main.search_notes("x", target_type="group")["error"]["code"] == "invalid_argument"


class TestCompact:
    def test_compact_replaces_the_log_and_returns_it(self, notes_store):
        main.annotate("chat", ALICE, "log", "2026-09-01: called", mode="append")
        main.annotate("chat", ALICE, "log", "2026-09-02: no answer", mode="append")
        out = main.compact("chat", ALICE, "log", "called twice in September, no answer")
        assert out["replaced"] == "2026-09-01: called\n2026-09-02: no answer"
        assert out["source"] == "compact" and out["version"] == 3
        assert main.get_notes("chat", ALICE)["notes"]["log"]["value"] == "called twice in September, no answer"

    def test_the_log_survives_in_the_history(self, notes_store):
        main.annotate("chat", ALICE, "log", "2026-09-01: called", mode="append")
        main.compact("chat", ALICE, "log", "one call")
        history = main.get_notes("chat", ALICE, include_history=True)["history"]
        assert [entry["source"] for entry in history] == ["compact", "append"]
        assert history[-1]["value"] == "2026-09-01: called"

    def test_compact_makes_room_for_the_next_append(self, notes_store):
        main.annotate("chat", ALICE, "log", "x" * (media_notes.MAX_VALUE_BYTES - 10))
        assert main.annotate("chat", ALICE, "log", "y" * 20, mode="append")["error"]["code"] == "invalid_argument"
        main.compact("chat", ALICE, "log", "a long log, summarised")
        assert main.annotate("chat", ALICE, "log", "y" * 20, mode="append")["success"] is True

    def test_compact_needs_a_summary(self, notes_store):
        main.annotate("chat", ALICE, "log", "something", mode="append")
        out = main.compact("chat", ALICE, "log", "   ")
        assert out["error"]["code"] == "invalid_argument"
        assert main.get_notes("chat", ALICE)["notes"]["log"]["value"] == "something"

    def test_compact_works_on_media_too(self, notes_store):
        main.annotate("media", SHA_A, "log", "seen in Alice's chat", mode="append")
        assert main.compact("media", SHA_A, "log", "seen once")["replaced"] == "seen in Alice's chat"


class TestInlineNotes:
    def test_list_chats_and_get_chat_carry_chat_notes(self, notes_store):
        main.annotate("chat", ALICE, "label", "patient")
        by_jid = {row["jid"]: row for row in main.list_chats()["items"]}
        assert by_jid[ALICE]["notes"] == {"label": "patient"}
        # An unnoted chat says so with an empty mapping rather than staying silent.
        assert by_jid[FAMILY]["notes"] == {}
        assert main.get_chat(ALICE)["notes"] == {"label": "patient"}

    def test_one_query_for_a_whole_page(self, notes_store, monkeypatch):
        """No N+1: the page costs one notes lookup, whatever it holds."""
        main.annotate("chat", ALICE, "label", "patient")
        main.annotate("chat", FAMILY, "label", "family")
        calls = []
        original = notes.fetch_notes_for

        def counted(target_type, target_ids):
            calls.append(target_type)
            return original(target_type, target_ids)

        monkeypatch.setattr(notes, "fetch_notes_for", counted)
        items = main.list_chats()["items"]
        assert len([row for row in items if row["notes"]]) == 2
        assert calls == ["chat"]

    def test_list_unanswered_and_list_unread_carry_them(self, notes_store):
        with notes_store.messages() as c:
            c.execute(
                "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) "
                "VALUES ('MSG2', ?, ?, 'oi', '2026-09-05 11:00:00', 0)",
                (ALICE, ALICE.split("@")[0]),
            )
        main.annotate("chat", ALICE, "importance", "5")
        assert [row["notes"] for row in main.list_unanswered()["items"] if row["jid"] == ALICE] == [{"importance": "5"}]
        assert [chat["notes"] for chat in main.list_unread()["chats"] if chat["chat_jid"] == ALICE] == [
            {"importance": "5"}
        ]

    def test_get_contact_carries_contact_notes(self, notes_store):
        main.annotate("contact", BOB, "label", "supplier")
        assert main.get_contact(BOB)["notes"] == {"label": "supplier"}
        assert main.get_contact(ALICE)["notes"] == {}

    def test_a_blocked_target_gets_no_notes_from_the_batch(self, notes_store, monkeypatch):
        """The batched lookup applies the allow-list itself, whatever the caller filtered."""
        main.annotate("contact", BOB, "label", "supplier")
        main.annotate("chat", FAMILY, "label", "family")
        policy = chat_policy.load_chat_policy({"WHATSAPP_ALLOWED_CHATS": ALICE})
        for module in (whatsapp, media_inventory, media_notes, notes):
            monkeypatch.setattr(module, "CHAT_POLICY", policy)
        assert notes.fetch_notes_for("contact", [BOB]) == {}
        assert notes.fetch_notes_for("chat", [FAMILY]) == {}
        assert main.get_contact(BOB)["error"]["code"] == "denied"

    def test_the_untrusted_envelope_reaches_message_notes(self, notes_store, monkeypatch):
        monkeypatch.setenv("WHATSAPP_WRAP_UNTRUSTED", "1")
        main.annotate("message", MSG, "summary", "asked about the quote")
        main.annotate("chat", ALICE, "label", "patient")
        row = next(row for row in main.list_messages(chat_jid=ALICE)["items"] if row["id"] == "MSG1")
        assert row["message_notes"]["summary"] == f"{untrusted.OPEN_TAG}asked about the quote{untrusted.CLOSE_TAG}"
        chat = main.get_chat(ALICE)
        assert chat["notes"]["label"] == f"{untrusted.OPEN_TAG}patient{untrusted.CLOSE_TAG}"

    def test_message_rows_carry_message_notes_only_where_there_are_any(self, notes_store):
        main.annotate("message", MSG, "summary", "asked about the quote")
        rows = main.list_messages(chat_jid=ALICE)["items"]
        annotated = [row for row in rows if row["id"] == "MSG1"]
        assert annotated and annotated[0]["message_notes"] == {"summary": "asked about the quote"}
        assert all("message_notes" not in row for row in rows if row["id"] != "MSG1")

    def test_message_notes_are_not_the_media_notes_of_the_same_row(self, notes_store):
        main.annotate("media", SHA_A, "summary", "a photo")
        main.annotate("message", f"{ALICE}/IMG1", "summary", "why the photo matters")
        row = next(row for row in main.list_messages(chat_jid=ALICE)["items"] if row["id"] == "IMG1")
        assert row["notes"] == {"summary": "a photo"}
        assert row["message_notes"] == {"summary": "why the photo matters"}

    def test_get_message_context_carries_them_in_one_query(self, notes_store, monkeypatch):
        with notes_store.messages() as conn:
            conn.executemany(
                "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?, ?, 'x', ?, ?, 0)",
                [
                    ("MSG0", ALICE, "antes", "2026-09-01 10:30:00"),
                    ("MSG2", ALICE, "depois", "2026-09-01 11:30:00"),
                ],
            )
        main.annotate("message", MSG, "summary", "asked about the quote")
        main.annotate("message", f"{ALICE}/MSG2", "summary", "the answer")
        calls = []
        original = notes.fetch_notes_for

        def counted(target_type, target_ids):
            calls.append(target_type)
            return original(target_type, target_ids)

        monkeypatch.setattr(notes, "fetch_notes_for", counted)
        context = main.get_message_context(ALICE, "MSG1", before=1, after=1)
        assert context["message"]["message_notes"] == {"summary": "asked about the quote"}
        assert [row["id"] for row in context["before"]] == ["MSG0"]
        assert [row["id"] for row in context["after"]] == ["MSG2"]
        assert context["after"][0]["message_notes"] == {"summary": "the answer"}
        # The whole window, not one query per side.
        assert calls == ["message"]

    def test_notes_are_projectable_and_droppable(self, notes_store):
        """`notes` is a CHAT_FIELDS name, so the compact reads can keep or drop it."""
        with notes_store.messages() as conn:
            conn.execute(
                "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) "
                "VALUES ('MSG3', ?, ?, 'oi', '2026-09-05 11:00:00', 0)",
                (ALICE, ALICE.split("@")[0]),
            )
        main.annotate("chat", ALICE, "label", "patient")
        projected = main.list_unanswered(fields=["jid", "notes"])["items"]
        assert next(row for row in projected if row["jid"] == ALICE) == {"jid": ALICE, "notes": {"label": "patient"}}
        # omit_nulls drops the empty mapping of a chat nobody annotated.
        lean = main.list_unanswered(omit_nulls=True)["items"]
        assert all("notes" not in row for row in lean if row["jid"] != ALICE)

    def test_a_store_without_notes_costs_nothing(self, notes_store):
        assert not os.path.exists(media_notes.notes_db_path())
        assert all(row["notes"] == {} for row in main.list_chats()["items"])
        assert not os.path.exists(media_notes.notes_db_path())


class TestBoundedSearch:
    """A note search reads and enriches what the limit needs, not the whole archive."""

    @staticmethod
    def _bulk_chat_notes(count):
        """``count`` matching chat notes on distinct groups, oldest first.

        Written straight into notes.db: what is under test is one search over a
        large archive, not the write path that filled it.
        """
        jids = [f"1203630000000{i:05d}@g.us" for i in range(count)]
        conn = notes._connect(create=True)
        assert conn is not None
        try:
            # notes.py opens notes.db in autocommit; one transaction for the
            # whole fixture instead of a WAL sync per row.
            conn.execute("BEGIN")
            conn.executemany(
                "INSERT INTO notes (target_type, target_id, key, value, updated_at, source, version)"
                " VALUES ('chat', ?, 'label', 'common label', ?, 'set', 1)",
                # Zero-padded so the string order the query sorts by is the index
                # order: group 0 holds the oldest note, examined last.
                [(jid, f"2026-01-01T00:00:00.{i:06d}+00:00") for i, jid in enumerate(jids)],
            )
            conn.commit()
        finally:
            conn.close()
        return jids

    @staticmethod
    def _visibility_spy(monkeypatch):
        """Every row the search decided the visibility of."""
        seen: list[str] = []
        original = notes._visible

        def spy(target_type, target_id):
            seen.append(target_id)
            return original(target_type, target_id)

        monkeypatch.setattr(notes, "_visible", spy)
        return seen

    def test_a_small_limit_bounds_the_identity_work(self, notes_store, monkeypatch):
        """1,000 notes on 1,000 chats, limit=1: one batch of rows enriched, not a thousand."""
        jids = self._bulk_chat_notes(1000)
        seen = self._visibility_spy(monkeypatch)

        hits = main.search_notes("common label", target_type="chat", limit=1)
        assert [hit["target_id"] for hit in hits] == [jids[-1]]  # newest note
        assert len(seen) == 1  # one row enriched for one result, not a thousand
        assert len(main.search_notes("common label", target_type="chat", limit=200)) == 200
        # A full page costs a page: 200 more rows, still one batch.
        assert len(seen) == 1 + media_notes.SEARCH_BATCH

    def test_hidden_chats_ahead_of_the_match_are_walked_through(self, notes_store, monkeypatch):
        """The allow-list is applied before the limit, so a buried hit is still found."""
        monkeypatch.setattr(media_notes, "SEARCH_BATCH", 2)
        jids = self._bulk_chat_notes(5)
        policy = chat_policy.load_chat_policy({"WHATSAPP_ALLOWED_CHATS": jids[0]})
        for module in (whatsapp, media_inventory, media_notes, notes):
            monkeypatch.setattr(module, "CHAT_POLICY", policy)
        seen = self._visibility_spy(monkeypatch)

        hits = main.search_notes("common label", target_type="chat", limit=1)
        assert [hit["target_id"] for hit in hits] == [jids[0]]
        assert seen == jids[::-1]  # every newer, denied chat examined on the way

    def test_the_media_side_is_limited_too(self, notes_store, monkeypatch):
        """Both halves of a mixed search return their own newest ``limit`` before the merge."""
        calls: list[int] = []
        original = media_notes.search_media_notes

        def spy(query, key=None, limit=50):
            calls.append(limit)
            return original(query, key, limit)

        monkeypatch.setattr(media_notes, "search_media_notes", spy)
        main.annotate("media", SHA_A, "label", "common label")
        jids = self._bulk_chat_notes(3)

        # The media note was written now; the three chat notes are dated 2026-01-01.
        hits = main.search_notes("common label", limit=2)
        assert calls == [2]  # the media half is asked for the limit, not for everything
        assert [(hit["target_type"], hit["target_id"]) for hit in hits] == [("media", SHA_A), ("chat", jids[-1])]
