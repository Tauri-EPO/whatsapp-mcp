"""Offline label-cache reads and public MCP to bridge mutation contract."""

import json
import sqlite3
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest
from mcp.server.mcpserver.exceptions import ToolError as SdkToolError

import main
import tool_policy
import whatsapp
from chat_policy import ChatPolicy
from errors import ToolError
from strict_args import StrictArgumentServer
from tool_policy import ToolPolicy

CHAT = "5511999999999@s.whatsapp.net"
OTHER = "5511888888888@s.whatsapp.net"
LID = "100000000000001@lid"


@pytest.fixture
def labels_db(tmp_path, monkeypatch):
    path = tmp_path / "messages.db"
    with sqlite3.connect(path) as db:
        db.executescript("""
        CREATE TABLE labels(id TEXT PRIMARY KEY, name TEXT, color INTEGER, deleted BOOLEAN,
                            updated_at TIMESTAMP, action_ms INTEGER,
                            type INTEGER NOT NULL DEFAULT 0, immutable BOOLEAN NOT NULL DEFAULT 0,
                            predefined_id INTEGER);
        CREATE TABLE chat_labels(chat_jid TEXT, label_id TEXT, labeled BOOLEAN,
                                 updated_at TIMESTAMP, action_ms INTEGER, PRIMARY KEY(chat_jid,label_id));
        INSERT INTO labels(id,name,color,deleted,updated_at,action_ms) VALUES('1','Alice',3,0,'2026-01-01 12:00:00+00:00',1);
        INSERT INTO labels(id,name,color,deleted,updated_at,action_ms) VALUES('2','Bob',4,1,'2026-01-01 12:00:00+00:00',2);
        INSERT INTO labels(id,name,color,deleted,updated_at,action_ms) VALUES('3','Alice',5,0,'2026-01-01 12:00:00+00:00',3);
        """)
        db.executemany(
            "INSERT INTO chat_labels VALUES(?,?,?,'2026-01-01 12:00:00+00:00',1)",
            [(CHAT, "1", True), (CHAT, "2", True), (OTHER, "3", True)],
        )
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", str(tmp_path / "missing.db"))
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([]))
    monkeypatch.setattr(whatsapp.bridge_http, "get", lambda *a, **kw: pytest.fail("read called bridge"))
    return path


def test_list_labels_offline_and_deleted(labels_db):
    assert [row["id"] for row in main.list_labels()["labels"]] == ["1", "3"]
    assert [row["id"] for row in main.list_labels(include_deleted=True)["labels"]] == ["1", "2", "3"]
    result = main.list_labels(CHAT)
    assert result == {
        "labels": [
            {
                "id": "1",
                "name": "Alice",
                "color": 3,
                "deleted": False,
                "type": 0,
                "immutable": False,
                "predefined_id": None,
            }
        ]
    }
    assert len(main.list_labels(CHAT, True)["labels"]) == 2
    with sqlite3.connect(labels_db) as db:
        db.execute("UPDATE chat_labels SET labeled=0 WHERE chat_jid=?", (CHAT,))
    assert main.list_labels(CHAT) == {"labels": []}


def test_empty_catalog(labels_db):
    with sqlite3.connect(labels_db) as db:
        db.execute("DELETE FROM labels")
    assert main.list_labels() == {"labels": []}
    assert main.label_chat(CHAT, "1")["error"]["code"] == "not_found"


@pytest.mark.parametrize("tool", ["list_labels", "label_chat"])
def test_label_policy_before_database(monkeypatch, tool):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([OTHER]))
    monkeypatch.setattr(whatsapp, "_connect_messages_db", lambda: pytest.fail("opened denied chat database"))
    fn = getattr(main, tool)
    result = fn(CHAT) if tool == "list_labels" else fn(CHAT, "1")
    assert result["error"]["code"] == "denied"


