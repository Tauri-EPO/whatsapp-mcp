# AGENTS.md

Single source of truth for working in **Tauri-EPO/whatsapp-mcp** — for AI coding agents (Claude Code, Codex, Cursor…) and for humans using them. `CLAUDE.md` only points here.

Read top to bottom once; afterwards jump to the section you need.

1. [What this repo is](#1-what-this-repo-is)
2. [Fork policy: hard fork, upstream as an idea source](#2-fork-policy)
3. [Architecture](#3-architecture)
4. [The routine: from issue to merged PR](#4-the-routine-from-issue-to-merged-pr)
5. [Local commands and tooling](#5-local-commands-and-tooling)
6. [CI gates](#6-ci-gates)
7. [Environment variables](#7-environment-variables)
8. [Gotchas](#8-gotchas-read-before-editing)
9. [Where to make changes](#9-where-to-make-changes)
10. [Persona](#10-persona-for-ai-agents)
11. [Issues](#11-issues)

---

## 1. What this repo is

A WhatsApp ↔ MCP bridge tuned for an **always-on home server** reached over **Tailscale** by remote MCP clients (an AI bot on another machine, an IDE on a laptop), sharing one WhatsApp account. Two components: a Go bridge (whatsmeow) and a Python MCP server (MCP SDK v2, streamable HTTP or stdio). Deployed with Docker Compose. `README.md` → "About this fork" has the user-facing version of this story and a table of what differs from upstream.

- **Repo:** https://github.com/Tauri-EPO/whatsapp-mcp — remote `origin`. All PRs, issues and `gh` commands target this repo.
- **Default branch:** `main`. `main` is the deployable state; there are no releases or tags here.
- **Lineage (for ideas, not for merging — see §2):**
  - https://github.com/verygoodplugins/whatsapp-mcp — remote `upstream` (also `vgp`). Maintained fork we started from at v0.6.0 (Sept 2026).
  - https://github.com/lharries/whatsapp-mcp — the original project (remote `lharries`; add with `git remote add lharries https://github.com/lharries/whatsapp-mcp.git` if missing).

## 2. Fork policy

**This is a hard fork.** Decided 2026-09-04: the fork is ahead of upstream (SDK v2, auth, allow-list, FTS5, polls, Docker…) and no longer merges `upstream/main`. Consequences:

- **Dependencies move here first.** Dependabot stays on; its PRs are merged once CI is green. Never pin a dependency just because upstream did (`mcp<2` and `cryptography<49` were both dropped).
- **Refactors are allowed.** The current plan is the "fork hardening" epic (issue #64). Keep each PR small (§4) even when the overall change is large.
- **Upstream is a source of ideas and cherry-picks, never a merge target.** When asked to "look upstream", "get ideas from the original", "check what VGP/lharries did", do this:
  1. `scripts/upstream-harvest.sh` — fetches both remotes and prints the bridge/server commits since the last harvest (`.upstream-harvest`) plus their open PRs and issues; `--all` ignores the marks. After reviewing, `scripts/upstream-harvest.sh --mark` records the new heads. (Manual equivalent: `git log --oneline main..upstream/main -- whatsapp-bridge/`; protocol/whatsmeow changes are the most valuable to harvest.)
  2. `gh pr list --repo verygoodplugins/whatsapp-mcp --state all --search "<topic>"` and the same on `lharries/whatsapp-mcp`; read the PR description first — it usually explains the WhatsApp behaviour better than the diff.
  3. Reimplement the delta against our code (`git cherry-pick -x <sha>` only when the patch applies cleanly to files we have not diverged in). Credit the source in the commit body ("Reimplements upstream VGP #NNN"), as every PR in this repo has done so far.
  4. Do not bring upstream's release-please, CHANGELOG or version bumps.
- **whatsmeow protocol drift** is the one thing upstream will keep fixing before us. Monthly routine (first done 2026-09-04):
  1. In `whatsapp-bridge/`: `go get go.mau.fi/whatsmeow@latest && go mod tidy` (inside the `golang:<version>-alpine` container on Windows). If the new version needs a newer Go, bump `go.mod`, the Dockerfile base image, `go-version` in every workflow and the golangci-lint version together — they must agree.
  2. `go vet`/`go test -tags sqlite_fts5 ./...`, `golangci-lint run`, `docker compose build`.
  3. Pair a test store, send/receive one text, one media, one poll; watch the log for new event types whatsmeow now emits.
  4. PR titled `chore(deps): bump whatsmeow to <version>`; commit body lists notable upstream changes.
- `ROADMAP.md` is upstream's. Its "out of scope" list no longer binds this fork; use it only to understand why upstream will not take something.

## 3. Architecture

```
whatsapp-mcp/
├── whatsapp-bridge/            # Go — WhatsApp Web via whatsmeow, REST API, messages.db owner
│   ├── main.go                 # startup and wiring only (flags, env, pairing, signal handling)
│   ├── bridge.go               # Bridge struct: runtime dependencies shared by handlers
│   ├── events.go               # whatsmeow event dispatch, handleMessage, calls, reconnect loop
│   ├── history_sync.go         # handleHistorySync (phone replays at pair time / on demand)
│   ├── content.go              # extract text/quotes/mentions/media/ephemeral from waE2E.Message
│   ├── jid.go                  # phone <-> LID resolution helpers
│   ├── send.go                 # /api/send types, sendWhatsAppMessage, media upload, Ogg Opus analysis
│   ├── media.go                # inbound media download into store/<chat>/
│   ├── rest.go                 # newRESTMux route table, health/ready, HTTP server
│   ├── store.go                # MessageStore: schema, migrations, message/chat/call queries
│   ├── logging.go              # bridgeLog + WHATSAPP_LOG_LEVEL
│   ├── rest_bind.go            # WHATSAPP_BRIDGE_BIND / WHATSAPP_BRIDGE_ALLOWED_HOSTS
│   ├── media_retention.go      # WHATSAPP_MEDIA_AUTODOWNLOAD / _RETENTION_DAYS, store size
│   ├── auth.go                 # bearer token + loopback Host allow-list for /api/*
│   ├── chat_policy.go          # WHATSAPP_ALLOWED_CHATS enforcement on outbound endpoints
│   ├── fts.go                  # FTS5 index over messages.content (needs -tags sqlite_fts5)
│   ├── media_retry.go          # re-download expired CDN media via the sender's phone
│   ├── instance_lock.go        # one bridge per store (flock / LockFileEx)
│   ├── polls.go                # native polls: creation, votes, /api/poll
│   ├── group_members.go        # /api/group/members
│   ├── delete_message.go       # /api/delete (revoke / local delete)
│   ├── history_ondemand.go     # POST /api/history
│   ├── webhook.go              # outbound webhook for inbound messages
│   ├── Dockerfile              # alpine, cgo sqlite, -tags sqlite_fts5
│   └── store/                  # WHATSAPP_STORE_DIR: whatsapp.db, messages.db, media, .bridge-token, .bridge.lock (gitignored)
├── whatsapp-mcp-server/        # Python — MCP tools; reads messages.db, calls bridge REST
│   ├── main.py                 # MCPServer (SDK v2) tool definitions + transport startup
│   ├── whatsapp.py             # SQL queries, bridge HTTP client, dict conversion
│   ├── mcp_config.py           # transport/host/port/allowed-hosts parsing
│   ├── http_auth.py            # WHATSAPP_MCP_TOKEN bearer middleware
│   ├── chat_policy.py          # WHATSAPP_ALLOWED_CHATS for reads and writes
│   ├── transcribe.py           # whisper.cpp backends for transcribe_audio
│   ├── audio.py                # ffmpeg helpers
│   └── Dockerfile              # python:3.11-slim + ffmpeg + uv, http transport
├── docker-compose.yml          # bridge + mcp (+ optional whisper profile) — docs/DOCKER.md
├── docs/DOCKER.md              # pairing, Tailscale, tokens, health, backups
└── .github/workflows/          # ci.yml, security.yml (release workflows are manual-only)
```

Data flow: MCP client → MCP server → reads `messages.db` directly for everything read-only, calls bridge REST (`WHATSAPP_API_URL`, default `http://localhost:8080/api`) for sends, media, group info, polls, deletes → bridge → WhatsApp Web.

Two SQLite databases: `whatsapp.db` (whatsmeow: session, contacts, LID map — opaque) and `messages.db` (ours: `chats`, `messages`, `calls`, `polls`, `poll_votes`, `messages_fts`). The bridge owns the schema; the MCP server only reads.

Compose topology: the `mcp` container joins the bridge's network namespace (`network_mode: service:bridge`), so the bridge keeps its loopback bind and loopback-only Host allow-list; the MCP port is published on the bridge service. An alternative topology is issue #58.

## 4. The routine: from issue to merged PR

This is how every change in this repo has been shipped; follow it unless the user says otherwise.

1. **Start from an issue.** Bugs and features have one. If none exists, open it (§11): one problem per issue, with a "Fix" sketch and acceptance boxes. The epic #64 lists the current plan.
2. **Branch from current `main`:** `git fetch origin && git checkout -b <type>/<slug> origin/main`. Types: `fix`, `feat`, `perf`, `refactor`, `docs`, `ci`, `chore`, `test`.
3. **One concern per PR, small.** Target under ~300 changed lines of code (docs and tests excluded). Split refactors into pure-move PRs. If a change needs another open PR, stack the branch on it, say "Stacked on #N" in the body, and retarget to `main` after that merges.
4. **Tests with the change.** Python: `tests/` (pytest, real SQLite files in `tmp_path`, `monkeypatch` for `requests`/policy/env). Go: table tests, `httptest`, fakes injected as functions (see `group_members.go`, `delete_message.go`, `polls.go`), `newTestMessageStore`. No test may need a paired phone.
5. **Docs in the same PR.** New env var → this file §7, `README.md` config table, `.env.example`, and `docker-compose.yml` passthrough if containers need it. New tool → README "Tools" section + tool docstring (that docstring is what the model reads).
6. **Run the gates locally** (§5) before pushing: ruff format + check, pytest, `go vet`/`go test -tags sqlite_fts5`, golangci-lint. For Docker-affecting changes, `docker compose up -d --build` and the curl smoke test in `docs/DOCKER.md`.
7. **Commit message = the PR description.** Conventional-commit title; body says the problem, the fix, what was verified and `Closes #N`. Co-author trailer for agents.
8. **Open the PR with `gh pr create --repo Tauri-EPO/whatsapp-mcp --base main`.** Body: what/why, verification, security note if auth/paths/network/exec are touched.
9. **Wait for CI, then squash-merge:** `gh pr merge N --squash --delete-branch`. All checks must be green; a `startup_failure` or network flake is re-run with `gh run rerun <id> --failed`, never bypassed. Agents automate this with a wait-then-merge loop; never merge with red checks.
10. **After merge:** `git fetch origin`; rebase any open stacked branch; confirm the issue closed (`Closes #N` does it when the PR targets `main`).
11. **Deploy** is a manual step on the server: `git pull && docker compose up -d --build`.

Rules that stay true across all steps:

- **Conventional commits** in titles: `feat:`, `fix:`, `perf:`, `refactor:`, `docs:`, `ci:`, `chore:`, `test:`. `!` for breaking changes.
- **No drive-by formatting** and no unrelated cleanups in a PR.
- **No new top-level dependencies** without a sentence of justification in the PR.
- **Security-sensitive changes** (auth, file paths, network bind, command exec, allow-lists) must be called out in the PR body and get tests for the deny path.
- **Never** hand-edit `CHANGELOG.md`, versions in `pyproject.toml`/`server.json`, or `.release-please-manifest.json`; they are upstream artefacts kept only so files stay comparable.

## 5. Local commands and tooling

```bash
# Python MCP server
cd whatsapp-mcp-server
uv sync --extra dev
uv run ruff format . && uv run ruff check .
uv run pytest -q
uv run main.py                                   # stdio; WHATSAPP_MCP_TRANSPORT=http for HTTP

# Go bridge — -tags sqlite_fts5 compiles FTS5 in (search index); without it the bridge
# still runs and search falls back to a substring scan
cd whatsapp-bridge
go run -tags sqlite_fts5 .
go vet -tags sqlite_fts5 ./... && go test -tags sqlite_fts5 ./...
golangci-lint run                                # build tag is set in .golangci.yml

# Containers (both components, MCP over streamable HTTP) — see docs/DOCKER.md
docker compose up -d --build
docker compose logs -f bridge                    # QR code on first run
docker compose --profile whisper up -d           # + local whisper.cpp for transcribe_audio
```

**Windows without a Go toolchain** (the primary dev box): build, test and lint the bridge inside Docker, mounting the module cache. From Git Bash with `MSYS_NO_PATHCONV=1`:

```bash
docker run --rm -v "$PWD/whatsapp-bridge:/src" -v "$USERPROFILE/go/pkg/mod:/go/pkg/mod" \
  -v wamcp-gobuild:/root/.cache/go-build -w /src golang:1.26-alpine \
  sh -c 'apk add --no-cache gcc musl-dev >/dev/null; go vet -tags sqlite_fts5 ./... && go test -tags sqlite_fts5 ./...'
docker run --rm -v "$PWD/whatsapp-bridge:/src" -v "$USERPROFILE/go/pkg/mod:/go/pkg/mod" \
  -w /src golangci/golangci-lint:v2.11.0 golangci-lint run
```

Working-copy files are CRLF (`core.autocrlf=true`); commits are LF. `*.sh` and `Dockerfile` are forced LF by `.gitattributes`. When editing files programmatically, read with universal newlines and write `\n`. Prefer writing whole files or line-anchored edits over shell heredocs containing backslash escapes.

## 6. CI gates

Every PR runs `.github/workflows/ci.yml` and `security.yml`. All of these must be green before merging (the informational ones too: investigate, do not ignore):

| Job | What |
|---|---|
| Python Lint | `ruff check` + `ruff format --check` |
| Python Tests | `pytest` |
| Go Lint | golangci-lint v2.11.0 (`errcheck`, `govet`, `ineffassign`, `unused`, `staticcheck`, `gosec`, `misspell`). Suppress a gosec finding only with `//nolint:gosec // <why>` on the line |
| Go Build | `go build -tags sqlite_fts5`, `go vet`, `go test` |
| Version Consistency | `pyproject.toml` vs `server.json` (kept for file parity with upstream) |
| CodeQL (Python, Go) | security scanning; `"host" in list` style asserts trip `py/incomplete-url-substring-sanitization`, use set comparisons in tests |
| Bandit, pip-audit, govulncheck | `continue-on-error`; read the output anyway |
| Docker Build | both images build with buildx (GHA cache); smoke: bridge starts and reports the FTS state, every MCP module imports inside the image |

Release workflows (`release.yml`, `release-please.yml`) are `workflow_dispatch` only and not used by this fork. Dependabot auto-merge was removed; merge its PRs through the normal routine.

## 7. Environment variables

| Variable | Default | Purpose |
|----------|---------|---------|
| `WHATSAPP_STORE_DIR` | `store` (bridge, relative to cwd), `../whatsapp-bridge/store` (MCP) | Directory for `whatsapp.db`, `messages.db`, media, `.bridge-token`, `.bridge.lock` (`store_dir.go`: `storeDir()`, `storePath()`). Set for both processes; the compose file uses `/app/store` |
| `WHATSAPP_DB_PATH` | `$WHATSAPP_STORE_DIR/messages.db` | SQLite path used by the MCP server (overrides the store dir) |
| `WHATSMEOW_DB_PATH` | `$WHATSAPP_STORE_DIR/whatsapp.db` | whatsmeow SQLite (LID ↔ phone resolution via `whatsmeow_lid_map`); overrides the store dir |
| `WHATSAPP_API_URL` | `http://localhost:8080/api` | Bridge REST endpoint |
| `WHATSAPP_BRIDGE_BIND` | `127.0.0.1` | Bridge REST listen address; `0.0.0.0` / `::` for other containers or hosts (`rest_bind.go`) |
| `WHATSAPP_BRIDGE_ALLOWED_HOSTS` | *(loopback only)* | Extra `Host` values accepted by the bridge (`host` any port, `host:port` exact, `*` any). Same semantics as `WHATSAPP_MCP_ALLOWED_HOSTS`; loopback spellings always included; a non-loopback bind without it stays loopback-only (403) |
| `WHATSAPP_BRIDGE_PORT` | `8080` | Port the bridge listens on |
| `WHATSAPP_BRIDGE_TOKEN` | generated next to `WHATSMEOW_DB_PATH` as `.bridge-token` | Bearer token required for bridge REST calls; also signed onto outbound webhooks |
| `WHATSAPP_MEDIA_AUTODOWNLOAD` | `true` | Cache inbound media on arrival; `false` = fetch only on `/api/download` (`media_retention.go`) |
| `WHATSAPP_MEDIA_RETENTION_DAYS` | *(unset)* | Daily sweep deletes media files older than N days under `store/<chat>/`; DB rows untouched |
| `WHATSAPP_MEDIA_ROOTS` | `~/.local/share/whatsapp-mcp/outbox` | Path-list of directories allowed for outbound media files |
| `WHATSAPP_DEVICE_NAME` | `whatsmeow` (whatsmeow default) | Linked-device label shown in WhatsApp > Linked Devices. Applied at pair time only; re-pair to change |
| `WHATSAPP_ALLOWED_CHATS` | *(unset = all chats)* | Conversation allow-list (JIDs, bare numbers, `*@g.us` / `*@s.whatsapp.net`). MCP server filters reads and refuses writes (`chat_policy.py`); bridge returns 403 on send/react/mark-read/typing/delete/group/poll (`chat_policy.go`). Set for both processes |
| `WHATSAPP_LOG_LEVEL` | `INFO` | Bridge log level (`DEBUG`/`INFO`/`WARN`/`ERROR`), applied to the bridge logger and the whatsmeow client. `DEBUG` echoes each stored message |
| `WHATSAPP_MCP_LOG_LEVEL` | `INFO` | MCP server log level (stderr) |
| `WHATSAPP_MCP_TRANSPORT` | `stdio` | MCP transport: `stdio`, `http`, or `sse` |
| `WHATSAPP_MCP_HOST` | `127.0.0.1` | Bind address for the `http`/`sse` transports |
| `WHATSAPP_MCP_PORT` | `8000` | Port for the `http`/`sse` transports |
| `WHATSAPP_MCP_ALLOWED_HOSTS` | loopback only | Extra `Host` header values accepted by the `http`/`sse` transports (comma-separated; bare hostnames match any port; `*` disables the check). Unset + non-loopback bind disables the check with a warning |
| `WHATSAPP_MCP_ALLOWED_ORIGINS` | derived from allowed hosts | Extra `Origin` header values for browser-based MCP clients |
| `WHATSAPP_MCP_RATE_LIMIT` | `120` with a token, `0` without | Requests/minute per client (X-Forwarded-For first hop or peer) on `http`/`sse`; token bucket in `http_auth.RateLimitMiddleware`, 429 + Retry-After; `0`/`off` disables |
| `WHATSAPP_MCP_MAX_BODY_BYTES` | `4194304` | Max request body for `http`/`sse` (passed to the SDK app) |
| `WHATSAPP_MCP_TOKEN` | bridge token when bound off-loopback; none on loopback | Static bearer token enforced on the `http`/`sse` transports (`http_auth.resolve_http_token`, min 16 chars). Unset + non-loopback bind → reuses the bridge token (env or `.bridge-token`); `off` disables auth explicitly. stdio unaffected |
| `WEBHOOK_URL` | `http://localhost:8769/whatsapp/webhook` | Outgoing webhook for incoming messages (empty falls back to this default) |
| `WEBHOOK_ENABLED` | `true` (compose: `false`) | Set to `false` to disable outbound webhooks entirely |
| `FORWARD_SELF` | `true` | Whether self-sent messages are forwarded to the webhook |
| `WHATSAPP_PARENT_WATCHDOG_S` | `30` | Stdio parent-liveness poll interval (seconds) |
| `WHISPER_URL` | *(unset)* | whisper.cpp `whisper-server` inference endpoint for `transcribe_audio` (`transcribe.py`). Wins over `WHISPER_BIN` |
| `WHISPER_BIN` / `WHISPER_MODEL` | *(unset)* | Local `whisper-cli` binary + `ggml-*.bin` model, alternative backend |
| `WHISPER_LANGUAGE` | `pt` | Default transcription language; `auto` to detect |
| `WHISPER_TIMEOUT_S` | `300` | Per-transcription timeout (seconds) |

Compose-only knobs (`WHATSAPP_MCP_BIND`, `WHATSAPP_OUTBOX`, `WHISPER_MODEL_NAME`, `WHISPER_THREADS`, `COMPOSE_PROFILES`) are documented in `.env.example` and `docs/DOCKER.md`.

When adding a new env var: document it here, in `README.md`, in `.env.example`, and pass it through in `docker-compose.yml` when a container needs it.

## 8. Gotchas (read before editing)

1. **JIDs.** WhatsApp identifies users as `1234567890@s.whatsapp.net` (DM), `123456@g.us` (group), and `<random>@lid` (link-ID, anonymous). The bridge maintains a phone↔LID map in `whatsapp.db.whatsmeow_lid_map`. Many "user is missing" / "messages don't show" bugs trace back to JID-form mismatches. Always think about both forms (`resolveUserJID`, `resolveQuotedParticipantJID`, `resolveMentionJIDs`).
2. **Message IDs are unique per chat, not globally.** The `messages` primary key is `(id, chat_jid)`. Always pass `chat_jid` alongside an ID; forwards reuse IDs across chats.
3. **Pointer rows.** `reaction` and `poll_vote` messages refer to another message via `messages.target_message_id` (the bridge also writes it to `filename` for one release; readers use `_target_id()` which falls back to `filename` for pre-migration rows). Do not add new meanings to `filename`.
4. **Media files** live under `store/{chat_jid}/` with timestamp + message-ID filenames. Use `/api/download`, never hand-built paths. CDN URLs expire (403/404/410 after days); `downloadMedia` runs one media-retry round trip against the sender's phone (`media_retry.go`) before failing.
5. **Audio.** Voice notes must be Opus `.ogg`; `send_audio_message` converts via ffmpeg. `transcribe_audio` converts to 16 kHz WAV before whisper.
6. **History sync** is controlled by the phone. Modern syncs put the group sender in top-level `WebMessageInfo.participant`; read it before `Key.participant`. Poll votes in history cannot be decrypted (issue #59).
7. **`messages.db` is the source of truth for reads.** The MCP server must never need the bridge for read-only tools. The bridge opens the DB in WAL mode with a busy timeout; the MCP side uses a 5 s timeout via `_connect_messages_db()`.
8. **Search index.** The bridge owns `messages_fts` (FTS5, `fts.go`) and its triggers; it creates them when built with `-tags sqlite_fts5` and *drops* them otherwise so writes never fail. The MCP server uses `MATCH` only when the table exists and falls back to `instr()`. Never create FTS triggers from Python.
9. **One bridge per store.** `main()` takes an exclusive OS lock on `store/.bridge.lock` (`instance_lock.go`); a second bridge exits naming the holder's PID. Tests that need concurrent bridge processes must use separate working directories.
10. **No package-level state in the bridge.** Runtime dependencies live on the `Bridge` struct (`bridge.go`); tests build one with `testBridge(...)` and override fields. The one sanctioned global is `bridgeLog` (`logging.go`), write-once configuration set by `initLogging()`; tests swap it with `installRecordingLogger(t)`.
11. **stdout is the protocol on stdio.** Anything the MCP server prints to stdout can corrupt a stdio session; log through `logging` (stderr), never `print()`.
12. **Bridge logs go through `bridgeLog`, not `fmt.Print*`.** Levels: `Errorf` for failures that lose data, `Warnf` for degraded-but-continuing, `Infof` for lifecycle, `Debugf` for per-request traces and message echoes (user content stays out of `INFO`). The only `fmt.Print*` left are the first-run token banner and the pairing QR code, which are meant for a human.
13. **REST starts before pairing.** `/api/health` is liveness (200 once the listener is up, body carries `connected`/`paired`); `/api/ready` is readiness (200 only while connected). Endpoints that need WhatsApp check `client.IsConnected()` themselves.
14. **Outgoing calls are not visible to linked devices.** Don't promise features that depend on them.

## 9. Where to make changes

| You want to… | Touch |
|---|---|
| Add or modify an MCP tool | `whatsapp-mcp-server/main.py` (+ README "Tools", tests) |
| Change DB queries / dict conversion | `whatsapp-mcp-server/whatsapp.py` |
| Change HTTP transport, auth, allowed hosts | `whatsapp-mcp-server/main.py` (`__main__`), `mcp_config.py`, `http_auth.py` |
| Change the conversation allow-list | `chat_policy.py` **and** `whatsapp-bridge/chat_policy.go` |
| Change voice-note transcription | `whatsapp-mcp-server/transcribe.py`, `whisper` profile in `docker-compose.yml` |
| Add a bridge REST endpoint | new `whatsapp-bridge/<feature>.go` with `handleX(deps…) http.HandlerFunc`, register in `newRESTMux` (`rest.go`), tests with fakes |
| Change inbound event handling | `handleEvent` / `handleMessage` in `events.go`, `handleHistorySync` in `history_sync.go`; content extraction in `content.go` |
| Change the messages schema | `ensureMessageStoreSchema` in `store.go`; migrations idempotent (`ensureColumn`); FTS in `fts.go` |
| Change webhook payload | `whatsapp-bridge/webhook.go` |
| Change build identity (`/api/version`, MCP `version`) | `whatsapp-bridge/version.go`, `ARG GIT_SHA/VERSION` in both Dockerfiles, compose build args |
| Change startup / wiring (env parsing, pairing, shutdown) | `whatsapp-bridge/main.go` (keep it under ~400 lines; logic goes in a feature file) |
| Change containers | `whatsapp-bridge/Dockerfile`, `whatsapp-mcp-server/Dockerfile`, `docker-compose.yml`, `docs/DOCKER.md` |
| Change CI | `.github/workflows/ci.yml`, `security.yml` |

## 10. Persona for AI agents

- **Be terse.** Don't restate the question.
- **Be decisive.** Pick the smallest change that fixes the problem and ship it through §4.
- **Bias to action** for low-risk improvements (lint, tests, error messages, comments that explain *why*).
- **Ask** before changing the compose topology, adding a dependency, or loosening auth semantics.
- **Cite files with `path:line`** when discussing code.
- **Report honestly.** If a test could not run (needs a paired phone, network), say so in the PR instead of implying coverage.

## 11. Issues

- One problem per issue. Title prefixed with priority (`P0:`, `P1:`, `P2:`); body with **Problem**, **Fix** (sketch sized for one PR) and **Acceptance** checkboxes. Labels: priority + `area:*` (+ `type:refactor`, `type:security`, `bug`, `documentation`, `upstream` when it mirrors an upstream item).
- Larger efforts get an `epic` issue holding the checklist (current one: #64).
- Bugs from operation: include bridge log lines, `docker compose ps`, the tool call and its result; redact phone numbers.
- "Won't do" is a valid outcome; close with a sentence explaining why.

See [`CONTRIBUTING.md`](./CONTRIBUTING.md) for the human-facing contribution guide and [`docs/DOCKER.md`](./docs/DOCKER.md) for operations.
