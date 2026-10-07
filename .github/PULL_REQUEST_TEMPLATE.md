<!--
Thanks for the PR! A couple of quick checks before you submit:

- Read AGENTS.md §4 (the routine from issue to merged PR).
- One concern per PR. Split anything bigger.
- Conventional-commit title (feat/fix/chore/docs/ci/refactor/test/perf).
-->

## Summary

<!-- What does this PR do, and why? 1-3 bullets is plenty. -->

-

## Type of change

- [ ] `fix` — bug fix
- [ ] `feat` — new feature
- [ ] `chore` / `docs` / `ci` / `refactor` / `test` / `perf`
- [ ] Breaking change (`!` in commit, or `BREAKING CHANGE:` in body)

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
- [ ] Manually exercised the affected code path

## Docs

- [ ] Updated `README.md` (if user-visible)
- [ ] Updated `AGENTS.md` / `CLAUDE.md` (if contributor-visible)
- [ ] Updated tool descriptions in `whatsapp-mcp-server/main.py` (if MCP tools changed)
- [ ] Updated `docs/TOOLS.md` (if MCP tools changed)
- [ ] Updated AGENTS.md §7, `docs/CONFIGURATION.md`, `.env.example` and the compose passthrough (if env vars changed)

## Security

<!-- Required when auth, file paths, network bind, command exec or allow-lists are touched: what changed and which test covers the deny path. Otherwise "n/a". -->

## Risk / rollback

<!-- Anything reviewers should worry about? How do we revert if this misbehaves? -->
