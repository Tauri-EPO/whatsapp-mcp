"""Agent notes about chats, contacts, messages and media, versioned, in notes.db.

``media_notes.py`` gave files a memory keyed by content hash and it works; the
judgements a triage pass produces about *people* — "this is a patient",
"importance 5", "marketing bot", "waiting on the accountant" — had nowhere to
live and left the archive in a Markdown file. This module is that home: one
table, four target types, and the same allow-list rules as everything else.

Storage is **append-only**. Every write inserts a new row and reads return the
highest ``version`` per ``(target_type, target_id, key)``, so overwriting a
``summary`` with a poorer one loses nothing that cannot be recovered:
``get_notes(..., include_history=True)`` returns the trail. Deleting writes an
empty-valued tombstone rather than removing rows.

Two more guards against silent loss, both in the write response:

- ``replaced`` (and ``replaced_at``) carry the value the write displaced, so an
  agent that forgot to carry a fact forward sees it in the same turn;
- ``if_unchanged_since`` refuses the write with ``conflict`` when somebody else
  wrote in between — two sessions of the same owner is the normal case here.

Media keeps its own store: ``target_type="media"`` is a thin alias over
``media_notes`` (one current value per key, feeding the transcript index), so
existing notes, transcripts and ``annotate_media`` keep working unchanged. The
only difference an agent sees is that media notes have a single history entry.

Targets are spelled canonically: a contact known as both ``<phone>@s.whatsapp.net``
and ``<lid>@lid`` is stored under the phone form, and reads look under every
spelling ``whatsapp._sender_aliases`` knows, so a note written before the LID
map learned the pair is still found afterwards. A message target is
``"<chat_jid>/<message_id>"`` because message IDs are unique per chat only.
"""

from __future__ import annotations

import os
import sqlite3
from collections.abc import Callable, Sequence
from datetime import UTC, datetime
from typing import Any

import media_notes
import whatsapp
from chat_policy import normalize_chat_entry
from errors import ToolError
from media_notes import MAX_SEARCH_LIMIT, MAX_VALUE_BYTES, normalize_key, normalize_sha256
from whatsapp import CHAT_POLICY

TARGET_TYPES = ("chat", "contact", "message", "media")
MODES = ("set", "append")
# What separates the entries of an append key. A newline keeps a dated log
# readable and lets the agent split it apart again when it rewrites one.
APPEND_SEPARATOR = "\n"

SCHEMA = """
CREATE TABLE IF NOT EXISTS notes (
    target_type TEXT NOT NULL,
    target_id TEXT NOT NULL,
    key TEXT NOT NULL,
    value TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    source TEXT NOT NULL DEFAULT '',
    version INTEGER NOT NULL,
    PRIMARY KEY (target_type, target_id, key, version)
);
"""


def _connect(create: bool) -> sqlite3.Connection | None:
    """Open notes.db; with create=False a missing file yields None instead of an empty database.

    Autocommit, because the write path needs an explicit ``BEGIN IMMEDIATE``:
    reading the current version and inserting the next one must be one
    transaction or two sessions hand out the same number.
    """
    path = media_notes.notes_db_path()
    if not create and not os.path.exists(path):
        return None
    conn = sqlite3.connect(path, timeout=whatsapp.SQLITE_BUSY_TIMEOUT_S, isolation_level=None)
    conn.execute("PRAGMA journal_mode=WAL")
    # Both tables: a media write goes through this connection too, and notes.db
    # may not exist yet when the first note an agent writes is about a file.
    conn.executescript(media_notes.SCHEMA)
    conn.executescript(SCHEMA)
    return conn


# --- Targets ---------------------------------------------------------------


# Only these two servers spell the same person two ways. Groups, newsletters,
# broadcasts and anything else WhatsApp adds later are left exactly as given:
# rewriting "<id>@newsletter" to "<id>@s.whatsapp.net" would collide with the DM
# of a phone number that is not the same conversation.
_ALIASED_SERVERS = ("s.whatsapp.net", "lid")


