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

A WhatsApp ↔ MCP bridge tuned for an **always-on home server** reached over **Tailscale** by remote MCP clients (an AI bot on another machine, an IDE on a laptop), sharing one WhatsApp account. Two components: a Go bridge (whatsmeow) and a Python MCP server (MCP SDK v2, streamable HTTP or stdio). Deployed with Docker Compose. `README.md` → "Why this fork" has the user-facing version of this story and a table of what differs from upstream.

- **Repo:** https://github.com/Tauri-EPO/whatsapp-mcp — remote `origin`. All PRs, issues and `gh` commands target this repo.
- **Default branch:** `main`. `main` is the deployable state. Releases are automatic (`release.yml` + `release-cut.yml`, see §4 "Versions"): every merge updates a release PR; once a day that PR is merged, which tags `vX.Y.Z` and publishes the GitHub Release and the `latest` images.
- **Lineage (for ideas, not for merging — see §2):**
  - https://github.com/verygoodplugins/whatsapp-mcp — remote `upstream` (also `vgp`). Maintained fork we started from at v0.6.0 (Sept 2026).
  - https://github.com/lharries/whatsapp-mcp — the original project (remote `lharries`; add with `git remote add lharries https://github.com/lharries/whatsapp-mcp.git` if missing).

## 2. Fork policy

**This is a hard fork.** Decided 2026-09-04: the fork is ahead of upstream (SDK v2, auth, allow-list, FTS5, polls, Docker…) and no longer merges `upstream/main`. Consequences:

