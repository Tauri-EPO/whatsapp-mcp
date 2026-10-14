"""list_messages(query=...) against the bridge's FTS5 index, with substring fallback."""

import re
import sqlite3
from pathlib import Path

import pytest

import main
import whatsapp
from chat_policy import ChatPolicy
from whatsapp import _fts_query_kind, _fts_quote_tokens

SCHEMA = """
CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP);
CREATE TABLE messages (
    id TEXT, chat_jid TEXT, sender TEXT, sender_server TEXT, content TEXT, timestamp TIMESTAMP, is_from_me BOOLEAN,
    media_type TEXT, filename TEXT, url TEXT, media_key BLOB, file_sha256 BLOB, file_enc_sha256 BLOB,
    file_length INTEGER, deleted_at TIMESTAMP, view_once BOOLEAN NOT NULL DEFAULT 0, target_message_id TEXT, quoted_message_id TEXT,
    PRIMARY KEY (id, chat_jid)
);
"""

# Mirrors whatsapp-bridge/fts.go (the bridge owns the index; tests replay it).
FTS_SCHEMA = re.search(
    r"const ftsSchema = `([^`]+)`",
    (Path(__file__).resolve().parents[2] / "whatsapp-bridge" / "fts.go").read_text(encoding="utf-8"),
).group(1)

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
def test_document_filename_search(tmp_path, monkeypatch, with_fts):
    path = _make_db(tmp_path, monkeypatch, with_fts)
    with sqlite3.connect(path) as conn:
        conn.executemany(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename) "
            "VALUES (?, ?, '111', '', '2026-01-01 00:00:00+00:00', 0, ?, ?)",
            [(kind, CHAT, kind, "Relatório técnico.pdf") for kind in ("document", "image", "audio", "video")],
        )
    for query in ("relatorio", "RELATÓRIO", "tecnico"):
        assert ids(query, sort_by="relevance") == ["document"]
        assert whatsapp.count_messages(query=query) == 1
        assert main.list_messages(query=query, count_only=True) == {"count": 1}
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
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["222@s.whatsapp.net"]))
    assert ids("relatorio") == []
    assert main.list_messages(query="relatorio", count_only=True) == {"count": 0}


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
