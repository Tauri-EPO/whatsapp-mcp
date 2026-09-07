"""Expiry of the TLS certificate on the published MCP endpoint.

The containers never terminate TLS: they bind loopback and something on the
host publishes them (``tailscale serve``, a reverse proxy). When that
certificate expires, every direct client fails at the handshake while
``bridge_status`` keeps reporting ``ok: true`` -- it only ever talks to the
bridge over loopback and has no idea what the outside world is served.

``WHATSAPP_PUBLIC_URL`` closes that blind spot. Set it to the URL clients use
(``https://host.tailnet.ts.net/mcp``) and ``bridge_status`` adds
``endpoint_cert_expires_at`` / ``endpoint_cert_days_left``, plus
``endpoint_cert_error`` when the handshake fails. Nothing is requested over the
connection: it is a TLS handshake and a close, the result is cached per host
(an hour when the certificate was read, a minute after a failure), and no
failure here can make ``bridge_status`` fail.

The probe runs from wherever this server runs, which under compose is the
bridge's network namespace rather than the host: an endpoint the host publishes
under a name only the host can resolve answers "cannot reach" here even though
clients are fine. ``docs/CONFIGURATION.md`` says how to tell the two apart.

The chain is verified with the default context, so an expired or untrusted
certificate surfaces as an error rather than as a silent pass. The date is
still reported in that case: a second, unverified handshake fetches the leaf
certificate, which is exactly the number an operator needs to see when the
error says "certificate has expired".
"""

from __future__ import annotations

import logging
import math
import os
import socket
import ssl
import tempfile
import threading
import time
from collections.abc import Callable, Mapping
from datetime import UTC, datetime
from typing import Any
from urllib.parse import urlsplit

logger = logging.getLogger("whatsapp_mcp")

PUBLIC_URL_ENV = "WHATSAPP_PUBLIC_URL"
DEFAULT_PORT = 443
# Per handshake, and a verification failure costs a second one. A published
# endpoint is one hop away over the tailnet; three seconds is a generous
# handshake and still short enough for the tool an agent calls first.
HANDSHAKE_TIMEOUT_S = 3.0
# Certificates are renewed on a scale of days. One probe an hour is plenty and
# keeps bridge_status cheap when an agent polls it.
CACHE_TTL_S = 3600.0
# A failure is not cached that long: the operator who renews the certificate
# re-runs bridge_status straight away and must not read the stale error.
ERROR_CACHE_TTL_S = 60.0

_cache: dict[tuple[str, int], tuple[float, dict[str, Any]]] = {}
_cache_lock = threading.Lock()


def endpoint_target(raw: str | None) -> tuple[str, int] | None:
    """(host, port) from ``WHATSAPP_PUBLIC_URL``; None when it is unset.

    Accepts the full client URL (``https://host.ts.net/mcp``) as well as a bare
    ``host`` or ``host:port``. Anything that is not HTTPS is refused: there is
    no certificate to look at.
    """
    value = (raw or "").strip()
    if not value:
        return None
    try:
        parts = urlsplit(value if "://" in value else f"https://{value}")
        if parts.scheme != "https":
            raise ValueError("it is not an https:// URL; there is no certificate to check")
        host, port = parts.hostname, parts.port or DEFAULT_PORT
    except ValueError as exc:  # unusable port, malformed IPv6 literal, …
        raise ValueError(f"{PUBLIC_URL_ENV}={raw!r} is unusable: {exc}") from None
    if not host:
        raise ValueError(f"{PUBLIC_URL_ENV}={raw!r} is unusable: it names no host")
    return host, port


def _describe(exc: Exception, host: str, port: int) -> str:
    """One line an operator can act on, naming what failed."""
    if isinstance(exc, ssl.SSLCertVerificationError):
        return f"certificate verify failed for {host}:{port}: {exc.verify_message or exc.reason}"
    if isinstance(exc, ssl.SSLError):
        return f"TLS handshake with {host}:{port} failed: {exc.reason or exc}"
    return f"cannot reach {host}:{port}: {exc}"


def _peer_cert(host: str, port: int, timeout_s: float, context: ssl.SSLContext) -> dict[str, Any]:
    """One handshake, no request: the peer certificate as ssl decodes it."""
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    with socket.create_connection((host, port), timeout=timeout_s) as raw:
        raw.settimeout(timeout_s)
        with context.wrap_socket(raw, server_hostname=host) as tls:
            return dict(tls.getpeercert() or {})


