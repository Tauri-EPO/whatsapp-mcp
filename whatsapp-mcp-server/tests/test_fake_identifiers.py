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

import ast
import json
import re
import subprocess
import unicodedata
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]

# The lock files hold hashes and URLs with long digit runs, never an example.
OWN_PATH = "whatsapp-mcp-server/tests/test_fake_identifiers.py"
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
    "551199999999",  # the same number without the ninth digit (docs/CONFIGURATION.md)
    "551133333333",
    "551188888888",
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
    "559999999999",  # the same number without the ninth digit (issue #475)
    "15511999999999",
    # Other countries: NANP 555 numbers and counting sequences
    "1234567890",
    "11234567890",
    "12025550100",
    "12025550101",
    "12025551234",
    "13135550002",
    "15550001111",
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
    "111222333444555",  # counting LID already used by the Go mention fixtures
    "100000000000006",
    "100000000000007",
    "100000000000008",
    "100000000000009",  # unmapped LID in the peer job-deadline regression
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
    "3",  # suffix of the deliberately malformed device-suffixed group fixture
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
    "example.ts.net",
    "gpu.tailnet.ts.net",
    "host.tail1234.ts.net",
    "host.tailnet.ts.net",
    "host.ts.net",
    "mcp.example.ts.net",
    "myserver.tail1234.ts.net",
}

# (shape, pattern, allow-list); the first group of the pattern is the value. A user JID
# shorter than eight digits ("111@s.whatsapp.net") cannot be a phone number or a LID,
# so it is a stand-in and needs no entry. A linked-device suffix (`:12@lid`) and the
# `hosted` servers carry the same user part.
SHAPES = (
    ("a Brazilian phone number", re.compile(r"(?<!\d)(55\d{10,11})(?!\d)"), FAKE_NUMBERS),
    (
        "a user JID",
        re.compile(r"(?<!\d)(\d{8,})(?::\d+)?@(?:s\.whatsapp\.net|lid|hosted(?:\.lid)?)(?!\w)", re.I),
        FAKE_NUMBERS,
    ),
    ("a group JID", re.compile(r"(?<!\d)(\d+(?:-\d+)?)@g\.us(?!\w)", re.I), FAKE_GROUPS),
    ("a *.ts.net host", re.compile(r"(?<![\w.-])((?:[A-Za-z0-9-]+\.)+ts\.net)(?![\w-])", re.I), FAKE_HOSTS),
)

# The same Brazilian number as people write it, with spaces, dashes and parentheses.
# Its digits are compared with the allow-list like a compact one.
FORMATTED_PHONE = (
    "a Brazilian phone number",
    re.compile(r"(?<![\w.])\+?55[ .-]*\(?[1-9]\d\)?[ .-]*9?[ .-]*\d{4}[ .-]?\d{4}(?!\d)"),
)


def unlisted(text: str) -> list[tuple[int, str]]:
    """(line number, shape) of every identifier in `text` that is not allow-listed, once per value."""
    found = []
    for number, line in enumerate(text.splitlines(), start=1):
        seen = set()
        for shape, pattern, allowed in SHAPES:
            for match in pattern.finditer(line):
                value = match.group(1).lower()
                if value not in allowed and (shape, value) not in seen:
                    seen.add((shape, value))
                    found.append((number, shape))
        shape, pattern = FORMATTED_PHONE
        for match in pattern.finditer(line):
            digits = re.sub(r"\D", "", match.group(0))
            if digits != match.group(0).lstrip("+") and digits not in FAKE_NUMBERS and (shape, digits) not in seen:
                seen.add((shape, digits))
                found.append((number, shape))
    return found


def describe(name: str, number: int, shape: str) -> str:
    """The line a failure prints: where and what shape, never the value."""
    return f"{name}:{number}: {shape}"


# Common examples rather than a list of anybody's actual contacts. Never add
# a private name here: this list and all test failures are public.
COMMON_FIRST_NAMES = set(
    """
ana bruno carla joao jose maria john jane peter mary sarah james michael
robert david daniel thomas susan linda patricia elizabeth jennifer fernando
carlos marcos paulo pedro julia luiza amanda beatriz lucas diego renata
rafael gabriel gustavo alice bob carol dave eve
""".split()
)
ALLOWED_FAKE_NAMES = {"alice", "bob", "carol", "dave", "eve", "john doe", "jane doe"}


def normalized_name_text(text):
    return "".join(
        c
        for c in unicodedata.normalize("NFKD", text.casefold())
        if not unicodedata.combining(c) and unicodedata.category(c) != "Cf"
    )


def denied_fixture_names(text):
    """Report a shape, never the matched name or original string."""
    normalized = normalized_name_text(text)
    for allowed in sorted(ALLOWED_FAKE_NAMES, key=len, reverse=True):
        normalized = re.sub(r"(?<!\w)" + re.escape(allowed).replace(r"\ ", r"\s+") + r"(?!\w)", " ", normalized)
    return any(token in COMMON_FIRST_NAMES for token in re.findall(r"\w+", normalized))


GO_LITERAL = re.compile(r'//[^\n]*|/\*.*?\*/|"(?:\\.|[^"\\])*"|`[^`]*`', re.S)


