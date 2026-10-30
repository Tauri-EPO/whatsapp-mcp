"""Run the restore script with a fake Docker command and real local copies."""

from __future__ import annotations

import json
import os
import sqlite3
import subprocess
import tarfile
from datetime import UTC, datetime, timedelta
from pathlib import Path

import pytest

import media_notes
import transcribe_worker
import whatsapp
from tests.bash_helpers import BASH_SKIP_REASON, resolve_bash

ROOT = Path(__file__).resolve().parents[2]
BASH = resolve_bash()
pytestmark = pytest.mark.skipif(
    BASH is None or os.name == "nt",
    reason=(
        BASH_SKIP_REASON
        if BASH is None
        else "restore tests DID NOT RUN: require POSIX 0600 modes; Windows stat reports 0666 after chmod 0600"
    ),
)


@pytest.mark.parametrize("session", ["replacement", "missing", "copy_failure"])
@pytest.mark.parametrize("kind", ["unlinked", "temporarily_banned", "banned", "locked", "client_outdated", "malformed"])
def test_restore_connection_marker_follows_session_replacement(tmp_path: Path, session: str, kind: str) -> None:
    store = tmp_path / "store"
    backup = tmp_path / "backup"
    binaries = tmp_path / "bin"
    for directory in (store, backup, binaries):
        directory.mkdir()
    original = {
        "kind": kind,
        "code": {"unlinked": 401, "temporarily_banned": 402, "locked": 403, "client_outdated": 405, "banned": 406}[
            kind if kind != "malformed" else "unlinked"
        ],
        "since": "2026-10-12T00:00:00Z",
        "expires_at": "2026-10-13T00:00:00Z",
        "unlinked_at": "2026-10-12T00:00:01Z",
        "revoked_device": "5511999999999@s.whatsapp.net",
        "session_removed": False,
    }
    marker = "not json" if kind == "malformed" else json.dumps(original)
    (store / ".connection-problem").write_text(marker)
    (store / ".connection-problem.part").write_text("fake pending marker A")
    (store / "whatsapp.db").write_bytes(b"fake old session A")
    if session != "missing":
        (backup / "whatsapp.db").write_bytes(b"fake restored session B")
    for database in ("messages", "notes"):
        (store / f"{database}.db").write_bytes(b"fake old archive")
        (backup / f"{database}.db").write_bytes(b"fake restored archive")
    (store / ".bridge-token").write_text("FAKE-OLD-RESTORE-TOKEN")
    (backup / "bridge-token").write_text("FAKE-NEW-RESTORE-TOKEN")
    (store / "fake-media.txt").write_text("fake old media")
    media = tmp_path / "fake-media.txt"
    media.write_text("fake restored media")
    with tarfile.open(backup / "media.tar", "w") as archive:
        archive.add(media, arcname="fake-media.txt")
    # Execute the real restore container body. Only container setup and mount
    # paths are replaced; its rm/cp/chmod operations run against this fixture.
    docker = binaries / "docker"
    docker.write_text(
        "#!/usr/bin/env python3\n"
        "import os, subprocess, sys\n"
        "if sys.argv[1] == 'ps': sys.exit(0)\n"
        "assert sys.argv[1] == 'run', sys.argv\n"
        "body = sys.argv[-1].replace('apk add --no-cache sqlite >/dev/null;', '')\n"
        "body = body.replace('/store', os.environ['FAKE_STORE'])\n"
        "body = body.replace('/backup', os.environ['FAKE_BACKUP'])\n"
        "body = body.replace('chown -R 1000:1000', 'true')\n"
        "if os.environ['FAKE_COPY_FAILURE'] == 'yes': body = 'cp() { return 1; }; ' + body\n"
        "sys.exit(subprocess.call(['sh', '-euc', body]))\n",
        encoding="utf-8",
        newline="\n",
    )
    docker.chmod(0o755)
    # Supply the CLI's real SQLite JSON evaluation in this local fake container.
    sqlite_cli = binaries / "sqlite3"
    sqlite_cli.write_text(
        "#!/usr/bin/env python3\n"
        "import pathlib, sqlite3, sys\n"
        "conn = sqlite3.connect(':memory:')\n"
        "conn.setconfig(sqlite3.SQLITE_DBCONFIG_DQS_DML, False)\n"
        "conn.create_function('readfile', 1, lambda p: pathlib.Path(p).read_bytes())\n"
        "for row in conn.execute(sys.argv[-1]): print(row[0])\n",
        encoding="utf-8",
        newline="\n",
    )
    sqlite_cli.chmod(0o755)
    env = os.environ | {
        "PATH": str(binaries) + os.pathsep + os.environ["PATH"],
        "WHATSAPP_STORE_VOLUME": "fake-store-x15",
        "FAKE_STORE": str(store),
        "FAKE_BACKUP": str(backup),
        "FAKE_COPY_FAILURE": "yes" if session == "copy_failure" else "no",
    }
    assert BASH is not None
    result = subprocess.run(
        [BASH, str(ROOT / "scripts" / "backup.sh"), "restore", str(backup)],
        env=env,
        capture_output=True,
        text=True,
        timeout=15,
    )
    assert result.returncode == (1 if session == "copy_failure" else 0), result.stdout + result.stderr
    replaced = session == "replacement"
    assert (store / "whatsapp.db").read_bytes() == (b"fake restored session B" if replaced else b"fake old session A")
    removed = replaced and kind == "unlinked"
    assert (store / ".connection-problem").exists() is not removed
    assert (store / ".connection-problem.part").exists() is not (replaced and kind != "malformed")
    if kind == "malformed":
        assert (store / ".connection-problem").read_text() == marker
        assert "could not be read; keeping it unchanged" in result.stderr
    copied = session != "copy_failure"
    for database in ("messages", "notes"):
        assert (store / f"{database}.db").read_bytes() == (b"fake restored archive" if copied else b"fake old archive")
        if copied:
            assert (store / f"{database}.db").stat().st_mode & 0o777 == 0o600
    assert (store / ".bridge-token").read_text() == ("FAKE-NEW-RESTORE-TOKEN" if copied else "FAKE-OLD-RESTORE-TOKEN")
    assert (store / "fake-media.txt").read_text() == ("fake restored media" if copied else "fake old media")
    if not removed and kind != "malformed":
        saved = json.loads((store / ".connection-problem").read_text())
        if replaced:
            assert saved == {
                k: v for k, v in original.items() if k not in {"unlinked_at", "revoked_device", "session_removed"}
            }
        else:
            assert saved == original


