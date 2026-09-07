import pytest

import whatsapp
from chat_policy import ChatPolicy
from errors import ToolError

GROUP = "120363000000000001@g.us"


class Resp:
    def __init__(self, status=200, payload=None, text=""):
        self.status_code = status
        self._payload = payload
        self.text = text

    def json(self):
        if self._payload is None:
            raise ValueError("no json")
        return self._payload


def test_get_group_members_happy_path(monkeypatch):
    calls = []

    def fake_get(url, params=None, headers=None, timeout=None):
        calls.append((url, params, headers, timeout))
        return Resp(
            payload={
                "success": True,
                "group_jid": GROUP,
                "name": "Obra",
                "members": [
                    {
                        "jid": "5511999999999@s.whatsapp.net",
                        "phone_number": "5511999999999",
                        "name": "Enrico",
                        "is_admin": True,
                    },
                    {"jid": "777@lid", "phone_number": "5511888888888", "is_admin": False},
                    {"jid": "888@lid", "is_admin": False},
                ],
            }
        )

    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "test-token-0123456789")
    monkeypatch.setattr(whatsapp.bridge_http, "get", fake_get)

    result = whatsapp.get_group_members(GROUP)

    assert result["success"] is True
    assert calls[0][0].endswith("/group/members")
    assert calls[0][1] == {"jid": GROUP}
    assert calls[0][2] == {"Authorization": "Bearer test-token-0123456789"}
    # admin first, then by JID; "members" is replaced by the PageResult "items"
    assert [m["display"] for m in result["items"]] == ["Enrico", "5511888888888", "888@lid"]
    assert "members" not in result
    assert result["participant_count"] == 3
    assert result["has_more"] is False and result["next_cursor"] is None


def test_get_group_members_rejects_non_group(monkeypatch):
    monkeypatch.setattr(whatsapp.bridge_http, "get", lambda *a, **k: pytest.fail("bridge called"))
    with pytest.raises(ToolError, match="Not a group JID") as exc:
        whatsapp.get_group_members("5511999999999@s.whatsapp.net")
    assert exc.value.code == "invalid_argument"


def test_get_group_members_respects_policy(monkeypatch):
    monkeypatch.setattr(whatsapp, "CHAT_POLICY", ChatPolicy.from_entries(["5511999999999"]))
    monkeypatch.setattr(whatsapp.bridge_http, "get", lambda *a, **k: pytest.fail("bridge called"))
    with pytest.raises(ToolError, match="WHATSAPP_ALLOWED_CHATS") as exc:
        whatsapp.get_group_members(GROUP)
    assert exc.value.code == "denied"


def test_get_group_members_bridge_error(monkeypatch):
    monkeypatch.setattr(
        whatsapp.bridge_http, "get", lambda *a, **k: Resp(502, {"success": False, "message": "not connected"})
    )
    with pytest.raises(ToolError, match="not connected") as exc:
        whatsapp.get_group_members(GROUP)
    assert exc.value.code == "bridge_unavailable"


def test_get_group_members_non_json_error(monkeypatch):
    monkeypatch.setattr(whatsapp.bridge_http, "get", lambda *a, **k: Resp(500, None, "boom"))
    with pytest.raises(ToolError, match="boom") as exc:
        whatsapp.get_group_members(GROUP)
    assert exc.value.code == "bridge_unavailable"


BIG_GROUP_SIZE = 731


def _big_group_members():
    """731 members in bridge order (shuffled deterministically), 7 of them admins."""
    members = []
    for n in range(BIG_GROUP_SIZE):
        # non-sequential JIDs so a page slice can never be confused with insertion order
        members.append(
            {
                "jid": f"5511{(n * 7919) % 100000000:08d}@s.whatsapp.net",
                "phone_number": f"5511{(n * 7919) % 100000000:08d}",
                "is_admin": n % 100 == 3,
                "is_super_admin": n == 0,
            }
        )
    return members


@pytest.fixture
def big_group(monkeypatch):
    """Bridge that always answers with the same 731 members, in a rotating order."""
    state = {"calls": 0}
    members = _big_group_members()

    def fake_get(url, params=None, headers=None, timeout=None):
        state["calls"] += 1
        # the live WhatsApp query has no order guarantee: rotate to prove ours holds
        rotated = members[state["calls"] :] + members[: state["calls"]]
        return Resp(payload={"success": True, "group_jid": GROUP, "name": "Obra", "members": rotated})

    monkeypatch.setattr(whatsapp.bridge_http, "get", fake_get)
    return members


def test_get_group_members_pages_cover_everyone_once(big_group):
    seen, cursor, pages = [], None, 0
    while True:
        page = whatsapp.get_group_members(GROUP, limit=100, cursor=cursor)
        pages += 1
        assert page["participant_count"] == BIG_GROUP_SIZE
        assert len(page["items"]) <= 100
        seen.extend(page["items"])
        if not page["has_more"]:
            assert page["next_cursor"] is None
            break
        cursor = page["next_cursor"]
        assert pages < 20, "cursor walk did not terminate"

    jids = [m["jid"] for m in seen]
    assert pages == 8
    assert len(jids) == BIG_GROUP_SIZE and len(set(jids)) == BIG_GROUP_SIZE
    # admins first (super admin at the head), then JID ascending within each rank
    ranks = [whatsapp._group_member_rank(m) for m in seen]
    assert ranks == sorted(ranks)
    assert seen[0]["is_super_admin"] is True
    assert sum(1 for m in seen[:9] if m["is_admin"] or m["is_super_admin"]) == 9


def test_get_group_members_page_argument_matches_cursor_walk(big_group):
    by_cursor = whatsapp.get_group_members(GROUP, limit=100)
    second_by_cursor = whatsapp.get_group_members(GROUP, limit=100, cursor=by_cursor["next_cursor"])
    second_by_page = whatsapp.get_group_members(GROUP, limit=100, page=1)
    assert [m["jid"] for m in second_by_cursor["items"]] == [m["jid"] for m in second_by_page["items"]]


def test_get_group_members_limit_is_capped_and_last_page_ends(big_group):
    first = whatsapp.get_group_members(GROUP, limit=5000)
    assert len(first["items"]) == 500 and first["has_more"] is True
    last = whatsapp.get_group_members(GROUP, limit=500, cursor=first["next_cursor"])
    assert len(last["items"]) == BIG_GROUP_SIZE - 500
    assert last["has_more"] is False and last["next_cursor"] is None
    # a page past the end is empty, not an error
    beyond = whatsapp.get_group_members(GROUP, limit=500, page=4)
    assert beyond["items"] == [] and beyond["has_more"] is False


def test_get_group_members_rejects_cursor_from_another_group(big_group):
    page = whatsapp.get_group_members(GROUP, limit=100)
    with pytest.raises(ToolError, match="different group") as exc:
        whatsapp.get_group_members("120363000000000002@g.us", cursor=page["next_cursor"])
    assert exc.value.code == "invalid_argument"


def test_get_group_members_rejects_foreign_cursor(big_group):
    foreign = whatsapp.encode_cursor({"k": "chats", "o": 100})
    with pytest.raises(ToolError, match="does not belong to group_members") as exc:
        whatsapp.get_group_members(GROUP, cursor=foreign)
    assert exc.value.code == "invalid_argument"
