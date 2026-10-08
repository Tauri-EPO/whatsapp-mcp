"""scripts/upstream-harvest.sh must not hide what --mark then records as reviewed.

The script lists what the two upstream projects did since the last harvest
(AGENTS.md section 2) and `--mark` records it. Issue #455: it used to cap the
PR/issue lists without saying so, never compare them with the mark's timestamp,
drop commits outside the two component directories, mark a head it had not
listed, and read a failing `git log` as "nothing new".

Here it runs against two local repositories standing in for the upstreams and a
fake `gh` on PATH: no network, and the remote can be moved between the listing
and the mark.
"""

from __future__ import annotations

import os
import shutil
import stat
import subprocess
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
HARVEST = ROOT / "scripts" / "upstream-harvest.sh"

BASH = shutil.which("bash")
GIT = shutil.which("git")
pytestmark = pytest.mark.skipif(
    BASH is None or GIT is None, reason="needs bash and git to run scripts/upstream-harvest.sh"
)

# What the fake GitHub clock says, and the same instant as a mark spells it.
GH_DATE = "Wed, 07 Oct 2026 18:45:38 GMT"
GH_STAMP = "2026-10-07T18:45:38Z"
LATER_DATE = "Thu, 08 Oct 2026 09:00:00 GMT"
LATER_STAMP = "2026-10-08T09:00:00Z"

# `gh api -i rate_limit` answers with headers (the script reads GitHub's clock
# from Date); any other call answers with the lines the real `--jq` would print.
FAKE_GH = r"""#!/usr/bin/env bash
printf '%s\n' "$*" >>"$FAKE_GH_LOG"
if [ "${FAKE_GH_MODE:-ok}" = "fail" ]; then
  echo "gh: could not resolve host api.github.com" >&2
  exit 1
fi
case "$*" in
  "api -i rate_limit")
    printf 'HTTP/2.0 200 OK\r\nDate: %s\r\nContent-Type: application/json\r\n\r\n{}\n' "$FAKE_GH_DATE" ;;
  *)
    if [ -n "${FAKE_GH_FILLER:-}" ]; then
      i=1
      while [ "$i" -le "$FAKE_GH_FILLER" ]; do
        echo "PR #$i 2026-10-01 open filler $i"
        i=$((i + 1))
      done
    else
      echo "PR #171 2026-09-29 open an old PR that moved after the mark"
      echo "PR #250 2026-09-23 merged a merged PR"
      echo "IS #9 2026-09-30 closed an issue"
    fi ;;
esac
"""


