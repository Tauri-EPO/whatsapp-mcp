"""The AGENTS.md section 3 file tree is the map agents read first; keep it honest.

Every top-level module of both components is supposed to have a line there,
and every line is supposed to name a file that exists. Both drifted silently
until this test, the way environment variables drifted before
`test_env_docs.py`: a PR adds a module, the tree is updated by hand or not at
all, and the next agent looks for a file the map never mentions.
"""

from __future__ import annotations

import re
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]

# Files the tree deliberately leaves out. The two build-tag halves of
# instance_lock.go are covered by its own line ("flock / LockFileEx"); listing
# them separately would say the same thing twice.
UNLISTED = {
    "whatsapp-bridge/instance_lock_unix.go",
    "whatsapp-bridge/instance_lock_windows.go",
}

# ├── name  /  └── name, one indent level per four columns of │ and spaces
ENTRY = re.compile(r"^([│| ]*)(?:├──|└──) (\S+)")


def tree_entries() -> set[str]:
    """Repository-relative paths named in the section 3 code block, directories dropped.

    Every directory node (`whatsapp-bridge/`, `store/`, …) prefixes the lines
    indented under it, so a nested entry keeps its whole path; a directory is
    itself never checked, because `store/` is gitignored.
    """
    text = (ROOT / "AGENTS.md").read_text(encoding="utf-8")
    heading, marker = "## 3. Architecture", "```"
    if heading not in text:
        pytest.fail(f"AGENTS.md has no {heading!r} heading: renumbered? update this test with it")
    section = text.split(heading, 1)[1].split("\n## 4.", 1)[0]
    if section.count(marker) < 2:
        pytest.fail(f"AGENTS.md {heading!r} has no fenced file tree")
    block = section.split(marker, 2)[1]
    paths: set[str] = set()
    parents: dict[int, str] = {}
    for line in block.splitlines():
        match = ENTRY.match(line)
        if not match:
            continue
        indent, name = match.groups()
        depth = round(len(indent) / 4)
        if name.endswith("/"):
            parents[depth] = name.rstrip("/")
            continue
        prefix = "/".join(parents[d] for d in range(depth) if d in parents)
        paths.add(f"{prefix}/{name}" if prefix else name)
    return paths


def source_files(component: str, suffix: str) -> set[str]:
    """Top-level non-test sources of one component, as repository-relative paths."""
    return {
        f"{component}/{path.name}"
        for path in (ROOT / component).glob(f"*{suffix}")
        if not path.name.endswith(f"_test{suffix}") and not path.name.startswith("test_")
    }


def test_every_listed_file_exists():
    listed = tree_entries()
    assert len(listed) >= 50, listed  # sanity: the parser still reads the block
    missing = sorted(path for path in listed if not (ROOT / path).is_file())
    assert not missing, f"listed in AGENTS.md section 3 but not in the repository: {missing}"


def test_every_module_is_listed():
    listed = tree_entries()
    sources = source_files("whatsapp-bridge", ".go") | source_files("whatsapp-mcp-server", ".py")
    undocumented = sorted(sources - listed - UNLISTED)
    assert not undocumented, f"module without a line in AGENTS.md section 3: {undocumented}"


def test_unlisted_allowlist_has_no_stale_entries():
    stale = sorted(path for path in UNLISTED if not (ROOT / path).is_file())
    assert not stale, f"allow-listed as deliberately unlisted but gone: {stale}"
