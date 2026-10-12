"""Run the restore script with a fake Docker command and real local copies."""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import tarfile
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
BASH = shutil.which("bash")
pytestmark = pytest.mark.skipif(BASH is None or os.name == "nt", reason="needs POSIX bash")


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
        "sys.exit(subprocess.call(['sh', '-euc', body]))\n"
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
        "for row in conn.execute(sys.argv[-1]): print(row[0])\n"
    )
    sqlite_cli.chmod(0o755)
    env = os.environ | {
        "PATH": str(binaries) + os.pathsep + os.environ["PATH"],
        "WHATSAPP_STORE_VOLUME": "fake-store-x15",
        "FAKE_STORE": str(store),
        "FAKE_BACKUP": str(backup),
        "FAKE_COPY_FAILURE": "yes" if session == "copy_failure" else "no",
    }
    result = subprocess.run(
        [BASH or "bash", str(ROOT / "scripts" / "backup.sh"), "restore", str(backup)],
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
