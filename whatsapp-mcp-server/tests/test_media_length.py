"""A declared empty file and an undeclared length stay distinct across readers."""

import pytest

import main
import media_read
from tests.conftest import ALICE


@pytest.mark.parametrize("length", [None, 0, 17])
def test_media_readers_preserve_length_presence(paired_dbs, length):
    sha = bytes.fromhex("aa" * 32)
    with paired_dbs.messages() as conn:
        conn.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, "
            "media_type, filename, file_length, file_sha256) "
            "VALUES ('LENGTH1', ?, 'x', '', '2026-09-04 10:00:00+00:00', 0, 'document', 'empty.txt', ?, ?)",
            (ALICE, length, sha),
        )

    inventory = main.list_media(chat_jid=ALICE)["items"][0]
    assert inventory["bytes"] == length
    assert inventory["resource_link"].get("size") == length
    messages = main.list_messages(chat_jid=ALICE, include_context=False)["items"]
    assert messages[0]["bytes"] == length
    notes = main.get_media_notes(sha.hex())["messages"]
    assert notes[0]["bytes"] == length
    assert media_read._media_row(ALICE, "LENGTH1")[2] == length
