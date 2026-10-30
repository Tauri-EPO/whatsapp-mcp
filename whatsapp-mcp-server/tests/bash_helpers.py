"""Native shell selection for script harnesses (never the Windows WSL shim)."""

import os
import shutil
import subprocess
import sys
from pathlib import Path

BASH_SKIP_REASON = "script tests DID NOT RUN: needs Bash; on Windows install Git for Windows with native Bash"


def resolve_bash(*, platform=sys.platform, which=shutil.which, env=None, run=subprocess.run) -> str | None:
    if platform != "win32":
        return which("bash")
    env = os.environ if env is None else env
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
        bash = root / "bin" / "bash.exe"
        if bash.is_file() and (root / "usr" / "bin" / "msys-2.0.dll").is_file():
            return str(bash)
    return None


def bash_command(bash: str, script: Path, *args: str, bin_dir: Path, inherit_path: bool = True) -> list[str]:
    """Restore the explicit fake directory, or restrict PATH to it for deny tests."""
    if sys.platform != "win32":
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
