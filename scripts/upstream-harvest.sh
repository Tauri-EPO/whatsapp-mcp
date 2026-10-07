#!/usr/bin/env bash
# Upstream harvest: list what the original projects did since our last pass.
#
# This fork does not merge upstream (see AGENTS.md §2). It harvests ideas:
# read the commits, PRs and issues below, reimplement what is worth having,
# credit the source in the commit body, then record what was listed with --mark.
#
#   scripts/upstream-harvest.sh            # commits, PRs and issues since the last mark
#   scripts/upstream-harvest.sh --all      # ignore the marks: every commit vs our main, every open PR and issue
#   scripts/upstream-harvest.sh --mark     # record what the last listing showed as harvested
#
# --mark records what the last listing in this clone read (the heads it printed
# and the time it started), not a fresh fetch: whatever upstream pushed while
# you were reviewing is still new at the next harvest. The timestamp of a mark
# is what the PR/issue listing filters on, so it only moves when that listing
# was complete; a missing gh, a failed call, the safety limit or --all leave it
# where it was.
#
# HARVEST_LIMIT (default 1000) bounds the PRs + issues listed per repository.
# Hitting it is said in the output, never silent.
#
# Requires: git remotes `upstream` (verygoodplugins) and `lharries` (added when
# missing), a local `main` when there is no mark to start from (--all), and
# `gh` for the PR/issue listing.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
MARK_FILE=".upstream-harvest"
# What the last listing read, one line per remote: name, head, the timestamp
# --mark will record, the mark SHA the listing started from ("-" = none). Kept
# in the git directory: per clone, never committed.
STATE_FILE="$(git rev-parse --git-path upstream-harvest.listed)"
LIMIT="${HARVEST_LIMIT:-1000}"
NAMES=(upstream lharries)
COMPONENTS=(whatsapp-bridge whatsapp-mcp-server)
declare -A REMOTES=(
  [upstream]="https://github.com/verygoodplugins/whatsapp-mcp.git"
  [lharries]="https://github.com/lharries/whatsapp-mcp.git"
)
declare -A REPOS=(
  [upstream]="verygoodplugins/whatsapp-mcp"
  [lharries]="lharries/whatsapp-mcp"
)
# One line per PR or issue, newest update first; the first word says which.
ACTIVITY_JQ='.[] | "\(if .pull_request then "PR" else "IS" end) #\(.number) \(.updated_at[:10]) \(if .pull_request.merged_at then "merged" else .state end) \(.title)"'

die() {
  echo "upstream-harvest: $*" >&2
  exit 1
}

# mark_field NAME N: field N of NAME's line in the mark file (2 = SHA,
# 3 = timestamp), empty when there is none. The file is CRLF in a Windows
# working copy, hence the sub().
mark_field() {
  [[ -f "$MARK_FILE" ]] || return 0
  awk -v name="$1" -v field="$2" '{ sub(/\r$/, "") } $1 == name { print $field; exit }' "$MARK_FILE"
}

# count TEXT: its number of lines, 0 when empty.
count() {
  if [[ -z "$1" ]]; then echo 0; else printf '%s\n' "$1" | grep -c ''; fi
}

# section TITLE TEXT: a titled list with its size.
section() {
  echo "--- $1 ($(count "$2")) ---"
  if [[ -n "$2" ]]; then printf '%s\n' "$2"; fi
}

mode="${1:-}"
case "$mode" in
  "" | --all | --mark) ;;
  *) die "unknown argument '$mode' (use no argument, --all or --mark)" ;;
esac
[[ "$LIMIT" =~ ^[1-9][0-9]*$ ]] || die "HARVEST_LIMIT must be a positive number, got '$LIMIT'"

if [[ "$mode" == "--mark" ]]; then
  [[ -s "$STATE_FILE" ]] ||
    die "nothing has been listed in this clone: run scripts/upstream-harvest.sh, review the output, then --mark"
  marks=""
  while read -r name head stamp from; do
    if [[ "$from" == "-" ]]; then from=""; fi
    if [[ "$stamp" == "-" ]]; then stamp=""; fi
    [[ "$(mark_field "$name" 2)" == "$from" ]] ||
      die "$MARK_FILE changed for $name since the listing (which started from ${from:-no mark}): list again, then --mark"
    marks+="$name $head${stamp:+ $stamp}"$'\n'
  done < "$STATE_FILE"
  printf '%s' "$marks" > "$MARK_FILE"
  rm -f "$STATE_FILE"
  echo "Recorded in $MARK_FILE (the heads the last listing read):"
  cat "$MARK_FILE"
  exit 0
