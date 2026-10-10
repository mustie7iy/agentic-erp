#!/usr/bin/env bash
# Whole-suite statement coverage of production code, with a minimum.
# Excluded from the figure: test-only packages (nfr, testkit, llmtest, and the
# platform's pgtest, authntest, natstest, apitest, fixture) and code generated from the
# API contract (api/gen). Database-backed packages need DATABASE_URL; without
# it their tests skip and the gate fails, so run `make db-up` first.
set -euo pipefail
if [ -z "${DATABASE_URL:-}" ]; then
  echo "coverage: DATABASE_URL is not set (make db-up, then make coverage)" >&2
  exit 1
fi
MIN="${COVERAGE_MIN:-100.0}"
profile="$(mktemp)"
go test -short -count=1 -coverpkg=./... -coverprofile="$profile" ./... >/dev/null
grep -v -E '/internal/(nfr|testkit|llm/llmtest)/|/internal/platform/(pgtest|authn/authntest|natsbus/natstest|httpapi/apitest|fixture)/|/api/gen/' "$profile" > "$profile.prod"
total="$(go tool cover -func="$profile.prod" | awk '/^total:/ {print $NF}' | tr -d '%')"
echo "statement coverage: ${total}% (minimum ${MIN}%)"
if awk -v t="$total" -v m="$MIN" 'BEGIN { exit !(t+0 < m+0) }'; then
  echo "uncovered functions:"
  go tool cover -func="$profile.prod" | grep -v '100.0%' | grep -v '^total:'
  exit 1
fi