def _jid_spellings(jid: str) -> list[str]:
    """Every form this user or group JID may have been stored under."""
    raw = normalize_chat_entry(jid)
    user, _, server = raw.rpartition("@")
    if server not in _ALIASED_SERVERS or not user:
        return [raw]
    spellings = {raw}
    for alias in whatsapp._sender_aliases(user):
        # A different user part is what proves the map confirmed a pair. Its
        # fallback repeats the same digits under both servers, and reading a
        # LID's notes because a phone number happens to share its digits would
        # hand one person's notes to another.
        if "@" in alias and alias.split("@", 1)[0] != user:
            spellings.add(alias)
    return sorted(spellings)


def _canonical_jid(jid: str) -> str:
    """The one spelling notes are written under: the phone JID when it is known.

    An unmapped LID stays a LID — guessing ``<lid>@s.whatsapp.net`` would invent
    a phone number that belongs to somebody else.
    """
    raw = normalize_chat_entry(jid)
    user, _, server = raw.rpartition("@")
    if server != "lid" or not user:
        return raw
    for alias in whatsapp._sender_aliases(user):
        # A different user part means the LID map resolved this to a phone.
        if alias.endswith("@s.whatsapp.net") and alias.split("@", 1)[0] != user:
            return alias
    return raw


def _allowed(jid: str) -> bool:
    """Is any spelling of this conversation in the allow-list?

    WHATSAPP_ALLOWED_CHATS is matched literally, so a DM the archive only knows
    as ``<lid>@lid`` is listed under that spelling — while a note about it is
    stored under the phone form. Checking the canonical spelling alone would
    both refuse chats every read tool allows and accept chats they hide, so
    every spelling of the identity has a say.
    """
    if not CHAT_POLICY.restricted:
        return True
    return any(CHAT_POLICY.allows(spelling) for spelling in _jid_spellings(jid))


def _require_allowed(jid: str) -> None:
    if not _allowed(jid):
        # The caller's spelling, not the resolved one: it never supplied that.
        raise ToolError("denied", CHAT_POLICY.denial_message(jid))


def resolve_target(target_type: str, target_id: str) -> tuple[str, str, list[str]]:
    """(target type, canonical id, every id to read under), or a ToolError.

    The target is not checked for existence: a chat the agent just listed, a
    contact that only ever appears as a sender, and a message it read in the
    same call are all legitimate targets, and three extra lookups per note would
    buy little. ``WHATSAPP_ALLOWED_CHATS`` is checked: a contact JID is spelled
    exactly like its direct chat, and ``search_contacts`` already filters on it,
    so a note about a person outside the allow-list is out of reach too.
    """
    ttype = (target_type or "").strip().lower()
    if ttype not in TARGET_TYPES:
        raise ToolError("invalid_argument", f"target_type must be one of {', '.join(TARGET_TYPES)}")
    raw = (target_id or "").strip()
    if not raw:
        raise ToolError("invalid_argument", "target_id must not be empty")
    if ttype == "media":
        sha = normalize_sha256(raw)
        return ttype, sha, [sha]
    if ttype == "message":
        chat_raw, separator, message_id = raw.partition("/")
        message_id = message_id.strip()
        if not separator or not message_id or not chat_raw.strip():
            raise ToolError(
                "invalid_argument",
                'target_id for a message is "<chat_jid>/<message_id>"; IDs are unique per chat only',
            )
        _require_allowed(chat_raw)
        chat = _canonical_jid(chat_raw)
        return ttype, f"{chat}/{message_id}", [f"{s}/{message_id}" for s in _jid_spellings(chat_raw)]
    _require_allowed(raw)
    return ttype, _canonical_jid(raw), _jid_spellings(raw)


def _canonical_target(target_type: str, target_id: str) -> str:
    """The spelling ``resolve_target`` would have written this stored id under."""
    if target_type == "media":
        return target_id
    chat, separator, message_id = target_id.partition("/")
    canonical = _canonical_jid(chat)
    return f"{canonical}{separator}{message_id}" if separator else canonical


def _visible(target_type: str, target_id: str) -> bool:
    """Allow-list check for a stored row. Media has its own, by visible hash."""
    if target_type == "media":
        return True
    return _allowed(target_id.partition("/")[0])


# --- Values ----------------------------------------------------------------


def _check_size(value: str) -> None:
    if len(value.encode("utf-8")) > MAX_VALUE_BYTES:
        raise ToolError(
            "invalid_argument",
            f"value exceeds {MAX_VALUE_BYTES} bytes; read the note and replace it with a summary through compact()",
        )


