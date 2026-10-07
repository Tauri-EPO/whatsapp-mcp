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
# you were reviewing is still new at the next harvest. Any worktree of the
# clone may run it. The timestamp of a mark is what the PR/issue listing
# filters on, so it only moves when that listing was complete; a missing gh, a
# failed call, the safety limit or --all leave it where it was.
#
# HARVEST_LIMIT (default 1000) bounds the PRs + issues printed per repository.
# Going over it is said in the output, never silent.
#
# Requires: git remotes `upstream` (verygoodplugins) and `lharries` (added when
# missing), a local `main` when there is no mark to start from (--all), and
# `gh` for the PR/issue listing. Nothing else: the text handling is bash.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
MARK_FILE=".upstream-harvest"
# What the last listing read, one line per remote: name, head, the timestamp
# --mark will record, the mark SHA the listing started from ("-" = none). Kept
# in the git directory the worktrees share: per clone, never committed. Empty
# means there is no complete listing to record.
STATE_FILE="$(git rev-parse --git-common-dir)/upstream-harvest.listed"
LIMIT="${HARVEST_LIMIT:-1000}"
NAMES=(upstream lharries)
COMPONENTS=(whatsapp-bridge whatsapp-mcp-server)
STAMP_RE='^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$'
# One line per PR or issue, newest update first; the first word says which.
ACTIVITY_JQ='.[] | "\(if .pull_request then "PR" else "IS" end) #\(.number) \(.updated_at[:10]) \(if .pull_request.merged_at then "merged" else .state end) \(.title)"'

die() {
  echo "upstream-harvest: $*" >&2
  exit 1
}

# The helpers below hand their result back in a variable instead of printing
# it, and use no external tool: every `$(...)` and every awk or sed is a new
# process, which under Git Bash on Windows costs a fraction of a second each.

# repo_of NAME: sets `repo` to the GitHub repository behind a remote (a case,
# not an associative array, so the script also runs on the bash 3.2 macOS ships).
repo_of() {
  case "$1" in
    upstream) repo="verygoodplugins/whatsapp-mcp" ;;
    lharries) repo="lharries/whatsapp-mcp" ;;
  esac
}

# read_mark NAME: sets `mark` and `mark_stamp` from NAME's line in the mark
# file, both empty when there is none. The file is CRLF in a Windows working
# copy, hence the trailing CR removed from either field.
read_mark() {
  mark=""
  mark_stamp=""
  [[ -f "$MARK_FILE" ]] || return 0
  local name sha stamp _
  while read -r name sha stamp _ || [[ -n "$name" ]]; do
    if [[ "$name" == "$1" ]]; then
      mark="${sha%$'\r'}"
      mark_stamp="${stamp%$'\r'}"
      return 0
    fi
  done < "$MARK_FILE"
}

# section TITLE PREFIX TEXT: the lines of TEXT that start with PREFIX, without
# it, as a titled list with its size.
section() {
  local line list="" n=0
  while IFS= read -r line; do
    if [[ "$line" == "$2"* ]]; then
      list+="${line#"$2"}"$'\n'
      n=$((n + 1))
    fi
  done <<< "$3"
  echo "--- $1 ($n) ---"
  printf '%s' "$list"
}

# github_now: sets `started` to GitHub's clock, from the Date header of an API
# response, in the spelling a mark carries; empty when gh cannot say. The
# timestamp is sent back as `since=` and compared with GitHub's own updated_at,
# so it has to be GitHub's time: with the local clock, a machine running fast
# would skip whatever moved in the gap.
github_now() {
  started=""
  local headers line day month year time _
  headers="$(gh api -i rate_limit 2> /dev/null)" || return 0
  while IFS= read -r line; do
    case "$line" in
      [Dd][Aa][Tt][Ee]:*)
        # Date: Wed, 07 Oct 2026 18:45:38 GMT
        read -r _ _ day month year time _ <<< "$line"
        case "$month" in
          Jan) month=01 ;; Feb) month=02 ;; Mar) month=03 ;; Apr) month=04 ;;
          May) month=05 ;; Jun) month=06 ;; Jul) month=07 ;; Aug) month=08 ;;
          Sep) month=09 ;; Oct) month=10 ;; Nov) month=11 ;; Dec) month=12 ;;
        esac
        started="$year-$month-${day}T${time}Z"
        if [[ ! "$started" =~ $STAMP_RE ]]; then started=""; fi
        return 0
        ;;
    esac
  done <<< "$headers"
}

(($# <= 1)) || die "one argument at most (--all or --mark), got $#: $*"
mode="${1:-}"
case "$mode" in
  "" | --all | --mark) ;;
  *) die "unknown argument '$mode' (use no argument, --all or --mark)" ;;
esac
[[ "$LIMIT" =~ ^[1-9][0-9]*$ ]] || die "HARVEST_LIMIT must be a positive number, got '$LIMIT'"

if [[ "$mode" == "--mark" ]]; then
  [[ -s "$STATE_FILE" ]] ||
    die "no complete listing to record in this clone: run scripts/upstream-harvest.sh, review the output, then --mark"
  marks=""
  while read -r name head stamp from; do
    if [[ "$from" == "-" ]]; then from=""; fi
    if [[ "$stamp" == "-" ]]; then stamp=""; fi
    read_mark "$name"
    [[ "$mark" == "$from" ]] ||
      die "$MARK_FILE differs for $name from the one the listing started from (${from:-no mark}): list again here, then --mark"
    marks+="$name $head${stamp:+ $stamp}"$'\n'
  done < "$STATE_FILE"
  printf '%s' "$marks" > "$MARK_FILE"
  : > "$STATE_FILE"
  echo "Recorded in $MARK_FILE (the heads the last listing read):"
  printf '%s' "$marks"
  exit 0
