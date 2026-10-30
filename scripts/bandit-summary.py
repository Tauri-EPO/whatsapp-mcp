"""Publish the real Bandit outcome and fail closed on an incomplete report."""

import json
import os
import sys
from pathlib import Path


def main() -> int:
    scanner_exit = int(sys.argv[1])
    try:
        report = json.loads(Path(sys.argv[2]).read_text(encoding="utf-8"))
        findings = report["results"]
        errors = report["errors"]
        count = len(findings)
        error_count = len(errors)
    except (OSError, ValueError, KeyError, TypeError) as exc:
        count = error_count = "unavailable"
        print(f"Cannot read Bandit report: {exc}", file=sys.stderr)
        findings = []

    summary = (
        "## Bandit: production Python (medium/high)\n\n"
        f"- Scanner exit code: {scanner_exit}\n"
        f"- Remaining findings: {count}\n"
        f"- Scan errors: {error_count}\n"
    )
    for finding in findings:
        summary += (
            f"{finding['filename']}:{finding['line_number']}: "
            f"{finding['test_id']} {finding['issue_severity']}: {finding['issue_text']}\n"
        )
    print(summary)
    if summary_path := os.environ.get("GITHUB_STEP_SUMMARY"):
        with Path(summary_path).open("a", encoding="utf-8") as handle:
            handle.write(summary)
    # A scanner crash, unreadable JSON or a skipped/unparseable source must not
    # look like a successful scan. Preserve its real exit code in the summary.
    return scanner_exit or (2 if error_count else 0) or (1 if count else 0)


if __name__ == "__main__":
    sys.exit(main())