class Harvest:
    """Our fork with the script, two fake upstreams as its remotes, a fake gh on PATH.

    Everything lives under one directory and the remotes are relative paths, so a
    prepared tree can be copied instead of rebuilt: spawning git a dozen times
    per test is what makes this slow on Windows.
    """

    def __init__(self, root: Path) -> None:
        self.root = root
        self.up = root / "up"
        self.lh = root / "lh"
        self.fork = root / "fork"
        self.marks = self.fork / ".upstream-harvest"
        self.gh_log = root / "gh.log"
        self.env = {
            **os.environ,
            "PATH": str(root / "bin") + os.pathsep + os.environ.get("PATH", ""),
            # The developer's own git settings (autocrlf, signing, hooks) stay out.
            "GIT_CONFIG_GLOBAL": str(root / "gitconfig"),
            "GIT_CONFIG_NOSYSTEM": "1",
            "GIT_AUTHOR_NAME": "t",
            "GIT_AUTHOR_EMAIL": "t@example.invalid",
            "GIT_COMMITTER_NAME": "t",
            "GIT_COMMITTER_EMAIL": "t@example.invalid",
            "FAKE_GH_LOG": str(self.gh_log),
            "FAKE_GH_DATE": GH_DATE,
        }
        self.env.pop("HARVEST_LIMIT", None)

    def build(self) -> None:
        """Create the three repositories and the fake gh under an empty root."""
        (self.root / "bin").mkdir()
        gh = self.root / "bin" / "gh"
        gh.write_text(FAKE_GH, encoding="utf-8", newline="\n")
        gh.chmod(gh.stat().st_mode | stat.S_IEXEC | stat.S_IXGRP | stat.S_IXOTH)
        (self.root / "gitconfig").write_text("", encoding="utf-8")
        for repo in (self.up, self.lh):
            self.git(self.root, "init", "-q", "-b", "main", repo.name)
            self.commit(repo, "whatsapp-bridge/main.go", f"{repo.name}: bridge change")
            self.commit(repo, "scripts/launchd.sh", f"{repo.name}: scripts-only change")
        self.git(self.root, "init", "-q", "-b", "main", self.fork.name)
        (self.fork / "scripts").mkdir()
        (self.fork / "scripts" / "upstream-harvest.sh").write_bytes(HARVEST.read_bytes())
        self.commit(self.fork, "README.md", "our main")
        self.git(self.fork, "remote", "add", "upstream", "../up")
        self.git(self.fork, "remote", "add", "lharries", "../lh")

    def copy_to(self, root: Path) -> Harvest:
        shutil.copytree(self.root, root)
        return Harvest(root)

    def git(self, cwd: Path, *args: str) -> str:
        assert GIT is not None
        done = subprocess.run([GIT, *args], cwd=cwd, env=self.env, capture_output=True, text=True, encoding="utf-8")
        assert done.returncode == 0, f"git {' '.join(args)}: {done.stderr}"
        return done.stdout.strip()

    def commit(self, repo: Path, path: str, subject: str) -> str:
        target = repo / path
        target.parent.mkdir(parents=True, exist_ok=True)
        with target.open("a", encoding="utf-8", newline="\n") as f:
            f.write(subject + "\n")
        self.git(repo, "add", "-A")
        self.git(repo, "commit", "-q", "-m", subject)
        return self.head(repo)

    def head(self, repo: Path) -> str:
        return self.git(repo, "rev-parse", "HEAD")

    def run(self, *args: str, cwd: Path | None = None, **env: str) -> subprocess.CompletedProcess[str]:
        assert BASH is not None
        return subprocess.run(
            [BASH, str(self.fork / "scripts" / "upstream-harvest.sh"), *args],
            cwd=cwd or self.fork,
            env={**self.env, **env},
            capture_output=True,
            text=True,
            encoding="utf-8",
            timeout=300,
        )

    def listed(self, *args: str, **env: str) -> str:
        done = self.run(*args, **env)
        assert done.returncode == 0, done.stdout + done.stderr
        return done.stdout

    def record(self, cwd: Path | None = None) -> None:
        done = self.run("--mark", cwd=cwd)
        assert done.returncode == 0, done.stdout + done.stderr

    def mark(self, name: str, cwd: Path | None = None) -> list[str]:
        """The fields after the name on that remote's line of the mark file."""
        for line in ((cwd or self.fork) / ".upstream-harvest").read_text(encoding="utf-8").splitlines():
            if line.split()[0] == name:
                return line.split()[1:]
        raise AssertionError(f"no mark for {name}")

    def gh_calls(self) -> str:
        return self.gh_log.read_text(encoding="utf-8") if self.gh_log.exists() else ""


@pytest.fixture(scope="module")
def fresh_template(tmp_path_factory: pytest.TempPathFactory) -> Harvest:
    template = Harvest(tmp_path_factory.mktemp("harvest") / "fresh")
    template.root.mkdir()
    template.build()
    return template


@pytest.fixture(scope="module")
def marked_template(fresh_template: Harvest, tmp_path_factory: pytest.TempPathFactory) -> Harvest:
    """The same tree after one listing and its --mark: both remotes marked at GH_STAMP."""
    template = fresh_template.copy_to(tmp_path_factory.mktemp("harvest") / "marked")
    template.listed()
    template.record()
    template.gh_log.unlink()
    return template


@pytest.fixture
def harvest(fresh_template: Harvest, tmp_path: Path) -> Harvest:
    """Nothing listed, nothing marked."""
    return fresh_template.copy_to(tmp_path / "w")


@pytest.fixture
def marked(marked_template: Harvest, tmp_path: Path) -> Harvest:
    """Both upstreams harvested and marked at their current heads."""
    return marked_template.copy_to(tmp_path / "w")


