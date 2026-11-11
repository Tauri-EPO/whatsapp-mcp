"""list_messages(query=...) against the bridge's FTS5 index, with substring fallback."""

import re
import sqlite3
from pathlib import Path

import pytest

import main
import whatsapp
from chat_policy import ChatPolicy
from tests.conftest import BOB
from whatsapp import _fts_query_kind, _fts_quote_tokens

SCHEMA = """
CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP);
CREATE TABLE messages (
    id TEXT, chat_jid TEXT, sender TEXT, sender_server TEXT, content TEXT, timestamp TIMESTAMP, is_from_me BOOLEAN,
    media_type TEXT, filename TEXT, url TEXT, media_key BLOB, file_sha256 BLOB, file_enc_sha256 BLOB,
    file_length INTEGER, deleted_at TIMESTAMP, view_once BOOLEAN NOT NULL DEFAULT 0, target_message_id TEXT, quoted_message_id TEXT,
    original_content TEXT,
    PRIMARY KEY (id, chat_jid)
);
"""

# Mirrors whatsapp-bridge/fts.go (the bridge owns the index; tests replay it).
_GO_FTS = (Path(__file__).resolve().parents[2] / "whatsapp-bridge" / "fts.go").read_text(encoding="utf-8")
FTS_SCHEMA = "".join(
    re.search(rf"const {name} = `([^`]+)`", _GO_FTS).group(1) for name in ("ftsViewSchema", "ftsSchema")
)

CHAT = "111@s.whatsapp.net"
ROWS = [
    ("m1", "Segue o orçamento da obra", "2024-01-01T10:00:00"),
    ("m2", "orcamento aprovado, obrigado", "2024-01-02T10:00:00"),
    ("m3", "A Eve chega na never que vem", "2024-01-03T10:00:00"),
    ("m4", "boleto do mês", "2024-01-04T10:00:00"),
    ("m5", "fatura da luz", "2024-01-05T10:00:00"),
    ("m6", "nota fiscal enviada", "2024-01-06T10:00:00"),
    ("m7", "会議の資料を送ります", "2024-01-07T10:00:00"),
    ("m8", "obra (casa) - fase 2", "2024-01-08T10:00:00"),
]


def _make_db(tmp_path, monkeypatch, with_fts: bool):
    path = tmp_path / "messages.db"
    conn = sqlite3.connect(path)
    conn.executescript(SCHEMA)
    if with_fts:
        conn.executescript(FTS_SCHEMA)
    conn.execute("INSERT INTO chats (jid, name) VALUES (?, ?)", (CHAT, "Alice"))
    conn.executemany(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?, ?, '111', ?, ?, 0)",
        [(mid, CHAT, content, ts) for mid, content, ts in ROWS],
    )
    conn.commit()
    conn.close()
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    return path


@pytest.fixture
def fts_db(tmp_path, monkeypatch):
    return _make_db(tmp_path, monkeypatch, with_fts=True)


@pytest.fixture
def plain_db(tmp_path, monkeypatch):
    return _make_db(tmp_path, monkeypatch, with_fts=False)


def ids(query, **kwargs):
    kwargs.setdefault("include_context", False)
    return [m["id"] for m in whatsapp.list_messages(query=query, **kwargs)]


@pytest.mark.parametrize("with_fts", [True, False])
def test_original_text_search_and_attribution(tmp_path, monkeypatch, with_fts):
    path = _make_db(tmp_path, monkeypatch, with_fts)
    with sqlite3.connect(path) as conn:
        conn.execute("UPDATE messages SET content='newword', original_content='oldword' WHERE id='m1'")
    row = main.list_messages(query="oldword", include_context=False)["items"]
    assert len(row) == 1 and row[0]["id"] == "m1"
    assert row[0]["matched_original"] is True
    assert row[0]["content"] == "newword" and row[0]["original_content"] == "oldword"
    assert whatsapp.count_messages(query="oldword") == 1
    assert main.list_messages(query="oldword", count_only=True) == {"count": 1}
    current = main.list_messages(query="newword", include_context=False)["items"]
    assert len(current) == 1 and not current[0].get("matched_original", False)
    projected = main.list_messages(query="oldword", fields=["id"], max_content_chars=3, include_context=False)["items"]
    assert projected == [
        {"id": "m1", "matched_original": True, "content_truncated": True, "original_content_truncated": True}
    ]
    assert whatsapp.message_stats(query="oldword")["total"]["messages"] == 1
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([BOB]))
    assert ids("oldword") == []
    assert whatsapp.count_messages(query="oldword") == 0
    assert main.list_messages(query="oldword", chat_jid=CHAT)["error"]["code"] == "denied"


