"""Durable UTC transcription accounting and atomic admission in MCP-owned notes.db."""

from __future__ import annotations

import contextlib
import logging
import math
import os
import queue
import re
import sqlite3
import subprocess
import threading
import time
import uuid
from dataclasses import dataclass
from datetime import UTC, datetime

from errors import ToolError
from media_notes import notes_db_path
from private_files import notes_connection
from runtime_settings import snapshot

SCHEMA = """
CREATE TABLE IF NOT EXISTS transcription_usage (
 month TEXT, provider TEXT, model TEXT, source TEXT, seconds REAL NOT NULL, requests INTEGER NOT NULL,
 PRIMARY KEY(month,provider,model,source));
CREATE TABLE IF NOT EXISTS transcription_reservations (
 id TEXT PRIMARY KEY, month TEXT, source TEXT, seconds REAL NOT NULL);
CREATE TABLE IF NOT EXISTS transcription_outcomes (
 provider TEXT, outcome TEXT, requests INTEGER NOT NULL, PRIMARY KEY(provider,outcome));
"""
logger = logging.getLogger("whatsapp_mcp")
_pause_lock = threading.Lock()
_paused = False
_completion_slots = threading.BoundedSemaphore(256)
_pending = queue.Queue(maxsize=256)
_reconciler_lock = threading.Lock()
_reconciler_started = False


def month_now() -> str:
    return datetime.now(UTC).strftime("%Y-%m")


def _connection(create=True, timeout: float = 5):
    expires = time.monotonic() + timeout
    while True:
        try:
            conn = notes_connection(notes_db_path(), create=create, timeout=max(0, expires - time.monotonic()))
            break
        except sqlite3.OperationalError as exc:
            # Concurrent WAL initialization can return BUSY without invoking
            # SQLite's busy handler. Retry within the caller's original budget.
            remaining = expires - time.monotonic()
            if (
                getattr(exc, "sqlite_errorcode", 0) & 0xFF not in (sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED)
                or remaining <= 0
            ):
                raise
            time.sleep(min(0.01, remaining))
    if conn is not None:
        try:
            conn.execute(f"PRAGMA busy_timeout={max(1, int(max(0, expires - time.monotonic()) * 1000))}")
            initialized = (
                conn.execute(
                    "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN "
                    "('transcription_usage','transcription_reservations','transcription_outcomes')"
                ).fetchone()[0]
                == 3
            )
            if not initialized:
                # Serialize first-use DDL; initialized observations only read.
                conn.executescript("BEGIN IMMEDIATE;\n" + SCHEMA + "\nCOMMIT;")
        except BaseException:
            conn.close()
            raise
    return conn


def _budget(deadline):
    if deadline is None:
        return 5
    from transcribe import _remaining

    return min(5, _remaining(deadline))


def limits(deadline=None):
    settings = snapshot(timeout_s=_budget(deadline))["settings"]
    minutes = settings["transcription.monthly_max_minutes"]["value"]
    return (None if minutes is None else minutes * 60), settings["transcription.cap_scope"]["value"]


def _exceeds_quota(seconds: float, cap: float) -> bool:
    # One nanosecond absorbs floating-point addition noise, far below one
    # decoded 16-kHz sample; it cannot admit another audio sample over the cap.
    return seconds > cap and not math.isclose(seconds, cap, rel_tol=0, abs_tol=1e-9)


def _used(conn, month, scope):
    suffix = " AND source='ingest'" if scope == "ingest" else ""
    return sum(
        conn.execute(f"SELECT COALESCE(SUM(seconds),0) FROM {table} WHERE month=?{suffix}", (month,)).fetchone()[0]
        for table in ("transcription_usage", "transcription_reservations")
    )


def current_usage():
    month, (cap, scope) = month_now(), limits()
    sources = {source: {"seconds": 0.0, "requests": 0} for source in ("tool", "ingest")}
    used = 0.0
    conn = _connection(create=False)
    if conn is not None:
        try:
            conn.execute("BEGIN")
            for source, seconds, requests in conn.execute(
                "SELECT source,SUM(seconds),SUM(requests) FROM transcription_usage WHERE month=? GROUP BY source",
                (month,),
            ):
                sources[source] = {"seconds": seconds, "requests": requests}
            used = _used(conn, month, scope)
        finally:
            conn.close()
    return {
        "month": month,
        "seconds": sum(v["seconds"] for v in sources.values()),
        "requests": sum(v["requests"] for v in sources.values()),
        "by_source": sources,
        "cap_scope": scope,
        "cap_seconds": cap,
        "remaining_seconds": None if cap is None else max(0, cap - used),
    }


