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
from transcribe import BackendUnavailableError

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
    link — all of them ``bridge_unavailable``); ``gone`` names the ones the
    sender's phone answered it no longer has (``media_unavailable``, which no
    retry can change); ``foreign_path`` makes it answer with the bridge's own
    view of the path, which a split deployment cannot open.
    """

    def __init__(
        self, *, fail: set[str] | None = None, gone: set[str] | None = None, foreign_path: bool = False
    ) -> None:
        self.calls: list[tuple[str, str]] = []
        self.fail = fail or set()
        self.gone = gone or set()
        self.foreign_path = foreign_path

    def __call__(self, message_id: str, chat_jid: str) -> str:
        self.calls.append((message_id, chat_jid))
        if message_id in self.gone:
            raise ToolError(
                "media_unavailable",
                "Failed to download media: sender's phone declined media retry: NOT_FOUND",
            )
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


ON = {"TRANSCRIBE_ON_INGEST": "yes", "WHISPER_URL": "http://127.0.0.1:8178/inference"}


def test_the_worker_names_tools_the_server_actually_registers():
    """A renamed tool must break here, not silently let the worker through every policy."""
    import main
    from tool_policy import registered_tool_names

    known = registered_tool_names(main.mcp)
    assert {transcribe_worker.TRANSCRIBE_TOOL, transcribe_worker.DOWNLOAD_TOOL} <= known


@pytest.fixture
def installed(monkeypatch):
    """install_ingest_worker without a thread: the configs it would have started."""
    configs: list[transcribe_worker.IngestConfig] = []
    monkeypatch.setattr(transcribe_worker, "start_worker", configs.append)
    return configs


@pytest.mark.parametrize(
    "policy",
    [
        {"WHATSAPP_DENY_TOOLS": "transcribe_audio"},
        {"WHATSAPP_ALLOW_TOOLS": "list_messages,search_contacts"},  # no transcribe_audio in it
    ],
)
def test_a_deployment_that_does_not_offer_transcribe_audio_gets_no_worker(installed, policy, caplog):
    """The worker is that tool on a timer; hiding the tool must not leave it running."""
    with caplog.at_level("WARNING", logger="whatsapp_mcp"):
        assert transcribe_worker.install_ingest_worker(ON | policy) is None
    assert installed == []
    assert "transcribe_audio" in caplog.text


def test_denying_download_media_leaves_the_fetch_path_off(installed, caplog):
    """Cached audio is still transcribed; the bridge is not asked for the rest."""
    env = ON | {"TRANSCRIBE_ON_INGEST_FETCH": "1", "WHATSAPP_DENY_TOOLS": "download_media"}
    with caplog.at_level("WARNING", logger="whatsapp_mcp"):
        transcribe_worker.install_ingest_worker(env)
    assert [config.fetch for config in installed] == [False]
    assert "download_media" in caplog.text

    # Without the deny-list the same environment fetches.
    installed.clear()
    transcribe_worker.install_ingest_worker(ON | {"TRANSCRIBE_ON_INGEST_FETCH": "1"})
    assert [config.fetch for config in installed] == [True]


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


def test_media_the_phone_no_longer_has_is_recorded_and_never_asked_for_again(paired_dbs, caplog):
    """Issue #378: 600 expired voice notes must not be asked for on every pass."""
    gone = ("AUD1", "AUD2", "AUD3", "AUD4")
    for message_id in gone:
        _add_audio(paired_dbs, message_id, ALICE, cached=False)
    bridge = FakeBridge(gone=set(gone))

    with caplog.at_level("INFO", logger="whatsapp_mcp"):
        result = transcribe_worker.run_once(10, transcribe=FakeBackend(), fetch=True, download=bridge)

    # A definitive miss is not a strike: all four are asked, not just three.
    assert (result.pending, result.transcribed, result.failed) == (0, 0, 0)
    assert len(bridge.calls) == 4
    notes = media_notes.fetch_notes([SHA[m] for m in gone])
    assert len(notes) == 4
    recorded = notes[SHA["AUD1"]]["media_unavailable"]
    assert "NOT_FOUND" in recorded and recorded[:2] == "20"  # the reason, dated
    assert "no longer has" in caplog.text

    # The next round does not ask again: the note takes the rows off the walk.
    again = FakeBridge(gone=set(gone))
    assert transcribe_worker.run_once(10, transcribe=FakeBackend(), fetch=True, download=again).pending == 0
    assert again.calls == []

    # Clearing the note asks the phone again (a backup restored on it, say).
    media_notes.annotate_media(SHA["AUD1"], "media_unavailable", "")
    third = FakeBridge()
    assert transcribe_worker.run_once(10, transcribe=FakeBackend(), fetch=True, download=third).transcribed == 1
    assert third.calls == [("AUD1", ALICE)]