@pytest.mark.parametrize(
    "target", ["", "5511999999999", "@lid", "5511999999999@", "5511999999999:1@s.whatsapp.net", CHAT + "@lid"]
)
def test_label_bad_chat(labels_db, target):
    assert main.list_labels(target)["error"]["code"] == "invalid_argument"
    assert main.label_chat(target, "1")["error"]["code"] == "invalid_argument"


@pytest.mark.parametrize("target", [CHAT, LID])
@pytest.mark.parametrize("restricted", [False, True])
def test_label_filter_verified_twins(labels_db, monkeypatch, tmp_path, target, restricted):
    map_path = tmp_path / "whatsapp.db"
    with sqlite3.connect(map_path) as db:
        db.execute("CREATE TABLE whatsmeow_lid_map(pn TEXT, lid TEXT)")
        db.execute("INSERT INTO whatsmeow_lid_map VALUES('5511999999999','100000000000001')")
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", str(map_path))
    if restricted:
        monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries([target]))
    with sqlite3.connect(labels_db) as db:
        db.execute("UPDATE chat_labels SET chat_jid=? WHERE chat_jid=?", (LID, CHAT))
    ids = [row["id"] for row in main.list_labels(target)["labels"]]
    assert ids == ([] if restricted and target == CHAT else ["1"])


def test_label_filter_does_not_guess_namespace(labels_db):
    assert main.list_labels(LID) == {"labels": []}
    assert main.list_labels("5511999999999@lid") == {"labels": []}


@pytest.mark.parametrize("target", [CHAT, LID])
@pytest.mark.parametrize("same_time", [False, True])
def test_latest_twin_removal_wins(labels_db, monkeypatch, tmp_path, target, same_time):
    map_path = tmp_path / "whatsapp.db"
    with sqlite3.connect(map_path) as db:
        db.execute("CREATE TABLE whatsmeow_lid_map(pn TEXT, lid TEXT)")
        db.execute("INSERT INTO whatsmeow_lid_map VALUES('5511999999999','100000000000001')")
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", str(map_path))
    with sqlite3.connect(labels_db) as db:
        db.execute("UPDATE chat_labels SET chat_jid=? WHERE chat_jid=?", (LID, CHAT))
        db.execute(
            "INSERT INTO chat_labels VALUES(?, '1', 0, '2026-01-01 12:00:00+00:00', ?)", (CHAT, 1 if same_time else 2)
        )
    assert main.list_labels(target) == {"labels": []}


@pytest.mark.parametrize("labeled", [True, False])
@pytest.mark.parametrize("by_name", [True, False])
def test_label_payload_and_token(labels_db, monkeypatch, labeled, by_name):
    with sqlite3.connect(labels_db) as db:
        db.execute("DELETE FROM labels WHERE id='3'")
    seen = []

    class Response:
        status_code = 200

        def json(self):
            return {"success": True, "label_id": "1", "labeled": labeled, "sent": True, "confirmed": False}

    monkeypatch.setattr(whatsapp, "_read_bridge_token", lambda: "t" * 32)
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda url, **kw: seen.append((url, kw)) or Response())
    result = main.label_chat(CHAT, "Alice" if by_name else "1", labeled)
    assert result["success"] and result["labeled"] == labeled and result["confirmed"] is False
    assert seen[0][0].endswith("/api/chat/label")
    assert seen[0][1]["json"] == {"chat_jid": CHAT, "label_id": "1", "labeled": labeled}
    assert seen[0][1]["headers"]["Authorization"] == "Bearer " + "t" * 32


@pytest.mark.parametrize(
    "label,code",
    [
        ("", "invalid_argument"),
        ("Alice", "invalid_argument"),
        ("2", "not_found"),
        ("Bob", "not_found"),
        ("missing", "not_found"),
    ],
)
def test_label_lookup_refusals(labels_db, monkeypatch, label, code):
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda *a, **kw: pytest.fail("refused label sent"))
    assert main.label_chat(CHAT, label)["error"]["code"] == code


