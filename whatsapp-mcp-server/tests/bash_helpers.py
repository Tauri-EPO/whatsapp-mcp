"""Native shell selection for script harnesses (never the Windows WSL shim)."""

import os
import shutil
import signal
import subprocess
import sys
from contextlib import suppress
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
        return f"script exceeded its {self.timeout:g}s execution bound"


def _stop_tree(process, job):
    with suppress(OSError, ValueError):
        if job:
            job.close()
        else:
            os.killpg(process.pid, signal.SIGKILL)


def run_bounded(command, *, cwd=None, env=None, timeout, text=True):
    """Capture a script; preserve its execution-bound error through cleanup."""
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
            text=text,
            creationflags=subprocess.CREATE_NO_WINDOW if job else 0,
            start_new_session=job is None,
        )
        if job:
            job.assign(process._handle)
        try:
            stdout, stderr = process.communicate(timeout=timeout)
        except subprocess.TimeoutExpired as error:
            stdout, stderr = error.output, error.stderr
            _stop_tree(process, job)
            try:
                remaining_stdout, remaining_stderr = process.communicate(timeout=10)
                stdout = remaining_stdout or stdout
                stderr = remaining_stderr or stderr
            except subprocess.TimeoutExpired as cleanup_error:
                if cleanup_error.output:
                    stdout = cleanup_error.output
                if cleanup_error.stderr:
                    stderr = cleanup_error.stderr
            except (OSError, ValueError, subprocess.SubprocessError):
                pass
            raise ScriptTimeout(command, timeout, output=stdout, stderr=stderr) from None
        return subprocess.CompletedProcess(command, process.returncode, stdout, stderr)
    finally:
        if process is not None:
            # Also kill a group/job on a normal return after its leader exited.
            _stop_tree(process, job)
            with suppress(OSError, ValueError, subprocess.SubprocessError):
                process.kill()
            with suppress(OSError, ValueError, subprocess.SubprocessError):
                process.wait(timeout=10)
            for pipe in (process.stdout, process.stderr):
                if pipe is not None:
                    with suppress(OSError, ValueError):
                        pipe.close()
        elif job:
            with suppress(OSError, ValueError):
                job.close()
