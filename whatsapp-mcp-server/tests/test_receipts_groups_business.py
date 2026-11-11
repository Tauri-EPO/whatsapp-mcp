"""Real SQLite and SDK/HTTP boundaries for receipts, group creation and business reads."""

import json

import pytest

import main
import tool_policy
import whatsapp
from chat_policy import ChatPolicy
from errors import ToolError
from tests.test_group_manage import group_http_bridge as receipt_http_bridge
from tests.test_group_manage import group_sdk_client
from tool_policy import ToolPolicy

pytestmark = pytest.mark.usefixtures("auth_runtime_store")
group_http_bridge = receipt_http_bridge
GROUP = "120363000000000001@g.us"
PHONE = "5511999999999@s.whatsapp.net"


def test_receipts_real_sqlite_ownership_latest_and_namespaces(paired_dbs, monkeypatch):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.unrestricted())
    with paired_dbs.messages() as conn:
        conn.execute(
            "CREATE TABLE message_receipts(chat_jid TEXT,message_id TEXT,recipient TEXT,recipient_server TEXT,type TEXT,at TEXT)"
        )
        for chat, own in ((GROUP, True), (PHONE, False)):
            conn.execute("INSERT INTO messages(id,chat_jid,is_from_me) VALUES('own',?,?)", (chat, own))
        for chat, user, server, kind, at in (
            (GROUP, "5511999999999", "s.whatsapp.net", "delivered", "2026-11-10 01:00:00+00:00"),
            (GROUP, "5511999999999", "s.whatsapp.net", "read", "2026-11-10 01:01:00+00:00"),
            (GROUP, "100000000000001", "lid", "played", "2026-11-10 01:02:00+00:00"),
            (PHONE, "5511999999999", "s.whatsapp.net", "read", "2026-11-10 01:03:00+00:00"),
        ):
            conn.execute("INSERT INTO message_receipts VALUES(?, 'own', ?, ?, ?, ?)", (chat, user, server, kind, at))
    result = main.get_message_receipts(GROUP, "own")
    assert len(result["recipients"]) == 2
    assert {r["jid"]: r["status"] for r in result["recipients"]} == {PHONE: "read", "100000000000001@lid": "played"}
    with paired_dbs.whatsmeow() as conn:
        conn.execute("INSERT INTO whatsmeow_lid_map VALUES ('100000000000001', '5511999999999')")
    combined = main.get_message_receipts(GROUP, "own")["recipients"]
    assert len(combined) == 1 and combined[0]["jid"] == PHONE and combined[0]["status"] == "played"
    assert main.get_message_receipts(PHONE, "own")["recipients"] == []
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([PHONE]))
    assert main.get_message_receipts(GROUP, "own")["error"]["code"] == "denied"
    assert main.get_message_receipts(PHONE, "own")["recipients"] == []


def test_old_receipt_store_empty(paired_dbs, monkeypatch):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.unrestricted())
    assert main.get_message_receipts(GROUP, "missing")["recipients"] == []


def test_receipt_lids_use_one_map_connection_and_query(paired_dbs, monkeypatch):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.unrestricted())
    with paired_dbs.messages() as conn:
        conn.execute(
            "CREATE TABLE message_receipts(chat_jid TEXT,message_id TEXT,recipient TEXT,recipient_server TEXT,type TEXT,at TEXT)"
        )
        conn.execute("INSERT INTO messages(id,chat_jid,is_from_me) VALUES('own',?,1)", (GROUP,))
        for user in ("100000000000001", "100000000000002"):
            for kind in ("delivered", "read", "played"):
                conn.execute(
                    "INSERT INTO message_receipts VALUES(?,'own',?,'lid',?,'2026-11-10 01:00:00+00:00')",
                    (GROUP, user, kind),
                )
    with paired_dbs.whatsmeow() as conn:
        conn.executemany(
            "INSERT INTO whatsmeow_lid_map VALUES (?,?)",
            [("100000000000001", "5511999999999"), ("100000000000002", "5511888888888")],
        )
    original = whatsapp._connect_whatsmeow_db
    connections, queries = [], []

    def connect():
        conn = original()
        connections.append(conn)
        conn.set_trace_callback(queries.append)
        return conn

    monkeypatch.setattr(whatsapp, "_connect_whatsmeow_db", connect)
    recipients = main.get_message_receipts(GROUP, "own")["recipients"]
    assert {row["jid"] for row in recipients} == {PHONE, "5511888888888@s.whatsapp.net"}
    assert all(row["status"] == "played" for row in recipients)
    assert len(connections) == 1
    assert len([q for q in queries if "FROM whatsmeow_lid_map" in q]) == 1


def test_create_group_reads_one_policy_snapshot(monkeypatch):
    class ChangingPolicy:
        calls = 0

        def effective(self):
            self.calls += 1
            return ChatPolicy.from_entries(["*@g.us", PHONE]) if self.calls == 1 else ChatPolicy.from_entries([PHONE])

        def __getattr__(self, name):
            return getattr(self.effective(), name)

    policy = ChangingPolicy()
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", policy)
    monkeypatch.setattr(whatsapp, "_bridge_request", lambda *a, **k: {"success": True})
    monkeypatch.setattr(whatsapp, "_bridge_json", lambda reply: reply)
    assert whatsapp.create_group("Alice", [PHONE], parent_jid=GROUP)["success"]
    assert policy.calls == 1


