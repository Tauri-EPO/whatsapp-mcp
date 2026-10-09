"""Voice-note transcription shared by the MCP tool and ingest worker.

The existing WHISPER_URL or WHISPER_BIN/MODEL whisper.cpp backends remain the
default, with their existing language and failure semantics. The opt-in
openai_compatible provider reimplements upstream VGP #247 against this fork:
an explicit endpoint, no ambient credentials/proxies/redirects/fallbacks,
bounded multipart uploads and provider/model notes. Remote endpoints receive
only transcoded metadata-free mono Opus chunks, never the original file bytes.
"""

from __future__ import annotations

import asyncio
import json
import os
import re
import shutil
import subprocess
import tempfile
import time
from collections.abc import Callable, Mapping
from dataclasses import dataclass, replace
from pathlib import Path
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
HTTP_UPLOAD_LIMIT = 25_000_000
HTTP_RESPONSE_LIMIT = 1024 * 1024
MAX_HTTP_AUDIO_BYTES = 256 * 1024 * 1024
MAX_HTTP_AUDIO_SECONDS = 24 * 60 * 60


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
    provider: str = "whisper_cpp"

    @property
    def backend(self) -> str | None:
        if self.provider == "openai_compatible":
            return "openai_compatible" if self.url and self.model else None
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

    provider = get("WHATSAPP_TRANSCRIPTION_PROVIDER") or "whisper_cpp"
    if provider not in ("whisper_cpp", "openai_compatible"):
        raise BackendUnavailableError("WHATSAPP_TRANSCRIPTION_PROVIDER must be whisper_cpp or openai_compatible")

    timeout_raw = get("WHISPER_TIMEOUT_S")
    try:
        timeout_s = int(timeout_raw) if timeout_raw else DEFAULT_TIMEOUT_S
    except ValueError:
        raise TranscriptionError(f"Invalid WHISPER_TIMEOUT_S={timeout_raw!r}; must be an integer") from None
    if timeout_s <= 0:
        raise TranscriptionError(f"Invalid WHISPER_TIMEOUT_S={timeout_raw!r}; must be positive")

    if provider == "openai_compatible":
        url, model = get("WHATSAPP_TRANSCRIPTION_URL"), get("WHATSAPP_TRANSCRIPTION_MODEL")
        if not url or not model:
            raise BackendUnavailableError(
                "HTTP transcription requires WHATSAPP_TRANSCRIPTION_URL and WHATSAPP_TRANSCRIPTION_MODEL"
            )
        try:
            parts = urlsplit(url)
            valid = (
                parts.scheme in ("http", "https")
                and parts.hostname
                and not parts.username
                and not parts.password
                and not parts.fragment
            )
            parts.port
        except ValueError:
            valid = False
        if not valid:
            raise BackendUnavailableError(
                "WHATSAPP_TRANSCRIPTION_URL must be a full HTTP(S) endpoint without credentials"
            )
        return WhisperConfig(url, None, model, get("WHATSAPP_TRANSCRIPTION_LANGUAGE") or "auto", timeout_s, provider)
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
    if get("WHATSAPP_TRANSCRIPTION_PROVIDER") not in (None, "whisper_cpp"):
        try:
            config = load_config(env)
        except TranscriptionError:
            return {
                "configured": False,
                "backend": "openai_compatible",
                "reachable": False,
                "model": None,
                "on_ingest": False,
            }
        return {
            "configured": True,
            "backend": config.backend,
            "provider": config.provider,
            "endpoint_host": urlsplit(config.url or "").hostname,
            "reachable": probe_http_provider(config, get("WHATSAPP_TRANSCRIPTION_API_KEY")),
            "model": config.model,
            "on_ingest": _on_ingest_requested(get(ON_INGEST_ENV)),
        }
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
        subprocess.run(cmd, stdin=subprocess.DEVNULL, capture_output=True, text=True, check=True, timeout=timeout)
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
        subprocess.run(
            cmd, stdin=subprocess.DEVNULL, capture_output=True, text=True, check=True, timeout=config.timeout_s
        )
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


