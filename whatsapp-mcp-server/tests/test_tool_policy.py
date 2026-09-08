"""Tool policy: read-only mode plus the per-tool allow/deny lists.

Hidden from tools/list, refused if called anyway, and a typo in a list is a
startup error rather than a silently wider policy.
"""

from __future__ import annotations

import pytest

import main
import tool_policy
import whatsapp
from tool_policy import (
    ALLOW_TOOLS_ENV,
    DENY_TOOLS_ENV,
    READ_ONLY_ENV,
    ToolPolicy,
    apply_tool_policy,
    load_tool_policy,
    mutating_tools,
    parse_bool_env,
    parse_tool_list,
    registered_tool_names,
)

# The contract this feature promises. Kept spelled out so a tool that quietly
# loses its @mutating_tool decorator fails here instead of in production.
EXPECTED_MUTATING = {
    "send_message",
    "send_file",
    "send_audio_message",
    "send_reaction",
    "send_typing",
    "mark_messages_read",
    "delete_message",
    "edit_message",
    "forward_message",
    "manage_group_participants",
    "update_group",
    "get_group_invite_link",
    "leave_group",
    "purge_media",
    "request_history",
}

# Reads (and local-only writes) that must survive read-only mode.
EXPECTED_READABLE = {
    "list_messages",
    "list_chats",
    "search_contacts",
    "get_message_context",
    "list_group_members",
    "get_poll_results",
    "download_media",
    "transcribe_audio",
    "annotate_media",
    "get_media_notes",
    "annotate",
    "compact",
    "get_notes",
    "search_notes",
    "bridge_status",
    "coverage",
    "message_stats",
}


@pytest.fixture(autouse=True)
def _reset_active_policy():
    """Tools consult the module-level policy; never leak one into another test."""
    yield
    tool_policy.set_active_policy(None)


class FakeServer:
    """Duck-typed MCPServer: the two members apply_tool_policy touches."""

    class _Manager:
        def __init__(self, names):
            self.names = list(names)

        def list_tools(self):
            return [type("T", (), {"name": n})() for n in self.names]

    def __init__(self, names):
        self._tool_manager = self._Manager(names)
        self.removed: list[str] = []

    def remove_tool(self, name: str) -> None:
        self._tool_manager.names.remove(name)
        self.removed.append(name)


class TestParsing:
    @pytest.mark.parametrize("raw", ["1", "true", "TRUE", " yes ", "on"])
    def test_truthy(self, raw):
        assert parse_bool_env(raw, READ_ONLY_ENV) is True

    @pytest.mark.parametrize("raw", [None, "", "   ", "0", "false", "no", "OFF"])
    def test_falsy(self, raw):
        assert parse_bool_env(raw, READ_ONLY_ENV) is False

    @pytest.mark.parametrize("raw", ["treu", "2", "read-only", "y"])
    def test_junk_is_an_error_not_a_silent_off(self, raw):
        with pytest.raises(ValueError, match=READ_ONLY_ENV):
            parse_bool_env(raw, READ_ONLY_ENV)

    def test_load_from_env_mapping(self):
        assert load_tool_policy({}).read_only is False
        assert load_tool_policy({READ_ONLY_ENV: "1"}).read_only is True

    @pytest.mark.parametrize(
        ("raw", "expected"),
        [
            (None, set()),
            ("", set()),
            ("  ,  ", set()),
            ("send_message", {"send_message"}),
            (" send_message , list_chats ,", {"send_message", "list_chats"}),
        ],
    )
    def test_parse_tool_list(self, raw, expected):
        assert parse_tool_list(raw) == expected

    def test_load_reads_both_lists(self):
        policy = load_tool_policy({ALLOW_TOOLS_ENV: "list_chats", DENY_TOOLS_ENV: "send_message,leave_group"})
        assert policy.allow == {"list_chats"}
        assert policy.deny == {"send_message", "leave_group"}


class TestPolicy:
    def test_every_mutating_tool_is_decorated(self):
        assert mutating_tools() == EXPECTED_MUTATING

    def test_registered_tools_cover_both_sets(self):
        names = registered_tool_names(main.mcp)
        assert EXPECTED_MUTATING <= names
        assert EXPECTED_READABLE <= names

    def test_unrestricted_allows_everything(self):
        policy = ToolPolicy(read_only=False)
        assert policy.blocked() == ()
        assert all(policy.allows(name) for name in EXPECTED_MUTATING | EXPECTED_READABLE)

    def test_read_only_blocks_exactly_the_mutating_tools(self):
        policy = ToolPolicy(read_only=True)
        assert set(policy.blocked()) == EXPECTED_MUTATING
        assert all(not policy.allows(name) for name in EXPECTED_MUTATING)
        assert all(policy.allows(name) for name in EXPECTED_READABLE)

    def test_summary_mentions_the_variable(self):
        assert READ_ONLY_ENV in ToolPolicy(read_only=False).summary()
        summary = ToolPolicy(read_only=True).summary()
        assert READ_ONLY_ENV in summary and "send_message" in summary