def _run_backup_command(tmp_path: Path, store: Path, backup: Path, command: str):
    """Run the real container body with local mounts and real SQLite backups."""
    binaries = tmp_path / "bin-reset"
    binaries.mkdir(exist_ok=True)
    docker = binaries / "docker"
    docker.write_text(
        "#!/usr/bin/env python3\n"
        "import os, subprocess, sys\n"
        "if sys.argv[1] == 'ps': sys.exit(0)\n"
        "body = sys.argv[-1].replace('apk add --no-cache sqlite >/dev/null;', '')\n"
        "body = body.replace('/store', os.environ['FAKE_STORE'])\n"
        "body = body.replace('/backup', os.environ['FAKE_BACKUP'])\n"
        "body = body.replace('chown -R 1000:1000', 'true')\n"
        "sys.exit(subprocess.call(['sh', '-euc', body]))\n"
    )
    docker.chmod(0o755)
    sqlite_cli = binaries / "sqlite3"
    sqlite_cli.write_text(
        "#!/usr/bin/env python3\n"
        "import shlex, sqlite3, sys\n"
        "with sqlite3.connect(sys.argv[1]) as conn:\n"
        "    if sys.argv[2].startswith('.backup '):\n"
        "        with sqlite3.connect(shlex.split(sys.argv[2])[1]) as dest: conn.backup(dest)\n"
        "    else:\n"
        "        for row in conn.execute(sys.argv[2]): print(row[0])\n"
    )
    sqlite_cli.chmod(0o755)
    return subprocess.run(
        [BASH or "bash", str(ROOT / "scripts" / "backup.sh"), command, str(backup)],
        env=os.environ
        | {
            "PATH": str(binaries) + os.pathsep + os.environ["PATH"],
            "WHATSAPP_STORE_VOLUME": "fake-reset-store",
            "FAKE_STORE": str(store),
            "FAKE_BACKUP": str(backup),
        },
        capture_output=True,
        text=True,
        timeout=20,
    )


