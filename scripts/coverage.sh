#!/usr/bin/env bash
# Whole-suite statement coverage of production code, with a minimum.
# Test-only packages (nfr, testkit, llmtest) are excluded from the figure.
set -euo pipefail
MIN="${COVERAGE_MIN:-100.0}"
profile="$(mktemp)"
go test -short -count=1 -coverpkg=./... -coverprofile="$profile" ./... >/dev/null
grep -v -E '/internal/(nfr|testkit|llm/llmtest)/' "$profile" > "$profile.prod"
total="$(go tool cover -func="$profile.prod" | awk '/^total:/ {print $NF}' | tr -d '%')"
echo "statement coverage: ${total}% (minimum ${MIN}%)"
if awk -v t="$total" -v m="$MIN" 'BEGIN { exit !(t+0 < m+0) }'; then
  echo "uncovered functions:"
  go tool cover -func="$profile.prod" | grep -v '100.0%' | grep -v '^total:'
  exit 1
fi
