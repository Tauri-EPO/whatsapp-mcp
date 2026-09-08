import main as mcp_main
from tests.conftest import BOB_LID, BOB_PN


def test_get_contact_normalizes_phone_number(monkeypatch):
    def fake_get_chat(jid: str, include_last_message: bool = True):
        assert include_last_message is False
        return {"jid": jid, "name": "John Doe"}

    monkeypatch.setattr(mcp_main, "whatsapp_get_chat", fake_get_chat)
    monkeypatch.setattr(mcp_main, "whatsapp_get_sender_name", lambda jid: jid)

    result = mcp_main.get_contact(identifier="12025551234")

    assert result["jid"] == "12025551234@s.whatsapp.net"
    assert result["is_lid"] is False
    assert result["phone_number"] == "12025551234"
    assert result["lid"] is None
    assert result["name"] == "John Doe"
    assert result["display_name"] == "John Doe"
    assert result["resolved"] is True


def test_get_contact_normalizes_lid(monkeypatch):
    def fake_get_chat(jid: str, include_last_message: bool = True):
        assert include_last_message is False
        if jid.endswith("@s.whatsapp.net"):
            return None
        return {"jid": jid, "name": "Vicky"}

    monkeypatch.setattr(mcp_main, "whatsapp_get_chat", fake_get_chat)
    monkeypatch.setattr(mcp_main, "whatsapp_get_sender_name", lambda jid: jid)

    result = mcp_main.get_contact(identifier="184125298348272")

    assert result["jid"] == "184125298348272@lid"
    assert result["is_lid"] is True
    assert result["phone_number"] is None
    assert result["lid"] == "184125298348272"
    assert result["name"] == "Vicky"
    assert result["display_name"] == "Vicky"
    assert result["resolved"] is True


def test_get_contact_falls_back_to_lid_for_14_digit_numeric_identifier(monkeypatch):
    calls = []

    def fake_get_chat(jid: str, include_last_message: bool = True):
        assert include_last_message is False
        calls.append(jid)
        if jid.endswith("@lid"):
            return {"jid": jid, "name": "Lidia"}
        return None

    monkeypatch.setattr(mcp_main, "whatsapp_get_chat", fake_get_chat)
    monkeypatch.setattr(mcp_main, "whatsapp_get_sender_name", lambda jid: jid)

    result = mcp_main.get_contact(identifier="35047067385985")

    assert calls == ["35047067385985@s.whatsapp.net", "35047067385985@lid"]
    assert result["jid"] == "35047067385985@lid"
    assert result["is_lid"] is True
    assert result["phone_number"] is None
    assert result["lid"] == "35047067385985"
    assert result["name"] == "Lidia"
    assert result["display_name"] == "Lidia"
    assert result["resolved"] is True


def test_get_contact_unresolved_phone_falls_back_to_jid_user(monkeypatch):
    monkeypatch.setattr(mcp_main, "whatsapp_get_chat", lambda *args, **kwargs: None)
    monkeypatch.setattr(mcp_main, "whatsapp_get_sender_name", lambda jid: jid)

    result = mcp_main.get_contact(identifier="12025551234")

    assert result["jid"] == "12025551234@s.whatsapp.net"
    assert result["resolved"] is False
    assert result["name"] == "12025551234"


def test_get_contact_unresolved_lid_has_no_name(monkeypatch):
    """Echoing the digits back would invent a contact called "184125298348272" (#281)."""
    monkeypatch.setattr(mcp_main, "whatsapp_get_chat", lambda *args, **kwargs: None)
    monkeypatch.setattr(mcp_main, "whatsapp_get_sender_name", lambda jid: jid)

    result = mcp_main.get_contact(identifier="184125298348272@lid")

    assert result["jid"] == "184125298348272@lid"
    assert result["is_lid"] is True
    assert result["resolved"] is False
    assert result["name"] is None
    assert result["display_name"] == "184125298348272@lid"  # still says who, never a name
    assert result["phone_number"] is None
    assert result["lid"] == "184125298348272"


def test_get_contact_keeps_a_device_suffixed_lid_jid_in_the_lid_namespace(monkeypatch):
    monkeypatch.setattr(mcp_main, "whatsapp_get_chat", lambda *args, **kwargs: None)
    monkeypatch.setattr(mcp_main, "whatsapp_get_sender_name", lambda jid: jid)

    result = mcp_main.get_contact(identifier="184125298348272:3@lid")

    assert result["is_lid"] is True
    assert result["lid"] == "184125298348272:3"
    assert result["phone_number"] is None


def test_get_contact_classifies_a_bare_lid_and_resolves_its_phone(paired_dbs, monkeypatch):
    """A bare number the LID map knows is a LID, not a phone number (#281)."""
    monkeypatch.setattr(mcp_main, "whatsapp_get_sender_name", lambda jid: jid)

    def fail_if_asked(jid: str, include_last_message: bool = True):
        # The map already answered: retrying the phone spelling could only
        # flip the classification back (#281).
        assert jid == f"{BOB_LID}@lid", jid
        return None

    monkeypatch.setattr(mcp_main, "whatsapp_get_chat", fail_if_asked)

    result = mcp_main.get_contact(identifier=BOB_LID)

    assert result["jid"] == f"{BOB_LID}@lid"
    assert result["is_lid"] is True
    assert result["lid"] == BOB_LID
    assert result["phone_number"] == BOB_PN  # through whatsmeow_lid_map
    assert result["resolved"] is False  # no name anywhere
    assert result["name"] == BOB_PN  # the number behind the LID, never the LID itself


def test_get_contact_classifies_an_over_long_bare_number_as_a_lid(paired_dbs, monkeypatch):
    monkeypatch.setattr(mcp_main, "whatsapp_get_chat", lambda *args, **kwargs: None)
    monkeypatch.setattr(mcp_main, "whatsapp_get_sender_name", lambda jid: jid)

    result = mcp_main.get_contact(identifier="1171581346817350")  # 16 digits: not E.164

    assert result["jid"] == "1171581346817350@lid"
    assert result["is_lid"] is True
    assert result["phone_number"] is None
    assert result["lid"] == "1171581346817350"
