"""Background transcription of inbound voice notes (``TRANSCRIBE_ON_INGEST``).

``transcribe_audio`` only ever transcribes the one message an agent asks about,
so a voice-heavy archive stays unreadable until somebody walks it by hand. With
``TRANSCRIBE_ON_INGEST=1`` this module runs a single daemon thread inside the
MCP server process that does the walking: every ``TRANSCRIBE_ON_INGEST_INTERVAL_S``
seconds it looks for inbound audio rows whose bytes are cached and whose hash has
no transcript yet, transcribes up to ``TRANSCRIBE_ON_INGEST_BATCH`` of them
through the configured whisper backend, and stores the text with
``media_notes`` under the same keys ``transcribe_audio`` writes. With
``TRANSCRIBE_ON_INGEST_FETCH=1`` it also asks the bridge for audio whose bytes
are not cached, so auto-download off or a retention sweep no longer leaves the
archive silent.

Properties that matter:

- **Off by default.** Whisper on CPU is the most expensive thing this server can
  do; an operator opts in.
- **Idempotent by sha256.** The work list is "rows with no ``transcript`` note",
  so a restart resumes where it stopped and the same audio forwarded into three
  chats is transcribed once.
- **The walk always moves.** Each round looks at the newest rows, so a voice
  note that just arrived is transcribed next interval, and then at a page
  starting where the previous round stopped (``Position``), so audio it cannot
  read never hides the older audio it can.
- **Concurrency one.** One thread, one file at a time, sleeping between batches:
  the box keeps a core free for the tools.
- **Never blocks a tool call.** Its own SQLite connections, its own thread, and
  a failure anywhere is logged and dropped, never raised into the server.
- **Failures are not retried forever.** A file whisper cannot read gets a
  ``transcript_error`` note, which excludes it from the work list exactly like a
  transcript does. Clearing the note
  (``annotate_media(sha256, "transcript_error", "")``) queues it again.

``messages.db`` is the bridge's; this module only ever reads it. Everything it
writes goes to ``notes.db``, and ``WHATSAPP_ALLOWED_CHATS`` bounds what it can
see just like it bounds the tools.
"""

from __future__ import annotations

import functools
import itertools
import logging
import os
import sqlite3
import threading
import time
from collections.abc import Callable, Iterator, Mapping
from dataclasses import dataclass
from typing import Any

import media_inventory
import media_notes
import whatsapp
from errors import ToolError
from media_notes import TRANSCRIPT_ERROR_KEY, TRANSCRIPT_KEY, store_transcript
from tool_policy import parse_bool_env
from transcribe import TranscriptionError, load_config, transcribe_file
from whatsapp import CHAT_POLICY

logger = logging.getLogger("whatsapp_mcp")

ENABLED_ENV = "TRANSCRIBE_ON_INGEST"
INTERVAL_ENV = "TRANSCRIBE_ON_INGEST_INTERVAL_S"
BATCH_ENV = "TRANSCRIBE_ON_INGEST_BATCH"
FETCH_ENV = "TRANSCRIBE_ON_INGEST_FETCH"

DEFAULT_INTERVAL_S = 300.0
MIN_INTERVAL_S = 5.0
DEFAULT_BATCH = 10
MAX_BATCH = 200
# Rows walked per wanted transcript, past the newest ones. Audio whose bytes were
# never cached (or were purged) cannot be transcribed and would otherwise fill
# every round; the walk steps over it a page at a time (see ``Position``).
CANDIDATE_FACTOR = 5
# Consecutive failed downloads that end the fetching for a page: one bad file
# must not stop the batch, a bridge that is down must not be asked ten times.
MAX_FETCH_FAILURES = 3
# Downloads a round may spend on the newest rows while the walk is deeper in the
# archive, so a voice note that arrives with no bytes cached is not invisible
# until the walk wraps. Small on purpose, and below MAX_FETCH_FAILURES: newest
# rows the bridge refuses must not eat the budget the walk needs.
HEAD_FETCHES = 2
# A note value is capped at 64 KiB; a backend error message needs far less.
MAX_ERROR_CHARS = 500


