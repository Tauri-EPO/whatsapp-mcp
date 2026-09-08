"""mentions_me and the owner identity (issue #290).

In a group "the last message is inbound" is always true; what waits for an
answer is a mention of this account. WhatsApp writes one as the mentioned
account's LID, so the archive is only searchable once the server knows both
spellings of "me".
"""

import sqlite3
from datetime import UTC, datetime, timedelta

import httpx
import pytest

import main
import whatsapp
from tests.conftest import MESSAGES_SCHEMA, WHATSMEOW_SCHEMA

OWNER_PHONE = "5511999999999"
OWNER_LID = "158883943301358"
TEAM = "120363000000000001@g.us"
FAMILY = "120363000000000002@g.us"
QUIET = "120363000000000003@g.us"
OBRA = "120363000000000004@g.us"
ALICE = "5511888888888@s.whatsapp.net"


def _stamp(**delta):
    return (datetime.now(UTC) - timedelta(**delta)).strftime("%Y-%m-%d %H:%M:%S+00:00")


# id, chat, timestamp, is_from_me, content, mentions
MESSAGES = [
    # A mention written as the owner's LID, unanswered: the case from the issue.
    ("t1", TEAM, _stamp(days=3), 0, f"Lucas chegou! @{OWNER_LID}", OWNER_LID),
    ("t2", TEAM, _stamp(days=2), 0, "alguem viu?", None),
    # The group kept talking after the mention, so min_age_hours hides the chat
    # even though the mention itself has been waiting for days.
    ("t3", TEAM, _stamp(minutes=20), 0, "bom dia", None),
    # A mention written as the owner's phone number, in another group.
    ("f1", FAMILY, _stamp(days=5), 1, "eu respondo depois", None),
    ("f2", FAMILY, _stamp(days=4), 0, f"o que acha desse, @{OWNER_PHONE} ?", OWNER_PHONE),
    # Somebody else was mentioned, and a number merely typed: neither is me.
    ("q1", QUIET, _stamp(days=6), 0, "@5511777777777 tudo bem?", "5511777777777"),
    ("q2", QUIET, _stamp(days=6), 0, "liga pro 5511999999999", None),
    # A mention I already answered.
    ("a1", ALICE, _stamp(days=8), 0, f"@{OWNER_LID} me manda o pdf", OWNER_LID),
    ("a2", ALICE, _stamp(days=7), 1, "mandei", None),
]


@pytest.fixture
def store(tmp_path, monkeypatch):
    """A paired store: messages with mentions, and whatsmeow's own device row."""
    messages_db = tmp_path / "messages.db"
    whatsmeow_db = tmp_path / "whatsapp.db"
    with sqlite3.connect(messages_db) as c:
        c.executescript(MESSAGES_SCHEMA)
        c.execute("ALTER TABLE chats ADD COLUMN last_read_time TIMESTAMP")
        c.executemany(
            "INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)",
            [
                (TEAM, "Clinic team", _stamp(minutes=20)),
                (FAMILY, "Family", _stamp(days=4)),
                (QUIET, "Neighbours", _stamp(days=6)),
                (ALICE, "Alice", _stamp(days=7)),
            ],
        )
        c.executemany(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, mentions)"
            " VALUES (?,?,?,?,?,?,?)",
            [(mid, chat, chat.split("@")[0], text, ts, me, mentions) for mid, chat, ts, me, text, mentions in MESSAGES],
        )
    with sqlite3.connect(whatsmeow_db) as c:
        c.executescript(WHATSMEOW_SCHEMA)
        c.execute("CREATE TABLE whatsmeow_device (jid TEXT PRIMARY KEY, lid TEXT)")
        c.execute(
            "INSERT INTO whatsmeow_device VALUES (?, ?)",
            (f"{OWNER_PHONE}.0:23@s.whatsapp.net", f"{OWNER_LID}:23@lid"),
        )
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(messages_db))
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", str(whatsmeow_db))
    whatsapp._reset_schema_cache()
    whatsapp._reset_name_cache()
    return messages_db


@pytest.fixture
def bridge_down(monkeypatch):
    """Every bridge call fails: reads must not need it."""

    def refuse(url, **kwargs):
        raise httpx.ConnectError("refused")

    monkeypatch.setattr(whatsapp.bridge_http, "get", refuse)
    monkeypatch.setattr(whatsapp.time, "sleep", lambda s: None)


def _ids(result):
    return [item["id"] for item in result["items"]]


# --- who am I ------------------------------------------------------------------