def json_fixture_strings(value):
    # JSON escapes must not hide fixture names. Paths/shape reports stay value-free.
    if isinstance(value, str):
        yield 1, value
    elif isinstance(value, dict):
        for key, item in value.items():
            yield from json_fixture_strings(key)
            yield from json_fixture_strings(item)
    elif isinstance(value, list):
        for item in value:
            yield from json_fixture_strings(item)


def fixture_strings(name, text):
    """Yield decoded literal values with source line numbers; ignore comments."""
    if name.endswith("_test.go"):
        for match in GO_LITERAL.finditer(text):
            value = match.group()
            if value.startswith("/"):
                continue
            if value.startswith("`"):
                value = value[1:-1]
            else:
                try:
                    value = ast.literal_eval(value)
                except (SyntaxError, ValueError):
                    # A literal that Python cannot decode remains visible,
                    # rather than silently falling outside the name check.
                    value = value[1:-1]
            yield text.count("\n", 0, match.start()) + 1, value
    elif name.endswith(".py") and "/tests/" in "/" + name:
        tree = ast.parse(text)
        docstrings = set()
        registry_literals = set()
        if name == OWN_PATH:
            for assignment in ast.walk(tree):
                if isinstance(assignment, ast.Assign) and any(
                    isinstance(target, ast.Name) and target.id in {"COMMON_FIRST_NAMES", "ALLOWED_FAKE_NAMES"}
                    for target in assignment.targets
                ):
                    registry_literals.update(
                        id(node) for node in ast.walk(assignment.value) if isinstance(node, ast.Constant)
                    )
        for node in ast.walk(tree):
            if isinstance(node, (ast.Module, ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
                if node.body and isinstance(node.body[0], ast.Expr):
                    first = node.body[0].value
                    if isinstance(first, ast.Constant) and isinstance(first.value, str):
                        docstrings.add(id(first))
        for node in ast.walk(tree):
            if (
                isinstance(node, ast.Constant)
                and isinstance(node.value, str)
                and id(node) not in docstrings
                and id(node) not in registry_literals
            ):
                yield node.lineno, node.value
    elif any(part in {"fixtures", "testdata"} for part in name.split("/")):
        # Serialized fixtures and sample payloads can carry names outside code.
        if name.endswith((".json", ".jsonl", ".yaml", ".yml", ".txt", ".csv", ".xml")):
            if name.endswith(".json"):
                try:
                    yield from json_fixture_strings(json.loads(text))
                except (ValueError, TypeError):
                    for line, value in enumerate(text.splitlines(), 1):
                        yield line, value
            elif name.endswith(".jsonl"):
                for line, value in enumerate(text.splitlines(), 1):
                    try:
                        for _, item in json_fixture_strings(json.loads(value)):
                            yield line, item
                    except (ValueError, TypeError):
                        yield line, value
            else:
                for line, value in enumerate(text.splitlines(), 1):
                    yield line, value


def unlisted_fixture_names(name, text):
    return [
        (line, "a common first name in a fixture")
        for line, value in fixture_strings(name, text)
        if denied_fixture_names(value)
    ]


def tracked_files() -> list[str]:
    if not (ROOT / ".git").exists():
        pytest.skip("not a git checkout (sdist, tarball): nothing tracked to scan")
    out = subprocess.run(["git", "-C", str(ROOT), "ls-files", "-z"], capture_output=True, check=True, timeout=30).stdout
    return [name for name in out.decode("utf-8").split("\0") if name and name not in NOT_SCANNED]


@pytest.fixture(scope="module")
def corpus() -> dict[str, str]:
    """Text of every scanned tracked file, by repository-relative path (binary files left out)."""
    texts = {}
    for name in tracked_files():
        path = ROOT / name
        if not path.is_file():  # deleted in the working copy, or a submodule
            continue
        data = path.read_bytes()
        if b"\0" not in data:
            texts[name] = data.decode("utf-8", "replace")
    return texts


def test_tracked_files_carry_only_fake_identifiers(corpus):
    assert len(corpus) >= 100, list(corpus)  # sanity: git still lists the tree
    problems = []
    for name, text in corpus.items():
        # a path is text too: media lives under store/<chat_jid>/
        problems += [describe(name, number, shape) for number, shape in unlisted(name.replace("\n", " "))]
        problems += [describe(name, number, shape) for number, shape in unlisted(text)]
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
        (f"call +55 (21) 9{DIGITS[:4]}-{DIGITS[4:8]}", "a Brazilian phone number"),
        (f"call 55 21 9{DIGITS[:4]} {DIGITS[4:8]}", "a Brazilian phone number"),
        (f"{DIGITS}@s.whatsapp.net", "a user JID"),
        (f"ends a sentence: {DIGITS}@lid.", "a user JID"),
        (f"user={DIGITS}@lid,", "a user JID"),
        (f"{DIGITS}:12@lid", "a user JID"),
        (f"{DIGITS}@S.WhatsApp.net", "a user JID"),
        (f"{DIGITS}@hosted.lid", "a user JID"),
        (f"{DIGITS}@g.us", "a group JID"),
        (f"{DIGITS[:4]}-{DIGITS}@g.us", "a group JID"),
        ("see " + "tail" + DIGITS[:4] + ".ts.net.", "a *.ts.net host"),
        ("http://" + "my-box.tailnet" + ".ts.net:8000/mcp", "a *.ts.net host"),
        ("HTTP://" + "MY-BOX.TAILNET" + ".TS.NET/x", "a *.ts.net host"),
    ],
)
def test_scanner_flags_an_identifier_that_is_not_allow_listed(line, shape):
    assert unlisted(line) == [(1, shape)]


