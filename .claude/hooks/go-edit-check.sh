#!/usr/bin/env bash
# PostToolUse hook (Edit|Write|MultiEdit): formats an edited Go file and lints its
# package against .golangci.yml, reporting only issues in changed code. Exit 2
# hands the findings back to Claude to fix.
set -uo pipefail
file=$(jq -r '.tool_input.file_path // empty')
case "$file" in
  *.go) ;;
  *) exit 0 ;;
esac
[ -f "$file" ] || exit 0
cd "${CLAUDE_PROJECT_DIR:-.}" || exit 0
case "$file" in */api/gen/*) exit 0 ;; esac

gofmt -w "$file"
pkg="$(dirname "$file")"
out=$(go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.5.0 run \
  --new-from-merge-base=main --show-stats=false "$pkg" 2>&1)
if [ $? -ne 0 ] && [ -n "$out" ]; then
  echo "golangci-lint on $pkg (CLAUDE.md layer 2). Fix the cause; do not add nolint without a reason:" >&2
  echo "$out" | head -60 >&2
  exit 2
fi
exit 0
