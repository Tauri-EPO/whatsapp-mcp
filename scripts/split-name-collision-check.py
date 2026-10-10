"""DNS name-collision check: verify MCP talks only to its bridge on the agent network."""

import errno
import hashlib
import json
import os
import socket
import struct
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def stats():
    for _ in range(40):
        try:
            with OPENER.open("http://127.0.0.1:8080/__stats", timeout=1) as response:
                return json.load(response)
        except OSError:
            time.sleep(0.1)
    raise AssertionError("name-collision peer did not start")


def server():
    counts = {"requests": 0, "bearers": 0, "digests": []}

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            if self.path == "/__stats":
                body = counts
            else:
                counts["requests"] += 1
                credential = self.headers.get("Authorization", "")
                if credential:
                    counts["bearers"] += 1
                    digest = hashlib.sha256(credential.encode()).hexdigest()
                    if digest not in counts["digests"] and len(counts["digests"]) < 4:
                        counts["digests"].append(digest)
                print(
                    "name-collision peer received a request; credential value discarded",
                    flush=True,
                )
                body = {"connected": False, "paired": False, "status": "ok"}
            raw = json.dumps(body).encode()
            self.send_response(200)
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def log_message(self, *_):
            pass

    HTTPServer(("0.0.0.0", 8080), Handler).serve_forever()


def add_route(destination, gateway):
    # RTM_NEWROUTE + ACK: prove the test NET_ADMIN route really exists.
    def attribute(kind, value):
        raw = struct.pack("HH", 4 + len(value), kind) + value
        return raw + bytes((-len(raw)) % 4)

    route = struct.pack("BBBBBBBBI", socket.AF_INET, 32, 0, 0, 254, 4, 0, 1, 0)
    route += attribute(1, socket.inet_aton(destination))
    route += attribute(5, socket.inet_aton(gateway))
    message = struct.pack("IHHII", 16 + len(route), 24, 0x605, 1, 0) + route
    with socket.socket(socket.AF_NETLINK, socket.SOCK_RAW, socket.NETLINK_ROUTE) as netlink:
        netlink.bind((0, 0))
        netlink.send(message)
        reply = netlink.recv(4096)
    assert struct.unpack_from("i", reply, 16)[0] == 0, "route injection was refused"
    print(f"NET_ADMIN route {destination}/32 via {gateway} -> installed", flush=True)


def route_probe():
    alias = os.environ["PROBE_OPERATOR_ALIAS"]
    port = os.environ["PROBE_OPERATOR_PORT"]
    request = urllib.request.Request(
        f"http://{alias}:{port}/operator/v1/health",
        headers={"Authorization": "Bearer " + os.environ["WHATSAPP_OPERATOR_TOKEN"]},
    )
    with OPENER.open(request, timeout=5) as response:
        assert response.status == 200
    gateway = socket.gethostbyname(alias)
    for variable, port in (
        ("PROBE_AGENT_BRIDGE_IP", 8080),
        ("PROBE_AGENT_MCP_IP", 8000),
    ):
        address = os.environ[variable]
        add_route(address, gateway)
        try:
            with socket.create_connection((address, port), timeout=3):
                pass
        except OSError as exc:
            assert exc.errno in (
                errno.ECONNREFUSED,
                errno.EHOSTUNREACH,
                errno.ETIMEDOUT,
            ) or isinstance(exc, TimeoutError), exc
            print(
                f"NET_ADMIN {address}:{port} -> blocked ({type(exc).__name__}, errno={exc.errno})",
                flush=True,
            )
        else:
            raise AssertionError(f"NET_ADMIN route reached {address}:{port}")


if __name__ == "__main__":
    role = os.environ.get("PROBE_ROLE", "server")
    if role == "server":
        server()
    elif role == "route":
        route_probe()
    elif role == "return-route":
        # Test fixture only: make return traffic possible so a TCP timeout
        # cannot pass merely because the disposable networks are asymmetric.
        add_route(os.environ["PROBE_ROUTE_TARGET"], os.environ["PROBE_ROUTE_GATEWAY"])
    else:
        observed = stats()
        if role == "check":
            print(
                f"name-collision peer {os.environ['PROBE_NETWORK']}: {json.dumps(observed)}",
                flush=True,
            )
            assert observed == {"requests": 0, "bearers": 0, "digests": []}, "name-collision peer received traffic"

        elif role == "stats":
            print(json.dumps(observed), flush=True)
