"""One WhatsApp attachment, addressable as an MCP resource.

``read_media`` inlines the bytes into a tool result: the agent asks, and pays
for the whole file in the same round trip. A resource is the other half of that
deal. ``list_media`` now hands back a ``resource_link`` per row — URI, filename,
MIME type, size — and the client fetches the bytes with ``resources/read``, for
the rows it decides to open, when it decides to open them (issue #367).

The URI is ``whatsapp://media/<chat_jid>/<message_id>``, the same identity the
``EmbeddedResource`` blocks of ``read_media`` carry, so a client can match a
block it already holds to the row it came from. There is no path in it: both
halves are looked up in ``messages.db`` and the bytes come from the cache entry
of *that* message, so a URI is not a way to name a file.

A resource read is the same read as ``read_media`` through the same gates in the
same order (``media_read.resolve_media``): the chat allow-list
(``WHATSAPP_ALLOWED_CHATS``), the row — a message carrying no media is not a
resource — the implicit-download policy of issue #350, the proof that the path
resolves inside that chat's own directory, and the size cap for the resolved
type. Reads are allowed in read-only mode, here as everywhere else.

Two things are not the SDK's ``@mcp.resource`` decorator, on purpose:

* a template's MIME type is fixed at registration
  (``ResourceTemplate.mime_type`` answers for every URI it matches), and the
  whole point of a media resource is that a PDF says ``application/pdf`` and a
  JPEG says ``image/jpeg``;
* the read has to happen off the event loop, because an uncached file is
  fetched from WhatsApp first.

So :class:`MediaResourceServer` serves the scheme itself and advertises the one
template it answers for. Everything the SDK would have registered normally
still works: any URI that is not ours goes straight to ``super()``.
"""

from __future__ import annotations

from collections.abc import Sequence
from typing import Any
from urllib.parse import unquote

import anyio.to_thread
from mcp.server.lowlevel.helper_types import ReadResourceContents
from mcp.server.mcpserver.context import Context
from mcp.server.mcpserver.exceptions import ResourceError, ResourceNotFoundError
from mcp_types import ResourceTemplate
from pydantic import AnyUrl

import media_read
from errors import ToolError
from strict_args import StrictArgumentServer

MEDIA_URI_TEMPLATE = f"{media_read.MEDIA_URI_PREFIX}{{chat_jid}}/{{message_id}}"

# What ``resources/templates/list`` says about the scheme. The description is
# read by a model, so it says where the two halves come from rather than what
# the module does.
MEDIA_TEMPLATE = ResourceTemplate(
    uri_template=MEDIA_URI_TEMPLATE,
    name="whatsapp_media",
    title="WhatsApp media",
    description=(
        "The bytes of one WhatsApp attachment. chat_jid and message_id are the ones list_media, "
        "list_messages and read_media return; the resource_link on a list_media row is this URI "
        "already built. Same limits and same permissions as read_media."
    ),
)


def parse_media_uri(uri: str) -> tuple[str, str] | None:
    """``(chat_jid, message_id)`` for a media URI, or None when it is not one.

    Exactly two segments after the prefix: ``whatsapp://media/a/b/c`` is not a
    chat whose JID contains a slash, it is a URI this server does not serve.
    """
    if not uri.startswith(media_read.MEDIA_URI_PREFIX):
        return None
    parts = uri[len(media_read.MEDIA_URI_PREFIX) :].split("/")
    if len(parts) != 2 or not all(parts):
        return None
    return unquote(parts[0]), unquote(parts[1])


def _as_resource_error(exc: ToolError) -> ResourceError:
    """The tool envelope as the SDK's resource failure.

    ``resources/read`` has no envelope of its own — a failure is a JSON-RPC
    error with a message — so the code travels in the text. It is the only
    place an agent can read *why* a file it can see in ``list_media`` did not
    come back.
    """
    kind = ResourceNotFoundError if exc.code == "not_found" else ResourceError
    return kind(f"{exc.code}: {exc.message}")


def read_media_resource(chat_jid: str, message_id: str) -> ReadResourceContents:
    """The bytes of one message's media, with the type they really are.

    Text comes back as text (``TextResourceContents`` on the wire) and
    everything else as bytes (``BlobResourceContents``): a client asked to
    render a resource should not have to un-base64 a CSV first. The text is
    *not* wrapped in the ``<untrusted>`` delimiters ``read_media`` uses — a
    resource is the file, byte for byte, and the boundary belongs on the tool
    result the model reads.
    """
    try:
        found = media_read.resolve_media(chat_jid, message_id, caller="resources/read")
        data = media_read.read_capped(found.path, found.size)
    except ToolError as exc:
        raise _as_resource_error(exc) from exc
    if media_read.is_text_mime(found.mime):
        # replace, not strict: a mislabelled .txt degrades to readable text
        # rather than failing the read, exactly as in read_media.
        return ReadResourceContents(content=data.decode("utf-8", errors="replace"), mime_type=found.mime)
    return ReadResourceContents(content=data, mime_type=found.mime)


def attach_resource_links(items: Sequence[dict[str, Any]]) -> None:
    """Put a ``resource_link`` on each ``list_media`` row, in place.

    Built from the row itself — no extra query and no stat — so a page of 200
    costs nothing. ``size`` is the cached file when the bytes are here and
    WhatsApp's reported length otherwise; a row with neither carries no size,
    which is what ``ResourceLink.size`` being optional is for.
    """
    for item in items:
        cached_file = item.get("cached_file")
        item["resource_link"] = media_read.resource_link(
            item["chat_jid"],
            item["message_id"],
            item.get("filename") or cached_file or f"{item.get('media_type') or 'media'}-{item['message_id']}",
            media_read.guess_mime(item.get("media_type") or "", item.get("filename"), cached_file or ""),
            item.get("cached_bytes") or item.get("bytes"),
        )


class MediaResourceServer(StrictArgumentServer):
    """``StrictArgumentServer`` that also answers ``whatsapp://media/...``.

    See the module docstring for why the scheme is served here instead of with
    ``@mcp.resource``.
    """

    async def list_resource_templates(self) -> list[ResourceTemplate]:
        return [*await super().list_resource_templates(), MEDIA_TEMPLATE]

    async def read_resource(
        self, uri: AnyUrl | str, context: Context[Any, Any] | None = None
    ) -> Any:  # Iterable[ReadResourceContents] | InputRequiredResult
        reference = parse_media_uri(str(uri))
        if reference is None:
            return await super().read_resource(uri, context)
        # In a worker thread: the file may not be cached yet, and fetching it
        # is an HTTP round trip to the bridge that can take as long as
        # WHATSAPP_BRIDGE_TIMEOUT_S allows. Blocking the event loop for that
        # would stall every other client on this endpoint.
        return [await anyio.to_thread.run_sync(read_media_resource, *reference)]
