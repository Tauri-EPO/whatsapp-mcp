<!--
Thanks for the PR! A couple of quick checks before you submit:

- Read AGENTS.md §4 (the routine from issue to merged PR).
- One concern per PR. Split anything bigger.
- Conventional-commit title: it becomes the squash commit and decides the release (feat / fix / perf / refactor / deps / docs / ci / test / chore).
-->

## Summary

<!-- What does this PR do, and why? 1-3 bullets is plenty. -->

-

## Type of change

- [ ] `feat` — new feature (minor release)
- [ ] `fix` / `perf` / `refactor` — bug fix, speed-up or code change (patch release)
- [ ] `deps` — dependency or base-image bump that changes the shipped images (patch release)
- [ ] `docs` / `ci` / `test` / `chore` — no release
- [ ] Breaking change (`!` in the title, or `BREAKING CHANGE:` in the body)

## Scope check

- [ ] There is an issue for this change (`Closes #N` in the description)
- [ ] PR is focused on one concern (split if not)
- [ ] PR is ≤ ~300 LOC, **or** justified in the description

## Linked issues

<!-- "Closes #N" / "Refs #N" -->

## Testing

<!-- How did you verify this? Manual steps, new tests, screenshots/logs as needed. -->

- [ ] Added or updated tests
- [ ] Ran `ruff format`, `ruff check`, `pyright` and `pytest` (Python changes)
- [ ] Ran `go vet`, `go test`, `go test -race` and `golangci-lint run` (Go changes)
- [ ] Ran `docker compose up -d --build` and `scripts/smoke.sh` (Dockerfile or compose changes)
- [ ] Manually exercised the affected code path

## Docs

- [ ] Updated `README.md` (if user-visible)
- [ ] Updated `AGENTS.md` (if contributor-visible: §3 tree for a new module, §7 for an env var, §9 for a new place to change)
- [ ] Updated the tool docstring in `whatsapp-mcp-server/main.py` and `docs/TOOLS.md` (if MCP tools changed)
- [ ] Updated `docs/CONFIGURATION.md`, `.env.example` and the compose passthrough (if env vars changed)

## Security

<!-- Required when auth, file paths, network bind, command exec or allow-lists are touched: what changed and which test covers the deny path. Otherwise "n/a". -->

## Risk / rollback

<!-- Anything reviewers should worry about? How do we revert if this misbehaves? -->