def _unverified_not_after(host: str, port: int, timeout_s: float) -> str | None:
    """``notAfter`` of the leaf certificate, whatever is wrong with the chain.

    ``getpeercert()`` returns nothing for a certificate OpenSSL did not
    validate, and neither ``ssl`` nor ``socket`` can decode one; the leaf is
    fetched in binary form and handed back to the same decoder the module uses
    for its own tests. Best effort by design -- the caller already has the
    error to report, this only adds the date to it.
    """
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    context.check_hostname = False
    context.verify_mode = ssl.CERT_NONE
    try:
        with socket.create_connection((host, port), timeout=timeout_s) as raw:
            raw.settimeout(timeout_s)
            with context.wrap_socket(raw, server_hostname=host) as tls:
                der = tls.getpeercert(binary_form=True)
        if not der:
            return None
        # Imported here, not at module level: this is the one place that needs
        # the certificate decoder `ssl` itself does not expose, and a build
        # without it must cost the caller nothing more than a missing date.
        import _ssl

        decode = getattr(_ssl, "_test_decode_cert", None)
        if decode is None:
            logger.warning(
                "endpoint certificate: this Python cannot decode an unverified certificate, "
                "so %s:%s reports the error without its expiry date",
                host,
                port,
            )
            return None
        with tempfile.TemporaryDirectory(prefix="wa-cert-") as tmp:
            path = os.path.join(tmp, "leaf.pem")
            with open(path, "w", encoding="ascii") as fh:
                fh.write(ssl.DER_cert_to_PEM_cert(der))
            decoded = decode(path)
        not_after = decoded.get("notAfter") if isinstance(decoded, dict) else None
        return str(not_after) if not_after else None
    except Exception as exc:  # the error is already reported; the date is a bonus
        logger.debug("endpoint certificate: could not read the expiry of %s:%s (%s)", host, port, exc)
        return None


def _expiry_fields(not_after: str, now: float) -> dict[str, Any]:
    """``notAfter`` as ISO-8601 UTC plus whole days left (negative once expired)."""
    seconds = ssl.cert_time_to_seconds(not_after)
    expires = datetime.fromtimestamp(seconds, tz=UTC)
    return {
        "endpoint_cert_expires_at": expires.strftime("%Y-%m-%dT%H:%M:%SZ"),
        "endpoint_cert_days_left": int(math.floor((seconds - now) / 86400)),
    }


def probe(
    host: str,
    port: int,
    timeout_s: float = HANDSHAKE_TIMEOUT_S,
    context_factory: Callable[[], ssl.SSLContext] = ssl.create_default_context,
    now: float | None = None,
) -> dict[str, Any]:
    """Handshake with host:port and report what the certificate says.

    Returns the expiry fields on success; on failure ``endpoint_cert_error``,
    with the expiry fields still filled in whenever the leaf could be read.
    Never raises.
    """
    clock = time.time() if now is None else now
    not_after: str | None = None
    error: str | None = None
    try:
        cert = _peer_cert(host, port, timeout_s, context_factory())
        not_after = str(cert.get("notAfter") or "") or None
        if not_after is None:
            error = f"{host}:{port} presented no certificate"
    except ssl.SSLError as exc:
        error = _describe(exc, host, port)
        not_after = _unverified_not_after(host, port, timeout_s)
    except OSError as exc:  # refused, unreachable, timed out: no second try
        error = _describe(exc, host, port)

    result: dict[str, Any] = {}
    if not_after:
        try:
            result.update(_expiry_fields(not_after, clock))
        except ValueError as exc:
            error = error or f"{host}:{port} returned an unreadable expiry date: {exc}"
    if error:
        result["endpoint_cert_error"] = error
    return result


def status(
    env: Mapping[str, str] | None = None,
    probe_fn: Callable[[str, int], dict[str, Any]] | None = None,
) -> dict[str, Any]:
    """Certificate fields for ``bridge_status``; empty when the URL is unset.

    The answer is cached per host, so an agent polling the status does not open
    a TLS connection every time: an hour for a certificate that was read, one
    minute for a failure, so a renewal on the host shows up almost at once.
    """
    source: Mapping[str, str] = os.environ if env is None else env
    try:
        target = endpoint_target(source.get(PUBLIC_URL_ENV))
    except ValueError as exc:
        return {"endpoint_cert_error": str(exc)}
    if target is None:
        return {}

    with _cache_lock:
        cached = _cache.get(target)
        if cached is not None and cached[0] > time.monotonic():
            return dict(cached[1])
    result = (probe_fn or probe)(*target)
    ttl = ERROR_CACHE_TTL_S if "endpoint_cert_error" in result else CACHE_TTL_S
    with _cache_lock:
        # Anchored after the probe: the handshake itself must not eat the TTL.
        _cache[target] = (time.monotonic() + ttl, dict(result))
    return dict(result)


def reset_cache() -> None:
    """Forget every cached probe (tests, and a reload of the configuration)."""
    with _cache_lock:
        _cache.clear()