def test_a_definitive_miss_does_not_spend_the_rounds_failure_budget(paired_dbs):
    """Three dead files in front of a live one must not end the fetching (issue #378)."""
    for message_id in ("AUD1", "AUD2", "AUD3", "AUD4"):
        _add_audio(paired_dbs, message_id, ALICE, cached=False)
    bridge = FakeBridge(gone={"AUD4", "AUD3", "AUD2"})  # the three newest are gone

    result = transcribe_worker.run_once(10, transcribe=FakeBackend(), fetch=True, download=bridge)
    assert (result.pending, result.transcribed) == (1, 1)  # AUD1 was still fetched
    assert [message_id for message_id, _ in bridge.calls] == ["AUD4", "AUD3", "AUD2", "AUD1"]


def test_a_transient_refusal_is_still_a_strike_and_writes_no_note(paired_dbs):
    """The bridge being down is not the phone saying no: nothing is recorded."""
    uncached = ("AUD1", "AUD2", "AUD3", "AUD4")
    for message_id in uncached:
        _add_audio(paired_dbs, message_id, ALICE, cached=False)
    down = FakeBridge(fail=set(uncached))

    transcribe_worker.run_once(10, transcribe=FakeBackend(), fetch=True, download=down)
    assert len(down.calls) == transcribe_worker.MAX_FETCH_FAILURES
    assert media_notes.fetch_notes([SHA[m] for m in uncached]) == {}


def test_a_copy_cached_in_another_chat_is_not_hidden_by_the_note(paired_dbs):
    """The note is per hash: it must not take a readable copy off the work list."""
    # The same voice note in two chats: Bob's copy is on disk, Alice's is not.
    _add_audio(paired_dbs, "AUD1", ALICE, cached=False)
    with paired_dbs.messages() as conn:
        conn.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, "
            "file_length, file_sha256, filename) VALUES ('FWD1', ?, ?, '', '2026-09-05 08:00:00', 0, 'audio', "
            "3000, ?, ?)",
            (BOB, BOB, bytes.fromhex(SHA["AUD1"]), _media_filename("FWD1")),
        )
    directory = media_inventory.chat_media_dir(BOB)
    os.makedirs(directory, exist_ok=True)
    with open(os.path.join(directory, _media_filename("FWD1")), "wb") as fh:
        fh.write(b"opus")

    backend = FakeBackend()
    result = transcribe_worker.run_once(10, transcribe=backend, fetch=True, download=FakeBridge(gone={"AUD1"}))

    # No note was written, and the cached copy was transcribed in the same round.
    assert result.transcribed == 1
    assert [os.path.basename(p) for p in backend.runs] == [_media_filename("FWD1")]
    assert "media_unavailable" not in media_notes.fetch_notes([SHA["AUD1"]]).get(SHA["AUD1"], {})


def test_a_note_that_cannot_be_written_still_ends_the_round(paired_dbs, monkeypatch):
    """With notes.db unwritable the misses are not remembered, so the strikes must stay."""
    uncached = ("AUD1", "AUD2", "AUD3", "AUD4")
    for message_id in uncached:
        _add_audio(paired_dbs, message_id, ALICE, cached=False)
    monkeypatch.setattr(transcribe_worker, "_record_unavailable", lambda sha256, reason: False)
    bridge = FakeBridge(gone=set(uncached))

    transcribe_worker.run_once(10, transcribe=FakeBackend(), fetch=True, download=bridge)
    assert len(bridge.calls) == transcribe_worker.MAX_FETCH_FAILURES


def test_a_transcript_retracts_a_recorded_miss(archive):
    """Another copy turned out to be readable: the hash is not unavailable after all."""
    media_notes.annotate_media(SHA["AUD1"], "media_unavailable", "2026-09-08: NOT_FOUND")
    media_notes.store_transcript(SHA["AUD1"], {"text": "spoken words", "language": "pt", "backend": "server"})
    assert media_notes.fetch_notes([SHA["AUD1"]])[SHA["AUD1"]] == {
        "transcript": "spoken words",
        "transcript_lang": "pt",
        "transcript_backend": "server",
    }


