"""Execute the shipped image probe; a proxy response cannot hide a dead MCP."""

import json
import os
import socket
import subprocess
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest


@pytest.mark.parametrize("backend_alive", [False, True])
def test_image_healthcheck_uses_loopback_even_with_environment_proxy(backend_alive):
    proxy_calls = []

    def receiver(calls, status):
        class Receiver(BaseHTTPRequestHandler):
            def do_GET(self):
                calls.append(self.path)
                self.send_response(status)
                self.end_headers()

            def log_message(self, *_args):
                pass

        return Receiver

    proxy = ThreadingHTTPServer(("127.0.0.1", 0), receiver(proxy_calls, 418))
    backend_calls = []
    backend = ThreadingHTTPServer(("127.0.0.1", 0), receiver(backend_calls, 405)) if backend_alive else None
    dead_socket = socket.socket()
    dead_socket.bind(("127.0.0.1", 0))  # Occupy the port without a listening backend.
    port = backend.server_port if backend else dead_socket.getsockname()[1]
    servers = [proxy] + ([backend] if backend else [])
    threads = [threading.Thread(target=server.serve_forever, daemon=True) for server in servers]
    for thread in threads:
        thread.start()
    dockerfile = Path(__file__).resolve().parents[1] / "Dockerfile"
    probe = next(
        line.strip()[4:]
        for line in dockerfile.read_text().splitlines()
        if line.strip().startswith('CMD ["python", "-c",')
    )
    command = json.loads(probe)
    command[0] = sys.executable
    env = dict(os.environ, WHATSAPP_MCP_PORT=str(port), NO_PROXY="", no_proxy="")
    for name in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"):
        env[name] = f"http://127.0.0.1:{proxy.server_port}"
    try:
        result = subprocess.run(command, env=env, capture_output=True, timeout=10)
        assert (result.returncode == 0) is backend_alive
        assert not proxy_calls, "image healthcheck reached the environment proxy"
        assert backend_calls == (["/mcp"] if backend_alive else [])
    finally:
        dead_socket.close()
        for server in servers:
            server.shutdown()
            server.server_close()
        for thread in threads:
            thread.join(timeout=5)