@pytest.mark.parametrize("pending", [False, True])
def test_sent_reset_intent_round_trip_and_next_worker(tmp_path, monkeypatch, pending):
    source, backup, destination = (tmp_path / name for name in ("source", "backup", "destination"))
    for directory in (source, backup, destination):
        directory.mkdir()
    for name in ("messages.db", "whatsapp.db"):
        with sqlite3.connect(source / name) as conn:
            conn.execute("CREATE TABLE sample(value TEXT)")
    monkeypatch.setattr(media_notes, "notes_db_path", lambda: str(source / "notes.db"))
    stale = "2020-01-01 00:00:00+00:00"
    media_notes.claim_meta(transcribe_worker.SENT_SINCE_KEY, stale)
    marker = "notes.db.ingest-sent-reset"
    if pending:
        (source / marker).write_text("fake-pending-reset")
    result = _run_backup_command(tmp_path, source, backup, "backup")
    assert result.returncode == 0, result.stdout + result.stderr
    assert (backup / marker).exists() is pending
    if pending:
        assert (backup / marker).read_text() == "fake-pending-reset"
        assert (backup / marker).stat().st_mode & 0o777 == 0o600
    else:
        (destination / marker).write_text("fake-unrelated-old-reset")
    result = _run_backup_command(tmp_path, destination, backup, "restore")
    assert result.returncode == 0, result.stdout + result.stderr
    assert (destination / marker).exists() is pending
    if pending:
        assert (destination / marker).stat().st_mode & 0o777 == 0o600
    monkeypatch.setattr(media_notes, "notes_db_path", lambda: str(destination / "notes.db"))
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(destination / "messages.db"))
    config = transcribe_worker._settle_sent_since(transcribe_worker.IngestConfig(True, 300, 10), running=True)
    if pending:
        assert abs(datetime.now(UTC) - whatsapp.parse_db_time(config.sent_since)) < timedelta(seconds=5)
        assert not (destination / marker).exists()
    else:
        assert config.sent_since == stale
    # Snapshotting did not edit source notes or acknowledge its pending reset.
    with sqlite3.connect(source / "notes.db") as conn:
        assert conn.execute("SELECT value FROM notes_meta WHERE key='ingest_sent_since'").fetchone()[0] == stale
    assert (source / marker).exists() is pending


@pytest.mark.parametrize("command", ["backup", "restore"])
@pytest.mark.parametrize("location", ["store", "artifact"])
@pytest.mark.parametrize("unsafe", ["symlink", "directory", "oversized", "non_ascii"])
def test_reset_marker_unsafe_paths_are_rejected(tmp_path, command, location, unsafe):
    store, backup = tmp_path / "store", tmp_path / "backup"
    store.mkdir()
    backup.mkdir()
    outside = tmp_path / "outside"
    outside.write_text("fake-private-outside")
    marker = (store if location == "store" else backup) / "notes.db.ingest-sent-reset"
    if unsafe == "symlink":
        marker.symlink_to(outside)
    elif unsafe == "directory":
        marker.mkdir()
    else:
        marker.write_bytes(b"x" * 129 if unsafe == "oversized" else b"\xff")
    result = _run_backup_command(tmp_path, store, backup, command)
    assert result.returncode == 3, result.stdout + result.stderr
    assert "unsafe sent audio reset marker" in result.stderr
    assert outside.read_text() == "fake-private-outside"