@dataclass(frozen=True)
class IngestConfig:
    enabled: bool
    interval_s: float
    batch: int
    fetch: bool = False


@dataclass(frozen=True)
class Candidate:
    """One inbound voice note waiting for a transcript."""

    message_id: str
    chat_jid: str
    sha256: str
    path: str


@dataclass(frozen=True)
class Position:
    """Where the last round stopped walking the pending rows.

    ``(timestamp, id, chat_jid)`` is a total order over ``messages`` (the primary
    key is ``(id, chat_jid)``, and forwards reuse an id across chats), so a round
    resumes exactly past the last row it looked at instead of reading the same
    newest page again. ``None`` means "start at the newest row".
    """

    timestamp: str
    message_id: str
    chat_jid: str


@dataclass(frozen=True)
class Selection:
    """The candidates one round will transcribe, and where its walk stopped."""

    candidates: list[Candidate]
    examined: int
    position: Position | None


@dataclass(frozen=True)
class BatchResult:
    pending: int
    transcribed: int
    failed: int
    examined: int = 0
    position: Position | None = None


def load_ingest_config(env: Mapping[str, str] | None = None) -> IngestConfig:
    """Read the TRANSCRIBE_ON_INGEST_* variables; an unreadable value is an error."""
    source: Mapping[str, str] = os.environ if env is None else env
    enabled = parse_bool_env(source.get(ENABLED_ENV), ENABLED_ENV)
    return IngestConfig(
        enabled=enabled,
        interval_s=_parse_interval(source.get(INTERVAL_ENV)),
        batch=_parse_batch(source.get(BATCH_ENV)),
        fetch=parse_bool_env(source.get(FETCH_ENV), FETCH_ENV),
    )


def _parse_interval(raw: str | None) -> float:
    value = (raw or "").strip()
    if not value:
        return DEFAULT_INTERVAL_S
    try:
        seconds = float(value)
    except ValueError:
        raise ValueError(f"{INTERVAL_ENV}={raw!r} is not a number of seconds") from None
    if seconds <= 0:
        raise ValueError(f"{INTERVAL_ENV}={raw!r} must be positive")
    # A sub-second interval would spin the whisper backend, not poll it.
    return max(seconds, MIN_INTERVAL_S)


def _parse_batch(raw: str | None) -> int:
    value = (raw or "").strip()
    if not value:
        return DEFAULT_BATCH
    try:
        count = int(value)
    except ValueError:
        raise ValueError(f"{BATCH_ENV}={raw!r} is not an integer") from None
    if count <= 0:
        raise ValueError(f"{BATCH_ENV}={raw!r} must be positive")
    return min(count, MAX_BATCH)


def _already_handled_clause(conn: sqlite3.Connection) -> tuple[str, list[Any]]:
    """SQL predicate for "this hash has neither a transcript nor a recorded failure".

    notes.db is a separate file, so it is attached to the read connection for the
    query instead of pulling every transcribed hash into the statement as
    parameters. No notes.db (or no table in it yet) means nothing is handled.
    """
    path = media_notes.notes_db_path()
    if not os.path.exists(path):
        return "1", []
    conn.execute("ATTACH DATABASE ? AS notesdb", (path,))
    table = conn.execute("SELECT 1 FROM notesdb.sqlite_master WHERE type = 'table' AND name = 'media_notes'").fetchone()
    if not table:
        return "1", []
    return (
        "NOT EXISTS (SELECT 1 FROM notesdb.media_notes n "
        "WHERE n.sha256 = lower(hex(m.file_sha256)) AND n.key IN (?, ?))",
        [TRANSCRIPT_KEY, TRANSCRIPT_ERROR_KEY],
    )


PendingRow = tuple[str, str, str, str]  # message_id, chat_jid, sha256, timestamp


