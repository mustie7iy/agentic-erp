#!/usr/bin/env bash
# Stop hook: before Claude reports work as done, the changed Go packages must
# build, lint clean and pass their short tests (CLAUDE.md, definition of done).
# Exit 2 blocks the stop and hands the failures back to Claude.
set -uo pipefail
input=$(cat)
# A second stop in the same turn after this hook already blocked: let it through
# so a failure Claude cannot fix is reported to the user, not looped on.
[ "$(echo "$input" | jq -r '.stop_hook_active // false')" = "true" ] && exit 0
cd "${CLAUDE_PROJECT_DIR:-.}" || exit 0

changed=$( (git diff --name-only HEAD; git ls-files --others --exclude-standard) 2>/dev/null \
  | grep '\.go$' | grep -v '^api/gen/' | sort -u)
[ -z "$changed" ] && exit 0
pkgs=$(echo "$changed" | xargs -n1 dirname | sort -u | sed 's#^#./#')

fail=""
if ! out=$(go build ./... 2>&1); then fail+=$'\n== go build\n'"$out"; fi
if [ -z "$fail" ]; then
  if ! out=$(go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.5.0 run \
      --new-from-merge-base=main --show-stats=false $pkgs 2>&1); then
    fail+=$'\n== make lint\n'"$(echo "$out" | head -60)"
  fi
  export DATABASE_URL="${DATABASE_URL:-postgres://postgres:dev@localhost:55432/postgres?sslmode=disable}"
  if ! out=$(go test -short -count=1 $pkgs 2>&1); then
    fail+=$'\n== go test -short\n'"$(echo "$out" | grep -vE '^(ok|\?) ' | tail -60)"
  fi
fi
if [ -n "$fail" ]; then
  echo "Definition of done not met for changed packages ($(echo $pkgs)). Fix these before finishing, or tell the user why you cannot:$fail" >&2
  exit 2
fi
exit 0
