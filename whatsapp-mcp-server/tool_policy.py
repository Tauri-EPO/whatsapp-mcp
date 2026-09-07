"""Operation-level access control (read-only mode).

``WHATSAPP_ALLOWED_CHATS`` (chat_policy.py) restricts *which chats* an agent may
touch. This module restricts *what it may do*. ``WHATSAPP_READ_ONLY=1`` turns the
server into a read-and-draft deployment: every tool with an external side effect
is removed from ``tools/list`` — so the model never sees it — and refuses with the
standard ``denied`` envelope if it is called anyway.

That is the right default whenever the agent reads attacker-controlled text
(any group, any forwarded message): a prompt injection can then ask for a send,
but there is nothing to call. The bridge enforces the same variable on its
mutating REST endpoints (403, whatsapp-bridge/read_only.go) as a second line of
defence, so a bug or a bypass on this side still cannot reach WhatsApp.

Which tools count as mutating is not a list maintained by hand: the
``@mutating_tool`` decorator in main.py registers each one, so a new tool is
either decorated (and covered) or it is not (and stays readable).

Deliberately *not* mutating:

- ``download_media`` / ``transcribe_audio`` — fetch and read; they only write to
  the local media cache.
- ``annotate_media`` — writes notes.db, which is local MCP-owned state, never
  WhatsApp. A read-only assistant still needs somewhere to keep its own notes.

Deliberately mutating even though it reads:

- ``get_group_invite_link`` — ``reset=True`` revokes the current link, and a link
  is group access that can be leaked. Blocked wholesale rather than per-argument.
"""

from __future__ import annotations

import functools
import os
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from typing import Any, Protocol

from errors import ToolError

READ_ONLY_ENV = "WHATSAPP_READ_ONLY"

_TRUE = ("1", "true", "yes", "on")
_FALSE = ("0", "false", "no", "off")

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


@dataclass(frozen=True)
class ToolPolicy:
    """Which tools this process offers. ``read_only`` blocks every mutating tool."""

    read_only: bool = False

    def allows(self, name: str) -> bool:
        return not (self.read_only and name in _MUTATING)

    def blocked(self) -> tuple[str, ...]:
        return tuple(sorted(name for name in _MUTATING if not self.allows(name)))

    def denial_message(self, name: str) -> str:
        return (
            f"{name} is disabled: {READ_ONLY_ENV} is set, so this server may read WhatsApp "
            f"but not act on it. Report the draft to the user and let them send it."
        )

    def summary(self) -> str:
        """One line for the startup log."""
        if not self.read_only:
            return f"{READ_ONLY_ENV} unset: every tool enabled"
        blocked = self.blocked()
        return f"{READ_ONLY_ENV}=1: read-only, {len(blocked)} mutating tool(s) hidden ({', '.join(blocked)})"


def load_tool_policy(env: Mapping[str, str] | None = None) -> ToolPolicy:
    """Build the policy from the environment. Raises ValueError on a bad value."""
    source: Mapping[str, str] = os.environ if env is None else env
    return ToolPolicy(read_only=parse_bool_env(source.get(READ_ONLY_ENV), READ_ONLY_ENV))


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
    removed = [name for name in policy.blocked() if name in registered]
    for name in removed:
        server.remove_tool(name)
    return removed
