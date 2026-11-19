"""release-cut.yml must approve every waiting run of the release PR head, and only those.

Issue #849: the 2.13.0 cut approved the release PR's CI run and never its
Security run, because the filtered run listing left the waiting Security run
out. The required Bandit check was then missing, `gh pr merge` was refused
three times and the job blamed a moving `main`.

The "Merge the release PR" step is cut out of the workflow file and run here
with a fake `gh` on PATH whose state lives in files: no network. The step
approves workflow runs, so the deny cases matter as much as the fix: a fork's
run on the same commit and a run of another commit are never approved, and a
pull request that stays blocked is never merged.
"""

from __future__ import annotations

import os
import shutil
import stat
import subprocess
from pathlib import Path

import pytest

from tests.bash_helpers import BASH_SKIP_REASON, bash_command, resolve_bash

ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / ".github" / "workflows" / "release-cut.yml"

BASH = resolve_bash()
pytestmark = pytest.mark.skipif(
    BASH is None or shutil.which("jq") is None, reason=BASH_SKIP_REASON + "; also needs jq (the step pipes into it)"
)

# State files in $FAKE_STATE: `ci` and `sec` hold action_required, in_progress,
# success or failure; `head` the release PR head; `calls` every invocation.
# SCENARIO selects what the listings hide and what goes wrong.
FAKE_GH = r"""#!/usr/bin/env bash
S=$FAKE_STATE
printf '%s\n' "$*" >>"$S/calls"
ci=$(cat "$S/ci"); sec=$(cat "$S/sec"); head=$(cat "$S/head")
run_json() { # id path state [repo] [sha]
  local concl status
  case "$3" in
    action_required) status=completed; concl='"action_required"' ;;
    in_progress) status=in_progress; concl=null ;;
    *) status=completed; concl="\"$3\"" ;;
  esac
  printf '{"id":%s,"path":"%s","status":"%s","conclusion":%s,"head_sha":"%s","event":"pull_request","head_branch":"release-please--branches--main","head_repository":{"full_name":"%s"},"created_at":"2026-01-01T00:00:0%sZ"}\n' \
    "$1" "$2" "$status" "$concl" "${5:-SHA1}" "${4:-example/repo}" "$1"
}
merge_state() { [ "$ci" = success ] && [ "$sec" = success ] && echo CLEAN || echo BLOCKED; }
case "$*" in
  "pr view 848 --json headRefOid --jq .headRefOid") echo "$head" ;;
  "api repos/example/repo/compare/release-please--branches--main...main --jq .ahead_by") echo 0 ;;
  "api repos/example/repo/actions/runs?head_sha=SHA1&event=pull_request&branch=release-please--branches--main&per_page=100 --jq .workflow_runs[]")
    run_json 1 .github/workflows/ci.yml "$ci"
    # Issue #849: this listing leaves the waiting Security run out.
    case "$SCENARIO" in
      listed) run_json 2 .github/workflows/security.yml "$sec" ;;
      *) [ "$sec" = action_required ] || run_json 2 .github/workflows/security.yml "$sec" ;;
    esac ;;
  "api repos/example/repo/actions/runs?per_page=100&page=1 --jq .workflow_runs[]")
    run_json 1 .github/workflows/ci.yml "$ci"
    case "$SCENARIO" in nowhere | third_page) ;; *) run_json 2 .github/workflows/security.yml "$sec" ;; esac
    # Never ours: a fork's run on the same commit, and another commit's run.
    run_json 7 .github/workflows/ci.yml action_required fork/repo
    run_json 8 .github/workflows/security.yml action_required example/repo OTHER ;;
  "api repos/example/repo/actions/runs?per_page=100&page=2 --jq .workflow_runs[]") ;;
  "api repos/example/repo/actions/runs?per_page=100&page=3 --jq .workflow_runs[]")
    [ "$SCENARIO" != third_page ] || run_json 2 .github/workflows/security.yml "$sec" ;;
  "api repos/example/repo/actions/runs?head_sha=SHA2"*) ;;
  "api -X POST repos/example/repo/actions/runs/1/approve") echo in_progress >"$S/ci" ;;
  "api -X POST repos/example/repo/actions/runs/2/approve")
    if [ "$SCENARIO" = approve_flaky ] && [ ! -e "$S/refused_once" ]; then
      : >"$S/refused_once"; echo "gh: HTTP 502" >&2; exit 1
    fi
    if [ "$SCENARIO" = security_fails ]; then echo failure >"$S/sec"; else echo in_progress >"$S/sec"; fi ;;
  "run watch 1 --exit-status --interval 20") echo success >"$S/ci" ;;
  "pr view 848 --json mergeStateStatus,headRefOid --jq "*)
    # A Security run in progress finishes on the second look at the merge state.
    if [ "$sec" = in_progress ]; then
      n=$(($(cat "$S/looks" 2>/dev/null || echo 0) + 1)); echo "$n" >"$S/looks"
      [ "$n" -lt 2 ] || echo success >"$S/sec"
    fi
    sec=$(cat "$S/sec")
    if [ "$SCENARIO" = head_moves ]; then echo SHA2 >"$S/head"; head=SHA2; fi
    echo "$(merge_state) $head" ;;
  "pr view 848 --json statusCheckRollup --jq "*) echo "  Python Lint: SUCCESS" ;;
  "pr merge 848 --squash --delete-branch --match-head-commit SHA1")
    if [ "$(merge_state)" = CLEAN ]; then : >"$S/merged"; else
      echo "X Pull request is not mergeable: the base branch policy prohibits the merge." >&2; exit 1
    fi ;;
  *) echo "fake gh: unexpected call: $*" >&2; exit 97 ;;
esac
"""


