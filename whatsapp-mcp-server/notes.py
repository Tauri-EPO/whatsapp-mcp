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
and ``<lid>@lid`` is stored under the phone form. Brazilian mobile spellings
share a deterministic 13-digit phone key, including the ninth digit,
regardless of which archive rows exist. Authorization checks the caller's spelling. Reads look under admitted
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
CREATE TABLE IF NOT EXISTS note_origins (
    target_type TEXT NOT NULL,
    target_id TEXT NOT NULL,
    key TEXT NOT NULL,
    version INTEGER NOT NULL,
    spelling TEXT NOT NULL,
    PRIMARY KEY (target_type, target_id, key, version)
);
"""

# Old rows authorize by their stored spelling. New canonical rows retain the
# spelling whose authorization admitted the write, including after policy changes.
_ORIGIN_SQL = (
    "COALESCE((SELECT spelling FROM note_origins o WHERE o.target_type = notes.target_type"
    " AND o.target_id = notes.target_id AND o.key = notes.key AND o.version = notes.version), notes.target_id)"
)


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


def _jid_spellings(jid: str) -> list[str]:
    """Every confirmed form this note identity may have been stored under."""
    raw = normalize_chat_entry(jid)
    return whatsapp.note_jid_spellings([raw])[raw]


def _canonical_jid(jid: str) -> str:
    """The note key: a Brazilian mobile always includes the ninth digit.

    An unmapped LID stays a LID — guessing ``<lid>@s.whatsapp.net`` would invent
    a phone number that belongs to somebody else.
    """
    raw = normalize_chat_entry(jid)
    return whatsapp.canonical_note_jids([raw])[raw]


def _allowed(jid: str) -> bool:
    """Authorize the supplied spelling; a storage key never grants access."""
    return jid.count("@") <= 1 and CHAT_POLICY.allows(normalize_chat_entry(jid))


def _require_allowed(jid: str) -> None:
    whatsapp._require_unambiguous_identifier(jid)
    if not _allowed(jid):
        # The caller's spelling, not the resolved one: it never supplied that.
        raise ToolError("denied", CHAT_POLICY.denial_message(jid))


def resolve_target(target_type: str, target_id: str) -> tuple[str, str, list[str]]:
    """(target type, canonical id, every id to read under), or a ToolError.

    The target is not checked for existence: a chat the agent just listed, a
    contact that only ever appears as a sender, and a message it read in the
    same call are all legitimate targets. Batched read-only metadata selects
    the canonical spelling without creating an archive row. ``WHATSAPP_ALLOWED_CHATS`` is checked: a contact JID is spelled
    exactly like its direct chat. Contact search also reads the address book
    outside that list, but a note requires the supplied spelling to be allowed.
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
        return ttype, f"{chat}/{message_id}", _listing_ids(ttype, raw)
    _require_allowed(raw)
    canonical = _canonical_jid(raw)
    return ttype, canonical, _listing_ids(ttype, raw)


def _canonical_target(target_type: str, target_id: str, canonical_jids: dict[str, str] | None = None) -> str:
    """The spelling ``resolve_target`` would have written this stored id under."""
    if target_type == "media":
        return target_id
    chat, separator, message_id = target_id.partition("/")
    chat = normalize_chat_entry(chat)
    canonical = canonical_jids[chat] if canonical_jids is not None else _canonical_jid(chat)
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
        f"SELECT target_id, value, updated_at, MAX(version), {_ORIGIN_SQL} FROM notes"
        f" WHERE target_type = ? AND key = ? AND target_id IN ({','.join('?' * len(ids))})"
        " GROUP BY target_id",
        [ttype, key, *ids],
    ).fetchall()
    if not rows:
        return None, 0
    visible = [row for row in rows if _visible(ttype, row[4])]
    best = max(visible, key=lambda row: (row[0] == tid, row[2], row[0])) if visible else None
    # An empty stored value is a tombstone: there is nothing to replace.
    previous = (best[1], best[2]) if best and best[1] else None
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
            origin = normalize_chat_entry(target_id.partition("/")[0])
            conn.execute("INSERT INTO note_origins VALUES (?, ?, ?, ?, ?)", (ttype, tid, key, version + 1, origin))
            # Move the current value to its canonical key in this transaction.
            # Original rows remain in the shared history; alias tombstones
            # close their current values without overwriting that history.
            aliases = conn.execute(
                "SELECT target_id, value, MAX(version) FROM notes"
                f" WHERE target_type = ? AND key = ? AND target_id IN ({','.join('?' * len(ids))}) AND target_id <> ?"
                " GROUP BY target_id HAVING value <> ''",
                [ttype, key, *ids, tid],
            ).fetchall()
            conn.executemany(
                "INSERT INTO notes VALUES (?, ?, ?, '', ?, 'alias', ?)",
                [(ttype, alias, key, now, version + 1) for alias, *_ in aliases],
            )
            conn.executemany(
                "INSERT INTO note_origins VALUES (?, ?, ?, ?, ?)",
                [(ttype, alias, key, version + 1, origin) for alias, *_ in aliases],
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
        f"SELECT target_id, key, value, updated_at, MAX(version), {_ORIGIN_SQL} FROM notes"
        f" WHERE target_type = ? AND target_id IN ({','.join('?' * len(ids))})"
        " GROUP BY target_id, key",
        [ttype, *ids],
    ).fetchall()
    best: dict[str, tuple[str, str, str]] = {}
    for target_id, key, value, updated_at, _version, origin in sorted(
        rows, key=lambda row: (row[0] == tid, row[3], row[0])
    ):
        if _visible(ttype, origin):
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


