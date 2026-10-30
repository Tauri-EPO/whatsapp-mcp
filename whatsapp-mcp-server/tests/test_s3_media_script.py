"""Linux S3 proof bounds external waits, forwards TERM and cleans up."""

import os
import shutil
import signal
import subprocess
import sys
import time
from pathlib import Path

import pytest

SCRIPT = Path(__file__).resolve().parents[2] / "scripts" / "test-s3-media.sh"
pytestmark = pytest.mark.skipif(sys.platform != "linux", reason="the S3 proof requires Linux and GNU timeout")


@pytest.mark.parametrize("hang", ["none", "download", "startup", "integration", "term", "empty-port"])
def test_s3_proof_bounds_waits_and_cleans_up(tmp_path, hang):
    root = tmp_path / "repo"
    (root / "scripts").mkdir(parents=True)
    script = root / "scripts" / SCRIPT.name
    script.write_bytes(SCRIPT.read_bytes())
    python = root / "whatsapp-mcp-server" / ".venv" / "bin" / "python"
    python.parent.mkdir(parents=True)
    python.touch(mode=0o755)
    (root / "whatsapp-bridge").mkdir()
    commands = tmp_path / "bin"
    commands.mkdir()
    log, ready, container = (tmp_path / name for name in ["calls", "ready", "container"])
    driver = commands / "driver"
    driver.write_text(
        r"""#!/usr/bin/env python3
import os, pathlib, sys, time
name, args = pathlib.Path(sys.argv[0]).name, sys.argv[1:]
with open(os.environ['CALLS'], 'a') as out:
    out.write(name + ' ' + ' '.join(args) + '\n')
container = pathlib.Path(os.environ['CONTAINER_STATE'])
phase = ('download' if name == 'curl' and '--output' in args else
         'startup' if name == 'docker' and args[0] == 'run' else
         'integration' if name == 'go' else '')
if phase == 'startup':
    container.touch()
if name == 'docker' and args[0] == 'inspect':
    sys.exit(0 if container.exists() else 1)
if name == 'docker' and args[0] in ('rm', 'logs') and not container.exists():
    sys.exit('No such container')
if phase == 'integration':
    pathlib.Path(os.environ['PHASE_READY']).write_text(str(os.getpgrp()))
if phase == os.environ['HANG'] or (phase == 'integration' and os.environ['HANG'] == 'term'):
    time.sleep(60)
if name == 'curl' and '--output' in args:
    pathlib.Path(args[args.index('--output') + 1]).touch()
if name == 'docker' and args[0] == 'port' and os.environ['HANG'] != 'empty-port':
    print('127.0.0.1:9000')
if name == 'docker' and args[0] == 'rm':
    container.unlink()
"""
    )
    driver.chmod(0o755)
    for name in ["curl", "docker", "go", "sha256sum"]:
        (commands / name).symlink_to(driver)
    timeout = commands / "timeout"
    real_timeout = shutil.which("timeout")
    assert real_timeout
    timeout.write_text(
        f'#!/bin/bash\nprintf "timeout %s\\n" "$*" >>"$CALLS"\n'
        "shift 2\nlimit=30s\n"
        'case "$HANG:$1:${2:-}" in download:curl:*|startup:docker:run|integration:go:test) limit=0.5s ;; esac\n'
        f'exec "{real_timeout}" --kill-after=0.1s "$limit" "$@"\n'
    )
    timeout.chmod(0o755)
    env = {
        **os.environ,
        "PATH": str(commands) + os.pathsep + os.environ["PATH"],
        "CALLS": str(log),
        "HANG": hang,
        "CONTAINER_STATE": str(container),
        "PHASE_READY": str(ready),
    }
    with subprocess.Popen(
        ["bash", str(script)],
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        start_new_session=True,
    ) as process:
        try:
            if hang == "term":
                deadline = time.monotonic() + 30
                while not ready.exists() and process.poll() is None and time.monotonic() < deadline:
                    time.sleep(0.01)
                assert ready.exists(), "integration did not reach its explicit startup barrier"
                process.terminate()
            stdout, stderr = process.communicate(timeout=15 if hang == "term" else 120)
        finally:
            # Drain this disposable proof even when signal forwarding regresses.
            if ready.exists():
                group = int(ready.read_text())
                assert group != os.getpgrp()
                try:
                    os.killpg(group, signal.SIGKILL)
                except ProcessLookupError:
                    pass
            if process.poll() is None:
                process.kill()
                process.communicate(timeout=5)
    calls = log.read_text()
    assert "Waiting for scratch cleanup" in stderr
    scratch_file = calls.split("--output ", 1)[1].splitlines()[0]
    assert not Path(scratch_file).parent.exists()
    assert not container.exists()
    assert "No such container" not in stderr
    assert "--connect-timeout 10 --max-time 85" in calls
    if hang == "download":
        assert "docker logs" not in calls and "docker rm" not in calls
    else:
        assert "docker rm -f wamcp-minio-proof-" in calls
    if hang == "none":
        assert process.returncode == 0, stdout + stderr
        assert "--connect-timeout 2 --max-time 3" in calls
        assert "timeout --kill-after=5s 420s go test -count=1 -timeout 6m" in calls
    elif hang == "empty-port":
        assert process.returncode == 1, stderr
        assert "MinIO published no port" in stderr
        assert "docker logs --tail 80" in calls
    elif hang == "term":
        assert process.returncode == 143, stderr
        assert "Interrupted waiting for S3 bridge/MCP integration and shutdown (TERM)" in stderr
        assert "docker logs --tail 80" in calls
    else:
        assert process.returncode == 124, stderr
        label = {
            "download": "MinIO binary download",
            "startup": "MinIO container startup",
            "integration": "S3 bridge/MCP integration and shutdown",
        }[hang]
        assert f"Failed waiting for {label}" in stderr
        if hang != "download":
            assert "docker logs --tail 80" in calls