def test_label_id_wins_over_name_collision(labels_db, monkeypatch):
    with sqlite3.connect(labels_db) as db:
        db.execute("UPDATE labels SET name='1' WHERE id='3'")
    seen = []
    monkeypatch.setattr(whatsapp, "_bridge_request", lambda *a, **kw: seen.append(kw["json"]) or object())
    monkeypatch.setattr(whatsapp, "_bridge_json", lambda response: {})
    whatsapp.label_chat(CHAT, "1")
    assert seen[0]["label_id"] == "1"


@pytest.mark.parametrize(
    "policy",
    [
        ToolPolicy(read_only=True),
        ToolPolicy(allow=frozenset({"archive_chat"})),
        ToolPolicy(deny=frozenset({"label_chat"})),
    ],
)
def test_label_tool_refusal_before_db(monkeypatch, policy):
    monkeypatch.setattr(tool_policy, "_active", policy)
    monkeypatch.setattr(whatsapp, "_connect_messages_db", lambda: pytest.fail("denied tool opened cache"))
    assert main.label_chat(CHAT, "1")["error"]["code"] == "denied"


def test_label_names_sanitized(labels_db):
    with sqlite3.connect(labels_db) as db:
        db.execute("UPDATE labels SET name=? WHERE id='1'", ("Alice\x00\u200b",))
    assert main.list_labels(CHAT)["labels"][0]["name"] == "Alice"


def test_missing_cache_does_not_create_tables(tmp_path, monkeypatch):
    path = tmp_path / "missing.db"
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    with pytest.raises(ToolError):
        whatsapp.list_labels()
    assert not path.exists()


def test_pre_labels_bridge_cache(labels_db, monkeypatch, caplog):
    with sqlite3.connect(labels_db) as db:
        db.execute("DROP TABLE chat_labels")
        db.execute("DROP TABLE labels")
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda *a, **kw: pytest.fail("unknown label sent"))
    assert main.list_labels() == {"labels": []}
    assert main.list_labels(CHAT) == {"labels": []}
    result = main.label_chat(CHAT, "1")
    assert result["error"] == {"code": "not_found", "message": "Unknown label; use list_labels"}
    assert not caplog.records
    with sqlite3.connect(labels_db) as db:
        assert not db.execute("SELECT 1 FROM sqlite_master WHERE name IN ('labels', 'chat_labels')").fetchall()


def test_labels_probe_sees_upgrade_in_wal(labels_db):
    with sqlite3.connect(labels_db) as writer:
        writer.execute("PRAGMA journal_mode=WAL")
        writer.execute("DROP TABLE labels")
        writer.commit()
        assert main.list_labels() == {"labels": []}
        writer.execute(
            "CREATE TABLE labels(id TEXT, name TEXT, color INTEGER, deleted BOOLEAN, "
            "type INTEGER, immutable BOOLEAN, predefined_id INTEGER)"
        )
        writer.execute("INSERT INTO labels VALUES('1','Alice',3,0,19,0,NULL)")
        writer.commit()
        assert main.list_labels()["labels"][0]["id"] == "1"


@pytest.mark.parametrize("label", ["1", "Alice"])
@pytest.mark.parametrize("labeled", [True, False])
def test_immutable_label_refused(labels_db, monkeypatch, label, labeled):
    with sqlite3.connect(labels_db) as db:
        db.execute("DELETE FROM labels WHERE id='3'")
        db.execute("UPDATE labels SET type=3, immutable=1, predefined_id=7 WHERE id='1'")
    entry = main.list_labels(CHAT)["labels"][0]
    assert (entry["type"], entry["immutable"], entry["predefined_id"]) == (3, True, 7)
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda *a, **kw: pytest.fail("immutable label sent"))
    assert main.label_chat(CHAT, label, labeled)["error"]["code"] == "invalid_argument"