def _next_value(previous: str, mode: str, value: str) -> str:
    if mode == "append":
        if not value.strip():
            raise ToolError("invalid_argument", "mode='append' needs a value; an empty value only deletes with 'set'")
        return f"{previous}{APPEND_SEPARATOR}{value}" if previous else value
    return value


def _check_unchanged(previous_at: str | None, if_unchanged_since: str | None) -> None:
    expected = (if_unchanged_since or "").strip()
    if not expected:
        return
    if previous_at != expected:
        raise ToolError(
            "conflict",
            f"note changed since {expected!r} (now {previous_at!r}); read it again and merge before writing",
        )


def _written(ttype: str, tid: str, key: str, previous: tuple[str, str] | None, **fields: Any) -> dict[str, Any]:
    """The common write response, carrying whatever the write displaced."""
    out: dict[str, Any] = {"success": True, "target_type": ttype, "target_id": tid, "key": key, **fields}
    if previous is not None:
        out["replaced"], out["replaced_at"] = previous
    return out


# --- Media alias -----------------------------------------------------------
#
# target_type="media" is the same note annotate_media writes, so the rows live in
# the media_notes table (one current value per key, feeding the transcript
# index) rather than in the versioned one. Reading and writing them through this
# connection is what keeps append and if_unchanged_since honest: the whole
# read-modify-write happens inside one BEGIN IMMEDIATE, as it does for the other
# three target types.


def _media_current(conn: sqlite3.Connection, sha: str, key: str) -> tuple[str, str] | None:
    row = conn.execute("SELECT value, updated_at FROM media_notes WHERE sha256 = ? AND key = ?", (sha, key)).fetchone()
    return (row[0], row[1]) if row and row[0] else None


def _media_write(conn: sqlite3.Connection, sha: str, key: str, value: str, now: str) -> None:
    """The rows annotate_media writes, in the caller's transaction."""
    if value:
        conn.execute(
            "INSERT INTO media_notes (sha256, key, value, updated_at) VALUES (?, ?, ?, ?)"
            " ON CONFLICT(sha256, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at",
            (sha, key, value, now),
        )
    else:
        conn.execute("DELETE FROM media_notes WHERE sha256 = ? AND key = ?", (sha, key))
    if key == media_notes.TRANSCRIPT_KEY:
        media_notes.index_transcript(conn, sha, value)


def _media_notes_view(sha: str, include_history: bool) -> dict[str, Any]:
    got = media_notes.get_media_notes(sha)
    view: dict[str, Any] = {
        "target_type": "media",
        "target_id": sha,
        "notes": got["notes"],
        "messages": got["messages"],
    }
    if include_history:
        # A media note keeps one version: the current value is the whole trail.
        view["history"] = [
            {
                "key": key,
                "value": note["value"],
                "updated_at": note["updated_at"],
                "source": "annotate_media",
                "version": 1,
            }
            for key, note in sorted(got["notes"].items())
        ]
    return view


# --- Public API ------------------------------------------------------------


def _versioned_current(
    conn: sqlite3.Connection, ttype: str, tid: str, ids: list[str], key: str
) -> tuple[tuple[str, str] | None, int]:
    """((value, updated_at) or None, highest version) for one key, across every spelling.

    The spellings share one version counter, so the row this write adds under the
    canonical id always outranks anything stored under an older spelling.
    """
    rows = conn.execute(
        "SELECT target_id, value, updated_at, MAX(version) FROM notes"
        f" WHERE target_type = ? AND key = ? AND target_id IN ({','.join('?' * len(ids))})"
        " GROUP BY target_id",
        [ttype, key, *ids],
    ).fetchall()
    if not rows:
        return None, 0
    best = max(rows, key=lambda row: (row[0] == tid, row[2], row[0]))
    # An empty stored value is a tombstone: there is nothing to replace.
    previous = (best[1], best[2]) if best[1] else None
    return previous, max(int(row[3]) for row in rows)


