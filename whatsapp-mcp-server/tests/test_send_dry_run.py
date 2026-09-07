"""dry_run on send_message / send_file / edit_message: resolved payload, zero HTTP calls."""

from __future__ import annotations

import pytest

import main
import whatsapp
from chat_policy import ChatPolicy

from .conftest import ALICE, FAMILY

OUTSIDE = "5511777777777@s.whatsapp.net"


@pytest.fixture(autouse=True)
def no_bridge(monkeypatch):
    """Any HTTP call at all fails the test: a dry run must not reach the bridge."""

    def explode(*args, **kwargs):
        raise AssertionError("dry_run contacted the bridge")

    for method in ("post", "get", "request"):
        if hasattr(whatsapp.bridge_http, method):
            monkeypatch.setattr(whatsapp.bridge_http, method, explode)


class TestSendMessage:
    def test_preview_shape(self, paired_dbs):
        result = main.send_message(ALICE, "olá", dry_run=True)
        assert result["success"] is True
        assert result["dry_run"] is True
        assert result["endpoint"] == "POST /api/send"
        assert result["payload"] == {"recipient": ALICE, "message": "olá"}
        assert result["recipient_jid"] == ALICE
        assert result["recipient_name"] == "Alice"
        assert "message_id" not in result
        assert "dry_run" in result["message"].lower()

    def test_bare_number_is_resolved_to_a_jid(self, paired_dbs):
        result = main.send_message("5511999999999", "oi", dry_run=True)
        # The payload keeps what the bridge expects; recipient_jid shows the resolution.
        assert result["payload"]["recipient"] == "5511999999999"
        assert result["recipient_jid"] == ALICE
        assert result["recipient_name"] == "Alice"

    def test_quote_and_mentions_are_included(self, paired_dbs):
        result = main.send_message(
            FAMILY,
            "hi @420601234567",
            quoted_message_id="MSG1",
            quoted_sender_jid=ALICE,
            quoted_content="original",
            mentions=["420601234567"],
            dry_run=True,
        )
        assert result["payload"] == {
            "recipient": FAMILY,
            "message": "hi @420601234567",
            "quoted_message_id": "MSG1",
            "quoted_sender_jid": ALICE,
            "quoted_content": "original",
            "mentions": ["420601234567"],
        }

    def test_unknown_chat_previews_without_a_name(self, paired_dbs):
        result = main.send_message(OUTSIDE, "hi", dry_run=True)
        assert result["recipient_jid"] == OUTSIDE
        assert result["recipient_name"] is None

    def test_validation_still_applies(self, paired_dbs):
        assert main.send_message("", "hi", dry_run=True)["error"]["code"] == "invalid_argument"

    def test_chat_allow_list_still_applies(self, paired_dbs, monkeypatch):
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([ALICE]))
        result = main.send_message(FAMILY, "hi", dry_run=True)
        assert result["error"]["code"] == "denied"


class TestSendFile:
    def test_preview_reports_the_file(self, paired_dbs, tmp_path):
        media = tmp_path / "photo.png"
        media.write_bytes(b"0123456789")
        result = main.send_file(ALICE, str(media), caption="look", dry_run=True)
        assert result["dry_run"] is True
        assert result["payload"] == {"recipient": ALICE, "media_path": str(media), "message": "look"}
        assert result["media"]["exists"] is True
        assert result["media"]["bytes"] == 10
        assert result["recipient_name"] == "Alice"

    def test_no_caption_leaves_the_message_field_out(self, paired_dbs, tmp_path):
        media = tmp_path / "doc.pdf"
        media.write_bytes(b"x")
        result = main.send_file(ALICE, str(media), dry_run=True)
        assert result["payload"] == {"recipient": ALICE, "media_path": str(media)}

    def test_missing_file_is_reported_as_in_a_real_send(self, paired_dbs, tmp_path):
        result = main.send_file(ALICE, str(tmp_path / "nope.png"), dry_run=True)
        assert result["error"]["code"] == "not_found"


class TestEditMessage:
    def test_preview_shape(self, paired_dbs):
        result = main.edit_message(ALICE, "MSG1", "corrected", dry_run=True)
        assert result["dry_run"] is True
        assert result["endpoint"] == "POST /api/edit"
        assert result["payload"] == {"chat_jid": ALICE, "message_id": "MSG1", "text": "corrected"}
        assert result["recipient_name"] == "Alice"

    def test_empty_text_still_refused(self, paired_dbs):
        assert main.edit_message(ALICE, "MSG1", "   ", dry_run=True)["error"]["code"] == "invalid_argument"


class TestDefaultsAreUnchanged:
    """dry_run defaults to False: an ordinary call still posts to the bridge."""

    @pytest.mark.parametrize(
        "call",
        [
            lambda: main.send_message(ALICE, "hi"),
            lambda: main.edit_message(ALICE, "MSG1", "hi"),
        ],
    )
    def test_without_dry_run_the_bridge_is_called(self, paired_dbs, call):
        result = call()
        # The autouse fixture makes any HTTP call raise; tool_errors maps it to internal.
        assert result["error"]["code"] == "internal"
