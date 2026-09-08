"""Triage state: which chats have been dealt with, snoozed, or marked as noise.

``list_unanswered`` answers "who is waiting on me" accurately, but a triage list
is only usable if the decisions taken on it can be fed back: without that, every
run returns the same backlog, including the chats already handled by phone call
or by somebody else (issue #287). ``mark_messages_read`` is the wrong instrument
twice over — it sends real read receipts, and "read" is not "handled".

So the state lives in notes.db, as three of the conventional chat notes:

- ``handled_at`` — when you dealt with the chat. Compared against the chat's
  last inbound message, so a *new* message brings the chat straight back;
- ``snooze_until`` — do not resurface before this instant. Its own ``updated_at``
  is compared the same way, so a message that arrives after the snooze was set
  lifts it: "chase the lab on Thursday" must not hide "urgente, me liga";
- ``mute`` — a truthy value keeps the conversation out of triage lists for good.
  The one filter a new message does not lift, because that is what it is for.

Nothing here touches WhatsApp: an agent that marks a chat handled sends nothing,
and the other side sees no change. They are ordinary notes, so an agent can also
write, read and clear them through ``annotate`` / ``get_notes``, and they survive
re-pairing with the rest of notes.db.

The filter itself is SQL. ``install_filter`` copies the current triage notes into
a temp table on the caller's ``messages.db`` connection, which lets
``list_unanswered`` and ``count_unanswered`` apply it *before* LIMIT: pagination
and ``count_only`` stay consistent with the rows returned. The copy is also where
the two timestamp notes are normalised into the spelling messages.timestamp uses,
so the comparison is a plain string comparison in SQL (a note written as
``2026-09-07T20:00:00+00:00`` must not sort after ``2026-09-07 21:00:00+00:00``),
and where a chat known under both a LID and a phone JID gets its note recorded
under every spelling — notes are stored canonically, chat rows are not.
"""

from __future__ import annotations

import sqlite3
from datetime import UTC, datetime
from typing import Any

import notes
import whatsapp
from errors import ToolError

HANDLED_KEY = "handled_at"
SNOOZE_KEY = "snooze_until"
MUTE_KEY = "mute"
LOG_KEY = "log"

# What counts as "yes" in a mute note. Anything else (including "no") is not
# muted: a chat must never drop out of triage because of a typo.
_MUTED_VALUES = frozenset({"yes", "y", "true", "1", "on", "muted"})

_TABLE = "triage_state"


def mark_handled(chat_jid: str, note: str = "") -> dict[str, Any]:
    """Record that this chat has been dealt with. No WhatsApp side effect.

    The log line is written first on purpose: it is the write that can be refused
    (an ``append`` key that has reached the 64 KB cap), and a caller that gets an
    error must not find the chat already hidden from its triage list.

    "Handled" supersedes "come back later", so a pending ``snooze_until`` is
    cleared — otherwise a chat snoozed to December would stay invisible after
    being dealt with in September, which is neither what the agent asked for nor
    something any result would tell it.
    """
    handled_at = whatsapp.timestamp_bound(datetime.now(UTC))
    logged = bool((note or "").strip())
    if logged:
        notes.annotate("chat", chat_jid, LOG_KEY, f"{handled_at[:10]}: {note.strip()}", "append", source="mark_handled")
    written = notes.annotate("chat", chat_jid, HANDLED_KEY, handled_at, source="mark_handled")
    # An empty value deletes; with nothing stored it writes no version at all.
    cleared = notes.annotate("chat", chat_jid, SNOOZE_KEY, "", source="mark_handled")
    return {
        "success": True,
        "chat_jid": written["target_id"],
        "handled_at": handled_at,
        "logged": logged,
        "snooze_cleared": bool(cleared.get("deleted")),
    }


