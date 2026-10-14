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
import sqlite3
from collections.abc import Iterable, Mapping
from contextlib import contextmanager
from contextvars import ContextVar
from dataclasses import dataclass, replace
from typing import Any

from errors import ToolError
from phone import br_mobile_alternate, normalize_recipient

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
    origin: str = ENV_VAR  # the list a refusal names: the deploy variable or the runtime override

    @classmethod
    def unrestricted(cls) -> ChatPolicy:
        return cls(exact=frozenset(), servers=frozenset(), restricted=False)

    def effective(self) -> ChatPolicy:
        return self

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

    def require_participant(self, raw: str, twins: Iterable[str] = ()) -> None:
        """Authorize one participant and its known local identity spellings.

        This does not change the typed/registered checks for sends.
        """
        if not self.restricted:
            return
        target = normalize_recipient(raw).strip()
        validate_chat_target(target)
        if "@" not in target and not (target.isascii() and target.isdigit()):
            raise ToolError("invalid_argument", "participant must be a phone number or user JID")
        if any(c in target.split("@", 1)[0] for c in ":."):
            raise ToolError("invalid_argument", "participant must not have a device suffix")
        jid = normalize_chat_entry(target)
        user, _, server = jid.rpartition("@")
        if not user or any(c in user for c in " :.\t") or server not in (DEFAULT_USER_SERVER, "lid"):
            raise ToolError("invalid_argument", "participant must be a phone number or user JID")
        for candidate in (jid, *twins):
            if self.allows(candidate):
                return
            number, _, namespace = candidate.rpartition("@")
            if namespace == DEFAULT_USER_SERVER and (alternate := br_mobile_alternate(number)):
                if self.allows(f"{alternate}@{namespace}"):
                    return
        raise ToolError("denied", f"Participant {raw!r} is not in {self.origin}")

    def sql_clause(self, column: str) -> tuple[str, list[str]]:
        """SQL predicate restricting ``column`` (a chat JID column) to allowed chats.

        Unrestricted reads use a constant predicate to preserve covering indexes.
        """
        unambiguous = f"(length({column}) - length(replace({column}, '@', '')) <= 1)"
        if not self.restricted:
            return "1=1", []
        if not self.exact and not self.servers:
            return "0=1", []
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
        return f"Chat {jid!r} is not in {self.origin}; this server is restricted to an allow-list of conversations"

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


def policy_entries(policy: ChatPolicy) -> list[str]:
    return sorted(policy.exact | {f"*@{server}" for server in policy.servers})


def intersect_policies(runtime: ChatPolicy, deploy: ChatPolicy) -> ChatPolicy:
    if not deploy.restricted:
        return runtime
    exact = {entry for entry in runtime.exact if deploy.allows(entry)}
    exact.update(entry for entry in deploy.exact if runtime.allows(entry))
    return ChatPolicy(frozenset(exact), runtime.servers & deploy.servers, True)


_operation: ContextVar[dict[Any, ChatPolicy] | None] = ContextVar("chat_policy_operation", default=None)


@contextmanager
def policy_operation():
    """One lazy immutable policy per tool invocation, including worker threads."""
    import runtime_settings

    token = _operation.set({}) if _operation.get() is None else None
    try:
        with runtime_settings.settings_operation():
            yield
    finally:
        if token is not None:
            _operation.reset(token)


class RuntimeChatPolicy:
    """Keep imported policy references live; read the bridge table without writing it."""

    def __init__(self, deploy: ChatPolicy):
        self.deploy = deploy

    def effective(self) -> ChatPolicy:
        import runtime_settings

        operation = _operation.get()
        if operation is not None and self in operation:
            return operation[self]
        try:
            setting = runtime_settings.snapshot(require_store=True)["settings"]["chats.allowed"]
        except (OSError, ValueError, sqlite3.Error):
            raise ToolError("denied", "Runtime chat policy unavailable") from None
        policy = self.deploy
        if setting["source"] == "runtime":
            entries = ChatPolicy.from_entries(setting["value"])
            policy = intersect_policies(ChatPolicy(entries.exact, entries.servers, True), self.deploy)
            origin = "runtime chats.allowed" + (f" within {ENV_VAR} (deploy)" if self.deploy.restricted else "")
            policy = replace(policy, origin=origin)
        if operation is not None:
            operation[self] = policy
        return policy

    def __getattr__(self, name: str) -> Any:
        if name == "warn_invalid_entries":
            return self.deploy.warn_invalid_entries
        return getattr(self.effective(), name)
