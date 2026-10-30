"""A reviewed nosec must not silently cover a changed multiline statement."""

import ast
import hashlib
import io
import json
import re
import tokenize
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
INVENTORY = ROOT / ".github" / "bandit-nosec.json"
REVIEW = "Re-review the full SQL statement and update .github/bandit-nosec.json with its fingerprint and reason."


def sites(source: str, path: str) -> list[dict[str, str]]:
    tree = ast.parse(source)
    nodes = list(ast.walk(tree))
    found = []
    for token in tokenize.generate_tokens(io.StringIO(source).readline):
        if token.type != tokenize.COMMENT:
            continue
        directive = re.search(r"\bnosec:?\s*([^#]+)(?:#(.*))?", token.string)
        if not directive or not re.search(r"\bB608\b", directive[1]):
            continue
        line = token.start[0]
        # The smallest statement includes the entire call/assignment,
        # including every concatenated SQL fragment and its bound parameters.
        covering = [
            node
            for node in nodes
            if isinstance(node, ast.stmt)
            and not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef))
            and node.lineno <= line <= node.end_lineno
        ]
        assert covering, f"{path}:{line}: cannot identify the suppressed statement. {REVIEW}"
        statement = min(covering, key=lambda node: (node.end_lineno - node.lineno, node.col_offset * -1))
        functions = sorted(
            (
                node
                for node in nodes
                if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) and node.lineno <= line <= node.end_lineno
            ),
            key=lambda node: node.lineno,
        )
        # unparse drops comments/formatting but retains full literal contents
        # and expressions, so even an interpolation on another line changes it.
        canonical = ast.unparse(statement)
        reason = (directive[2] or "").strip()
        assert reason, f"{path}:{line}: suppression needs a one-line reason"
        found.append(
            {
                "path": path,
                "function": ".".join(node.name for node in functions) or "<module>",
                "fingerprint": hashlib.sha256(canonical.encode()).hexdigest(),
                "reason": reason,
            }
        )
    return found


def production_inventory() -> list[dict[str, str]]:
    return [
        site
        for path in sorted((ROOT / "whatsapp-mcp-server").glob("*.py"))
        for site in sites(path.read_text(encoding="utf-8"), path.relative_to(ROOT).as_posix())
    ]


def test_reviewed_sql_inventory():
    expected = json.loads(INVENTORY.read_text(encoding="utf-8"))
    actual = production_inventory()
    assert actual == expected, REVIEW + "\n" + json.dumps(actual, indent=2)


def test_changed_multiline_sql_cannot_reuse_review():
    source = """def query(value):
    cursor.execute(
        f"SELECT {columns} FROM messages "  # nosec B608 # fixed columns; values bound
        "WHERE id = ?",
        (value,),
    )
"""
    reviewed = sites(source, "example.py")
    for syntax in ("nosec: B608", "nosec B104,B608", "nosec: B104, B608"):
        assert sites(source.replace("nosec B608", syntax), "example.py") == reviewed
    changed = source.replace('"WHERE id = ?"', 'f"WHERE id = {value}"')
    assert sites(changed, "example.py")[0]["fingerprint"] != reviewed[0]["fingerprint"]
    assert sites(source.replace("(value,)", "(other,)"), "example.py") != reviewed
    assert sites(source.replace("def query", "def renamed"), "example.py") != reviewed
    assert sites(source + "\n" + source.replace("query", "other_query"), "example.py") != reviewed
    assert sites(source.replace("# nosec B608 # fixed columns; values bound", ""), "example.py") == []