@pytest.mark.parametrize(
    "line",
    [
        "5511999999999 and 5511999999999@s.whatsapp.net",
        "120363000000000001@g.us, 100000000000001@lid, 100000000000001:3@lid",
        "mcp.example.ts.net:8000",
        "`*.ts.net` and `.ts.net` name no host",
        "a media id 13812002_698058036224062_3424455886509161511_n.enc",
        "ids are digits: 5511999999999999 is longer than a number",
        "someone@example.com",
        "+55 (88) 97777-6666 and 55 88 97777-6666",
        "dated 2026-10-07 55 minutes, 1234 5678",
    ],
)
def test_scanner_leaves_fakes_and_other_shapes_alone(line):
    assert unlisted(line) == []


def test_a_real_identifier_in_a_tracked_path_is_caught():
    assert unlisted(f"whatsapp-bridge/store/{DIGITS}@s.whatsapp.net/img.jpg") == [(1, "a user JID")]


def test_scanner_reports_one_finding_per_value_per_line():
    assert unlisted(f"{DIGITS}@lid and again {DIGITS}@lid") == [(1, "a user JID")]


def test_failure_lines_do_not_carry_the_value():
    line = f"x {DIGITS}@lid"
    ((number, shape),) = unlisted(line)
    assert DIGITS not in describe("some/file", number, shape)
    assert describe("some/file", number, shape) == "some/file:1: a user JID"


def test_allow_lists_have_no_stale_entries(corpus):
    """A fake nobody uses any more is a hole left open for a real value to move into."""
    elsewhere = "\n".join(text for name, text in corpus.items() if name != OWN_PATH)

    def used(entry: str) -> bool:
        # a number may be used spelled with separators ("+55 11 3333-3333")
        spelled = r"[ .()-]*".join(entry) if entry in FAKE_NUMBERS else re.escape(entry)
        return re.search(rf"(?<![\w.-]){spelled}(?!\w)", elsewhere) is not None

    stale = sorted(entry for entry in FAKE_NUMBERS | FAKE_GROUPS | FAKE_HOSTS if not used(entry))
    # the entries are the fakes the repository already publishes, so naming them is fine
    assert not stale, f"allow-listed but not used by any other tracked file: {len(stale)} entries, e.g. {stale[:3]}"


def test_fixtures_use_allowed_fake_names(corpus):
    """Common names are a public heuristic; review still checks arbitrary names."""
    assert len(corpus) >= 100
    problems = [
        describe(name, line, shape)
        for name, source in corpus.items()
        for line, shape in unlisted_fixture_names(name, source)
    ]
    assert not problems, "Fixture names must use the public fake allow-list; values are never printed:\n" + "\n".join(
        problems
    )


def test_name_guard_denies_common_names_and_allows_reviewed_fakes():
    sample = sorted(COMMON_FIRST_NAMES - ALLOWED_FAKE_NAMES)[0]
    assert denied_fixture_names(sample.upper())
    assert denied_fixture_names(sample[0] + "\u200b" + sample[1:])
    assert not denied_fixture_names("prefix" + sample + "suffix")
    assert all(not denied_fixture_names(name) for name in ALLOWED_FAKE_NAMES)
    assert denied_fixture_names("ma" + "r\u00eda")


def test_fixture_literal_parser_covers_escaped_raw_and_multiline_values():
    sample = sorted(COMMON_FIRST_NAMES - ALLOWED_FAKE_NAMES)[0]
    encoded = "".join(f"\\u{ord(char):04x}" for char in sample)
    go = "package main\n// ignored " + sample + '\nvar first="' + encoded + '"\nvar second=`' + sample + "`\n"
    assert len(unlisted_fixture_names("fixture_test.go", go)) == 2
    python = (
        '"""prose ' + sample + '"""\nfirst="' + encoded + '"\nsecond=r"' + sample + '"\nthird="""' + sample + '"""\n'
    )
    assert len(unlisted_fixture_names("tests/fixture.py", python)) == 3
    assert not unlisted_fixture_names("production.py", 'value="' + sample + '"')
    assert unlisted_fixture_names("fixtures/payload.json", '{"name":"' + sample + '"}') == [
        (1, "a common first name in a fixture")
    ]
    assert unlisted_fixture_names("fixtures/escaped.json", '{"name":"' + encoded + '"}')
    assert unlisted_fixture_names("fixtures/escaped.jsonl", '{"name":"' + encoded + '"}')
    report = describe("fixtures/payload.json", 1, "a common first name in a fixture")
    assert sample not in report
