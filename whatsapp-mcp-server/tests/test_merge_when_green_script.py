"""scripts/merge-when-green.sh merges only an up-to-date, green pull request.

The "protect main" ruleset requires the CI jobs on a head that is up to date
with main (issue #545: two green pull requests broke main together). The script
is the loop every merge goes through instead of a bare `gh pr merge`: update the
branch when main moved, wait while checks run, squash-merge, and say why when it
cannot.

It runs here against a fake `gh` on PATH that plays a scripted pull request: no
network, no repository. What the fake cannot show is the `--jq` projection the
script asks gh for (one "bucket<TAB>name" line per check): the fake prints those
lines itself.
"""

from __future__ import annotations

import os
import shutil
import stat
import subprocess
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "merge-when-green.sh"

BASH = shutil.which("bash")
pytestmark = pytest.mark.skipif(BASH is None, reason="needs bash to run scripts/merge-when-green.sh")

# The fake pull request.
#   FAKE_STATE     state answered (OPEN, CLOSED, MERGED); MERGED once a merge went through
#   FAKE_STATUSES  mergeStateStatus per successive read, comma-separated, the last repeats
#   FAKE_CHECKS    check snapshots per successive read, ";"-separated, the last repeats;
#                  a snapshot is "none" or "bucket:name,bucket:name"
#   FAKE_MERGE     ok | refuse-once | refuse-always
#   FAKE_GH_MODE   down = every call fails
# Every call is appended to FAKE_GH_LOG.
FAKE_GH = r"""#!/usr/bin/env bash
printf '%s\n' "$*" >>"$FAKE_GH_LOG"
if [ "${FAKE_GH_MODE:-ok}" = "down" ]; then
  echo "HTTP 502: Bad Gateway" >&2
  exit 1
fi
nth() { # $1 counter name, $2 separator, $3 list: the entry for this call, the last one repeating
  local file="$FAKE_GH_LOG.$1" n items last
  n=$(cat "$file" 2>/dev/null || echo 0)
  echo $((n + 1)) >"$file"
  IFS="$2" read -r -a items <<<"$3"
  last=$((${#items[@]} - 1))
  [ "$n" -gt "$last" ] && n=$last
  printf '%s' "${items[$n]}"
}
case "$*" in
  "pr view "*"--json state "*)
    if [ -f "$FAKE_GH_LOG.merged" ]; then echo MERGED; else echo "${FAKE_STATE:-OPEN}"; fi ;;
  "pr view "*"--json mergeStateStatus "*) nth statuses , "${FAKE_STATUSES:-CLEAN}"; echo ;;
  "pr view "*"--json title "*) echo "fix(bridge): a thing is fixed" ;;
  "pr view "*"--json body "*) echo "What changed. Closes #1" ;;  # one line: the log holds one call per line
  "pr update-branch "*) echo "branch updated" ;;
  "pr checks "*)
    snapshot=$(nth checks ';' "${FAKE_CHECKS:-pass:Python Lint,pass:Go Build,pass:Docker Build}")
    [ "$snapshot" = "none" ] && exit 1
    IFS=',' read -r -a entries <<<"$snapshot"
    for entry in "${entries[@]}"; do printf '%s\t%s\n' "${entry%%:*}" "${entry#*:}"; done
    case "$snapshot" in *pending:*) exit 8 ;; *fail:*) exit 1 ;; esac ;;
  "pr merge "*)
    mode="${FAKE_MERGE:-ok}"
    if [ "$mode" = "refuse-always" ] || { [ "$mode" = "refuse-once" ] && [ ! -f "$FAKE_GH_LOG.refused" ]; }; then
      touch "$FAKE_GH_LOG.refused"
      echo "the base branch policy prohibits the merge" >&2
      exit 1
    fi
    touch "$FAKE_GH_LOG.merged" ;;
esac
"""


def _run(tmp_path: Path, *args: str, **env: str) -> tuple[subprocess.CompletedProcess[str], list[str]]:
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir(exist_ok=True)
    gh = bin_dir / "gh"
    gh.write_text(FAKE_GH, encoding="utf-8", newline="\n")
    gh.chmod(gh.stat().st_mode | stat.S_IEXEC)
    log = tmp_path / "gh.log"
    full_env = {
        **os.environ,
        "PATH": f"{bin_dir}{os.pathsep}{os.environ['PATH']}",
        "FAKE_GH_LOG": str(log),
        "MERGE_POLL_SECONDS": "0",
        "MERGE_MAX_ROUNDS": "4",
        "MERGE_MAX_WAITS": "3",
        "MERGE_API_RETRIES": "2",
        "GH_REPO": "example/repo",
        **env,
    }
    assert BASH is not None
    done = subprocess.run([BASH, str(SCRIPT), *args], capture_output=True, text=True, env=full_env, timeout=180)
    calls = log.read_text(encoding="utf-8").splitlines() if log.exists() else []
    return done, calls


def _calls(calls: list[str], verb: str) -> list[str]:
    return [c for c in calls if c.startswith(f"pr {verb} ")]


def test_a_green_up_to_date_pull_request_is_squashed_under_its_title_and_description(tmp_path):
    done, calls = _run(tmp_path, "12")
    assert done.returncode == 0, done.stderr
    (merge,) = _calls(calls, "merge")
    assert merge.startswith("pr merge 12 --repo example/repo --squash --delete-branch")
    # After an update the branch holds a merge commit, and GitHub's default
    # message would be the commit list: the title and the description are passed.
    assert "--subject fix(bridge): a thing is fixed (#12)" in merge
    assert "--body What changed. Closes #1" in merge
    assert not _calls(calls, "update-branch")


