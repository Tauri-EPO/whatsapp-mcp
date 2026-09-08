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
- **Default branch:** `main`. `main` is the deployable state. Releases are automatic (`release.yml` + `release-cut.yml`, see §4 "Versions"): every merge updates a release PR; once a day that PR is merged, which tags `vX.Y.Z` and publishes the GitHub Release and the `latest` images.
- **Lineage (for ideas, not for merging — see §2):**
  - https://github.com/verygoodplugins/whatsapp-mcp — remote `upstream` (also `vgp`). Maintained fork we started from at v0.6.0 (Sept 2026).
  - https://github.com/lharries/whatsapp-mcp — the original project (remote `lharries`; add with `git remote add lharries https://github.com/lharries/whatsapp-mcp.git` if missing).

## 2. Fork policy

**This is a hard fork.** Decided 2026-09-04: the fork is ahead of upstream (SDK v2, auth, allow-list, FTS5, polls, Docker…) and no longer merges `upstream/main`. Consequences:

- **Dependencies move here first.** Dependabot stays on; its PRs are merged once CI is green. Bumps that change the shipped images (uv, gomod, docker, docker-compose) are titled `deps:` and cut a patch release; GitHub Actions bumps stay `chore(deps)`. Python dev tooling (ruff, pytest, pyright) never reaches an image, so it arrives in its own PR (the `pip-dev-*` groups in `.github/dependabot.yml`) — Dependabot cannot title it differently, because the commit prefix is per entry and its `uv` ecosystem classifies every dependency as production ([dependabot-core#13202](https://github.com/dependabot/dependabot-core/issues/13202); `[project.optional-dependencies]` is not a development group either, [#10606](https://github.com/dependabot/dependabot-core/issues/10606)), so retitle it when merging: `gh pr merge N --squash --delete-branch --subject "chore(deps): bump the python dev tooling"`. The squash title is what release-please reads, so that keeps it out of the release. Never pin a dependency just because upstream did (`mcp<2` and `cryptography<49` were both dropped).
- **Refactors are allowed.** Keep each PR small (§4) even when the overall change is large; §4 step 1 says where the current work comes from.
- **Upstream is a source of ideas and cherry-picks, never a merge target.** When asked to "look upstream", "get ideas from the original", "check what VGP/lharries did", do this:
  1. `scripts/upstream-harvest.sh` — fetches both remotes and prints the bridge/server commits since the last harvest (`.upstream-harvest`) plus their open PRs and issues; `--all` ignores the marks. After reviewing, `scripts/upstream-harvest.sh --mark` records the new heads. (Manual equivalent: `git log --oneline main..upstream/main -- whatsapp-bridge/`; protocol/whatsmeow changes are the most valuable to harvest.)
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
│   ├── events.go               # whatsmeow event dispatch, handleMessage, calls, reconnect loop
│   ├── history_sync.go         # handleHistorySync (phone replays at pair time / on demand)
│   ├── content.go              # extract text/quotes/mentions/media/ephemeral from waE2E.Message
│   ├── jid.go                  # phone <-> LID resolution helpers
│   ├── send.go                 # /api/send types, sendWhatsAppMessage, media upload, Ogg Opus analysis
│   ├── media.go                # inbound media download into store/<chat>/
│   ├── rest.go                 # newRESTMux route table + HTTP server; handlers live next to their features
│   ├── rest_middleware.go      # writeError (JSON error shape), requireMethod, requestLog
│   ├── health.go               # /api/health (liveness), /api/ready (readiness)
│   ├── me.go                   # GET /api/me: the account's own phone JID and LID (authenticated)
│   ├── chat_actions.go         # /api/react, /api/typing
│   ├── mark_read.go            # /api/mark-read: listed IDs, or the whole chat up to a timestamp
│   ├── store.go                # MessageStore: schema, migrations, message/chat/call queries
│   ├── store_time.go           # dbTime/parseDBTime: the one UTC timestamp spelling + its migration
│   ├── mentions.go             # messages.mentions: who a message addressed + the backfill from old text
│   ├── sender_namespace.go     # messages.sender_server: which namespace a stored sender is in + its backfill
│   ├── logging.go              # bridgeLog + WHATSAPP_LOG_LEVEL
│   ├── logging_json.go         # WHATSAPP_LOG_FORMAT=json line logger
│   ├── metrics.go              # counters + GET /metrics (Prometheus text)
│   ├── rest_bind.go            # WHATSAPP_BRIDGE_BIND / WHATSAPP_BRIDGE_ALLOWED_HOSTS
│   ├── media_retention.go      # WHATSAPP_MEDIA_AUTODOWNLOAD / _RETENTION_DAYS, store size
│   ├── auth.go                 # bearer token + loopback Host allow-list for /api/*
│   ├── chat_policy.go          # WHATSAPP_ALLOWED_CHATS enforcement on outbound endpoints
│   ├── read_only.go            # WHATSAPP_READ_ONLY: 403 on every mutating /api/* endpoint
│   ├── tool_policy.go          # WHATSAPP_ALLOW_TOOLS / _DENY_TOOLS: tool -> endpoint map, 403 on the rest
│   ├── fts.go                  # FTS5 index over messages.content
│   ├── media_inflight.go       # one transfer per destination file; the other callers share its result
│   ├── media_budget.go         # bounded pool for automatic media caching (4 at a time, 256 queued, drops the rest)
│   ├── media_retry.go          # re-download expired CDN media via the sender's phone
│   ├── media_purge.go          # POST /api/media/purge: drop cached files by (id, chat) or criteria, rows untouched
│   ├── instance_lock.go        # one bridge per store (flock / LockFileEx)
│   ├── polls.go                # native polls: creation, votes, /api/poll
│   ├── group_members.go        # /api/group/members
│   ├── group_members_store.go  # group_members table: rosters cached per group
│   ├── group_events.go         # GroupInfo join/leave/promote/demote + the paced roster refresh
│   ├── delete_message.go       # /api/delete (revoke / local delete)
│   ├── history_ondemand.go     # POST /api/history
│   ├── webhook.go              # outbound webhook for inbound messages
│   ├── Dockerfile              # alpine, pure-Go sqlite (modernc), CGO_ENABLED=0
│   └── store/                  # WHATSAPP_STORE_DIR: whatsapp.db, messages.db, media, .bridge-token, .bridge.lock (gitignored)
├── whatsapp-mcp-server/        # Python — MCP tools; reads messages.db, calls bridge REST
│   ├── main.py                 # MCPServer (SDK v2) tool definitions + transport startup
│   ├── strict_args.py          # StrictArgumentServer: call_tool refuses arguments no tool declares
│   ├── whatsapp.py             # SQL queries, bridge HTTP client, dict conversion
│   ├── media_inventory.py      # list_media / get_media_stats: sizes, sha256 copies, cache scan of store/<chat>/
│   ├── media_resource.py       # MediaResourceServer: the whatsapp://media/<chat>/<id> resource, and the resource_link on list_media rows
│   ├── media_notes.py          # notes.db (MCP-owned): agent notes keyed by sha256; annotate/get/search_media_notes; transcripts_fts
│   ├── notes.py                # notes.db: versioned notes on chats/contacts/messages (media via media_notes)
│   ├── triage.py               # mark_handled / snooze + the handled/snoozed/muted SQL filter list_unanswered applies
│   ├── mcp_config.py           # transport/host/port/allowed-hosts parsing
│   ├── http_auth.py            # WHATSAPP_MCP_TOKEN bearer middleware
│   ├── chat_policy.py          # WHATSAPP_ALLOWED_CHATS for reads and writes
│   ├── tool_policy.py          # WHATSAPP_READ_ONLY / _ALLOW_TOOLS / _DENY_TOOLS: hides + refuses tools
│   ├── untrusted.py            # @untrusted_content: the third-party-data sentence, name sanitisation, WHATSAPP_WRAP_UNTRUSTED
│   ├── endpoint_cert.py        # WHATSAPP_PUBLIC_URL: TLS expiry of the published endpoint, cached
│   ├── transcribe.py           # whisper.cpp backends for transcribe_audio
│   ├── audio.py                # ffmpeg helpers
│   └── Dockerfile              # python:3.13-slim + ffmpeg + uv, http transport
├── docker-compose.yml          # bridge + mcp (+ optional whisper profile) — docs/DOCKER.md
├── scripts/                    # backup.sh (hot backup/restore of the store volume), smoke.sh (post-deploy check), upstream-harvest.sh
├── docs/                       # user docs: DOCKER.md (ops), CONFIGURATION.md (every env var), TOOLS.md (tool reference),
│                               # LAPTOP.md (stdio setup), TROUBLESHOOTING.md, ARCHITECTURE.md (diagrams)
├── release-please-config.json  # release-please (simple, release-as for the first tag); .release-please-manifest.json holds the current version
└── .github/workflows/          # ci.yml, security.yml, publish.yml (main/sha images, steps aside on a release commit), release.yml (release PR, tags, every image tag of a release commit), release-cut.yml (daily merge of the release PR), build-push.yml (reusable)
```

Data flow: MCP client → MCP server → reads `messages.db` directly for everything read-only, calls bridge REST (`WHATSAPP_API_URL`, default `http://localhost:8080/api`) for sends, media, group info, polls, deletes → bridge → WhatsApp Web.

Three SQLite databases in the store directory: `whatsapp.db` (whatsmeow: session, contacts, LID map — opaque) and `messages.db` (ours: `chats`, `messages`, `calls`, `polls`, `poll_votes`, `group_members`, `messages_fts`) are written by the bridge and only read by the MCP server; `notes.db` (`media_notes`, keyed by content hash, plus `notes_meta` and the `transcripts_fts` index over the stored transcripts) is created lazily and owned by the MCP server, and the bridge never opens it — the "never create FTS from Python" rule is about `messages.db` only.

Compose topology: the `mcp` container joins the bridge's network namespace (`network_mode: service:bridge`), so the bridge keeps its loopback bind and loopback-only Host allow-list; the MCP port is published on the bridge service. An alternative topology is issue #58.

## 4. The routine: from issue to merged PR

This is how every change in this repo has been shipped; follow it unless the user says otherwise.

1. **Start from an issue.** Bugs and features have one. If none exists, open it (§11): one problem per issue, with a "Fix" sketch and acceptance boxes. The plan is the open backlog, read by priority: `gh issue list --repo Tauri-EPO/whatsapp-mcp --label P0`, then the same with `--label P1` and `--label P2`. The one open epic is #339 (manual verification of the September round on the home server — server work, not a coding checklist). The hardening and media epics #64, #138 and #99 all closed on 2026-09-04: read them for the history behind a design, never as a to-do list.
2. **Branch from current `main`:** `git fetch origin && git checkout -b <type>/<slug> origin/main` when the clone is yours alone, `git worktree add` when it is not ("Working in parallel" below). Types: `fix`, `feat`, `perf`, `refactor`, `docs`, `ci`, `chore`, `test`.
3. **One concern per PR, small.** Target under ~300 changed lines of code (docs and tests excluded). Split refactors into pure-move PRs. If a change needs another open PR, stack the branch on it, say "Stacked on #N" in the body, and retarget to `main` after that merges.
4. **Tests with the change.** Python: `tests/` (pytest, real SQLite files in `tmp_path`, `monkeypatch` for `requests`/policy/env). Go: table tests, `httptest`, fakes injected as functions (see `group_members.go`, `delete_message.go`, `polls.go`), `newTestMessageStore`. No test may need a paired phone.
5. **Docs in the same PR.** New env var → this file §7, `docs/CONFIGURATION.md`, `.env.example`, and `docker-compose.yml` passthrough if containers need it. `tests/test_env_docs.py` fails the Python job when the four disagree with what the code reads (compose-only knobs live in its allow-list). New tool → `docs/TOOLS.md` + the README "What your agent can do" table if it adds a capability + tool docstring (that docstring is what the model reads). `README.md` is the landing page for people arriving from search (Claude Code / Codex / Cursor / bots wanting WhatsApp): keep it short and outcome-oriented; technical detail goes in `docs/`.
6. **Run the gates locally** (§5) before pushing: ruff format + check, pyright, pytest, `go vet`/`go test`/`go test -race`, golangci-lint. For Docker-affecting changes, `docker compose up -d --build` and `scripts/smoke.sh` (CI runs it on the unpaired stack too).
7. **Commit message = the PR description.** Conventional-commit title; body says the problem, the fix, what was verified and `Closes #N`. Co-author trailer for agents.
8. **Open the PR with `gh pr create --repo Tauri-EPO/whatsapp-mcp --base main`.** Body: what/why, verification, security note if auth/paths/network/exec are touched.
9. **Wait for CI, then squash-merge:** `gh pr merge N --squash --delete-branch`. All checks must be green; a `startup_failure` or network flake is re-run with `gh run rerun <id> --failed`, never bypassed. Agents automate this with a wait-then-merge loop; never merge with red checks. One exception to the plain command: a Dependabot `pip-dev-*` PR is merged with `--subject "chore(deps): …"` so the dev-tooling bump does not cut a release (§2).
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
  - Run the Docker-based Go gates (§5) one session at a time: every one of them mounts the same host Go module cache, and `go vet`/`go test` also share the `wamcp-gobuild` build-cache volume.
  - After a crash, confirm the worktree still exists (`git worktree list`) before running anything in it.
- **No new top-level dependencies** without a sentence of justification in the PR.
- **Security-sensitive changes** (auth, file paths, network bind, command exec, allow-lists) must be called out in the PR body and get tests for the deny path.
- **Versions.** Automatic. `release.yml` runs release-please on every push to `main`: the next version comes from the Conventional Commit titles since the last tag (`feat:` minor, `fix:`/`perf:`/`refactor:`/`deps:` patch, `!` major; `docs:`, `ci:`, `test:` and `chore:` never trigger a release and stay out of the notes — release-please treats every visible changelog section as releasable, so a new type is releasable unless its section is `hidden`); it opens or updates a PR labelled `release` with the grouped notes and `CHANGELOG.md`. `release-cut.yml` merges that PR once a day (03:00 America/Sao_Paulo, or `gh workflow run release-cut.yml` to cut now) when CI on `main` is green, then dispatches `release.yml` — a merge made with `GITHUB_TOKEN` fires no `on: push` workflow, so the dispatch is what creates the tag. One dispatch is enough: a release commit is built exactly once, by `release.yml`, which tags that single build `vX.Y.Z`, `X.Y`, `latest` **and** `main` / `sha-<7>`; `publish.yml` recognises the `chore(main): release ` commit subject and steps aside, on the scheduled path and on the manual one alike (so keep the generated title when merging the release PR — `gh pr merge --squash` does). The consequence to know: at a release commit `:main` and `:latest` are the same digest and `/api/version` reports `vX.Y.Z+sha` on both; the next ordinary merge moves `:main` back to `main+sha`. Because two workflows can now push that mutable tag, `build-push.yml` drops `main` from its tag list when the commit it built is no longer the head of `main` (an overtaken release build must not pull `:main` backwards); the immutable tags are always pushed. The gaps are covered automatically: `release.yml` publishes the edge images itself when release-please cut no release, and `release-cut.yml` dispatches `publish.yml` when its own dispatch of `release.yml` failed after the merge. If the release build fails outright, `gh workflow run publish.yml --ref main` moves the edge tags by hand (a manual dispatch has no `head_commit`, so it never skips). Merging the release PR by hand earlier still works (`gh pr merge --squash` is fine; CI does not run on it because the bot's PR carries only the changelog and the manifest); the daily job then finds nothing. The merge creates tag `vX.Y.Z`, the GitHub Release and publishes the images as `vX.Y.Z`, `X.Y`, `latest`, `main` and `sha-<7>`. Never tag by hand, never edit `CHANGELOG.md` or `.release-please-manifest.json` by hand; the commit title decides the bump, so a `!` or `BREAKING CHANGE:` footer belongs on the squash commit. Repository setting required: Actions → General → "Allow GitHub Actions to create and approve pull requests" (set 2026-09-05; without it the bot cannot open the release PR).
- **Repository settings that live outside the code** (set 2026-09-07, Settings → Rules / Actions): ruleset "protect main" — no direct pushes (every change is a squash-merged PR, zero approvals required because the owner is the only collaborator), no force-push, no deletion, linear history; ruleset "protect release tags" — `v*` tags cannot be moved or deleted (release.yml still creates them); Actions restricted to GitHub-owned and verified creators plus the eight actions the workflows use (`actions/permissions/selected-actions`; a new third-party action must be added there or its job fails with "not allowed"). Fork PRs from first-time contributors wait for approval before workflows run. No collaborators, no deploy keys, no repository secrets (GHCR uses `GITHUB_TOKEN`); keep it that way, and check with `gh api repos/Tauri-EPO/whatsapp-mcp/rulesets` and `gh api repos/Tauri-EPO/whatsapp-mcp/collaborators`. The v1.0.0 notes cover everything since upstream's v0.6.0, so issue links in that first entry may point at upstream numbers; later entries are ours only. `pyproject.toml` keeps a nominal version. `/api/version` and the MCP `version` report `VERSION+sha` (`main+sha` for edge images, `vX.Y.Z+sha` for releases).

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
golangci-lint run                                # build tag is set in .golangci.yml

# Containers (both components, MCP over streamable HTTP) — see docs/DOCKER.md
docker compose up -d --build
docker compose logs -f bridge                    # QR code on first run
docker compose --profile whisper up -d           # + local whisper.cpp for transcribe_audio
```

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
  -w /src golangci/golangci-lint:v2.13.2 golangci-lint run
```

Working-copy files are CRLF (`core.autocrlf=true`); commits are LF. `*.sh` and `Dockerfile` are forced LF by `.gitattributes`. When editing files programmatically, read with universal newlines and write `\n`. Prefer writing whole files or line-anchored edits over shell heredocs containing backslash escapes.

## 6. CI gates

Every PR runs `.github/workflows/ci.yml` and `security.yml` (a newer push cancels the run in flight). All of these must be green before merging (the informational ones too: investigate, do not ignore):

| Job | What |
|---|---|
| Python Lint | `uv sync --frozen --extra dev`, `ruff check`, `ruff format --check`, `pyright` (basic mode, `tests/` excluded: they use duck-typed fakes), `pytest` (one job, one toolchain setup) |
| Go Build | `go build`, `go vet`, `go test` (again under `TZ=America/Sao_Paulo`), `go test -race` as its own step, then golangci-lint v2.13.2 (`errcheck`, `govet`, `ineffassign`, `unused`, `staticcheck`, `gosec`, `misspell`). Suppress a gosec finding only with `//nolint:gosec // <why>` on the line |
| CodeQL (Python, Go) | security scanning on PRs and weekly on `main`; `"host" in list` style asserts trip `py/incomplete-url-substring-sanitization`, use set comparisons in tests |
| Bandit, pip-audit, govulncheck, Trivy image scan | `continue-on-error`; read the output anyway. Trivy scans the freshly built images on PRs and the published `:main` tags weekly (HIGH/CRITICAL, fixed only), report in the job summary |
| Docker Build | both images build with buildx (GHA cache); smoke: bridge starts and reports the FTS state, every MCP module imports inside the image; the bridge also cross-builds for `linux/arm64` |

`publish.yml` pushes the `main` and `sha-<7>` images to GHCR on every merge to `main` except the release commit (§4); `release.yml` (release-please) maintains the release PR and, when it merges, builds that commit once and tags it `vX.Y.Z` / `X.Y` / `latest` / `main` / `sha-<7>` through the reusable `build-push.yml`; `release-cut.yml` merges that PR once a day. Dependabot auto-merge was removed; merge its PRs through the normal routine.

## 7. Environment variables

| Variable | Default | Purpose |
|----------|---------|---------|
| `WHATSAPP_STORE_DIR` | `store` (bridge, relative to cwd), `../whatsapp-bridge/store` (MCP) | Directory for `whatsapp.db`, `messages.db`, media, `.bridge-token`, `.bridge.lock` (`store_dir.go`: `storeDir()`, `storePath()`). Set for both processes; the compose file uses `/app/store` |
| `WHATSAPP_DB_PATH` | `$WHATSAPP_STORE_DIR/messages.db` | SQLite path used by the MCP server (overrides the store dir) |
| `WHATSMEOW_DB_PATH` | `$WHATSAPP_STORE_DIR/whatsapp.db` | whatsmeow SQLite (LID ↔ phone resolution via `whatsmeow_lid_map`); overrides the store dir |
| `WHATSAPP_API_URL` | `http://localhost:8080/api` | Bridge REST endpoint |
| `WHATSAPP_BRIDGE_TIMEOUT_S` | `30` | Timeout per MCP → bridge REST call (`whatsapp._bridge_request`); uploads/downloads use 120 s. Connection errors retry twice with backoff, read timeouts never (a POST is not re-sent) |
| `WHATSAPP_BRIDGE_BIND` | `127.0.0.1` | Bridge REST listen address; `0.0.0.0` / `::` for other containers or hosts (`rest_bind.go`) |
| `WHATSAPP_BRIDGE_ALLOWED_HOSTS` | *(loopback only)* | Extra `Host` values accepted by the bridge (`host` any port, `host:port` exact, `*` any). Same semantics as `WHATSAPP_MCP_ALLOWED_HOSTS`; loopback spellings always included; a non-loopback bind without it stays loopback-only (403) |
| `WHATSAPP_BRIDGE_PORT` | `8080` | Port the bridge listens on |
| `WHATSAPP_BRIDGE_TOKEN` | generated next to `WHATSMEOW_DB_PATH` as `.bridge-token` | Bearer token required for bridge REST calls; also signed onto outbound webhooks |
| `WHATSAPP_MEDIA_AUTODOWNLOAD` | `true` | Cache inbound media on arrival; `false` = fetch only on `/api/download` (`media_retention.go`) |
| `WHATSAPP_MEDIA_MAX_BYTES` | `268435456` (256 MiB) | Inbound files larger than this are not auto-downloaded (`/api/download` still fetches them); `0` = no limit. Downloads stream to `<file>.part` then rename (`media.go`) |
| `WHATSAPP_MEDIA_RETENTION_DAYS` | *(unset)* | Daily sweep deletes media files older than N days under `store/<chat>/`; DB rows untouched |
| `WHATSAPP_GROUP_ROSTER_SYNC_HOURS` | `6` | How stale a cached group roster may get before the bridge refreshes it in the background (`group_events.go`), one group per second, only while connected. `0` turns the pass off, leaving `group_members` fed only by `/api/group/members`, group events and group messages. It is a read (`GetGroupInfo`), so it keeps running under `WHATSAPP_READ_ONLY` |
| `WHATSAPP_MEDIA_ROOTS` | `~/.local/share/whatsapp-mcp/outbox` | Path-list of directories allowed for outbound media files |
| `WHATSAPP_EXPORT_DIR` | `$WHATSAPP_STORE_DIR/exports` | Directory `export_messages` writes NDJSON archives into (`export.py`). `out_path` is resolved under it and anything escaping it (`..`, an absolute path elsewhere, a symlink pointing out) is refused with `denied`. Compose leaves it at `/app/store/exports`, inside the `whatsapp-store` volume |
| `WHATSAPP_DEVICE_NAME` | `whatsmeow` (whatsmeow default) | Linked-device label shown in WhatsApp > Linked Devices. Applied at pair time only; re-pair to change |
| `WHATSAPP_ALLOWED_CHATS` | *(unset = all chats)* | Conversation allow-list (JIDs, bare numbers, `*@g.us` / `*@s.whatsapp.net`). MCP server filters reads and refuses writes (`chat_policy.py`); bridge returns 403 on send/react/mark-read/typing/delete/group/poll (`chat_policy.go`). Set for both processes |
| `WHATSAPP_READ_ONLY` | *(unset = everything enabled)* | Read-only deployment: the MCP server omits every mutating tool from `tools/list` and refuses it with `denied` if called anyway (`tool_policy.py`, `@mutating_tool`); the bridge answers 403 on the matching endpoints (`read_only.go`). Reads, `download_media`, `transcribe_audio` and `annotate_media` (local notes.db) stay available. `1/true/yes/on` or `0/false/no/off`; anything else stops the process. Set for both processes |
| `WHATSAPP_ALLOW_TOOLS` | *(unset = every tool)* | Allow-list of tool names (comma-separated): only these are offered, reads included (`tool_policy.py`). The bridge maps the names to the endpoints they call (`endpointTools` in `tool_policy.go`) and answers 403 on the rest; reads stay open there, as in read-only mode. Unknown names stop the process with the valid list. Set for both processes |
| `WHATSAPP_DENY_TOOLS` | *(unset)* | Deny-list of tool names, enforced in both processes. Wins over `WHATSAPP_ALLOW_TOOLS`; `WHATSAPP_READ_ONLY` wins over both (the three filters only ever remove capability). Unknown names stop the process. The bridge check is per endpoint, so an endpoint shared by several tools (`/api/send`) stays open while any of them is allowed |
| `WHATSAPP_WRAP_UNTRUSTED` | *(unset = off)* | MCP-server-only: wrap third-party text in the results (`content`, `last_message`, transcripts, note values, group `topic`, poll `question`) in `<untrusted>…</untrusted>` delimiters, so a model that skipped the tool description still sees a boundary (`untrusted.py`). JIDs, IDs, timestamps and cursors are untouched. Name fields are never wrapped in either mode: they are sanitised instead, always (control, zero-width and bidi characters stripped, capped at 200 characters, issue #273); `topic` and `question` are sanitised too, keeping their line breaks and capped at 4096 characters (issue #332). Same strict boolean parse as `WHATSAPP_READ_ONLY`. A hint, not a control — the enforced mitigations are `WHATSAPP_READ_ONLY` and `WHATSAPP_ALLOWED_CHATS` |
| `WHATSAPP_LOG_LEVEL` | `INFO` | Bridge log level (`DEBUG`/`INFO`/`WARN`/`ERROR`), applied to the bridge logger and the whatsmeow client. `DEBUG` echoes each stored message |
| `WHATSAPP_LOG_FORMAT` | `text` | `json` switches the bridge (and whatsmeow) log lines to one JSON object per line (`ts`, `level`, `module`, `msg`) (`logging_json.go`) |
| `WHATSAPP_METRICS` | `true` | Serve `GET /metrics` on the bridge (Prometheus text, unauthenticated like `/api/version`: counters and connection state only, `metrics.go`); `false` removes the route |
| `WHATSAPP_MCP_LOG_LEVEL` | `INFO` | MCP server log level (stderr) |
| `WHATSAPP_MCP_LOG_FORMAT` | `text` | `json` switches the MCP server stderr log to one JSON object per line (`observability.py`) |
| `WHATSAPP_MCP_METRICS` | `true` | Serve `GET /metrics` on the `http`/`sse` transports (tool calls/errors/seconds per tool, the `whatsapp_mcp_tool_duration_seconds` histogram, HTTP requests by status class); `false` disables it |
| `WHATSAPP_MCP_METRICS_TOKEN` | *(unset = open)* | Bearer token required on the MCP `/metrics` only (401 otherwise); set it when the MCP port is reachable beyond the tailnet (Funnel). Independent of `WHATSAPP_MCP_TOKEN` |
| `WHATSAPP_MCP_TRANSPORT` | `stdio` | MCP transport: `stdio`, `http`, or `sse` |
| `WHATSAPP_MCP_HOST` | `127.0.0.1` | Bind address for the `http`/`sse` transports |
| `WHATSAPP_MCP_PORT` | `8000` | Port for the `http`/`sse` transports |
| `WHATSAPP_MCP_ALLOWED_HOSTS` | loopback only | Extra `Host` header values accepted by the `http`/`sse` transports (comma-separated; bare hostnames match any port; `*` disables the check). Unset + non-loopback bind disables the check with a warning |
| `WHATSAPP_MCP_ALLOWED_ORIGINS` | derived from allowed hosts | Extra `Origin` header values for browser-based MCP clients |
| `WHATSAPP_MCP_RATE_LIMIT` | `120` with a token, `0` without | Requests/minute per client (X-Forwarded-For first hop or peer) on `http`/`sse`; token bucket in `http_auth.RateLimitMiddleware`, 429 + Retry-After; `0`/`off` disables |
| `WHATSAPP_MCP_MAX_BODY_BYTES` | `4194304` | Max request body for `http`/`sse` (passed to the SDK app) |
| `WHATSAPP_MCP_TOKEN` | bridge token when bound off-loopback; none on loopback | Static bearer token enforced on the `http`/`sse` transports (`http_auth.resolve_http_token`, min 16 chars). Unset + non-loopback bind → reuses the bridge token (env or `.bridge-token`); `off` disables auth explicitly. stdio unaffected |
| `WHATSAPP_PUBLIC_URL` | *(unset)* | MCP-server-only: the URL clients use to reach this server (`https://host.tailnet.ts.net/mcp`; a bare `host` / `host:port` also works). Set it and `bridge_status` reports `endpoint_cert_expires_at` / `endpoint_cert_days_left` for that endpoint's certificate, and `endpoint_cert_error` when the handshake fails (`endpoint_cert.py`: one outbound TLS handshake, no HTTP request, 3 s, chain verified with the default context, result cached an hour). Unset = no fields, no probe |
| `WEBHOOK_URL` | `http://localhost:8769/whatsapp/webhook` | Outgoing webhook for incoming messages (empty falls back to this default) |
| `WEBHOOK_ENABLED` | `true` (compose: `false`) | Set to `false` to disable outbound webhooks entirely |
| `FORWARD_SELF` | `true` (compose: `false`) | Whether self-sent messages are forwarded to the webhook |
| `WHATSAPP_PARENT_WATCHDOG_S` | `30` | Stdio parent-liveness poll interval (seconds) |
| `WHISPER_URL` | *(unset)* | whisper.cpp `whisper-server` inference endpoint for `transcribe_audio` (`transcribe.py`). Wins over `WHISPER_BIN` |
| `WHISPER_BIN` / `WHISPER_MODEL` | *(unset)* | Local `whisper-cli` binary + `ggml-*.bin` model, alternative backend |
| `WHISPER_LANGUAGE` | `pt` | Default transcription language; `auto` to detect |
| `WHISPER_TIMEOUT_S` | `300` | Per-transcription timeout (seconds) |
| `TRANSCRIBE_ON_INGEST` | *(unset = off)* | MCP-server-only: background thread that transcribes inbound voice notes as they arrive (`transcribe_worker.py`) instead of waiting for an agent to call `transcribe_audio`. Reads `messages.db`, writes the same `transcript` / `transcript_lang` / `transcript_backend` notes into `notes.db`, idempotent by sha256, concurrency one. Costs CPU on this machine; with no whisper backend configured it stays off with a warning, and so it does when the tool policy does not offer `transcribe_audio` (a policy that hides `download_media` only turns the fetch path off). Same strict boolean parse as `WHATSAPP_READ_ONLY` |
| `TRANSCRIBE_ON_INGEST_INTERVAL_S` | `300` | Seconds between batches (values below 5 are raised to 5) |
| `TRANSCRIBE_ON_INGEST_BATCH` | `10` | Voice notes transcribed per batch (capped at 200). A file the backend cannot read gets a `transcript_error` note and is not tried again until that note is cleared; a backend that cannot be reached at all (`BackendUnavailableError`: refused, 502/503/504, a misrouted `WHISPER_URL`, no binary or model) writes no note, three of those in a row end the round with a warning, and the notes an older build wrote for such an outage are cleared when the worker starts |
| `TRANSCRIBE_ON_INGEST_FETCH` | *(unset = off)* | Let the ingest worker ask the bridge (`/api/download`, the path `transcribe_audio` uses) for audio whose bytes are not cached, instead of skipping it — what an archive with `WHATSAPP_MEDIA_AUTODOWNLOAD=false` or a retention sweep needs. `TRANSCRIBE_ON_INGEST_BATCH` bounds the attempts, failures included, and the downloaded bytes stay in the store like any other download. A file the bridge cannot send this time is skipped without a note (the next round retries it); three failures in a row end the fetching for that round. A file the bridge answers `media_unavailable` for (the sender's phone was asked to re-upload and said the media is gone, or the row carries no CDN fields to download with) gets a dated `media_unavailable` note, leaves the work list for good and costs no strike (`coverage().audio.unavailable`). Same strict boolean parse as `WHATSAPP_READ_ONLY` |
| `FFMPEG_TIMEOUT_S` | `120` | Timeout for each ffmpeg conversion (voice-note encode in `audio.py`, 16 kHz WAV prep in `transcribe.py`) |

Compose-only knobs (`WHATSAPP_MCP_BIND`, `WHATSAPP_OUTBOX`, `WHISPER_MODEL_NAME`, `WHISPER_THREADS`, `COMPOSE_PROFILES`) are documented in `.env.example` and `docs/DOCKER.md`.

When adding a new env var: document it here, in `docs/CONFIGURATION.md`, in `.env.example`, and pass it through in `docker-compose.yml` when a container needs it. The README only lists the day-one essentials.

## 8. Gotchas (read before editing)

1. **JIDs.** WhatsApp identifies users as `1234567890@s.whatsapp.net` (DM), `123456@g.us` (group), and `<random>@lid` (link-ID, anonymous). The bridge maintains a phone↔LID map in `whatsapp.db.whatsmeow_lid_map`. Many "user is missing" / "messages don't show" bugs trace back to JID-form mismatches. Always think about both forms (`resolveUserJID`, `resolveQuotedParticipantJID`, `resolveMentionJIDs`). `messages.sender` holds the bare user part and `messages.sender_server` the namespace it belongs to (`s.whatsapp.net` / `lid`, NULL when unknown — rows written before the column existed, and senders that are not user JIDs): a bare number does not say which one it is, and a 15-digit LID reads exactly like a phone number. Write paths pass the full resolved JID to `StoreMessage`, which splits it (`sender_namespace.go`); readers keep using the bare `sender`.
2. **Message IDs are unique per chat, not globally.** The `messages` primary key is `(id, chat_jid)`. Always pass `chat_jid` alongside an ID; forwards reuse IDs across chats.
3. **Pointer rows.** `reaction` and `poll_vote` messages refer to another message via `messages.target_message_id` (the bridge also writes it to `filename` for one release; readers use `_target_id()` which falls back to `filename` for pre-migration rows). Do not add new meanings to `filename`.
4. **Media files** live under `store/{chat_jid}/` with timestamp + message-ID filenames. Use `/api/download`, never hand-built paths. CDN URLs expire (403/404/410 after days); `downloadMedia` runs one media-retry round trip against the sender's phone (`media_retry.go`) before failing. Only one transfer per destination file ever runs (`media_inflight.go`): callers that miss the cache together share its result instead of writing the same `<file>.part`, and the transfer follows the bridge lifecycle rather than the caller that started it. Automatic caching of inbound media is background work with a budget (`media_budget.go`): four at a time, 256 queued, the overflow dropped with a WARN and a counter — the row stays, so `download_media` still fetches that file.
5. **Audio.** Voice notes must be Opus `.ogg`; `send_audio_message` converts via ffmpeg. `transcribe_audio` converts to 16 kHz WAV before whisper.
6. **History sync** is controlled by the phone. Modern syncs put the group sender in top-level `WebMessageInfo.participant`; read it before `Key.participant`. Poll votes in history cannot be decrypted (issue #59).
7. **`messages.db` is the source of truth for reads.** The MCP server must never need the bridge for read-only tools. The bridge opens the DB in WAL mode with a busy timeout; the MCP side uses a 5 s timeout via `_connect_messages_db()`.
8. **Search index.** The bridge owns `messages_fts` (FTS5, `fts.go`) and its triggers; the driver (modernc.org/sqlite) always ships FTS5, and the startup check still *drops* the index on a build without it so writes never fail. The MCP server uses `MATCH` only when the table exists and falls back to `instr()`. Never create FTS triggers from Python.
9. **One bridge per store.** `main()` takes an exclusive OS lock on `store/.bridge.lock` (`instance_lock.go`); a second bridge exits naming the holder's PID. Tests that need concurrent bridge processes must use separate working directories.
10. **Configuration is read once.** `os.Getenv` belongs in `main.go` and the `resolve*` / `load*` / `new*` helpers it calls at startup; handlers and event paths read `Bridge` fields (`MediaRoots`, `MediaRetention`, `Webhook.enabled`, …). `storeDir()` is the one per-call read left, because tests point it at temp dirs.
11. **No package-level state in the bridge.** Runtime dependencies live on the `Bridge` struct (`bridge.go`); tests build one with `testBridge(...)` and override fields. The one sanctioned global is `bridgeLog` (`logging.go`), write-once configuration set by `initLogging()`; tests swap it with `installRecordingLogger(t)`. A knob a test reassigns and restores (`streamReplacedDelay` was one, issue #351) is also a data race the moment a goroutine reads it: put it on the `Bridge` and set it on the test's own instance. Same for what a test leaves running — a bridge that queued an auto-download outlives the test unless it is drained (`drainBridge(t, b)`), and `go test -race` is what catches both.
12. **stdout is the protocol on stdio.** Anything the MCP server prints to stdout can corrupt a stdio session; log through `logging` (stderr), never `print()`.
13. **Bridge logs go through `bridgeLog`, not `fmt.Print*`.** Levels: `Errorf` for failures that lose data, `Warnf` for degraded-but-continuing, `Infof` for lifecycle, `Debugf` for per-request traces and message echoes (user content stays out of `INFO`). The only `fmt.Print*` left are the first-run token banner and the pairing QR code, which are meant for a human.
14. **REST starts before pairing.** `/api/health` is liveness (200 once the listener is up, body carries `connected`/`paired`); `/api/ready` is readiness (200 only while connected). Endpoints that need WhatsApp check `client.IsConnected()` themselves.
15. **Outgoing calls are not visible to linked devices.** Don't promise features that depend on them.
16. **One timestamp spelling.** Every TIMESTAMP column the bridge writes holds `YYYY-MM-DD HH:MM:SS+00:00` (UTC, seconds, fixed width) — `dbTime()` in `store_time.go`, never a bound `time.Time`, or the driver stamps the machine's local offset and `ORDER BY timestamp` starts sorting by wall clock. Read with `parseDBTime` / `anchorTime`, which also accept the legacy spellings. A new time column goes in `canonicalTimeColumns` (and bumps `messagesDBUserVersion` so the rewrite runs again). Bounds compared against such a column must be rendered the same way on both sides.

## 9. Where to make changes

| You want to… | Touch |
|---|---|
| Add or modify an MCP tool | `whatsapp-mcp-server/main.py` (+ `docs/TOOLS.md`, tests) |
| Change what `resources/read` serves, or add a resource scheme | `whatsapp-mcp-server/media_resource.py` (the `mcp` instance is a `MediaResourceServer`) |
| Change agent notes (chats, contacts, messages, media) | `whatsapp-mcp-server/notes.py`, `media_notes.py` |
| Change what `list_unanswered` hides (handled, snoozed, muted, closing messages) | `whatsapp-mcp-server/triage.py` + `_closing_message_clause` in `whatsapp.py` |
| Change DB queries / dict conversion | `whatsapp-mcp-server/whatsapp.py` |
| Change HTTP transport, auth, allowed hosts | `whatsapp-mcp-server/main.py` (`__main__`), `mcp_config.py`, `http_auth.py` |
| Change the conversation allow-list | `chat_policy.py` **and** `whatsapp-bridge/chat_policy.go` |
| Add or rename an MCP tool that calls the bridge | also `endpointTools` / `unenforcedTools` in `whatsapp-bridge/tool_policy.go` (`tests/test_bridge_tool_policy.py` fails otherwise) |
| Change how tool results are marked as untrusted | `whatsapp-mcp-server/untrusted.py` (+ the allow-list in `tests/test_untrusted_content.py`) |
| Change voice-note transcription | `whatsapp-mcp-server/transcribe.py`, `whisper` profile in `docker-compose.yml` |
| Add a bridge REST endpoint | new `whatsapp-bridge/<feature>.go` with `handleX(deps…) http.HandlerFunc`, register in `newRESTMux` (`rest.go`) wrapped in `auth(requireMethod(...))`, fail with `writeError` (never `http.Error`), tests with fakes |
| Change inbound event handling | `handleEvent` / `handleMessage` in `events.go`, `handleHistorySync` in `history_sync.go`; content extraction in `content.go` |
| Change the messages schema | `ensureMessageStoreSchema` in `store.go`; migrations idempotent (`ensureColumn`, and a backfill that marks the rows it read like `mentions.go`); FTS in `fts.go` |
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
- Larger efforts get an `epic` issue holding the checklist; §4 step 1 names the open one and the closed ones.
- Bugs from operation: include bridge log lines, `docker compose ps`, the tool call and its result; redact phone numbers.
- "Won't do" is a valid outcome; close with a sentence explaining why.

See [`CONTRIBUTING.md`](./CONTRIBUTING.md) for the human-facing contribution guide, [`docs/DOCKER.md`](./docs/DOCKER.md) for operations and [`docs/CONFIGURATION.md`](./docs/CONFIGURATION.md) for the user-facing variable reference.