fi

for name in "${NAMES[@]}"; do
  repo_of "$name"
  git remote get-url "$name" > /dev/null 2>&1 || git remote add "$name" "https://github.com/$repo.git"
  git fetch -q "$name"
done

# A listing that stops half way must not leave an older one for --mark to record.
: > "$STATE_FILE"
started=""
have_gh=""
if command -v gh > /dev/null 2>&1; then
  have_gh=1
  github_now
fi
state=""
for name in "${NAMES[@]}"; do
  repo_of "$name"
  head="$(git rev-parse --verify -q "$name/main^{commit}")" || die "$name/main is not a commit after the fetch"
  read_mark "$name"
  # The mark file is repository content: refuse anything that is not a SHA or a
  # UTC timestamp before it reaches a git or gh command line.
  [[ -z "$mark" || "$mark" =~ ^[0-9a-f]{7,40}$ ]] || die "$MARK_FILE: '$mark' is not a commit SHA ($name)"
  [[ -z "$mark_stamp" || "$mark_stamp" =~ $STAMP_RE ]] || die "$MARK_FILE: '$mark_stamp' is not a UTC timestamp ($name)"

  # The one place that decides what this listing covers: the commit range and
  # its wording, the PR/issue filter and its wording, and the timestamp --mark
  # records if that filter's list turns out complete.
  since="$mark"
  stamp="$mark_stamp"
  next_stamp="$started"
  if [[ "$mode" == "--all" ]]; then
    since=""
    stamp=""
    # Open PRs and issues only: not everything that moved since the old
    # timestamp, so that one stays.
    if [[ -n "$mark_stamp" ]]; then next_stamp="$mark_stamp"; fi
  fi
  if [[ -n "$since" ]]; then
    # A mark the head does not descend from must stop the harvest, not read as
    # "nothing new": upstream rewrote main, or this clone never had the commit.
    git merge-base --is-ancestor "$since" "$head" 2> /dev/null ||
      die "the mark for $name ($since) is not an ancestor of $name/main ($head): upstream force-pushed, or this clone does not have that commit. Review with --all, then --mark."
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

  # One walk, each commit classified by the files it changed. A path-limited
  # `git log` would simplify history and drop commits that did touch a
  # component (a change and its revert behind one merge). A commit's header
  # line starts with a tab, which no path git prints can (it quotes those). Not
  # with "/": Git Bash rewrites an argument that looks like a POSIX path.
  log="$(git -c core.quotePath=false log --no-merges --date=short --format='%x09%h %ad %s' --name-only "$range")" ||
    die "git log $range failed for $name"
  commits=""
  commit=""
  where=""
  while IFS= read -r line; do
    if [[ "$line" == $'\t'* ]]; then
      if [[ -n "$commit" ]]; then commits+="$where $commit"$'\n'; fi
      commit="${line#$'\t'}"
      where="OUT"
    else
      for dir in "${COMPONENTS[@]}"; do
        if [[ "$line" == "$dir"/* ]]; then where="IN"; fi
      done
    fi
  done <<< "$log"
  if [[ -n "$commit" ]]; then commits+="$where $commit"$'\n'; fi

  echo "=================================================================="
  echo "$name ($repo) — $scope, head $head"
  echo "=================================================================="
  section "commits touching ${COMPONENTS[*]}" "IN " "$commits"
  section "commits touching nothing there (docs, scripts, compose, workflows)" "OUT " "$commits"
  echo

  # PRs and issues come from one endpoint, filtered by update time on the
  # server (issues?since=), so an old PR that moved after the mark is listed.
  activity=""
  problem=""
  if [[ -z "$have_gh" ]]; then
    problem="gh is not installed"
  elif [[ -z "$started" ]]; then
    problem="gh api failed"
  else
    fetched="$(gh api --paginate "repos/$repo/issues?$activity_query&sort=updated&direction=desc&per_page=100" --jq "$ACTIVITY_JQ")" || {
      fetched=""
      problem="gh api failed"
    }
    total=0
    while IFS= read -r line; do
      [[ -n "$line" ]] || continue
      total=$((total + 1))
      if ((total <= LIMIT)); then activity+="$line"$'\n'; fi
    done <<< "$fetched"
    if ((total > LIMIT)); then
      problem="$total PRs + issues, only the $LIMIT most recently updated are shown: raise HARVEST_LIMIT"
    fi
  fi
  section "PRs $activity_scope" "PR " "$activity"
  section "issues $activity_scope" "IS " "$activity"

  # The timestamp only moves when everything since the old one was on screen.
  if [[ -n "$problem" ]]; then
    next_stamp="$mark_stamp"
    echo "!! PR/issue listing of $repo is INCOMPLETE ($problem); --mark will keep the timestamp of $name where it was."
  fi
  echo
  state+="$name $head ${next_stamp:--} ${mark:--}"$'\n'
done

printf '%s' "$state" > "$STATE_FILE"
echo "When done reviewing: scripts/upstream-harvest.sh --mark   (records the heads listed above, not a fresh fetch)"
