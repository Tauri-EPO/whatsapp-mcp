"""Side-effect-free helpers for MCP server configuration env vars."""

import errno
import os
import socket
import stat
from pathlib import Path

# Accepted WHATSAPP_MCP_TRANSPORT values mapped to MCP SDK transport names.
# "http" is a friendly alias for the spec's current "streamable-http" transport.
TRANSPORT_ALIASES = {
    "stdio": "stdio",
    "http": "streamable-http",
    "streamable-http": "streamable-http",
    "streamable_http": "streamable-http",
    "sse": "sse",
}
DEFAULT_MCP_HOST = "127.0.0.1"
DEFAULT_MCP_PORT = 8000


def resolve_transport(value: str | None) -> str:
    """Map a WHATSAPP_MCP_TRANSPORT value to an MCP SDK transport name.

    Unset or whitespace-only values default to "stdio".
    Raises ValueError for unrecognized values.
    """
    normalized = (value or "").strip().lower() or "stdio"
    try:
        return TRANSPORT_ALIASES[normalized]
    except KeyError:
        accepted = ", ".join(sorted(TRANSPORT_ALIASES))
        raise ValueError(
            f"Invalid WHATSAPP_MCP_TRANSPORT={value!r}; recommended values: stdio, http, sse "
            f"(http maps to the spec's streamable-http transport; all accepted inputs: {accepted})"
        ) from None


def resolve_host(value: str | None) -> str:
    """Parse WHATSAPP_MCP_HOST, defaulting to DEFAULT_MCP_HOST."""
    return (value or "").strip() or DEFAULT_MCP_HOST


def resolve_port(value: str | None) -> int:
    """Parse WHATSAPP_MCP_PORT, defaulting to DEFAULT_MCP_PORT.

    Unset or whitespace-only values default to DEFAULT_MCP_PORT.
    Raises ValueError for non-integer or out-of-range values.
    """
    value = (value or "").strip()
    if not value:
        return DEFAULT_MCP_PORT
    try:
        port = int(value)
    except ValueError:
        raise ValueError(f"Invalid WHATSAPP_MCP_PORT={value!r}; must be an integer") from None
    if not 1 <= port <= 65535:
        raise ValueError(f"Invalid WHATSAPP_MCP_PORT={value!r}; must be between 1 and 65535") from None
    return port


# Host header values the MCP SDK accepts by default when bound to loopback.
# Mirrors the SDK's own loopback allow-list so that enabling extra
# hosts never locks out local clients.
LOOPBACK_HOSTS = ("127.0.0.1", "localhost", "[::1]")
LOOPBACK_BIND_ADDRESSES = ("127.0.0.1", "localhost", "::1")
# Disables DNS-rebinding protection entirely (allow any Host header).
ALLOW_ANY_HOST = "*"


def _split_csv(value: str | None) -> list[str]:
    return [item.strip() for item in (value or "").split(",") if item.strip()]


def _expand_host_patterns(hosts: list[str]) -> list[str]:
    """Return SDK-style Host patterns for the given entries.

    The SDK matches either an exact ``host[:port]`` string or a ``host:*``
    wildcard. A bare hostname is expanded to both forms so that
    ``WHATSAPP_MCP_ALLOWED_HOSTS=mcp.example.ts.net`` matches requests that
    arrive with or without an explicit port (a TLS terminator such as
    Tailscale Serve/Funnel forwards ``Host: mcp.example.ts.net``, while a
    direct call carries ``Host: mcp.example.ts.net:8000``).
    """
    expanded: list[str] = []
    for host in hosts:
        candidates = [host]
        if not host.endswith(":*") and not _has_explicit_port(host):
            candidates.append(f"{host}:*")
        for candidate in candidates:
            if candidate not in expanded:
                expanded.append(candidate)
    return expanded


def _has_explicit_port(host: str) -> bool:
    if host.startswith("["):
        return "]:" in host
    return host.count(":") == 1


def _origins_for_hosts(hosts: list[str]) -> list[str]:
    origins: list[str] = []
    for host in hosts:
        for scheme in ("http", "https"):
            origin = f"{scheme}://{host}"
            if origin not in origins:
                origins.append(origin)
    return origins


