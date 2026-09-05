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
#
# Two stages: the venv is resolved with uv in a builder and copied over, so
# the runtime carries neither uv nor its cache. ffmpeg comes as one static
# binary (multi-arch) instead of Debian's ffmpeg package and its ~450 MB of
# codec and X11 libraries.

FROM python:3.11-slim AS build

COPY --from=ghcr.io/astral-sh/uv:0.10.10 /uv /bin/uv

WORKDIR /app
ENV UV_NO_CACHE=1 \
    UV_COMPILE_BYTECODE=1 \
    UV_LINK_MODE=copy

# Dependencies first so source edits don't invalidate the layer.
COPY pyproject.toml uv.lock ./
RUN uv sync --frozen --no-dev --no-install-project


FROM python:3.11-slim

RUN useradd --uid 1000 --create-home --home-dir /app --shell /usr/sbin/nologin app \
    && mkdir -p /app/store /app/outbox \
    && chown app:app /app/store /app/outbox \
    # The runtime only needs the interpreter: the app lives in /app/.venv (built
    # by uv) and nothing installs packages here. Dropping the base image's pip,
    # setuptools (with its vendored wheel/jaraco.*) and ensurepip removes code
    # Trivy keeps flagging and that an attacker could use to fetch more.
    && rm -rf /usr/local/lib/python3.11/site-packages/pip* \
              /usr/local/lib/python3.11/site-packages/setuptools* \
              /usr/local/lib/python3.11/site-packages/wheel* \
              /usr/local/lib/python3.11/site-packages/_distutils_hack \
              /usr/local/lib/python3.11/site-packages/distutils-precedence.pth \
              /usr/local/lib/python3.11/ensurepip \
              /usr/local/bin/pip*

# ffmpeg: send_audio_message converts arbitrary audio to Opus/OGG voice notes,
# transcribe_audio resamples to 16 kHz WAV. Static build, linux/amd64 + arm64.
COPY --from=mwader/static-ffmpeg:7.1 /ffmpeg /usr/local/bin/ffmpeg

WORKDIR /app
COPY --from=build --chown=app:app /app/.venv /app/.venv
COPY --chown=app:app *.py ./

# Build identity reported in the MCP initialize response and startup log.
ARG GIT_SHA=unknown
ARG VERSION=dev
ENV WHATSAPP_MCP_VERSION=${VERSION}+${GIT_SHA}

USER app

ENV PATH="/app/.venv/bin:$PATH" \
    PYTHONUNBUFFERED=1 \
    WHATSAPP_MCP_TRANSPORT=http \
    WHATSAPP_MCP_HOST=0.0.0.0 \
    WHATSAPP_MCP_PORT=8000 \
    WHATSAPP_STORE_DIR=/app/store \
    WHATSAPP_API_URL=http://127.0.0.1:8080/api

EXPOSE 8000

# Any HTTP answer on /mcp (even 4xx for a session-less GET) means the ASGI
# server is up; only a connection failure marks the container unhealthy.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD ["python", "-c", "import os,urllib.request,urllib.error;\nu='http://127.0.0.1:%s/mcp'%os.environ.get('WHATSAPP_MCP_PORT','8000')\ntry:\n    urllib.request.urlopen(u,timeout=4)\nexcept urllib.error.HTTPError:\n    pass"]

CMD ["python", "main.py"]
