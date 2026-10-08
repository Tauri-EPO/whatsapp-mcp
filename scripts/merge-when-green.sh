#!/usr/bin/env bash
# Merge a pull request the way the "protect main" ruleset allows (issue #545):
# the CI jobs green on a head that is up to date with main.
#
#   scripts/merge-when-green.sh <PR> [flags for `gh pr merge`]
#   scripts/merge-when-green.sh 123 --subject "chore(deps): bump the python dev tooling (#123)"
#
# It polls: when main moved it brings the branch up to date, it waits while
# checks run, and it squash-merges once every check of the current head has
# passed. When several pull requests are ready at once, every merge to main
# sends the others round again and one of them wins each round.
#
# The squash commit is "<PR title> (#N)" with the PR description as its body
# (AGENTS.md section 4 step 7), unless --subject / --body are passed: after an
# update the branch holds a merge commit, and GitHub's default would then build
# the message from the commit list.
#
# Exit status:
#   0  merged
#   1  a check failed or was cancelled, the PR is closed or a draft, or the
#      merge was refused for a reason that waiting cannot fix
#   2  the branch conflicts with main (rebase it by hand)
#   3  gave up: main kept moving for MERGE_MAX_ROUNDS rounds, GitHub never
#      settled, or MERGE_TIMEOUT_SECONDS passed
#   4  GitHub could not be reached (after retries)
#   64 usage
#
# Requires `gh`. GH_REPO selects the repository (default: this one).
set -uo pipefail

REPO="${GH_REPO:-Tauri-EPO/whatsapp-mcp}"
MAX_ROUNDS="${MERGE_MAX_ROUNDS:-12}"        # branch updates plus merge attempts
POLL="${MERGE_POLL_SECONDS:-20}"
MAX_WAITS="${MERGE_MAX_WAITS:-45}"          # consecutive polls with nothing to look at
TIMEOUT="${MERGE_TIMEOUT_SECONDS:-5400}"    # the whole run
API_RETRIES="${MERGE_API_RETRIES:-3}"

if [[ $# -lt 1 || ! "$1" =~ ^[0-9]+$ ]]; then
  echo "usage: scripts/merge-when-green.sh <PR number> [flags for gh pr merge]" >&2
  exit 64
fi
pr="$1"
shift
extra=("$@")

# One answer from gh, retried: a 502 or a rate-limit blip in an hour-long run
# must not look like a failed check.
ask() {
  local attempt=0 out
  while :; do
    if out="$("$@" 2>&1)"; then
      printf '%s' "$out"
      return 0
    fi
    attempt=$((attempt + 1))
    if ((attempt >= API_RETRIES)); then
      echo "GitHub did not answer '$*': $out" >&2
      return 1
    fi
    sleep "$POLL"
  done
}
field() { ask gh pr view "$pr" --repo "$REPO" --json "$1" --jq ".$1"; }

# "pass", "pending", "none" (nothing reported for this head yet) or
# "fail:<names>". A cancelled check counts as failed: `gh pr checks --watch`
# would report success over it. gh exits non-zero while checks are pending or
# failed and still prints them, so its status is not what is read here.
verdict() {
  local lines failed
  lines="$(gh pr checks "$pr" --repo "$REPO" --json name,bucket --jq '.[] | "\(.bucket)\t\(.name)"' 2>/dev/null || true)"
  if [[ -z "$lines" ]]; then
    echo none
    return
  fi
  failed="$(printf '%s\n' "$lines" | awk -F'\t' '$1 == "fail" || $1 == "cancel" { printf "%s%s", sep, $2; sep = ", " }')"
  if [[ -n "$failed" ]]; then
    echo "fail:$failed"
  elif printf '%s\n' "$lines" | grep -q '^pending'; then
    echo pending
  else
    echo pass
  fi
}

passed() {
  local flag
  for flag in "${extra[@]+"${extra[@]}"}"; do
    case "$flag" in "$1" | "$1"=* | "$2") return 0 ;; esac
  done
  return 1
}

merge() {
  local args=(--squash --delete-branch) title body
  if ! passed --subject -t; then
    title="$(field title)" || return 4
    args+=(--subject "$title (#$pr)")
  fi
  if ! passed --body -b && ! passed --body-file -F; then
    body="$(field body)" || return 4
    args+=(--body "$body")
  fi
  gh pr merge "$pr" --repo "$REPO" "${args[@]}" "${extra[@]+"${extra[@]}"}"
}

deadline=$((SECONDS + TIMEOUT))
round=0
waits=0
idle() { # $1: what was being waited for
  waits=$((waits + 1))
  if ((waits > MAX_WAITS)); then
    echo "Gave up on PR #$pr: $1." >&2
    exit 3
  fi
  sleep "$POLL"
}
spend_round() {
  round=$((round + 1))
  if ((round > MAX_ROUNDS)); then
    echo "Gave up on PR #$pr after $MAX_ROUNDS rounds: main keeps moving. Run this again." >&2
    exit 3
  fi
}

while :; do
  if ((SECONDS >= deadline)); then
    echo "Gave up on PR #$pr after ${TIMEOUT}s: its checks never finished." >&2
    exit 3
  fi

  state="$(field state)" || exit 4
  if [[ "$state" == "MERGED" ]]; then
    echo "PR #$pr is merged."
    exit 0
  fi
  if [[ "$state" != "OPEN" ]]; then
    echo "PR #$pr is $state, not open: nothing to merge." >&2
    exit 1
  fi

  status="$(field mergeStateStatus)" || exit 4
  case "$status" in
    DIRTY)
      echo "PR #$pr conflicts with main: rebase it by hand, then run this again." >&2
      exit 2
      ;;
    DRAFT)
      echo "PR #$pr is a draft: mark it ready first." >&2
      exit 1
      ;;
    BEHIND)
      spend_round
      echo "round $round: main moved, updating the branch of PR #$pr"
      ask gh pr update-branch "$pr" --repo "$REPO" >/dev/null || exit 4
      waits=0
      sleep "$POLL"
      continue
      ;;
    UNKNOWN)
      idle "GitHub never settled its merge state"
      continue
      ;;
  esac

  checks="$(verdict)"
  case "$checks" in
    fail:*)
      echo "PR #$pr has failed or cancelled checks: ${checks#fail:}" >&2
      echo "A flake is re-run with: gh run rerun <run id> --failed. A red check is never merged over." >&2
      exit 1
      ;;
    none)
      idle "no check ever reported for its head"
      continue
      ;;
    pending)
      waits=0
      sleep "$POLL"
      continue
      ;;
  esac

  waits=0
  spend_round
  output="$(merge 2>&1)"
  code=$?
  if ((code == 0)); then
    echo "PR #$pr merged."
    exit 0
  fi
  if ((code == 4)); then
    echo "$output" >&2
    exit 4
  fi
  # Refused. Only a main that moved (or a state GitHub is still computing) is
  # worth another round; anything else would be refused the same way again.
  after="$(field mergeStateStatus)" || exit 4
  case "$after" in
    BEHIND | UNKNOWN | BLOCKED)
      echo "round $round: the merge of PR #$pr was refused ($after): $output"
      sleep "$POLL"
      ;;
    *)
      echo "The merge of PR #$pr was refused and waiting will not change it: $output" >&2
      exit 1
      ;;
  esac
done
