"""scripts/merge-when-green.sh merges only an up-to-date, green pull request.

The "protect main" ruleset requires the CI jobs on a head that is up to date
with main (issue #545: two green pull requests broke main together). The script
is the loop every lane runs instead of a bare `gh pr merge`: update the branch
when main moved, wait for the checks, squash-merge, and say why when it cannot.

It runs here against a fake `gh` on PATH that plays a scripted pull request: no
network, no repository.
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

# The fake pull request. FAKE_STATUSES is the mergeStateStatus answered on each
# successive `gh pr view --json mergeStateStatus` (the last one repeats);
# FAKE_STATE the state; FAKE_CHECKS "pass", "fail" or "none"; FAKE_MERGE "ok" or
# "refuse-once". Every call is appended to FAKE_GH_LOG.
FAKE_GH = r"""#!/usr/bin/env bash
printf '%s\n' "$*" >>"$FAKE_GH_LOG"
count_file="$FAKE_GH_LOG.count"
case "$*" in
  "pr view "*"--json state"*)
    if [ -f "$FAKE_GH_LOG.merged" ]; then echo MERGED; else echo "${FAKE_STATE:-OPEN}"; fi ;;
  "pr view "*"--json mergeStateStatus"*)
    n=$(cat "$count_file" 2>/dev/null || echo 0)
    IFS=',' read -r -a statuses <<<"$FAKE_STATUSES"
    last=$((${#statuses[@]} - 1))
    [ "$n" -gt "$last" ] && n=$last
    echo "${statuses[$n]}"
    echo $(( $(cat "$count_file" 2>/dev/null || echo 0) + 1 )) >"$count_file" ;;
  "pr update-branch "*) echo "branch updated" ;;
  "pr checks "*"--json name,bucket"*)
    [ "${FAKE_CHECKS:-pass}" = "fail" ] && echo "Go Build" ;;
  "pr checks "*"--watch"*)
    case "${FAKE_CHECKS:-pass}" in
      pass) exit 0 ;;
      *) exit 1 ;;
    esac ;;
  "pr merge "*)
    if [ "${FAKE_MERGE:-ok}" = "refuse-once" ] && [ ! -f "$FAKE_GH_LOG.refused" ]; then
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
        "FAKE_STATUSES": "CLEAN",
        "MERGE_POLL_SECONDS": "0",
        "MERGE_MAX_ROUNDS": "4",
        "MERGE_MAX_WAITS": "3",
        "GH_REPO": "example/repo",
        **env,
    }
    assert BASH is not None
    done = subprocess.run([BASH, str(SCRIPT), *args], capture_output=True, text=True, env=full_env, timeout=120)
    calls = log.read_text(encoding="utf-8").splitlines() if log.exists() else []
    return done, calls


def test_a_green_up_to_date_pull_request_is_squash_merged_with_the_flags_given(tmp_path):
    done, calls = _run(tmp_path, "12", "--subject", "chore(deps): bump the python dev tooling (#12)")
    assert done.returncode == 0, done.stderr
    merges = [c for c in calls if c.startswith("pr merge ")]
    assert merges == [
        "pr merge 12 --repo example/repo --squash --delete-branch --subject chore(deps): bump the python dev tooling (#12)"
    ]
    assert not [c for c in calls if c.startswith("pr update-branch")]


def test_a_branch_behind_main_is_updated_and_its_checks_awaited_before_the_merge(tmp_path):
    done, calls = _run(tmp_path, "12", FAKE_STATUSES="BEHIND,BLOCKED,CLEAN")
    assert done.returncode == 0, done.stderr
    order = [c.split()[1] for c in calls if c.split()[1] in {"update-branch", "checks", "merge"}]
    assert order[0] == "update-branch"
    assert order.index("checks") < order.index("merge")
    assert order.count("merge") == 1


def test_a_merge_refused_because_main_moved_goes_round_again(tmp_path):
    done, calls = _run(tmp_path, "12", FAKE_STATUSES="CLEAN,BEHIND,CLEAN", FAKE_MERGE="refuse-once")
    assert done.returncode == 0, done.stderr
    assert len([c for c in calls if c.startswith("pr merge ")]) == 2
    assert len([c for c in calls if c.startswith("pr update-branch")]) == 1


def test_a_red_check_is_never_merged_over(tmp_path):
    done, calls = _run(tmp_path, "12", FAKE_CHECKS="fail")
    assert done.returncode == 1
    assert "Go Build" in done.stderr
    assert not [c for c in calls if c.startswith("pr merge ")]


def test_a_conflict_with_main_stops_with_its_own_status(tmp_path):
    done, calls = _run(tmp_path, "12", FAKE_STATUSES="DIRTY")
    assert done.returncode == 2
    assert "rebase it by hand" in done.stderr
    assert not [c for c in calls if c.startswith(("pr merge ", "pr update-branch"))]


def test_a_branch_that_never_catches_up_gives_up_instead_of_looping_for_ever(tmp_path):
    done, calls = _run(tmp_path, "12", FAKE_STATUSES="BEHIND")
    assert done.returncode == 3
    assert len([c for c in calls if c.startswith("pr update-branch")]) == 4  # MERGE_MAX_ROUNDS
    assert not [c for c in calls if c.startswith("pr merge ")]


def test_an_already_merged_or_closed_pull_request_is_reported(tmp_path):
    done, calls = _run(tmp_path, "12", FAKE_STATE="CLOSED")
    assert done.returncode == 1 and "CLOSED" in done.stderr
    assert not [c for c in calls if c.startswith("pr merge ")]


def test_a_missing_or_non_numeric_pull_request_is_a_usage_error(tmp_path):
    done, calls = _run(tmp_path, "main")
    assert done.returncode == 64
    assert calls == []