def _pending_rows(limit: int, after: Position | None = None) -> list[PendingRow]:
    """Inbound audio with no transcript, newest first, resuming just past ``after``."""
    conn = whatsapp._connect_messages_db()
    try:
        handled_clause, handled_params = _already_handled_clause(conn)
        clauses = [
            "m.media_type = 'audio'",
            "m.is_from_me = 0",
            "m.file_sha256 IS NOT NULL",
            "m.deleted_at IS NULL",
            handled_clause,
        ]
        params: list[Any] = list(handled_params)
        if after is not None:
            # Keyset, not OFFSET: rows transcribed since the last round leave the
            # result and would shift every offset under the walk.
            clauses.append("(m.timestamp < ? OR (m.timestamp = ? AND (m.id > ? OR (m.id = ? AND m.chat_jid > ?))))")
            params.extend([after.timestamp, after.timestamp, after.message_id, after.message_id, after.chat_jid])
        if CHAT_POLICY.restricted:
            clause, clause_params = CHAT_POLICY.sql_clause("m.chat_jid")
            clauses.append(clause)
            params.extend(clause_params)
        rows = conn.execute(
            f"""
            SELECT m.id, m.chat_jid, lower(hex(m.file_sha256)), m.timestamp
            FROM messages m
            WHERE {" AND ".join(clauses)}
            ORDER BY m.timestamp DESC, m.id, m.chat_jid
            LIMIT ?
            """,
            (*params, limit),
        ).fetchall()
    finally:
        conn.close()
    return [(str(r[0]), str(r[1]), str(r[2]), str(r[3])) for r in rows]


def _walk(head: list[PendingRow], page: list[PendingRow]) -> Iterator[tuple[PendingRow, bool]]:
    """The newest rows first, then the traversal page; the flag says which is which."""
    return itertools.chain(((row, False) for row in head), ((row, True) for row in page))


def find_pending(
    batch: int,
    *,
    fetch: bool = False,
    download: Callable[[str, str], str | None] | None = None,
    position: Position | None = None,
) -> Selection:
    """Up to ``batch`` voice notes to transcribe, one per distinct hash, and where to resume.

    Two pages are walked. The newest ``batch`` rows first, so a voice note that
    just arrived is never queued behind a backlog (skipped when ``position`` is
    ``None``: the page below already starts at the newest row). Then
    ``CANDIDATE_FACTOR`` times ``batch`` rows starting at ``position``: this is
    the walk that makes progress, so audio this process cannot read — never
    cached, purged, or a bridge that will not send it — is stepped over instead
    of filling every round with the same unprocessable prefix. The returned
    position is the last row the walk resolved, and ``None`` once it reached the
    oldest row: the next round starts again at the newest, and an older readable
    file is therefore reached within a bounded number of rounds.

    With ``fetch`` (``TRANSCRIBE_ON_INGEST_FETCH``) audio whose bytes are not
    cached is asked from the bridge over the same ``/api/download`` path
    ``transcribe_audio`` uses, so auto-download off or a retention sweep no
    longer silences the archive. ``batch`` bounds the downloads — failed
    attempts included, so a bridge that says no to everything cannot turn one
    round into ``CANDIDATE_FACTOR`` times as many requests — and
    ``MAX_FETCH_FAILURES`` failures in a row (what a bridge that is down looks
    like) end the fetching for a page. Most of that budget belongs to the walk:
    the newest rows may spend at most ``HEAD_FETCHES`` of it, and never its last
    download — enough to pick up an arrival whose bytes are not here, too little
    for a newest row the bridge refuses to eat the round. When the budget runs
    out mid-page the position stays at the last row that was *resolved*, so the
    next round asks for the rows this one could not try instead of striding over
    them: with the bridge refusing everything the walk still advances by the
    downloads it attempted, and the rest of the page is read anyway, so audio
    that is already cached further down is picked up in the same round. A file
    the bridge will never send is just skipped: it is not the file whisper could
    not read, so it gets no ``transcript_error`` note, and it is tried again
    when the walk comes round.
    """
    batch = max(1, batch)
    page_limit = batch * CANDIDATE_FACTOR
    head = _pending_rows(batch) if position is not None else []
    page = _pending_rows(page_limit, after=position)
    fetcher = download or whatsapp.download_media
    budget = batch if fetch else 0  # fetch attempts left this round
    head_budget = min(batch - 1, HEAD_FETCHES) if fetch else 0  # ... of which the newest rows may spend these
    failures = 0  # consecutive; a success clears them
    parked = False  # out of fetches: the walk stops advancing so nothing is stepped over untried
    walking = False
    caches: dict[str, dict[str, media_inventory.CachedFile]] = {}
    out: list[Candidate] = []
    seen: set[str] = set()
    looked_at: set[tuple[str, str]] = set()  # rows read this round, for the log line
    resolved: set[tuple[str, str]] = set()  # ... of which these are done with (the rest go to the walk)
    next_position = position
    full = False
    for (message_id, chat_jid, sha256, timestamp), traversing in _walk(head, page):
        if traversing and not walking:
            walking, failures = True, 0  # the walk gets its own three strikes
        if sha256 in seen or (message_id, chat_jid) in resolved:
            continue  # already picked, or the same row seen on both pages
        looked_at.add((message_id, chat_jid))
        if chat_jid not in caches:
            caches[chat_jid] = media_inventory.scan_chat_cache(chat_jid)
        cached = caches[chat_jid].get(message_id)
        if cached is None and not traversing:
            if head_budget <= 0 or budget <= 0:
                continue  # the newest rows are out of downloads; the walk still owns the rest
            head_budget -= 1
        if cached is None and traversing and fetch and budget <= 0:
            parked = True
        if traversing and not parked:
            next_position = Position(timestamp=timestamp, message_id=message_id, chat_jid=chat_jid)
        path: str | None = None
        if cached is not None:
            path = os.path.join(media_inventory.chat_media_dir(chat_jid), cached.name)
        elif budget > 0:
            budget -= 1
            path = _fetch_bytes(message_id, chat_jid, fetcher, caches, quiet=failures > 0)
            if path is None:
                failures += 1
                if failures >= MAX_FETCH_FAILURES:
                    budget = 0
                    logger.warning(
                        "transcribe_on_ingest: %d fetches failed in a row, no more this round", MAX_FETCH_FAILURES
                    )
            else:
                failures = 0
        resolved.add((message_id, chat_jid))
        if path is None:
            continue  # not fetching, done fetching, or the bridge would not send it
        seen.add(sha256)
        out.append(Candidate(message_id=message_id, chat_jid=chat_jid, sha256=sha256, path=path))
        if len(out) >= batch:
            full = True
            break
    if not full and not parked and len(page) < page_limit:
        next_position = None  # the walk reached the oldest row; start over at the newest
    return Selection(candidates=out, examined=len(looked_at), position=next_position)