def audio_duration(path: str, deadline: float | None = None) -> float:
    if not os.path.isfile(path):
        raise FileNotFoundError(path)
    from transcribe import BackendUnavailableError, TranscriptionError, ffmpeg_timeout_s

    try:
        result = subprocess.run(
            [
                "ffmpeg",
                "-nostdin",
                "-hide_banner",
                "-loglevel",
                "error",
                "-protocol_whitelist",
                "file,pipe",
                "-format_whitelist",
                "ogg,wav,mp3,flac,aac,mov,matroska,amr,aiff,au",
                "-i",
                path,
                "-map",
                "0:a:0",
                "-vn",
                "-ac",
                "1",
                "-ar",
                "16000",
                "-af",
                "asetpts=N/SR/TB",
                "-c:a",
                "pcm_s16le",
                "-t",
                "86400.0625",
                "-f",
                "s16le",
                "-progress",
                "pipe:1",
                "-y",
                os.devnull,
            ],
            stdin=subprocess.DEVNULL,
            capture_output=True,
            text=True,
            check=True,
            timeout=min(ffmpeg_timeout_s(), max(0.001, deadline - time.monotonic()))
            if deadline is not None
            else ffmpeg_timeout_s(),
        )
        # Count decoded PCM samples, not container duration metadata: chained
        # Ogg streams can report only their last stream. PCM is discarded by
        # the OS; progress output stays small and no decoded file is retained.
        sizes = re.findall(r"^total_size=(\d+)$", result.stdout, re.MULTILINE)
        seconds = int(sizes[-1]) / 32000 if sizes else 0
        if not 0 < seconds <= 86400:
            raise ValueError
        return seconds
    except FileNotFoundError:
        raise BackendUnavailableError("Transcription metering requires ffmpeg") from None
    except subprocess.TimeoutExpired:
        if deadline is not None and time.monotonic() >= deadline:
            raise BackendUnavailableError("HTTP transcription exceeded whole-file deadline") from None
        raise TranscriptionError("Audio duration probe timed out") from None
    except (subprocess.SubprocessError, ValueError, TypeError, KeyError):
        raise TranscriptionError("Transcription input is not supported audio or has no readable duration") from None


def _pause(value):
    global _paused
    with _pause_lock:
        if _paused != value:
            _paused = value
            logger.info("transcribe_on_ingest: monthly quota %s", "paused" if value else "resumed")


def ingest_quota_exhausted(required_seconds: float = 0):
    remaining = current_usage()["remaining_seconds"]
    if remaining is not None and (remaining <= 0 or _exceeds_quota(required_seconds, remaining)):
        _pause(True)
        return True
    return False


class QuotaExceededError(ToolError):
    def __init__(self, seconds):
        super().__init__("transcription_quota_exceeded", "Audio does not fit the remaining monthly transcription quota")
        self.required_seconds = seconds  # Internal scheduling receipt, never serialized.


@dataclass(frozen=True)
class Completion:
    path: str
    token: str
    month: str
    seconds: float
    provider: str
    model: str
    source: str
    outcome: str


def _finish(conn, entry):
    with conn:
        conn.execute("BEGIN IMMEDIATE")
        if not conn.execute("SELECT 1 FROM transcription_reservations WHERE id=?", (entry.token,)).fetchone():
            return  # A repeated completion cannot increment counters twice.
        if entry.outcome == "success":
            conn.execute(
                "INSERT INTO transcription_usage VALUES (?,?,?,?,?,1) ON CONFLICT(month,provider,model,source) "
                "DO UPDATE SET seconds=seconds+excluded.seconds,requests=requests+1",
                (entry.month, entry.provider, entry.model, entry.source, entry.seconds),
            )
        conn.execute(
            "INSERT INTO transcription_outcomes VALUES (?,?,1) ON CONFLICT(provider,outcome) "
            "DO UPDATE SET requests=requests+1",
            (entry.provider, entry.outcome),
        )
        conn.execute("DELETE FROM transcription_reservations WHERE id=?", (entry.token,))


def _reconcile():
    while True:
        entry = _pending.get()
        while True:
            try:
                conn = notes_connection(entry.path, create=False, timeout=0.05)
                if conn is not None:
                    try:
                        _finish(conn, entry)
                    finally:
                        conn.close()
                break
            except (sqlite3.Error, OSError):
                time.sleep(0.25)
        _completion_slots.release()
        _pending.task_done()