def _listing_ids(
    target_type: str,
    target_id: str,
    canonical_jids: dict[str, str] | None = None,
    aliases: dict[str, list[str]] | None = None,
) -> list[str]:
    """The spellings a listing reads one target under: as given, plus the canonical one.

    The page supplies one batched alias map instead of looking up each row.
    """
    raw = (target_id or "").strip()
    if not raw:
        return []
    if target_type == "message":
        chat, separator, message_id = raw.partition("/")
        if not separator or not message_id:
            return []
        chat = normalize_chat_entry(chat)
    else:
        raw = normalize_chat_entry(raw)
        chat, separator, message_id = raw, "", ""
    canonical_chat = canonical_jids[chat] if canonical_jids is not None else _canonical_jid(chat)
    spellings = aliases[chat] if aliases is not None else _jid_spellings(chat)
    # Canonical last: it must shadow every legacy value, including tombstones.
    ids = [
        f"{jid}{separator}{message_id}"
        for jid in dict.fromkeys(spellings)
        if jid and jid != canonical_chat and _allowed(jid)
    ]
    return [*ids, f"{canonical_chat}{separator}{message_id}"]


def fetch_notes_for(target_type: str, target_ids: Sequence[str]) -> dict[str, dict[str, str]]:
    """Current notes with batched identity and bounded queries: ``{target_id as given: {key: value}}``.

    This puts ``notes`` on a listing row without an N+1: bounded batches per
    page, keyed by the id the caller passed, so a row looks its own notes up.
    Targets with nothing recorded are absent from the result.

    The allow-list is applied here rather than trusted from the caller: most
    callers pass rows a policy-filtered query produced, but ``get_contact``
    resolves an identifier of its own, and a note must not be the one place a
    blocked conversation shows through.
    """
    visible = [tid for tid in dict.fromkeys(target_ids) if tid and _visible(target_type, tid)]
    canonical = whatsapp.canonical_note_jids([tid.partition("/")[0] for tid in visible])
    aliases = whatsapp.note_jid_spellings([tid.partition("/")[0] for tid in visible], canonical)
    spellings = {tid: _listing_ids(target_type, tid, canonical, aliases) for tid in visible}
    lookup = sorted({spelling for ids in spellings.values() for spelling in ids})
    if not lookup:
        return {}
    conn = _connect(create=False)
    if conn is None:
        return {}
    try:
        # Every chunk sees the same snapshot, like the former single SELECT.
        conn.execute("BEGIN")
        size = max(1, min(whatsapp._SQL_IN_CHUNK, conn.getlimit(sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER) - 1))
        rows = []
        for chunk in whatsapp._in_chunks(lookup, size):
            rows.extend(
                conn.execute(
                    f"SELECT target_id, key, value, updated_at, MAX(version), {_ORIGIN_SQL} FROM notes"
                    f" WHERE target_type = ? AND target_id IN ({','.join('?' * len(chunk))})"
                    " GROUP BY target_id, key",
                    [target_type, *chunk],
                ).fetchall()
            )
    finally:
        conn.close()
    by_id: dict[str, dict[str, tuple[str, str]]] = {}
    for target_id, key, value, updated_at, _version, origin in rows:
        if _visible(target_type, origin):
            by_id.setdefault(target_id, {})[key] = (value, updated_at)
    out: dict[str, dict[str, str]] = {}
    for tid, ids in spellings.items():
        merged: dict[str, tuple[str, str]] = {}
        # Use the same winning row as _current_rows: canonical first, then
        # update time and JID among legacy aliases, including tombstones.
        candidates = [
            (spelling, key, value, at) for spelling in ids for key, (value, at) in by_id.get(spelling, {}).items()
        ]
        for spelling, key, value, at in sorted(candidates, key=lambda row: (row[0] == ids[-1], row[3], row[0])):
            merged[key] = (value, at)
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
    """Put each row's notes on it, from batched queries for the whole page.

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
                for key, value, updated_at, source, version, origin in conn.execute(
                    f"SELECT key, value, updated_at, source, version, {_ORIGIN_SQL} FROM notes"
                    f" WHERE target_type = ? AND target_id IN ({','.join('?' * len(ids))})"
                    " ORDER BY updated_at DESC, version DESC",
                    [ttype, *ids],
                )
                if _visible(ttype, origin)
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
    limit = whatsapp.page_size(limit, MAX_SEARCH_LIMIT)
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
            "SELECT target_type, target_id, key, value, updated_at, origin FROM ("
            f"  SELECT target_type, target_id, key, value, updated_at, MAX(version), {_ORIGIN_SQL} AS origin"
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
            canonical_jids = whatsapp.canonical_note_jids(
                [tid.partition("/")[0] for ttype, tid, *_rest in batch if ttype != "media"]
            )
            for ttype, tid, hit_key, value, updated_at, origin in batch:
                if not _visible(ttype, origin):
                    continue
                canonical = _canonical_target(ttype, tid, canonical_jids)
                if canonical != tid:
                    chat, separator, message_id = tid.partition("/")
                    aliases = [f"{jid}{separator}{message_id}" for jid in _jid_spellings(chat)]
                    current = _current_rows(conn, ttype, canonical, list(dict.fromkeys([canonical, *aliases])))
                    if (hit_key, value, updated_at) not in current:
                        continue
                slot = (ttype, canonical, hit_key)
                if slot in best:
                    continue
                _, separator, message_id = tid.partition("/")
                visible_id = f"{origin.partition('/')[0]}{separator}{message_id}" if separator else origin
                best[slot] = {
                    "target_type": ttype,
                    "target_id": canonical if canonical == origin or _visible(ttype, canonical) else visible_id,
                    "key": hit_key,
                    "value": value,
                    "updated_at": updated_at,
                }
                if len(best) == limit:
                    break
    finally:
        conn.close()
    return list(best.values())
