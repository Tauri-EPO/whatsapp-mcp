"""Read bridge-owned runtime overrides through the read-only message connection."""

from __future__ import annotations

import json
import logging
import math
import os
import re
import threading
from collections.abc import Callable, Mapping
from contextlib import contextmanager
from contextvars import ContextVar
from dataclasses import dataclass
from typing import TYPE_CHECKING, Any

if TYPE_CHECKING:
    from chat_policy import ChatPolicy

INGEST_CHATS_ENV = "TRANSCRIBE_ON_INGEST_CHATS"
CAP_ENV = "TRANSCRIBE_MONTHLY_MAX_MINUTES"
CAP_SCOPE_ENV = "TRANSCRIBE_CAP_SCOPE"
_startup_env: Mapping[str, str] | None = None
_warned: dict[str, int] = {}
_warn_lock = threading.Lock()
_operation: ContextVar[dict[str, Any] | None] = ContextVar("runtime_settings_operation", default=None)


@contextmanager
def settings_operation():
    token = _operation.set({}) if _operation.get() is None else None
    try:
        yield
    finally:
        if token is not None:
            _operation.reset(token)


def _warn(key: str, version: int) -> None:
    with _warn_lock:
        if _warned.get(key) == version:
            return
        _warned[key] = version
    logging.getLogger("whatsapp_mcp").warning(
        "Saved runtime setting key=%s version=%s is incompatible; applying safe deploy fallback", key, version
    )


def parse_ingest_chats(raw: str | None) -> str:
    value = (raw or "").strip() or "all"
    if value not in {"all", "direct"}:
        raise ValueError(f"{INGEST_CHATS_ENV}: expected all or direct")
    return value


def _tool_env(raw: str | None) -> list[str]:
    from tool_policy import parse_tool_list

    return sorted(parse_tool_list(raw))


def _chat_env(raw: str | None) -> list[str]:
    from chat_policy import ChatPolicy, policy_entries

    return policy_entries(ChatPolicy.from_entries((raw or "").split(",")))


# A digits-only number, a JID or a server wildcard: an entry that can never
# match is refused. Keep identical to chatEntryPattern in whatsapp-bridge/chat_policy.go.
CHAT_ENTRY = re.compile(r"(?:[0-9]+|(?:\*|[0-9A-Za-z_:-]+)@[0-9A-Za-z.-]+)")


def _chat_value(value: Any) -> list[str]:
    if (
        not isinstance(value, list)
        or not value
        or any(not isinstance(entry, str) or not CHAT_ENTRY.fullmatch(entry.strip()) for entry in value)
    ):
        raise ValueError("expected a nonempty array of chat entries")
    return _chat_env(",".join(value))


def _tool_value(value: Any) -> list[str]:
    if not isinstance(value, list) or any(
        not isinstance(name, str) or not name or name != name.strip() or "," in name for name in value
    ):
        raise ValueError("expected an array of tool names")
    return value


def _ingest_value(value: Any) -> str:
    if not isinstance(value, str) or not value.strip():
        raise ValueError("expected all or direct")
    return parse_ingest_chats(value)


def parse_cap(value: Any) -> float | None:
    if isinstance(value, str):
        value = value.strip()
    if value is None or value == "":
        return None
    if isinstance(value, bool):
        raise ValueError(f"{CAP_ENV}: expected finite non-negative minutes")
    if isinstance(value, str) and not re.fullmatch(r"[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?", value):
        raise ValueError(f"{CAP_ENV}: expected decimal minutes")
    number = float(value)
    if not math.isfinite(number) or not 0 <= number <= 525600:
        raise ValueError(f"{CAP_ENV}: expected minutes in 0..525600")
    return number


def _cap_value(value: Any) -> float:
    if not isinstance(value, (int, float)) or isinstance(value, bool):
        raise ValueError("expected non-negative minutes")
    parsed = parse_cap(value)
    assert parsed is not None
    return parsed


def parse_cap_scope(raw: str | None) -> str:
    value = (raw or "").strip() or "ingest"
    if value not in {"ingest", "all"}:
        raise ValueError(f"{CAP_SCOPE_ENV}: expected ingest or all")
    return value


def _scope_value(value: Any) -> str:
    if not isinstance(value, str) or value not in {"ingest", "all"}:
        raise ValueError("expected ingest or all")
    return value


