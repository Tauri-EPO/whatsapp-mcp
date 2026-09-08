"""Operation-level access control (read-only mode, per-tool allow/deny).

``WHATSAPP_ALLOWED_CHATS`` (chat_policy.py) restricts *which chats* an agent may
touch. This module restricts *what it may do*. ``WHATSAPP_READ_ONLY=1`` turns the
server into a read-and-draft deployment: every tool with an external side effect
is removed from ``tools/list`` — so the model never sees it — and refuses with the
standard ``denied`` envelope if it is called anyway.

Two more knobs cut finer, by tool name:

- ``WHATSAPP_DENY_TOOLS=delete_message,leave_group`` — never offer these.
- ``WHATSAPP_ALLOW_TOOLS=list_messages,search_contacts,send_reaction`` — offer
  *only* these (an allow-list over every tool, reads included).

The three filters only ever remove capability: deny wins over allow, and
read-only wins over both, so ``WHATSAPP_ALLOW_TOOLS=send_message`` cannot switch
sending back on while read-only is set. Unknown tool names are a startup error
listing the valid ones — a typo in an allow-list must not silently widen it.

That is the right default whenever the agent reads attacker-controlled text
(any group, any forwarded message): a prompt injection can then ask for a send,
but there is nothing to call. The bridge enforces all three variables on its
mutating REST endpoints (403, whatsapp-bridge/read_only.go and tool_policy.go,
which maps each tool name to the endpoint it calls) as a second line of defence,
so a bug or a bypass on this side still cannot reach WhatsApp.

Which tools count as mutating is not a list maintained by hand: the
``@mutating_tool`` decorator in main.py registers each one, so a new tool is
either decorated (and covered) or it is not (and stays readable).

Deliberately *not* mutating:

- ``download_media`` / ``read_media`` / ``transcribe_audio`` — fetch and read;
  they only write to the local media cache. Naming ``download_media`` in a list
  still binds the other two: both fetch an uncached file through the endpoint
  ``download_media`` calls, so ``offers_download()`` gates that implicit fetch
  and the tools fall back to what the store already holds (issue #350).
- ``annotate_media`` and ``annotate`` — write notes.db, which is local MCP-owned
  state, never WhatsApp. A read-only assistant still needs somewhere to keep its
  own notes; a triage pass that cannot record what it concluded is the failure
  issue #286 describes.

Deliberately mutating even though it reads:

- ``get_group_invite_link`` — ``reset=True`` revokes the current link, and a link
  is group access that can be leaked. Blocked wholesale rather than per-argument.
"""

from __future__ import annotations

import functools
import os
from collections.abc import Callable, Collection, Mapping, Sequence
from dataclasses import dataclass
from typing import Any, Protocol

from errors import ToolError

READ_ONLY_ENV = "WHATSAPP_READ_ONLY"
ALLOW_TOOLS_ENV = "WHATSAPP_ALLOW_TOOLS"
DENY_TOOLS_ENV = "WHATSAPP_DENY_TOOLS"

_TRUE = ("1", "true", "yes", "on")
_FALSE = ("0", "false", "no", "off")

# The tool every implicit fetch is: read_media, transcribe_audio and the ingest
# worker all reach the bridge's /api/download, the endpoint download_media
# calls. Named once here so the three ask the same question.
DOWNLOAD_TOOL = "download_media"

# Populated by @mutating_tool at import time; see module docstring.
_MUTATING: set[str] = set()


def parse_bool_env(raw: str | None, var: str) -> bool:
    """Strict boolean parse: unset is False, anything unrecognised is an error.

    A security switch must not silently fall back to "off" because it was spelled
    ``WHATSAPP_READ_ONLY=treu``.
    """
    value = (raw or "").strip().lower()
    if not value:
        return False
    if value in _TRUE:
        return True
    if value in _FALSE:
        return False
    raise ValueError(f"{var}={raw!r} is not a boolean; use one of {', '.join(_TRUE + _FALSE)}")


def parse_tool_list(raw: str | None) -> frozenset[str]:
    """Comma-separated tool names; empty entries ignored. Validation is separate."""
    return frozenset(item.strip() for item in (raw or "").split(",") if item.strip())