def annotate(
    target_type: str,
    target_id: str,
    key: str,
    value: str = "",
    mode: str = "set",
    if_unchanged_since: str | None = None,
    source: str = "",
) -> dict[str, Any]:
    """Write one note and report what it displaced.

    ``mode="set"`` replaces the whole value (an empty value deletes it);
    ``mode="append"`` adds a line to what is already there.
    """
    ttype, tid, ids = resolve_target(target_type, target_id)
    key = normalize_key(key)
    mode = (mode or "set").strip().lower()
    if mode not in MODES:
        raise ToolError("invalid_argument", f"mode must be one of {', '.join(MODES)}")
    value = value if value is not None else ""
    source = (source or mode).strip()[:32]
    if ttype == "media":
        # Before anything else: a note must never confirm that a file exists in
        # a chat the agent may not look at, not even through a conflict message.
        media_notes.require_visible_hash(tid)

    # Full precision: updated_at is the optimistic-locking token, and two writes
    # landing in the same second would otherwise share one.
    now = datetime.now(UTC).isoformat()
    conn = _connect(create=True)
    assert conn is not None
    try:
        conn.execute("BEGIN IMMEDIATE")
        if ttype == "media":
            previous, version = _media_current(conn, tid, key), 0
        else:
            previous, version = _versioned_current(conn, ttype, tid, ids, key)
        _check_unchanged(previous[1] if previous else None, if_unchanged_since)
        new_value = _next_value(previous[0] if previous else "", mode, value)
        _check_size(new_value)
        deleting = not new_value.strip()
        if deleting and previous is None:
            conn.rollback()
            return _written(ttype, tid, key, None, deleted=False)
        stored = "" if deleting else new_value
        if ttype == "media":
            _media_write(conn, tid, key, stored, now)
        else:
            conn.execute(
                "INSERT INTO notes (target_type, target_id, key, value, updated_at, source, version)"
                " VALUES (?, ?, ?, ?, ?, ?, ?)",
                (ttype, tid, key, stored, now, "delete" if deleting else source, version + 1),
            )
        conn.commit()
    except BaseException:
        conn.rollback()
        raise
    finally:
        conn.close()
    if deleting:
        return _written(ttype, tid, key, previous, deleted=True, version=version + 1)
    return _written(ttype, tid, key, previous, value=new_value, updated_at=now, version=version + 1, source=source)


def _current_rows(conn: sqlite3.Connection, ttype: str, tid: str, ids: list[str]) -> list[tuple[str, str, str]]:
    """Current (key, value, updated_at) per key across every spelling, tombstones dropped.

    SQLite pairs the bare columns with the row holding ``MAX(version)``, which is
    what makes one grouped query enough. Writes always land on the canonical
    spelling, so it wins outright: anything under an older spelling predates it,
    whatever the clock says.
    """
    rows = conn.execute(
        "SELECT target_id, key, value, updated_at, MAX(version) FROM notes"
        f" WHERE target_type = ? AND target_id IN ({','.join('?' * len(ids))})"
        " GROUP BY target_id, key",
        [ttype, *ids],
    ).fetchall()
    best: dict[str, tuple[str, str, str]] = {}
    for target_id, key, value, updated_at, _version in sorted(rows, key=lambda row: (row[0] == tid, row[3], row[0])):
        best[key] = (key, value, updated_at)
    return [row for row in best.values() if row[1]]


def compact(target_type: str, target_id: str, key: str, value: str) -> dict[str, Any]:
    """Replace an accumulated ``append`` key with a summary, in one write.

    The growth policy for append keys: a value is capped at 64 KB, a write that
    would cross the cap is refused, and this is how the agent makes room —
    read the log, summarise it, write the summary back. The whole log comes back
    as ``replaced`` and stays in the history, so compacting loses nothing.
    """
    if not (value or "").strip():
        raise ToolError(
            "invalid_argument", "compact needs the summary that replaces the log; annotate deletes a note instead"
        )
    return annotate(target_type, target_id, key, value, mode="set", source="compact")


def _listing_ids(target_type: str, target_id: str) -> list[str]:
    """The spellings a listing reads one target under: as given, plus the canonical one.

    Deliberately not the full ``_jid_spellings`` set: that asks the LID map about
    every row, and a page of fifty chats would pay fifty lookups for a case
    ``get_notes`` still covers — notes written under a LID before the map learned
    its phone number.
    """
    raw = (target_id or "").strip()
    if not raw:
        return []
    if target_type == "message":
        chat, separator, message_id = raw.partition("/")
        if not separator or not message_id:
            return []
        canonical = f"{_canonical_jid(chat)}{separator}{message_id}"
    else:
        raw = normalize_chat_entry(raw)
        canonical = _canonical_jid(raw)
    return [raw] if raw == canonical else [raw, canonical]