def test_coverage_counts_the_media_the_phone_no_longer_has(paired_dbs):
    _add_audio(paired_dbs, "AUD1", ALICE, cached=False)
    _add_audio(paired_dbs, "AUD2", ALICE)
    transcribe_worker.run_once(10, transcribe=FakeBackend(), fetch=True, download=FakeBridge(gone={"AUD1"}))

    audio = whatsapp.coverage()["audio"]
    assert (audio["messages"], audio["transcribed"], audio["unavailable"]) == (2, 1, 1)
    assert audio["backlog"] == 0  # nothing left to do: one done, one impossible


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


class DownBackend:
    """A whisper backend nothing can reach: every call is an outage, not a verdict."""

    def __init__(self, message: str = "whisper server request failed: [Errno 111] Connection refused") -> None:
        self.runs: list[str] = []
        self.message = message

    def __call__(self, path: str) -> dict[str, str]:
        self.runs.append(path)
        raise BackendUnavailableError(self.message)


@pytest.fixture
def four_voice_notes(paired_dbs):
    """Four cached inbound voice notes: enough to spend MAX_OUTAGE_SKIPS on."""
    for message_id in ("AUD1", "AUD2", "AUD3", "AUD4"):
        _add_audio(paired_dbs, message_id, ALICE)
    return paired_dbs


def test_an_unreachable_backend_parks_nothing_and_is_retried_next_round(four_voice_notes, caplog):
    """Issue #377: the whisper container was still starting; the audio is not to blame."""
    down = DownBackend()
    with caplog.at_level("WARNING", logger="whatsapp_mcp"):
        result = transcribe_worker.run_once(10, transcribe=down)

    assert (result.pending, result.transcribed, result.failed, result.outage) == (4, 0, 0, True)
    assert len(down.runs) == transcribe_worker.MAX_OUTAGE_SKIPS  # asked three times, not once per file
    assert "unavailable" in caplog.text
    assert media_notes.fetch_notes([SHA[m] for m in ("AUD1", "AUD2", "AUD3", "AUD4")]) == {}  # nothing parked

    # The backend is up a minute later: the same files are transcribed.
    assert transcribe_worker.run_once(10, transcribe=FakeBackend()).transcribed == 4


def test_a_round_the_backend_ended_leaves_the_walk_where_it_was(four_voice_notes):
    """Nothing was done, so the next round asks for the same page, not the one after it."""
    here = transcribe_worker.Position(timestamp="2026-09-05 09:00:00", message_id="AUD9", chat_jid=ALICE)
    result = transcribe_worker.run_once(10, transcribe=DownBackend(), position=here)
    assert result.position == here


def test_an_outage_keeps_what_it_already_transcribed(four_voice_notes):
    """The files whisper answered about are stored; the round ends at the ones it did not."""
    calls: list[str] = []

    def flaky(path: str) -> dict[str, str]:
        calls.append(path)
        if len(calls) > 1:
            raise BackendUnavailableError("whisper server request failed: [Errno 111] Connection refused")
        return {"text": "spoken words", "language": "pt", "backend": "server"}

    result = transcribe_worker.run_once(10, transcribe=flaky)
    assert (result.transcribed, result.failed, result.outage) == (1, 0, True)
    notes = media_notes.fetch_notes([SHA[m] for m in ("AUD1", "AUD2", "AUD3", "AUD4")])
    assert len(notes) == 1 and next(iter(notes.values()))["transcript"] == "spoken words"


def test_one_request_the_server_choked_on_does_not_stall_the_walk(four_voice_notes):
    """A single file that kills the backend is skipped, not allowed to park the round for ever."""
    poison = _media_filename("AUD2")

    def choking(path: str) -> dict[str, str]:
        if os.path.basename(path) == poison:
            raise BackendUnavailableError("whisper server returned HTTP 503: out of memory")
        return {"text": "spoken words", "language": "pt", "backend": "server"}

    result = transcribe_worker.run_once(10, transcribe=choking)
    # The others are transcribed and the walk moves on; the file itself keeps no
    # note, so it is asked again — one attempt per round, not a dead end.
    assert (result.transcribed, result.failed, result.outage) == (3, 0, False)
    assert media_notes.fetch_notes([SHA["AUD2"]]) == {}
    assert result.position is None  # the page ended: the next round starts at the newest row


