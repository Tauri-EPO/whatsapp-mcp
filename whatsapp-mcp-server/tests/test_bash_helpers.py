"""Native and POSIX shells, with Windows WSL shims excluded."""

import os
import subprocess

import pytest

from tests import bash_helpers


@pytest.mark.parametrize("git_dir", ["cmd", "bin", "mingw64/bin"])
def test_windows_ignores_path_shim(monkeypatch, tmp_path, git_dir):
    root = tmp_path / "Portable Git"
    git = root / git_dir / "git.exe"
    git.parent.mkdir(parents=True)
    git.touch()
    bash = root / "bin" / "bash.exe"
    bash.parent.mkdir(exist_ok=True)
    bash.touch()
    dll = root / "usr" / "bin" / "msys-2.0.dll"
    dll.parent.mkdir(parents=True)
    dll.touch()
    monkeypatch.setattr(bash_helpers.sys, "platform", "win32")
    monkeypatch.setattr(
        bash_helpers.shutil, "which", lambda name: str(git) if name == "git" else "C:/Windows/System32/bash.exe"
    )
    assert bash_helpers.resolve_bash() == str(bash)


def test_windows_without_native_shell(monkeypatch, tmp_path):
    monkeypatch.setattr(bash_helpers.sys, "platform", "win32")
    monkeypatch.setattr(
        bash_helpers.shutil, "which", lambda name: None if name == "git" else "C:/Windows/System32/bash.exe"
    )
    monkeypatch.setenv("ProgramFiles", str(tmp_path))
    assert bash_helpers.resolve_bash() is None
    assert "Git for Windows" in bash_helpers.BASH_SKIP_REASON


def test_standard_install_without_git_on_path(monkeypatch, tmp_path):
    root = tmp_path / "Git"
    (root / "bin").mkdir(parents=True)
    (root / "bin" / "bash.exe").touch()
    (root / "usr" / "bin").mkdir(parents=True)
    (root / "usr" / "bin" / "msys-2.0.dll").touch()
    monkeypatch.setattr(bash_helpers.sys, "platform", "win32")
    monkeypatch.setattr(bash_helpers.shutil, "which", lambda name: None)
    monkeypatch.setenv("ProgramFiles", str(tmp_path))
    assert bash_helpers.resolve_bash() == str(root / "bin" / "bash.exe")


def test_posix_path(monkeypatch):
    monkeypatch.setattr(bash_helpers.sys, "platform", "linux")
    monkeypatch.setattr(bash_helpers.shutil, "which", lambda name: "/usr/bin/bash" if name == "bash" else None)
    assert bash_helpers.resolve_bash() == "/usr/bin/bash"


def test_native_script_path(tmp_path):
    bash = bash_helpers.resolve_bash()
    if bash is None:
        pytest.skip(bash_helpers.BASH_SKIP_REASON)
    script = tmp_path / "native path.sh"
    script.write_text('printf "native shell works"', encoding="utf-8")
    done = subprocess.run([bash, str(script)], capture_output=True, text=True, timeout=20)
    assert done.returncode == 0, done.stderr
    assert done.stdout == "native shell works"


def test_harness_fake_precedes_git_bash_bundled_curl(tmp_path):
    bash = bash_helpers.resolve_bash()
    if bash is None:
        pytest.skip(bash_helpers.BASH_SKIP_REASON)
    bindir = tmp_path / "fake tools"
    bindir.mkdir()
    curl = bindir / "curl"
    curl.write_text('#!/usr/bin/env bash\nprintf "fake curl"\n', encoding="utf-8", newline="\n")
    curl.chmod(0o755)
    script = tmp_path / "probe.sh"
    script.write_text("curl", encoding="utf-8")
    done = subprocess.run(
        bash_helpers.bash_command(bash, script, bin_dir=bindir),
        capture_output=True,
        text=True,
        timeout=20,
        env={**os.environ, "PATH": str(bindir) + os.pathsep + os.environ["PATH"]},
    )
    assert done.returncode == 0, done.stderr
    assert done.stdout == "fake curl"
