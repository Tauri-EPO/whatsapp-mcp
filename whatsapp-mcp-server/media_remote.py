"""Authenticated media reads for the S3 backend; object credentials stay in Go."""

from __future__ import annotations

import hashlib
import os
import tempfile
from contextlib import contextmanager
from urllib.parse import quote, unquote, urlsplit

import httpx

import whatsapp
from errors import ToolError


def enabled() -> bool:
    return os.environ.get("WHATSAPP_MEDIA_BACKEND", "local").strip() == "s3"


def uri(chat_jid: str, message_id: str) -> str:
    return f"whatsapp://media/{quote(chat_jid, safe='')}/{quote(message_id, safe='')}"


def catalog(chat_jid: str) -> dict[str, dict]:
    whatsapp._require_allowed(chat_jid)
    items = {}
    cursor = ""
    while True:
        result = whatsapp._bridge_json(
            whatsapp._bridge_request("GET", "/media/cache", params={"chat_jid": chat_jid, "cursor": cursor})
        )
        items.update({item["message_id"]: item for item in result["items"]})
        next_cursor = result.get("next_cursor", "")
        if not next_cursor:
            return items
        if next_cursor <= cursor:
            raise ToolError("internal", "media catalog cursor did not advance")
        cursor = next_cursor


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


@contextmanager
def local_file(value: str, limit: int, sha256: str | None = None, *, cache_only: bool = False):
    """One bounded, private spool; removed even if rendering/extraction fails."""
    if not value.startswith("whatsapp://"):
        yield value
        return
    chat, message = identity(value)
    params = {"chat_jid": chat, "message_id": message, "cache_only": str(cache_only).lower()}
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
                    raise ToolError("bridge_unavailable", "bridge media read failed")
                total = 0
                digest = hashlib.sha256()
                with open(path, "xb") as output:
                    for chunk in response.iter_bytes(65536):
                        total += len(chunk)
                        if total > limit:
                            raise ToolError("too_large", "media exceeds the requested byte limit")
                        digest.update(chunk)
                        output.write(chunk)
                if sha256 and digest.hexdigest() != sha256.lower():
                    raise ToolError("internal", "media plaintext SHA256 mismatch")
            yield path
    except httpx.HTTPError as exc:
        raise ToolError("bridge_unavailable", "bridge media stream failed") from exc
