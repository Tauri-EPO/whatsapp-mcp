"""Raw HTTP uploads, dispatched inside the existing bearer/rate middleware."""

from __future__ import annotations

import asyncio
import hashlib
from datetime import UTC, datetime
from urllib.parse import unquote

from mcp.server.transport_security import TransportSecurityMiddleware, TransportSecuritySettings
from starlette.requests import ClientDisconnect, Request
from starlette.responses import JSONResponse
from starlette.types import ASGIApp, Receive, Scope, Send

import media_upload
from errors import ToolError
from private_files import private_open
from tool_policy import active_policy

SWEEP_INTERVAL_S = 60


def refusal(code: str, message: str, status: int, **extra) -> JSONResponse:
    return JSONResponse(ToolError(code, message, **extra).to_dict(), status, headers={"Cache-Control": "no-store"})


class UploadApp:
    def __init__(
        self, app: ASGIApp, security: TransportSecuritySettings | None, limit: int = media_upload.MAX_INLINE_BYTES
    ):
        self.app = app
        self.security = TransportSecurityMiddleware(security)
        self.limit = limit
        media_upload.sweep_uploads()

    async def sweep_periodically(self) -> None:
        while True:
            await asyncio.sleep(SWEEP_INTERVAL_S)
            await asyncio.to_thread(media_upload.sweep_uploads)

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        if scope["type"] == "lifespan":
            sweeper = asyncio.create_task(self.sweep_periodically())
            try:
                await self.app(scope, receive, send)
            finally:
                sweeper.cancel()
                await asyncio.gather(sweeper, return_exceptions=True)
            return
        if scope["type"] != "http" or scope["path"] != "/upload":
            await self.app(scope, receive, send)
            return
        request = Request(scope, receive)
        # The SDK checks only its own transport endpoints. Reuse its validator,
        # without the JSON Content-Type check, on this raw-body route.
        refused = await self.security.validate_request(request)
        if refused is not None:
            await refused(scope, receive, send)
            return
        if request.method != "POST":
            response = refusal("invalid_argument", "Use POST /upload", 405)
            response.headers["Allow"] = "POST"
        else:
            response = await self.upload(request)
        await response(scope, receive, send)

    async def upload(self, request: Request) -> JSONResponse:
        try:
            policy = active_policy()
        except ToolError:
            return refusal("denied", "Runtime tool policy unavailable", 403)
        if not any(policy.allows(name) for name in ("send_file", "send_audio_message")):
            return refusal("denied", "File sending is disabled", 403)
        if request.headers.get("content-type", "").split(";", 1)[0].strip().lower() == "multipart/form-data":
            return refusal("invalid_argument", "Send raw bytes with --data-binary, not multipart/form-data", 415)
        try:
            declared = request.headers.get("content-length")
            if declared is not None:
                if int(declared) < 0:
                    raise ValueError
                if int(declared) > self.limit:
                    return refusal("too_large", "File exceeds the upload size limit", 413, limit_bytes=self.limit)
            name = request.headers.get("x-filename", "")
            if not name.strip():
                return refusal("invalid_argument", "X-Filename is required; percent-encode UTF-8 names", 400)
            name.encode("ascii")
            filename = media_upload.safe_filename(unquote(name, encoding="utf-8", errors="strict"), "")
            if not filename:
                return refusal("invalid_argument", "X-Filename must contain a usable base name", 400)
        except (ValueError, UnicodeError):
            return refusal("invalid_argument", "Invalid Content-Length or X-Filename; percent-encode UTF-8 names", 400)
        try:
            with media_upload.receiving_upload(self.limit) as upload:
                size = 0
                digest = hashlib.sha256()
                with private_open(upload.pending, "xb", buffering=0) as handle:
                    async for chunk in request.stream():
                        size += len(chunk)
                        if size > self.limit:
                            return refusal(
                                "too_large", "File exceeds the upload size limit", 413, limit_bytes=self.limit
                            )
                        upload.write(handle, chunk)
                        digest.update(chunk)
                if not size:
                    return refusal("invalid_argument", "The file is empty", 400)
                upload.finish(filename)
                return JSONResponse(
                    {
                        "upload_id": upload.upload_id,
                        "filename": filename,
                        "bytes": size,
                        "sha256": digest.hexdigest(),
                        "expires_at": datetime.fromtimestamp(
                            media_upload.upload_expiry(upload.upload_id), UTC
                        ).isoformat(),
                    },
                    headers={"Cache-Control": "no-store"},
                )
        except ToolError as exc:
            return JSONResponse(
                exc.to_dict(),
                413 if exc.code == "too_large" else 403 if exc.code == "denied" else 500,
                headers={"Cache-Control": "no-store"},
            )
        except (OSError, ClientDisconnect):
            return refusal("internal", "Upload could not be stored", 500)
