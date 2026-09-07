"""export_messages: NDJSON on disk, summary in the response (issue #227)."""

import json
import os
import sqlite3

import pytest

import export
import whatsapp
from chat_policy import load_chat_policy
from errors import ToolError

A = "111@s.whatsapp.net"
B = "222@s.whatsapp.net"
G = "120363000000000009@g.us"

SCHEMA = """
CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP, last_read_time TIMESTAMP);
CREATE TABLE messages (
    id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP, is_from_me BOOLEAN,
    media_type TEXT, filename TEXT, url TEXT, media_key BLOB, file_sha256 BLOB, file_enc_sha256 BLOB,
    file_length INTEGER, deleted_at TIMESTAMP, view_once BOOLEAN NOT NULL DEFAULT 0, target_message_id TEXT,
    quoted_message_id TEXT, PRIMARY KEY (id, chat_jid)
);
"""

# (id, chat, sender, timestamp, is_from_me, media_type, content).
# Timestamps use the bridge's "YYYY-MM-DD HH:MM:SS" spelling so that the
# text comparison against a bound datetime behaves as it does in production.
ROWS = [
    ("a1", A, "111", "2024-01-01 10:00:00+00:00", 0, None, "oi, tudo bem? — açaí"),
    ("a2", A, "me", "2024-01-02 10:00:00+00:00", 1, None, "tudo"),
    ("a3", A, "111", "2024-01-03 10:00:00+00:00", 0, "image", "foto"),
    ("b1", B, "222", "2024-01-04 10:00:00+00:00", 0, None, "outro chat"),
    ("g1", G, "333", "2024-01-05 10:00:00+00:00", 0, None, "grupo"),
]


@pytest.fixture
def store(tmp_path, monkeypatch):
    path = tmp_path / "store" / "messages.db"
    path.parent.mkdir()
    conn = sqlite3.connect(path)
    conn.executescript(SCHEMA)
    conn.executemany(
        "INSERT INTO chats (jid, name, last_message_time, last_read_time) VALUES (?, ?, ?, NULL)",
        [(A, "Alice", None), (B, "Bob", None), (G, "Group", None)],
    )
    conn.executemany(
        "INSERT INTO messages (id, chat_jid, sender, timestamp, is_from_me, media_type, content) "
        "VALUES (?, ?, ?, ?, ?, ?, ?)",
        ROWS,
    )
    conn.commit()
    conn.close()
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    monkeypatch.delenv("WHATSAPP_EXPORT_DIR", raising=False)
    whatsapp._reset_schema_cache()
    whatsapp._reset_name_cache()
    yield path.parent
    whatsapp._reset_schema_cache()
    whatsapp._reset_name_cache()


def lines(path) -> list[dict]:
    with open(path, encoding="utf-8") as handle:
        return [json.loads(line) for line in handle]


def test_default_destination_is_exports_under_the_store(store):
    result = export.export_messages()
    assert os.path.dirname(result["path"]) == os.path.realpath(str(store / "exports"))
    assert os.path.basename(result["path"]).startswith("messages-all-")


def test_writes_ndjson_oldest_first_and_returns_only_a_summary(store):
    result = export.export_messages(out_path="all.ndjson")
    assert set(result) == {"path", "count", "first_timestamp", "last_timestamp", "bytes"}
    assert result["count"] == 5
    assert result["first_timestamp"] == "2024-01-01T10:00:00+00:00"
    assert result["last_timestamp"] == "2024-01-05T10:00:00+00:00"
    assert result["bytes"] == os.path.getsize(result["path"])

    written = lines(result["path"])
    assert [row["id"] for row in written] == ["a1", "a2", "a3", "b1", "g1"]
    assert written[0]["content"] == "oi, tudo bem? — açaí"  # UTF-8, not escaped
    assert written[0]["chat_name"] == "Alice"


def test_filters_apply(store):
    result = export.export_messages(chat_jid=A, from_me=False, out_path="alice-inbound.ndjson")
    assert [row["id"] for row in lines(result["path"])] == ["a1", "a3"]

    result = export.export_messages(exclude_groups=True, after="2024-01-03T12:00:00", out_path="direct.ndjson")
    assert [row["id"] for row in lines(result["path"])] == ["b1"]


def test_fields_subset(store):
    result = export.export_messages(fields=["id", "timestamp"], out_path="thin.ndjson")
    assert all(set(row) == {"id", "timestamp"} for row in lines(result["path"]))


def test_empty_result_still_writes_a_file(store):
    result = export.export_messages(chat_jid=A, after="2030-01-01", out_path="none.ndjson")
    assert (result["count"], result["first_timestamp"], result["bytes"]) == (0, None, 0)
    assert os.path.exists(result["path"])


def test_export_dir_is_configurable(store, tmp_path, monkeypatch):
    elsewhere = tmp_path / "exports-elsewhere"
    monkeypatch.setenv("WHATSAPP_EXPORT_DIR", str(elsewhere))
    result = export.export_messages(out_path="x.ndjson")
    assert result["path"] == str(elsewhere / "x.ndjson")