def resolve_allowed_hosts(value: str | None) -> list[str] | None:
    """Parse WHATSAPP_MCP_ALLOWED_HOSTS.

    Returns None when unset, an empty list when set to ``*`` (allow any Host),
    otherwise the configured entries expanded into SDK ``host`` / ``host:*``
    patterns, always including the loopback spellings.
    """
    entries = _split_csv(value)
    if not entries:
        return None
    if ALLOW_ANY_HOST in entries:
        return []
    return _expand_host_patterns(list(LOOPBACK_HOSTS) + entries)


def build_transport_security(host: str, allowed_hosts_env: str | None, allowed_origins_env: str | None = None):
    """Derive the transport-security settings passed to ``MCPServer.run()``.

    The SDK auto-enables a loopback-only DNS-rebinding ``Host`` allow-list when
    the bind address is loopback and applies none otherwise. This adds the
    operator's allow-list on top so a non-loopback bind (Tailscale, Docker,
    reverse proxy) can keep the protection on instead of turning it off.

    Return explicit settings so the transport and additional routes share
    the SDK's loopback defaults as well as the operator's allow-list.
    """
    from mcp.server.transport_security import TransportSecuritySettings

    allowed_hosts = resolve_allowed_hosts(allowed_hosts_env)
    if allowed_hosts is None:
        if host in LOOPBACK_BIND_ADDRESSES:
            return TransportSecuritySettings(
                allowed_hosts=["127.0.0.1:*", "localhost:*", "[::1]:*"],
                allowed_origins=["http://127.0.0.1:*", "http://localhost:*", "http://[::1]:*"],
            )
        # Bound to a non-loopback address without an allow-list: any Host is
        # accepted (explicitly, so the intent is visible). main.py warns on stderr.
        return TransportSecuritySettings(enable_dns_rebinding_protection=False)
    if not allowed_hosts:
        return TransportSecuritySettings(enable_dns_rebinding_protection=False)

    origins = _origins_for_hosts(allowed_hosts) + _split_csv(allowed_origins_env)
    return TransportSecuritySettings(
        enable_dns_rebinding_protection=True,
        allowed_hosts=allowed_hosts,
        allowed_origins=list(dict.fromkeys(origins)),
    )


def resolve_socket(value: str | None, store: str, media_roots: str | None = None) -> str | None:
    """Validate an additional Unix listener without filesystem mutation."""
    if not value or not value.strip():
        return None
    path = Path(value.strip())
    if os.name != "posix" or not path.is_absolute() or len(os.fsencode(path)) > 100:
        raise ValueError("WHATSAPP_MCP_SOCKET requires a Unix absolute path of at most 100 bytes")
    roots = [Path(store), Path.home() / ".local/share/whatsapp-mcp/outbox"]
    roots += [Path(root) for root in (media_roots or "").split(os.pathsep) if root]
    if any(path.resolve().is_relative_to(root.resolve()) for root in roots):
        raise ValueError("WHATSAPP_MCP_SOCKET must be outside store, outbox and media roots")
    if any(part.is_symlink() for part in (path, *path.parents)):
        raise ValueError("WHATSAPP_MCP_SOCKET refuses symlinks")
    try:
        parent = path.parent.stat()
    except OSError:
        raise ValueError("WHATSAPP_MCP_SOCKET parent must exist") from None
    if not stat.S_ISDIR(parent.st_mode) or parent.st_mode & 0o002:
        raise ValueError("WHATSAPP_MCP_SOCKET parent must not be world-writable")
    return str(path)


