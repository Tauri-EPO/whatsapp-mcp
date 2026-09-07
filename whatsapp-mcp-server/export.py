"""Bulk export of the archive to a file on disk (issue #227).

`export_messages` streams straight from SQLite to NDJSON and returns a summary
— path, count, bounds, size — never the rows. Sibling to `download_media`,
which also hands back a path instead of bytes: an archive is far too big to
travel through a model's context, and paging it there is what
`WHATSAPP_EXPORT_DIR` exists to avoid.

Security: every destination is resolved under that directory and refused
otherwise (`..`, absolute paths elsewhere, symlinks pointing out). The chat
allow-list applies exactly as it does to `list_messages`.
"""

from __future__ import annotations

import json
import logging
import os
import sqlite3
from datetime import UTC, datetime
from typing import Any

import whatsapp
from errors import ToolError
from whatsapp import MESSAGE_COLUMNS, MessageFilters

logger = logging.getLogger("whatsapp_mcp")

EXPORT_FORMATS = ("ndjson",)

# Rows are converted and written in batches, so memory stays flat no matter how
# many messages match; the batch also lets one notes query cover 1000 rows.
EXPORT_BATCH = 1000

# Every key msg_to_dict can produce. tests/test_export_messages.py asserts this
# stays in sync with the real conversion, so `fields` cannot silently rot.
EXPORT_FIELDS = (
    "id",
    "timestamp",
    "sender_jid",
    "sender_phone",
    "sender_name",
    "sender_display",
    "content",
    "is_from_me",
    "chat_jid",
    "chat_name",
    "media_type",
    "filename",
    "target_message_id",
    "reaction_to_message_id",
    "poll_message_id",
    "quoted_message_id",
    "deleted_at",
    "view_once",
    "bytes",
    "sha256",
    "notes",
)


def export_dir() -> str:
    """Where exports are written: WHATSAPP_EXPORT_DIR, else <store>/exports."""
    configured = (os.getenv("WHATSAPP_EXPORT_DIR") or "").strip()
    if configured:
        return os.path.abspath(os.path.expanduser(configured))
    store = os.path.dirname(os.path.abspath(whatsapp.MESSAGES_DB_PATH))
    return os.path.join(store, "exports")


def _default_name(chat_jid: str | None) -> str:
    stamp = datetime.now(UTC).strftime("%Y%m%dT%H%M%SZ")
    who = chat_jid.split("@", 1)[0] if chat_jid else "all"
    safe = "".join(c if c.isalnum() or c in "-_" else "-" for c in who)
    return f"messages-{safe}-{stamp}.ndjson"


def resolve_export_path(out_path: str | None, chat_jid: str | None = None) -> str:
    """Absolute destination inside the export directory, or ToolError('denied').

    The name is joined onto the export root and the *resolved* result must still
    live under it, which rejects `..`, an absolute path elsewhere, and symlinks
    inside the directory that point out of it.
    """
    root = export_dir()
    try:
        os.makedirs(root, exist_ok=True)
    except OSError as exc:
        raise ToolError(
            "internal",
            f"export directory {root} is not usable ({exc}); set WHATSAPP_EXPORT_DIR to a writable path",
        ) from exc
    root_real = os.path.realpath(root)

    candidate = (out_path or "").strip() or _default_name(chat_jid)
    target = os.path.realpath(os.path.join(root_real, candidate))
    try:
        contained = os.path.commonpath([root_real, target]) == root_real
    except ValueError:  # different drives on Windows
        contained = False
    if not contained or target == root_real:
        raise ToolError(
            "denied",
            f"out_path must stay inside the export directory ({root_real}); {candidate!r} resolves outside it",
        )
    if os.path.isdir(target):
        raise ToolError("invalid_argument", f"{candidate!r} is a directory")
    os.makedirs(os.path.dirname(target), exist_ok=True)
    return target


def export_messages(
    after: str | None = None,
    before: str | None = None,
    chat_jid: str | None = None,
    out_path: str | None = None,
    format: str = "ndjson",  # noqa: A002 - the tool argument is named `format`
    fields: list[str] | None = None,
    sender_phone_number: str | None = None,
    from_me: bool | None = None,
    has_media: bool | None = None,
    media_type: str | None = None,
    exclude_groups: bool = False,
    include_deleted: bool = True,
) -> dict[str, Any]:
    """Stream matching messages to a file and return only a summary."""
    if format not in EXPORT_FORMATS:
        raise ToolError("invalid_argument", f"format must be one of {', '.join(EXPORT_FORMATS)}")
    if fields is not None:
        unknown = [f for f in fields if f not in EXPORT_FIELDS]
        if unknown:
            raise ToolError("invalid_argument", f"unknown fields: {', '.join(sorted(unknown))}")
        if not fields:
            raise ToolError("invalid_argument", "fields must name at least one column")
    if chat_jid:
        whatsapp._require_allowed(chat_jid)

    target = resolve_export_path(out_path, chat_jid)
    partial = f"{target}.part"

    count = 0
    first_ts: str | None = None
    last_ts: str | None = None
    try:
        conn = whatsapp._connect_messages_db()
        try:
            cur = conn.cursor()
            clauses, params = MessageFilters(
                after=after,
                before=before,
                sender_phone_number=sender_phone_number,
                chat_jid=chat_jid,
                from_me=from_me,
                has_media=has_media,
                media_type=media_type,
                exclude_groups=exclude_groups,
                include_deleted=include_deleted,
            ).build(cur)
            where = f"WHERE {' AND '.join(clauses)}" if clauses else ""
            cur.execute(
                f"SELECT {MESSAGE_COLUMNS} FROM messages JOIN chats ON messages.chat_jid = chats.jid "
                f"{where} ORDER BY messages.timestamp ASC, messages.id ASC",
                tuple(params),
            )
            # Batched, never fetchall(): a 100k-message export holds one batch
            # in memory at a time.
            with open(partial, "w", encoding="utf-8", newline="\n") as handle:
                while rows := cur.fetchmany(EXPORT_BATCH):
                    messages = [whatsapp._row_to_message(row) for row in rows]
                    notes = whatsapp.fetch_media_notes(messages)
                    for message in messages:
                        record = whatsapp.msg_to_dict(message, notes=notes)
                        if fields is not None:
                            record = {k: v for k, v in record.items() if k in fields}
                        handle.write(json.dumps(record, ensure_ascii=False) + "\n")
                        count += 1
                        last_ts = message.timestamp.isoformat()
                        if first_ts is None:
                            first_ts = last_ts
        finally:
            conn.close()
    except sqlite3.Error as exc:
        _discard(partial)
        logger.error("Database error: %s", exc)
        raise ToolError("internal", f"database error: {exc}") from exc
    except OSError as exc:
        _discard(partial)
        raise ToolError("internal", f"could not write {target}: {exc}") from exc

    os.replace(partial, target)
    return {
        "path": target,
        "count": count,
        "first_timestamp": first_ts,
        "last_timestamp": last_ts,
        "bytes": os.path.getsize(target),
    }


def _discard(path: str) -> None:
    try:
        os.unlink(path)
    except OSError:
        pass