@dataclass(frozen=True)
class SettingDefinition:
    env: str
    default: Any
    parse_env: Callable[[str | None], Any]
    parse_value: Callable[[Any], Any]


# Extend alongside the bridge registry when another consumer introduces a key.
DEFINITIONS = {
    "chats.allowed": SettingDefinition("WHATSAPP_ALLOWED_CHATS", [], _chat_env, _chat_value),
    "tools.allow": SettingDefinition("WHATSAPP_ALLOW_TOOLS", [], _tool_env, _tool_value),
    "tools.deny": SettingDefinition("WHATSAPP_DENY_TOOLS", [], _tool_env, _tool_value),
    "transcription.ingest_chats": SettingDefinition(INGEST_CHATS_ENV, "all", parse_ingest_chats, _ingest_value),
    "transcription.monthly_max_minutes": SettingDefinition(CAP_ENV, None, parse_cap, _cap_value),
    "transcription.cap_scope": SettingDefinition(CAP_SCOPE_ENV, "ingest", parse_cap_scope, _scope_value),
}


def capture_environment() -> None:
    global _startup_env
    _startup_env = dict(os.environ)
    for definition in DEFINITIONS.values():
        definition.parse_env(_startup_env.get(definition.env))


def _effective_chat_setting(raw_setting, deploy):
    from dataclasses import replace

    from chat_policy import ENV_VAR, ChatPolicy, intersect_policies, policy_entries

    policy = deploy
    if raw_setting["source"] == "runtime":
        runtime = ChatPolicy.from_entries(raw_setting["value"])
        policy = intersect_policies(ChatPolicy(runtime.exact, runtime.servers, True), deploy)
        origin = "runtime chats.allowed" + (f" within {ENV_VAR} (deploy)" if deploy.restricted else "")
        policy = replace(policy, origin=origin)
        return {"value": policy_entries(policy), "source": raw_setting["source"]}, policy
    return dict(raw_setting), policy


class RuntimeSnapshot(dict[str, Any]):
    """One observation, with immutable chat policies cached by captured deploy."""

    def __init__(
        self,
        values: dict[str, Any],
        deploy: ChatPolicy,
        raw_chats: dict[str, Any],
        *,
        root: RuntimeSnapshot | None = None,
    ):
        super().__init__(values)
        self._raw_chats = raw_chats
        setting, self.chat_policy = _effective_chat_setting(raw_chats, deploy)
        self["settings"] = {key: dict(value) for key, value in values["settings"].items()}
        self["settings"]["chats.allowed"] = setting
        self._root = self if root is None else root
        self._views_lock = threading.Lock() if root is None else root._views_lock
        self._views: dict[ChatPolicy, RuntimeSnapshot] = {deploy: self} if root is None else root._views

    def for_deploy(self, deploy: ChatPolicy | None) -> RuntimeSnapshot:
        if deploy is None:
            return self
        with self._views_lock:
            if deploy not in self._views:
                self._views[deploy] = RuntimeSnapshot(dict(self._root), deploy, self._raw_chats, root=self._root)
            return self._views[deploy]


def snapshot(
    env: Mapping[str, str] | None = None,
    *,
    require_store: bool = False,
    timeout_s: float | None = None,
    deploy_policy: ChatPolicy | None = None,
) -> RuntimeSnapshot:
    operation = _operation.get() if env is None else None
    if operation and (not require_store or operation["strict"]):
        return operation["snapshot"].for_deploy(deploy_policy)
    result, verified = _read_snapshot(env, require_store=require_store, timeout_s=timeout_s)
    if operation is not None:
        operation.update(snapshot=result, strict=verified)
    return result.for_deploy(deploy_policy)