- **Dependabot changes `uv.lock` only** (`versioning-strategy: lockfile-only`, #486): the floors and caps in `whatsapp-mcp-server/pyproject.toml` move by hand when code needs a newer feature, and a release that needs a cap moved (`mcp<3`), security fixes included, gets no Dependabot PR or alert-driven PR — watch the alert and move the cap yourself.
- **Dependencies move here first.** Dependabot stays on; its PRs are merged once CI is green. Bumps that change the shipped images (uv, gomod, docker, docker-compose) are titled `deps:` and cut a patch release; GitHub Actions bumps stay `chore(deps)`. Python dev tooling (ruff, pytest, pyright) never reaches an image, so it arrives in its own PR (the `pip-dev-*` groups in `.github/dependabot.yml`) — Dependabot cannot title it differently, because the commit prefix is per entry and its `uv` ecosystem classifies every dependency as production ([dependabot-core#13202](https://github.com/dependabot/dependabot-core/issues/13202); `[project.optional-dependencies]` is not a development group either, [#10606](https://github.com/dependabot/dependabot-core/issues/10606)), so retitle it when merging: `scripts/merge-when-green.sh N --subject "chore(deps): bump the python dev tooling (#N)"`. The squash title is what release-please reads, so that keeps it out of the release. Never pin a dependency just because upstream did (`mcp<2` and `cryptography<49` were both dropped).
- **Refactors are allowed.** Keep each PR small (§4) even when the overall change is large; §4 step 1 says where the current work comes from.
- **Upstream is a source of ideas and cherry-picks, never a merge target.** When asked to "look upstream", "get ideas from the original", "check what VGP/lharries did", do this:
  1. `scripts/upstream-harvest.sh` — fetches both remotes and prints, for each, the commits since the last harvest (`.upstream-harvest`) in two lists (those touching `whatsapp-bridge/` or `whatsapp-mcp-server/`, then the rest: docs, scripts, compose, workflows) and every PR and issue updated since the mark's timestamp, open, closed or merged. `--all` ignores the marks: every commit vs our `main`, every open PR and issue. The PR/issue list prints at most `HARVEST_LIMIT` entries per repository (default 1000) and says so when there are more, as it does when `gh` failed. After reviewing, `scripts/upstream-harvest.sh --mark`, run from any worktree of the same clone, records what that listing read (the heads it printed and GitHub's clock when it started, never a fresh fetch); a mark's timestamp only moves when the PR/issue list was complete. Commit `.upstream-harvest` through a PR. A mark the upstream head no longer descends from (a force-push) stops the listing with an error instead of reading as "nothing new": review with `--all`, then `--mark`. The script is tested with fake remotes and a fake `gh` in `whatsapp-mcp-server/tests/test_upstream_harvest_script.py`. (Manual equivalent: `git log --oneline main..upstream/main -- whatsapp-bridge/`; protocol/whatsmeow changes are the most valuable to harvest.)
  2. `gh pr list --repo verygoodplugins/whatsapp-mcp --state all --search "<topic>"` and the same on `lharries/whatsapp-mcp`; read the PR description first — it usually explains the WhatsApp behaviour better than the diff.
  3. Reimplement the delta against our code (`git cherry-pick -x <sha>` only when the patch applies cleanly to files we have not diverged in). Credit the source in the commit body ("Reimplements upstream VGP #NNN"), as every PR in this repo has done so far.
  4. Do not bring upstream's release commits, CHANGELOG entries or version bumps: this repo computes its own versions with release-please and `CHANGELOG.md` is generated, never hand-edited.
- **whatsmeow protocol drift** is the one thing upstream will keep fixing before us. Monthly routine (first done 2026-09-04):
  1. In `whatsapp-bridge/`: `go get go.mau.fi/whatsmeow@latest && go mod tidy` (inside the `golang:<version>-alpine` container on Windows). If the new version needs a newer Go, bump `go.mod`, the Dockerfile base image, `go-version` in every workflow and the golangci-lint version together — they must agree.
  2. `go vet`/`go test ./...`, `golangci-lint run`, `docker compose build`.
  3. Pair a test store, send/receive one text, one media, one poll; watch the log for new event types whatsmeow now emits.
  4. PR titled `deps: bump whatsmeow to <version>` (`deps:` cuts a patch release, so the fix reaches `latest`); commit body lists notable upstream changes.
- Upstream's `ROADMAP.md` (read it in their repo) lists what they will not take; it does not bind this fork.

## 3. Architecture

```
whatsapp-mcp/
├── whatsapp-bridge/            # Go — WhatsApp Web via whatsmeow, REST API, messages.db owner
│   ├── main.go                 # startup and wiring only (flags, env, pairing, signal handling)
│   ├── bridge.go               # Bridge struct: runtime dependencies shared by handlers
│   ├── pairing.go              # QR pairing and first connection, one context per code sequence
│   ├── operator.go             # private opt-in listener: separate bearer token, Host/Origin checks, limits and audit
│   ├── operator_config.go      # bounded operator configuration, token-file checks and single-interface bind
│   ├── operator_probe.go        # credential-free argv/output for the operator smoke probe
│   ├── operator_pairing.go     # HTTP pairing state, QR/code, explicit restart and passkey actions
│   ├── runtime_client.go      # atomic client handoff, refreshed REST handlers and retired-event filtering
│   ├── connection_problem.go   # classified connection failures, persisted ban expiry and dial blocking
│   ├── connection_events.go    # opt-in safe connection webhooks with debounced disconnection
│   ├── events.go               # whatsmeow event dispatch, handleMessage, calls, reconnect loop
│   ├── history_sync.go         # handleHistorySync (phone replays at pair time / on demand)
│   ├── persist.go              # one extraction + storage path shared by live messages and history sync
│   ├── store_failures.go       # a row that cannot be written: bounded retry on a busy database, one ERROR with ID + chat, a counter
│   ├── content.go              # extract text/quotes/mentions/media/ephemeral from waE2E.Message
│   ├── location.go             # typed non-file locations and exact-key live position updates
│   ├── history_share.go        # bounded shared-group history download/decode into canonical history sync
│   ├── history_share_budget.go # one background shared-history import and one waiting job
│   ├── history_share_decoder.go # protobuf wire preflight caps messages and allocations before decoding
│   ├── history_share_guard.go  # transaction-bound existence check prevents peer archive replacement
│   ├── media_header.go         # template/buttons/interactive header media extraction
│   ├── view_once.go            # view-once envelopes unwrapped and archived; the phone keeps its one view
│   ├── jid.go                  # phone <-> LID resolution helpers; shared nil-safe LID-map read
│   ├── quoted_participant.go   # the quoted sender JID a reply's recipients can match against a member
│   ├── outbound_quote.go       # same-chat typed quote previews and context-bound archive lookups
│   ├── send.go                 # /api/send types, sendWhatsAppMessage, media upload, Ogg Opus analysis
│   ├── recipient_cache.go      # bounded, expiring typed -> registered number answers, cleared on reconnect
│   ├── send_mime.go            # sniff MIME for category-named cached images/videos
│   ├── edit_forward.go         # /api/edit (own message, edit window), /api/forward (re-send elsewhere)
│   ├── forward_media.go        # forward stored category and original presentation, keeping cache names
│   ├── media_presentation.go   # recipient-visible metadata persisted with the media snapshot
│   ├── media.go                # inbound media download into store/<chat>/
│   ├── media_cache_path.go     # the one rule for where a cached media file is (download lookup, purge, webhook read)
│   ├── media_path.go           # WHATSAPP_MEDIA_ROOTS: outbound media_path confined to an allow-list
│   ├── rest.go                 # newRESTMux route table + HTTP server; handlers live next to their features
│   ├── rest_middleware.go      # writeError (JSON error shape), requireMethod, requestLog
│   ├── health.go               # /api/health (liveness), /api/ready (readiness)
│   ├── version.go              # GET /api/version: build identity injected by ldflags
│   ├── me.go                   # GET /api/me: the account's own phone JID and LID (authenticated)
│   ├── chat_actions.go         # /api/react, /api/typing
│   ├── mark_read.go            # /api/mark-read: listed IDs, or the whole chat up to a timestamp
│   ├── labels.go               # cached Business labels, chat associations, REST endpoints and startup sync
│   ├── chat_archive.go         # /api/chat/archive: archive/unarchive anchored to the newest stored message
│   ├── store.go                # MessageStore: schema, migrations, message/chat/call queries
│   ├── umask_unix.go           # startup-only POSIX owner-only creation mask
│   ├── umask_other.go          # creation-mask no-op on non-POSIX systems
│   ├── store_dir.go            # storeDir/storePath: WHATSAPP_STORE_DIR resolution; DSN options and connection-pool bounds (boundPool)
│   ├── store_batch.go          # atomic live-message writes and bounded history-sync transactions
│   ├── migration_markers.go   # independent named markers for data rewrites
│   ├── store_time.go           # dbTime/parseDBTime: the one UTC timestamp spelling + its migration
│   ├── mentions.go             # messages.mentions: who a message addressed + the backfill from old text
│   ├── sender_namespace.go     # messages.sender_server: which namespace a stored sender is in + its backfill
│   ├── chat_names.go           # chat name resolution; history sync never calls the network for one
│   ├── logging.go              # bridgeLog + WHATSAPP_LOG_LEVEL; text writer: marked multiline stacks, escaped controls and 8 KiB lines
│   ├── sdk_log.go              # permanent SDK logger adapter redacts network-error URLs
│   ├── logging_json.go         # WHATSAPP_LOG_FORMAT=json line logger
│   ├── metrics.go              # counters + GET /metrics (Prometheus text)
│   ├── rest_bind.go            # WHATSAPP_BRIDGE_BIND / WHATSAPP_BRIDGE_ALLOWED_HOSTS
│   ├── config.go               # validates all startup values before filesystem/database/listener effects; one diagnostic, exit 1
│   ├── env_bool.go             # the one boolean parser (a value it cannot read stops the bridge) + the four default-on switches
│   ├── media_retention.go      # WHATSAPP_MEDIA_AUTODOWNLOAD / _RETENTION_DAYS, store size
│   ├── auth.go                 # bearer token + loopback Host allow-list for /api/*
│   ├── chat_policy.go          # WHATSAPP_ALLOWED_CHATS enforcement on outbound endpoints
│   ├── read_only.go            # WHATSAPP_READ_ONLY: 403 on every mutating /api/* endpoint
│   ├── tool_policy.go          # WHATSAPP_ALLOW_TOOLS / _DENY_TOOLS: tool -> endpoint map, 403 on the rest
│   ├── fts.go                  # FTS5 index over messages.content
│   ├── media_inflight.go       # one transfer per destination file; the other callers share its result
│   ├── media_cap.go           # automatic download byte budget, shared-cache checks
│   ├── media_length.go        # declared-length presence and the NULL migration
│   ├── media_budget.go         # bounded pool for automatic media caching (4 at a time, 256 queued, drops the rest)
│   ├── media_retry.go          # re-download expired CDN media via the sender's phone
│   ├── media_purge.go          # POST /api/media/purge: drop cached files by (id, chat) or criteria, rows untouched
│   ├── instance_lock.go        # one bridge per store (flock / LockFileEx)
│   ├── polls.go                # native polls: creation, votes, /api/poll
│   ├── group_members.go        # /api/group/members
│   ├── group_members_store.go  # group_members table: rosters cached per group
│   ├── group_events.go         # GroupInfo join/leave/promote/demote + the paced roster refresh
│   ├── session_keepalive.go    # WHATSAPP_SESSION_KEEPALIVE_HOURS: the presence blip that keeps WhatsApp from logging the device out
│   ├── hours_env.go            # resolveHoursEnv: the "whole hours, 0 disables" variables of the periodic passes
│   ├── group_manage.go         # /api/group/participants, /subject, /invite, /leave
│   ├── delete_message.go       # /api/delete (revoke / local delete)
│   ├── history_ondemand.go     # POST /api/history
│   ├── webhook.go              # outbound webhook for inbound messages
│   ├── Dockerfile              # alpine, pure-Go sqlite (modernc), CGO_ENABLED=0
│   └── store/                  # WHATSAPP_STORE_DIR: whatsapp.db, messages.db, media, .bridge-token, .bridge.lock, .session-keepalive (gitignored)
├── whatsapp-mcp-server/        # Python — MCP tools; reads messages.db, calls bridge REST
│   ├── main.py                 # MCPServer (SDK v2) tool definitions + transport startup
│   ├── strict_args.py          # StrictArgumentServer: call_tool refuses arguments no tool declares
│   ├── errors.py               # the one failure envelope: {"error": {"code", "message"}} and its codes
│   ├── whatsapp.py             # SQL queries, bridge HTTP client, dict conversion
│   ├── phone.py                # phone spellings for lookups (Brazilian ninth digit) and outbound recipient separators
│   ├── export.py               # export_messages: NDJSON archive under WHATSAPP_EXPORT_DIR, path not rows
│   ├── media_inventory.py      # list_media / get_media_stats: sizes, sha256 copies, cache scan of store/<chat>/
│   ├── media_read.py           # read_media: the bytes as MCP content blocks instead of a server path
│   ├── media_text.py           # read_media(as_text=True): PDF/DOCX/XLSX text extracted here, no OCR
│   ├── media_image.py          # read_media images: downscaled, upright, stripped, in a format clients render
│   ├── media_pdf.py            # read_media(as_images=True): PDF pages rendered with pypdfium2
│   ├── media_upload.py         # send_file / send_audio_message media_base64: bytes written under <media root>/.uploads for the bridge, removed after the send
│   ├── media_resource.py       # MediaResourceServer: the whatsapp://media/<chat>/<id> resource, and the resource_link on list_media rows
│   ├── media_notes.py          # notes.db (MCP-owned): agent notes keyed by sha256; annotate/get/search_media_notes; clear_media_refusal; media_refusals; transcripts_fts
│   ├── notes.py                # notes.db: versioned notes on chats/contacts/messages (media via media_notes)
│   ├── private_files.py        # MCP-owned notes/export/upload permissions and shared notes connection factory
│   ├── triage.py               # mark_handled / snooze + the handled/snoozed/muted SQL filter list_unanswered applies
│   ├── mcp_config.py           # transport/host/port/allowed-hosts parsing
│   ├── observability.py        # WHATSAPP_MCP_LOG_FORMAT=json + the MCP /metrics middleware
│   ├── parent_watchdog.py      # stdio: exit once the parent process is gone (WHATSAPP_PARENT_WATCHDOG_S)
│   ├── http_auth.py            # WHATSAPP_MCP_TOKEN bearer middleware
│   ├── http_upload.py          # POST /upload: bounded raw-body HTTP uploads, sent by upload_id, one-hour TTL
│   ├── chat_policy.py          # WHATSAPP_ALLOWED_CHATS for reads and writes
│   ├── tool_policy.py          # WHATSAPP_READ_ONLY / _ALLOW_TOOLS / _DENY_TOOLS: hides + refuses tools
│   ├── untrusted.py            # @untrusted_content: the third-party-data sentence, name sanitisation, WHATSAPP_WRAP_UNTRUSTED
│   ├── endpoint_cert.py        # WHATSAPP_PUBLIC_URL: TLS expiry of the published endpoint, cached
│   ├── transcribe.py           # whisper.cpp backends for transcribe_audio
│   ├── transcribe_worker.py    # TRANSCRIBE_ON_INGEST: background thread transcribing inbound voice notes
│   ├── audio.py                # ffmpeg helpers
│   └── Dockerfile              # python:3.13-slim + ffmpeg + uv, http transport
├── docker-compose.yml          # bridge + mcp — docs/DOCKER.md (no whisper: WHISPER_URL points at a server you run)
├── docker-compose.operator.yml # optional private operator network; no operator host port
├── scripts/                    # backup.sh (hot backup/restore of the store volume), smoke.sh (post-deploy check), upstream-harvest.sh, merge-when-green.sh (update / wait for CI / squash-merge a PR)
├── docs/                       # user docs: DOCKER.md (ops), CONFIGURATION.md (every env var), TOOLS.md (tool reference),
│                               # LAPTOP.md (stdio setup), TROUBLESHOOTING.md, ARCHITECTURE.md (diagrams)
├── release-please-config.json  # release-please (simple, release-as for the first tag); .release-please-manifest.json holds the current version
└── .github/workflows/          # ci.yml, security.yml, publish.yml (main/sha images, steps aside on a release commit), release.yml (release PR, tags, every image tag of a release commit), release-cut.yml (daily merge of the release PR), build-push.yml (reusable)
```

Data flow: MCP client → MCP server → reads `messages.db` directly for everything read-only, calls bridge REST (`WHATSAPP_API_URL`, default `http://localhost:8080/api`) for sends, media, group info, polls, deletes → bridge → WhatsApp Web.

Three SQLite databases in the store directory: `whatsapp.db` (whatsmeow: session, contacts, LID map — opaque) and `messages.db` (ours: `chats`, `messages`, `calls`, `polls`, `poll_votes`, `group_members`, `labels`, `chat_labels`, `messages_fts`, `schema_migrations` (bridge-owned, one row per applied one-off migration)) are written by the bridge and only read by the MCP server; `notes.db` (`media_notes`, keyed by content hash, plus per-message `media_refusals` (dated cache-identity refusals), `notes_meta` and the `transcripts_fts` index over the stored transcripts) is created lazily and owned by the MCP server, and the bridge never opens it — the "never create FTS from Python" rule is about `messages.db` only.

Compose topology: the `mcp` container joins the bridge's network namespace (`network_mode: service:bridge`), so the bridge keeps its loopback bind and loopback-only Host allow-list; the MCP port is published on the bridge service. An alternative topology is issue #58.

## 4. The routine: from issue to merged PR

This is how every change in this repo has been shipped; follow it unless the user says otherwise.

1. **Start from an issue.** Bugs and features have one. If none exists, open it (§11): one problem per issue, with a "Fix" sketch and acceptance boxes. The plan is the open backlog, read by priority: `gh issue list --repo Tauri-EPO/whatsapp-mcp --label P0`, then the same with `--label P1` and `--label P2`. Issues filed through the forms arrive as `needs-triage` with no priority, so read `--label needs-triage` first and triage them (§11). Larger efforts are `epic` issues: `gh issue list --repo Tauri-EPO/whatsapp-mcp --label epic --state open` lists the ones in progress. The closed ones are history behind a design, never a to-do list: hardening and media (#64, #138, #99) and the manual verification of the September round on the home server (#339, the record of what was exercised against a paired phone, how, and what was left unexercised — the leftovers are #487).
2. **Branch from current `main`:** `git fetch origin && git checkout -b <type>/<slug> origin/main` when the clone is yours alone, `git worktree add` when it is not ("Working in parallel" below). Types: `fix`, `feat`, `perf`, `refactor`, `docs`, `ci`, `chore`, `test`.
3. **One concern per PR, small.** Target under ~300 changed lines of code (docs and tests excluded). Split refactors into pure-move PRs. If a change needs another open PR, stack the branch on it, say "Stacked on #N" in the body, and retarget to `main` after that merges.
4. **Tests with the change.** Python: `tests/` (pytest, real SQLite files in `tmp_path`, `monkeypatch` for `requests`/policy/env). Go: table tests, `httptest`, fakes injected as functions (see `group_members.go`, `delete_message.go`, `polls.go`), `newTestMessageStore`. No test may need a paired phone.
5. **Docs in the same PR.** New env var → this file §7, `docs/CONFIGURATION.md`, `.env.example`, and `docker-compose.yml` passthrough if containers need it. `tests/test_env_docs.py` fails the Python job when the four disagree with what the code reads (compose-only knobs live in its allow-list). New tool → `docs/TOOLS.md` + the README "What your agent can do" table if it adds a capability + tool docstring (that docstring is what the model reads). `README.md` is the landing page for people arriving from search (Claude Code / Codex / Cursor / bots wanting WhatsApp): keep it short and outcome-oriented; technical detail goes in `docs/`.
6. **Run the gates locally** (§5; sessions running in parallel use its "Several sessions at once" variant) before pushing: ruff format + check, pyright, pytest, `go vet`/`go test`/`go test -race`, golangci-lint. For Docker-affecting changes, `docker compose up -d --build` and `scripts/smoke.sh` (CI runs it on the unpaired stack too).
7. **Commit message = the PR description.** Conventional-commit title; body says the problem, the fix, what was verified and `Closes #N`. Co-author trailer for agents.
8. **Open the PR with `gh pr create --repo Tauri-EPO/whatsapp-mcp --base main`.** Body: what/why, verification, security note if auth/paths/network/exec are touched.
9. **Wait for CI, then squash-merge:** `scripts/merge-when-green.sh N` (flags after the number go to `gh pr merge`, e.g. `--subject`). The ruleset on `main` only accepts a PR whose three CI jobs are green on a head that is up to date with `main` (issue #545: two green PRs broke `main` together), so the script updates the branch when `main` moved, waits for the checks and squash-merges, going round again when another PR got in first; it stops on a red or cancelled check (exit 1) and on a conflict (exit 2: rebase by hand), and the squash commit is the PR title plus the PR description unless `--subject` / `--body` are passed. All checks must be green; a `startup_failure` or network flake is re-run with `gh run rerun <id> --failed`, never bypassed; never merge with red checks. One exception to the plain command: a Dependabot `pip-dev-*` PR is merged with `--subject "chore(deps): …"` so the dev-tooling bump does not cut a release (§2).
10. **After merge:** `git fetch origin`; rebase any open stacked branch; confirm the issue closed (`Closes #N` does it when the PR targets `main`).
11. **Deploy** is a manual step on the server: `git pull && docker compose up -d --build` (build mode) or `git pull && docker compose pull && docker compose up -d` (pull mode: `latest` is the last release, `main` the edge, `sha-<7>` / `vX.Y.Z` pins; `WHATSAPP_IMAGE_TAG` selects, default `latest`). Then `scripts/smoke.sh`. When the stack belongs to a manager (Komodo on the home server), the compose directory is root-owned, environment changes go in the manager and the check is `scripts/smoke.sh --project whatsapp-mcp --url <endpoint>`: read [`docs/DOCKER.md`](./docs/DOCKER.md#managed-stacks-komodo-portainer) before touching that host.

Rules that stay true across all steps:

- **Conventional commits** in titles: `feat:`, `fix:`, `perf:`, `refactor:`, `deps:`, `docs:`, `ci:`, `chore:`, `test:`. `!` for breaking changes.
- **No drive-by formatting** and no unrelated cleanups in a PR.
- **Working in parallel.** Several agent sessions share this clone, and every destructive git command can land in a directory that belongs to someone else.
  - One worktree per branch, created by you from a fresh `origin/main`: `git fetch origin && git worktree add -b <type>/<slug> <dir> origin/main`. Work only inside it, with absolute paths.
  - Before reviewing or committing, print `pwd` and `git branch --show-current`. If the directory is not the one you created, or the tree is unexpectedly clean, stop and say so: a session resumed after its worktree was garbage-collected lands in another session's checkout.
  - `git reset --hard`, `git checkout <other-branch>` and `git clean` belong to the directory you created. Elsewhere they wipe uncommitted work that exists nowhere else.
  - The stash stack is shared across worktrees: tag your entries (`git stash push -u -m <tag>`) and restore them by hash (`git stash apply <sha>`). Bare `git stash pop` takes whatever another session pushed last.
  - Give a review subagent your worktree path as its target; it otherwise starts in the coordinator's checkout and reviews the wrong diff.
  - Run the Docker-based Go gates (§5) one session at a time **with the default commands**: every one of them mounts the same host Go module cache, and `go vet`/`go test` also share the `wamcp-gobuild` build-cache volume. Sessions that run in parallel use the per-session variant in §5 ("Several sessions at once") instead, with their own module-cache and build-cache volumes. The gofmt LF copy is already per worktree and needs no such care.
  - After a crash, confirm the worktree still exists (`git worktree list`) before running anything in it.
- **No new top-level dependencies** without a sentence of justification in the PR.
- **Fake identifiers only.** The repository is public: examples, docstrings and fixtures use made-up phone numbers, JIDs, LIDs, names and hosts (`5511999999999`, `120363000000000001@g.us`, `example.ts.net`, Alice/Bob), never a value copied from a live archive. `whatsapp-mcp-server/tests/test_fake_identifiers.py` fails the Python job on a `55`-prefixed number, a user or group JID or a `*.ts.net` host that is not on its allow-list; a new fake is added there in the same PR (the failure does not print the value). A public list of common first names also checks decoded literals in Go/Python tests and serialized fixtures against reviewed fakes (Alice/Bob/Carol/Dave/Eve and John Doe/Jane Doe), without printing a rejected value. This is a heuristic, not a private contact list: arbitrary names still require review; never add a private name to the list.
- **Security-sensitive changes** (auth, file paths, network bind, command exec, allow-lists) must be called out in the PR body and get tests for the deny path.
- **Versions.** Automatic. `release.yml` runs release-please on every push to `main`: the next version comes from the Conventional Commit titles since the last tag (`feat:` minor, `fix:`/`perf:`/`refactor:`/`deps:` patch, `!` major; `docs:`, `ci:`, `test:` and `chore:` never trigger a release and stay out of the notes — release-please treats every visible changelog section as releasable, so a new type is releasable unless its section is `hidden`); it opens or updates a PR labelled `release` with the grouped notes and `CHANGELOG.md`. `release-cut.yml` merges that PR once a day (03:00 America/Sao_Paulo, or `gh workflow run release-cut.yml` to cut now) when CI on `main` is green, then dispatches `release.yml` — a merge made with `GITHUB_TOKEN` fires no `on: push` workflow, so the dispatch is what creates the tag. One dispatch is enough: a release commit is built exactly once, by `release.yml`, which tags that single build `vX.Y.Z`, `X.Y`, `latest` **and** `main` / `sha-<7>`; `publish.yml` recognises the `chore(main): release ` commit subject and steps aside, on the scheduled path and on the manual one alike (so keep the generated title when merging the release PR — `gh pr merge --squash` does). The consequence to know: at a release commit `:main` and `:latest` are the same digest and `/api/version` reports `vX.Y.Z+sha` on both; the next ordinary merge moves `:main` back to `main+sha`. Because two workflows can now push that mutable tag, `build-push.yml` drops `main` from its tag list when the commit it built is no longer the head of `main` (an overtaken release build must not pull `:main` backwards); the immutable tags are always pushed. The gaps are covered automatically: `release.yml` publishes the edge images itself when release-please cut no release, and `release-cut.yml` dispatches `publish.yml` when its own dispatch of `release.yml` failed after the merge. If the release build fails outright, `gh workflow run publish.yml --ref main` moves the edge tags by hand (a manual dispatch has no `head_commit`, so it never skips). To cut earlier, run the job by hand (`gh workflow run release-cut.yml`); the daily run then finds nothing. A bare `gh pr merge` on the release PR is refused: the bot's PR gets no check of its own (its `pull_request` runs wait for an approval), and the ruleset requires them, so `release-cut.yml` brings the release branch up to date, approves the `pull_request` runs waiting on that head, waits for the CI one and merges that exact commit (a CI run dispatched on the branch does not count: its jobs pass on the same commit and the pull request stays blocked). `gh workflow run release-cut.yml -f dry_run=true` goes through all of that and merges nothing (it does bring the release branch up to date and approve its runs): run it after a change to that workflow or to the ruleset. The merge creates tag `vX.Y.Z`, the GitHub Release and publishes the images as `vX.Y.Z`, `X.Y`, `latest`, `main` and `sha-<7>`. Never tag by hand, never edit `CHANGELOG.md` or `.release-please-manifest.json` by hand; the commit title decides the bump, so a `!` or `BREAKING CHANGE:` footer belongs on the squash commit. Repository setting required: Actions → General → "Allow GitHub Actions to create and approve pull requests" (set 2026-09-05; without it the bot cannot open the release PR).
- **Repository settings that live outside the code** (set 2026-09-07, Settings → Rules / Actions): ruleset "protect main" — no direct pushes (every change is a squash-merged PR, zero approvals required because the owner is the only collaborator), no force-push, no deletion, linear history, and, switched on right after the PR that added `scripts/merge-when-green.sh` (2026-10-07), the CI jobs `Python Lint`, `Go Build` and `Docker Build` required on a branch that is up to date with `main` (a merge queue would do this without serialising merges, but it is not offered to repositories of a personal account; the release PR gets its checks from `release-cut.yml`, which approves the `pull_request` runs waiting on its head before merging); ruleset "protect release tags" — `v*` tags cannot be moved or deleted (release.yml still creates them); Actions restricted to GitHub-owned and verified creators plus the eight actions the workflows use (`actions/permissions/selected-actions`; a new third-party action must be added there or its job fails with "not allowed"). Fork PRs from first-time contributors wait for approval before workflows run. No collaborators, no deploy keys, no repository secrets (GHCR uses `GITHUB_TOKEN`); keep it that way, and check with `gh api repos/Tauri-EPO/whatsapp-mcp/rulesets` and `gh api repos/Tauri-EPO/whatsapp-mcp/collaborators`. The v1.0.0 notes cover everything since upstream's v0.6.0, so issue links in that first entry may point at upstream numbers; later entries are ours only. `pyproject.toml` keeps a nominal version. `/api/version` and the MCP `version` report `VERSION+sha` (`main+sha` for edge images, `vX.Y.Z+sha` for releases).

## 5. Local commands and tooling

```bash
# Python MCP server
cd whatsapp-mcp-server
uv sync --extra dev
uv run ruff format . && uv run ruff check . && uv run pyright   # pyright: basic mode, tests excluded
uv run pytest -q
uv run main.py                                   # stdio; WHATSAPP_MCP_TRANSPORT=http for HTTP

# Go bridge — pure Go (modernc.org/sqlite, FTS5 built in); no cgo, no C toolchain
cd whatsapp-bridge
go run .
go vet ./... && go test ./...
go test -race ./...                              # CI runs it too (§6); needs cgo, ~10 s
golangci-lint run                                # config in whatsapp-bridge/.golangci.yml; fails on unformatted files

# Containers (both components, MCP over streamable HTTP) — see docs/DOCKER.md
docker compose up -d --build
docker compose logs -f bridge                    # QR code on first run
```

The MCP server is a real package (issue #409): `pyproject.toml` declares `[build-system]` with setuptools, so `uv sync` installs it into the local venv as an editable install and `uv build --wheel` ships exactly the `[tool.setuptools] py-modules` list (a local check; the release artifacts stay the container images, so `version` in `pyproject.toml` remains nominal) — the image is unaffected, its `uv sync --no-install-project` still skips the project and the Dockerfile copies `*.py` itself.

**Windows without a Go toolchain** (the primary dev box): build, test and lint the bridge inside Docker, mounting the module cache. From Git Bash with `MSYS_NO_PATHCONV=1`:

```bash
docker run --rm -v "$PWD/whatsapp-bridge:/src" -v "$USERPROFILE/go/pkg/mod:/go/pkg/mod" \
  -v wamcp-gobuild:/root/.cache/go-build -w /src golang:1.27-alpine \
  sh -c 'go vet ./... && go test ./...'
# The race detector, which CI also runs (§6). -race needs cgo, and the alpine
# image ships no C compiler, so install one first; -count=3 is what shakes out
# the races that only appear when a leaked goroutine outlives its test.
docker run --rm -v "$PWD/whatsapp-bridge:/src" -v "$USERPROFILE/go/pkg/mod:/go/pkg/mod" \
  -v wamcp-gobuild:/root/.cache/go-build -w /src golang:1.27-alpine \
  sh -c 'apk add --no-cache gcc musl-dev && go test -race -count=3 ./...'
docker run --rm -v "$PWD/whatsapp-bridge:/src" -v "$USERPROFILE/go/pkg/mod:/go/pkg/mod" \
  -w /src golangci/golangci-lint:v2.14.0 golangci-lint run
```

**gofmt on Windows.** `golangci-lint run` fails on a file `gofmt` would rewrite (the `gofmt` formatter in `.golangci.yml`), and `gofmt` also rewrites line endings — so running it on this CRLF working copy reports every file and would commit whole-file churn. Check it on an LF copy of the tree instead, and apply what it reports with an editor, not with `gofmt -w` on the working copy. The copy goes to a directory named after the worktree and is rebuilt from scratch on every run, so two worktrees side by side never share it and a file deleted or renamed in the branch cannot be linted from an earlier run:

```bash
# LF copy of the working tree (committed, modified and new-but-untracked files; .gitignore respected),
# in a directory Docker Desktop can mount (not /tmp), one per worktree
LF="../lf-check-$(basename "$PWD")" && rm -rf "$LF" && mkdir -p "$LF"
git ls-files -z --cached --others --exclude-standard whatsapp-bridge | while IFS= read -r -d '' f; do
  [ -e "$f" ] && mkdir -p "$LF/$(dirname "$f")" && tr -d '\r' < "$f" > "$LF/$f"; done
if [ -f "$LF/whatsapp-bridge/go.mod" ]; then
  MSYS_NO_PATHCONV=1 docker run --rm -v "$(cd "$LF" && pwd -W)/whatsapp-bridge:/src" \
    -w /src golang:1.27-alpine gofmt -s -l .
else echo "the LF copy is empty: run this from the repository root"; fi
```

The copy strips every CR from every file under `whatsapp-bridge/`, which is right while they are all text; a binary fixture added there later would need to be excluded. Delete `../lf-check-*` when the worktree goes away.

(`[ -e "$f" ]` skips a tracked file you deleted and have not committed yet. To check exactly what is committed, use `git -c core.autocrlf=false -c core.eol=lf archive --format=tar HEAD whatsapp-bridge | tar -x -C "$LF"` instead of the loop.)

**Several sessions at once.** The commands above share the host module cache and the `wamcp-gobuild` volume, so two sessions at the same time would write the same cache. Give each session its own two volumes (the LF directory is already per worktree) and nothing is shared; `$S` is any name unique to the session:

```bash
S=mylane
V="-v wamcp-gomod-$S:/go/pkg/mod -v wamcp-gobuild-$S:/root/.cache/go-build"
docker run --rm -v "$PWD/whatsapp-bridge:/src" $V -w /src golang:1.27-alpine \
  sh -c 'go vet ./... && go test ./...'
docker run --rm -v "$PWD/whatsapp-bridge:/src" $V -w /src golang:1.27-alpine \
  sh -c 'apk add --no-cache gcc musl-dev && go test -race -count=3 ./...'
# golangci-lint runs on the LF copy built above (not on the CRLF tree), same cache volumes:
docker run --rm -v "$(cd "$LF" && pwd -W)/whatsapp-bridge:/src" $V -w /src \
  golangci/golangci-lint:v2.14.0 golangci-lint run
```

The price is a cold module download the first time a session uses its volumes; remove them when the session ends (`docker volume rm "wamcp-gomod-$S" "wamcp-gobuild-$S"`).

`-s` matters: golangci-lint's `gofmt` formatter simplifies by default, so a plain `gofmt -l` can be silent on a file the Go Build job rejects.

`git diff --stat` after the edit must show only the hunks gofmt asked for. Note that gofmt reformats doc comments too, and turns a `''` in one into a typographic `”`: reword the comment rather than commit the curly quote.

Working-copy files are CRLF (`core.autocrlf=true`); commits are LF. `*.sh` and `Dockerfile` are forced LF by `.gitattributes`. When editing files programmatically, read with universal newlines and write `\n`. Prefer writing whole files or line-anchored edits over shell heredocs containing backslash escapes.

## 6. CI gates

Every PR runs `.github/workflows/ci.yml` and `security.yml` (a newer push cancels the run in flight). All of these must be green before merging (the informational ones too: investigate, do not ignore):

| Job | What |
|---|---|
| Python Lint | `uv sync --frozen --extra dev`, `ruff check`, `ruff format --check`, `pyright` (basic mode, `tests/` excluded: they use duck-typed fakes), `pytest` (one job, one toolchain setup) |
| Go Build | `go build`, `go vet`, `go test` (again under `TZ=America/Sao_Paulo`), `go test -race` as its own step, then golangci-lint v2.14.0 with `whatsapp-bridge/.golangci.yml`: linters `errcheck`, `govet`, `ineffassign`, `unused`, `staticcheck`, `gosec`, `misspell`, `nolintlint` and the `gofmt` formatter (an unformatted file fails the job — see §5 for checking it from a CRLF checkout). Suppress a finding only with `//nolint:<linter> // <why>` on the line; `nolintlint` rejects a bare `//nolint`, a directive without a reason, and one that no longer silences anything, so delete a suppression once its finding is gone |
| CodeQL (Python, Go) | security scanning on PRs and weekly on `main`; `"host" in list` style asserts trip `py/incomplete-url-substring-sanitization`, use set comparisons in tests |
| Bandit, pip-audit, govulncheck, Trivy image scan | `continue-on-error`; read the output anyway. Trivy scans the freshly built images on PRs and the published `:main` tags weekly (HIGH/CRITICAL, fixed only), report in the job summary |
| Docker Build | both images build with buildx (GHA cache); smoke: bridge starts and reports the FTS state, every module in `py-modules` imports inside the image (the arm64 bridge has the next row) |
| Bridge arm64 (QEMU) | the arm64 bridge image starts under QEMU and reports the FTS state; the whole Go test suite runs as an arm64 binary in that image (about 10 minutes cold, parallel to the other jobs; the image cross-compiles on the native builder; runtime smoke and tests still execute under QEMU); QEMU actions are on the allowed list (`docker/setup-qemu-action`) |

`publish.yml` pushes the `main` and `sha-<7>` images to GHCR on every merge to `main` except the release commit (§4); `release.yml` (release-please) maintains the release PR and, when it merges, builds that commit once and tags it `vX.Y.Z` / `X.Y` / `latest` / `main` / `sha-<7>` through the reusable `build-push.yml`; `release-cut.yml` merges that PR once a day. Dependabot auto-merge was removed; merge its PRs through the normal routine.

## 7. Environment variables

| Variable | Default | Purpose |
|----------|---------|---------|
| `WHATSAPP_STORE_DIR` | `store` (bridge, relative to cwd), `../whatsapp-bridge/store` (MCP) | Directory for `whatsapp.db`, `messages.db`, media, `.bridge-token`, `.bridge.lock`, `.session-keepalive` (`store_dir.go`: `storeDir()`, `storePath()`). Set for both processes; the compose file uses `/app/store` |
| `WHATSAPP_DB_PATH` | `$WHATSAPP_STORE_DIR/messages.db` | SQLite path used by the MCP server (overrides the store dir). Opened read-only; a missing file is an error naming this path, never a new empty database |
| `WHATSMEOW_DB_PATH` | `$WHATSAPP_STORE_DIR/whatsapp.db` | whatsmeow SQLite (LID ↔ phone resolution via `whatsmeow_lid_map`); overrides the store dir. Opened read-only; the tools that use it work without it |
| `WHATSAPP_API_URL` | `http://localhost:8080/api` | Bridge REST endpoint |
| `WHATSAPP_BRIDGE_TIMEOUT_S` | `30` | Timeout per MCP → bridge REST call (`whatsapp._bridge_request`); uploads/downloads use 120 s. Connection errors retry twice with backoff, read timeouts never (a POST is not re-sent) |
| `WHATSAPP_BRIDGE_BIND` | `127.0.0.1` | Bridge REST listen address; `0.0.0.0` / `::` for other containers or hosts (`rest_bind.go`) |
| `WHATSAPP_BRIDGE_ALLOWED_HOSTS` | *(loopback only)* | Extra `Host` values accepted by the bridge (`host` any port, `host:port` exact, `*` any). Same semantics as `WHATSAPP_MCP_ALLOWED_HOSTS`; loopback spellings always included; a non-loopback bind without it stays loopback-only (403) |
| `WHATSAPP_BRIDGE_PORT` | `8080` | Port the bridge listens on |
| `WHATSAPP_OPERATOR_BIND` | empty (off) | Separate operator listener: one explicit IP or hostname resolving to one private network address; wildcard binds refused. Bridge REST must remain loopback. |
| `WHATSAPP_OPERATOR_PORT` | `8090` | Operator port; no host publication in compose. |
| `WHATSAPP_OPERATOR_TOKEN` | required when enabled | At least 32 random bytes encoded as 64 hex or 43-256 unpadded base64url characters; low-entropy/repeated values refused. Generate with `openssl rand -hex 32`. Distinct from the effective bridge token, including its stored fallback. |
| `WHATSAPP_OPERATOR_TOKEN_FILE` | empty | Alternative owner-only regular token file; symlinks, permissive modes and oversized files refused. Set token or file, never both. |
| `WHATSAPP_OPERATOR_ALLOWED_HOSTS` | loopback hosts | Explicit comma-separated operator Hosts (`host` or `host:port`); wildcard refused. Browser Origin must match the listener's scheme and Host; native clients may omit it. |
| `WHATSAPP_PAIRING_STDOUT` | `false` with operator enabled, otherwise `true` | Draw QR codes on stdout. False also suppresses SDK DEBUG logs (QR payloads and raw protocol frames), while bridge/database DEBUG logs remain available. Set false for operator HTTP or exported logs; the operator compose override defaults false. |
| `WHATSAPP_BRIDGE_TOKEN` | generated next to `WHATSMEOW_DB_PATH` as `.bridge-token` | Bearer token required for bridge REST calls; also signed onto outbound webhooks |
| `WHATSAPP_MEDIA_AUTODOWNLOAD` | `true` | Cache inbound media on arrival and retain uploaded plaintext bytes after successful bridge sends; both honor `WHATSAPP_MEDIA_MAX_BYTES` (sent files check actual byte size). `false` = fetch only on `/api/download` (`media_retention.go`). That includes the image the webhook payload would carry: with `false` the event goes out without `mediaBase64` and nothing is written (issue #484). `1/true/yes/on` or `0/false/no/off`; anything else stops the bridge (`env_bool.go`) |
| `WHATSAPP_MEDIA_AUTODOWNLOAD_STATUS` | `false` | Cache the media of status updates (`status@broadcast`) on arrival and after a successful status send. Off by default: the row is stored with its CDN fields, nothing is written under `store/status@broadcast/`, a status image forwarded to the webhook (`WEBHOOK_FORWARD_STATUS`) goes without its bytes, and `/api/download` still fetches a file on demand (`skipsStatusMedia` in `media_retention.go`, issue #447). `true` caches the status feed like any chat; `WHATSAPP_MEDIA_AUTODOWNLOAD=false` wins over it, on the webhook path too. Same strict boolean parse as `WHATSAPP_READ_ONLY` |
| `WHATSAPP_MEDIA_MAX_BYTES` | `268435456` (256 MiB) | Automatic caching checks declared and actual bytes; while a nonzero cap is set an undeclared length is skipped; an explicitly empty file is allowed (`/api/download` still fetches them); `0` = no limit. A decimal unsigned integer (up to `18446744073709551615`); malformed values stop startup. Downloads stream to `<file>.part` then rename (`media.go`) |
| `WHATSAPP_MEDIA_RETENTION_DAYS` | *(unset)* | Daily sweep deletes media files older than N days (`0`–`106751`; `0` keeps forever) under `store/<chat>/`; DB rows untouched |
| `WHATSAPP_GROUP_ROSTER_SYNC_HOURS` | `6` | How stale a cached group roster may get before the bridge refreshes it in the background (`group_events.go`), one group per second, only while connected. A number of hours too large to represent as a duration is refused at startup. `0` turns the pass off, leaving `group_members` fed only by `/api/group/members`, group events and group messages. It is a read (`GetGroupInfo`), so it keeps running under `WHATSAPP_READ_ONLY` |
| `WHATSAPP_SESSION_KEEPALIVE_HOURS` | `12` | How often the bridge marks its linked device available for a few seconds and unavailable again (`session_keepalive.go`). WhatsApp logs a linked device out about a month after it was last "opened", and a connection does not count as opening: only this presence blip moved the date the phone shows (issue #576). `0` turns it off, and the bridge is then logged out a month after pairing; more than `168` (a week) is refused at startup. The first blip comes one to two minutes after the session is connected and logged in, never while the QR code is showing; the interval is wall-clock time and the last successful blip is remembered across restarts in `.session-keepalive` (0600). Missing or invalid state means never. For those seconds the account shows as online and the phone may hold a notification back. It keeps the session, it does not act on a chat, so it runs under `WHATSAPP_READ_ONLY` |
| `WHATSAPP_MEDIA_ROOTS` | `~/.local/share/whatsapp-mcp/outbox` | Path-list of directories allowed for outbound media files (bridge). The MCP server reads it too: `media_base64` uploads and ffmpeg voice-note conversions are written under `<first root>/.uploads` (`media_upload.py`) so the bridge may read them, and removed after the send. Set the same value for both processes; compose passes `/app/outbox` to both |
| `WHATSAPP_EXPORT_DIR` | `$WHATSAPP_STORE_DIR/exports` | Directory `export_messages` writes NDJSON archives into (`export.py`). `out_path` is resolved under it and anything escaping it (`..`, an absolute path elsewhere, a symlink pointing out) is refused with `denied`. Compose leaves it at `/app/store/exports`, inside the `whatsapp-store` volume |
| `WHATSAPP_DEVICE_NAME` | `whatsmeow` (whatsmeow default) | Linked-device label shown in WhatsApp > Linked Devices. Applied at pair time only; re-pair to change |
| `WHATSAPP_ALLOWED_CHATS` | *(unset = all chats)* | Conversation allow-list (JIDs, bare numbers, `*@g.us` / `*@s.whatsapp.net`). MCP filters restricted reads and refuses other tool targets; bridge handlers use `authorizeChat`, including history/download. Group add/remove/promote/demote requires every participant too (known phone/LID twins and Brazilian mobile ninth-digit spellings); an outside participant refuses the whole batch. Message webhooks check this boundary before building payloads or reading media, with feed/self opt-ins still required. Malformed targets are refused even when unset; malformed entries stay restrictive and startup warns about positions without values. Set for both processes |
| `WHATSAPP_READ_ONLY` | *(unset = everything enabled)* | Read-only deployment: the MCP server omits every mutating tool from `tools/list` and refuses it with `denied` if called anyway (`tool_policy.py`, `@mutating_tool`); the bridge answers 403 on the matching endpoints (`read_only.go`). Reads, `download_media`, `transcribe_audio`, `annotate_media` and `clear_media_refusal` (local notes.db) stay available. `1/true/yes/on` or `0/false/no/off`; anything else stops the process. Set for both processes |
| `WHATSAPP_ALLOW_TOOLS` | *(unset = every tool)* | Allow-list of tool names (comma-separated): only these are offered, reads included (`tool_policy.py`). The bridge maps the names to the endpoints they call (`endpointTools` in `tool_policy.go`) and answers 403 on the rest; reads stay open there, as in read-only mode. Unknown names stop the process with the valid list. Set for both processes |
| `WHATSAPP_DENY_TOOLS` | *(unset)* | Deny-list of tool names, enforced in both processes. Wins over `WHATSAPP_ALLOW_TOOLS`; `WHATSAPP_READ_ONLY` wins over both (the three filters only ever remove capability). Unknown names stop the process. The bridge check is per endpoint, so an endpoint shared by several tools (`/api/send`) stays open while any of them is allowed |
| `WHATSAPP_WRAP_UNTRUSTED` | *(unset = off)* | MCP-server-only: wrap third-party text in the results (`content`, `last_message`, transcripts, note values, group `topic`, poll `question`) in `<untrusted>…</untrusted>` delimiters, so a model that skipped the tool description still sees a boundary (`untrusted.py`). JIDs, IDs, timestamps and cursors are untouched. Name fields are never wrapped in either mode: they are sanitised instead, always (control, zero-width and bidi characters stripped, capped at 200 characters, issue #273); `topic` and `question` are sanitised too, keeping their line breaks and capped at 4096 characters (issue #332). Same strict boolean parse as `WHATSAPP_READ_ONLY`. A hint, not a control — the enforced mitigations are `WHATSAPP_READ_ONLY` and `WHATSAPP_ALLOWED_CHATS` |
| `WHATSAPP_LOG_LEVEL` | `INFO` | Bridge log level (`DEBUG`/`INFO`/`WARN`/`ERROR`), applied to the bridge logger and the whatsmeow client. `DEBUG` echoes each stored message |
| `WHATSAPP_LOG_FORMAT` | `text` | `json` switches the bridge (and whatsmeow) log lines to one JSON object per line (`ts`, `level`, `module`, `msg`) (`logging_json.go`). In `text` each physical line has the real timestamp/module/level, continuations are marked `[continued]`, controls and bidi overrides are escaped, and literal backslashes are doubled. Lines are capped at 8 KiB including prefix/newline with `[truncated]`; stacks remain readable (`textLogger`, `logging.go`). In `json` the characters `json.Marshal` leaves as they are and a terminal acts on (DEL, the C1 controls, the reordering controls) are written as JSON escapes; the decoded `msg` is unchanged |
| `WHATSAPP_METRICS` | `true` | Serve `GET /metrics` on the bridge (Prometheus text, unauthenticated like `/api/version`: counters, connection state and fixed-label database pool statistics only, `metrics.go`); `false` removes the route. Anything that is not a boolean stops the bridge |
| `WHATSAPP_MCP_LOG_LEVEL` | `INFO` | MCP server log level (stderr) |
| `WHATSAPP_MCP_LOG_FORMAT` | `text` | `json` switches the MCP server stderr log to one JSON object per line (`observability.py`) |
| `WHATSAPP_MCP_METRICS` | `true` | Serve `GET /metrics` on the `http`/`sse` transports (tool calls/errors/seconds per tool, the `whatsapp_mcp_tool_duration_seconds` histogram, HTTP requests by status class); `false` disables it |
| `WHATSAPP_MCP_METRICS_TOKEN` | *(unset = open)* | Bearer token required on the MCP `/metrics` only (401 otherwise); set it when the MCP port is reachable beyond the tailnet (Funnel). Independent of `WHATSAPP_MCP_TOKEN` |
| `WHATSAPP_MCP_TRANSPORT` | `stdio` | MCP transport: `stdio`, `http`, or `sse` |
| `WHATSAPP_MCP_HOST` | `127.0.0.1` | Bind address for the `http`/`sse` transports |
| `WHATSAPP_MCP_PORT` | `8000` | Port for the `http`/`sse` transports |
| `WHATSAPP_MCP_ALLOWED_HOSTS` | loopback only | Extra `Host` header values accepted by the `http`/`sse` transports (comma-separated; bare hostnames match any port; `*` disables the check). Unset + non-loopback bind disables the check with a warning |
| `WHATSAPP_MCP_ALLOWED_ORIGINS` | derived from allowed hosts | Extra `Origin` header values for browser-based MCP clients |
| `WHATSAPP_MCP_RATE_LIMIT` | `120` with a token, `0` without | Requests/minute per socket peer (or verified forwarding chain) on `http`/`sse`; token bucket in `http_auth.RateLimitMiddleware`, 429 + Retry-After; `0`/`off` disables |
| `WHATSAPP_MCP_TRUSTED_PROXIES` | *(unset = trust none)* | MCP-only comma-separated CIDRs, `loopback` (`127.0.0.0/8` and `::1/128`), or `gateway` (one default-route gateway resolved from `/proc/net/route`; unavailable/ambiguous stops startup). Use gateway only with a loopback-bound published MCP port. Only a trusted socket peer may supply `X-Forwarded-For`; the rightmost untrusted hop keys the limiter. Invalid chains fall back to the socket peer; invalid configuration stops startup. Uvicorn forwarding is disabled so the middleware sees the real socket peer |
| `WHATSAPP_MCP_MAX_BODY_BYTES` | `4194304` | Max request body for `http`/`sse` (passed to the SDK app) |
| `WHATSAPP_MCP_UPLOAD_MAX_BYTES` | `67108864` (64 MiB) | Maximum raw file bytes streamed into `POST /upload` on `http`/`sse` (positive integer, at most 268435456 / 256 MiB); independent of the JSON-RPC body limit. Uploads expire after one hour and are swept at startup, on new uploads and every minute; a fixed 256 MiB shared outbox budget also bounds upload writes. Sent files are removed after success; failures preserve IDs until expiry |
| `WHATSAPP_MCP_TOKEN` | bridge token when bound off-loopback; none on loopback | Static bearer token enforced on the `http`/`sse` transports (`http_auth.resolve_http_token`, min 16 chars). Unset + non-loopback bind → reuses the bridge token (env or `.bridge-token`); `off` disables auth explicitly. stdio unaffected |
| `WHATSAPP_PUBLIC_URL` | *(unset)* | MCP-server-only: the URL clients use to reach this server (`https://host.tailnet.ts.net/mcp`; a bare `host` / `host:port` also works). Set it and `bridge_status` reports `endpoint_cert_expires_at` / `endpoint_cert_days_left` for that endpoint's certificate, and `endpoint_cert_error` when the handshake fails (`endpoint_cert.py`: one outbound TLS handshake, no HTTP request, 3 s, chain verified with the default context, result cached an hour). Unset = no fields, no probe |
| `WEBHOOK_URL` | `http://localhost:8769/whatsapp/webhook` | Outgoing webhook for incoming messages (empty falls back to this default) |
| `WEBHOOK_ENABLED` | `true` (compose: `false`) | Set to `false` to disable outbound webhooks entirely. Anything that is not a boolean stops the bridge |
| `FORWARD_SELF` | `true` (compose: `false`) | Whether self-sent messages are forwarded to the webhook. Anything that is not a boolean stops the bridge |
| `WEBHOOK_FORWARD_STATUS` | `false` | Forward status updates (`status@broadcast`: text, images, reactions) to the webhook too. Off by default: the webhook is for conversations, and the feed is one event per status post of every contact (`forwardsToWebhook` in `webhook.go`, issue #482). The posts are stored either way. With it on, a status image carries its bytes only when `WHATSAPP_MEDIA_AUTODOWNLOAD_STATUS` is on as well. Anything that is not a boolean stops the bridge |
| `WEBHOOK_FORWARD_CHANNELS` | `false` | Forward channel posts (`@newsletter`) to the webhook too. Text, images and reactions require this opt-in; rows are stored either way. A boolean; anything else stops the bridge |
| `WEBHOOK_FORWARD_BROADCASTS` | `false` | Forward broadcast-list messages (`@broadcast`, except `status@broadcast`) to the webhook too. Text, images and reactions require this opt-in; rows are stored either way. Status posts still need `WEBHOOK_FORWARD_STATUS`. A boolean; anything else stops the bridge |
| `WHATSAPP_PARENT_WATCHDOG_S` | `30` | Stdio parent-liveness poll interval (seconds) |
| `WEBHOOK_FORWARD_CONNECTION_EVENTS` | `false` | POST safe connection-state transitions to `WEBHOOK_URL` when `WEBHOOK_ENABLED` is on; disconnects wait five seconds and are cancelled on a quick reconnect. Logout finishes a POST bounded to two seconds before exit. No QR, passkey material, phone/JID or message content. Strict boolean parsing |
| `WHISPER_URL` | *(unset)* | whisper.cpp `whisper-server` inference endpoint for `transcribe_audio` (`transcribe.py`). Wins over `WHISPER_BIN` |
| `WHISPER_BIN` / `WHISPER_MODEL` | *(unset)* | Local `whisper-cli` binary + `ggml-*.bin` model, alternative backend |
| `WHISPER_LANGUAGE` | `pt` | Default transcription language; `auto` to detect |
| `WHISPER_TIMEOUT_S` | `300` | Per-transcription timeout (seconds) |
| `TRANSCRIBE_ON_INGEST` | *(unset = off)* | MCP-server-only: background thread that transcribes inbound voice notes as they arrive (`transcribe_worker.py`) instead of waiting for an agent to call `transcribe_audio`. Reads `messages.db`, writes the same `transcript` / `transcript_lang` / `transcript_backend` notes into `notes.db`, idempotent by sha256, concurrency one. Costs CPU on this machine; with no whisper backend configured it stays off with a warning, and so it does when the tool policy does not offer `transcribe_audio` (a policy that hides `download_media` only turns the fetch path off). The status feed (`status@broadcast`) is not walked: its voice notes are neither fetched nor transcribed in the background, whatever `WHATSAPP_MEDIA_AUTODOWNLOAD_STATUS` says, and `coverage().audio` leaves them out (issue #447). Same strict boolean parse as `WHATSAPP_READ_ONLY` |
| `TRANSCRIBE_ON_INGEST_INTERVAL_S` | `300` | Seconds between batches (values below 5 are raised to 5) |
| `TRANSCRIBE_ON_INGEST_BATCH` | `10` | Voice notes transcribed per batch (capped at 200). A file the backend cannot read gets a `transcript_error` note and is not tried again until that note is cleared; a backend that cannot be reached at all (`BackendUnavailableError`: refused, 502/503/504, a misrouted `WHISPER_URL`, no binary or model) writes no note, three of those in a row end the round with a warning, and the notes an older build wrote for such an outage are cleared when the worker starts |
| `TRANSCRIBE_ON_INGEST_FETCH` | *(unset = off)* | Let the ingest worker ask the bridge (`/api/download`, the path `transcribe_audio` uses) for audio whose bytes are not cached, instead of skipping it — what an archive with `WHATSAPP_MEDIA_AUTODOWNLOAD=false` or a retention sweep needs. `TRANSCRIBE_ON_INGEST_BATCH` bounds the attempts, failures included, and the downloaded bytes stay in the store like any other download. A file the bridge cannot send this time is skipped without a note (the next round retries it); three failures in a row end the fetching for that round. A file the bridge answers `media_unavailable` for (the sender's phone was asked to re-upload and said the media is gone, or the row carries no CDN fields to download with) gets a dated `media_unavailable` note, leaves the work list for good and costs no strike (`coverage().audio.unavailable`). An unsafe identity gets a per-message `media_refused` record in `notes.db` (`coverage().audio.refused`), clearable with `clear_media_refusal` or a successful fetch; other copies remain eligible. Same strict boolean parse as `WHATSAPP_READ_ONLY` |
| `FFMPEG_TIMEOUT_S` | `120` | Timeout for each ffmpeg conversion (voice-note encode in `audio.py`, 16 kHz WAV prep in `transcribe.py`) |

Compose-only knobs (`WHATSAPP_MCP_BIND`, `WHATSAPP_OUTBOX`) are documented in `.env.example` and `docs/DOCKER.md`. The whisper server is not part of the compose file: `WHISPER_URL` names one the operator runs (`docs/DOCKER.md`, "Voice-note transcription").

When adding a new env var: document it here, in `docs/CONFIGURATION.md`, in `.env.example`, and pass it through in `docker-compose.yml` when a container needs it. The README only lists the day-one essentials.

Compose-only operator knobs: `WHATSAPP_OPERATOR_NETWORK` names an existing private network and `WHATSAPP_OPERATOR_ALIAS` is unique per instance in `docker-compose.operator.yml`. Neither is a process setting.

## 8. Gotchas (read before editing)

1. **JIDs.** WhatsApp identifies users as `1234567890@s.whatsapp.net` (DM), `123456@g.us` (group), and `<random>@lid` (link-ID, anonymous). The bridge maintains a phone↔LID map in `whatsapp.db.whatsmeow_lid_map`. Many "user is missing" / "messages don't show" bugs trace back to JID-form mismatches. Always think about both forms (`resolveUserJID`, `resolveQuotedParticipantJID`, `resolveMentionJIDs`). A phone number as typed is not always the registered one either (a Brazilian mobile with or without its ninth digit): `/api/send` and `/api/forward` ask WhatsApp on a LID-map miss (`canonicalRecipientJID`, issue #444) and checks `WHATSAPP_ALLOWED_CHATS` on the number as normalised at the request boundary before the lookup and on the registered one after it, refusing the send when a bridge with an allow-list gets no answer — an endpoint that starts resolving numbers has to do all three. Positive registered-number answers are cached on the Bridge for one hour (256 entries, cleared on Connected / Disconnected / LoggedOut); both allow-list checks still run on every hit. New endpoints must reuse this cache and never cache an authorization decision. An endpoint accepting a bare phone recipient normalises once, at its boundary, before the allow-list check. `messages.sender` holds the bare user part and `messages.sender_server` the namespace it belongs to (`s.whatsapp.net` / `lid`, NULL when unknown — rows written before the column existed, and senders that are not user JIDs): a bare number does not say which one it is, and a 15-digit LID reads exactly like a phone number. Write paths pass the full resolved JID to `StoreMessage`, which splits it (`sender_namespace.go`); readers keep using the bare `sender`. Every chat-taking HTTP handler must call `authorizeChat` before effects and use its returned canonical JID (trimmed, server lower-cased). It refuses missing user/server, extra `@` parts and device-suffixed chat targets before checking the canonical identity. Metadata and internal LID lookups keep their caller-specific device keys; `parseRecipientJID` interprets spelling and does not validate input. Never parse first and authorize only `String()`; `ParseJID` has already dropped extra `@` parts and `String()` hides an empty user.
2. **Message IDs are unique per chat, not globally.** The `messages` primary key is `(id, chat_jid)`. Always pass `chat_jid` alongside an ID; forwards reuse IDs across chats.
3. **Pointer rows.** `reaction` and `poll_vote` messages refer to another message via `messages.target_message_id` (the bridge also writes it to `filename` for one release; readers use `_target_id()` which falls back to `filename` for pre-migration rows). Do not add new meanings to `filename`.
4. **Media files.** The shared message upsert is no longer last-write-wins: `StoreMessage` cannot clear or partially correct existing media credentials, nor move a media snapshot's timestamp, without `mediaComplete`. A row with no credentials may accept its first partial snapshot. An incomplete write with no caption preserves a stored media caption. Dedicated changes use an explicit `UPDATE`, as `media_retry.go` does; a complete write with an omitted direct path clears that path. `media_presentation` stores validated MIME, document name/title, audio PTT/seconds/waveform, sticker animation and the plaintext file SHA256. It is co-upserted with credentials, bounded on ingress and again on forwarding (stored display names/titles 200 characters with controls/bidi removed and punctuation retained; wire filenames also remove paths, waveform exactly 64 bytes, seconds 0–86400, category-valid MIME with the exact audio/ogg; codecs=opus exception). A mismatching hash or invalid JSON falls back to legacy forwarding; do not trust metadata left by an older image for another file. The LID row copy carries it and direct_path together. Media files live under `store/{chat_jid}/` with timestamp + message-ID filenames. Use `/api/download`, never hand-built paths. A download asks the CDN for `messages.direct_path`, the message's own direct path (whatsmeow downloads by that field alone); a row without it falls back to the path cut out of `url`. On a CDN refusal the path cut out of `url` is tried once when it names something else; after that, a refusal of a message inside `cdnFreshWindow` (six hours, `media_retry.go`) whose link is not stamped as expired is a retryable 502 that names the status, never an expiry: no media retry, no `media_unavailable`. The write goes through `Bridge.StoreRoot` (`os.Root`) like the delete does, after `checkMediaPathComponents` refused a chat directory or a file name that is not one plain component (issue #453), and it follows no symlink, not even one that stays inside the store (`Lstat`, `O_EXCL`): a new step on that path uses the root and a store-relative path, never `os.*` on a joined one. Whether a file is cached is one question with one answer for download, purge, webhook, retention and media usage: ask `findCachedMedia` / `openChatMediaDir` / `eachCachedMedia` (`media_cache_path.go`), do not write another `Stat`. CDN URLs expire (403/404/410 after days); `downloadMedia` runs one media-retry round trip against the sender's phone (`media_retry.go`) before failing. Only one transfer per destination file ever runs (`media_inflight.go`): callers that miss the cache together share its result instead of writing the same `<file>.part`, and the transfer follows the bridge lifecycle rather than the caller that started it. Automatic caching of inbound media is background work with a budget (`media_budget.go`): four at a time, 256 queued, the overflow dropped with a WARN and a counter — the row stays, so `download_media` still fetches that file.
5. **Audio.** Voice notes must be Opus `.ogg`; `send_audio_message` converts via ffmpeg. `transcribe_audio` converts to 16 kHz WAV before whisper.
6. **History sync** is controlled by the phone. Modern syncs put the group sender in top-level `WebMessageInfo.participant`; read it before `Key.participant`. Poll votes in history cannot be decrypted (issue #59).
7. **`messages.db` is the source of truth for reads.** The MCP server must never need the bridge for read-only tools. The bridge opens the DB in WAL mode with a busy timeout; the MCP side uses a 5 s timeout via `_connect_messages_db()` / `_connect_whatsmeow_db()`, which open the files read-only (`mode=ro`, never `immutable`: WAL must be read through its log). A missing `messages.db` raises `MessagesDbNotFoundError` (a `ToolError` that is also a `sqlite3.Error`): a handler that degrades on `sqlite3.Error` must not swallow it where a wrong path should be the answer (`search_contacts` re-raises it). `notes.db` is the one database the MCP server writes, on its own connections; ATTACH it to a read connection only through `whatsapp.attach_notes_read_only`.
8. **Search index.** The bridge owns `messages_fts` (FTS5, `fts.go`) and its triggers; the driver (modernc.org/sqlite) always ships FTS5, and the startup check still *drops* the index on a build without it so writes never fail. The MCP server uses `MATCH` only when the table exists and falls back to `instr()`. Never create FTS triggers from Python.
9. **One bridge per store.** `main()` takes an exclusive OS lock on `store/.bridge.lock` (`instance_lock.go`); a second bridge exits naming the holder's PID. Tests that need concurrent bridge processes must use separate working directories.
10. **Configuration is read once.** `os.Getenv` belongs in `config.go` and the `resolve*` / `load*` / `new*` helpers it calls at startup; handlers and event paths read `Bridge` fields (`MediaRoots`, `MediaRetention`, `Webhook.enabled`, …). `storeDir()` is the one per-call read left, because tests point it at temp dirs.
    Configuration errors are collected before opening a store, creating the outbox or binding a listener; one diagnostic names all invalid variables and the process exits with status 1. Startup I/O failures also exit non-zero after cleanup.

11. **No package-level state in the bridge.** Runtime dependencies live on the `Bridge` struct (`bridge.go`); tests build one with `testBridge(...)` and override fields. The one sanctioned global is `bridgeLog` (`logging.go`), write-once configuration set by `initLogging()` in `run()` before validation; tests swap it with `installRecordingLogger(t)`. A knob a test reassigns and restores (`streamReplacedDelay` was one, issue #351) is also a data race the moment a goroutine reads it: put it on the `Bridge` and set it on the test's own instance. Same for what a test leaves running — a bridge that queued an auto-download outlives the test unless it is shut down, which `testBridge` does in a cleanup of its own (build test bridges through it), and `go test -race` is what catches both.
12. **stdout is the protocol on stdio.** Anything the MCP server prints to stdout can corrupt a stdio session; log through `logging` (stderr), never `print()`.
13. **Bridge logs go through `bridgeLog`, not `fmt.Print*`.** The text writer formats once, prefixes every physical line, marks continuations and caps each line at 8 KiB. Control and bidi overrides are escaped and backslashes doubled; values from a stanza need no quoting at the call site (`logging.go`). Levels: `Errorf` for failures that lose data, `Warnf` for degraded-but-continuing, `Infof` for lifecycle, `Debugf` for per-request traces and message echoes (user content stays out of `INFO`). Direct stdout exceptions are the first-run token banner and the pairing QR code for a human, plus the bounded state-name output of `--operator-status` for the smoke script.
14. **REST starts before pairing.** `/api/health` is liveness (200 once the listener is up, body carries `connected`/`paired`); `/api/ready` is readiness (200 only while connected). Endpoints that need WhatsApp check `client.IsConnected()` themselves.
15. **Outgoing calls are not visible to linked devices.** Don't promise features that depend on them.
16. **One timestamp spelling.** Every TIMESTAMP column the bridge writes holds `YYYY-MM-DD HH:MM:SS+00:00` (UTC, seconds, fixed width) — `dbTime()` in `store_time.go`, never a bound `time.Time`, or the driver stamps the machine's local offset and `ORDER BY timestamp` starts sorting by wall clock. Read with `parseDBTime` / `anchorTime`, which also accept the legacy spellings. A new time column goes in `canonicalTimeColumns` (and changes `canonicalTimestampsMigration` to a new name so that rewrite runs again; each data migration owns a name in `schema_migrations`, never a shared version counter; `applyNamedMigration` commits one-off effects and the marker together, while timestamps explicitly record completion after their idempotent chunks). Bounds compared against such a column must be rendered the same way on both sides.
17. **Every database handle has a pool bound.** `database/sql` opens one connection per concurrent goroutine unless told not to, and SQLite has one WAL writer, so extra connections cost memory and `SQLITE_BUSY` (issue #471). Each production `sql.Open` is followed by `boundPool` (`store_dir.go`: 4 for `messages.db` and the whatsmeow session store, 2 for the read-only contacts handle; `store_pool_test.go` scans for a missing call). A rows cursor held while the loop writes needs a second connection, a cursor plus a transaction a third in the worst case: drain a cursor before opening a transaction, and keep a cursor from living across a slow callback. Media purge drains pages of 256 rows and closes each cursor before disk callbacks. `/metrics` exposes `whatsapp_bridge_db_in_use`, `whatsapp_bridge_db_wait_total` and `whatsapp_bridge_db_wait_seconds_total` with fixed `pool="messages|session|contacts"` labels. Startup migrations detach their session alias on the same connection before it returns to the pool; single-writer redesign remains a separate decision.

## 9. Where to make changes

| You want to… | Touch |
|---|---|
| Add a module at the top level of either component | also its line in the §3 tree, and for a Python one its name in `[tool.setuptools] py-modules` (`tests/test_agents_file_tree.py` checks both and fails the Python job otherwise, the way `tests/test_env_docs.py` does for a new env var) |
| Add or modify an MCP tool | `whatsapp-mcp-server/main.py` (+ `docs/TOOLS.md`, tests) |
| Change what `resources/read` serves, or add a resource scheme | `whatsapp-mcp-server/media_resource.py` (the `mcp` instance is a `MediaResourceServer`) |
| Change agent notes (chats, contacts, messages, media) | `whatsapp-mcp-server/notes.py`, `media_notes.py` |
| Change what `list_unanswered` hides (handled, snoozed, muted, closing messages) | `whatsapp-mcp-server/triage.py` + `_closing_message_clause` in `whatsapp.py` |
| Change DB queries / dict conversion | `whatsapp-mcp-server/whatsapp.py` |
| Change HTTP transport, auth, allowed hosts | `whatsapp-mcp-server/main.py` (`__main__`), `mcp_config.py`, `http_auth.py` |
| Change the conversation allow-list | `chat_policy.py` **and** `whatsapp-bridge/chat_policy.go` |
| Add or rename an MCP tool that calls the bridge | also `endpointTools` / `unenforcedTools` in `whatsapp-bridge/tool_policy.go` (`tests/test_bridge_tool_policy.py` fails otherwise) |
| Change how tool results are marked as untrusted | `whatsapp-mcp-server/untrusted.py` (+ the allow-list in `tests/test_untrusted_content.py`) |
| Change voice-note transcription | `whatsapp-mcp-server/transcribe.py`; the whisper server itself is not in the repo (`docs/DOCKER.md` shows how to run one next to the stack) |
| Add a bridge REST endpoint | new `whatsapp-bridge/<feature>.go` with `handleX(deps…) http.HandlerFunc`, register in `newRESTMux` (`rest.go`) wrapped in `auth(requireMethod(...))`, fail with `writeError` (never `http.Error`), tests with fakes |
| Change Business label caching / chat labelling | `whatsapp-bridge/labels.go`, `events.go`, `whatsapp-mcp-server/whatsapp.py` |
| Change inbound event handling | `handleEvent` / `handleMessage` in `events.go`, `handleHistorySync` in `history_sync.go`; content extraction in `content.go` |
| Change the messages schema | `ensureMessageStoreSchema` in `store.go`; migrations idempotent (`ensureColumn`, row backfills, and independent named `schema_migrations` markers via `migration_markers.go`; use `applyNamedMigration` to commit a one-off rewrite and its marker in the same transaction; timestamps explicitly record completion after bounded idempotent chunks; never gate a rewrite on legacy `user_version`); FTS in `fts.go` |
| Change webhook payload | `whatsapp-bridge/webhook.go` |
| Change build identity (`/api/version`, MCP `version`) | `whatsapp-bridge/version.go`, `ARG GIT_SHA/VERSION` in both Dockerfiles, compose build args |
| Change configuration parsing | `whatsapp-bridge/config.go` |
| Change startup / wiring (pairing, shutdown) | `whatsapp-bridge/main.go` (keep it under ~400 lines; logic goes in a feature file) |
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
- An issue filed through the forms (`.github/ISSUE_TEMPLATE/`) carries `needs-triage` and a plain title. Triage is: add the priority prefix and label, the `area:*` label, and remove `needs-triage`.
- Larger efforts get an `epic` issue holding the checklist; §4 step 1 says how to list the open ones and names the closed ones.
- Bugs from operation: include bridge log lines, `docker compose ps`, the tool call and its result; redact phone numbers.
- "Won't do" is a valid outcome; close with a sentence explaining why.

See [`CONTRIBUTING.md`](./CONTRIBUTING.md) for the human-facing contribution guide, [`docs/DOCKER.md`](./docs/DOCKER.md) for operations and [`docs/CONFIGURATION.md`](./docs/CONFIGURATION.md) for the user-facing variable reference.
