"""TRANSCRIBE_ON_INGEST: the background worker, with a fake backend and a fake clock."""

from __future__ import annotations

import os
import threading

import pytest

import chat_policy
import media_inventory
import media_notes
import transcribe_worker
import whatsapp
from errors import ToolError
from tests.conftest import ALICE, BOB

SHA = {
    "AUD1": "a1" * 32,
    "AUD2": "a2" * 32,
    "AUD3": "a3" * 32,
    "AUD4": "a4" * 32,
    "OUT1": "0f" * 32,
    "BOB1": "b1" * 32,
}


def _media_filename(message_id: str) -> str:
    return f"audio_20260905_09000{message_id[-1]}_{message_id}.ogg"


def _add_audio(store, message_id: str, chat_jid: str, *, from_me: int = 0, cached: bool = True) -> None:
    """One audio row the way the bridge writes it, optionally with its cached bytes."""
    filename = _media_filename(message_id)
    with store.messages() as conn:
        conn.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, "
            "file_length, file_sha256, filename) VALUES (?, ?, ?, '', ?, ?, 'audio', 3000, ?, ?)",
            (
                message_id,
                chat_jid,
                chat_jid,
                f"2026-09-05 09:0{message_id[-1]}:00",
                from_me,
                bytes.fromhex(SHA[message_id]),
                filename,
            ),
        )
    if cached:
        directory = media_inventory.chat_media_dir(chat_jid)
        os.makedirs(directory, exist_ok=True)
        with open(os.path.join(directory, filename), "wb") as fh:
            fh.write(b"opus")


@pytest.fixture
def archive(paired_dbs):
    """Two inbound voice notes from Alice with their bytes on disk."""
    _add_audio(paired_dbs, "AUD1", ALICE)
    _add_audio(paired_dbs, "AUD2", ALICE)
    return paired_dbs


class FakeBackend:
    """Counts every run; can be told to fail for one path."""

    def __init__(self, fail: set[str] | None = None, text: str = "spoken words") -> None:
        self.runs: list[str] = []
        self.fail = fail or set()
        self.text = text

    def __call__(self, path: str) -> dict[str, str]:
        self.runs.append(path)
        if os.path.basename(path) in self.fail:
            raise RuntimeError("whisper exploded")
        return {"text": self.text, "language": "pt", "backend": "server"}


class FakeBridge:
    """Stands in for whatsapp.download_media: caches the bytes and answers with the path.

    ``fail`` names the messages it refuses (down, disconnected, expired media
    link — all of them ``bridge_unavailable``); ``foreign_path`` makes it answer
    with the bridge's own view of the path, which a split deployment cannot open.
    """

    def __init__(self, *, fail: set[str] | None = None, foreign_path: bool = False) -> None:
        self.calls: list[tuple[str, str]] = []
        self.fail = fail or set()
        self.foreign_path = foreign_path

    def __call__(self, message_id: str, chat_jid: str) -> str:
        self.calls.append((message_id, chat_jid))
        if message_id in self.fail:
            raise ToolError("bridge_unavailable", "bridge unreachable at http://localhost:8080/api")
        directory = media_inventory.chat_media_dir(chat_jid)
        os.makedirs(directory, exist_ok=True)
        name = _media_filename(message_id)
        with open(os.path.join(directory, name), "wb") as fh:
            fh.write(b"opus")
        return f"/app/store/{chat_jid}/{name}" if self.foreign_path else os.path.join(directory, name)


# --- configuration ----------------------------------------------------------


def test_worker_is_off_by_default_and_reads_its_knobs():
    off = transcribe_worker.load_ingest_config({})
    assert off.enabled is False
    assert off.fetch is False  # fetching uncached audio is its own opt-in
    assert (off.interval_s, off.batch) == (transcribe_worker.DEFAULT_INTERVAL_S, transcribe_worker.DEFAULT_BATCH)

    on = transcribe_worker.load_ingest_config(
        {
            "TRANSCRIBE_ON_INGEST": "1",
            "TRANSCRIBE_ON_INGEST_INTERVAL_S": "60",
            "TRANSCRIBE_ON_INGEST_BATCH": "3",
            "TRANSCRIBE_ON_INGEST_FETCH": "yes",
        }
    )
    assert (on.enabled, on.interval_s, on.batch, on.fetch) == (True, 60.0, 3, True)

    # A hot loop and an unbounded batch are clamped, not obeyed.
    clamped = transcribe_worker.load_ingest_config(
        {"TRANSCRIBE_ON_INGEST_INTERVAL_S": "0.01", "TRANSCRIBE_ON_INGEST_BATCH": "10000"}
    )
    assert clamped.interval_s == transcribe_worker.MIN_INTERVAL_S
    assert clamped.batch == transcribe_worker.MAX_BATCH


