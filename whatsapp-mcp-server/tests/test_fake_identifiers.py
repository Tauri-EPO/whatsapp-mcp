"""Tracked files may only carry fake WhatsApp identifiers (issue #524).

The repository is public. The privacy audit of 2026-10-07 found real phone
numbers, LIDs, group JIDs and a tailnet host in doc examples, docstrings and
test fixtures: nothing checked what a new example contained, and once merged a
value stays in history for good. Like `test_env_docs.py` for environment
variables, this test walks `git ls-files` and fails on any

- Brazilian number (`55` + 10 or 11 digits),
- user JID (`digits@s.whatsapp.net`, `digits@lid`),
- group JID (`digits@g.us`, `digits-digits@g.us`) or
- `*.ts.net` host

that is not on an explicit allow-list of the fakes the repository uses. A new
fake is added to the list below in the same PR, which is the moment somebody
looks at it. The failure names file, line and shape and never prints the value:
CI logs of a public repository are public too.

`CHANGELOG.md` is generated from the squash titles and is not scanned.
"""

from __future__ import annotations

import re
import subprocess
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]

# The lock files hold hashes and URLs with long digit runs, never an example.
NOT_SCANNED = {"CHANGELOG.md", "whatsapp-bridge/go.sum", "whatsapp-mcp-server/uv.lock"}

# Phone numbers and the user part of `@s.whatsapp.net` / `@lid` JIDs. Patterned
# on purpose (repeated digits, 1234…, 555 numbers), or the made-up ones of the
# phone-lookup tests, or `1000…` for a LID.
FAKE_NUMBERS = {
    # Brazil: repeated digits, and the made-up ninth-digit pair of the phone tests
    "5500000000000",
    "5511000000000",
    "5511111111111",
    "5511222222222",
    "5511333333333",
    "5511444444444",
    "5511555555555",
    "5511666666666",
    "5511777777777",
    "5511888888888",
    "5511999999999",
    "5511999990004",
    "551188887777",
    "5511900000000",
    "5511977776666",
    "5511988887777",
    "5511999990000",
    "5511999990003",
    "5511911111111",
    "5511922222222",
    "5511933333333",
    "5511933334444",
    "551133334444",
    "551077776666",
    "5521777777777",
    "5588877776666",
    "558877776666",
    "5588977776666",
    "558899998888",
    "5588999998888",
    "5599000000000",
    "5599999999999",
    "15511999999999",
    # Other countries: NANP 555 numbers and counting sequences
    "1234567890",
    "11234567890",
    "12025550100",
    "12025550101",
    "12025551234",
    "13135550002",
    "15551234567",
    "15552223333",
    "15557654321",
    "491510000001",
    "6281300000001",
    # short or repeated stand-ins
    "123456789",
    "9876543210",
    "9999999999",
    "10000000000",
    "99887766",
    "9988776655",
    "999888777",
    # LIDs: 1000… for the ones that stand for a real person, counting or
    # repeated digits for the rest
    "100000000000001",
    "100000000000002",
    "100000000000003",
    "100000000000004",
    "100000000000006",
    "100000000000007",
    "100000000000008",
    "10000000000005",
    "1000000000000030",
    "123456789012345",
    "271234567890123",
    "356789012345678",
    "555444333222111",
    "999888777666555",
}

# The user part of `@g.us` JIDs, new-style (`1203630…`) or phone-timestamp.
FAKE_GROUPS = {
    "1",
    "123",
    "222",
    "404",
    "123456",
    "123456789",
    "120363",
    "120363000000000000",
    "120363000000000001",
    "120363000000000002",
    "120363000000000003",
    "120363000000000004",
    "120363000000000009",
    "120363012345678901",
    "120363041234567890",
    "5511-1400000000",
    "5511999990004-1400000000",
}

# Tailnet hosts in examples: placeholders, never a machine of ours.
FAKE_HOSTS = {
    "box.tailnet.ts.net",
    "gpu.tailnet.ts.net",
    "host.tail1234.ts.net",
    "host.tailnet.ts.net",
    "host.ts.net",
    "mcp.example.ts.net",
    "myserver.tail1234.ts.net",
}

# (shape, pattern, allow-list); the first group of the pattern is the value. A user JID
# shorter than eight digits ("111@s.whatsapp.net") cannot be a phone number or a LID,
# so it is a stand-in and needs no entry.
SHAPES = (
    ("a Brazilian phone number", re.compile(r"(?<!\d)(55\d{10,11})(?!\d)"), FAKE_NUMBERS),
    ("a user JID", re.compile(r"(?<!\d)(\d{8,})@(?:s\.whatsapp\.net|lid)(?!\w)"), FAKE_NUMBERS),
    ("a group JID", re.compile(r"(?<!\d)(\d+(?:-\d+)?)@g\.us(?!\w)"), FAKE_GROUPS),
    ("a *.ts.net host", re.compile(r"(?<![\w.-])((?:[A-Za-z0-9-]+\.)+ts\.net)(?![\w-])"), FAKE_HOSTS),
)