def fetch_notes_for(target_type: str, target_ids: Sequence[str]) -> dict[str, dict[str, str]]:
    """Current notes for many targets in one query: ``{target_id as given: {key: value}}``.

    This is what puts ``notes`` on a listing row without an N+1: one query per
    page, keyed by the id the caller passed, so a row looks its own notes up.
    Targets with nothing recorded are absent from the result.

    The allow-list is applied here rather than trusted from the caller: most
    callers pass rows a policy-filtered query produced, but ``get_contact``
    resolves an identifier of its own, and a note must not be the one place a
    blocked conversation shows through.
    """
    spellings = {
        tid: _listing_ids(target_type, tid) for tid in dict.fromkeys(target_ids) if tid and _visible(target_type, tid)
    }
    lookup = sorted({spelling for ids in spellings.values() for spelling in ids})
    if not lookup:
        return {}
    conn = _connect(create=False)
    if conn is None:
        return {}
    try:
        rows = conn.execute(
            "SELECT target_id, key, value, updated_at, MAX(version) FROM notes"
            f" WHERE target_type = ? AND target_id IN ({','.join('?' * len(lookup))})"
            " GROUP BY target_id, key",
            [target_type, *lookup],
        ).fetchall()
    finally:
        conn.close()
    by_id: dict[str, dict[str, tuple[str, str]]] = {}
    for target_id, key, value, updated_at, _version in rows:
        by_id.setdefault(target_id, {})[key] = (value, updated_at)
    out: dict[str, dict[str, str]] = {}
    for tid, ids in spellings.items():
        merged: dict[str, tuple[str, str]] = {}
        # _listing_ids puts the canonical spelling last, and that is where writes
        # land, so it overwrites anything left under an older one.
        for spelling in ids:
            merged.update(by_id.get(spelling, {}))
        current = {key: value for key, (value, _at) in merged.items() if value}
        if current:
            out[tid] = current
    return out


def attach_notes(
    rows: Sequence[dict[str, Any]],
    target_type: str,
    id_of: Callable[[dict[str, Any]], str],
    field: str = "notes",
    only_when_present: bool = False,
) -> None:
    """Put each row's notes on it, from one query for the whole page.

    ``only_when_present`` leaves unnoted rows untouched — right for message rows,
    where most of a page never carries a note and an empty mapping per row is
    noise. Chat and contact rows always get the field: an empty one is the signal
    that nobody has recorded anything about that conversation yet.
    """
    ids = [id_of(row) or "" for row in rows]
    found = fetch_notes_for(target_type, ids)
    for row, tid in zip(rows, ids, strict=True):
        current = found.get(tid, {})
        if current or not only_when_present:
            row[field] = current


def get_notes(target_type: str, target_id: str, include_history: bool = False) -> dict[str, Any]:
    """Current notes for one target, optionally with the whole version trail."""
    ttype, tid, ids = resolve_target(target_type, target_id)
    if ttype == "media":
        return _media_notes_view(tid, include_history)
    view: dict[str, Any] = {"target_type": ttype, "target_id": tid, "notes": {}}
    if include_history:
        view["history"] = []
    conn = _connect(create=False)
    if conn is None:
        return view
    try:
        view["notes"] = {
            key: {"value": value, "updated_at": updated_at}
            for key, value, updated_at in _current_rows(conn, ttype, tid, ids)
        }
        if include_history:
            view["history"] = [
                {"key": key, "value": value, "updated_at": updated_at, "source": source, "version": version}
                for key, value, updated_at, source, version in conn.execute(
                    "SELECT key, value, updated_at, source, version FROM notes"
                    f" WHERE target_type = ? AND target_id IN ({','.join('?' * len(ids))})"
                    " ORDER BY updated_at DESC, version DESC",
                    [ttype, *ids],
                )
            ]
    finally:
        conn.close()
    return view