fi

for name in "${NAMES[@]}"; do
  git remote get-url "$name" > /dev/null 2>&1 || git remote add "$name" "${REMOTES[$name]}"
  git fetch -q "$name"
done

started="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
state=""
for name in "${NAMES[@]}"; do
  repo="${REPOS[$name]}"
  head="$(git rev-parse --verify -q "$name/main^{commit}")" || die "$name/main is not a commit after the fetch"
  mark="$(mark_field "$name" 2)"
  mark_stamp="$(mark_field "$name" 3)"
  # The mark file is repository content: refuse anything that is not a SHA or a
  # UTC timestamp before it reaches a git or gh command line.
  [[ -z "$mark" || "$mark" =~ ^[0-9a-f]{7,40}$ ]] || die "$MARK_FILE: '$mark' is not a commit SHA ($name)"
  [[ -z "$mark_stamp" || "$mark_stamp" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] ||
    die "$MARK_FILE: '$mark_stamp' is not a UTC timestamp ($name)"

  # The one place that decides what this listing covers: the commit range and
  # its wording, the PR/issue filter and its wording.
  since="$mark"
  stamp="$mark_stamp"
  if [[ "$mode" == "--all" ]]; then
    since=""
    stamp=""
  fi
  if [[ -n "$since" ]]; then
    range="$since..$head"
    scope="since $since"
  else
    git rev-parse --verify -q "main^{commit}" > /dev/null ||
      die "no mark to start from for $name, and this clone has no local branch 'main' to compare with"
    range="main..$head"
    scope="vs our main"
  fi
  if [[ -n "$stamp" ]]; then
    activity_query="state=all&since=$stamp"
    activity_scope="updated since $stamp, any state"
  else
    activity_query="state=open"
    activity_scope="open"
  fi

  # A range git cannot resolve must stop the harvest, not read as "nothing new".
  log_failed="git log $range failed: the mark for $name is not a commit this clone can reach (upstream force-pushed?). Review with --all, then --mark."
  in_components="$(git log --no-merges --date=short --format='%h %ad %s' "$range" -- "${COMPONENTS[@]}")" || die "$log_failed"
  everything="$(git log --no-merges --date=short --format='%h %ad %s' "$range")" || die "$log_failed"
  elsewhere="$(awk 'NR == FNR { seen[$0]; next } !($0 in seen)' \
    <(printf '%s\n' "$in_components") <(printf '%s\n' "$everything"))"

  echo "=================================================================="
  echo "$name ($repo) — $scope, head $head"
  echo "=================================================================="
  section "commits touching ${COMPONENTS[*]}" "$in_components"
  section "commits touching nothing there (docs, scripts, compose, workflows)" "$elsewhere"
  echo

  # PRs and issues come from one endpoint, filtered by update time on the
  # server (issues?since=), so an old PR that moved after the mark is listed.
  activity=""
  problem=""
  if ! command -v gh > /dev/null 2>&1; then
    problem="gh is not installed"
  else
    page=1
    while :; do
      chunk="$(gh api "repos/$repo/issues?$activity_query&sort=updated&direction=desc&per_page=100&page=$page" --jq "$ACTIVITY_JQ")" || {
        problem="gh api failed"
        break
      }
      if [[ -n "$chunk" ]]; then activity+="$chunk"$'\n'; fi
      if (($(count "$chunk") < 100)); then break; fi
      if (($(count "${activity%$'\n'}") >= LIMIT)); then
        problem="stopped at the limit of $LIMIT PRs + issues: older activity is not shown, raise HARVEST_LIMIT"
        break
      fi
      page=$((page + 1))
    done
  fi
  section "PRs $activity_scope" "$(sed -n 's/^PR //p' <<< "$activity")"
  section "issues $activity_scope" "$(sed -n 's/^IS //p' <<< "$activity")"

  # The timestamp only moves when everything since the old one was on screen.
  new_stamp="$started"
  if [[ -n "$problem" ]]; then
    new_stamp="$mark_stamp"
    echo "!! PR/issue listing of $repo is INCOMPLETE ($problem); --mark will keep the timestamp of $name where it was."
  elif [[ "$mode" == "--all" && -n "$mark_stamp" ]]; then
    new_stamp="$mark_stamp"
  fi
  echo
  state+="$name $head ${new_stamp:--} ${mark:--}"$'\n'
done

printf '%s' "$state" > "$STATE_FILE"
echo "When done reviewing: scripts/upstream-harvest.sh --mark   (records the heads listed above, not a fresh fetch)"
