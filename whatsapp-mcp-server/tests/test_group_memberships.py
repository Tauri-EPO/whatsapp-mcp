"""get_contact_chats reports the groups a contact belongs to, not only the ones they talk in.

Issue #288: joining over `messages` alone hides a member who has never posted,
which is exactly the person an agent needs to know about before replying. The
bridge caches each group's roster in `group_members`; these tests drive that
table the way the bridge writes it (bare user parts in every address column).
"""

import sqlite3

import pytest

import chat_policy
import whatsapp
from errors import ToolError
from tests.conftest import BOB, BOB_LID, BOB_PN, FAMILY

# group_members as whatsapp-bridge/group_members_store.go creates it.
GROUP_MEMBERS_SCHEMA = """
CREATE TABLE group_members (
    group_jid TEXT NOT NULL,
    user TEXT NOT NULL,
    lid TEXT,
    phone TEXT,
    name TEXT,
    is_admin BOOLEAN NOT NULL DEFAULT 0,
    is_super_admin BOOLEAN NOT NULL DEFAULT 0,
    first_seen TIMESTAMP,
    last_seen TIMESTAMP,
    source TEXT,
    PRIMARY KEY (group_jid, user)
);
"""

BEIRA_MAR = "120363000000000002@g.us"
BSPAR = "120363000000000003@g.us"


def add_group(conn, jid, name, last_message_time="2026-09-01 08:00:00"):
    conn.execute("INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)", (jid, name, last_message_time))


def add_member(conn, group_jid, user, *, phone="", lid="", is_admin=0, last_seen="2026-09-07 12:00:00"):
    conn.execute(
        """INSERT INTO group_members
           (group_jid, user, lid, phone, name, is_admin, is_super_admin, first_seen, last_seen, source)
           VALUES (?, ?, ?, ?, '', ?, 0, ?, ?, 'roster')""",
        (group_jid, user, lid, phone, is_admin, last_seen, last_seen),
    )


def spoke(conn, chat_jid, sender, msg_id, timestamp="2026-09-04 09:00:00"):
    conn.execute(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?, ?, ?, 'oi', ?, 0)",
        (msg_id, chat_jid, sender, timestamp),
    )


@pytest.fixture
def rostered(paired_dbs):
    """Bob: speaks in Family, belongs to two project groups without ever posting."""
    with paired_dbs.messages() as c:
        c.executescript(GROUP_MEMBERS_SCHEMA)
        add_group(c, BEIRA_MAR, "Projeto Beira-mar", "2026-08-01 08:00:00")
        add_group(c, BSPAR, "Projeto Beira Mar | BSPAR", "2026-08-02 08:00:00")
        spoke(c, FAMILY, BOB_PN, "F1")
        add_member(c, FAMILY, BOB_PN, phone=BOB_PN, is_admin=0, last_seen="2026-09-07 12:00:00")
        add_member(c, BEIRA_MAR, BOB_PN, phone=BOB_PN, is_admin=1, last_seen="2026-09-07 11:00:00")
        add_member(c, BSPAR, BOB_PN, phone=BOB_PN, is_admin=1, last_seen="2026-09-07 10:00:00")
    whatsapp._reset_schema_cache()
    return paired_dbs


def by_jid(items):
    return {item["jid"]: item for item in items}


def test_membership_only_groups_are_returned_and_flagged(rostered):
    items = by_jid(whatsapp.get_contact_chats_page(BOB, limit=50).items)

    # The regression the issue reported: two groups he belongs to but has
    # never posted in were missing entirely.
    assert BEIRA_MAR in items and BSPAR in items
    assert items[BEIRA_MAR]["membership"] == "member"
    assert items[BEIRA_MAR]["is_admin"] is True
    assert items[BEIRA_MAR]["roster_seen_at"] == "2026-09-07T11:00:00+00:00"
    assert items[BEIRA_MAR]["is_group"] is True

    # He speaks in Family and is on its roster.
    assert items[FAMILY]["membership"] == "both"
    assert items[FAMILY]["is_admin"] is False

    # His own direct chat has no roster, and he has not written in it.
    assert items[BOB]["membership"] is None
    assert items[BOB]["is_admin"] is None
    assert items[BOB]["roster_seen_at"] is None