def search_notes(
    query: str, key: str | None = None, target_type: str | None = None, limit: int = 50
) -> list[dict[str, Any]]:
    """Substring search over the current note values, across every target type."""
    needle = (query or "").strip()
    if not needle:
        raise ToolError("invalid_argument", "query must not be empty")
    wanted = (target_type or "").strip().lower()
    if wanted and wanted not in TARGET_TYPES:
        raise ToolError("invalid_argument", f"target_type must be one of {', '.join(TARGET_TYPES)}")
    limit = max(1, min(int(limit), MAX_SEARCH_LIMIT))
    key = normalize_key(key) if key else None

    hits: list[dict[str, Any]] = []
    if wanted in ("", "media"):
        hits += [
            {"target_type": "media", "target_id": hit["sha256"], **{k: hit[k] for k in ("key", "value", "updated_at")}}
            for hit in media_notes.search_media_notes(needle, key, limit)
        ]
    if wanted != "media":
        hits += _search_targets(needle, key, wanted, limit)
    # Each side already returns its own newest ``limit``, and the newest ``limit``
    # of the union can only be made of those, so the merge stays exact.
    hits.sort(key=lambda hit: hit["updated_at"], reverse=True)
    return hits[:limit]


def _search_targets(needle: str, key: str | None, target_type: str, limit: int) -> list[dict[str, Any]]:
    """Current notes matching ``needle``, allow-list applied. Media lives elsewhere.

    The matches are walked newest first in batches of ``media_notes.SEARCH_BATCH``
    and the walk stops once ``limit`` of them survive, so the per-row work below
    (an allow-list check and a LID-map lookup each) is paid for the rows walked
    to reach the answer, not for every note in the archive. Rows *ahead* of the
    survivors are still walked: an allow-list that hides the newest thousand
    matches costs a thousand checks, because the limit is applied here and not
    in the SQL — a page of matches the allow-list hides would otherwise come
    back as "nothing found".
    """
    conn = _connect(create=False)
    if conn is None:
        return []
    inner = ["target_type = ?"] if target_type else ["1=1"]
    params: list[Any] = [target_type] if target_type else []
    if key:
        inner.append("key = ?")
        params.append(key)
    try:
        # Group first, filter after: matching an older version whose replacement
        # no longer contains the needle would report a value nobody can read.
        rows = conn.execute(
            "SELECT target_type, target_id, key, value, updated_at FROM ("
            "  SELECT target_type, target_id, key, value, updated_at, MAX(version)"
            f"  FROM notes WHERE {' AND '.join(inner)} GROUP BY target_type, target_id, key"
            ") WHERE value <> '' AND (instr(lower(value), lower(?)) > 0 OR instr(value, ?) > 0)"
            " ORDER BY updated_at DESC",
            [*params, needle, needle],
        )
        # One hit per identity: a row left under an older spelling of the same
        # contact is shadowed by the canonical one exactly as get_notes shadows
        # it, so search never reports a value the target no longer carries —
        # including when the current value is the reason it stopped matching.
        # The first survivor takes the slot: rows arrive newest first, and a row
        # under a non-canonical spelling only gets here when the canonical
        # spelling holds nothing at all, so the two can never compete for one.
        best: dict[tuple[str, str, str], dict[str, Any]] = {}
        while len(best) < limit:
            batch = rows.fetchmany(media_notes.SEARCH_BATCH)
            if not batch:
                break
            for ttype, tid, hit_key, value, updated_at in batch:
                if not _visible(ttype, tid):
                    continue
                canonical = _canonical_target(ttype, tid)
                if canonical != tid and _has_note(conn, ttype, canonical, hit_key):
                    continue
                slot = (ttype, canonical, hit_key)
                if slot in best:
                    continue
                best[slot] = {
                    "target_type": ttype,
                    "target_id": canonical,
                    "key": hit_key,
                    "value": value,
                    "updated_at": updated_at,
                }
                if len(best) == limit:
                    break
    finally:
        conn.close()
    return list(best.values())


def _has_note(conn: sqlite3.Connection, ttype: str, tid: str, key: str) -> bool:
    """Is anything (a value or a tombstone) stored under this exact spelling?

    Only asked about hits that carry a non-canonical spelling, which the LID map
    makes rare: in the usual store this query never runs.
    """
    return (
        conn.execute(
            "SELECT 1 FROM notes WHERE target_type = ? AND target_id = ? AND key = ? LIMIT 1",
            (ttype, tid, key),
        ).fetchone()
        is not None
    )