def probe_http_provider(config: WhisperConfig, key: str | None = None) -> bool:
    """Authenticated HEAD only; no audio or ambient credentials are sent."""
    headers = {"Authorization": f"Bearer {key}"} if key else {}

    async def probe() -> bool:
        async with httpx.AsyncClient(trust_env=False, follow_redirects=False, timeout=STATUS_PROBE_TIMEOUT_S) as client:
            async with client.stream("HEAD", config.url or "", headers=headers) as response:
                return (
                    response.status_code not in (401, 403, 408, 429)
                    and response.status_code < 500
                    and not 300 <= response.status_code < 400
                )

    try:
        return asyncio.run(asyncio.wait_for(probe(), timeout=STATUS_PROBE_TIMEOUT_S))
    except (httpx.HTTPError, httpx.InvalidURL, ValueError, TimeoutError):
        return False


def confine_audio_path(source: str) -> str:
    """Explicit tool paths resolve inside the archive or configured media roots."""
    import whatsapp
    from errors import ToolError
    from media_upload import configured_media_root

    try:
        path = Path(source).expanduser()
        if not path.is_absolute():
            raise ValueError
        path = path.resolve()
        raw = os.getenv("WHATSAPP_MEDIA_ROOTS", "")
        roots = [Path(whatsapp.MESSAGES_DB_PATH).resolve().parent]
        roots.extend(Path(p.strip()).expanduser().resolve() for p in raw.split(os.pathsep) if p.strip())
        if not raw.strip():
            roots.append(Path(configured_media_root()).resolve())
        if not any(path.is_relative_to(root) for root in roots):
            raise ValueError
    except (OSError, RuntimeError, ValueError):
        raise ToolError("denied", "Audio file_path must stay inside the store or configured media roots") from None
    return str(path)


def _remaining(deadline: float) -> float:
    seconds = deadline - time.monotonic()
    if seconds <= 0:
        raise BackendUnavailableError("HTTP transcription exceeded whole-file deadline")
    return seconds


def _http_parts(source: str, work_dir: str, deadline: float) -> list[Path]:
    path = Path(source)
    size = path.stat().st_size
    if size > MAX_HTTP_AUDIO_BYTES:
        raise TranscriptionError("HTTP transcription input exceeds 256 MiB")
    try:
        # Restrict demuxers/protocols: playlists cannot fetch other local files
        # or remote URLs. Probe with the already packaged ffmpeg, no ffprobe.
        input_args = [
            "-protocol_whitelist",
            "file,pipe",
            "-format_whitelist",
            "ogg,wav,mp3,flac,aac,mov,matroska,amr,aiff,au",
            "-i",
            source,
        ]
        duration = subprocess.run(
            ["ffmpeg", "-nostdin", "-hide_banner", *input_args],
            stdin=subprocess.DEVNULL,
            capture_output=True,
            text=True,
            encoding="utf-8",
            errors="replace",
            timeout=min(ffmpeg_timeout_s(), _remaining(deadline)),
        )
        match = re.search(r"Duration: (\d+):(\d+):(\d+(?:\.\d+)?)", duration.stderr)
        if not match or "Audio:" not in duration.stderr:
            raise TranscriptionError("HTTP transcription input is not supported audio")
        hours, minutes, fraction = (float(v) for v in match.groups())
        seconds = hours * 3600 + minutes * 60 + fraction
        if not 0 < seconds <= MAX_HTTP_AUDIO_SECONDS:
            raise TranscriptionError("HTTP transcription audio duration must be at most 24 hours")
        # 32 kbit/s mono, ten-minute parts: far below 25 MB, in chronological order.
        subprocess.run(
            [
                "ffmpeg",
                "-nostdin",
                "-hide_banner",
                "-loglevel",
                "error",
                *input_args,
                "-map",
                "0:a:0",
                "-map_metadata",
                "-1",
                "-map_chapters",
                "-1",
                "-vn",
                "-ac",
                "1",
                "-ar",
                "16000",
                "-c:a",
                "libopus",
                "-b:a",
                "32k",
                "-f",
                "segment",
                "-segment_time",
                "600",
                "-reset_timestamps",
                "1",
                "-y",
                str(Path(work_dir) / "part-%04d.ogg"),
            ],
            stdin=subprocess.DEVNULL,
            capture_output=True,
            check=True,
            timeout=min(_remaining(deadline), max(ffmpeg_timeout_s(), 10 + seconds / 10)),
        )
    except FileNotFoundError:
        raise BackendUnavailableError("HTTP transcription requires ffmpeg") from None
    except subprocess.TimeoutExpired:
        _remaining(deadline)
        raise TranscriptionError("HTTP transcription audio preparation exceeded its duration-scaled limit") from None
    except (subprocess.CalledProcessError, ValueError):
        raise TranscriptionError("HTTP transcription could not prepare this audio") from None
    parts = sorted(Path(work_dir).glob("part-*.ogg"))
    if not parts or len(parts) > 145 or any(p.stat().st_size > HTTP_UPLOAD_LIMIT for p in parts):
        raise TranscriptionError("HTTP transcription chunk exceeds upload limits")
    return parts