def test_a_skip_on_the_last_row_of_a_batch_does_not_pin_the_walk(paired_dbs):
    """Fewer than three strikes must never revert the position, or one file stalls the archive."""
    _add_audio(paired_dbs, "AUD1", ALICE)  # oldest, and the one the backend chokes on
    _add_audio(paired_dbs, "AUD2", ALICE)

    def choking(path: str) -> dict[str, str]:
        if os.path.basename(path) == _media_filename("AUD1"):
            raise BackendUnavailableError("whisper server returned HTTP 503: out of memory")
        return {"text": "spoken words", "language": "pt", "backend": "server"}

    here = transcribe_worker.Position(timestamp="2026-09-05 09:05:00", message_id="AUD9", chat_jid=ALICE)
    result = transcribe_worker.run_once(10, transcribe=choking, position=here)
    assert (result.transcribed, result.outage) == (1, False)
    assert result.position != here  # the walk advanced past the row it could not do


def test_bytes_that_vanish_before_the_transcription_are_not_parked(archive, monkeypatch):
    """A retention sweep between the listing and whisper is not the file's failure."""

    def gone(path: str) -> dict[str, str]:
        raise FileNotFoundError(f"Audio file not found: {path}")

    result = transcribe_worker.run_once(10, transcribe=gone)
    assert (result.pending, result.transcribed, result.failed, result.outage) == (2, 0, 0, False)
    assert media_notes.fetch_notes([SHA["AUD1"], SHA["AUD2"]]) == {}


def test_the_repair_queues_the_files_an_outage_parked_and_leaves_the_others(archive, caplog):
    """Notes an older build wrote for a backend that was down are cleared once, at startup."""
    media_notes.annotate_media(
        SHA["AUD1"],
        "transcript_error",
        "TranscriptionError: whisper server request failed: [Errno 111] Connection refused",
    )
    media_notes.annotate_media(SHA["AUD2"], "transcript_error", "TranscriptionError: ffmpeg failed to convert x.ogg")

    with caplog.at_level("INFO", logger="whatsapp_mcp"):
        assert transcribe_worker.clear_outage_failures() == 1

    assert media_notes.fetch_notes([SHA["AUD1"]]) == {}  # queued again
    assert "ffmpeg failed" in media_notes.fetch_notes([SHA["AUD2"]])[SHA["AUD2"]]["transcript_error"]

    backend = FakeBackend()
    result = transcribe_worker.run_once(10, transcribe=backend)
    assert (result.pending, result.transcribed) == (1, 1)
    assert [os.path.basename(p) for p in backend.runs] == ["audio_20260905_090001_AUD1.ogg"]

    # Idempotent: a second start finds nothing to clear.
    assert transcribe_worker.clear_outage_failures() == 0


def test_the_repair_runs_when_the_worker_is_installed(archive, monkeypatch):
    monkeypatch.setattr(transcribe_worker, "start_worker", lambda config: None)
    media_notes.annotate_media(
        SHA["AUD1"], "transcript_error", "TranscriptionError: whisper server returned HTTP 503: loading model"
    )
    transcribe_worker.install_ingest_worker(ON)
    assert media_notes.fetch_notes([SHA["AUD1"]]) == {}


def test_a_missing_notes_db_is_nothing_to_repair(paired_dbs):
    assert transcribe_worker.clear_outage_failures() == 0


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


# --- walking the archive ----------------------------------------------------


def _numbered_filename(index: int) -> str:
    return f"audio_20260905_{index:06d}_VOICE{index:03d}.ogg"


def _add_numbered_audio(store, index: int, *, cached: bool, chat_jid: str = ALICE) -> None:
    """One inbound voice note; a higher index is newer and has its own hash."""
    message_id = f"VOICE{index:03d}"
    filename = _numbered_filename(index)
    with store.messages() as conn:
        conn.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, "
            "file_length, file_sha256, filename) VALUES (?, ?, ?, '', ?, 0, 'audio', 3000, ?, ?)",
            (
                message_id,
                chat_jid,
                chat_jid,
                f"2026-09-05 {10 + index // 60:02d}:{index % 60:02d}:00",
                bytes.fromhex(f"{index:02x}" * 32),
                filename,
            ),
        )
    if cached:
        directory = media_inventory.chat_media_dir(chat_jid)
        os.makedirs(directory, exist_ok=True)
        with open(os.path.join(directory, filename), "wb") as fh:
            fh.write(b"opus")