@dataclass(frozen=True)
class ToolPolicy:
    """Which tools this process offers.

    Each field only removes capability: ``deny`` wins over ``allow``, and
    ``read_only`` wins over both.
    """

    read_only: bool = False
    allow: frozenset[str] = frozenset()
    deny: frozenset[str] = frozenset()

    def allows(self, name: str) -> bool:
        if name in self.deny:
            return False
        if self.allow and name not in self.allow:
            return False
        return not (self.read_only and name in _MUTATING)

    def blocked(self) -> tuple[str, ...]:
        """Mutating tools blocked by read-only (allow/deny also reach read tools)."""
        return tuple(sorted(name for name in _MUTATING if not self.allows(name)))

    def denial_message(self, name: str) -> str:
        if name in self.deny:
            return f"{name} is disabled: it is listed in {DENY_TOOLS_ENV}"
        if self.allow and name not in self.allow:
            return f"{name} is disabled: {ALLOW_TOOLS_ENV} is set and does not list it"
        return (
            f"{name} is disabled: {READ_ONLY_ENV} is set, so this server may read WhatsApp "
            f"but not act on it. Report the draft to the user and let them send it."
        )

    def validate(self, known: Collection[str]) -> None:
        """Fail closed on a name that is not a tool: a typo must not widen a list.

        ``known`` is what the server has registered (``registered_tool_names``).
        """
        for var, names in ((ALLOW_TOOLS_ENV, self.allow), (DENY_TOOLS_ENV, self.deny)):
            unknown = sorted(names - set(known))
            if unknown:
                raise ValueError(
                    f"{var} lists unknown tool(s): {', '.join(unknown)}. Valid names: {', '.join(sorted(known))}"
                )

    def summary(self, removed: Sequence[str] | None = None) -> str:
        """One line for the startup log; ``removed`` comes from apply_tool_policy."""
        parts: list[str] = []
        parts.append(f"{READ_ONLY_ENV}=1: read-only" if self.read_only else f"{READ_ONLY_ENV} unset")
        if self.allow:
            parts.append(f"{ALLOW_TOOLS_ENV}: {', '.join(sorted(self.allow))}")
        if self.deny:
            parts.append(f"{DENY_TOOLS_ENV}: {', '.join(sorted(self.deny))}")
        state = "; ".join(parts)
        if removed is None:
            removed = self.blocked()
        if not removed:
            return f"Tool policy — {state}; every tool enabled"
        return f"Tool policy — {state}; {len(removed)} tool(s) hidden ({', '.join(removed)})"


def load_tool_policy(env: Mapping[str, str] | None = None) -> ToolPolicy:
    """Build the policy from the environment. Raises ValueError on a bad value."""
    source: Mapping[str, str] = os.environ if env is None else env
    return ToolPolicy(
        read_only=parse_bool_env(source.get(READ_ONLY_ENV), READ_ONLY_ENV),
        allow=parse_tool_list(source.get(ALLOW_TOOLS_ENV)),
        deny=parse_tool_list(source.get(DENY_TOOLS_ENV)),
    )


_active: ToolPolicy | None = None


def set_active_policy(policy: ToolPolicy | None) -> None:
    """Install the policy the call-time guard checks (None = resolve from env again)."""
    global _active
    _active = policy


def active_policy() -> ToolPolicy:
    """The policy in force, resolved from the environment on first use.

    ``__main__`` installs the parsed policy at startup and exits on a bad value;
    this fallback matters for an in-process import (tests, embedding), where a
    value we cannot parse means read-only — fail closed.
    """
    global _active
    if _active is None:
        try:
            _active = load_tool_policy()
        except ValueError:
            _active = ToolPolicy(read_only=True)
    return _active


def offers_download() -> bool:
    """Whether the active policy still offers ``download_media``.

    The question ``read_media``, ``transcribe_audio`` and the ingest worker ask
    before fetching a file the store does not have: that fetch is
    ``download_media`` under another name, so a deployment that took the tool
    away must not get it back through a tool it kept (issue #350).
    """
    return active_policy().allows(DOWNLOAD_TOOL)


def download_denied(caller: str) -> ToolError:
    """The ``denied`` error for an implicit fetch the policy does not allow.

    Returned rather than raised, so the caller decides where the refusal sits.
    It names ``download_media`` and the list that removed it: ``caller`` is
    still enabled, so an agent reading only "denied" would take the refusal for
    a bug in the tool it actually called.
    """
    return ToolError(
        "denied",
        f"{caller} would have to fetch this file from WhatsApp first, and "
        f"{active_policy().denial_message(DOWNLOAD_TOOL)}. Media already in the store is still readable.",
    )


def mutating_tool(fn: Callable[..., Any]) -> Callable[..., Any]:
    """Mark an MCP tool as having an external side effect.

    Registers the name as mutating and refuses the call when the active policy
    blocks it. Goes *below* ``@tool_errors`` so the refusal is returned as the
    standard ``{"error": {"code": "denied", ...}}`` envelope::

        @mcp.tool()
        @tool_errors
        @mutating_tool
        def send_message(...): ...
    """
    _MUTATING.add(fn.__name__)

    @functools.wraps(fn)
    def wrapper(*args: Any, **kwargs: Any) -> Any:
        policy = active_policy()
        if not policy.allows(fn.__name__):
            raise ToolError("denied", policy.denial_message(fn.__name__))
        return fn(*args, **kwargs)

    return wrapper


def mutating_tools() -> frozenset[str]:
    """Every tool marked with @mutating_tool (empty until main.py is imported)."""
    return frozenset(_MUTATING)


class _ToolServer(Protocol):
    """The slice of MCPServer this module uses (tests pass a fake)."""

    def remove_tool(self, name: str) -> None: ...


def registered_tool_names(server: Any) -> frozenset[str]:
    """Names currently offered by the server (SDK v2 keeps no public accessor)."""
    return frozenset(tool.name for tool in server._tool_manager.list_tools())


def apply_tool_policy(server: _ToolServer, policy: ToolPolicy) -> list[str]:
    """Unregister every blocked tool so it never reaches ``tools/list``.

    Returns the names removed, for the startup log.
    """
    registered = registered_tool_names(server)
    removed = sorted(name for name in registered if not policy.allows(name))
    for name in removed:
        server.remove_tool(name)
    return removed