def test_owner_comes_from_the_local_store(store, bridge_down):
    """whatsmeow already knows: no bridge call, device and agent suffixes stripped."""
    assert whatsapp.owner_identity() == {
        "jid": f"{OWNER_PHONE}@s.whatsapp.net",
        "phone": OWNER_PHONE,
        "lid": OWNER_LID,
    }


def test_owner_falls_back_to_the_bridge(tmp_path, monkeypatch):
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", str(tmp_path / "absent.db"))

    class Resp:
        status_code = 200

        def json(self):
            return {
                "phone_jid": f"{OWNER_PHONE}@s.whatsapp.net",
                "phone": OWNER_PHONE,
                "lid_jid": f"{OWNER_LID}@lid",
                "lid": OWNER_LID,
            }

    calls = []

    def get(url, **kwargs):
        calls.append(url)
        return Resp()

    monkeypatch.setattr(whatsapp.bridge_http, "get", get)
    assert whatsapp.owner_identity()["lid"] == OWNER_LID
    assert calls and calls[0].endswith("/me")
    # Cached: a second call does not ask again.
    whatsapp.owner_identity()
    assert len(calls) == 1


def test_unknown_owner_is_an_error_not_an_empty_page(tmp_path, monkeypatch, store, bridge_down):
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", str(tmp_path / "absent.db"))
    out = main.list_messages(mentions_me=True)
    assert out["error"]["code"] == "bridge_unavailable"


# --- the filter ----------------------------------------------------------------


def test_mentions_me_matches_both_spellings(store, bridge_down):
    """The LID form and the phone form of my own account, and nothing else."""
    out = main.list_messages(mentions_me=True, include_context=False)
    assert sorted(_ids(out)) == ["a1", "f2", "t1"]
    assert main.list_messages(mentions_me=True, count_only=True) == {"count": 3}


def test_mentions_me_is_not_a_text_search(store, bridge_down):
    """A number typed into the text is not a mention (q2), nor is someone else's (q1)."""
    ids = _ids(main.list_messages(mentions_me=True, include_context=False, chat_jid=QUIET))
    assert ids == []


def test_mentions_me_combines_with_the_other_filters(store, bridge_down):
    out = main.list_messages(mentions_me=True, chat_jid=TEAM, include_context=False)
    assert _ids(out) == ["t1"]
    stats = main.message_stats(mentions_me=True, group_by="chat")
    assert {bucket["key"]: bucket["messages"] for bucket in stats["buckets"]} == {TEAM: 1, FAMILY: 1, ALICE: 1}


def test_store_without_the_column_says_so(tmp_path, monkeypatch, store, bridge_down):
    """An archive written by an older bridge cannot answer this question."""
    old = tmp_path / "old.db"
    with sqlite3.connect(old) as c:
        c.executescript(MESSAGES_SCHEMA.replace("target_message_id TEXT, mentions TEXT,", "target_message_id TEXT,"))
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(old))
    whatsapp._reset_schema_cache()
    out = main.list_messages(mentions_me=True)
    assert out["error"]["code"] == "bridge_unavailable"
    assert "mentions" in out["error"]["message"]


# --- triage --------------------------------------------------------------------


def test_group_mentions_are_flagged_in_triage(store, bridge_down):
    out = main.list_unanswered(include_group_mentions=True)
    flagged = {item["jid"]: item.get("mention") for item in out["items"]}
    assert flagged[TEAM] is True and flagged[FAMILY] is True
    assert flagged[QUIET] is False  # inbound last, but nobody addressed me
    team = next(item for item in out["items"] if item["jid"] == TEAM)
    assert team["mention_message_id"] == "t1"
    # ALICE mentioned me but I answered: not waiting, and not in the list at all.
    assert ALICE not in flagged


def test_group_mention_survives_a_chatty_group(store, bridge_down):
    """The mention is three days old even though the group spoke 20 minutes ago."""
    without = main.list_unanswered(min_age_hours=24)
    assert TEAM not in [item["jid"] for item in without["items"]]

    out = main.list_unanswered(min_age_hours=24, include_group_mentions=True)
    team = next(item for item in out["items"] if item["jid"] == TEAM)
    assert team["mention"] is True and team["mention_message_id"] == "t1"
    # Anchored on the mention, so the row says how long it has been waiting.
    assert 71 < team["age_hours"] < 73
    # Each chat appears once.
    jids = [item["jid"] for item in out["items"]]
    assert len(jids) == len(set(jids))


def test_min_messages_bounds_the_mention_stream_too(store, bridge_down):
    """A group added by its mention obeys the same floor as the ordinary rule (#336)."""
    kept = main.list_unanswered(min_age_hours=24, include_group_mentions=True, min_messages=3)
    assert TEAM in [item["jid"] for item in kept["items"]]  # t1, t2, t3
    dropped = main.list_unanswered(min_age_hours=24, include_group_mentions=True, min_messages=4)
    assert TEAM not in [item["jid"] for item in dropped["items"]]


