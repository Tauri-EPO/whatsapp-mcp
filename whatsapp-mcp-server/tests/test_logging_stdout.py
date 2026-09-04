"""stdout must stay clean: on the stdio transport it carries the MCP protocol."""

import ast
import logging
import pathlib

import pytest

import whatsapp
from errors import ToolError

SERVER_DIR = pathlib.Path(__file__).resolve().parent.parent
MODULES = [
    "whatsapp.py",
    "transcribe.py",
    "http_auth.py",
    "chat_policy.py",
    "mcp_config.py",
    "parent_watchdog.py",
    "audio.py",
]


def _print_calls_outside_main(path: pathlib.Path) -> list[int]:
    """Line numbers of print() calls not nested under `if __name__ == "__main__":`."""
    tree = ast.parse(path.read_text(encoding="utf-8"))
    guarded: set[int] = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.If):
            test = node.test
            if (
                isinstance(test, ast.Compare)
                and isinstance(test.left, ast.Name)
                and test.left.id == "__name__"
                and any(isinstance(c, ast.Constant) and c.value == "__main__" for c in test.comparators)
            ):
                for inner in ast.walk(node):
                    guarded.add(id(inner))
    hits = []
    for node in ast.walk(tree):
        if isinstance(node, ast.Call) and isinstance(node.func, ast.Name) and node.func.id == "print":
            if id(node) not in guarded:
                hits.append(node.lineno)
    return hits


@pytest.mark.parametrize("module", MODULES)
def test_no_print_outside_cli_entrypoints(module):
    assert _print_calls_outside_main(SERVER_DIR / module) == [], f"{module} prints to stdout"


def test_main_only_prints_to_stderr():
    source = (SERVER_DIR / "main.py").read_text(encoding="utf-8")
    tree = ast.parse(source)
    for node in ast.walk(tree):
        if isinstance(node, ast.Call) and isinstance(node.func, ast.Name) and node.func.id == "print":
            keywords = {k.arg: ast.unparse(k.value) for k in node.keywords}
            assert keywords.get("file") == "sys.stderr", f"main.py:{node.lineno} print() without file=sys.stderr"


def test_db_error_goes_to_logger_not_stdout(tmp_path, monkeypatch, capsys, caplog):
    # A path inside a directory that does not exist makes sqlite3.connect fail.
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(tmp_path / "missing" / "messages.db"))
    with caplog.at_level(logging.ERROR, logger="whatsapp_mcp"):
        # An unreadable database is an error, never an empty account.
        with pytest.raises(ToolError) as exc:
            whatsapp.list_chats()
        assert exc.value.code == "internal"
    out, _ = capsys.readouterr()
    assert out == ""
    assert any("Database error" in record.getMessage() for record in caplog.records)


def test_download_refusal_goes_to_logger(monkeypatch, capsys, caplog):
    from chat_policy import ChatPolicy

    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["5511999999999"]))
    with caplog.at_level(logging.WARNING, logger="whatsapp_mcp"):
        with pytest.raises(ToolError) as exc:
            whatsapp.download_media("m1", "5511888888888@s.whatsapp.net")
        assert exc.value.code == "denied"
    assert capsys.readouterr().out == ""