@pytest.mark.parametrize(
    "env",
    [
        {"TRANSCRIBE_ON_INGEST": "treu"},
        {"TRANSCRIBE_ON_INGEST_INTERVAL_S": "soon"},
        {"TRANSCRIBE_ON_INGEST_INTERVAL_S": "-5"},
        {"TRANSCRIBE_ON_INGEST_BATCH": "many"},
        {"TRANSCRIBE_ON_INGEST_BATCH": "0"},
        {"TRANSCRIBE_ON_INGEST_FETCH": "sometimes"},
    ],
)
def test_an_unreadable_value_stops_the_process(env):
    with pytest.raises(ValueError):
        transcribe_worker.load_ingest_config(env)


def test_install_does_nothing_when_off_or_without_a_backend(archive, monkeypatch):
    started: list[str] = []
    monkeypatch.setattr(transcribe_worker, "start_worker", lambda config: started.append("go"))

    assert transcribe_worker.install_ingest_worker({}) is None
    # Enabled but no WHISPER_URL / WHISPER_BIN: nothing to transcribe with.
    assert transcribe_worker.install_ingest_worker({"TRANSCRIBE_ON_INGEST": "yes"}) is None
    assert started == []

    transcribe_worker.install_ingest_worker(
        {"TRANSCRIBE_ON_INGEST": "yes", "WHISPER_URL": "http://127.0.0.1:8178/inference"}
    )
    assert started == ["go"]


# --- one batch --------------------------------------------------------------


def test_a_batch_transcribes_pending_audio_and_is_idempotent(archive):
    backend = FakeBackend()

    first = transcribe_worker.run_once(10, transcribe=backend)
    assert (first.pending, first.transcribed, first.failed) == (2, 2, 0)
    assert sorted(os.path.basename(p) for p in backend.runs) == [
        "audio_20260905_090001_AUD1.ogg",
        "audio_20260905_090002_AUD2.ogg",
    ]
    notes = media_notes.fetch_notes([SHA["AUD1"], SHA["AUD2"]])
    assert notes[SHA["AUD1"]] == {
        "transcript": "spoken words",
        "transcript_lang": "pt",
        "transcript_backend": "server",
    }
    assert notes[SHA["AUD2"]]["transcript"] == "spoken words"

    # Second run: the notes are the work list, so whisper is not asked again —
    # which is also what happens after a restart.
    second = transcribe_worker.run_once(10, transcribe=backend)
    assert (second.pending, second.transcribed) == (0, 0)
    assert len(backend.runs) == 2


def test_the_batch_size_bounds_one_run(archive):
    backend = FakeBackend()
    assert transcribe_worker.run_once(1, transcribe=backend).transcribed == 1
    assert transcribe_worker.run_once(1, transcribe=backend).transcribed == 1
    assert transcribe_worker.run_once(1, transcribe=backend).pending == 0


def test_outbound_audio_and_uncached_bytes_are_skipped(paired_dbs):
    _add_audio(paired_dbs, "OUT1", ALICE, from_me=1)  # my own voice note
    _add_audio(paired_dbs, "AUD1", ALICE, cached=False)  # row without bytes on disk
    _add_audio(paired_dbs, "AUD2", ALICE)

    backend = FakeBackend()
    result = transcribe_worker.run_once(10, transcribe=backend)
    assert (result.pending, result.transcribed) == (1, 1)
    assert [os.path.basename(p) for p in backend.runs] == ["audio_20260905_090002_AUD2.ogg"]
    assert media_notes.fetch_notes([SHA["OUT1"], SHA["AUD1"]]) == {}


def test_uncached_audio_is_fetched_from_the_bridge_only_with_the_flag_on(paired_dbs):
    _add_audio(paired_dbs, "AUD1", ALICE, cached=False)
    bridge = FakeBridge()
    backend = FakeBackend()

    # Default: the worker never talks to the bridge, so the row stays untouched.
    off = transcribe_worker.run_once(10, transcribe=backend, download=bridge)
    assert (off.pending, off.transcribed) == (0, 0)
    assert (bridge.calls, backend.runs) == ([], [])
    assert media_notes.fetch_notes([SHA["AUD1"]]) == {}

    on = transcribe_worker.run_once(10, transcribe=backend, fetch=True, download=bridge)
    assert (on.pending, on.transcribed, on.failed) == (1, 1, 0)
    assert bridge.calls == [("AUD1", ALICE)]
    assert [os.path.basename(p) for p in backend.runs] == ["audio_20260905_090001_AUD1.ogg"]
    assert media_notes.fetch_notes([SHA["AUD1"]])[SHA["AUD1"]]["transcript"] == "spoken words"