def section(out: str, remote: str, title: str) -> list[str]:
    """The lines listed under `--- <title> (n) ---` in one remote's block."""
    block = out.split(f"\n{remote} (", 1)[1]
    lines = block.split(f"--- {title} (", 1)[1].splitlines()[1:]
    taken = []
    for line in lines:
        if not line or line.startswith(("---", "===", "!!")):
            break
        taken.append(line)
    return taken


def subjects(out: str, remote: str, title: str) -> list[str]:
    """The subjects of the `<sha> <date> <subject>` lines of a commit section."""
    return [line.split(" ", 2)[2] for line in section(out, remote, title)]


COMPONENT_COMMITS = "commits touching whatsapp-bridge whatsapp-mcp-server"
OTHER_COMMITS = "commits touching nothing there"


def test_mark_refuses_before_any_listing(harvest: Harvest) -> None:
    done = harvest.run("--mark")
    assert done.returncode != 0
    assert "no complete listing to record" in done.stderr
    assert not harvest.marks.exists()


def test_first_listing_compares_with_our_main_and_shows_commits_outside_the_components(harvest: Harvest) -> None:
    out = harvest.listed()
    assert f"upstream (verygoodplugins/whatsapp-mcp) — vs our main, head {harvest.head(harvest.up)}" in out
    assert subjects(out, "upstream", COMPONENT_COMMITS) == ["up: bridge change"]
    # A commit that touches only scripts/ used to be dropped by the path filter.
    assert subjects(out, "upstream", OTHER_COMMITS) == ["up: scripts-only change"]
    assert subjects(out, "lharries", OTHER_COMMITS) == ["lh: scripts-only change"]
    assert "--- PRs open (2) ---" in out
    assert "--- issues open (1) ---" in out
    assert "issues?state=open&sort=updated" in harvest.gh_calls()
    assert "since=" not in harvest.gh_calls()


def test_mark_records_the_listed_head_not_what_upstream_pushed_meanwhile(harvest: Harvest) -> None:
    harvest.listed()
    listed_head = harvest.head(harvest.up)
    late = harvest.commit(harvest.up, "whatsapp-bridge/late.go", "up: pushed while reviewing")
    assert late != listed_head

    harvest.record()
    # The timestamp is GitHub's clock (the Date header), not this machine's.
    assert harvest.mark("upstream") == [listed_head, GH_STAMP]
    assert harvest.mark("lharries") == [harvest.head(harvest.lh), GH_STAMP]

    # Recording consumed the listing: a second --mark has nothing to record.
    again = harvest.run("--mark")
    assert again.returncode != 0
    assert "no complete listing to record" in again.stderr

    # The next harvest starts from the mark: the late commit is new, the rest is not.
    harvest.gh_log.unlink()
    out = harvest.listed()
    header = next(line for line in out.splitlines() if line.startswith("upstream ("))
    assert header == f"upstream (verygoodplugins/whatsapp-mcp) — since {listed_head}, head {late}"
    assert header.count(listed_head) == 1  # the old header printed the mark twice
    assert subjects(out, "upstream", COMPONENT_COMMITS) == ["up: pushed while reviewing"]
    assert section(out, "upstream", OTHER_COMMITS) == []
    # PRs and issues are filtered by the mark's timestamp, whatever their state.
    assert f"--- PRs updated since {GH_STAMP}, any state (2) ---" in out
    assert "#171 2026-09-29 open an old PR that moved after the mark" in out
    assert f"issues?state=all&since={GH_STAMP}&sort=updated" in harvest.gh_calls()


def test_all_ignores_the_marks_and_leaves_the_timestamp_alone(marked: Harvest) -> None:
    out = marked.listed("--all", FAKE_GH_DATE=LATER_DATE)
    assert f"upstream (verygoodplugins/whatsapp-mcp) — vs our main, head {marked.head(marked.up)}" in out
    assert subjects(out, "upstream", COMPONENT_COMMITS) == ["up: bridge change"]  # listed again although marked
    assert "--- PRs open (2) ---" in out
    # --all shows open PRs and issues only, so it cannot vouch for everything
    # that moved since the old timestamp: --mark keeps it.
    marked.record()
    assert marked.mark("upstream") == [marked.head(marked.up), GH_STAMP]