def test_a_subject_passed_by_the_caller_wins_over_the_pull_request_title(tmp_path):
    done, calls = _run(tmp_path, "12", "--subject", "chore(deps): bump the python dev tooling (#12)")
    assert done.returncode == 0, done.stderr
    (merge,) = _calls(calls, "merge")
    assert merge.count("--subject") == 1
    assert "--subject chore(deps): bump the python dev tooling (#12)" in merge
    assert "a thing is fixed" not in merge


def test_a_branch_behind_main_is_updated_and_its_checks_awaited_before_the_merge(tmp_path):
    done, calls = _run(
        tmp_path,
        "12",
        FAKE_STATUSES="BEHIND,BLOCKED,BLOCKED,CLEAN",
        FAKE_CHECKS="none;pending:Go Build,pass:Python Lint;pass:Go Build,pass:Python Lint",
    )
    assert done.returncode == 0, done.stderr
    order = [
        c.split()[1] for c in calls if c.startswith("pr ") and c.split()[1] in {"update-branch", "checks", "merge"}
    ]
    assert order[0] == "update-branch"
    assert order.count("checks") == 3 and order.count("merge") == 1
    assert order[-1] == "merge"


def test_a_merge_refused_because_main_moved_goes_round_again(tmp_path):
    done, calls = _run(tmp_path, "12", FAKE_STATUSES="CLEAN,BEHIND,BEHIND,CLEAN", FAKE_MERGE="refuse-once")
    assert done.returncode == 0, done.stderr
    assert len(_calls(calls, "merge")) == 2
    assert len(_calls(calls, "update-branch")) == 1


def test_a_merge_refused_for_another_reason_is_not_retried(tmp_path):
    """A typo in a passed-through flag, a missing review: the same refusal every round."""
    done, calls = _run(tmp_path, "12", FAKE_STATUSES="CLEAN", FAKE_MERGE="refuse-always")
    assert done.returncode == 1
    assert "prohibits the merge" in done.stderr and "waiting will not change it" in done.stderr
    assert len(_calls(calls, "merge")) == 1


def test_a_red_check_is_never_merged_over(tmp_path):
    done, calls = _run(tmp_path, "12", FAKE_CHECKS="pass:Python Lint,fail:Go Build")
    assert done.returncode == 1
    assert "Go Build" in done.stderr
    assert not _calls(calls, "merge")


def test_a_cancelled_check_is_not_green(tmp_path):
    """`gh pr checks --watch` exits 0 over a cancelled check; the script must not."""
    done, calls = _run(tmp_path, "12", FAKE_CHECKS="pass:Python Lint,cancel:CodeQL Analysis (Go)")
    assert done.returncode == 1
    assert "CodeQL Analysis (Go)" in done.stderr
    assert not _calls(calls, "merge")


def test_a_conflict_with_main_stops_with_its_own_status(tmp_path):
    done, calls = _run(tmp_path, "12", FAKE_STATUSES="DIRTY")
    assert done.returncode == 2
    assert "rebase it by hand" in done.stderr
    assert not _calls(calls, "merge") and not _calls(calls, "update-branch")


def test_a_draft_is_not_merged(tmp_path):
    done, calls = _run(tmp_path, "12", FAKE_STATUSES="DRAFT")
    assert done.returncode == 1 and "draft" in done.stderr
    assert not _calls(calls, "merge")


def test_a_branch_that_never_catches_up_gives_up_instead_of_looping_for_ever(tmp_path):
    done, calls = _run(tmp_path, "12", FAKE_STATUSES="BEHIND")
    assert done.returncode == 3
    assert len(_calls(calls, "update-branch")) == 4  # MERGE_MAX_ROUNDS
    assert not _calls(calls, "merge")


def test_a_head_no_check_ever_reports_for_gives_up(tmp_path):
    done, calls = _run(tmp_path, "12", FAKE_CHECKS="none")
    assert done.returncode == 3 and "no check ever reported" in done.stderr
    assert not _calls(calls, "merge")


def test_a_merge_state_github_never_settles_gives_up(tmp_path):
    done, calls = _run(tmp_path, "12", FAKE_STATUSES="UNKNOWN")
    assert done.returncode == 3 and "never settled" in done.stderr
    assert not _calls(calls, "merge")


def test_a_closed_pull_request_is_an_error_and_a_merged_one_is_done(tmp_path):
    closed, calls = _run(tmp_path, "12", FAKE_STATE="CLOSED")
    assert closed.returncode == 1 and "CLOSED" in closed.stderr
    assert not _calls(calls, "merge")
    merged, calls = _run(tmp_path, "12", FAKE_STATE="MERGED")
    assert merged.returncode == 0 and "is merged" in merged.stdout
    assert not _calls(calls, "merge")


def test_github_being_unreachable_has_its_own_status_and_is_not_a_failed_check(tmp_path):
    done, calls = _run(tmp_path, "12", FAKE_GH_MODE="down")
    assert done.returncode == 4
    assert "did not answer" in done.stderr
    assert len(calls) == 2  # MERGE_API_RETRIES, then it stops


def test_a_missing_or_non_numeric_pull_request_is_a_usage_error(tmp_path):
    done, calls = _run(tmp_path, "main")
    assert done.returncode == 64
    assert calls == []