class TestToolsList:
    def test_read_only_removes_mutating_tools_from_the_listing(self):
        server = FakeServer(sorted(EXPECTED_MUTATING | EXPECTED_READABLE))
        removed = apply_tool_policy(server, ToolPolicy(read_only=True))
        assert set(removed) == EXPECTED_MUTATING
        assert set(server._tool_manager.names) == EXPECTED_READABLE

    def test_unrestricted_removes_nothing(self):
        server = FakeServer(sorted(EXPECTED_MUTATING | EXPECTED_READABLE))
        assert apply_tool_policy(server, ToolPolicy(read_only=False)) == []
        assert len(server._tool_manager.names) == len(EXPECTED_MUTATING | EXPECTED_READABLE)

    def test_applying_twice_is_harmless(self):
        server = FakeServer(sorted(EXPECTED_MUTATING | EXPECTED_READABLE))
        apply_tool_policy(server, ToolPolicy(read_only=True))
        assert apply_tool_policy(server, ToolPolicy(read_only=True)) == []


class TestAllowDeny:
    ALL = sorted(EXPECTED_MUTATING | EXPECTED_READABLE)

    def test_deny_removes_named_tools_only(self):
        server = FakeServer(self.ALL)
        policy = ToolPolicy(deny=frozenset({"delete_message", "leave_group"}))
        assert apply_tool_policy(server, policy) == ["delete_message", "leave_group"]
        assert "send_message" in server._tool_manager.names

    def test_deny_reaches_read_tools_too(self):
        server = FakeServer(self.ALL)
        assert apply_tool_policy(server, ToolPolicy(deny=frozenset({"list_messages"}))) == ["list_messages"]

    def test_allow_is_exhaustive(self):
        server = FakeServer(self.ALL)
        policy = ToolPolicy(allow=frozenset({"list_messages", "send_reaction"}))
        apply_tool_policy(server, policy)
        assert set(server._tool_manager.names) == {"list_messages", "send_reaction"}

    def test_deny_wins_over_allow(self):
        policy = ToolPolicy(allow=frozenset({"send_message"}), deny=frozenset({"send_message"}))
        assert policy.allows("send_message") is False
        assert DENY_TOOLS_ENV in policy.denial_message("send_message")

    def test_read_only_wins_over_allow(self):
        # An allow-list can narrow a read-only deployment, never widen it.
        policy = ToolPolicy(read_only=True, allow=frozenset({"send_reaction", "list_messages"}))
        assert policy.allows("send_reaction") is False
        assert policy.allows("list_messages") is True
        assert READ_ONLY_ENV in policy.denial_message("send_reaction")

    def test_composed_removal(self):
        server = FakeServer(self.ALL)
        policy = ToolPolicy(read_only=True, deny=frozenset({"list_messages"}))
        removed = apply_tool_policy(server, policy)
        assert set(removed) == EXPECTED_MUTATING | {"list_messages"}

    def test_denial_message_explains_which_list(self):
        policy = ToolPolicy(allow=frozenset({"list_messages"}))
        assert ALLOW_TOOLS_ENV in policy.denial_message("send_message")

    def test_summary_names_every_active_list(self):
        summary = ToolPolicy(
            read_only=True,
            allow=frozenset({"list_messages"}),
            deny=frozenset({"list_chats"}),
        ).summary(["send_message"])
        assert READ_ONLY_ENV in summary
        assert ALLOW_TOOLS_ENV in summary and "list_messages" in summary
        assert DENY_TOOLS_ENV in summary and "list_chats" in summary
        assert "1 tool(s) hidden (send_message)" in summary

    def test_summary_without_restrictions(self):
        assert "every tool enabled" in ToolPolicy().summary([])