def _add_obra(store, *, mention: str | None, media_type: str | None = None, older: str | None = None):
    """A group that only mentions me to acknowledge, and keeps chatting afterwards.

    min_age_hours hides it from the ordinary rule, so the mention stream is the
    only door it can come through. `older` adds a real question before it.
    """
    rows = [
        # My last word: everything after it is what "pending" is measured from.
        ("c0", OBRA, "me", "vou ver", _stamp(days=5), 1, None, None),
        ("c1", OBRA, "x", mention, _stamp(days=3), 0, OWNER_LID, media_type),
        ("c9", OBRA, "x", "bom dia", _stamp(minutes=15), 0, None, None),
    ]
    if older is not None:
        rows.append(("c0b", OBRA, "x", older, _stamp(days=4), 0, OWNER_LID, None))
    with sqlite3.connect(store) as c:
        c.execute("INSERT INTO chats (jid, name, last_message_time) VALUES (?, 'Obra', ?)", (OBRA, _stamp(minutes=15)))
        c.executemany(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, mentions, media_type)"
            " VALUES (?,?,?,?,?,?,?,?)",
            rows,
        )


def _waiting(**kwargs):
    return [
        item["jid"] for item in main.list_unanswered(min_age_hours=24, include_group_mentions=True, **kwargs)["items"]
    ]


@pytest.mark.parametrize(
    "mention, media_type",
    [
        # WhatsApp writes the mention into the text, so this is stored as "ok @158…".
        (f"ok @{OWNER_LID}", None),
        (f"@{OWNER_LID} 👍", None),
        (f"@{OWNER_LID}", "sticker"),
        # Removing the mention leaves "valeu  !": the space it left goes with the "!".
        (f"valeu @{OWNER_LID}!", None),
        (f"Obrigado @{OWNER_LID}.", None),
    ],
)
def test_a_closing_mention_does_not_put_a_group_on_the_list(store, bridge_down, mention, media_type):
    """ignore_closing_messages reads the mention too, not only the last message (#395)."""
    _add_obra(store, mention=mention, media_type=media_type)
    assert OBRA in _waiting()
    hidden = _waiting(ignore_closing_messages=True)
    assert OBRA not in hidden
    # A real mention in another group is untouched by the flag.
    assert TEAM in hidden


def test_a_closing_mention_leaves_the_older_real_one_in_charge(store, bridge_down):
    """The row must be anchored on, and name, the mention the flag did not hide."""
    _add_obra(store, mention=f"ok @{OWNER_LID}", older=f"@{OWNER_LID} manda o orçamento?")
    without = main.list_unanswered(min_age_hours=24, include_group_mentions=True)
    obra = next(item for item in without["items"] if item["jid"] == OBRA)
    assert obra["mention_message_id"] == "c1"

    out = main.list_unanswered(min_age_hours=24, include_group_mentions=True, ignore_closing_messages=True)
    obra = next(item for item in out["items"] if item["jid"] == OBRA)
    assert obra["mention_message_id"] == "c0b"
    assert obra["mention_time"] == obra["last_inbound_time"]  # anchor and pointer agree
    assert 95 < obra["age_hours"] < 97  # four days, not three


def test_a_closing_mention_of_somebody_else_is_still_a_mention(store, bridge_down):
    """Only this account's own @… is read past: the rest of the text still counts."""
    _add_obra(store, mention=f"ok @5511777777777 @{OWNER_LID}")
    assert OBRA in _waiting(ignore_closing_messages=True)


def test_closing_mentions_never_take_a_slot_in_a_page(store, bridge_down):
    """Hidden in SQL, before LIMIT, so a walk returns no hole and no repeat."""
    _add_obra(store, mention=f"ok @{OWNER_LID}")
    seen: list[str] = []
    cursor = None
    for _ in range(5):
        page = main.list_unanswered(
            limit=1, min_age_hours=24, include_group_mentions=True, ignore_closing_messages=True, cursor=cursor
        )
        seen += [item["jid"] for item in page["items"]]
        cursor = page["next_cursor"]
        if not cursor:
            break
    assert OBRA not in seen
    assert TEAM in seen
    assert len(seen) == len(set(seen)), seen


def test_exclude_groups_wins_over_group_mentions(store, bridge_down):
    """ "No groups" means no group comes back through the mention door either."""
    out = main.list_unanswered(exclude_groups=True, include_group_mentions=True, min_age_hours=24)
    assert [item["jid"] for item in out["items"]] == []


