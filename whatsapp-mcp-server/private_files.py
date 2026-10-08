"""Permissions for files the MCP server owns, independent of the host umask."""

from __future__ import annotations

import json
import logging
import os
import re
import sqlite3
import stat
from pathlib import Path

_EXPORT_NAME = re.compile(r"messages-[\w-]+-[0-9]{8}T[0-9]{6}Z\.ndjson(?:\.part)?\Z")
_UPLOAD_FOLDER = re.compile(r"[0-9]{8}T[0-9]{6}Z-(?:[0-9a-f]{8}|[0-9a-f]{32})\Z")
_CONVERTED_FILE = re.compile(r"tmp[^/\\]+\.ogg\Z")
EXPORT_MANIFEST = ".mcp-export-artifacts"


def record_export(root: str, path: str) -> None:
    """Remember custom export names without claiming ownership of a shared root."""
    relative = os.path.relpath(path, root)
    # One append write per record, safe for concurrent exporters. The names are
    # JSON encoded because out_path can contain newline characters.
    try:
        fd = _open_fd(os.path.join(root, EXPORT_MANIFEST), os.O_CREAT | os.O_WRONLY | os.O_APPEND)
        try:
            os.write(fd, (json.dumps(relative) + "\n").encode())
        finally:
            os.close(fd)
    except OSError:
        # The archive is already complete and private. An unavailable optional
        # ownership record must not turn that successful export into a failure.
        logging.getLogger("whatsapp_mcp").warning("Export ownership was not recorded; migration may skip this artifact")


def private_makedirs(path: str) -> None:
    """Create each missing directory owner-only; leave existing ancestors alone."""
    target = Path(path)
    missing = []
    while not target.exists():
        if target.parent == target:
            raise FileNotFoundError("Artifact directory has no accessible filesystem root")
        missing.append(target)
        target = target.parent
    for folder in reversed(missing):
        try:
            folder.mkdir(mode=0o700)
            if os.name == "posix":
                os.chmod(folder, 0o700)
        except FileExistsError:
            if not folder.is_dir():
                raise


def tighten(path: str | Path, mode: int) -> bool:
    """Tighten a real file/directory, never a symlink, on POSIX."""
    if os.name != "posix":
        return False
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        return False
    if stat.S_ISLNK(info.st_mode) or not (stat.S_ISREG(info.st_mode) or stat.S_ISDIR(info.st_mode)):
        return False
    if stat.S_IMODE(info.st_mode) == mode:
        return False
    os.chmod(path, mode, follow_symlinks=False)
    return True


def _open_fd(path: str, flags: int) -> int:
    if os.path.islink(path):
        raise OSError("An MCP-owned file must not be a symlink")
    fd = os.open(path, flags | getattr(os, "O_NOFOLLOW", 0), 0o600)
    try:
        if not stat.S_ISREG(os.fstat(fd).st_mode):
            raise OSError("An MCP-owned file must be regular")
        if os.name == "posix":
            os.fchmod(fd, 0o600)
        return fd
    except BaseException:
        os.close(fd)
        raise


def private_open(path: str, mode: str, **kwargs):
    """Open a new or replaced artifact owner-only, refusing a final symlink."""
    private_makedirs(os.path.dirname(os.path.abspath(path)))
    flags = os.O_WRONLY | os.O_CREAT | (os.O_EXCL if "x" in mode else os.O_TRUNC)
    fd = _open_fd(path, flags)
    try:
        return os.fdopen(fd, mode, **kwargs)
    except BaseException:
        os.close(fd)
        raise


def notes_connection(path: str, *, create: bool, timeout: float, autocommit: bool = False) -> sqlite3.Connection | None:
    """One private-file factory shared by the two notes stores."""
    if not create and not os.path.exists(path):
        return None
    private_makedirs(os.path.dirname(os.path.abspath(path)))
    for suffix in ("", "-wal", "-shm"):
        if os.path.islink(path + suffix):
            raise OSError("An MCP-owned notes file must not be a symlink")
        tighten(path + suffix, 0o600)
    os.close(_open_fd(path, os.O_CREAT | os.O_RDWR))
    conn = (
        sqlite3.connect(path, timeout=timeout, isolation_level=None)
        if autocommit
        else sqlite3.connect(path, timeout=timeout)
    )
    try:
        conn.execute("PRAGMA journal_mode=WAL")
        for suffix in ("", "-wal", "-shm"):
            tighten(path + suffix, 0o600)
        return conn
    except BaseException:
        conn.close()
        raise


def tighten_existing_artifacts(notes_path: str, export_root: str, upload_root: str) -> None:
    """Tighten existing MCP-owned artifacts once at startup, with one summary."""
    files = sum(tighten(notes_path + suffix, 0o600) for suffix in ("", "-wal", "-shm"))
    directories = 0
    known: set[str] = set()
    manifest = os.path.join(export_root, EXPORT_MANIFEST)
    if not os.path.islink(manifest) and os.path.isfile(manifest):
        with open(manifest, encoding="utf-8") as handle:
            for line in handle:
                try:
                    relative = json.loads(line)
                    if not isinstance(relative, str):
                        continue
                    candidate = os.path.abspath(os.path.join(export_root, relative))
                    if (
                        os.path.commonpath([export_root, candidate]) == export_root
                        and os.path.realpath(candidate) == candidate
                    ):
                        known.update((candidate, candidate + ".part"))
                except (ValueError, OSError):
                    continue
    if os.path.isdir(export_root) and not os.path.islink(export_root):
        owned_directories: set[str] = set()
        for folder, subdirs, filenames in os.walk(export_root, topdown=False, followlinks=False):
            owned = [
                name
                for name in filenames
                if not os.path.islink(os.path.join(folder, name))
                and (
                    _EXPORT_NAME.fullmatch(name)
                    or os.path.join(folder, name) in known
                    or os.path.join(folder, name) == manifest
                )
            ]
            files += sum(tighten(os.path.join(folder, name), 0o600) for name in owned)
            # Shared directories retain their modes: tightening an ancestor
            # would remove access to unrelated files even when their modes stay.
            if (
                (owned or subdirs)
                and len(owned) == len(filenames)
                and all(os.path.join(folder, name) in owned_directories for name in subdirs)
            ):
                directories += tighten(folder, 0o700)
                owned_directories.add(folder)
    # An unsafe uploads root cannot block unrelated read tools at startup.
    if os.path.realpath(upload_root) == os.path.abspath(upload_root) and os.path.isdir(upload_root):
        directories += tighten(upload_root, 0o700)
        for entry in Path(upload_root).iterdir():
            if entry.is_symlink():
                continue
            if entry.is_dir() and _UPLOAD_FOLDER.fullmatch(entry.name):
                for folder, subdirs, filenames in os.walk(entry, followlinks=False):
                    subdirs[:] = [name for name in subdirs if not os.path.islink(os.path.join(folder, name))]
                    directories += tighten(folder, 0o700)
                    files += sum(tighten(os.path.join(folder, name), 0o600) for name in filenames)
            elif entry.is_file() and _CONVERTED_FILE.fullmatch(entry.name):
                files += tighten(entry, 0o600)
    if files or directories:
        logging.getLogger("whatsapp_mcp").info(
            "Tightened MCP-owned permissions: %d files, %d directories", files, directories
        )
