# syntax=docker/dockerfile:1
#
# Go bridge image. Build context: whatsapp-bridge/
#
#   docker build -t whatsapp-mcp-bridge ./whatsapp-bridge
#
# The bridge keeps its WhatsApp session, message DB, downloaded media and the
# generated REST token under /app/store. Mount a volume there or you will have
# to re-pair on every container recreation. See docs/DOCKER.md.

FROM golang:1.26-alpine AS build

# go-sqlite3 needs cgo.
RUN apk add --no-cache gcc musl-dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# sqlite_fts5: compiles the FTS5 module into go-sqlite3 so the bridge can keep
# the full-text index over messages.content (see fts.go).
RUN CGO_ENABLED=1 go build -tags sqlite_fts5 -trimpath -ldflags="-s -w" -o /out/whatsapp-bridge .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 1000 -h /app app

WORKDIR /app
COPY --from=build /out/whatsapp-bridge /usr/local/bin/whatsapp-bridge
COPY docker-healthcheck.sh /usr/local/bin/bridge-healthcheck
RUN chmod +x /usr/local/bin/bridge-healthcheck \
    && mkdir -p /app/store /app/outbox \
    && chown -R app:app /app

USER app

# Both directories are meant to be mounted:
#   /app/store  – session + messages.db + media + .bridge-token (persistent!)
#   /app/outbox – files the MCP client wants the bridge to send (send_file)
VOLUME ["/app/store"]

ENV WHATSAPP_BRIDGE_PORT=8080 \
    WHATSAPP_STORE_DIR=/app/store \
    WHATSAPP_MEDIA_ROOTS=/app/outbox

EXPOSE 8080

# Healthy only while the WhatsApp connection is up (GET /api/health -> 200).
# During first-run QR pairing the container therefore shows "unhealthy" until
# the code is scanned; that is expected.
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
    CMD ["/usr/local/bin/bridge-healthcheck"]

ENTRYPOINT ["/usr/local/bin/whatsapp-bridge"]
