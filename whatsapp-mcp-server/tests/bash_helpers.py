"""Native shell selection for script harnesses (never the Windows WSL shim)."""

import os
import shutil
import sys
from pathlib import Path

BASH_SKIP_REASON = "needs bash (on Windows, install Git for Windows with its native Bash)"


def resolve_bash() -> str | None:
    if sys.platform != "win32":
        return shutil.which("bash")

    # git.exe may live in cmd/, bin/ or mingw64/bin/ in a portable install.
    # Do not fall back to PATH's bash.exe: System32's executable launches WSL,
    # which cannot consume the native paths or share the host's loopback.
    git = shutil.which("git")
    roots = list(Path(git).resolve().parents) if git else []
    roots.append(Path(os.environ.get("ProgramFiles", "C:/Program Files")) / "Git")
    for root in roots:
        bash = root / "bin" / "bash.exe"
        if bash.is_file() and (root / "usr" / "bin" / "msys-2.0.dll").is_file():
            return str(bash)
    return None


def bash_command(bash: str, script: Path, *args: str, bin_dir: Path) -> list[str]:
    """Keep harness fakes ahead of the tools prepended by Git's Bash launcher."""
    if sys.platform != "win32":
        return [bash, str(script), *args]
    return [
        bash,
        "-c",
        'export PATH="$(cygpath -u "$1"):$PATH"; shift; exec "$BASH" "$@"',
        "test-bash",
        str(bin_dir),
        str(script),
        *args,
    ]
