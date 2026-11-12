#!/usr/bin/env bash
# Tests scripts/coverage.sh's gate on profiles made here: the shards'
# profiles count together, the exclusions hold, and a shortfall fails with
# the blocks named, as the whole-suite gate reports it (06 §6.5).
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
# shellcheck source=scripts/lib/on-exit.sh
. "$root/scripts/lib/on-exit.sh"
on_exit 'rm -rf "$work"'
failed=0
m=github.com/mustie7iy/agentic-erp

# expect NAME STATUS PATTERN ARG... runs coverage.sh ARG... and checks its
# exit status and that its output has a line matching PATTERN.
expect() {
  local name="$1" want="$2" pattern="$3" status=0 out
  shift 3
  out="$("$root/scripts/coverage.sh" "$@" 2>&1)" || status=$?
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

# Two shards: each covers a block the other does not.
printf '%s\n' "mode: set" "$m/internal/a/a.go:1.1,2.2 3 1" "$m/internal/b/b.go:1.1,2.2 2 0" >"$work/1"
printf '%s\n' "mode: set" "$m/internal/a/a.go:1.1,2.2 3 0" "$m/internal/b/b.go:1.1,2.2 2 1" >"$work/2"
expect "blocks covered by any shard are covered" 0 '^statement coverage: 5 of 5 statements \(minimum 100.0%\)$' gate "$work/1" "$work/2"
expect "one shard alone falls short" 1 "^  $m/internal/b/b.go:1.1,2.2 \(2 statements\)\$" gate "$work/1"
expect "and says how far, under the heading the whole-suite gate prints" 1 '^statement coverage: 3 of 5 statements \(minimum 100.0%\)$' gate "$work/1"
expect "with the list of blocks" 1 '^uncovered blocks:$' gate "$work/1"
# A raw go test profile repeats a block once per test binary.
printf '%s\n' "mode: set" "$m/internal/a/a.go:1.1,2.2 3 0" "$m/internal/a/a.go:1.1,2.2 3 4" "$m/internal/a/a.go:1.1,2.2 3 0" >"$work/raw"
expect "a block repeated counts once, covered if any line hit it" 0 '^statement coverage: 3 of 3 statements' gate "$work/raw"
# Test-only packages and generated code are outside the figure.
printf '%s\n' "mode: set" "$m/internal/a/a.go:1.1,2.2 3 1" "$m/api/gen/x.go:1.1,2.2 900 0" "$m/internal/platform/pgtest/x.go:1.1,2.2 7 0" "$m/internal/nfr/x.go:1.1,2.2 5 0" >"$work/excluded"
expect "excluded packages are not counted, even from a profile that carries them" 0 '^statement coverage: 3 of 3 statements' gate "$work/excluded"
printf '%s\n' "mode: set" "$m/api/gen/x.go:1.1,2.2 900 1" >"$work/none"
expect "a profile without production statements fails" 1 'has no production statements' gate "$work/none"
expect "a missing shard profile fails" 1 "no profile at $work/3" gate "$work/1" "$work/2" "$work/3"
: >"$work/empty"
expect "an empty shard profile fails" 1 "no profile at $work/empty" gate "$work/1" "$work/empty"
expect "the gate needs a profile" 2 'usage: coverage.sh gate PROFILE' gate
expect "a shard run needs its output and packages" 2 'usage: coverage.sh run OUT PKG' run "$work/out"
expect "an unknown mode is refused" 2 'usage: coverage.sh' merge
if out="$(COVERAGE_MIN=50 "$root/scripts/coverage.sh" gate "$work/1")" && [ "$out" = "statement coverage: 3 of 5 statements (minimum 50%)" ]; then echo "ok   a lower minimum passes a shortfall"; else echo "FAIL a lower minimum passes a shortfall: got $out" >&2; failed=1; fi
finished
exit "$failed"