@pytest.fixture
def starved_archive(paired_dbs):
    """One older cached voice note buried under 50 newer ones whose bytes are not here."""
    _add_numbered_audio(paired_dbs, 0, cached=True)
    for index in range(1, 51):
        _add_numbered_audio(paired_dbs, index, cached=False)
    return paired_dbs


def _rounds(backend, *, batch: int = 10, limit: int = 8, **kwargs) -> list[transcribe_worker.BatchResult]:
    """Run batches the way the loop does — carrying the position — until one transcribes."""
    results: list[transcribe_worker.BatchResult] = []
    position = None
    for _ in range(limit):
        result = transcribe_worker.run_once(batch, transcribe=backend, position=position, **kwargs)
        results.append(result)
        position = result.position
        if result.transcribed:
            break
    return results


def test_an_unreadable_prefix_does_not_hide_older_cached_audio(starved_archive):
    """Fetch off: the walk steps past the 50 rows with no bytes and reaches the cached one."""
    backend = FakeBackend()
    results = _rounds(backend)

    assert [r.transcribed for r in results] == [0, 1]  # second round, not the tenth
    assert results[0].examined == 50  # the whole candidate page was looked at, not just its head
    assert results[0].position is not None  # and the next round resumes past it
    assert [os.path.basename(p) for p in backend.runs] == [_numbered_filename(0)]
    assert results[-1].position is None  # oldest row reached: the next round starts at the newest again


def test_a_bridge_that_refuses_everything_does_not_hide_older_cached_audio(starved_archive):
    """Fetch on: the round parks in front of what it could not try, and reads the page anyway."""
    refused = {f"VOICE{index:03d}" for index in range(1, 51)}
    bridge = FakeBridge(fail=refused)
    backend = FakeBackend()

    results = _rounds(backend, fetch=True, download=bridge)

    assert [r.transcribed for r in results] == [0, 1]
    assert [os.path.basename(p) for p in backend.runs] == [_numbered_filename(0)]
    # Per round the bridge is asked at most the walk's three strikes plus the slice
    # the newest rows may spend, however many rows the page holds.
    per_round = transcribe_worker.MAX_FETCH_FAILURES + transcribe_worker.HEAD_FETCHES
    assert len(bridge.calls) <= len(results) * per_round
    # A file the bridge would not send is skipped, never recorded as a whisper failure.
    assert media_notes.fetch_notes([f"{index:02x}" * 32 for index in range(1, 51)]) == {}


def test_the_next_round_resumes_at_the_row_the_fetch_budget_did_not_reach(paired_dbs):
    """A round that spends its downloads mid-page parks there; nothing is stepped over untried."""
    for index in range(1, 9):
        _add_numbered_audio(paired_dbs, index, cached=False)
    bridge = FakeBridge(fail={"VOICE005"})
    backend = FakeBackend()

    first = transcribe_worker.run_once(4, transcribe=backend, fetch=True, download=bridge)
    assert (first.transcribed, first.failed) == (3, 0)
    assert [message_id for message_id, _ in bridge.calls] == ["VOICE008", "VOICE007", "VOICE006", "VOICE005"]

    second = transcribe_worker.run_once(4, transcribe=backend, fetch=True, download=bridge, position=first.position)
    # VOICE004 is where the budget ran out, so that is where the walk starts again.
    assert "VOICE004" in [message_id for message_id, _ in bridge.calls[4:]]
    assert second.transcribed == 3


def test_a_few_refused_rows_at_the_top_do_not_stall_the_fetching(paired_dbs):
    """The newest rows the bridge will not send must not be the only ones ever asked for."""
    for index in range(1, 21):
        _add_numbered_audio(paired_dbs, index, cached=False)
    bridge = FakeBridge(fail={"VOICE020", "VOICE019", "VOICE018"})  # expired media, say
    backend = FakeBackend()

    results = _rounds(backend, fetch=True, download=bridge)

    assert results[0].transcribed == 0  # the round spends its strikes on the three dead rows
    assert results[-1].transcribed > 0  # and the next one asks for the rows behind them
    assert ("VOICE017", ALICE) in bridge.calls


