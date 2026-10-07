#!/usr/bin/env bash
# Merge a pull request the way the "protect main" ruleset allows (issue #545):
# the CI jobs green on a head that is up to date with main.
#
#   scripts/merge-when-green.sh <PR> [flags for `gh pr merge`]
#   scripts/merge-when-green.sh 123 --subject "chore(deps): bump the python dev tooling (#123)"
#
# It loops: bring the branch up to date when main moved, wait for the checks,
# squash-merge. When several pull requests are ready at once, every merge to
# main sends the others round again and one of them wins each round, so the
# loop is bounded rather than endless.
#
# Exit status: 0 merged, 1 a check failed or the PR is closed, 2 the branch
# conflicts with main (rebase it by hand), 3 gave up after MERGE_MAX_ROUNDS,
# 64 usage.
#
# Requires `gh`. GH_REPO selects the repository (default: this one).
set -euo pipefail

REPO="${GH_REPO:-Tauri-EPO/whatsapp-mcp}"
MAX_ROUNDS="${MERGE_MAX_ROUNDS:-12}"
POLL="${MERGE_POLL_SECONDS:-20}"
# GitHub answers UNKNOWN while it computes mergeability, and "no checks" right
# after a push: neither is a round, but neither may spin for ever.
MAX_WAITS="${MERGE_MAX_WAITS:-45}"

if [[ $# -lt 1 || ! "$1" =~ ^[0-9]+$ ]]; then
  echo "usage: scripts/merge-when-green.sh <PR number> [flags for gh pr merge]" >&2
  exit 64
fi
pr="$1"
shift

field() { gh pr view "$pr" --repo "$REPO" --json "$1" --jq ".$1"; }
failed_checks() {
  gh pr checks "$pr" --repo "$REPO" --json name,bucket \
    --jq '[.[] | select(.bucket == "fail" or .bucket == "cancel") | .name] | join(", ")' 2>/dev/null || true
}

round=0
waits=0
while ((round < MAX_ROUNDS)); do
  state="$(field state)"
  if [[ "$state" == "MERGED" ]]; then
    echo "PR #$pr is merged."
    exit 0
  fi
  if [[ "$state" != "OPEN" ]]; then
    echo "PR #$pr is $state, not open: nothing to merge." >&2
    exit 1
  fi

  status="$(field mergeStateStatus)"
  case "$status" in
    DIRTY)
      echo "PR #$pr conflicts with main: rebase it by hand, then run this again." >&2
      exit 2
      ;;
    BEHIND)
      round=$((round + 1))
      echo "round $round: main moved, updating the branch of PR #$pr"
      gh pr update-branch "$pr" --repo "$REPO"
      sleep "$POLL"
      continue
      ;;
    UNKNOWN)
      waits=$((waits + 1))
      if ((waits > MAX_WAITS)); then
        echo "GitHub never settled the merge state of PR #$pr." >&2
        exit 3
      fi
      sleep "$POLL"
      continue
      ;;
  esac

  # BLOCKED (checks still running), UNSTABLE, CLEAN, HAS_HOOKS: wait for the
  # checks of this head, then try.
  if ! gh pr checks "$pr" --repo "$REPO" --watch --fail-fast --interval "$POLL" >/dev/null 2>&1; then
    failed="$(failed_checks)"
    if [[ -n "$failed" ]]; then
      echo "PR #$pr has failing checks: $failed" >&2
      echo "A flake is re-run with: gh run rerun <run id> --failed. A red check is never merged over." >&2
      exit 1
    fi
    # No check reported yet for a head that was just pushed.
    waits=$((waits + 1))
    if ((waits > MAX_WAITS)); then
      echo "No check ever reported for PR #$pr." >&2
      exit 3
    fi
    sleep "$POLL"
    continue
  fi

  round=$((round + 1))
  if gh pr merge "$pr" --repo "$REPO" --squash --delete-branch "$@"; then
    echo "PR #$pr merged."
    exit 0
  fi
  echo "round $round: the merge of PR #$pr was refused (main moved meanwhile?); going round again"
  sleep "$POLL"
done

echo "Gave up on PR #$pr after $MAX_ROUNDS rounds: main keeps moving. Run this again." >&2
exit 3