def test_going_over_the_limit_is_said_and_keeps_the_timestamp(marked: Harvest) -> None:
    out = marked.listed(FAKE_GH_FILLER="250", HARVEST_LIMIT="200", FAKE_GH_DATE=LATER_DATE)
    assert f"--- PRs updated since {GH_STAMP}, any state (200) ---" in out
    assert "PR/issue listing of verygoodplugins/whatsapp-mcp is INCOMPLETE (250 PRs + issues, only the 200" in out
    marked.record()
    assert marked.mark("upstream")[1] == GH_STAMP


def test_exactly_the_limit_is_complete(marked: Harvest) -> None:
    out = marked.listed(FAKE_GH_FILLER="200", HARVEST_LIMIT="200", FAKE_GH_DATE=LATER_DATE)
    assert f"--- PRs updated since {GH_STAMP}, any state (200) ---" in out
    assert "INCOMPLETE" not in out
    marked.record()
    assert marked.mark("upstream")[1] == LATER_STAMP


def test_a_failing_gh_is_said_keeps_the_timestamp_and_still_records_the_head(marked: Harvest) -> None:
    late = marked.commit(marked.up, "whatsapp-bridge/late.go", "up: later")
    out = marked.listed(FAKE_GH_MODE="fail", FAKE_GH_DATE=LATER_DATE)
    assert subjects(out, "upstream", COMPONENT_COMMITS) == ["up: later"]
    assert "PR/issue listing of verygoodplugins/whatsapp-mcp is INCOMPLETE (gh api failed)" in out
    marked.record()
    assert marked.mark("upstream") == [late, GH_STAMP]


def test_without_gh_the_commits_are_listed_and_the_timestamp_stays(marked: Harvest) -> None:
    # PATH holds git and nothing else: no gh, and no awk, sed or grep either,
    # which the script must not need.
    assert GIT is not None
    only_git = marked.root / "only-git"
    only_git.mkdir()
    wrapper = only_git / "git"
    wrapper.write_text(f'#!/bin/sh\nexec "{Path(GIT).as_posix()}" "$@"\n', encoding="utf-8", newline="\n")
    wrapper.chmod(wrapper.stat().st_mode | stat.S_IEXEC | stat.S_IXGRP | stat.S_IXOTH)
    path = [str(only_git)]
    if os.name == "nt":
        # Git for Windows cannot run from an empty PATH (its fetch crashes), so
        # there the rest of PATH stays, minus every directory that offers a gh.
        path += [
            d
            for d in os.environ.get("PATH", "").split(os.pathsep)
            if d and not any((Path(d) / gh).exists() for gh in ("gh", "gh.exe", "gh.cmd", "gh.bat"))
        ]
    late = marked.commit(marked.up, "whatsapp-bridge/late.go", "up: later")

    out = marked.listed(PATH=os.pathsep.join(path))
    assert subjects(out, "upstream", COMPONENT_COMMITS) == ["up: later"]
    assert "PR/issue listing of verygoodplugins/whatsapp-mcp is INCOMPLETE (gh is not installed)" in out
    assert marked.gh_calls() == ""
    marked.record()
    assert marked.mark("upstream") == [late, GH_STAMP]


def test_mark_refuses_when_the_mark_file_is_not_the_one_listed_from(marked: Harvest) -> None:
    listed_from = marked.mark("upstream")[0]
    marked.listed()
    marked.marks.write_text(f"upstream {'a' * 40} {GH_STAMP}\n", encoding="utf-8", newline="\n")
    done = marked.run("--mark")
    assert done.returncode != 0
    assert f"differs for upstream from the one the listing started from ({listed_from})" in done.stderr


def test_a_mark_this_clone_does_not_have_stops_the_listing(marked: Harvest) -> None:
    marked.listed()  # leaves a listing --mark could record
    good = marked.marks.read_text(encoding="utf-8")
    marked.marks.write_text(f"upstream {'d' * 40} {GH_STAMP}\n", encoding="utf-8", newline="\n")
    done = marked.run()
    assert done.returncode != 0
    assert f"the mark for upstream ({'d' * 40}) is not an ancestor of upstream/main" in done.stderr
    # A listing that stopped is not something --mark may record, and neither is
    # the older, complete one before it.
    marked.marks.write_text(good, encoding="utf-8", newline="\n")
    after = marked.run("--mark")
    assert after.returncode != 0
    assert "no complete listing to record" in after.stderr


