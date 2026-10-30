"""Publish the real Bandit outcome and fail closed on an incomplete report."""

import json
import os
import re
import sys
from pathlib import Path


def main() -> int:
    scanner_exit = int(sys.argv[1])
    if len(sys.argv) > 3:
        # Bandit visits non-SQL AST nodes on the same suppressed statement.
        # Only this exact redundant B608 warning is hidden; findings come from
        # JSON and every other diagnostic remains visible.
        for line in Path(sys.argv[3]).read_text(encoding="utf-8").splitlines():
            if not re.fullmatch(
                r"\[tester\]\s+WARNING\s+nosec encountered \(B608\), but no failed test on file .+:\d+", line
            ):
                print(line, file=sys.stderr)
    malformed = False
    try:
        report = json.loads(Path(sys.argv[2]).read_text(encoding="utf-8"))
        if not isinstance(report, dict):
            raise ValueError("expected a report object")
        findings = report["results"]
        errors = report["errors"]
        if not isinstance(findings, list) or not isinstance(errors, list):
            raise ValueError("results and errors must be lists")
        count = len(findings)
        error_count = len(errors)
    except (OSError, ValueError, KeyError, TypeError) as exc:
        count = error_count = "unavailable"
        print(f"Cannot read Bandit report: {exc}", file=sys.stderr)
        findings = []
        malformed = True

    summary = (
        "## Bandit: production Python (medium/high)\n\n"
        f"- Scanner exit code: {scanner_exit}\n"
        f"- Remaining findings: {count}\n"
        f"- Scan errors: {error_count}\n"
    )
    for finding in findings:
        expected = {"filename": str, "line_number": int, "test_id": str, "issue_severity": str, "issue_text": str}
        if not isinstance(finding, dict):
            finding = {}
        if any(not isinstance(finding.get(key), kind) for key, kind in expected.items()):
            malformed = True
        summary += (
            f"{finding.get('filename', '<unknown file>')}:{finding.get('line_number', '?')}: "
            f"{finding.get('test_id', '<unknown test>')} {finding.get('issue_severity', '?')}: "
            f"{finding.get('issue_text', '<missing finding text>')}\n"
        )
    if malformed:
        message = "Malformed or incomplete Bandit report; refusing to pass."
        print(message, file=sys.stderr)
        summary += f"\n{message}\n"
    print(summary)
    if summary_path := os.environ.get("GITHUB_STEP_SUMMARY"):
        with Path(summary_path).open("a", encoding="utf-8") as handle:
            handle.write(summary)
    # A scanner crash, unreadable JSON or a skipped/unparseable source must not
    # look like a successful scan. Preserve its real exit code in the summary.
    return 1 if malformed else scanner_exit or (2 if error_count else 0) or (1 if count else 0)


if __name__ == "__main__":
    sys.exit(main())
