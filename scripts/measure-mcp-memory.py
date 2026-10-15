#!/usr/bin/env python3
"""Measure a local MCP image with fake SQLite data and no external network.

Run: python scripts/measure-mcp-memory.py --image IMAGE --output docs/evidence/NAME.json
Requires Docker Desktop/Linux with cgroup v1 or v2. Never touches an existing store.
The probe clients exit before sampling: their anonymous memory is not counted.
"""

import argparse
import importlib
import json
import sqlite3
import time
import uuid
from pathlib import Path


def seed():
    root = Path("/app/store")
    root.mkdir(exist_ok=True)
    with sqlite3.connect(root / "messages.db") as db:
        db.executescript("""
            CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TEXT);
            CREATE TABLE messages (id TEXT, chat_jid TEXT, sender TEXT, sender_server TEXT,
                content TEXT, timestamp TEXT, is_from_me BOOLEAN, media_type TEXT,
                filename TEXT, url TEXT, media_key BLOB, file_sha256 BLOB,
                file_enc_sha256 BLOB, file_length INTEGER, quoted_message_id TEXT,
                deleted_at TEXT, view_once INTEGER DEFAULT 0, target_message_id TEXT,
                mentions TEXT, PRIMARY KEY(id, chat_jid));
        """)
        jid = "120363000000000001@g.us"
        db.execute("INSERT INTO chats VALUES (?, ?, ?)", (jid, "Synthetic group", "2026-01-01 00:00:00+00:00"))
        db.executemany(
            "INSERT INTO messages (id,chat_jid,sender,sender_server,content,timestamp,is_from_me) "
            "VALUES (?,?,?,?,?,?,?)",
            [
                (
                    f"fake-{i}",
                    jid,
                    "10000000001",
                    "s.whatsapp.net",
                    f"Synthetic message {i}",
                    "2026-01-01 00:00:00+00:00",
                    False,
                )
                for i in range(1000)
            ],
        )


def rpc_batch(count):
    # Import inside the transient client, never in the process sampled below.
    import asyncio

    from mcp import ClientSession
    from mcp.client.streamable_http import streamable_http_client

    async def run():
        async with streamable_http_client("http://127.0.0.1:8000/mcp") as (read, write):
            async with ClientSession(read, write) as client:
                await client.initialize()
                for _ in range(count):
                    result = await client.call_tool("list_chats", {"limit": 20})
                    if result.is_error:
                        raise RuntimeError(f"Tool failed: {result}")
                    if "Synthetic group" not in str(result):
                        raise RuntimeError("Tool did not read the synthetic database")

    asyncio.run(run())


def imports():
    import sys

    sys.path.insert(0, "/app")

    def rss():
        fields = Path("/proc/self/status").read_text().splitlines()
        return int(next(line.split()[1] for line in fields if line.startswith("VmRSS:"))) * 1024

    result = []
    for module in ("mcp.server", "http_oauth", "main", "PIL.Image", "pypdfium2", "openpyxl", "docx"):
        before = rss()
        importlib.import_module(module)
        result.append({"module": module, "rss_bytes": rss(), "delta_bytes": rss() - before})
        if module == "main":
            result.append(
                {
                    "optional_at_startup": [
                        name for name in ("PIL.Image", "pypdfium2", "openpyxl", "docx") if name in sys.modules
                    ]
                }
            )
    result.append(
        {"loaded_optional": [name for name in ("PIL.Image", "pypdfium2", "openpyxl", "docx") if name in sys.modules]}
    )
    import whatsapp

    whatsapp._reset_name_cache()
    cache_before = rss()
    for index in range(10000):
        whatsapp._cache_put("sender", f"fake-{index}", f"Synthetic name {index}")
    cache = {
        "name_cache_inputs": 10000,
        "active_entries": len(whatsapp._name_cache),
        "active_rss_delta_bytes": rss() - cache_before,
    }
    monotonic = whatsapp.time.monotonic
    try:
        whatsapp.time.monotonic = lambda: monotonic() + whatsapp.NAME_CACHE_TTL_S + 1
        whatsapp._cache_put("sender", "fake-final", "Final synthetic name")
        cache["expired_entries_after_put"] = len(whatsapp._name_cache)
    finally:
        whatsapp.time.monotonic = monotonic
    result.append(cache)
    print(json.dumps(result))