def _read_snapshot(
    env: Mapping[str, str] | None = None,
    *,
    require_store: bool = False,
    timeout_s: float | None = None,
) -> tuple[RuntimeSnapshot, bool]:
    """One SQLite snapshot per observation: both tool lists change together.

    Null rows are tombstones, preserving max(version) after the last clear.
    Legacy/missing stores have no overrides. Other database errors propagate:
    callers enforcing permissions must never silently drop a saved restriction.
    """
    import whatsapp

    source = env if env is not None else (_startup_env if _startup_env is not None else os.environ)
    settings: dict[str, dict[str, Any]] = {}
    for key, definition in DEFINITIONS.items():
        raw = source.get(definition.env)
        present = bool(raw and raw.strip())
        value = definition.parse_env(raw) if present else definition.default
        settings[key] = {"value": value, "source": "env" if present else "default"}
    version = 0
    defaults = {key: dict(setting) for key, setting in settings.items()}
    closed_allow = False
    if not require_store and not os.path.exists(whatsapp.MESSAGES_DB_PATH):
        return _apply_floor(settings, defaults, version, closed_allow), False
    conn = whatsapp._connect_messages_db()
    try:
        if timeout_s is not None:
            conn.execute(f"PRAGMA busy_timeout={max(1, int(timeout_s * 1000))}")
        if not conn.execute("SELECT 1 FROM sqlite_master WHERE name='runtime_settings' AND type='table'").fetchone():
            return _apply_floor(settings, defaults, version, closed_allow), True
        rows = conn.execute("SELECT key,value,version FROM runtime_settings").fetchall()
    finally:
        conn.close()
    for key, raw, row_version in rows:
        version = max(version, int(row_version))
        if key not in DEFINITIONS:
            continue
        try:
            value = json.loads(raw)
            if value is None:
                continue
            value = DEFINITIONS[key].parse_value(value)
            if key in {"tools.allow", "tools.deny"}:
                known = _known_tools()
                filtered = sorted(set(value) & known)
                if set(value) - known:
                    _warn(key, row_version)
                if key == "tools.allow" and value and not filtered:
                    closed_allow = True
                value = filtered
        except (ValueError, TypeError):
            _warn(key, row_version)
            if key == "chats.allowed":
                settings[key] = {"value": [], "source": "runtime"}
            if key == "tools.allow":
                closed_allow = not defaults[key]["value"]
            continue
        settings[key] = {"value": value, "source": "runtime"}
    return _apply_floor(settings, defaults, version, closed_allow), True


def _known_tools() -> set[str]:
    import main
    from tool_policy import registered_tool_names

    return set(registered_tool_names(main.mcp))


def _apply_floor(settings, defaults, version, closed_allow):
    from chat_policy import ChatPolicy

    deploy = ChatPolicy.from_entries(defaults["chats.allowed"]["value"])
    raw_chats = dict(settings["chats.allowed"])
    cap_key, scope_key = "transcription.monthly_max_minutes", "transcription.cap_scope"
    ceiling = defaults[cap_key]["value"]
    cap = settings[cap_key]["value"]
    if ceiling is not None and (cap is None or cap > ceiling):
        settings[cap_key] = dict(defaults[cap_key])
    if defaults[scope_key]["value"] == "all":
        settings[scope_key] = dict(defaults[scope_key])
    allow = set(settings["tools.allow"]["value"])
    env_allow = set(defaults["tools.allow"]["value"])
    if env_allow:
        if not allow and not closed_allow:
            allow = env_allow
        else:
            allow &= env_allow
            closed_allow = not allow
    deny = set(settings["tools.deny"]["value"])
    deny_source = (
        "runtime tools.deny" if settings["tools.deny"]["source"] == "runtime" else "WHATSAPP_DENY_TOOLS (deploy)"
    )
    origins = {name: deny_source for name in deny}
    for name in defaults["tools.deny"]["value"]:
        deny.add(name)
        origins[name] = "WHATSAPP_DENY_TOOLS (deploy)"
    if closed_allow and not allow:
        for name in _known_tools():
            deny.add(name)
            origins.setdefault(name, "runtime tools.allow has no readable tools inside the deploy floor")
        settings["tools.deny"]["source"] = "runtime"
    settings["tools.allow"]["value"] = sorted(allow)
    settings["tools.deny"]["value"] = sorted(deny)
    allow_source = "WHATSAPP_ALLOW_TOOLS (deploy)"
    if settings["tools.allow"]["source"] == "runtime":
        allow_source = "runtime tools.allow within WHATSAPP_ALLOW_TOOLS (deploy)"
    return RuntimeSnapshot(
        {"version": version, "settings": settings, "deny_origins": origins, "allow_source": allow_source},
        deploy,
        raw_chats,
    )


def ingest_setting() -> dict[str, Any]:
    return snapshot()["settings"]["transcription.ingest_chats"]


def ingest_chat_clause(column: str) -> str:
    """SQL predicate shared by the worker and coverage; column is internal."""
    if ingest_setting()["value"] == "all":
        return "1=1"
    return f"({column} LIKE '%@s.whatsapp.net' OR {column} LIKE '%@lid')"