def test_a_force_push_stops_the_listing_even_when_the_old_commit_is_still_here(marked: Harvest) -> None:
    old = marked.mark("upstream")[0]
    # Upstream replaces main with an unrelated history; our clone keeps the old objects.
    marked.git(marked.up, "checkout", "-q", "--orphan", "rewritten")
    marked.commit(marked.up, "whatsapp-bridge/new.go", "up: rewritten history")
    marked.git(marked.up, "branch", "-M", "rewritten", "main")
    done = marked.run()
    assert done.returncode != 0
    assert f"the mark for upstream ({old}) is not an ancestor" in done.stderr
    assert "force-pushed" in done.stderr
    # --all is the way out, as the message says.
    assert subjects(marked.listed("--all"), "upstream", COMPONENT_COMMITS) == ["up: rewritten history"]


def test_component_commits_behind_a_merge_are_not_listed_as_docs(marked: Harvest) -> None:
    # A change and its revert on a side branch: the merge is TREESAME to main for
    # the component paths, which a path-limited `git log` simplifies away.
    marked.git(marked.up, "checkout", "-q", "-b", "side")
    marked.commit(marked.up, "whatsapp-bridge/a.go", "up: bridge change A")
    marked.git(marked.up, "rm", "-q", "whatsapp-bridge/a.go")
    marked.git(marked.up, "commit", "-q", "-m", "up: revert bridge change A")
    marked.git(marked.up, "checkout", "-q", "main")
    marked.commit(marked.up, "docs/notes.md", "up: docs on main")
    marked.git(marked.up, "merge", "-q", "--no-ff", "-m", "merge side", "side")

    out = marked.listed()
    assert set(subjects(out, "upstream", COMPONENT_COMMITS)) == {"up: bridge change A", "up: revert bridge change A"}
    assert subjects(out, "upstream", OTHER_COMMITS) == ["up: docs on main"]


def test_mark_works_from_another_worktree_of_the_clone(harvest: Harvest) -> None:
    harvest.listed()
    worktree = harvest.root / "wt"
    harvest.git(harvest.fork, "worktree", "add", "-q", "-b", "chore/harvest", str(worktree), "main")
    harvest.record(cwd=worktree)
    assert harvest.mark("upstream", cwd=worktree) == [harvest.head(harvest.up), GH_STAMP]
    assert not harvest.marks.exists()  # written where --mark ran, not in the other checkout


@pytest.mark.parametrize(
    ("args", "marks", "env", "message"),
    [
        (("--makr",), None, {}, "unknown argument '--makr'"),
        (("--all", "--mark"), None, {}, "one argument at most"),
        ((), "upstream $(touch pwned) 2026-01-01T00:00:00Z\n", {}, "is not a commit SHA"),
        ((), "upstream --output=pwned 2026-01-01T00:00:00Z\n", {}, "is not a commit SHA"),
        ((), f"upstream {'a' * 40} yesterday&state=closed\n", {}, "is not a UTC timestamp"),
        ((), None, {"HARVEST_LIMIT": "lots"}, "HARVEST_LIMIT must be a positive number"),
    ],
)
def test_bad_input_is_refused(
    harvest: Harvest, args: tuple[str, ...], marks: str | None, env: dict[str, str], message: str
) -> None:
    if marks is not None:
        harvest.marks.write_text(marks, encoding="utf-8", newline="\n")
    done = harvest.run(*args, **env)
    assert done.returncode != 0
    assert message in done.stderr
    assert not (harvest.fork / "pwned").exists()
    # Nothing was listed, so there is nothing to mark.
    assert harvest.run("--mark").returncode != 0


def test_a_crlf_mark_file_is_read_like_an_lf_one(marked: Harvest) -> None:
    marked.marks.write_bytes(marked.marks.read_bytes().replace(b"\n", b"\r\n"))
    out = marked.listed()
    assert f"--- PRs updated since {GH_STAMP}, any state (2) ---" in out
    assert f"since={GH_STAMP}&sort" in marked.gh_calls()