def test_a_fetch_fills_a_slot_of_the_batch(paired_dbs):
    _add_audio(paired_dbs, "AUD1", ALICE, cached=False)
    _add_audio(paired_dbs, "AUD2", ALICE, cached=False)
    bridge = FakeBridge()

    result = transcribe_worker.run_once(1, transcribe=FakeBackend(), fetch=True, download=bridge)
    assert (result.pending, result.transcribed) == (1, 1)
    assert len(bridge.calls) == 1  # the batch bounds the downloads, not just the transcriptions


def test_a_file_the_bridge_will_not_send_is_skipped_not_parked(paired_dbs):
    _add_audio(paired_dbs, "AUD1", ALICE, cached=False)
    _add_audio(paired_dbs, "AUD2", ALICE)
    backend = FakeBackend()

    down = FakeBridge(fail={"AUD1"})
    result = transcribe_worker.run_once(10, transcribe=backend, fetch=True, download=down)
    # The cached file is still transcribed; the uncached one is left alone.
    assert (result.pending, result.transcribed, result.failed) == (1, 1, 0)
    assert [os.path.basename(p) for p in backend.runs] == ["audio_20260905_090002_AUD2.ogg"]
    assert media_notes.fetch_notes([SHA["AUD1"]]) == {}  # no transcript_error: whisper never saw it
    assert down.calls == [("AUD1", ALICE)]

    # With the bridge answering again, the same file is picked up on the next round.
    back = FakeBridge()
    assert transcribe_worker.run_once(10, transcribe=backend, fetch=True, download=back).transcribed == 1
    assert back.calls == [("AUD1", ALICE)]


def test_one_refused_file_does_not_end_the_fetching_of_a_round(paired_dbs):
    _add_audio(paired_dbs, "AUD1", ALICE, cached=False)
    _add_audio(paired_dbs, "AUD2", ALICE, cached=False)  # newest, and refused
    bridge = FakeBridge(fail={"AUD2"})

    result = transcribe_worker.run_once(10, transcribe=FakeBackend(), fetch=True, download=bridge)
    assert (result.pending, result.transcribed) == (1, 1)
    assert [message_id for message_id, _ in bridge.calls] == ["AUD2", "AUD1"]


def test_a_bridge_that_refuses_everything_is_asked_a_bounded_number_of_times(paired_dbs):
    uncached = ("AUD1", "AUD2", "AUD3", "AUD4")
    for message_id in uncached:
        _add_audio(paired_dbs, message_id, ALICE, cached=False)
    down = FakeBridge(fail=set(uncached))

    result = transcribe_worker.run_once(10, transcribe=FakeBackend(), fetch=True, download=down)
    assert (result.pending, result.transcribed, result.failed) == (0, 0, 0)
    assert len(down.calls) == transcribe_worker.MAX_FETCH_FAILURES
    assert media_notes.fetch_notes([SHA[m] for m in uncached]) == {}


def test_failed_fetches_count_against_the_batch(paired_dbs):
    for message_id in ("AUD1", "AUD2", "AUD3", "AUD4"):
        _add_audio(paired_dbs, message_id, ALICE, cached=False)
    bridge = FakeBridge(fail={"AUD3", "AUD4"})  # the two newest

    result = transcribe_worker.run_once(2, transcribe=FakeBackend(), fetch=True, download=bridge)
    assert (result.pending, result.transcribed) == (0, 0)
    assert [message_id for message_id, _ in bridge.calls] == ["AUD4", "AUD3"]  # the batch, failures included


def test_the_file_is_found_where_this_process_sees_it_not_where_the_bridge_does(paired_dbs):
    _add_audio(paired_dbs, "AUD1", ALICE, cached=False)
    backend = FakeBackend()
    # Split deployment: the bridge answers with /app/store/..., which is not a
    # path here, but the bytes land in the store this process reads.
    result = transcribe_worker.run_once(10, transcribe=backend, fetch=True, download=FakeBridge(foreign_path=True))
    assert (result.pending, result.transcribed) == (1, 1)
    assert [os.path.basename(p) for p in backend.runs] == ["audio_20260905_090001_AUD1.ogg"]
    assert media_notes.fetch_notes([SHA["AUD1"]])[SHA["AUD1"]]["transcript"] == "spoken words"