def _parse_until(until: str) -> datetime:
    """A snooze bound as an aware instant. A bare date means that day at 00:00 UTC."""
    text = (until or "").strip()
    if not text:
        raise ToolError("invalid_argument", "until must be an ISO-8601 date or timestamp, e.g. '2026-09-10'")
    try:
        moment = whatsapp.parse_db_time(text)
    except ValueError as exc:
        raise ToolError("invalid_argument", f"until must be ISO-8601, got {until!r}") from exc
    if moment <= datetime.now(UTC):
        raise ToolError("invalid_argument", f"until must be in the future, got {until!r}")
    return moment


def snooze(chat_jid: str, until: str) -> dict[str, Any]:
    """Keep this chat out of the triage list until ``until``."""
    snooze_until = whatsapp.timestamp_bound(_parse_until(until))
    written = notes.annotate("chat", chat_jid, SNOOZE_KEY, snooze_until, source="snooze")
    return {"success": True, "chat_jid": written["target_id"], "snooze_until": snooze_until}


def _normalised_moment(value: str) -> str | None:
    """A timestamp note in the spelling messages.timestamp uses, or None if it is not one.

    An empty (deleted) or unparseable value reads as None rather than being
    guessed at: hiding a chat on the strength of a note nobody can read would be
    the one failure mode this feature must not have.
    """
    try:
        return whatsapp.timestamp_bound(whatsapp.parse_db_time(value))
    except ValueError:
        return None


def _current_triage_notes(keys: tuple[str, ...]) -> list[tuple[str, str, str, str]]:
    """Current (chat target_id, key, value, updated_at) for the wanted keys, tombstones included.

    A deleted note keeps an empty-valued row, and that row has to reach
    ``_state_by_jid``: dropping it here would let a delete written under the
    canonical spelling lose to a live row left under an older one, hiding a chat
    that ``get_notes`` reports as unmarked.
    """
    conn = notes._connect(create=False)
    if conn is None:
        return []
    try:
        rows = conn.execute(
            "SELECT target_id, key, value, updated_at, MAX(version) FROM notes"
            f" WHERE target_type = 'chat' AND key IN ({','.join('?' * len(keys))})"
            " GROUP BY target_id, key",
            list(keys),
        ).fetchall()
    finally:
        conn.close()
    return [(target_id, key, value, updated_at) for target_id, key, value, updated_at, _v in rows]


_ALIASED_SERVERS = ("s.whatsapp.net", "lid")


def _spellings(target_id: str, counterparts: dict[str, str]) -> list[str]:
    """Every ``chats.jid`` this note's target may be stored under.

    ``annotate`` records a note about a chat known as ``<lid>@lid`` under its phone
    JID once the LID map has learned the pair, while ``chats.jid`` keeps whichever
    form the bridge wrote. Matching the stored spelling alone would let a handled
    LID chat come back on every run. Groups, newsletters and broadcasts have one
    spelling and are left alone.
    """
    user, _, server = target_id.rpartition("@")
    if server not in _ALIASED_SERVERS or not user:
        return [target_id]
    other = counterparts.get(user)
    if not other:
        return [target_id]
    twin = f"{other}@lid" if server == "s.whatsapp.net" else f"{other}@s.whatsapp.net"
    return [target_id, twin]


