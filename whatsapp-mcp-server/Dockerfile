# syntax=docker/dockerfile:1
#
# Python MCP server image, configured for the streamable-HTTP transport.
# Build context: whatsapp-mcp-server/
#
#   docker build -t whatsapp-mcp-server ./whatsapp-mcp-server
#
# The server reads messages.db directly and calls the bridge REST API, so it
# must see the bridge's /app/store volume and reach the bridge over the
# network. docker-compose.yml wires both up. See docs/DOCKER.md.

FROM python:3.11-slim

COPY --from=ghcr.io/astral-sh/uv:0.10.10 /uv /uvx /bin/

# ffmpeg: send_audio_message converts arbitrary audio to Opus/OGG voice notes.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 1000 --create-home --home-dir /app --shell /usr/sbin/nologin app

WORKDIR /app

# Dependencies first so source edits don't invalidate the layer.
COPY pyproject.toml uv.lock ./
RUN uv sync --frozen --no-dev

COPY main.py whatsapp.py audio.py mcp_config.py parent_watchdog.py ./
RUN mkdir -p /app/store /app/outbox && chown -R app:app /app

USER app

ENV PATH="/app/.venv/bin:$PATH" \
    PYTHONUNBUFFERED=1 \
    WHATSAPP_MCP_TRANSPORT=http \
    WHATSAPP_MCP_HOST=0.0.0.0 \
    WHATSAPP_MCP_PORT=8000 \
    WHATSAPP_DB_PATH=/app/store/messages.db \
    WHATSMEOW_DB_PATH=/app/store/whatsapp.db \
    WHATSAPP_API_URL=http://127.0.0.1:8080/api

EXPOSE 8000

# Any HTTP answer on /mcp (even 4xx for a session-less GET) means the ASGI
# server is up; only a connection failure marks the container unhealthy.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD ["python", "-c", "import os,urllib.request,urllib.error;\nu='http://127.0.0.1:%s/mcp'%os.environ.get('WHATSAPP_MCP_PORT','8000')\ntry:\n    urllib.request.urlopen(u,timeout=4)\nexcept urllib.error.HTTPError:\n    pass"]

CMD ["python", "main.py"]