@pytest.mark.parametrize("wrap", ["0", "1"])
def test_location_multiline_address_survives_list_messages(paired_dbs, monkeypatch, wrap):
    monkeypatch.setenv("WHATSAPP_WRAP_UNTRUSTED", wrap)
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.unrestricted())
    address = "Alice Street\n" + "Bob Street " * 30
    with paired_dbs.messages() as conn:
        conn.execute("ALTER TABLE messages ADD COLUMN location TEXT")
        conn.execute(
            "INSERT INTO messages(id,chat_jid,sender,content,timestamp,is_from_me,media_type,location) VALUES('LOC',?,'5511999999999','Alice','2026-11-10 01:00:00+00:00',0,'location',?)",
            (PHONE, json.dumps({"live": False, "address": address, "latitude": 1.0, "longitude": 2.0})),
        )
    whatsapp._reset_schema_cache()
    result = main.list_messages(chat_jid=PHONE, include_context=False)["items"][0]["location"]["address"]
    assert result == (f"<untrusted>{address}</untrusted>" if wrap == "1" else address)


def test_business_metadata_sanitization_is_scoped(monkeypatch):
    monkeypatch.setattr(
        whatsapp,
        "check_business",
        lambda jid: {"address": "Alice\nStreet\u202e", "email": "alice\n@example.test", "categories": ["Shop\u202e"]},
    )
    result = main.check_business(PHONE)
    assert result["address"] == "AliceStreet"
    assert result["email"] == "alice@example.test"
    assert result["categories"] == ["Shop"]


@pytest.mark.parametrize("denial", ["read-only", "allow-list", "deny-list"])
async def test_create_policy_refuses_before_http(monkeypatch, group_http_bridge, denial):
    _, observed = group_http_bridge
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.unrestricted())
    tool_policy.set_active_policy(
        ToolPolicy(
            read_only=denial == "read-only",
            allow=frozenset({"send_reaction"}) if denial == "allow-list" else frozenset(),
            deny=frozenset({"create_group"}) if denial == "deny-list" else frozenset(),
        )
    )
    try:
        async with group_sdk_client() as client:
            result = await client.call_tool("create_group", {"name": "Alice", "participants": [PHONE]})
        assert result.is_error
        assert json.loads(result.content[0].text)["error"]["code"] == "denied"
        assert observed == []
    finally:
        tool_policy.set_active_policy(None)


@pytest.mark.parametrize("entries", [[PHONE], [GROUP, PHONE], ["*@g.us", "5511888888888"]])
async def test_create_chat_denies_whole_batch(monkeypatch, group_http_bridge, entries):
    _, observed = group_http_bridge
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(entries))
    async with group_sdk_client() as client:
        result = await client.call_tool("create_group", {"name": "Alice", "participants": [PHONE]})
    assert result.is_error
    assert json.loads(result.content[0].text)["error"]["code"] == "denied"
    assert observed == []


async def test_create_sdk_through_http_and_partial_result(monkeypatch, group_http_bridge):
    reply, observed = group_http_bridge
    reply.update(
        status=200,
        payload={
            "success": True,
            "group_jid": GROUP,
            "not_added": [{"jid": PHONE, "error": 403, "invite_code": "fake-invite"}],
        },
    )
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["*@g.us", PHONE]))
    async with group_sdk_client() as client:
        result = await client.call_tool("create_group", {"name": "Alice", "participants": [PHONE], "parent_jid": GROUP})
    assert not result.is_error
    payload = json.loads(result.content[0].text)
    assert payload["group_jid"] == GROUP and payload["not_added"] == [
        {"jid": PHONE, "error": 403, "invite_code": "fake-invite"}
    ]
    assert observed[0][0].endswith("/group/create")
    assert observed[0][1] == {"name": "Alice", "participants": [PHONE], "community": False, "parent_jid": GROUP}
    assert observed[0][2].startswith("Bearer ")


def test_business_allow_list_precedes_bridge(monkeypatch):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([GROUP]))
    monkeypatch.setattr(whatsapp, "_bridge_request", lambda *a, **k: pytest.fail("bridge reached"))
    with pytest.raises(ToolError, match="allow-list"):
        whatsapp.check_business(PHONE)


def test_business_partial_answer_and_subgroup_filter(monkeypatch):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([GROUP, PHONE]))
    calls = []

    def request(method, path, **kwargs):
        calls.append((method, path, kwargs))
        return (
            {
                "success": True,
                "is_business": True,
                "rate_limited": True,
                "verified_name_error": "429",
                "email": "alice@example.test",
            }
            if path == "/business"
            else {
                "success": True,
                "groups": [{"jid": GROUP, "name": "Alice"}, {"jid": "120363000000000002@g.us", "name": "Bob"}],
            }
        )

    monkeypatch.setattr(whatsapp, "_bridge_request", request)
    monkeypatch.setattr(whatsapp, "_bridge_json", lambda response: response)
    result = main.check_business(PHONE)
    assert result["is_business"] and result["rate_limited"] and result["email"] == "alice@example.test"
    assert main.list_subgroups(GROUP)["groups"] == [{"jid": GROUP, "name": "Alice"}]
    assert calls[0] == ("GET", "/business", {"params": {"jid": PHONE}})