def _state_by_jid(keys: tuple[str, ...]) -> dict[str, dict[str, Any]]:
    """{chat JID spelling: {handled_at, snooze_until, snoozed_at, muted}} for every noted chat.

    One LID-map query for the whole set rather than one per note: this runs on
    every ``list_unanswered`` call and the note table grows with every triage pass.

    When the same identity carries a note under both spellings — one written
    before the LID map paired them, one after — the phone spelling wins, and
    between two rows on equal footing the newer one does. Letting SQLite's row
    order decide would let a stale far-future ``snooze_until`` hide a chat for
    months.
    """
    rows = _current_triage_notes(keys)
    counterparts = whatsapp.lid_map_counterparts([target_id.rpartition("@")[0] for target_id, *_rest in rows])

    def rank(row: tuple[str, str, str, str]) -> tuple[bool, str]:
        return row[0].endswith("@s.whatsapp.net"), row[3]

    state: dict[str, dict[str, Any]] = {}
    for target_id, key, value, updated_at in sorted(rows, key=rank):
        entry: dict[str, Any]
        if key == MUTE_KEY:
            # Recorded even when it says "no": that row is how a canonical
            # "not muted" overrides a stale "yes" under an older spelling.
            entry = {"muted": value.strip().lower() in _MUTED_VALUES}
        else:
            # None for a tombstone and for a value that is not a timestamp; both
            # land in the state as "nothing recorded" so they can override an
            # earlier row rather than leaving it standing.
            moment = _normalised_moment(value)
            entry = {key: moment}
            if key == SNOOZE_KEY:
                # When the snooze was set, so a message that arrived afterwards
                # can lift it — the same rule handled_at follows.
                entry["snoozed_at"] = _normalised_moment(updated_at) if moment else None
        for spelling in _spellings(target_id, counterparts):
            state.setdefault(spelling, {}).update(entry)
    return state


def install_filter(
    conn: sqlite3.Connection,
    hide_handled: bool,
    exclude_muted: bool,
    include_snoozed: bool,
) -> tuple[str, list[Any]]:
    """Copy the triage notes onto ``conn`` and return the SQL that applies them.

    The returned fragment is AND-prefixed and expects ``chats`` and ``messages``
    in scope, ``messages`` being the chat's newest inbound row. Empty when no
    filter is active or when nothing has been marked yet.

    A temp table rather than an ``ATTACH``ed join because the two timestamp notes
    have to be normalised into the spelling ``messages.timestamp`` uses before they
    can be compared as text, and because the JID spellings are resolved in Python.
    It is rebuilt per call and holds one row per marked chat.
    """
    keys = tuple(
        key
        for key, wanted in ((HANDLED_KEY, hide_handled), (SNOOZE_KEY, not include_snoozed), (MUTE_KEY, exclude_muted))
        if wanted
    )
    if not keys:
        return "", []
    state = _state_by_jid(keys)
    rows = [
        (jid, entry.get(HANDLED_KEY), entry.get(SNOOZE_KEY), entry.get("snoozed_at"), int(entry.get("muted", False)))
        for jid, entry in state.items()
        # A chat left with nothing but "mute: no" would never match the predicate.
        if entry.get(HANDLED_KEY) or entry.get(SNOOZE_KEY) or entry.get("muted")
    ]
    if not rows:
        return "", []
    conn.execute(
        f"CREATE TEMP TABLE IF NOT EXISTS {_TABLE} (jid TEXT PRIMARY KEY, handled_at TEXT,"
        " snooze_until TEXT, snoozed_at TEXT, muted INTEGER NOT NULL DEFAULT 0)"
    )
    conn.execute(f"DELETE FROM temp.{_TABLE}")
    conn.executemany(
        f"INSERT INTO temp.{_TABLE} (jid, handled_at, snooze_until, snoozed_at, muted) VALUES (?, ?, ?, ?, ?)", rows
    )

    tests: list[str] = []
    params: list[Any] = []
    if hide_handled:
        # ">=", not ">": the message that arrived in the same second as the note
        # is the one the agent had just read when it marked the chat handled.
        tests.append("(t.handled_at IS NOT NULL AND t.handled_at >= messages.timestamp)")
    if not include_snoozed:
        tests.append("(t.snooze_until IS NOT NULL AND t.snooze_until > ? AND t.snoozed_at >= messages.timestamp)")
        params.append(whatsapp.timestamp_bound(datetime.now(UTC)))
    if exclude_muted:
        tests.append("t.muted = 1")
    clause = f"AND NOT EXISTS (SELECT 1 FROM temp.{_TABLE} t WHERE t.jid = chats.jid AND ({' OR '.join(tests)}))"
    return clause, params
