"""Conversation allow-list (least-privilege access for agents).

``WHATSAPP_ALLOWED_CHATS`` restricts which chats the MCP server will read from
or act on. Unset means everything (today's behaviour). When set, every read
tool filters to the listed chats and every write tool refuses other targets,
so an agent that goes off the rails can only touch what you enabled.

Entries are comma-separated and may be:

- a full JID: ``5511999999999@s.whatsapp.net``, ``120363...@g.us``, ``...@lid``
- a bare phone number (country code, digits only) → ``@s.whatsapp.net``
- a server wildcard: ``*@g.us`` (all groups) or ``*@s.whatsapp.net`` (all DMs)

The bridge's authorizeChat helper repeats enforcement on chat-taking REST
endpoints, including history and download; see whatsapp-bridge/chat_policy.go.
"""

from __future__ import annotations

import logging
import os
from collections.abc import Mapping
from dataclasses import dataclass

from errors import ToolError

ENV_VAR = "WHATSAPP_ALLOWED_CHATS"
DEFAULT_USER_SERVER = "s.whatsapp.net"
MALFORMED_TARGET = "Malformed chat target: more than one '@'"


def validate_chat_target(jid: str | None) -> None:
    if (jid or "").count("@") > 1:
        raise ToolError("invalid_argument", MALFORMED_TARGET)


def normalize_chat_entry(raw: str) -> str:
    """Canonical form for allow-list entries and for JIDs checked against them."""
    value = (raw or "").strip()
    if not value:
        return ""
    # Keep invalid entries literal: ignoring them could make the policy unrestricted.
    if value.count("@") > 1:
        return value
    if "@" not in value:
        return f"{value}@{DEFAULT_USER_SERVER}"
    user, _, server = value.rpartition("@")
    # Drop a device suffix ("123:4@s.whatsapp.net") so linked-device JIDs match the chat.
    user = user.split(":", 1)[0]
    return f"{user}@{server.lower()}"


@dataclass(frozen=True)
class ChatPolicy:
    exact: frozenset[str]
    servers: frozenset[str]  # from "*@server" wildcards
    restricted: bool
    invalid_positions: tuple[int, ...] = ()

    @classmethod
    def unrestricted(cls) -> ChatPolicy:
        return cls(exact=frozenset(), servers=frozenset(), restricted=False)

    @classmethod
    def from_entries(cls, entries: list[str]) -> ChatPolicy:
        exact: set[str] = set()
        servers: set[str] = set()
        invalid: list[int] = []
        for index, entry in enumerate(entries, 1):
            if entry.count("@") > 1:
                invalid.append(index)
            normalized = normalize_chat_entry(entry)
            if not normalized:
                continue
            user, _, server = normalized.rpartition("@")
            if user == "*":
                servers.add(server)
            else:
                exact.add(normalized)
        if not exact and not servers:
            return cls.unrestricted()
        return cls(
            exact=frozenset(exact), servers=frozenset(servers), restricted=True, invalid_positions=tuple(invalid)
        )

    def allows(self, jid: str | None) -> bool:
        if (jid or "").count("@") > 1:
            return False
        if not self.restricted:
            return True
        normalized = normalize_chat_entry(jid or "")
        if not normalized:
            return False
        if normalized in self.exact:
            return True
        return normalized.rpartition("@")[2] in self.servers

    def sql_clause(self, column: str) -> tuple[str, list[str]]:
        """SQL predicate restricting ``column`` (a chat JID column) to allowed chats.

        Unrestricted reads use a constant predicate to preserve covering indexes.
        """
        unambiguous = f"(length({column}) - length(replace({column}, '@', '')) <= 1)"
        if not self.restricted:
            return "1=1", []
        parts: list[str] = []
        params: list[str] = []
        if self.exact:
            parts.append(f"{column} IN ({','.join('?' * len(self.exact))})")
            params.extend(sorted(self.exact))
        for server in sorted(self.servers):
            parts.append(f"{column} LIKE ?")
            params.append(f"%@{server}")
        return "(" + unambiguous + " AND (" + " OR ".join(parts) + "))", params

    def denial_message(self, jid: str | None) -> str:
        if (jid or "").count("@") > 1:
            return MALFORMED_TARGET
        return f"Chat {jid!r} is not in {ENV_VAR}; this server is restricted to an allow-list of conversations"

    def warn_invalid_entries(self) -> None:
        if self.invalid_positions:
            logging.getLogger("whatsapp_mcp").warning(
                "%s: malformed entries at positions %s are retained as refused literals",
                ENV_VAR,
                list(self.invalid_positions),
            )


def load_chat_policy(env: Mapping[str, str] | None = None) -> ChatPolicy:
    source: Mapping[str, str] = os.environ if env is None else env
    return ChatPolicy.from_entries(source.get(ENV_VAR, "").split(","))