@pytest.mark.parametrize(
    ("query", "original"),
    [
        ("prepalavra", True),
        ('"old phrase"', True),
        ("old*", True),
        ("oldword AND newword", True),
        ("oldword OR newword", False),
        ("oldword NOT absentword", True),
        ("obra (casa) -", True),
        ("original_content:oldword", True),
        ("content:newword", False),
    ],
)
def test_original_fts_operators_and_attribution(fts_db, query, original):
    with sqlite3.connect(fts_db) as conn:
        conn.execute(
            "UPDATE messages SET content='newword', original_content='oldword prépalavra old phrase obra (casa) -' WHERE id='m1'"
        )
    rows = main.list_messages(query=query, sort_by="relevance", include_context=False)["items"]
    row = next(row for row in rows if row["id"] == "m1")
    assert row.get("matched_original", False) is original
    assert sum(row["id"] == "m1" for row in rows) == 1


@pytest.mark.parametrize("with_fts", [True, False])
def test_original_context_paging_and_composite_key(tmp_path, monkeypatch, with_fts):
    path = _make_db(tmp_path, monkeypatch, with_fts)
    with sqlite3.connect(path) as conn:
        conn.execute("UPDATE messages SET content='newword', original_content='会議 oldword' WHERE id='m1'")
        conn.execute("INSERT INTO chats(jid,name) VALUES (?, 'Bob')", (BOB,))
        conn.execute(
            "INSERT INTO messages(id,chat_jid,sender,content,timestamp,is_from_me,original_content) "
            "VALUES ('m1',?,'5511888888888','oldword','2024-01-02T10:00:00',0,'oldword')",
            (BOB,),
        )
        conn.execute("UPDATE messages SET timestamp=replace(timestamp,'T',' ') || '+00:00'")
    result = main.list_messages(query="oldword", limit=1, include_context=True, context_before=0, context_after=1)
    assert result["items"][0]["chat_jid"] == BOB and not result["items"][0].get("matched_original", False)
    second = main.list_messages(
        query="oldword", cursor=result["next_cursor"], limit=1, include_context=True, context_before=0, context_after=1
    )
    assert second["items"][0]["chat_jid"] == CHAT and second["items"][0]["matched_original"]
    assert "matched_original" not in second["items"][1]  # a surrounding non-hit
    unicode_hits = main.list_messages(query="会議", include_context=False)["items"]
    assert next(row for row in unicode_hits if row["id"] == "m1")["matched_original"]
    assert not next(row for row in unicode_hits if row["id"] == "m7").get("matched_original", False)


def test_original_with_legacy_index_uses_read_only_fallback(tmp_path, monkeypatch):
    path = _make_db(tmp_path, monkeypatch, False)
    with sqlite3.connect(path) as conn:
        conn.execute("UPDATE messages SET content='newword', original_content='oldword' WHERE id='m1'")
        conn.execute("CREATE VIRTUAL TABLE messages_fts USING fts5(content, content='messages', content_rowid='rowid')")
        conn.execute("INSERT INTO messages_fts(messages_fts) VALUES ('rebuild')")
    assert ids("oldword") == ["m1"]
    assert main.list_messages(query="oldword", include_context=False)["items"][0]["matched_original"]
    with sqlite3.connect(path) as conn:
        assert [row[1] for row in conn.execute("PRAGMA table_info(messages_fts)")][:1] == ["content"]
        assert "original_content" not in {row[1] for row in conn.execute("PRAGMA table_info(messages_fts)")}


def test_original_attribution_uses_the_returned_snapshot(fts_db, monkeypatch):
    with sqlite3.connect(fts_db) as conn:
        conn.execute("PRAGMA journal_mode=WAL")
        conn.execute("UPDATE messages SET content='newword', original_content='oldword' WHERE id='m1'")
    real = whatsapp._original_match_keys

    def edit_between_reads(conn, predicate, query, messages):
        with sqlite3.connect(fts_db) as writer:
            writer.execute("UPDATE messages SET content='oldword' WHERE id='m1'")
        return real(conn, predicate, query, messages)

    monkeypatch.setattr(whatsapp, "_original_match_keys", edit_between_reads)
    row = main.list_messages(query="oldword", include_context=False)["items"][0]
    assert row["content"] == "newword" and row["matched_original"]


