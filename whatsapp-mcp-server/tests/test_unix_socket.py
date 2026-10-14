"""Real TCP/Unix MCP parity, plus path and forwarding denial cases."""

import os
import socket
import stat
import subprocess
import sys
import time
from pathlib import Path

import httpx
import pytest

from http_auth import client_key, resolve_trusted_proxies
from mcp_config import bind_unix_socket, resolve_socket


def test_unix_peer_forwarding_requires_explicit_trust():
    scope = {"client": None, "headers": [(b"x-forwarded-for", b"198.51.100.2")], "type": "http"}
    assert client_key(scope) == "unix"
    assert client_key(scope, resolve_trusted_proxies("loopback")) == "unix"
    assert client_key(scope, resolve_trusted_proxies("unix")) == "198.51.100.2"
    scope["headers"] = [(b"x-forwarded-for", b"invalid,198.51.100.2")]
    assert client_key(scope, resolve_trusted_proxies("unix")) == "unix"
    scope["client"] = ("198.51.100.10", 123)
    scope["headers"] = [(b"x-forwarded-for", b"198.51.100.2")]
    assert client_key(scope, resolve_trusted_proxies("unix")) == "198.51.100.10"


UNIX = pytest.mark.skipif(os.name != "posix", reason="Unix listener needs POSIX")
OWNER = pytest.mark.skipif(
    os.name != "posix" or getattr(os, "geteuid", lambda: -1)() not in (0, 1000),
    reason="Fixed uid 1000 is verified inside the image smoke",
)


@UNIX
def test_socket_paths_refuse_store_outbox_and_symlinks(tmp_path):
    store, outbox = tmp_path / "store", tmp_path / "outbox"
    store.mkdir()
    outbox.mkdir()
    for path in (store / "socket", outbox / "socket"):
        with pytest.raises(ValueError, match="outside"):
            resolve_socket(str(path), str(store), str(outbox))
    link = tmp_path / "link"
    link.symlink_to(tmp_path, target_is_directory=True)
    with pytest.raises(ValueError, match="symlinks"):
        resolve_socket(str(link / "socket"), str(store), str(outbox))
    with pytest.raises(ValueError, match="absolute"):
        resolve_socket("relative.sock", str(store))


@UNIX
@OWNER
def test_socket_mode_live_stale_foreign_file_and_symlink(tmp_path):
    path = str(tmp_path / "mcp.sock")
    listener = bind_unix_socket(path)
    listener.listen(1)
    info = os.lstat(path)
    assert stat.S_IMODE(info.st_mode) == 0o660 and info.st_uid == 1000
    with pytest.raises(ValueError, match="already listening"):
        bind_unix_socket(path)
    listener.close()  # Keep the inode to simulate a crash.
    replacement = bind_unix_socket(path)
    replacement.close()
    os.unlink(path)
    Path(path).write_text("preserve")
    with pytest.raises(ValueError, match="non-owned"):
        bind_unix_socket(path)
    assert Path(path).read_text() == "preserve"
    os.unlink(path)
    Path(path).symlink_to(tmp_path / "target")
    with pytest.raises(ValueError, match="symlinks"):
        bind_unix_socket(path)


@UNIX
@OWNER
def test_real_mcp_tcp_unix_parity_and_host_bearer_denials(tmp_path, paired_store):
    path = str(tmp_path / "mcp.sock")
    token = "synthetic-socket-token-0123456789"
    with socket.socket() as reserve:
        reserve.bind(("127.0.0.1", 0))
        port = reserve.getsockname()[1]
    env = dict(
        os.environ,
        WHATSAPP_MCP_TRANSPORT="http",
        WHATSAPP_MCP_HOST="localhost",
        WHATSAPP_MCP_PORT=str(port),
        WHATSAPP_MCP_SOCKET=f" {path} ",
        WHATSAPP_MCP_TOKEN=token,
        WHATSAPP_MCP_RATE_LIMIT="off",
        WHATSAPP_MCP_ALLOWED_HOSTS="localhost",
        WHATSAPP_DB_PATH=str(paired_store.messages_db),
        WHATSAPP_STORE_DIR=str(tmp_path / "store"),
        TRANSCRIBE_ON_INGEST="false",
    )
    process = subprocess.Popen(
        [sys.executable, str(Path(__file__).parents[1] / "main.py")],
        env=env,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.PIPE,
    )
    try:
        deadline = time.monotonic() + 15
        while not Path(path).exists() and process.poll() is None and time.monotonic() < deadline:
            time.sleep(0.05)
        transports = [httpx.HTTPTransport(uds=path), httpx.HTTPTransport(), httpx.HTTPTransport()]
        for transport, base in zip(
            transports, ("http://localhost", f"http://127.0.0.1:{port}", f"http://[::1]:{port}")
        ):
            with httpx.Client(transport=transport, base_url=base, timeout=5) as client:
                init = {
                    "jsonrpc": "2.0",
                    "id": 1,
                    "method": "initialize",
                    "params": {
                        "protocolVersion": "2025-03-26",
                        "capabilities": {},
                        "clientInfo": {"name": "fake", "version": "0"},
                    },
                }
                headers = {"Authorization": f"Bearer {token}", "Accept": "application/json, text/event-stream"}
                for _ in range(100):
                    try:
                        response = client.post("/mcp", json=init, headers=headers)
                        break
                    except httpx.ConnectError:
                        if process.poll() is not None:
                            pytest.fail(process.stderr.read().decode())
                        time.sleep(0.05)
                else:
                    pytest.fail(f"MCP listener did not answer on {base}")
                assert response.status_code == 200, response.text
                assert "mcp-session-id" in response.headers
                assert client.post("/mcp", json=init, headers={"Accept": headers["Accept"]}).status_code == 401
                assert (
                    client.post("/mcp", json=init, headers={**headers, "Host": "outside.example.test"}).status_code
                    == 421
                )
                sid = response.headers["mcp-session-id"]
                ready_headers = {**headers, "mcp-session-id": sid}
                client.post(
                    "/mcp", json={"jsonrpc": "2.0", "method": "notifications/initialized"}, headers=ready_headers
                )
                tools = client.post(
                    "/mcp", json={"jsonrpc": "2.0", "id": 2, "method": "tools/list"}, headers=ready_headers
                )
                assert tools.status_code == 200 and "list_chats" in tools.text
        subprocess.run(
            [sys.executable, "-c", "from mcp_config import probe_http_health; probe_http_health()"],
            env=env,
            cwd=Path(__file__).parents[1],
            check=True,
        )
    finally:
        process.terminate()
        process.communicate(timeout=10)