def bind_unix_socket(path: str) -> socket.socket:
    """Bind uid 1000 / mode 0660; replace only our demonstrably stale socket."""
    if getattr(os, "geteuid")() not in (0, 1000):
        raise ValueError("WHATSAPP_MCP_SOCKET requires uid 1000")
    if any(part.is_symlink() for part in (Path(path), *Path(path).parents)):
        raise ValueError("WHATSAPP_MCP_SOCKET refuses symlinks")
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        info = None
    if info is not None:
        if not stat.S_ISSOCK(info.st_mode) or info.st_uid != 1000:
            raise ValueError("WHATSAPP_MCP_SOCKET refuses an existing non-owned socket or file")
        with socket.socket(getattr(socket, "AF_UNIX")) as probe:
            probe.settimeout(1)
            try:
                probe.connect(path)
            except OSError as exc:
                if exc.errno != errno.ECONNREFUSED:
                    raise ValueError("WHATSAPP_MCP_SOCKET cannot prove stale socket") from None
            else:
                raise ValueError("WHATSAPP_MCP_SOCKET is already listening")
        if os.lstat(path) != info:
            raise ValueError("WHATSAPP_MCP_SOCKET changed before removing stale socket")
        os.unlink(path)
    listener = socket.socket(getattr(socket, "AF_UNIX"))
    owned = None
    try:
        listener.bind(path)
        owned = os.lstat(path)
        os.chmod(path, 0o660)  # nosec B103 - Unix socket group access is the explicit local proxy boundary.
        if getattr(os, "geteuid")() == 0:
            getattr(os, "chown")(path, 1000, 1000)
        elif getattr(os, "geteuid")() != 1000:
            raise ValueError("WHATSAPP_MCP_SOCKET requires uid 1000")
        listener.set_inheritable(True)
        return listener
    except BaseException:
        listener.close()
        if owned:
            try:
                current = os.lstat(path)
                if (owned.st_dev, owned.st_ino) == (current.st_dev, current.st_ino):
                    os.unlink(path)
            except FileNotFoundError:
                pass
        raise


def run_http_listeners(app, host: str, port: int, socket_path: str | None):
    """One uvicorn Server/app/lifespan, listening on TCP and optional Unix."""
    import uvicorn

    if not socket_path:
        # Keep uvicorn's existing hostname resolution and TCP startup behavior.
        uvicorn.run(app, host=host, port=port, log_level="info", proxy_headers=False)
        return
    config = uvicorn.Config(app, host=host, port=port, log_level="info", proxy_headers=False)
    sockets = []
    owned = None
    try:
        if socket_path:
            sockets.append(bind_unix_socket(socket_path))
            owned = os.lstat(socket_path)
        addresses = socket.getaddrinfo(host, port, type=socket.SOCK_STREAM)
        seen = set()
        for family, socktype, protocol, _, address in addresses:
            if (family, address) in seen:
                continue
            seen.add((family, address))
            listener = socket.socket(family, socktype, protocol)
            sockets.append(listener)
            listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            if family == socket.AF_INET6:
                listener.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 1)
            listener.bind(address)
            listener.set_inheritable(True)
        server = uvicorn.Server(config)
        server.run(sockets=sockets)
        if not server.started:
            raise SystemExit(3)  # Preserve uvicorn.run's nonzero startup-failure contract.
    finally:
        for listener in sockets:
            listener.close()
        if socket_path and owned:
            try:
                current = os.lstat(socket_path)
                if (owned.st_dev, owned.st_ino) == (current.st_dev, current.st_ino):
                    os.unlink(socket_path)
            except FileNotFoundError:
                pass


def probe_http_health():
    """Probe TCP and, when configured, Unix without inherited proxy settings."""
    import http.client

    host = resolve_host(os.getenv("WHATSAPP_MCP_HOST"))
    host = {"0.0.0.0": "127.0.0.1", "::": "::1"}.get(host, host)  # nosec B104 - Outbound probe maps wildcard binds to loopback.
    connections = [http.client.HTTPConnection(host, resolve_port(os.getenv("WHATSAPP_MCP_PORT")), timeout=4)]
    path = (os.getenv("WHATSAPP_MCP_SOCKET") or "").strip()
    if path:
        connection = http.client.HTTPConnection("localhost", timeout=4)

        def connect():
            connection.sock = socket.socket(getattr(socket, "AF_UNIX"))
            connection.sock.settimeout(4)
            connection.sock.connect(path)

        connection.connect = connect
        connections.append(connection)
    for connection in connections:
        try:
            connection.request("GET", "/mcp")
            connection.getresponse().close()
        finally:
            connection.close()