def _defer(entry):
    global _reconciler_started
    _pending.put_nowait(entry)  # Admission slots bound in-flight plus deferred work.
    with _reconciler_lock:
        if not _reconciler_started:
            threading.Thread(target=_reconcile, name="transcription-accounting", daemon=True).start()
            _reconciler_started = True


@contextlib.contextmanager
def admission(seconds: float, provider: str, model: str, source: str, *, deadline=None):
    if not _completion_slots.acquire(blocking=False):
        raise ToolError("internal", "Transcription accounting busy; retry after the database recovers")
    conn = None
    deferred = False
    try:
        month, (cap, scope), token = month_now(), limits(deadline), uuid.uuid4().hex
        path = notes_db_path()
        conn = _connection(timeout=_budget(deadline))
        assert conn is not None
        conn.execute(f"PRAGMA busy_timeout={max(1, int(_budget(deadline) * 1000))}")
        with conn:
            conn.execute("BEGIN IMMEDIATE")
            if (
                cap is not None
                and (scope == "all" or source == "ingest")
                and _exceeds_quota(_used(conn, month, scope) + seconds, cap)
            ):
                if source == "ingest":
                    _pause(True)
                raise QuotaExceededError(seconds)
            conn.execute("INSERT INTO transcription_reservations VALUES (?,?,?,?)", (token, month, source, seconds))
        if source == "ingest":
            _pause(False)
        outcome = "error"
        try:
            yield
            outcome = "success"
        finally:
            entry = Completion(path, token, month, seconds, provider, model, source, outcome)
            try:
                if deadline is not None:
                    conn.execute(
                        f"PRAGMA busy_timeout={max(1, int(min(5, max(0, deadline - time.monotonic())) * 1000))}"
                    )
                _finish(conn, entry)
            except sqlite3.Error:
                _defer(entry)
                deferred = True
                if outcome == "success":
                    # Do not report success before its accounting is durable.
                    if deadline is not None:
                        from transcribe import BackendUnavailableError

                        raise BackendUnavailableError(
                            "HTTP transcription accounting pending database recovery"
                        ) from None
                    raise ToolError(
                        "internal", "Transcription accounting pending database recovery; retry later"
                    ) from None
    except sqlite3.Error:
        if deadline is not None and deadline - time.monotonic() < 0.01:
            from transcribe import BackendUnavailableError

            raise BackendUnavailableError(
                "HTTP transcription exceeded whole-file deadline waiting for accounting"
            ) from None
        raise ToolError("internal", "Transcription accounting unavailable; retry after the database recovers") from None
    finally:
        if conn is not None:
            conn.close()
        if not deferred:
            _completion_slots.release()


def metrics_text():
    lines = [
        "# HELP whatsapp_mcp_transcription_usage_available Whether durable transcription accounting is readable.",
        "# TYPE whatsapp_mcp_transcription_usage_available gauge",
        "whatsapp_mcp_transcription_usage_available 1",
        "# HELP whatsapp_mcp_transcription_seconds_total Successfully transcribed audio seconds.",
        "# TYPE whatsapp_mcp_transcription_seconds_total counter",
        "# HELP whatsapp_mcp_transcription_requests_total Completed transcription requests by outcome.",
        "# TYPE whatsapp_mcp_transcription_requests_total counter",
        "# HELP whatsapp_mcp_transcription_quota_remaining_seconds Remaining admitted audio seconds this UTC month.",
        "# TYPE whatsapp_mcp_transcription_quota_remaining_seconds gauge",
    ]
    conn = _connection(create=False)
    if conn is not None:
        try:
            for provider, source, seconds in conn.execute(
                "SELECT provider,source,SUM(seconds) FROM transcription_usage GROUP BY provider,source"
            ):
                lines.append(
                    f'whatsapp_mcp_transcription_seconds_total{{provider="{provider}",source="{source}"}} {seconds:g}'
                )
            for provider, outcome, requests in conn.execute("SELECT * FROM transcription_outcomes"):
                lines.append(
                    f'whatsapp_mcp_transcription_requests_total{{provider="{provider}",outcome="{outcome}"}} {requests}'
                )
        finally:
            conn.close()
    remaining = current_usage()["remaining_seconds"]
    lines.append(f"whatsapp_mcp_transcription_quota_remaining_seconds {remaining if remaining is not None else '+Inf'}")
    return "\n".join(lines) + "\n"
