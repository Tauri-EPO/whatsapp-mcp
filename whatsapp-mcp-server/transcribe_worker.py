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
import logging
import os
import sqlite3
import threading
import time
from collections.abc import Callable, Mapping
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
# Rows read per wanted transcript. Audio whose bytes were never cached (or were
# purged) cannot be transcribed and would otherwise fill every batch; reading a
# few times the batch size lets the newest cached rows through anyway.
CANDIDATE_FACTOR = 5
# Consecutive failed downloads that end the fetching for a round: one bad file
# must not stop the batch, a bridge that is down must not be asked ten times.
MAX_FETCH_FAILURES = 3
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
class BatchResult:
    pending: int
    transcribed: int
    failed: int


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


def _pending_rows(limit: int) -> list[tuple[str, str, str]]:
    """(message_id, chat_jid, sha256) for inbound audio with no transcript, newest first."""
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
        if CHAT_POLICY.restricted:
            clause, clause_params = CHAT_POLICY.sql_clause("m.chat_jid")
            clauses.append(clause)
            params.extend(clause_params)
        rows = conn.execute(
            f"""
            SELECT m.id, m.chat_jid, lower(hex(m.file_sha256))
            FROM messages m
            WHERE {" AND ".join(clauses)}
            ORDER BY m.timestamp DESC, m.id
            LIMIT ?
            """,
            (*params, limit),
        ).fetchall()
    finally:
        conn.close()
    return [(str(r[0]), str(r[1]), str(r[2])) for r in rows]


def find_pending(
    batch: int,
    *,
    fetch: bool = False,
    download: Callable[[str, str], str | None] | None = None,
) -> list[Candidate]:
    """Up to ``batch`` voice notes to transcribe, newest first, one per distinct hash.

    With ``fetch`` (``TRANSCRIBE_ON_INGEST_FETCH``) audio whose bytes are not
    cached is asked from the bridge over the same ``/api/download`` path
    ``transcribe_audio`` uses, so auto-download off or a retention sweep no
    longer silences the archive. ``batch`` bounds the downloads too — failed
    attempts included, so a bridge that says no to everything cannot turn one
    round into ``CANDIDATE_FACTOR`` times as many requests — and
    ``MAX_FETCH_FAILURES`` failures in a row (what a bridge that is down looks
    like) end the fetching for the round. A file the bridge will never send is
    just skipped: it is not the file whisper could not read, so it gets no
    ``transcript_error`` note, and the next round tries it again.
    """
    rows = _pending_rows(max(1, batch) * CANDIDATE_FACTOR)
    fetcher = download or whatsapp.download_media
    budget = max(1, batch) if fetch else 0  # fetch attempts left this round
    failures = 0  # consecutive; a success clears them
    caches: dict[str, dict[str, media_inventory.CachedFile]] = {}
    out: list[Candidate] = []
    seen: set[str] = set()
    for message_id, chat_jid, sha256 in rows:
        if sha256 in seen:
            continue
        if chat_jid not in caches:
            caches[chat_jid] = media_inventory.scan_chat_cache(chat_jid)
        cached = caches[chat_jid].get(message_id)
        if cached is not None:
            path = os.path.join(media_inventory.chat_media_dir(chat_jid), cached.name)
        elif budget <= 0:
            continue  # not fetching (or done fetching): nothing on disk to read
        else:
            budget -= 1
            fetched = _fetch_bytes(message_id, chat_jid, fetcher, caches, quiet=failures > 0)
            if fetched is None:
                failures += 1
                if failures >= MAX_FETCH_FAILURES:
                    budget = 0
                    logger.warning(
                        "transcribe_on_ingest: %d fetches failed in a row, no more this round", MAX_FETCH_FAILURES
                    )
                continue
            failures = 0
            path = fetched
        seen.add(sha256)
        out.append(Candidate(message_id=message_id, chat_jid=chat_jid, sha256=sha256, path=path))
        if len(out) >= batch:
            break
    return out


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
) -> BatchResult:
    """Transcribe one batch. Never raises: a broken batch is a logged batch."""
    transcribe = transcribe or _default_transcribe
    started = time.monotonic()
    try:
        pending = find_pending(batch, fetch=fetch, download=download)
    except Exception as exc:  # noqa: BLE001 - the work list must not kill the thread
        logger.warning("transcribe_on_ingest: could not read the archive: %s", exc)
        return BatchResult(0, 0, 0)

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
            "transcribe_on_ingest: %d pending, %d transcribed, %d failed in %.1fs",
            len(pending),
            transcribed,
            failed,
            time.monotonic() - started,
        )
    else:
        logger.debug("transcribe_on_ingest: nothing pending")
    return BatchResult(len(pending), transcribed, failed)


def run_forever(
    config: IngestConfig,
    *,
    stop: threading.Event,
    sleep: Callable[[float], None] = time.sleep,
    run: Callable[[int], BatchResult] | None = None,
) -> None:
    """Poll until ``stop`` is set. ``sleep`` and ``run`` are injected by the tests."""
    run_batch = run or functools.partial(run_once, fetch=config.fetch)
    while not stop.is_set():
        run_batch(config.batch)
        if stop.is_set():
            return
        sleep(config.interval_s)


def start_worker(
    config: IngestConfig,
    *,
    stop: threading.Event | None = None,
    sleep: Callable[[float], None] = time.sleep,
    run: Callable[[int], BatchResult] | None = None,
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
