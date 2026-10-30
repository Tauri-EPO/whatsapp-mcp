"""Linux S3 proof bounds external waits and cleans up failed phases."""

import os
import shutil
import subprocess
import sys
from pathlib import Path

import pytest

SCRIPT = Path(__file__).resolve().parents[2] / "scripts" / "test-s3-media.sh"
pytestmark = pytest.mark.skipif(sys.platform != "linux", reason="the S3 proof requires Linux and GNU timeout")


@pytest.mark.parametrize("hang", ["none", "download", "startup", "integration"])
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
    log = tmp_path / "calls"
    driver = commands / "driver"
    driver.write_text(
        r"""#!/usr/bin/env python3
import os, pathlib, sys, time
name, args = pathlib.Path(sys.argv[0]).name, sys.argv[1:]
with open(os.environ['CALLS'], 'a') as out:
    out.write(name + ' ' + ' '.join(args) + '\n')
phase = ('download' if name == 'curl' and '--output' in args else
         'startup' if name == 'docker' and args[0] == 'run' else
         'integration' if name == 'go' else '')
if phase == os.environ['HANG']:
    time.sleep(30)
if name == 'curl' and '--output' in args:
    pathlib.Path(args[args.index('--output') + 1]).touch()
if name == 'docker' and args[0] == 'port':
    print('127.0.0.1:9000')
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
    result = subprocess.run(
        ["bash", str(script)],
        env={**os.environ, "PATH": str(commands) + os.pathsep + os.environ["PATH"], "CALLS": str(log), "HANG": hang},
        capture_output=True,
        text=True,
        timeout=120,
    )
    calls = log.read_text()
    assert "docker rm -f wamcp-minio-proof-" in calls
    assert "Waiting for scratch cleanup" in result.stderr
    scratch_file = calls.split("--output ", 1)[1].splitlines()[0]
    assert not Path(scratch_file).parent.exists()
    assert "--connect-timeout 10 --max-time 85" in calls
    if hang == "none":
        assert result.returncode == 0, result.stderr
        assert "--connect-timeout 2 --max-time 3" in calls
        assert "timeout --kill-after=5s 420s go test" in calls
    else:
        assert result.returncode == 124, result.stderr
        label = {
            "download": "MinIO binary download",
            "startup": "MinIO container startup",
            "integration": "S3 bridge/MCP integration and shutdown",
        }[hang]
        assert f"Failed waiting for {label}" in result.stderr
        assert "docker logs --tail 80" in calls