def measure(args):
    from docker_local import LocalDocker

    docker = LocalDocker()
    script = str(Path(__file__).resolve())
    name = "wamcp-memory-" + uuid.uuid4().hex[:12]
    cid = None
    try:
        flags = [
            "run",
            "-d",
            "--name",
            name,
            "--network",
            "none",
            "--mount",
            f"type=bind,source={script},target=/measure.py,readonly",
            "-e",
            "WHATSAPP_MCP_HOST=127.0.0.1",
            "-e",
            "WHATSAPP_MCP_TOKEN=off",
            "-e",
            "WHATSAPP_MCP_RATE_LIMIT=off",
            "-e",
            "WHATSAPP_MCP_ALLOWED_HOSTS=localhost",
            "-e",
            "TRANSCRIBE_ON_INGEST=false",
        ]
        if args.arena:
            flags += ["-e", f"MALLOC_ARENA_MAX={args.arena}"]
        cid = docker(*flags, "--entrypoint", "sh", args.image, "-c", "python /measure.py --seed && exec python main.py")
        for _ in range(60):
            probe = docker.run(
                "exec",
                cid,
                "python",
                "-c",
                'import socket;s=socket.create_connection(("127.0.0.1",8000),1);s.close()',
                capture_output=True,
            )
            if probe.returncode == 0:
                break
            time.sleep(1)
        else:
            raise RuntimeError(docker("logs", cid))

        def sample(stage):
            time.sleep(3)
            raw = docker(
                "exec",
                cid,
                "sh",
                "-c",
                "if [ -f /sys/fs/cgroup/memory.current ]; then "
                "echo 2; cat /sys/fs/cgroup/memory.current /sys/fs/cgroup/memory.stat; "
                "else echo 1; cat /sys/fs/cgroup/memory/memory.usage_in_bytes "
                "/sys/fs/cgroup/memory/memory.stat; fi; cat /proc/1/status",
            )
            lines = raw.splitlines()
            version = int(lines[0])
            stat = {line.split()[0]: line.split()[1] for line in lines[2:] if len(line.split()) >= 2}
            return {
                "stage": stage,
                "cgroup_version": version,
                "current_bytes": int(lines[1]),
                "current_source": "memory.current" if version == 2 else "memory.usage_in_bytes",
                "anon_bytes": int(stat["anon" if version == 2 else "total_rss"]),
                "anon_source": "anon" if version == 2 else "total_rss",
                "file_bytes": int(stat["file" if version == 2 else "total_cache"]),
                "threads": int(stat["Threads:"]),
                "rss_bytes": int(stat["VmRSS:"]) * 1024,
            }

        samples = [sample("idle_after_start")]
        docker("exec", cid, "python", "/measure.py", "--calls", "1")
        samples.append(sample("after_first_tool"))
        docker("exec", cid, "python", "/measure.py", "--calls", str(args.batch))
        samples.append(sample("after_fixed_batch"))
        profile = json.loads(
            docker(
                "run",
                "--rm",
                "--network",
                "none",
                "--mount",
                f"type=bind,source={script},target=/measure.py,readonly",
                "--entrypoint",
                "python",
                args.image,
                "/measure.py",
                "--imports",
            )
        )
        result = {
            "image": args.image,
            "image_id": docker("image", "inspect", args.image, "--format", "{{.Id}}"),
            "batch_calls": args.batch,
            "synthetic_messages": 1000,
            "arena_override": args.arena,
            "samples": samples,
            "import_profile": profile,
        }
        output = Path(args.output)
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")
        print(json.dumps(result, indent=2))
    finally:
        if cid:
            docker("rm", "-f", cid)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image")
    parser.add_argument("--output")
    parser.add_argument("--batch", type=int, default=100)
    parser.add_argument("--arena", type=int)
    parser.add_argument("--seed", action="store_true")
    parser.add_argument("--imports", action="store_true")
    parser.add_argument("--calls", type=int)
    args = parser.parse_args()
    if args.seed:
        seed()
    elif args.imports:
        imports()
    elif args.calls:
        rpc_batch(args.calls)
    elif args.image and args.output and args.batch > 0:
        measure(args)
    else:
        parser.error("--image and --output are required, with a positive --batch")