def _transcribe_http(source: Path, config: WhisperConfig, language: str) -> str:
    # Both callers are synchronous (the MCP SDK runs sync tools in a worker).
    # Cancellation closes the socket even while headers/upload are trickling.
    async def bounded() -> str:
        try:
            return await asyncio.wait_for(_transcribe_http_request(source, config, language), config.timeout_s)
        except TimeoutError:
            raise BackendUnavailableError("HTTP transcription backend exceeded request deadline") from None

    return asyncio.run(bounded())


async def _transcribe_http_request(source: Path, config: WhisperConfig, language: str) -> str:
    data = {"model": config.model or "", "response_format": "json"}
    if language and language != "auto":
        data["language"] = language
    key = os.getenv("WHATSAPP_TRANSCRIPTION_API_KEY", "").strip()
    headers = {"Accept-Encoding": "identity"}
    if key:
        headers["Authorization"] = f"Bearer {key}"
    try:
        async with httpx.AsyncClient(
            trust_env=False,
            follow_redirects=False,
            timeout=httpx.Timeout(config.timeout_s, connect=min(10, config.timeout_s)),
        ) as client:
            with source.open("rb") as audio:
                async with client.stream(
                    "POST",
                    config.url or "",
                    headers=headers,
                    files={"file": (source.name, audio, "audio/ogg")},
                    data=data,
                ) as response:
                    status = response.status_code
                    if status in (401, 403, 404, 405, 408, 429) or status >= 500 or 300 <= status < 400:
                        raise BackendUnavailableError(f"HTTP transcription backend returned HTTP {status}")
                    if not 200 <= status < 300:
                        raise TranscriptionError(f"HTTP transcription backend rejected this file (HTTP {status})")
                    if response.headers.get("Content-Encoding", "identity").strip().lower() != "identity":
                        raise BackendUnavailableError("HTTP transcription backend encoded response refused")
                    body = bytearray()
                    async for chunk in response.aiter_raw():
                        if len(body) + len(chunk) > HTTP_RESPONSE_LIMIT:
                            raise BackendUnavailableError("HTTP transcription backend response exceeds limits")
                        body.extend(chunk)
                    payload = json.loads(body)
    except (httpx.HTTPError, httpx.InvalidURL):
        raise BackendUnavailableError("HTTP transcription backend request failed") from None
    except (ValueError, RecursionError):
        raise TranscriptionError("HTTP transcription backend returned invalid JSON") from None
    text = payload.get("text") if isinstance(payload, dict) else None
    if not isinstance(text, str) or not text.strip():
        raise TranscriptionError("HTTP transcription backend returned no non-empty text field")
    return text.strip()


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

    if backend == "openai_compatible":
        deadline = time.monotonic() + config.timeout_s
        with tempfile.TemporaryDirectory(prefix="wa-http-transcribe-") as tmp:
            parts = _http_parts(audio_path, tmp, deadline)
            text = " ".join(
                _transcribe_http(part, replace(config, timeout_s=_remaining(deadline)), lang) for part in parts
            )
        return {"text": text, "language": lang, "backend": backend, "provider": config.provider, "model": config.model}

    with tempfile.TemporaryDirectory(prefix="wa-whisper-") as tmp:
        wav_path = convert_to_wav16k(audio_path, os.path.join(tmp, "audio.wav"))
        if backend == "server":
            text = _transcribe_via_server(wav_path, config, lang)
        else:
            text = _transcribe_via_cli(wav_path, config, lang)

    return {"text": text, "language": lang, "backend": backend}
