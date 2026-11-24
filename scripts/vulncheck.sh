#!/usr/bin/env bash
# The govulncheck gate of one Go module, run from that module's directory
# by scripts/each-module.sh in scripts/ci-local.sh, ci.yml and weekly.yml
# alike: it runs the govulncheck command it is given with `-format json
# ./...` and hands the result to cmd/vulngate, which fails on every called
# vulnerability that .govulncheck.yaml does not accept for this module
# until a date not yet past, and on an expired or unused exception
# (ADR-0032 As built). GOVULNCHECK_EXCEPTIONS names another exceptions
# file, for scripts/test-each-module.sh alone: no workflow sets it.
#
#   scripts/each-module.sh ./scripts/vulncheck.sh govulncheck
#   scripts/each-module.sh ./scripts/vulncheck.sh go run golang.org/x/vuln/cmd/govulncheck@v1.9.0
set -euo pipefail
[ "$#" -gt 0 ] || {
  echo "usage: vulncheck.sh GOVULNCHECK-COMMAND [ARG...]" >&2
  exit 2
}
root="$(cd "$(dirname "$0")/.." && pwd -P)"
# shellcheck source=scripts/lib/on-exit.sh
. "$root/scripts/lib/on-exit.sh"
here="$(pwd -P)"
case "$here/" in
"$root"/*) ;;
*)
  echo "vulncheck: $here is not inside the repository at $root" >&2
  exit 2
  ;;
esac
module="${here#"$root"}"
module="${module#/}"
out="$(mktemp)"
on_exit 'rm -f "$out"'
# In JSON mode govulncheck exits 0 whatever it finds; a failure is its own
# (no network, a package that does not load) and stops the gate here.
"$@" -format json ./... >"$out"
go -C "$root" run ./cmd/vulngate -root "$root" -module "${module:-.}" \
  "${GOVULNCHECK_EXCEPTIONS:-$root/.govulncheck.yaml}" <"$out"
finished