@pytest.mark.parametrize("needle", ["Follow up", "Follow\u200b up\u200f", "Follow\u200e up"])
def test_label_matches_displayed_name(labels_db, monkeypatch, needle):
    with sqlite3.connect(labels_db) as db:
        db.execute("UPDATE labels SET name=? WHERE id='1'", ("Follow\u200b up\u200f",))
    displayed = main.list_labels(CHAT)["labels"][0]["name"]
    assert displayed == "Follow up"
    seen = []
    monkeypatch.setattr(whatsapp, "_bridge_request", lambda *a, **kw: seen.append(kw["json"]) or object())
    monkeypatch.setattr(whatsapp, "_bridge_json", lambda response: {"success": True})
    assert main.label_chat(CHAT, needle)["success"]
    assert seen == [{"chat_jid": CHAT, "label_id": "1", "labeled": True}]


@pytest.mark.parametrize("needle", ["Alice", "Alice\u200b"])
def test_label_sanitized_collision_refused(labels_db, monkeypatch, needle):
    with sqlite3.connect(labels_db) as db:
        db.execute("UPDATE labels SET name=? WHERE id='1'", ("Alice\u200b",))
    monkeypatch.setattr(whatsapp.bridge_http, "post", lambda *a, **kw: pytest.fail("ambiguous label sent"))
    assert main.label_chat(CHAT, needle)["error"]["code"] == "invalid_argument"


def test_exact_name_preserves_whitespace(labels_db, monkeypatch):
    with sqlite3.connect(labels_db) as db:
        db.execute("UPDATE labels SET name=' Alice ' WHERE id='1'")
    seen = []
    monkeypatch.setattr(whatsapp, "_bridge_request", lambda *a, **kw: seen.append(kw["json"]) or object())
    monkeypatch.setattr(whatsapp, "_bridge_json", lambda response: {})
    whatsapp.label_chat(CHAT, " Alice ")
    assert seen[0]["label_id"] == "1"


@pytest.mark.parametrize(
    "policy", [ToolPolicy(allow=frozenset({"label_chat"})), ToolPolicy(deny=frozenset({"list_labels"}))]
)
async def test_list_policy_at_mcp_entry(labels_db, monkeypatch, policy):
    server = StrictArgumentServer("labels-test")
    server.add_tool(main.list_labels)
    assert tool_policy.apply_tool_policy(server, policy) == ["list_labels"]
    monkeypatch.setattr(whatsapp, "_connect_messages_db", lambda: pytest.fail("denied read opened cache"))
    with pytest.raises(SdkToolError):
        await server.call_tool("list_labels", {})


async def test_mcp_label_real_http(labels_db, monkeypatch):
    seen = []

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            seen.append((self.path, self.headers["Authorization"], body))
            data = json.dumps(
                {
                    "success": True,
                    "label_id": body["label_id"],
                    "labeled": body["labeled"],
                    "sent": True,
                    "confirmed": False,
                }
            ).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def log_message(self, *args):
            pass

    server = HTTPServer(("127.0.0.1", 0), Handler)
    worker = threading.Thread(target=server.serve_forever)
    worker.start()
    monkeypatch.setattr(whatsapp, "WHATSAPP_API_BASE_URL", f"http://127.0.0.1:{server.server_port}/api")
    monkeypatch.setattr(whatsapp, "_read_bridge_token", lambda: "t" * 32)
    try:
        for labeled in (True, False):
            result = await main.mcp.call_tool("label_chat", {"chat_jid": CHAT, "label": "1", "labeled": labeled})
            body = json.loads(result.content[0].text)
            assert body["success"] is True and body["confirmed"] is False
            assert seen[-1] == (
                "/api/chat/label",
                "Bearer " + "t" * 32,
                {"chat_jid": CHAT, "label_id": "1", "labeled": labeled},
            )
        before = len(seen)
        result = await main.mcp.call_tool("list_labels", {"chat_jid": CHAT})
        assert json.loads(result.content[0].text)["labels"][0]["id"] == "1"
        assert len(seen) == before
    finally:
        server.shutdown()
        worker.join(timeout=5)
        server.server_close()
