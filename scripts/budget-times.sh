#!/usr/bin/env bash
# budget-times.sh FILE: the whole wall time of every budget test in a
# `go test -json` stream (scripts/budgets.sh writes one), slowest first,
# and a failure when one took longer than BUDGET_TEST_LIMIT seconds.
#
# A budget test's own budget covers the calls it measures, not the test:
# on 2026-11-11 TestConsolidatedStatementBudget spent about 290 s seeding
# its data inside an 8-minute context on the runner, passed, and no gate
# looked at the time until the context ran out (main's run 43007697247,
# fixed forward in pull request 113). The time counted here is the one go
# test reports for the top-level test, seeding included.
#
# The limit is 240 s: half of that 8-minute context, the tightest any
# budget test gives its run, so a test is stopped here while it still has
# as long again before it fails on its own deadline; and half as much
# again as the slowest test today (TestLiquidityForecastBudget, 157 s on a
# GitHub runner at scale 2, less on this machine). It fails rather than
# warns: a warning in a log nobody reads is what the incident had. A test
# above three quarters of the limit is named, without failing.
set -euo pipefail
file="${1:?usage: budget-times.sh <go test -json output>}"
limit="${BUDGET_TEST_LIMIT:-240}"
[ -s "$file" ] || { echo "budget-times: no go test -json output in $file" >&2; exit 1; }
# One line per finished top-level test: seconds, package, test. Lines that
# are not JSON (a build error go test prints as text) are passed over.
times="$(jq -rR 'fromjson? | select(.Test != null and (.Test | contains("/") | not) and (.Action == "pass" or .Action == "fail"))
  | "\(.Elapsed) \(.Package) \(.Test)"' "$file" | sort -k1,1nr -k2,3)"
[ -n "$times" ] || { echo "budget-times: $file names no finished test" >&2; exit 1; }
echo "budget tests, whole time with seeding (limit ${limit} s):"
awk -v limit="$limit" '
  { printf "  %8.1f s  %s %s%s\n", $1, $2, $3, ($1 > limit ? "  OVER THE LIMIT" : ($1 > limit * 0.75 ? "  near the limit" : "")) }
  $1 > limit { over++ }
  END {
    if (over) { printf "budget-times: %d budget test%s took longer than %d s in all: shorten the seeding, or split the test\n", over, (over > 1 ? "s" : ""), limit; exit 1 }
  }' <<<"$times"
