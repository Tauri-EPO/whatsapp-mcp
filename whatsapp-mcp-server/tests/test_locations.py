"""Native locations are queryable messages, never files or inferred old text."""

import json

import pytest

import media_inventory
import whatsapp
from tests.conftest import ALICE
from untrusted import clean_untrusted


def test_location_fields_filters_and_unanswered(paired_dbs):
    with paired_dbs.messages() as conn:
        conn.execute("ALTER TABLE messages ADD COLUMN location TEXT")
        conn.executemany(
            "INSERT INTO messages(id,chat_jid,sender,content,timestamp,is_from_me,media_type,location,filename,file_length,file_sha256) "
            "VALUES (?, ?, '5511999999999', ?, '2026-10-08 10:00:00+00:00', 0, ?, ?, 'ignored', 42, X'0102')",
            [
                (
                    "LOCATION",
                    ALICE,
                    "📍 Park — East",
                    "location",
                    json.dumps(
                        {
                            "live": False,
                            "name": "Park — East",
                            "address": "Street — 1",
                            "latitude": 0.25,
                            "longitude": 0.5,
                        }
                    ),
                ),
                ("OLD", ALICE, "📍 Park — East", None, None),
                ("IMAGE", ALICE, "image", "image", None),
            ],
        )
    whatsapp._reset_schema_cache()
    rows = whatsapp.list_messages(chat_jid=ALICE, media_type="location", has_media=False, include_context=False)
    assert [row["id"] for row in rows] == ["LOCATION"]
    assert rows[0]["location"] == {
        "live": False,
        "name": "Park — East",
        "address": "Street — 1",
        "latitude": 0.25,
        "longitude": 0.5,
    }
    assert rows[0]["filename"] is None and rows[0]["bytes"] is None and rows[0]["sha256"] is None
    assert "notes" not in rows[0]
    assert [r["id"] for r in whatsapp.list_messages(chat_jid=ALICE, has_media=True, include_context=False)] == ["IMAGE"]
    old = whatsapp.list_messages(chat_jid=ALICE, query="Park", has_media=False, include_context=False)
    assert {r["id"] for r in old} == {"LOCATION", "OLD"}
    assert next(r for r in old if r["id"] == "OLD")["location"] is None
    with pytest.raises(whatsapp.ToolError, match="no downloadable media"):
        whatsapp.list_messages(media_type="location", has_media=True)
    # A location is speech awaiting an answer even if its comment is a closing word.
    with paired_dbs.messages() as conn:
        conn.execute("UPDATE messages SET content='ok', timestamp='2026-10-08 11:00:00+00:00' WHERE id='LOCATION'")
    pending = whatsapp.list_unanswered(ignore_closing_messages=True)
    assert any(r["jid"] == ALICE for r in pending)


def test_old_schema_and_corrupt_location_are_readable(paired_dbs):
    with paired_dbs.messages() as conn:
        conn.execute(
            "INSERT INTO messages(id,chat_jid,sender,content,timestamp,is_from_me) VALUES ('OLD',?,'5511999999999','📍 text','2026-10-08 10:00:00+00:00',0)",
            (ALICE,),
        )
    assert whatsapp.list_messages(chat_jid=ALICE, include_context=False)[0]["location"] is None
    with paired_dbs.messages() as conn:
        conn.execute("ALTER TABLE messages ADD COLUMN location TEXT")
        conn.execute("UPDATE messages SET media_type='location', location='broken'")
    whatsapp._reset_schema_cache()
    assert whatsapp.list_messages(chat_jid=ALICE, media_type="location", include_context=False)[0]["location"] is None


def test_media_inventory_has_no_location_rows(paired_dbs):
    with paired_dbs.messages() as conn:
        conn.execute(
            "INSERT INTO messages(id,chat_jid,sender,content,timestamp,is_from_me,media_type) VALUES ('LOC',?,'5511999999999','📍 place','2026-10-08 10:00:00+00:00',0,'location')",
            (ALICE,),
        )
    page = media_inventory.list_media_page(chat_jid=ALICE)
    assert page.items == []


def test_location_text_respects_the_untrusted_envelope():
    payload = {
        "location": {
            "name": "Park\u202e",
            "address": "Street — 1",
            "comment": "meet here",
            "url": "https://example.test/place",
            "latitude": 0.25,
        }
    }
    plain = clean_untrusted(payload, wrap=False)["location"]
    wrapped = clean_untrusted(payload, wrap=True)["location"]
    assert plain["name"] == wrapped["name"] == "Park"
    assert plain["address"] == "Street — 1"
    assert wrapped["address"] == "<untrusted>Street — 1</untrusted>"
    assert wrapped["comment"] == "<untrusted>meet here</untrusted>"
    assert wrapped["url"] == "<untrusted>https://example.test/place</untrusted>"
    assert wrapped["latitude"] == plain["latitude"] == 0.25