def test_an_uncached_arrival_is_fetched_while_the_walk_is_deep_in_the_archive(starved_archive):
    """Autodownload off, fetch on: the newest rows keep a slice of the downloads."""
    bridge = FakeBridge(fail={f"VOICE{index:03d}" for index in range(1, 51)})
    backend = FakeBackend()

    first = transcribe_worker.run_once(10, transcribe=backend, fetch=True, download=bridge)
    assert (first.transcribed, first.position is None) == (0, False)  # the walk is mid-archive now

    _add_numbered_audio(starved_archive, 60, cached=False)  # arrives with no bytes here
    second = transcribe_worker.run_once(10, transcribe=backend, fetch=True, download=bridge, position=first.position)

    assert ("VOICE060", ALICE) in bridge.calls
    assert any("VOICE060" in os.path.basename(path) for path in backend.runs)  # the bridge's own name
    assert second.transcribed == 2  # the arrival and the older cached row the walk reached


def test_a_new_arrival_is_not_queued_behind_the_walk(starved_archive):
    backend = FakeBackend()
    first = transcribe_worker.run_once(10, transcribe=backend)
    assert (first.transcribed, first.position is None) == (0, False)  # mid-archive now

    _add_numbered_audio(starved_archive, 60, cached=True)  # arrives while the walk is far from the top
    second = transcribe_worker.run_once(10, transcribe=backend, position=first.position)

    # The newest rows are looked at every round, so the arrival is transcribed
    # in the very next one — together with the older row the walk had reached.
    assert second.transcribed == 2
    assert [os.path.basename(p) for p in backend.runs] == [_numbered_filename(60), _numbered_filename(0)]


# --- the loop ---------------------------------------------------------------


def test_the_loop_sleeps_the_configured_interval_between_batches():
    """Fake clock: no whisper, no wall-clock waiting, just the shape of the loop."""
    config = transcribe_worker.IngestConfig(enabled=True, interval_s=42.0, batch=7)
    stop = threading.Event()
    batches: list[int] = []
    slept: list[float] = []

    def run(batch: int, *, position: transcribe_worker.Position | None = None) -> transcribe_worker.BatchResult:
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

    def run(batch: int, *, position: transcribe_worker.Position | None = None) -> transcribe_worker.BatchResult:
        ran.set()
        stop.set()
        return transcribe_worker.BatchResult(0, 0, 0)

    thread = transcribe_worker.start_worker(config, stop=stop, run=run)
    assert thread.daemon  # never holds up shutdown
    thread.join(timeout=5)
    assert ran.is_set() and not thread.is_alive()


# --- the cache is not rebuilt per fetched file (issue #318) -------------------


def test_ten_fetches_do_not_read_the_chat_directory_ten_more_times(paired_dbs, monkeypatch):
    """The batch entry is updated from the file that was just fetched."""
    for i in range(10):
        message_id = f"FET{i}"
        with paired_dbs.messages() as conn:
            conn.execute(
                "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, "
                "file_length, file_sha256, filename) VALUES (?, ?, ?, '', ?, 0, 'audio', 3000, ?, ?)",
                (
                    message_id,
                    ALICE,
                    ALICE,
                    f"2026-09-05 09:1{i}:00",
                    bytes.fromhex(f"{i:02x}" * 32),
                    _media_filename(message_id),
                ),
            )
    listings, lookups = [], []
    real_list, real_lookup = media_inventory.list_chat_names, media_inventory.lookup_cached_name

    def counting_list(chat_jid):
        listings.append(chat_jid)
        return real_list(chat_jid)

    def counting_lookup(chat_jid, message_id):
        lookups.append((chat_jid, message_id))
        return real_lookup(chat_jid, message_id)

    monkeypatch.setattr(media_inventory, "list_chat_names", counting_list)
    monkeypatch.setattr(media_inventory, "lookup_cached_name", counting_lookup)
    bridge = FakeBridge()
    result = transcribe_worker.run_once(10, transcribe=FakeBackend(), fetch=True, download=bridge)
    assert result.transcribed == 10 and len(bridge.calls) == 10
    assert listings == [ALICE]  # one directory read for the chat, not one per download
    assert len(lookups) == 10  # one targeted lookup per fetched file