def _merge_step() -> str:
    """The `run:` script of the step with `id: merge`, as Actions would write it to a file."""
    lines = WORKFLOW.read_text(encoding="utf-8").splitlines()
    at = lines.index("        id: merge")
    start = next(i for i in range(at, len(lines)) if lines[i] == "        run: |") + 1
    body = []
    for line in lines[start:]:
        if line.strip() and not line.startswith(" " * 10):
            break
        body.append(line[10:])
    assert any("gh pr merge" in line for line in body), "the merge step was not found in release-cut.yml"
    # Actions runs a step with `bash --noprofile --norc -eo pipefail`.
    return "set -eo pipefail\n" + "\n".join(body) + "\n"


def _run(
    tmp_path: Path, scenario: str, *, dry_run: bool = False
) -> tuple[subprocess.CompletedProcess[str], list[str], bool]:
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    for name, text in (("gh", FAKE_GH), ("sleep", "#!/usr/bin/env bash\nexit 0\n")):
        tool = bin_dir / name
        tool.write_text(text, encoding="utf-8", newline="\n")
        tool.chmod(tool.stat().st_mode | stat.S_IEXEC)
    state = tmp_path / "state"
    state.mkdir()
    for name, value in (("ci", "action_required"), ("sec", "action_required"), ("head", "SHA1")):
        (state / name).write_text(value + "\n", encoding="utf-8", newline="\n")
    script = tmp_path / "step.sh"
    script.write_text(_merge_step(), encoding="utf-8", newline="\n")
    env = {
        **os.environ,
        "PATH": f"{bin_dir}{os.pathsep}{os.environ['PATH']}",
        "FAKE_STATE": state.as_posix(),
        "SCENARIO": scenario,
        "GH_REPO": "example/repo",
        "PR": "848",
        "BRANCH": "release-please--branches--main",
        "DRY_RUN": "true" if dry_run else "false",
        "BLOCKED_WAIT_S": "0" if scenario == "nowhere" else "1800",
    }
    assert BASH is not None
    done = subprocess.run(
        bash_command(BASH, script, bin_dir=bin_dir), capture_output=True, text=True, env=env, timeout=180
    )
    calls = (state / "calls").read_text(encoding="utf-8").splitlines()
    return done, calls, (state / "merged").exists()


def _approved(calls: list[str]) -> list[str]:
    return [c.rsplit("/", 2)[1] for c in calls if c.startswith("api -X POST") and c.endswith("/approve")]


def _merge_attempts(calls: list[str]) -> int:
    return sum(c.startswith("pr merge ") for c in calls)


@pytest.mark.parametrize("scenario", ["hidden_while_waiting", "listed", "third_page"])
def test_the_security_run_is_approved_wherever_it_is_listed(tmp_path, scenario):
    done, calls, merged = _run(tmp_path, scenario)

    assert done.returncode == 0, done.stdout + done.stderr
    # Each of ours once; never the fork's run (7) or the other commit's (8).
    assert sorted(_approved(calls)) == ["1", "2"]
    assert merged and _merge_attempts(calls) == 1


def test_a_dry_run_approves_and_waits_but_merges_nothing(tmp_path):
    done, calls, merged = _run(tmp_path, "hidden_while_waiting", dry_run=True)

    assert done.returncode == 0, done.stdout + done.stderr
    assert sorted(_approved(calls)) == ["1", "2"]
    assert "is CLEAN after its CI passed; the cut would merge it" in done.stdout
    assert not merged and _merge_attempts(calls) == 0


@pytest.mark.parametrize("dry_run", [False, True])
def test_a_pull_request_that_stays_blocked_is_not_merged_and_the_error_names_the_checks(tmp_path, dry_run):
    done, calls, merged = _run(tmp_path, "nowhere", dry_run=dry_run)

    assert done.returncode == 1
    assert _approved(calls) == ["1"]
    assert not merged and _merge_attempts(calls) == 0
    assert "is still blocked" in done.stdout and "Bandit Security Scan" in done.stdout
    assert "  Python Lint: SUCCESS" in done.stdout
    assert "main keeps moving" not in done.stdout


def test_a_failed_security_run_stops_the_cut_without_waiting(tmp_path):
    done, calls, merged = _run(tmp_path, "security_fails")

    assert done.returncode == 1
    assert ".github/workflows/security.yml (run 2): failure" in done.stdout
    assert not merged and _merge_attempts(calls) == 0
    # One look at the merge state, not a wait: the budget here is 1800 s.
    assert sum(c.startswith("pr view 848 --json mergeStateStatus") for c in calls) == 1


def test_a_refused_approval_is_tried_again(tmp_path):
    done, calls, merged = _run(tmp_path, "approve_flaky")

    assert done.returncode == 0, done.stdout + done.stderr
    assert _approved(calls).count("2") == 2
    assert "could not approve run 2 (try 1 of 3)" in done.stdout
    assert merged


def test_a_head_that_moves_during_the_wait_is_not_merged(tmp_path):
    done, calls, merged = _run(tmp_path, "head_moves")

    assert "the release branch moved while waiting for its checks; going round again" in done.stdout
    assert not merged and _merge_attempts(calls) == 0
    # The new head has no run in this fake, so the next attempt stops on its own.
    assert done.returncode == 1
    assert "No approved pull_request CI run appeared for the release PR head SHA2" in done.stdout
