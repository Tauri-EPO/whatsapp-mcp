"""Failure propagation and cleanup of the disposable network proof."""

from __future__ import annotations

import os
import shutil
import subprocess
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
BASH = shutil.which("bash")
pytestmark = pytest.mark.skipif(BASH is None, reason="needs bash")


@pytest.mark.parametrize("failure", ["", "up", "probe", "ports", "outbox", "operator"])
def test_smoke_propagates_failure_and_cleans_only_its_run(tmp_path: Path, failure: str) -> None:
    scripts = tmp_path / "scripts"
    scripts.mkdir()
    for name in ("smoke-proxy.sh", "proxy-probe.py"):
        shutil.copyfile(ROOT / "scripts" / name, scripts / name)
    # This checkout's env must never be sourced by the disposable proof.
    (tmp_path / ".env").write_text("exit 88\n", encoding="utf-8")
    bindir = tmp_path / "bin"
    bindir.mkdir()
    log = tmp_path / "docker.log"
    docker = bindir / "docker"
    docker.write_text(
        r"""#!/usr/bin/env bash
printf '%s\n' "$*" >> "$PROXY_TEST_LOG"
case "$1" in
  network) exit 0 ;;
  inspect)
    if [ "$PROXY_TEST_FAIL" = ports ]; then echo '{"8000/tcp":[{}]}'; else echo null; fi ;;
  run) cat >/dev/null; [ "$PROXY_TEST_FAIL" != probe ] ;;
  compose)
    case " $* " in
      *" up "*) [ "$PROXY_TEST_FAIL" != up ] ;;
      *" ps -q bridge "*) echo fake-bridge ;;
      *" ps -q mcp "*) echo fake-mcp ;;
      *) exit 0 ;;
    esac ;;
  exec)
    case " $* " in
      *" --operator-status "*)
        if [ "$PROXY_TEST_FAIL" = operator ]; then echo disabled; else echo awaiting_qr; fi ;;
      *" python - "*) cat >/dev/null ;;
      *) [ "$PROXY_TEST_FAIL" != outbox ] ;;
    esac ;;
  *) exit 99 ;;
esac
""",
        encoding="utf-8",
        newline="\n",
    )
    docker.chmod(0o755)
    result = subprocess.run(
        [BASH or "bash", str(scripts / "smoke-proxy.sh")],
        env={
            **os.environ,
            "PATH": str(bindir) + os.pathsep + os.environ["PATH"],
            "PROXY_TEST_LOG": str(log),
            "PROXY_TEST_FAIL": failure,
        },
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert (result.returncode == 0) == (failure == ""), result.stdout + result.stderr
    assert ("proxy smoke -> PASS" in result.stdout) == (failure == "")
    calls = log.read_text(encoding="utf-8")
    assert "down -v --remove-orphans" in calls
    assert "network rm wamcp-proxy-" in calls
    assert not list(tmp_path.glob(".proxy-smoke-env.*"))
    assert "prune" not in calls
    if not failure:
        assert calls.count(" up -d --no-build --wait ") == 4
        assert calls.count("run --rm -i --network") == 2
        assert calls.count("config --quiet") == 5
