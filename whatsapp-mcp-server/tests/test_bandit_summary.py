"""The blocking scanner gate must report failures, including incomplete scans."""

import json
import os
import subprocess
import sys
from pathlib import Path

import pytest

SCRIPT = Path(__file__).resolve().parents[2] / "scripts" / "bandit-summary.py"


def test_only_redundant_ast_diagnostics_are_filtered(tmp_path):
    report = tmp_path / "bandit.json"
    report.write_text('{"results": [], "errors": []}', encoding="utf-8")
    log = tmp_path / "bandit.log"
    log.write_text(
        "[tester]\tWARNING\tnosec encountered (B608), but no failed test on file example.py:2\n"
        "[tester]\tWARNING\tnosec encountered (B104), but no failed test on file example.py:3\n"
        "[tester]\tWARNING\tnosec encountered (B102), but no failed test on file example.py:4\n"
        "[tester]\tWARNING\tnosec encountered (B103), but no failed test on file example.py:5\n"
        "[manager]\tWARNING\tTest in comment: reason is not a test name or id, ignoring\n"
        "[tester]\tERROR\tunexpected scanner error\n",
        encoding="utf-8",
    )
    result = subprocess.run(
        [sys.executable, str(SCRIPT), "0", str(report), str(log)],
        capture_output=True,
        text=True,
        check=False,
    )
    assert result.returncode == 0
    assert "B608" not in result.stderr
    assert "B104" not in result.stderr and "B102" not in result.stderr
    assert "B103" in result.stderr
    assert "Test in comment" in result.stderr
    assert "unexpected scanner error" in result.stderr


@pytest.mark.parametrize(
    ("scanner_exit", "findings", "errors", "expected"),
    [(0, [], [], 0), (1, [{}], [], 1), (0, [{}], [], 1), (2, [], [], 2), (0, [], [{}], 2)],
)
def test_gate_reports_counts_and_preserves_failure(tmp_path, scanner_exit, findings, errors, expected):
    findings = [
        {
            "filename": "example.py",
            "line_number": 1,
            "test_id": "B104",
            "issue_severity": "MEDIUM",
            "issue_text": "Test listener",
        }
        for _ in findings
    ]
    report = tmp_path / "bandit.json"
    report.write_text(json.dumps({"results": findings, "errors": errors}), encoding="utf-8")
    summary = tmp_path / "summary.md"
    summary.write_text("Existing summary\n", encoding="utf-8")
    result = subprocess.run(
        [sys.executable, str(SCRIPT), str(scanner_exit), str(report)],
        env={**os.environ, "GITHUB_STEP_SUMMARY": str(summary)},
        capture_output=True,
        text=True,
        check=False,
    )
    assert result.returncode == expected, result.stderr
    body = summary.read_text(encoding="utf-8")
    assert body.startswith("Existing summary\n")
    assert f"Scanner exit code: {scanner_exit}" in body
    assert f"Remaining findings: {len(findings)}" in body
    assert f"Scan errors: {len(errors)}" in body
    if findings:
        assert "example.py:1: B104 MEDIUM: Test listener" in result.stdout
        assert "example.py:1: B104 MEDIUM: Test listener" in body


def test_local_run_without_actions_environment(tmp_path):
    report = tmp_path / "bandit.json"
    report.write_text('{"results": [], "errors": []}', encoding="utf-8")
    env = dict(os.environ)
    env.pop("GITHUB_STEP_SUMMARY", None)
    result = subprocess.run(
        [sys.executable, str(SCRIPT), "0", str(report)], env=env, capture_output=True, text=True, check=False
    )
    assert result.returncode == 0, result.stderr
    assert "Remaining findings: 0" in result.stdout


@pytest.mark.parametrize(
    "report", [{"results": [{}], "errors": []}, {"results": [None], "errors": []}, {"results": {}, "errors": []}, []]
)
def test_malformed_findings_fail_closed_without_traceback(tmp_path, report):
    source = tmp_path / "bandit.json"
    source.write_text(json.dumps(report), encoding="utf-8")
    summary = tmp_path / "summary.md"
    result = subprocess.run(
        [sys.executable, str(SCRIPT), "0", str(source)],
        env={**os.environ, "GITHUB_STEP_SUMMARY": str(summary)},
        capture_output=True,
        text=True,
        check=False,
    )
    assert result.returncode == 1
    assert "Malformed or incomplete Bandit report" in result.stderr
    assert "Traceback" not in result.stderr
    assert "refusing to pass" in summary.read_text(encoding="utf-8")


@pytest.mark.parametrize("contents", [None, "not JSON", "{}"])
def test_missing_or_invalid_report_cannot_pass(tmp_path, contents):
    report = tmp_path / "bandit.json"
    if contents is not None:
        report.write_text(contents, encoding="utf-8")
    summary = tmp_path / "summary.md"
    result = subprocess.run(
        [sys.executable, str(SCRIPT), "0", str(report)],
        env={**os.environ, "GITHUB_STEP_SUMMARY": str(summary)},
        capture_output=True,
        text=True,
        check=False,
    )
    assert result.returncode != 0
    body = summary.read_text(encoding="utf-8")
    assert "Scanner exit code: 0" in body
    assert "Remaining findings: unavailable" in body
    assert "Scan errors: unavailable" in body
