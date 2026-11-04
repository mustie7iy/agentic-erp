#!/usr/bin/env bash
# Tests scripts/lib/on-exit.sh, and that every script under scripts/ that
# cleans up on exit does it through on_exit and says when it finished: a
# bare `trap … EXIT` exits 0 after an unbound variable under macOS's bash
# 3.2, which make walkthrough and make ci-local run on (review:T61j).
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
# shellcheck source=scripts/lib/on-exit.sh
. "$root/scripts/lib/on-exit.sh"
on_exit 'rm -rf "$work"'
failed=0

check() { # check NAME GOT WANT
  if [ "$2" != "$3" ]; then
    echo "FAIL $1: got $2, want $3" >&2
    failed=1
  else
    echo "ok   $1"
  fi
}

# case_ NAME WANT REASON BODY runs BODY in a script that cleans up through
# on_exit and checks its exit status, that the cleanup ran and whether it
# printed the reason of a stop (yes or no; - for either: bash 5 keeps the
# status of an expansion error itself, so on_exit adds no reason).
case_() {
  local status=0
  rm -f "$work/cleaned"
  printf 'set -euo pipefail\n. %q\non_exit %q\n%s\n' "$root/scripts/lib/on-exit.sh" ": >$work/cleaned" "$4" >"$work/script.sh"
  bash "$work/script.sh" 2>"$work/err" || status=$?
  check "$1: exit status" "$status" "$2"
  check "$1: cleaned up" "$([ -e "$work/cleaned" ] && echo yes || echo no)" yes
  if [ "$3" != - ]; then
    check "$1: the reason of a stop printed" "$(grep -q 'stopped before its end' "$work/err" && echo yes || echo no)" "$3"
  fi
}

case_ "an unbound variable" 1 - 'echo "$NOT_SET"; finished'
case_ "an unbound variable in a function" 1 - 'f() { echo "$NOT_SET"; }; f; finished'
case_ "a missing argument" 1 - 'x=${1:?an argument}; finished'
case_ "a failed command" 1 no 'false; finished'
case_ "an exit status kept" 3 no 'exit 3'
case_ "an end without finished" 1 yes ':'
case_ "an exit 0 before finished" 1 yes 'exit 0'
case_ "finished" 0 no 'finished'
case_ "finished, then exit 0" 0 no 'finished; exit 0'

# Every script under scripts/ that cleans up on exit goes through on_exit
# and calls finished.
bare="$(cd "$root" && grep -rlE '^[^#]*\btrap\b.*[[:space:]](EXIT|0)([[:space:]]|$)' scripts --include='*.sh' | grep -v '^scripts/lib/on-exit.sh$' || true)"
check "scripts that set an EXIT trap themselves" "${bare:-none}" none
unfinished=''
for f in $(cd "$root" && grep -rlE '^[[:space:]]*on_exit ' scripts --include='*.sh' | grep -v '^scripts/lib/on-exit.sh$'); do
  grep -qE '^[[:space:]]*finished$' "$root/$f" || unfinished="$unfinished $f"
done
check "scripts with on_exit that never call finished" "${unfinished:-none}" none

finished
exit "$failed"
