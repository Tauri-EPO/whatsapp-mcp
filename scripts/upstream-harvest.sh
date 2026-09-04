#!/usr/bin/env bash
# Upstream harvest: list what the original projects did since our last pass.
#
# This fork does not merge upstream (see AGENTS.md §2). It harvests ideas:
# read the commits and PRs below, reimplement what is worth having, credit
# the source in the commit body, then record the new SHAs with --mark.
#
#   scripts/upstream-harvest.sh            # show commits + open PRs/issues since last mark
#   scripts/upstream-harvest.sh --mark     # record current upstream heads as harvested
#   scripts/upstream-harvest.sh --all      # ignore the marks, show everything vs main
#
# Requires: git remotes `upstream` (verygoodplugins) and `lharries`; `gh` for
# the PR/issue listing (skipped when gh is missing or offline).
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
MARK_FILE=".upstream-harvest"
declare -A REMOTES=(
  [upstream]="https://github.com/verygoodplugins/whatsapp-mcp.git"
  [lharries]="https://github.com/lharries/whatsapp-mcp.git"
)
declare -A REPOS=(
  [upstream]="verygoodplugins/whatsapp-mcp"
  [lharries]="lharries/whatsapp-mcp"
)

mode="${1:-}"

for name in "${!REMOTES[@]}"; do
  git remote get-url "$name" >/dev/null 2>&1 || git remote add "$name" "${REMOTES[$name]}"
  git fetch -q "$name"
done

mark_of() { grep "^$1 " "$MARK_FILE" 2>/dev/null | awk '{print $2}' || true; }

if [[ "$mode" == "--mark" ]]; then
  : > "$MARK_FILE"
  for name in upstream lharries; do
    echo "$name $(git rev-parse "$name/main") $(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$MARK_FILE"
  done
  echo "Recorded in $MARK_FILE:"; cat "$MARK_FILE"
  exit 0
fi

for name in upstream lharries; do
  since="$(mark_of "$name")"
  [[ "$mode" == "--all" ]] && since=""
  range="${since:+$since..}$name/main"
  echo "=================================================================="
  echo "$name (${REPOS[$name]}) — commits ${since:+since $since}${since:-vs our main} touching the bridge/server"
  echo "=================================================================="
  if [[ -n "$since" ]]; then
    git log --no-merges --date=short --format='%h %ad %s' "$range" -- whatsapp-bridge whatsapp-mcp-server || true
  else
    git log --no-merges --date=short --format='%h %ad %s' "main..$name/main" -- whatsapp-bridge whatsapp-mcp-server || true
  fi
  echo
  if command -v gh >/dev/null 2>&1; then
    echo "--- open PRs ---"
    gh pr list --repo "${REPOS[$name]}" --state open --limit 30 --json number,title,updatedAt \
      -q '.[] | "#\(.number) \(.updatedAt[:10]) \(.title)"' 2>/dev/null || echo "(gh unavailable)"
    echo "--- open issues (newest 20) ---"
    gh issue list --repo "${REPOS[$name]}" --state open --limit 20 --json number,title,updatedAt \
      -q '.[] | "#\(.number) \(.updatedAt[:10]) \(.title)"' 2>/dev/null || echo "(gh unavailable)"
  fi
  echo
done

echo "When done reviewing: scripts/upstream-harvest.sh --mark"
