"""Local voice-note transcription via whisper.cpp.

Fork-specific feature (upstream lists transcription as out of scope). Two
backends, both fully local, selected by environment variables:

- ``WHISPER_URL``  – a running whisper.cpp ``whisper-server`` inference endpoint,
  e.g. ``http://127.0.0.1:8178/inference`` (the ``whisper`` compose profile
  starts one). Preferred: the model stays loaded between calls.
- ``WHISPER_BIN``  – path to a whisper.cpp CLI binary (``whisper-cli`` or the
  legacy ``main``), used with ``WHISPER_MODEL`` (path to a ``ggml-*.bin`` file).

If ``WHISPER_URL`` is set it wins. Other knobs:

- ``WHISPER_LANGUAGE``  – ISO-639-1 code passed to whisper (default ``pt``;
  ``auto`` lets whisper detect).
- ``WHISPER_TIMEOUT_S`` – per-transcription timeout in seconds (default 300).

Input audio is always normalised to 16 kHz mono PCM WAV with ffmpeg first,
which is what whisper.cpp expects regardless of backend.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import tempfile
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from typing import Any
from urllib.parse import urlsplit

import httpx

from audio import ffmpeg_timeout_s
from tool_policy import parse_bool_env

DEFAULT_LANGUAGE = "pt"
DEFAULT_TIMEOUT_S = 300
ON_INGEST_ENV = "TRANSCRIBE_ON_INGEST"
# Budget for the liveness probe behind bridge_status: long enough for a
# whisper-server container on the same host, short enough that a status call
# never feels like it hung.
STATUS_PROBE_TIMEOUT_S = 2.0
# Statuses that are about the deployment rather than about this audio, so the
# caller retries instead of parking the file (issue #377): 404/405/501 are what
# a WHISPER_URL missing its /inference path answers for every file, 502/503/504
# what a server still loading its model (or a proxy in front of a dead one)
# answers. A plain 500 is left out on purpose: whisper.cpp reports an inference
# it could not finish that way, which *is* about the file it was given.
OUTAGE_STATUSES = frozenset({404, 405, 501, 502, 503, 504})


class TranscriptionError(RuntimeError):
    """Raised when no backend is configured or the backend fails."""


class BackendUnavailableError(TranscriptionError):
    """The backend could not be asked at all — nothing to do with this file.

    A connection that was refused, reset or never established on ``WHISPER_URL``,
    one of ``OUTAGE_STATUSES`` from it, a ``WHISPER_BIN`` or model that is not
    there, no backend at all, no ffmpeg on PATH: the same call succeeds as soon
    as the deployment is whole again. Callers that remember failures must not
    remember these — the background worker (``transcribe_worker.py``) writes no
    ``transcript_error`` note for one, so the file is tried again next interval
    instead of being parked for ever (issue #377). Everything else the backend
    says is about this file (it could not be decoded, it timed out, it produced
    no text, whisper exited non-zero) and stays a plain ``TranscriptionError``.
    """


@dataclass(frozen=True)
class WhisperConfig:
    url: str | None
    binary: str | None
    model: str | None
    language: str
    timeout_s: int

    @property
    def backend(self) -> str | None:
        if self.url:
            return "server"
        if self.binary:
            return "cli"
        return None


def _env_reader(env: Mapping[str, str] | None) -> Callable[[str], str | None]:
    """Reader over ``env`` (or ``os.environ``) returning None for blank values."""
    source: Mapping[str, str] = os.environ if env is None else env

    def get(name: str) -> str | None:
        value = (source.get(name) or "").strip()
        return value or None

    return get


def load_config(env: Mapping[str, str] | None = None) -> WhisperConfig:
    """Read the WHISPER_* variables (from ``env`` or ``os.environ``)."""
    get = _env_reader(env)

    timeout_raw = get("WHISPER_TIMEOUT_S")
    try:
        timeout_s = int(timeout_raw) if timeout_raw else DEFAULT_TIMEOUT_S
    except ValueError:
        raise TranscriptionError(f"Invalid WHISPER_TIMEOUT_S={timeout_raw!r}; must be an integer") from None
    if timeout_s <= 0:
        raise TranscriptionError(f"Invalid WHISPER_TIMEOUT_S={timeout_raw!r}; must be positive")

    return WhisperConfig(
        url=get("WHISPER_URL"),
        binary=get("WHISPER_BIN"),
        model=get("WHISPER_MODEL"),
        language=get("WHISPER_LANGUAGE") or DEFAULT_LANGUAGE,
        timeout_s=timeout_s,
    )


def describe_setup_help() -> str:
    return (
        "No whisper backend configured. Set WHISPER_URL to a whisper.cpp server inference endpoint "
        "(e.g. http://whisper:8178/inference, a whisper.cpp server you run; docs/DOCKER.md), "
        "or WHISPER_BIN=/path/to/whisper-cli together with WHISPER_MODEL=/path/to/ggml-small.bin."
    )


def probe_server(url: str, timeout_s: float = STATUS_PROBE_TIMEOUT_S) -> bool:
    """Is something answering HTTP at ``WHISPER_URL``?

    A HEAD on the configured URL itself, path included: whisper-server only
    handles POST there, so any answer (404, 405, 200) proves it is routed and
    listening, while a reverse proxy that serves an unrelated app at ``/`` no
    longer counts as a working backend. No body is sent or read, so the probe
    stays cheap enough for ``bridge_status``; only a transport failure or a
    timeout counts as unreachable.
    """
    try:
        parts = urlsplit(url)
    except ValueError:  # not even a URL (unbalanced brackets in the host, …)
        return False
    if not parts.scheme or not parts.netloc:
        return False
    try:
        httpx.head(url, timeout=timeout_s)
    except (httpx.HTTPError, httpx.InvalidURL):
        return False
    return True


def _cli_ready(binary: str, model: str | None) -> bool:
    """Both halves of the CLI backend present: the executable and the model file."""
    if not (os.path.isfile(binary) or shutil.which(binary)):
        return False
    return bool(model) and os.path.isfile(model or "")


def _on_ingest_requested(raw: str | None) -> bool:
    """``TRANSCRIBE_ON_INGEST`` as a status report reads it: never fatal.

    The startup path parses it strictly and refuses to boot on a value it
    cannot read, so a running server can only get here with a good one. A
    report must not lose the whole capability block over a typo anyway.
    """
    try:
        return parse_bool_env(raw, ON_INGEST_ENV)
    except ValueError:
        return False


def describe_status(
    env: Mapping[str, str] | None = None,
    probe: Callable[[str], bool] = probe_server,
) -> dict[str, Any]:
    """Whether transcription is possible here, for ``bridge_status``.

    ``backend`` names the variable that configures it — ``"url"`` for
    ``WHISPER_URL``, ``"bin"`` for ``WHISPER_BIN`` (the same two backends
    ``transcribe_audio`` reports as ``server`` / ``cli`` once it has run one).
    ``reachable`` is a live check: the HTTP probe for the server backend, the
    presence of the binary and the model file for the CLI one, and None when
    nothing is configured. ``on_ingest`` says whether the background worker is
    actually working through the backlog, which needs a backend too — without
    one it refuses to start whatever the variable says.
    """
    get = _env_reader(env)
    url, binary, model = get("WHISPER_URL"), get("WHISPER_BIN"), get("WHISPER_MODEL")
    backend = "url" if url else ("bin" if binary else None)
    reachable: bool | None = None
    if backend == "url" and url:
        reachable = bool(probe(url))
    elif backend == "bin" and binary:
        reachable = _cli_ready(binary, model)
    return {
        "configured": backend is not None,
        "backend": backend,
        "reachable": reachable,
        "model": model,
        "on_ingest": backend is not None and _on_ingest_requested(get(ON_INGEST_ENV)),
    }


def convert_to_wav16k(input_file: str, output_file: str) -> str:
    """Transcode any audio (WhatsApp voice notes are Opus/OGG) to 16 kHz mono PCM WAV."""
    if not os.path.isfile(input_file):
        raise FileNotFoundError(f"Audio file not found: {input_file}")
    cmd = [
        "ffmpeg",
        "-hide_banner",
        "-loglevel",
        "error",
        "-i",
        input_file,
        "-vn",
        "-ac",
        "1",
        "-ar",
        "16000",
        "-c:a",
        "pcm_s16le",
        "-y",
        output_file,
    ]
    timeout = ffmpeg_timeout_s()
    try:
        subprocess.run(cmd, capture_output=True, text=True, check=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        raise TranscriptionError(f"ffmpeg timed out after {timeout}s preparing {input_file}") from None
    except FileNotFoundError:
        # Missing ffmpeg breaks every file, not this one: an outage of the
        # pipeline, so the caller retries instead of parking the audio.
        raise BackendUnavailableError(
            "ffmpeg is required to prepare audio for whisper but was not found on PATH"
        ) from None
    except subprocess.CalledProcessError as exc:
        raise TranscriptionError(f"ffmpeg failed to convert {input_file}: {exc.stderr.strip()}") from None
    return output_file


def _transcribe_via_server(wav_path: str, config: WhisperConfig, language: str) -> str:
    data = {"response_format": "json", "temperature": "0.0"}
    if language and language != "auto":
        data["language"] = language
    if not config.url:
        raise BackendUnavailableError("WHISPER_URL is not set")
    try:
        with open(wav_path, "rb") as fh:
            response = httpx.post(
                config.url,
                files={"file": (os.path.basename(wav_path), fh, "audio/wav")},
                data=data,
                timeout=config.timeout_s,
            )
    # The server had the request and ran out of time on it: that is this file
    # (a long voice note, a slow model), the same way whisper-cli timing out is,
    # so it is parked rather than retried for WHISPER_TIMEOUT_S every round.
    except (httpx.ReadTimeout, httpx.WriteTimeout) as exc:
        raise TranscriptionError(f"whisper server timed out on this file after {config.timeout_s}s: {exc}") from None
    # Refused, reset, connect-timed-out, DNS, a URL httpx will not even build (a
    # port that is not a number): the server never answered about this file.
    # InvalidURL is not an HTTPError, which is why probe_server names it too.
    except (httpx.HTTPError, httpx.InvalidURL) as exc:
        raise BackendUnavailableError(f"whisper server request failed: {exc}") from None
    if response.status_code in OUTAGE_STATUSES:
        raise BackendUnavailableError(f"whisper server returned HTTP {response.status_code}: {response.text[:300]}")
    if response.status_code != 200:
        raise TranscriptionError(f"whisper server returned HTTP {response.status_code}: {response.text[:300]}")
    try:
        payload = response.json()
    except (json.JSONDecodeError, ValueError):
        # Some builds answer text/plain for response_format=text; accept that too.
        return response.text.strip()
    if isinstance(payload, dict) and "error" in payload:
        raise TranscriptionError(f"whisper server error: {payload['error']}")
    text = payload.get("text") if isinstance(payload, dict) else None
    if text is None:
        raise TranscriptionError(f"whisper server returned no text: {str(payload)[:300]}")
    return str(text).strip()


def _transcribe_via_cli(wav_path: str, config: WhisperConfig, language: str) -> str:
    # Half a CLI backend (no model, no binary) is a broken deployment, not a
    # file this machine cannot read: BackendUnavailableError, so nothing is parked.
    if not config.model:
        raise BackendUnavailableError("WHISPER_MODEL must point to a ggml model file when using WHISPER_BIN")
    if not config.model or not os.path.isfile(config.model):
        raise BackendUnavailableError(f"WHISPER_MODEL not found: {config.model}")
    binary = config.binary or ""
    if not binary or not (os.path.isfile(binary) or shutil.which(binary)):
        raise BackendUnavailableError(f"WHISPER_BIN not found or not executable: {binary}")

    out_prefix = os.path.splitext(wav_path)[0]
    cmd = [binary, "-m", config.model, "-f", wav_path, "-otxt", "-of", out_prefix, "-np", "-nt"]
    if language and language != "auto":
        cmd += ["-l", language]
    try:
        subprocess.run(cmd, capture_output=True, text=True, check=True, timeout=config.timeout_s)
    except subprocess.TimeoutExpired:
        raise TranscriptionError(f"whisper-cli timed out after {config.timeout_s}s") from None
    except subprocess.CalledProcessError as exc:
        raise TranscriptionError(f"whisper-cli failed (exit {exc.returncode}): {exc.stderr.strip()[:500]}") from None

    txt_path = out_prefix + ".txt"
    try:
        with open(txt_path, encoding="utf-8") as fh:
            return fh.read().strip()
    except FileNotFoundError:
        raise TranscriptionError(f"whisper-cli produced no transcript at {txt_path}") from None
    finally:
        if os.path.exists(txt_path):
            os.unlink(txt_path)


def transcribe_file(audio_path: str, language: str | None = None, config: WhisperConfig | None = None) -> dict:
    """Transcribe an audio file with the configured whisper backend.

    Returns ``{"text", "language", "backend"}``. Raises TranscriptionError when the
    backend refuses this file, BackendUnavailableError (a TranscriptionError) when there
    is no reachable backend at all, FileNotFoundError for a missing input file.
    """
    config = config or load_config()
    backend = config.backend
    if backend is None:
        raise BackendUnavailableError(describe_setup_help())
    lang = (language or "").strip() or config.language

    with tempfile.TemporaryDirectory(prefix="wa-whisper-") as tmp:
        wav_path = convert_to_wav16k(audio_path, os.path.join(tmp, "audio.wav"))
        if backend == "server":
            text = _transcribe_via_server(wav_path, config, lang)
        else:
            text = _transcribe_via_cli(wav_path, config, lang)

    return {"text": text, "language": lang, "backend": backend}
