"""Native shell selection for script harnesses (never the Windows WSL shim)."""

import os
import shutil
import signal
import subprocess
import sys
import time
from functools import cache
from pathlib import Path

BASH_SKIP_REASON = "script tests DID NOT RUN: needs Bash; on Windows install Git for Windows with native Bash"


def resolve_bash(*, platform=None, which=None, env=None, run=None) -> str | None:
    if platform is None and which is None and env is None and run is None:
        return _default_bash()
    return _discover_bash(
        platform=sys.platform if platform is None else platform,
        which=shutil.which if which is None else which,
        env=os.environ if env is None else env,
        run=subprocess.run if run is None else run,
    )


@cache
def _default_bash() -> str | None:
    return _discover_bash(platform=sys.platform, which=shutil.which, env=os.environ, run=subprocess.run)


def _discover_bash(*, platform, which, env, run) -> str | None:
    if platform != "win32":
        return which("bash")
    roots = []
    git = which("git")
    if git:
        roots.extend(Path(git).resolve().parents)
        # Scoop and other launchers need not live inside the Git installation.
        try:
            done = run([git, "--exec-path"], capture_output=True, text=True, check=True, timeout=10)
            if done.stdout.strip():
                roots.extend(Path(done.stdout.strip()).resolve().parents)
        except (OSError, subprocess.SubprocessError):
            pass
    for variable, suffix in (
        ("ProgramFiles", "Git"),
        ("ProgramFiles(x86)", "Git"),
        ("LOCALAPPDATA", "Programs/Git"),
    ):
        if base := env.get(variable):
            roots.append(Path(base) / suffix)
    # Never fall back to PATH's bash.exe: System32's executable launches WSL.
    for root in roots:
        # Direct usr/bin/bash leaves PATH unchanged: on Windows this host's
        # env-bash fake executables then select the System32 WSL shim (exit 127).
        # The launcher installs native Bash/Git helper paths; bash_command puts
        # the explicitly chosen fake directory ahead of those tools afterwards.
        bash = root / "bin" / "bash.exe"
        if bash.is_file() and (root / "usr" / "bin" / "msys-2.0.dll").is_file():
            return str(bash)
    return None


def bash_command(
    bash: str, script: Path, *args: str, bin_dir: Path, inherit_path: bool = True, platform=sys.platform
) -> list[str]:
    """Restore the explicit fake directory, or restrict PATH to it for deny tests."""
    if platform != "win32":
        return [bash, str(script), *args]
    return [
        bash,
        "-c",
        'export PATH="$(cygpath -u "$1")${2:+:$PATH}"; shift 2; exec "$BASH" "$@"',
        "test-bash",
        str(bin_dir),
        "inherit" if inherit_path else "",
        str(script),
        *args,
    ]


class ScriptTimeout(subprocess.TimeoutExpired):
    def __str__(self):
        return f"script exceeded its {self.timeout:g}s execution bound; terminated the process tree"


def run_bounded(command, *, cwd=None, env=None, timeout):
    """Capture a script without leaving descendants or inherited pipes on timeout."""
    deadline = time.monotonic() + timeout
    job = None
    if sys.platform == "win32":
        from tests._windows_job import WindowsJob

        job = WindowsJob()
    process = None
    try:
        process = subprocess.Popen(
            command,
            cwd=cwd,
            env=env,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            creationflags=0x08000004 if job else 0,  # CREATE_NO_WINDOW | CREATE_SUSPENDED
            start_new_session=job is None,
        )
        if job:
            job.assign_and_resume(process.pid)
        try:
            stdout, stderr = process.communicate(timeout=max(0, deadline - time.monotonic()))
        except subprocess.TimeoutExpired:
            if job:
                job.close()
            else:
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
            stdout, stderr = process.communicate(timeout=10)
            raise ScriptTimeout(command, timeout, output=stdout, stderr=stderr) from None
        return subprocess.CompletedProcess(command, process.returncode, stdout, stderr)
    finally:
        if job:
            job.close()
        if process is not None and process.poll() is None:
            if job is None:
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
            else:
                process.kill()
            process.wait(timeout=10)