def test_paging_never_repeats_a_group_with_two_mentions(store, bridge_down):
    """The page key is the chat's newest mention, not each mention row."""
    with sqlite3.connect(store) as c:
        c.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, mentions)"
            " VALUES ('t0', ?, 'x', ?, ?, 0, ?)",
            (TEAM, f"@{OWNER_LID} e antes disso", _stamp(days=4), OWNER_LID),
        )
    seen = []
    cursor = None
    for _ in range(5):
        page = main.list_unanswered(limit=1, min_age_hours=24, include_group_mentions=True, cursor=cursor)
        seen += [item["jid"] for item in page["items"]]
        cursor = page["next_cursor"]
        if not cursor:
            break
    assert len(seen) == len(set(seen)), seen
    assert TEAM in seen


def test_a_reaction_does_not_answer_a_mention(store, bridge_down):
    """The rule list_unanswered already applies: a thumbs-up is not a reply."""
    with sqlite3.connect(store) as c:
        c.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type)"
            " VALUES ('t9', ?, 'me', '👍', ?, 1, 'reaction')",
            (TEAM, _stamp(days=1)),
        )
        # And a message of mine that I then revoked.
        c.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, deleted_at)"
            " VALUES ('t8', ?, 'me', 'ops', ?, 1, ?)",
            (TEAM, _stamp(days=1), _stamp(days=1)),
        )
    out = main.list_unanswered(include_group_mentions=True)
    team = next(item for item in out["items"] if item["jid"] == TEAM)
    assert team["mention"] is True and team["mention_message_id"] == "t1"


def test_a_revoked_mention_is_not_pending(store, bridge_down):
    with sqlite3.connect(store) as c:
        c.execute("UPDATE messages SET deleted_at = ? WHERE id = 't1'", (_stamp(days=2),))
    out = main.list_unanswered(include_group_mentions=True)
    team = next(item for item in out["items"] if item["jid"] == TEAM)
    assert team["mention"] is False and "mention_message_id" not in team


def test_the_named_mention_is_the_one_the_row_is_about(store, bridge_down):
    """min_age_hours bounds the anchor, so it must bound the pointer as well."""
    with sqlite3.connect(store) as c:
        c.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, mentions)"
            " VALUES ('t7', ?, 'x', ?, ?, 0, ?)",
            (TEAM, f"@{OWNER_LID} ainda?", _stamp(minutes=20), OWNER_LID),
        )
    out = main.list_unanswered(min_age_hours=24, include_group_mentions=True)
    team = next(item for item in out["items"] if item["jid"] == TEAM)
    assert team["mention_message_id"] == "t1"  # not the 20-minute-old t7
    assert team["mention_time"] == team["last_inbound_time"]


def test_owner_lid_comes_from_the_mapping_on_an_older_pairing(store, monkeypatch, bridge_down):
    """A pairing older than whatsmeow's device.lid column still knows its LID."""
    with sqlite3.connect(whatsapp.WHATSMEOW_DB_PATH) as c:
        c.execute("UPDATE whatsmeow_device SET lid = NULL")
        c.execute("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", (OWNER_LID, OWNER_PHONE))
    whatsapp._reset_owner_cache()
    assert whatsapp.owner_identity()["lid"] == OWNER_LID
    assert sorted(_ids(main.list_messages(mentions_me=True, include_context=False))) == ["a1", "f2", "t1"]


def test_mention_fields_are_only_valid_names_with_the_flag(store, bridge_down):
    ok = main.list_unanswered(include_group_mentions=True, fields=["jid", "mention"])
    assert ok["items"] and set(ok["items"][0]) == {"jid", "mention"}
    refused = main.list_unanswered(fields=["jid", "mention"])
    assert refused["error"]["code"] == "invalid_argument"


def test_group_mentions_compose_with_the_triage_notes(store, bridge_down):
    """A group marked handled stays hidden even when it holds a mention (#287).

    notes.db sits next to messages.db, which the fixture already points at
    tmp_path, so this writes nothing outside the test.
    """
    main.annotate("chat", TEAM, "handled_at", _stamp(minutes=1))
    out = main.list_unanswered(include_group_mentions=True)
    assert TEAM not in [item["jid"] for item in out["items"]]
    shown = main.list_unanswered(include_group_mentions=True, hide_handled=False)
    assert TEAM in [item["jid"] for item in shown["items"]]


