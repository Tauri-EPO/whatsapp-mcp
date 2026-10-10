"""Authenticated media reads for the S3 backend; object credentials stay in Go."""

from __future__ import annotations

import hashlib
import json
import os
import tempfile
from collections.abc import Sequence
from contextlib import contextmanager
from urllib.parse import quote, unquote, urlsplit

import httpx

import whatsapp
from errors import ToolError


class MediaReadError(ToolError):
    """A remote media failure, distinct from transcription accounting errors."""


def enabled() -> bool:
    return os.environ.get("WHATSAPP_MEDIA_BACKEND", "local").strip() == "s3"


def uri(chat_jid: str, message_id: str) -> str:
    return f"whatsapp://media/{quote(chat_jid, safe='')}/{quote(message_id, safe='')}"


def catalog(chat_jid: str, message_id: str | None = None) -> dict[str, dict]:
    whatsapp._require_allowed(chat_jid)
    items = {}
    cursor = ""
    params = {"chat_jid": chat_jid}
    if message_id is not None:
        params["message_id"] = message_id
    while True:
        result = whatsapp._bridge_json(
            whatsapp._bridge_request("GET", "/media/cache", params={**params, "cursor": cursor})
        )
        items.update({item["message_id"]: item for item in result["items"]})
        next_cursor = result.get("next_cursor", "")
        if not next_cursor:
            return items
        if next_cursor <= cursor:
            raise ToolError("internal", "media catalog cursor did not advance")
        cursor = next_cursor


def lookup(chat_jid: str, message_id: str) -> dict | None:
    """One indexed bridge query, regardless of how many files a chat has."""
    return catalog(chat_jid, message_id).get(message_id)


def lookup_many(chat_jid: str, message_ids: Sequence[str]) -> dict[str, dict]:
    """Only the requested identities, in bridge-sized batches of 256."""
    whatsapp._require_allowed(chat_jid)
    ids = list(dict.fromkeys(message_ids))
    items = {}
    for start in range(0, len(ids), 256):
        batch = ids[start : start + 256]
        params = [("chat_jid", chat_jid), *(("message_id", message_id) for message_id in batch)]
        result = whatsapp._bridge_json(whatsapp._bridge_request("GET", "/media/cache", params=params))
        if result.get("next_cursor"):
            raise ToolError("internal", "media identity batch exceeded the catalog bound")
        items.update({item["message_id"]: item for item in result["items"] if item["message_id"] in batch})
    return items


def identity(value: str) -> tuple[str, str]:
    parsed = urlsplit(value)
    parts = parsed.path.removeprefix("/").split("/")
    if parsed.scheme != "whatsapp" or parsed.netloc != "media" or len(parts) != 2 or parsed.query or parsed.fragment:
        raise ToolError("invalid_argument", "invalid media resource URI")
    chat, message = (unquote(part) for part in parts)
    if any(part in {"", ".", ".."} or "/" in part or "\\" in part for part in (chat, message)):
        raise ToolError("invalid_argument", "invalid media resource URI")
    whatsapp._require_allowed(chat)
    return chat, message


def _read_error(response: httpx.Response) -> MediaReadError:
    """Retain permanent outcome codes without forwarding a remote diagnostic."""
    payload = bytearray()
    for chunk in response.iter_bytes(4096):
        payload.extend(chunk)
        if len(payload) > 8192:
            return MediaReadError("bridge_unavailable", "bridge media read failed")
    try:
        body = json.loads(payload)
    except (ValueError, UnicodeError):
        body = None
    failure = body.get("error") if isinstance(body, dict) else None
    code = failure.get("code") if isinstance(failure, dict) else None
    if code == "media_unavailable":
        return MediaReadError(code, "media is permanently unavailable")
    if code == "media_refused":
        return MediaReadError(code, "media identity refused")
    if code == "too_large":
        return MediaReadError(code, "media exceeds the requested byte limit")
    return MediaReadError("bridge_unavailable", "bridge media read failed")


def read_range(value: str, offset: int, length: int, sha256: str | None, *, cache_only: bool = False):
    """A bounded bridge byte range and its verified whole-file identity."""
    chat, message = identity(value)
    params = {
        "chat_jid": chat,
        "message_id": message,
        "offset": str(offset),
        "length": str(length),
        "cache_only": str(cache_only).lower(),
    }
    target = f"{whatsapp.WHATSAPP_API_BASE_URL}/media/blob"
    try:
        with whatsapp.bridge_http.stream(
            "GET", target, params=params, headers=whatsapp._bridge_headers(), timeout=whatsapp.BRIDGE_MEDIA_TIMEOUT_S
        ) as response:
            if response.status_code == 400:
                raise MediaReadError("invalid_argument", "invalid media byte range")
            if response.status_code != 200:
                raise _read_error(response)
            try:
                size = int(response.headers["X-Media-Total-Bytes"])
                digest = response.headers["X-Media-SHA256"].lower()
                valid_hash = len(bytes.fromhex(digest)) == 32 and len(digest) == 64
            except (KeyError, ValueError) as exc:
                raise MediaReadError("internal", "invalid media range metadata") from exc
            if size < offset or not valid_hash:
                raise MediaReadError("internal", "invalid media range metadata")
            if sha256 and digest != sha256.lower():
                raise MediaReadError("conflict", "media changed; restart from offset 0")
            expected = min(length, size - offset)
            data = bytearray()
            for chunk in response.iter_bytes(65536):
                if len(data) + len(chunk) > expected:
                    raise MediaReadError("internal", "media range exceeded the requested length")
                data.extend(chunk)
            if len(data) != expected:
                raise MediaReadError("conflict", "media range was incomplete; restart from offset 0")
            return bytes(data), size, digest
    except httpx.HTTPError as exc:
        raise MediaReadError("bridge_unavailable", "bridge media stream failed") from exc


@contextmanager
def local_file(value: str, limit: int, sha256: str | None = None, *, cache_only: bool = False):
    """One bounded, private spool; removed even if rendering/extraction fails."""
    if not value.startswith("whatsapp://"):
        yield value
        return
    chat, message = identity(value)
    params = {
        "chat_jid": chat,
        "message_id": message,
        "cache_only": str(cache_only).lower(),
        "max_bytes": str(max(1, limit)),
    }
    target = f"{whatsapp.WHATSAPP_API_BASE_URL}/media/blob"
    try:
        with tempfile.TemporaryDirectory(prefix="wamcp-read-") as directory:
            path = os.path.join(directory, "media")
            with whatsapp.bridge_http.stream(
                "GET",
                target,
                params=params,
                headers=whatsapp._bridge_headers(),
                timeout=whatsapp.BRIDGE_MEDIA_TIMEOUT_S,
            ) as response:
                if response.status_code != 200:
                    raise _read_error(response)
                total = 0
                digest = hashlib.sha256()
                with open(path, "xb") as output:
                    for chunk in response.iter_bytes(65536):
                        total += len(chunk)
                        if total > limit:
                            raise MediaReadError("too_large", "media exceeds the requested byte limit")
                        digest.update(chunk)
                        output.write(chunk)
                if sha256 and digest.hexdigest() != sha256.lower():
                    raise MediaReadError("internal", "media plaintext SHA256 mismatch")
            yield path
    except httpx.HTTPError as exc:
        raise MediaReadError("bridge_unavailable", "bridge media stream failed") from exc