def _fetch_bytes(
    message_id: str,
    chat_jid: str,
    download: Callable[[str, str], str | None],
    caches: dict[str, dict[str, media_inventory.CachedFile]],
    *,
    quiet: bool,
) -> str | None:
    """Ask the bridge to cache one message's media and return the local path, or None.

    The path the bridge answers with is the bridge's; this process looks the file
    up in its own view of the chat directory first, so a store mounted under two
    names still works. ``caches`` is refreshed for that chat on the way.
    """
    try:
        path = download(message_id, chat_jid)
    except Exception as exc:  # noqa: BLE001 - one message must not end the round
        _log_fetch_problem(quiet, f"the bridge could not send {message_id}: {exc}")
        return None
    caches[chat_jid] = media_inventory.scan_chat_cache(chat_jid)
    cached = caches[chat_jid].get(message_id)
    if cached is not None:
        return os.path.join(media_inventory.chat_media_dir(chat_jid), cached.name)
    if path and os.path.exists(path):
        return path
    _log_fetch_problem(quiet, f"the bytes of {message_id} are not readable here ({path or 'no path'})")
    return None


def _log_fetch_problem(quiet: bool, message: str) -> None:
    """One warning per round about the bridge; the rest of the round is debug."""
    log = logger.debug if quiet else logger.warning
    log("transcribe_on_ingest: %s", message)


def _default_transcribe(path: str) -> dict[str, Any]:
    return transcribe_file(path, config=load_config())


def _record_failure(sha256: str, reason: str) -> None:
    """Park a file that cannot be transcribed so the next batch skips it."""
    try:
        media_notes.annotate_media(sha256, TRANSCRIPT_ERROR_KEY, reason[:MAX_ERROR_CHARS] or "unknown error")
    except (ToolError, sqlite3.Error) as exc:
        logger.warning("transcribe_on_ingest: could not record the failure of %s: %s", sha256[:12], exc)


