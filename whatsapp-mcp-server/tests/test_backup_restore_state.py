"""Run the restore script with a fake Docker command and real local copies."""

from __future__ import annotations

import os
import shutil
import subprocess
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
BASH = shutil.which("bash")
pytestmark = pytest.mark.skipif(BASH is None or os.name == "nt", reason="needs POSIX bash")


@pytest.mark.parametrize("session", ["replacement", "missing", "copy_failure"])
def test_restore_connection_marker_follows_session_replacement(tmp_path: Path, session: str) -> None:
    store = tmp_path / "store"
    backup = tmp_path / "backup"
    binaries = tmp_path / "bin"
    for directory in (store, backup, binaries):
        directory.mkdir()
    (store / ".connection-problem").write_text("fake revoked device A")
    (store / ".connection-problem.part").write_text("fake pending marker A")
    (store / "whatsapp.db").write_bytes(b"fake old session A")
    if session != "missing":
        (backup / "whatsapp.db").write_bytes(b"fake restored session B")
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
    assert (store / ".connection-problem").exists() is not replaced
    assert (store / ".connection-problem.part").exists() is not replaced