@pytest.mark.parametrize(
    "out_path",
    [
        "../escape.ndjson",
        "sub/../../escape.ndjson",
        os.path.join(os.path.abspath(os.sep), "tmp", "escape.ndjson"),
    ],
)
def test_path_traversal_is_refused(store, out_path):
    with pytest.raises(whatsapp.ToolError) as exc:
        export.export_messages(out_path=out_path)
    assert exc.value.code == "denied"
    assert not os.path.exists(os.path.join(str(store), "escape.ndjson"))


def test_a_subdirectory_inside_the_export_dir_is_allowed(store):
    result = export.export_messages(out_path="2024/january.ndjson")
    assert result["count"] == 5
    assert os.path.exists(result["path"])


def test_symlink_out_of_the_export_dir_is_refused(store, tmp_path):
    root = export.export_dir()
    os.makedirs(root, exist_ok=True)
    outside = tmp_path / "outside"
    outside.mkdir()
    try:
        os.symlink(str(outside), os.path.join(root, "link"), target_is_directory=True)
    except (OSError, NotImplementedError):
        pytest.skip("symlinks not permitted on this host")
    with pytest.raises(whatsapp.ToolError) as exc:
        export.export_messages(out_path="link/escape.ndjson")
    assert exc.value.code == "denied"


def test_allow_list_filters_rows_and_refuses_a_blocked_chat(store, monkeypatch):
    monkeypatch.setenv("WHATSAPP_ALLOWED_CHATS", A)
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", load_chat_policy())
    result = export.export_messages(out_path="allowed.ndjson")
    assert {row["chat_jid"] for row in lines(result["path"])} == {A}
    with pytest.raises(whatsapp.ToolError) as exc:
        export.export_messages(chat_jid=B, out_path="blocked.ndjson")
    assert exc.value.code == "denied"


def test_unsupported_format_and_unknown_fields_are_rejected(store):
    with pytest.raises(whatsapp.ToolError) as exc:
        export.export_messages(format="csv")
    assert exc.value.code == "invalid_argument"
    with pytest.raises(whatsapp.ToolError) as exc:
        export.export_messages(fields=["id", "nope"])
    assert exc.value.code == "invalid_argument"


class _NoFetchallCursor(sqlite3.Cursor):
    def fetchall(self):
        raise AssertionError("export must stream in batches, never fetchall()")


class _StreamingConnection(sqlite3.Connection):
    def cursor(self, factory=_NoFetchallCursor):  # type: ignore[override]
        return super().cursor(factory)


def test_streams_in_batches_without_fetchall(store, monkeypatch):
    """Memory stays flat on a large export: rows are consumed batch by batch."""
    db = str(store / "messages.db")
    monkeypatch.setattr(whatsapp, "_connect_messages_db", lambda: sqlite3.connect(db, factory=_StreamingConnection))
    monkeypatch.setattr(export, "EXPORT_BATCH", 2)  # 5 rows => 3 batches
    result = export.export_messages(out_path="streamed.ndjson")
    assert result["count"] == 5
    assert len(lines(result["path"])) == 5


def test_export_fields_match_the_real_conversion(store):
    """EXPORT_FIELDS drifting from msg_to_dict would silently break `fields`."""
    conn = sqlite3.connect(str(store / "messages.db"))
    row = conn.execute(
        f"SELECT {whatsapp.MESSAGE_COLUMNS} FROM messages JOIN chats ON messages.chat_jid = chats.jid "
        "WHERE messages.id = 'a3'"
    ).fetchone()
    conn.close()
    message = whatsapp._row_to_message(row)
    message.sha256 = "deadbeef"  # force the notes key, which only media rows carry
    assert set(whatsapp.msg_to_dict(message, notes={})) <= set(export.EXPORT_FIELDS)
    assert set(export.EXPORT_FIELDS) - set(whatsapp.msg_to_dict(message, notes={})) == set()


def test_export_fields_derive_from_the_message_field_list(store):
    """A new msg_to_dict key reaches exports with no second list to hand-edit (#257)."""
    assert list(export.EXPORT_FIELDS) == [f for f in whatsapp.MESSAGE_FIELDS if f not in export.PAGE_ONLY_FIELDS]
    # Every derived name is a valid projection...
    result = export.export_messages(fields=list(export.EXPORT_FIELDS), out_path="all-fields.ndjson")
    assert result["count"] == 5
    # ...and the page-only ones are not, because an export never writes them.
    for name in export.PAGE_ONLY_FIELDS:
        with pytest.raises(ToolError) as exc:
            export.export_messages(fields=[name], out_path="page-only.ndjson")
        assert exc.value.code == "invalid_argument"


def test_tool_forwards_arguments(monkeypatch):
    import main

    seen = {}
    monkeypatch.setattr(main, "export_messages_to_disk", lambda **kw: seen.update(kw) or {})
    main.export_messages(chat_jid=A, sender_jid="111", out_path="x.ndjson", fields=["id"])
    assert seen["chat_jid"] == A
    assert seen["sender_phone_number"] == "111"
    assert seen["out_path"] == "x.ndjson"
    assert seen["fields"] == ["id"]


def test_tool_returns_the_denied_envelope_for_traversal(store):
    import main

    assert main.export_messages(out_path="../escape.ndjson")["error"]["code"] == "denied"
