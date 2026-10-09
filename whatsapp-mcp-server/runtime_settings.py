"""Read bridge-owned runtime overrides through the read-only message connection."""

from __future__ import annotations

import json
import os
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from typing import Any

INGEST_CHATS_ENV = "TRANSCRIBE_ON_INGEST_CHATS"
_startup_env: Mapping[str, str] | None = None


def parse_ingest_chats(raw: str | None) -> str:
    value = (raw or "").strip() or "all"
    if value not in {"all", "direct"}:
        raise ValueError(f"{INGEST_CHATS_ENV}: expected all or direct")
    return value


def _tool_env(raw: str | None) -> list[str]:
    from tool_policy import parse_tool_list

    return sorted(parse_tool_list(raw))


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


@dataclass(frozen=True)
class SettingDefinition:
    env: str
    default: Any
    parse_env: Callable[[str | None], Any]
    parse_value: Callable[[Any], Any]


# Extend alongside the bridge registry when another consumer introduces a key.
DEFINITIONS = {
    "tools.allow": SettingDefinition("WHATSAPP_ALLOW_TOOLS", [], _tool_env, _tool_value),
    "tools.deny": SettingDefinition("WHATSAPP_DENY_TOOLS", [], _tool_env, _tool_value),
    "transcription.ingest_chats": SettingDefinition(INGEST_CHATS_ENV, "all", parse_ingest_chats, _ingest_value),
}


def capture_environment() -> None:
    global _startup_env
    _startup_env = dict(os.environ)
    for definition in DEFINITIONS.values():
        definition.parse_env(_startup_env.get(definition.env))


def snapshot(env: Mapping[str, str] | None = None, *, require_store: bool = False) -> dict[str, Any]:
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
    if not require_store and not os.path.exists(whatsapp.MESSAGES_DB_PATH):
        return {"version": version, "settings": settings}
    conn = whatsapp._connect_messages_db()
    try:
        if not conn.execute("SELECT 1 FROM sqlite_master WHERE name='runtime_settings' AND type='table'").fetchone():
            return {"version": version, "settings": settings}
        rows = conn.execute("SELECT key,value,version FROM runtime_settings").fetchall()
    finally:
        conn.close()
    for key, raw, row_version in rows:
        version = max(version, int(row_version))
        if key not in DEFINITIONS:
            continue
        value = json.loads(raw)
        if value is None:
            continue
        value = DEFINITIONS[key].parse_value(value)
        settings[key] = {"value": value, "source": "runtime"}
    return {"version": version, "settings": settings}


def ingest_setting() -> dict[str, Any]:
    return snapshot()["settings"]["transcription.ingest_chats"]


def ingest_chat_clause(column: str) -> str:
    """SQL predicate shared by the worker and coverage; column is internal."""
    if ingest_setting()["value"] == "all":
        return "1=1"
    return f"({column} LIKE '%@s.whatsapp.net' OR {column} LIKE '%@lid')"