def test_spoken_only_chat_is_flagged_spoke(rostered):
    with rostered.messages() as c:
        spoke(c, "120363000000000009@g.us", BOB_PN, "X1")
        add_group(c, "120363000000000009@g.us", "Sem roster")
    items = by_jid(whatsapp.get_contact_chats_page(BOB, limit=50).items)
    assert items["120363000000000009@g.us"]["membership"] == "spoke"
    assert items["120363000000000009@g.us"]["is_admin"] is None


def test_conversations_come_before_memberships(rostered):
    items = whatsapp.get_contact_chats_page(BOB, limit=50).items
    ranks = [item["membership"] == "member" for item in items]
    assert ranks == sorted(ranks), f"memberships must follow the conversations: {[i['jid'] for i in items]}"
    # Within the memberships, the most recently confirmed roster first.
    memberships = [item["jid"] for item in items if item["membership"] == "member"]
    assert memberships == [BEIRA_MAR, BSPAR]


def test_membership_matches_the_contacts_other_address_form(paired_dbs):
    """A roster the bridge cached under a LID still answers a lookup by phone."""
    with paired_dbs.messages() as c:
        c.executescript(GROUP_MEMBERS_SCHEMA)
        add_group(c, BEIRA_MAR, "Projeto Beira-mar")
        add_member(c, BEIRA_MAR, BOB_LID, lid=BOB_LID, is_admin=1)
    whatsapp._reset_schema_cache()

    items = by_jid(whatsapp.get_contact_chats_page(BOB, limit=50).items)
    assert items[BEIRA_MAR]["membership"] == "member"
    # ...and the same lookup from the LID side.
    items = by_jid(whatsapp.get_contact_chats_page(f"{BOB_LID}@lid", limit=50).items)
    assert items[BEIRA_MAR]["membership"] == "member"


def test_memberships_respect_the_chat_allow_list(rostered, monkeypatch):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", chat_policy.ChatPolicy.from_entries([FAMILY, BSPAR]))
    jids = {item["jid"] for item in whatsapp.get_contact_chats_page(BOB, limit=50).items}
    assert BEIRA_MAR not in jids, "a membership must not leak a chat the allow-list excludes"
    assert jids == {FAMILY, BSPAR}


def test_store_without_the_table_still_answers(paired_dbs):
    """A messages.db written by a bridge from before this release has no rosters."""
    with paired_dbs.messages() as c:
        spoke(c, FAMILY, BOB_PN, "F1")
    items = by_jid(whatsapp.get_contact_chats_page(BOB, limit=50).items)
    assert items[FAMILY]["membership"] == "spoke"
    assert items[FAMILY]["roster_seen_at"] is None


def test_cursor_walk_crosses_the_rank_boundary_exactly_once(rostered):
    seen, cursor, pages = [], None, 0
    while True:
        page = whatsapp.get_contact_chats_page(BOB, limit=1, cursor=cursor)
        pages += 1
        seen.extend(item["jid"] for item in page.items)
        if not page.has_more:
            break
        cursor = page.next_cursor
        assert pages < 20, "cursor walk did not terminate"

    assert len(seen) == len(set(seen)), f"a row was repeated across pages: {seen}"
    assert set(seen) == {BOB, FAMILY, BEIRA_MAR, BSPAR}
    assert seen == [item["jid"] for item in whatsapp.get_contact_chats_page(BOB, limit=50).items]


def test_group_with_a_roster_but_no_chat_row_is_still_reported(paired_dbs):
    """list_group_members can cache a roster for a group the archive never saw."""
    with paired_dbs.messages() as c:
        c.executescript(GROUP_MEMBERS_SCHEMA)
        add_member(c, BSPAR, BOB_PN, phone=BOB_PN)
    whatsapp._reset_schema_cache()

    items = by_jid(whatsapp.get_contact_chats_page(BOB, limit=50).items)
    assert items[BSPAR]["membership"] == "member"
    assert items[BSPAR]["has_messages"] is False
    assert items[BSPAR]["last_message_time"] is None


