"""The shared IN-list builder emits parameter tokens and leaves values bound."""

import sqlite3

import pytest

import whatsapp


def test_scalar_and_tuple_values_never_enter_sql():
    hostile = "example'); DROP TABLE records; --"
    with sqlite3.connect(":memory:") as conn:
        conn.execute("CREATE TABLE records (a TEXT, b TEXT)")
        conn.executemany("INSERT INTO records VALUES (?, ?)", [(hostile, "example"), ("other", "other")])
        query = whatsapp._in_list_sql("SELECT a FROM records WHERE a IN ({placeholders})", [hostile])
        assert hostile not in query and query.endswith("IN (?)")
        assert conn.execute(query, [hostile]).fetchall() == [(hostile,)]
        pairs = [(hostile, "example"), ("other", "other")]
        query = whatsapp._in_list_sql(
            "SELECT a FROM records WHERE (a, b) IN (VALUES {placeholders})", pairs, row_width=2
        )
        assert query.endswith("VALUES (?, ?),(?, ?))")
        assert conn.execute(query, [value for pair in pairs for value in pair]).fetchall() == [(hostile,), ("other",)]
        assert conn.execute("SELECT count(*) FROM records").fetchone() == (2,)


def test_placeholder_edge_cases():
    assert whatsapp._placeholders([]) == ""
    assert whatsapp._placeholders(["example", "other"]) == "?,?"
    with pytest.raises(ValueError, match="positive"):
        whatsapp._placeholders(["example"], row_width=0)