@pytest.mark.parametrize("with_fts", [True, False])
def test_document_filename_search(tmp_path, monkeypatch, with_fts):
    path = _make_db(tmp_path, monkeypatch, with_fts)
    with sqlite3.connect(path) as conn:
        conn.executemany(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename) "
            "VALUES (?, ?, '111', '', '2026-01-01 00:00:00+00:00', 0, ?, ?)",
            [(kind, CHAT, kind, "Relatório técnico.pdf") for kind in ("document", "image", "audio", "video")],
        )
    queries = ("relatorio", "RELATÓRIO", "tecnico") if with_fts else ("Relatório", "técnico", "pdf")
    with sqlite3.connect(path) as conn:
        conn.execute("UPDATE messages SET original_content='Relatório técnico.pdf' WHERE media_type='document'")
    for query in queries:
        assert ids(query, sort_by="relevance") == ["document"]
        assert whatsapp.count_messages(query=query) == 1
        assert main.list_messages(query=query, count_only=True) == {"count": 1}
        assert not main.list_messages(query=query, include_context=False)["items"][0].get("matched_original", False)
    if with_fts:
        for query in (
            '"relatorio tecnico"',
            "relator*",
            "relatorio AND tecnico",
            "relatorio NOT boleto",
            "missing OR tecnico",
        ):
            assert ids(query) == ["document"]
        assert ids("relatorio NOT tecnico") == []
    else:
        assert ids("relatorio") == []  # same exact Unicode/ASCII LOWER behavior as content
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["222@s.whatsapp.net"]))
    assert ids("relatorio") == []
    assert main.list_messages(query="relatorio", count_only=True) == {"count": 0}


@pytest.mark.parametrize("with_fts", [True, False])
def test_generated_document_names_are_excluded(tmp_path, monkeypatch, with_fts):
    path = _make_db(tmp_path, monkeypatch, with_fts)
    rows = [
        ("generated", "document_20261014_190000_generated"),
        ("special[*]", "document_20261014_190000_special[*]"),
        ("real", "document_final.pdf"),
        ("other", "document_20261014_190000_different.pdf"),
    ]
    with sqlite3.connect(path) as conn:
        conn.executemany(
            "INSERT INTO messages(id, chat_jid, sender, content, timestamp, media_type, filename) "
            "VALUES (?, ?, '111', '', '2026-01-01 00:00:00+00:00', 'document', ?)",
            [(mid, CHAT, name) for mid, name in rows],
        )
    assert set(ids("document")) == {"real", "other"}
    assert ids("final") == ["real"]


def test_fallback_preserves_combining_marks_like_content(plain_db):
    filename = "は\u3099な_ก\u0e49.pdf"
    with sqlite3.connect(plain_db) as conn:
        conn.execute(
            "INSERT INTO messages(id, chat_jid, sender, content, timestamp, media_type, filename) "
            "VALUES ('unicode', ?, '111', '', '2026-01-01 00:00:00+00:00', 'document', ?)",
            (CHAT, filename),
        )
    assert ids("は\u3099な") == ["unicode"]
    assert ids("はな") == []
    assert ids("ก\u0e49") == ["unicode"]


class TestFtsSearch:
    def test_accent_folding_both_directions(self, fts_db):
        assert sorted(ids("orcamento")) == ["m1", "m2"]
        assert sorted(ids("orçamento")) == ["m1", "m2"]

    def test_whole_words_not_substrings(self, fts_db):
        assert ids("eve") == ["m3"]  # not "never"
        assert ids("obra") == ["m8", "m1"]  # newest first; not "obrigado"

    def test_operators(self, fts_db):
        assert sorted(ids("boleto OR fatura")) == ["m4", "m5"]
        assert ids('"nota fiscal"') == ["m6"]
        assert sorted(ids("orcament*")) == ["m1", "m2"]
        assert ids("obra NOT casa") == ["m1"]

    def test_operator_characters_in_plain_text_do_not_raise(self, fts_db):
        # "(" and "-" are FTS5 syntax; the retry quotes tokens so this is a literal search.
        assert ids("obra (casa) -") == ["m8"]

    def test_relevance_sort(self, fts_db):
        # bm25 ranks the shorter document with the same term count first.
        assert ids("orcamento", sort_by="relevance") == ["m2", "m1"]

    def test_unsegmented_scripts_use_substring_scan(self, fts_db):
        assert ids("会議") == ["m7"]

    def test_filters_combine_with_match(self, fts_db):
        assert ids("obra", chat_jid=CHAT, after="2024-01-02T00:00:00") == ["m8"]
        assert ids("obra", chat_jid="nobody@s.whatsapp.net") == []

    def test_paging(self, fts_db):
        assert ids("obra", limit=1, page=0) == ["m8"]
        assert ids("obra", limit=1, page=1) == ["m1"]


class TestSubstringFallback:
    def test_no_index_uses_substring_scan(self, plain_db):
        assert ids("orçamento") == ["m1"]  # exact bytes only
        assert ids("orcamento") == ["m2"]
        assert ids("nev") == ["m3"]  # substring match inside "never"


class TestHelpers:
    @pytest.mark.parametrize(
        ("query", "kind"),
        [("orcamento", "fts"), ("", "substring"), ("   ", "substring"), ("会議", "substring"), ("สวัสดี", "substring")],
    )
    def test_query_kind(self, query, kind):
        assert _fts_query_kind(query) == kind

    def test_quote_tokens(self):
        assert _fts_quote_tokens("obra (casa) -") == '"obra" "(casa)" "-"'
        assert _fts_quote_tokens("orcament*") == '"orcament"*'
        assert _fts_quote_tokens('say "hi"') == '"say" """hi"""'