def run_once(
    batch: int,
    *,
    transcribe: Callable[[str], dict[str, Any]] | None = None,
    fetch: bool = False,
    download: Callable[[str, str], str | None] | None = None,
    position: Position | None = None,
) -> BatchResult:
    """Transcribe one batch from ``position``. Never raises: a broken batch is a logged batch."""
    transcribe = transcribe or _default_transcribe
    started = time.monotonic()
    try:
        selection = find_pending(batch, fetch=fetch, download=download, position=position)
    except Exception as exc:  # noqa: BLE001 - the work list must not kill the thread
        logger.warning("transcribe_on_ingest: could not read the archive: %s", exc)
        return BatchResult(0, 0, 0, position=position)  # the read failed, the walk did not move

    pending = selection.candidates
    transcribed = failed = 0
    for candidate in pending:
        try:
            result = transcribe(candidate.path)
            text = str(result.get("text") or "").strip()
            if not text:
                raise TranscriptionError("whisper returned no text")
            store_transcript(candidate.sha256, {**result, "text": text})
        except Exception as exc:  # noqa: BLE001 - one bad file must not end the batch
            failed += 1
            _record_failure(candidate.sha256, f"{type(exc).__name__}: {exc}")
            logger.warning("transcribe_on_ingest: %s failed: %s", os.path.basename(candidate.path), exc)
            continue
        transcribed += 1

    if pending:
        logger.info(
            "transcribe_on_ingest: %d examined, %d pending, %d transcribed, %d failed in %.1fs",
            selection.examined,
            len(pending),
            transcribed,
            failed,
            time.monotonic() - started,
        )
    else:
        logger.debug("transcribe_on_ingest: nothing pending in %d rows examined", selection.examined)
    return BatchResult(len(pending), transcribed, failed, selection.examined, selection.position)


def run_forever(
    config: IngestConfig,
    *,
    stop: threading.Event,
    sleep: Callable[[float], None] = time.sleep,
    run: Callable[..., BatchResult] | None = None,
) -> None:
    """Poll until ``stop`` is set. ``sleep`` and ``run`` are injected by the tests.

    The traversal position is carried from one round to the next: that is what
    makes the walk advance past audio it cannot read instead of restarting on
    the same newest page every interval. It lives in this loop only — a restart
    starts at the newest rows again, which is where the new work is.
    """
    run_batch = run or functools.partial(run_once, fetch=config.fetch)
    position: Position | None = None
    while not stop.is_set():
        position = run_batch(config.batch, position=position).position
        if stop.is_set():
            return
        sleep(config.interval_s)


def start_worker(
    config: IngestConfig,
    *,
    stop: threading.Event | None = None,
    sleep: Callable[[float], None] = time.sleep,
    run: Callable[..., BatchResult] | None = None,
) -> threading.Thread:
    """Start the poll loop on a daemon thread (so it never holds up shutdown)."""
    event = stop or threading.Event()
    thread = threading.Thread(
        target=run_forever,
        args=(config,),
        kwargs={"stop": event, "sleep": sleep, "run": run},
        name="transcribe-on-ingest",
        daemon=True,
    )
    thread.start()
    return thread


def install_ingest_worker(env: Mapping[str, str] | None = None) -> threading.Thread | None:
    """Start the worker when the operator asked for it and a backend exists.

    Called once at startup, for every transport. Returns None (after one log
    line) when it stays off, so a misread of the switch is visible in the log.
    """
    config = load_ingest_config(env)
    if not config.enabled:
        return None
    if load_config(env).backend is None:
        logger.warning(
            "%s=1 but no whisper backend is configured; the worker stays off (WHISPER_URL / WHISPER_BIN)", ENABLED_ENV
        )
        return None
    logger.info(
        "%s=1: transcribing up to %d inbound voice notes every %.0fs (whisper on this machine)%s",
        ENABLED_ENV,
        config.batch,
        config.interval_s,
        f", fetching uncached audio from the bridge ({FETCH_ENV}=1)" if config.fetch else "",
    )
    return start_worker(config)
