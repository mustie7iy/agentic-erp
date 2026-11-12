#!/usr/bin/env bash
# Tests scripts/budget-times.sh on go test -json streams made here: which
# times it reads, and when it fails (06 §6.5, ci:batch64).
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
# shellcheck source=scripts/lib/on-exit.sh
. "$root/scripts/lib/on-exit.sh"
on_exit 'rm -rf "$work"'
failed=0

# event ACTION PACKAGE TEST ELAPSED prints one go test -json line.
event() {
  printf '{"Time":"2026-11-11T10:00:00Z","Action":"%s","Package":"example.com/erp/%s","Test":"%s","Elapsed":%s}\n' "$@"
}

# expect NAME STATUS PATTERN [ENV...] runs the script on $work/in and checks
# its exit status and that its output has a line matching PATTERN.
expect() {
  local name="$1" want="$2" pattern="$3" status=0 out
  shift 3
  out="$(env "$@" "$root/scripts/budget-times.sh" "$work/in" 2>&1)" || status=$?
  if [ "$status" != "$want" ]; then
    echo "FAIL $name: got status $status, want $want: $out" >&2
    failed=1
  elif ! grep -Eq -- "$pattern" <<<"$out"; then
    echo "FAIL $name: got $out, want a line matching $pattern" >&2
    failed=1
  else
    echo "ok   $name"
  fi
}

{
  echo '{"Action":"start","Package":"example.com/erp/workflows"}'
  event run workflows TestConsolidatedStatementBudget 0
  event pass workflows TestConsolidatedStatementBudget/seeded 300
  event pass workflows TestConsolidatedStatementBudget 44.9
  event pass ledger TestTrialBalanceBudget 14.8
  echo '{"Action":"pass","Package":"example.com/erp/workflows","Elapsed":60}'
} >"$work/in"
expect "tests under the limit pass" 0 '44\.9 s  example.com/erp/workflows TestConsolidatedStatementBudget$'
expect "a subtest's time is not a test's" 0 '^budget tests, whole time with seeding \(limit 240 s\):$'
first="$("$root/scripts/budget-times.sh" "$work/in" | sed -n 2p)"
case "$first" in
*TestConsolidatedStatementBudget) echo "ok   the slowest test is listed first" ;;
*) echo "FAIL the slowest test is listed first: got $first" >&2; failed=1 ;;
esac

# The incident: 290 s of seeding inside a passing test.
event pass workflows TestConsolidatedStatementBudget 334.2 >"$work/in"
event pass ledger TestTrialBalanceBudget 14.8 >>"$work/in"
expect "a passing test over the limit fails the check" 1 'TestConsolidatedStatementBudget  OVER THE LIMIT$'
expect "the failure says how many and the limit" 1 '^budget-times: 1 budget test took longer than 240 s in all'
expect "the limit can be raised" 0 'TestConsolidatedStatementBudget$' BUDGET_TEST_LIMIT=500
expect "a failed test's time counts too" 1 'OVER THE LIMIT' BUDGET_TEST_LIMIT=10

event pass cash TestLiquidityForecastBudget 200 >"$work/in"
expect "a test above three quarters of the limit is named" 0 'TestLiquidityForecastBudget  near the limit$'
event pass cash TestLiquidityForecastBudget 240 >"$work/in"
expect "exactly the limit passes" 0 'near the limit$'

# go test prints a build error as text between the JSON lines.
{ echo '# example.com/erp/cash [build failed]'; event pass ledger TestTrialBalanceBudget 3; } >"$work/in"
expect "a line that is not JSON is passed over" 0 'TestTrialBalanceBudget$'

echo '{"Action":"skip","Package":"example.com/erp/cash","Test":"TestLiquidityForecastBudget","Elapsed":0}' >"$work/in"
expect "a stream without a finished test fails: the budgets did not run" 1 'names no finished test'
: >"$work/in"
expect "an empty stream fails" 1 'no go test -json output'
finished
exit "$failed"
