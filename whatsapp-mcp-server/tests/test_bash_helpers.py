"""Native and POSIX shells, with Windows WSL shims excluded."""

import os
import shutil
import subprocess
from pathlib import Path

import pytest

from tests import bash_helpers


def _install(root):
    bash = root / "bin" / "bash.exe"
    bash.parent.mkdir(parents=True, exist_ok=True)
    bash.touch()
    dll = root / "usr" / "bin" / "msys-2.0.dll"
    dll.parent.mkdir(parents=True)
    dll.touch()
    return bash


def _no_exec(*args, **kwargs):
    return subprocess.CompletedProcess(args, 0, stdout="")


@pytest.mark.parametrize("git_dir", ["cmd", "bin", "mingw64/bin"])
def test_windows_ignores_path_shim(tmp_path, git_dir):
    root = tmp_path / "Portable Git"
    bash = _install(root)
    git = root / git_dir / "git.exe"
    git.parent.mkdir(parents=True, exist_ok=True)
    git.touch()

    def which(name):
        return str(git) if name == "git" else "C:/Windows/System32/bash.exe"

    assert bash_helpers.resolve_bash(platform="win32", which=which, env={}, run=_no_exec) == str(bash)


def test_windows_without_native_shell():
    def which(name):
        return None if name == "git" else "C:/Windows/System32/bash.exe"

    assert bash_helpers.resolve_bash(platform="win32", which=which, env={}) is None
    assert "DID NOT RUN" in bash_helpers.BASH_SKIP_REASON
    assert "Git for Windows" in bash_helpers.BASH_SKIP_REASON


@pytest.mark.parametrize(
    "variable,suffix", [("ProgramFiles", "Git"), ("ProgramFiles(x86)", "Git"), ("LOCALAPPDATA", "Programs/Git")]
)
def test_install_without_git_on_path(tmp_path, variable, suffix):
    bash = _install(tmp_path / suffix)
    assert bash_helpers.resolve_bash(platform="win32", which=lambda name: None, env={variable: str(tmp_path)}) == str(
        bash
    )


def test_git_exec_path_resolves_launcher_outside_install(tmp_path):
    bash = _install(tmp_path / "actual Git")
    shim = tmp_path / "shims" / "git.exe"
    calls = []

    def run(args, **kwargs):
        calls.append(args)
        return subprocess.CompletedProcess(args, 0, stdout=str(bash.parent.parent / "mingw64/libexec/git-core"))

    assert bash_helpers.resolve_bash(
        platform="win32", which=lambda name: str(shim) if name == "git" else None, env={}, run=run
    ) == str(bash)
    assert calls == [[str(shim), "--exec-path"]]


def test_failed_git_probe_still_finds_standard_install(tmp_path):
    bash = _install(tmp_path / "Git")

    def run(*args, **kwargs):
        raise OSError("unavailable launcher")

    assert bash_helpers.resolve_bash(
        platform="win32", which=lambda name: "missing-git", env={"ProgramFiles": str(tmp_path)}, run=run
    ) == str(bash)


def test_posix_path():
    assert (
        bash_helpers.resolve_bash(platform="linux", which=lambda name: "/usr/bin/bash" if name == "bash" else None)
        == "/usr/bin/bash"
    )


def test_native_script_path(tmp_path):
    bash = bash_helpers.resolve_bash()
    if bash is None:
        pytest.skip(bash_helpers.BASH_SKIP_REASON)
    script = tmp_path / "native path.sh"
    script.write_text('printf "native shell works"', encoding="utf-8", newline="\n")
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
    script.write_text("curl", encoding="utf-8", newline="\n")
    done = subprocess.run(
        bash_helpers.bash_command(bash, script, bin_dir=bindir),
        capture_output=True,
        text=True,
        timeout=20,
        env={**os.environ, "PATH": str(bindir) + os.pathsep + os.environ["PATH"]},
    )
    assert done.returncode == 0, done.stderr
    assert done.stdout == "fake curl"


def test_documented_scripts_are_executable_with_working_shebangs():
    root = Path(__file__).resolve().parents[2]
    git = shutil.which("git")
    assert git is not None
    done = subprocess.run(
        [git, "ls-files", "-s", "--", "scripts/*.sh"], cwd=root, capture_output=True, text=True, check=True, timeout=20
    )
    assert done.stdout
    for entry in done.stdout.splitlines():
        metadata, name = entry.split("\t", 1)
        assert metadata.split()[0] == "100755", name
        assert (root / name).read_bytes().splitlines(keepends=True)[0] in (b"#!/usr/bin/env bash\n", b"#!/bin/sh\n"), (
            name
        )


def test_restricted_path_exposes_no_host_tools(tmp_path):
    bash = bash_helpers.resolve_bash()
    if bash is None:
        pytest.skip(bash_helpers.BASH_SKIP_REASON)
    bindir = tmp_path / "only fake tools"
    bindir.mkdir()
    script = tmp_path / "restricted.sh"
    script.write_text(
        'for tool in gh awk sed grep; do if command -v "$tool" >/dev/null 2>&1; then exit 9; fi; done\nprintf restricted',
        encoding="utf-8",
        newline="\n",
    )
    done = subprocess.run(
        bash_helpers.bash_command(bash, script, bin_dir=bindir, inherit_path=False),
        env={**os.environ, "PATH": str(bindir)},
        capture_output=True,
        text=True,
        timeout=20,
    )
    assert done.returncode == 0, done.stderr
    assert done.stdout == "restricted"