def test_sqlite_error_surfaces_as_a_tool_error(rostered):
    with rostered.messages() as c:
        c.execute("DROP TABLE chats")
    with pytest.raises(Exception) as excinfo:
        whatsapp.get_contact_chats_page(BOB, limit=5)
    assert "database error" in str(excinfo.value)


def test_paging_by_offset_still_works(rostered):
    first = whatsapp.get_contact_chats_page(BOB, limit=2, page=0).items
    second = whatsapp.get_contact_chats_page(BOB, limit=2, page=1).items
    assert {item["jid"] for item in first} & {item["jid"] for item in second} == set()
    assert len(first) == 2 and len(second) == 2


def test_connection_is_closed_even_on_success(rostered):
    """The finally block closes the handle; a second call must not see a lock."""
    whatsapp.get_contact_chats_page(BOB, limit=5)
    whatsapp.get_contact_chats_page(BOB, limit=5)
    with sqlite3.connect(rostered.messages_db) as c:
        assert c.execute("SELECT COUNT(*) FROM group_members").fetchone()[0] == 3


def test_empty_contact_is_refused(rostered):
    """An empty alias would match every row whose address form the bridge left blank."""
    for bad in ("", "   ", "@lid", "@s.whatsapp.net"):
        with pytest.raises(ToolError) as excinfo:
            whatsapp.get_contact_chats_page(bad, limit=50)
        assert excinfo.value.code == "invalid_argument"


def test_blank_address_columns_do_not_match_another_contact(paired_dbs):
    """The bridge writes an unknown address form as '', never NULL."""
    with paired_dbs.messages() as c:
        c.executescript(GROUP_MEMBERS_SCHEMA)
        add_group(c, BEIRA_MAR, "Projeto Beira-mar")
        # A LID-only member: `phone` is the empty string, not NULL.
        add_member(c, BEIRA_MAR, "777", lid="777")
    items = by_jid(whatsapp.get_contact_chats_page(BOB, limit=50).items)
    assert BEIRA_MAR not in items, "a blank phone column matched an unrelated contact"


def test_admin_and_roster_time_only_come_from_a_roster_fetch(paired_dbs):
    """A join event or a message says "they are here", not "they are an admin"."""
    with paired_dbs.messages() as c:
        c.executescript(GROUP_MEMBERS_SCHEMA)
        add_group(c, BEIRA_MAR, "Projeto Beira-mar")
        c.execute(
            """INSERT INTO group_members
               (group_jid, user, lid, phone, name, is_admin, is_super_admin, first_seen, last_seen, source)
               VALUES (?, ?, '', ?, '', 0, 0, ?, ?, 'message')""",
            (BEIRA_MAR, BOB_PN, BOB_PN, "2026-09-07 12:00:00", "2026-09-07 12:00:00"),
        )
    items = by_jid(whatsapp.get_contact_chats_page(BOB, limit=50).items)
    assert items[BEIRA_MAR]["membership"] == "member"
    assert items[BEIRA_MAR]["is_admin"] is None
    assert items[BEIRA_MAR]["roster_seen_at"] is None

    # The same group, once a roster fetch has actually seen it.
    with paired_dbs.messages() as c:
        c.execute("UPDATE group_members SET source = 'roster', is_admin = 1 WHERE group_jid = ?", (BEIRA_MAR,))
    items = by_jid(whatsapp.get_contact_chats_page(BOB, limit=50).items)
    assert items[BEIRA_MAR]["is_admin"] is True
    assert items[BEIRA_MAR]["roster_seen_at"] == "2026-09-07T12:00:00+00:00"


def test_table_created_after_the_first_read_is_picked_up(paired_dbs):
    """The bridge's migration must not be hidden by a cached schema probe."""
    assert whatsapp.get_contact_chats_page(BOB, limit=50).items is not None
    with paired_dbs.messages() as c:
        c.executescript(GROUP_MEMBERS_SCHEMA)
        add_group(c, BSPAR, "Projeto Beira Mar | BSPAR")
        add_member(c, BSPAR, BOB_PN, phone=BOB_PN)
    items = by_jid(whatsapp.get_contact_chats_page(BOB, limit=50).items)
    assert items[BSPAR]["membership"] == "member"
