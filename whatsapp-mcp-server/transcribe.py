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
from dataclasses import dataclass

import requests

from audio import ffmpeg_timeout_s

DEFAULT_LANGUAGE = "pt"
DEFAULT_TIMEOUT_S = 300


class TranscriptionError(RuntimeError):
    """Raised when no backend is configured or the backend fails."""


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


def load_config(env: dict[str, str] | None = None) -> WhisperConfig:
    """Read the WHISPER_* variables (from ``env`` or ``os.environ``)."""
    env = os.environ if env is None else env

    def get(name: str) -> str | None:
        value = (env.get(name) or "").strip()
        return value or None

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
        "(e.g. http://127.0.0.1:8178/inference; `docker compose --profile whisper up -d` starts one), "
        "or WHISPER_BIN=/path/to/whisper-cli together with WHISPER_MODEL=/path/to/ggml-small.bin."
    )


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
        raise TranscriptionError("ffmpeg is required to prepare audio for whisper but was not found on PATH") from None
    except subprocess.CalledProcessError as exc:
        raise TranscriptionError(f"ffmpeg failed to convert {input_file}: {exc.stderr.strip()}") from None
    return output_file


def _transcribe_via_server(wav_path: str, config: WhisperConfig, language: str) -> str:
    data = {"response_format": "json", "temperature": "0.0"}
    if language and language != "auto":
        data["language"] = language
    try:
        with open(wav_path, "rb") as fh:
            response = requests.post(
                config.url,
                files={"file": (os.path.basename(wav_path), fh, "audio/wav")},
                data=data,
                timeout=config.timeout_s,
            )
    except requests.RequestException as exc:
        raise TranscriptionError(f"whisper server request failed: {exc}") from None
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
    if not config.model:
        raise TranscriptionError("WHISPER_MODEL must point to a ggml model file when using WHISPER_BIN")
    if not os.path.isfile(config.model):
        raise TranscriptionError(f"WHISPER_MODEL not found: {config.model}")
    binary = config.binary
    if not (os.path.isfile(binary) or shutil.which(binary)):
        raise TranscriptionError(f"WHISPER_BIN not found or not executable: {binary}")

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

    Returns ``{"text", "language", "backend"}``. Raises TranscriptionError when no
    backend is configured or the backend fails, FileNotFoundError for a missing
    input file.
    """
    config = config or load_config()
    backend = config.backend
    if backend is None:
        raise TranscriptionError(describe_setup_help())
    lang = (language or "").strip() or config.language

    with tempfile.TemporaryDirectory(prefix="wa-whisper-") as tmp:
        wav_path = convert_to_wav16k(audio_path, os.path.join(tmp, "audio.wav"))
        if backend == "server":
            text = _transcribe_via_server(wav_path, config, lang)
        else:
            text = _transcribe_via_cli(wav_path, config, lang)

    return {"text": text, "language": lang, "backend": backend}