class TestValidation:
    """A typo must fail the process, not silently widen or narrow the policy."""

    KNOWN = frozenset({"list_messages", "send_message"})

    def test_known_names_pass(self):
        ToolPolicy(allow=frozenset({"list_messages"}), deny=frozenset({"send_message"})).validate(self.KNOWN)

    def test_empty_lists_pass(self):
        ToolPolicy(read_only=True).validate(self.KNOWN)

    @pytest.mark.parametrize("var", [ALLOW_TOOLS_ENV, DENY_TOOLS_ENV])
    def test_unknown_name_raises_and_lists_valid_names(self, var):
        field = "allow" if var == ALLOW_TOOLS_ENV else "deny"
        policy = ToolPolicy(**{field: frozenset({"send_mesage"})})
        with pytest.raises(ValueError) as excinfo:
            policy.validate(self.KNOWN)
        message = str(excinfo.value)
        assert var in message
        assert "send_mesage" in message
        assert "list_messages" in message and "send_message" in message

    def test_validates_against_the_real_server(self):
        known = registered_tool_names(main.mcp)
        ToolPolicy(allow=frozenset({"list_messages"}), deny=frozenset({"send_message"})).validate(known)
        with pytest.raises(ValueError, match="unknown tool"):
            ToolPolicy(deny=frozenset({"send_whatsapp"})).validate(known)


class TestCallTimeDenial:
    """Belt and braces: a tool called anyway (direct import, stale client) refuses."""

    @pytest.fixture(autouse=True)
    def _read_only(self):
        tool_policy.set_active_policy(ToolPolicy(read_only=True))

    @pytest.fixture(autouse=True)
    def _no_bridge(self, monkeypatch):
        def explode(*args, **kwargs):
            raise AssertionError("a blocked tool reached the bridge")

        monkeypatch.setattr(whatsapp.bridge_http, "post", explode)

    @pytest.mark.parametrize(
        ("call"),
        [
            lambda: main.send_message("5511999999999@s.whatsapp.net", "hi"),
            lambda: main.send_file("5511999999999@s.whatsapp.net", "/tmp/x.png"),
            lambda: main.send_audio_message("5511999999999@s.whatsapp.net", "/tmp/x.ogg"),
            lambda: main.send_reaction("5511999999999@s.whatsapp.net", "MSG1", "👍"),
            lambda: main.send_typing("5511999999999@s.whatsapp.net", True),
            lambda: main.mark_messages_read("5511999999999@s.whatsapp.net", ["MSG1"]),
            lambda: main.delete_message("5511999999999@s.whatsapp.net", "MSG1", True),
            lambda: main.edit_message("5511999999999@s.whatsapp.net", "MSG1", "new"),
            lambda: main.forward_message("5511999999999@s.whatsapp.net", "MSG1", "120363@g.us"),
            lambda: main.manage_group_participants("120363@g.us", "remove", ["5511999999999"]),
            lambda: main.update_group("120363@g.us", name="new"),
            lambda: main.get_group_invite_link("120363@g.us", reset=True),
            lambda: main.leave_group("120363@g.us"),
            lambda: main.purge_media(chat_jid="5511999999999@s.whatsapp.net", dry_run=False),
        ],
    )
    def test_mutating_tools_return_the_denied_envelope(self, call):
        result = call()
        assert result["error"]["code"] == "denied"
        assert READ_ONLY_ENV in result["error"]["message"]

    def test_denial_names_the_tool(self):
        assert "send_message" in main.send_message("5511999999999@s.whatsapp.net", "hi")["error"]["message"]


class TestCallTimeDenialByList:
    """A denied mutating tool refuses at call time whichever list blocked it."""

    @pytest.fixture(autouse=True)
    def _no_bridge(self, monkeypatch):
        def explode(*args, **kwargs):
            raise AssertionError("a blocked tool reached the bridge")

        monkeypatch.setattr(whatsapp.bridge_http, "post", explode)

    def test_deny_list(self):
        tool_policy.set_active_policy(ToolPolicy(deny=frozenset({"delete_message"})))
        result = main.delete_message("5511999999999@s.whatsapp.net", "MSG1", True)
        assert result["error"]["code"] == "denied"
        assert DENY_TOOLS_ENV in result["error"]["message"]

    def test_allow_list_omission(self):
        tool_policy.set_active_policy(ToolPolicy(allow=frozenset({"list_messages"})))
        result = main.send_typing("5511999999999@s.whatsapp.net", True)
        assert result["error"]["code"] == "denied"
        assert ALLOW_TOOLS_ENV in result["error"]["message"]


class TestActivePolicyFallback:
    def test_unparseable_value_fails_closed(self, monkeypatch):
        monkeypatch.setenv(READ_ONLY_ENV, "treu")
        tool_policy.set_active_policy(None)
        assert tool_policy.active_policy().read_only is True

    def test_unset_env_allows_everything(self, monkeypatch):
        monkeypatch.delenv(READ_ONLY_ENV, raising=False)
        tool_policy.set_active_policy(None)
        assert tool_policy.active_policy().read_only is False