def test_a_failure_is_recorded_once_and_never_retried(archive):
    backend = FakeBackend(fail={"audio_20260905_090001_AUD1.ogg"})

    result = transcribe_worker.run_once(10, transcribe=backend)
    # The bad file does not end the batch: the other one is still transcribed.
    assert (result.pending, result.transcribed, result.failed) == (2, 1, 1)
    parked = media_notes.fetch_notes([SHA["AUD1"]])[SHA["AUD1"]]
    assert "transcript" not in parked
    assert "whisper exploded" in parked["transcript_error"]
    assert media_notes.fetch_notes([SHA["AUD2"]])[SHA["AUD2"]]["transcript"] == "spoken words"

    assert transcribe_worker.run_once(10, transcribe=backend).pending == 0
    assert len(backend.runs) == 2  # the parked file is not tried again

    # Clearing the note queues it again, and a success clears the failure.
    media_notes.annotate_media(SHA["AUD1"], "transcript_error", "")
    assert transcribe_worker.run_once(10, transcribe=FakeBackend()).transcribed == 1
    assert media_notes.fetch_notes([SHA["AUD1"]])[SHA["AUD1"]] == {
        "transcript": "spoken words",
        "transcript_lang": "pt",
        "transcript_backend": "server",
    }


def test_an_empty_transcript_is_a_failure_not_an_endless_retry(archive):
    silent = FakeBackend(text="   ")
    result = transcribe_worker.run_once(10, transcribe=silent)
    assert (result.transcribed, result.failed) == (0, 2)
    assert "no text" in media_notes.fetch_notes([SHA["AUD1"]])[SHA["AUD1"]]["transcript_error"]
    assert transcribe_worker.run_once(10, transcribe=silent).pending == 0


def test_a_broken_archive_is_logged_not_raised(archive, monkeypatch):
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", os.path.join(os.sep, "nope", "messages.db"))
    assert transcribe_worker.run_once(10, transcribe=FakeBackend()) == transcribe_worker.BatchResult(0, 0, 0)


def test_the_allow_list_bounds_what_the_worker_transcribes(paired_dbs, monkeypatch):
    _add_audio(paired_dbs, "AUD1", ALICE)
    _add_audio(paired_dbs, "BOB1", BOB)
    policy = chat_policy.load_chat_policy({"WHATSAPP_ALLOWED_CHATS": ALICE})
    for module in (whatsapp, media_inventory, media_notes, transcribe_worker):
        monkeypatch.setattr(module, "CHAT_POLICY", policy)

    backend = FakeBackend()
    assert transcribe_worker.run_once(10, transcribe=backend).transcribed == 1
    assert [os.path.basename(p) for p in backend.runs] == ["audio_20260905_090001_AUD1.ogg"]
    assert media_notes.fetch_notes([SHA["BOB1"]]) == {}


# --- the loop ---------------------------------------------------------------


def test_the_loop_sleeps_the_configured_interval_between_batches():
    """Fake clock: no whisper, no wall-clock waiting, just the shape of the loop."""
    config = transcribe_worker.IngestConfig(enabled=True, interval_s=42.0, batch=7)
    stop = threading.Event()
    batches: list[int] = []
    slept: list[float] = []

    def run(batch: int) -> transcribe_worker.BatchResult:
        batches.append(batch)
        return transcribe_worker.BatchResult(0, 0, 0)

    def sleep(seconds: float) -> None:
        slept.append(seconds)
        if len(slept) == 3:
            stop.set()

    transcribe_worker.run_forever(config, stop=stop, sleep=sleep, run=run)
    assert batches == [7, 7, 7]
    assert slept == [42.0, 42.0, 42.0]


def test_the_loop_carries_the_fetch_flag_into_every_batch(monkeypatch):
    config = transcribe_worker.IngestConfig(enabled=True, interval_s=1.0, batch=4, fetch=True)
    stop = threading.Event()
    seen: list[tuple[int, bool]] = []

    def fake_run_once(batch: int, *, fetch: bool = False, **kwargs) -> transcribe_worker.BatchResult:
        seen.append((batch, fetch))
        stop.set()
        return transcribe_worker.BatchResult(0, 0, 0)

    monkeypatch.setattr(transcribe_worker, "run_once", fake_run_once)
    transcribe_worker.run_forever(config, stop=stop, sleep=lambda _seconds: None)
    assert seen == [(4, True)]


def test_start_worker_runs_on_a_daemon_thread_and_stops():
    config = transcribe_worker.IngestConfig(enabled=True, interval_s=0.01, batch=1)
    stop = threading.Event()
    ran = threading.Event()

    def run(batch: int) -> transcribe_worker.BatchResult:
        ran.set()
        stop.set()
        return transcribe_worker.BatchResult(0, 0, 0)

    thread = transcribe_worker.start_worker(config, stop=stop, run=run)
    assert thread.daemon  # never holds up shutdown
    thread.join(timeout=5)
    assert ran.is_set() and not thread.is_alive()
