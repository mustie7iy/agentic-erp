#!/usr/bin/env bash
# Runs one command in every Go module of the repository: the root's and
# each go.mod git tracks below it (erp-temporal's, temporal/, ADR-0035),
# from that module's directory. `govulncheck ./...` and the other gates
# that load packages stop at a nested go.mod, so run from the root alone
# they never saw temporal/ (06 §6.5, batch 21). The gates whose result can
# change without a change to the code (govulncheck, as advisories and the
# standard library's fixes appear) and the static analysers CI runs on
# the whole tree go through here, in scripts/ci-local.sh, ci.yml and
# weekly.yml alike, so the three cannot drift apart; vet, the lint, the
# tests and coverage of temporal/ are scripts/temporal-checks.sh's.
#
# Every module is checked even after one fails, so one run shows every
# failure; the exit status is the first failure's.
#
#   scripts/each-module.sh govulncheck ./...
set -euo pipefail
[ "$#" -gt 0 ] || {
  echo "usage: each-module.sh COMMAND [ARG...]" >&2
  exit 2
}
cd "$(dirname "$0")/.."
# Go ignores testdata directories, so a go.mod there is a fixture.
# git's own failure (no repository, an ownership refusal) stops the run
# with its message; only an empty result from the filter is let through.
tracked="$(git ls-files -- ':(glob)**/go.mod')"
modules="$(grep -Ev '(^|/)testdata/' <<<"$tracked" | sort)" || true
if ! grep -qx go.mod <<<"$modules"; then
  echo "each-module: git tracks no go.mod at the repository's root" >&2
  exit 2
fi
rc=0
while IFS= read -r mod; do
  dir="$(dirname "$mod")"
  echo "each-module: $dir: $*"
  # A module's failure is kept and the next one still checked.
  (cd "$dir" && "$@") || { s=$?; [ "$rc" != 0 ] || rc="$s"; }
done <<<"$modules"
exit "$rc"
