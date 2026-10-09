"""Network-peer probe for smoke-proxy.sh; initializes MCP, never calls tools."""

import errno
import json
import os
import socket
import time
import urllib.error
import urllib.request


def request(alias, token):
    body = {
        "jsonrpc": "2.0",
        "id": 1,
        "method": "initialize",
        "params": {
            "protocolVersion": "2025-03-26",
            "capabilities": {},
            "clientInfo": {"name": "proxy-smoke", "version": "0"},
        },
    }
    req = urllib.request.Request(
        f"http://{alias}:8000/mcp",
        data=json.dumps(body).encode(),
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
            "Accept": "application/json, text/event-stream",
        },
    )
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    return opener.open(req, timeout=5)


def probe(alias, token, other_token):
    deadline = time.monotonic() + 90
    while True:
        try:
            with request(alias, token) as response:
                assert response.status == 200
                assert response.headers.get("mcp-session-id")
                assert b"serverInfo" in response.read()
            break
        except (OSError, urllib.error.URLError):
            if time.monotonic() >= deadline:
                raise
            time.sleep(1)
    print(f"{alias}:8000 initialize -> 200 + session + serverInfo", flush=True)
    for label, credential in (("missing", ""), ("other instance", other_token)):
        try:
            request(alias, credential).close()
        except urllib.error.HTTPError as exc:
            assert exc.code == 401, exc.code
        else:
            raise AssertionError(f"{alias} accepted {label} token")
        print(f"{alias}:8000 {label} token -> 401", flush=True)
    for port in (8080, 8090, 8091):
        try:
            with socket.create_connection((alias, port), timeout=5):
                pass
        except OSError as exc:
            # DNS errors and timeouts are not proof of connection refusal.
            assert exc.errno == errno.ECONNREFUSED, (alias, port, exc.errno)
        else:
            raise AssertionError(f"{alias}:{port} reachable from proxy network")
        print(f"{alias}:{port} -> ECONNREFUSED", flush=True)
    print(f"{alias}: only MCP 8000 reachable among tested ports", flush=True)


if __name__ == "__main__":
    probe(os.environ["PROBE_ALIAS_A"], os.environ["PROBE_TOKEN_A"], os.environ["PROBE_TOKEN_B"])
    probe(os.environ["PROBE_ALIAS_B"], os.environ["PROBE_TOKEN_B"], os.environ["PROBE_TOKEN_A"])