def test_a_muted_group_does_not_come_back_through_the_mention_stream(store, bridge_down):
    """mute is "never surface this", and the mention stream is a surface (#361)."""
    main.annotate("chat", TEAM, "mute", "yes")
    out = main.list_unanswered(min_age_hours=24, include_group_mentions=True)
    assert TEAM not in [item["jid"] for item in out["items"]]
    shown = main.list_unanswered(min_age_hours=24, include_group_mentions=True, exclude_muted=False)
    assert TEAM in [item["jid"] for item in shown["items"]]


def test_a_snoozed_group_does_not_come_back_through_the_mention_stream(store, bridge_down):
    """The snooze was set today, after the mention: it covers it (#361)."""
    main.annotate("chat", TEAM, "snooze_until", _stamp(days=-2))
    out = main.list_unanswered(min_age_hours=24, include_group_mentions=True)
    assert TEAM not in [item["jid"] for item in out["items"]]
    shown = main.list_unanswered(min_age_hours=24, include_group_mentions=True, include_snoozed=True)
    assert TEAM in [item["jid"] for item in shown["items"]]


def test_handled_hides_the_mention_stream_until_a_newer_mention(store, bridge_down):
    """The mark is compared with the mention, so a new one brings the group back (#361)."""
    main.annotate("chat", TEAM, "handled_at", _stamp(days=2))
    out = main.list_unanswered(min_age_hours=24, include_group_mentions=True)
    assert TEAM not in [item["jid"] for item in out["items"]]

    with sqlite3.connect(store) as c:
        c.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, mentions)"
            " VALUES ('t6', ?, 'x', ?, ?, 0, ?)",
            (TEAM, f"@{OWNER_LID} e agora?", _stamp(hours=30), OWNER_LID),
        )
    again = main.list_unanswered(min_age_hours=24, include_group_mentions=True)
    team = next(item for item in again["items"] if item["jid"] == TEAM)
    assert team["mention_message_id"] == "t6"
    assert team["mention_time"] == team["last_inbound_time"]


def test_a_hidden_group_never_takes_a_slot_in_a_page(store, bridge_down):
    """The filter runs before LIMIT, so paging returns no hole and no repeat (#361)."""
    main.annotate("chat", TEAM, "mute", "yes")
    seen: list[str] = []
    cursor = None
    for _ in range(5):
        page = main.list_unanswered(limit=1, min_age_hours=24, include_group_mentions=True, cursor=cursor)
        seen += [item["jid"] for item in page["items"]]
        cursor = page["next_cursor"]
        if not cursor:
            break
    assert TEAM not in seen
    assert FAMILY in seen and QUIET in seen
    assert len(seen) == len(set(seen)), seen


def test_group_mentions_are_off_by_default(store, bridge_down):
    out = main.list_unanswered()
    assert all("mention" not in item for item in out["items"])


def test_a_reply_under_one_spelling_answers_a_mention_under_the_other(store, bridge_down):
    """A phone/LID pair is one conversation, so my last word covers both rows (#366)."""
    alice_lid = "197654321098765"
    alice_lid_chat = f"{alice_lid}@lid"
    with sqlite3.connect(whatsapp.WHATSMEOW_DB_PATH) as c:
        c.execute("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", (alice_lid, ALICE.split("@")[0]))
    with sqlite3.connect(store) as c:
        c.execute(
            "INSERT INTO chats (jid, name, last_message_time) VALUES (?, 'Alice', ?)",
            (alice_lid_chat, _stamp(days=6)),
        )
        c.executemany(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, mentions)"
            " VALUES (?,?,?,?,?,?,?)",
            [
                # Older than the reply stored under the phone JID (a2, seven days).
                ("al1", alice_lid_chat, alice_lid, f"@{OWNER_LID} e o pdf?", _stamp(days=9), 0, OWNER_LID),
                ("al2", alice_lid_chat, alice_lid, "ainda?", _stamp(days=6), 0, None),
            ],
        )
    whatsapp._reset_name_cache()

    out = main.list_unanswered(include_group_mentions=True)
    alice = next(item for item in out["items"] if item["jid"] == ALICE)
    assert alice["aliases"] == [ALICE, alice_lid_chat]
    assert alice["mention"] is False and "mention_message_id" not in alice

    # A mention newer than that reply is still found in the row that does not list.
    with sqlite3.connect(store) as c:
        c.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, mentions)"
            " VALUES ('al3', ?, ?, ?, ?, 0, ?)",
            (alice_lid_chat, alice_lid, f"@{OWNER_LID} ?", _stamp(days=1), OWNER_LID),
        )
    again = main.list_unanswered(include_group_mentions=True)
    alice = next(item for item in again["items"] if item["jid"] == ALICE)
    assert alice["mention"] is True and alice["mention_message_id"] == "al3"