def unlisted(text: str) -> list[tuple[int, str]]:
    """(line number, shape) of every identifier in `text` that is not allow-listed."""
    found = []
    for number, line in enumerate(text.splitlines(), start=1):
        for shape, pattern, allowed in SHAPES:
            for match in pattern.finditer(line):
                if match.group(1).lower() not in allowed:
                    found.append((number, shape))
    return found


def tracked_files() -> list[str]:
    try:
        out = subprocess.run(
            ["git", "-C", str(ROOT), "ls-files", "-z"], capture_output=True, check=True, timeout=30
        ).stdout
    except (OSError, subprocess.SubprocessError):
        pytest.skip("not a git checkout: nothing tracked to scan")
    return [name for name in out.decode("utf-8").split("\0") if name and name not in NOT_SCANNED]


def test_tracked_files_carry_only_fake_identifiers():
    files = tracked_files()
    assert len(files) >= 100, files  # sanity: git still lists the tree
    problems = []
    for name in files:
        path = ROOT / name
        if not path.is_file():  # deleted in the working copy, or a submodule
            continue
        data = path.read_bytes()
        if b"\0" in data:
            continue
        problems += [f"{name}:{number}: {shape}" for number, shape in unlisted(data.decode("utf-8", "replace"))]
    assert not problems, (
        "a real identifier may have come in. Use a made-up one, or if the value is a fake, add it to the matching "
        "allow-list in tests/test_fake_identifiers.py (values are not printed, the repository is public):\n"
        + "\n".join(problems)
    )


# The synthetic values below are assembled at run time: a literal would be
# scanned like any other line of this file.
DIGITS = "".join(str(n % 10) for n in range(1, 12))


@pytest.mark.parametrize(
    ("line", "shape"),
    [
        (f"call 55{DIGITS}", "a Brazilian phone number"),
        (f"call 55{DIGITS[:10]} now", "a Brazilian phone number"),
        (f"{DIGITS}@s.whatsapp.net", "a user JID"),
        (f"ends a sentence: {DIGITS}@lid.", "a user JID"),
        (f"user={DIGITS}@lid,", "a user JID"),
        (f"{DIGITS}@g.us", "a group JID"),
        (f"{DIGITS[:4]}-{DIGITS}@g.us", "a group JID"),
        ("see " + "tail" + DIGITS[:4] + ".ts.net.", "a *.ts.net host"),
        ("http://" + "my-box.tailnet" + ".ts.net:8000/mcp", "a *.ts.net host"),
    ],
)
def test_scanner_flags_an_identifier_that_is_not_allow_listed(line, shape):
    assert unlisted(line) == [(1, shape)]


@pytest.mark.parametrize(
    "line",
    [
        "5511999999999 and 5511999999999@s.whatsapp.net",
        "120363000000000001@g.us, 100000000000001@lid",
        "mcp.example.ts.net:8000",
        "`*.ts.net` and `.ts.net` name no host",
        "a media id 13812002_698058036224062_3424455886509161511_n.enc",
        "ids are digits: 5511999999999999 is longer than a number",
        "someone@example.com",
    ],
)
def test_scanner_leaves_fakes_and_other_shapes_alone(line):
    assert unlisted(line) == []


def test_scanner_message_does_not_carry_the_value():
    line = f"x {DIGITS}@lid"
    ((number, shape),) = unlisted(line)
    assert (number, shape) == (1, "a user JID")
    assert DIGITS not in f"f:{number}: {shape}"


def test_allow_lists_have_no_stale_entries():
    """A fake nobody uses any more is a hole left open for a real value to move into."""
    corpus = ""
    for name in tracked_files():
        path = ROOT / name
        if path.is_file():
            data = path.read_bytes()
            if b"\0" not in data:
                corpus += data.decode("utf-8", "replace") + "\n"
    own = (ROOT / "whatsapp-mcp-server" / "tests" / "test_fake_identifiers.py").read_text(encoding="utf-8")
    elsewhere = corpus.replace(own, "")
    stale = sorted(
        entry
        for entry in FAKE_NUMBERS | FAKE_GROUPS | FAKE_HOSTS
        if re.search(rf"(?<![\w.-]){re.escape(entry)}(?!\w)", elsewhere) is None
    )
    assert not stale, f"allow-listed but not used by any other tracked file: {len(stale)} entries, e.g. {stale[:3]}"
