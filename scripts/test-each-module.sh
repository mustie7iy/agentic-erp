#!/usr/bin/env bash
# Tests scripts/each-module.sh on a throwaway repository, and that the
# gates which must see every Go module (govulncheck, staticcheck, gosec)
# reach them only through it in scripts/ci-local.sh, ci.yml and weekly.yml:
# run as `<tool> ./...` from the root they skip temporal/ (06 §6.5).
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
# shellcheck source=scripts/lib/on-exit.sh
. "$root/scripts/lib/on-exit.sh"
on_exit 'rm -rf "$work"'
failed=0

check() { # check NAME GOT WANT
  if [ "$2" != "$3" ]; then
    echo "FAIL $1: got $(tr '\n' ' ' <<<"$2"), want $(tr '\n' ' ' <<<"$3")" >&2
    failed=1
  else
    echo "ok   $1"
  fi
}

repo="$work/repo"
mkdir -p "$repo/scripts" "$repo/temporal" "$repo/internal/x/testdata/m" "$repo/untracked"
cp "$root/scripts/each-module.sh" "$repo/scripts/"
for f in go.mod temporal/go.mod internal/x/testdata/m/go.mod untracked/go.mod; do echo "module m" >"$repo/$f"; done
git init -q -b main "$repo"
git -C "$repo" add scripts go.mod temporal/go.mod internal/x/testdata/m/go.mod
log="$work/log"
# record notes the directory it ran in, relative to the repository, and
# fails in the module named by FAIL_IN.
cat >"$work/record" <<EOF
#!/usr/bin/env bash
dir="\$(pwd -P)"; dir="\${dir#$(cd "$repo" && pwd -P)}"; dir="\${dir#/}"
echo "\${dir:-.} \$*" >>"$log"
[ "\${dir:-.}" != "\${FAIL_IN:-}" ] || exit 3
EOF
chmod +x "$work/record"

: >"$log"
(cd "$work" && "$repo/scripts/each-module.sh" "$work/record" ./...) >/dev/null
check "runs in the root and every tracked module, from anywhere, testdata and untracked go.mod left out" \
  "$(cat "$log")" "$(printf '. ./...\ntemporal ./...')"

: >"$log"
rc=0
FAIL_IN=. "$repo/scripts/each-module.sh" "$work/record" ./... >/dev/null || rc=$?
check "a failure in the root still checks temporal/" "$(cut -d' ' -f1 "$log" | paste -sd' ' -)" ". temporal"
check "a failure is the run's exit status" "$rc" 3

rc=0
FAIL_IN=temporal "$repo/scripts/each-module.sh" "$work/record" ./... >/dev/null || rc=$?
check "a failure in temporal/ alone fails the run" "$rc" 3

rc=0
"$repo/scripts/each-module.sh" >/dev/null 2>&1 || rc=$?
check "no command is refused" "$rc" 2

git -C "$repo" rm -q --cached go.mod
rc=0
"$repo/scripts/each-module.sh" "$work/record" >/dev/null 2>&1 || rc=$?
check "a tree without the root's go.mod is refused rather than half checked" "$rc" 2

# No gate that must see every module runs from the root alone. A line that
# runs one of them, with any arguments, has each-module.sh as its command.
for f in scripts/ci-local.sh .github/workflows/ci.yml .github/workflows/weekly.yml; do
  bare="$(grep -nE '(^|[ /])(govulncheck|staticcheck|gosec)(@[^ ]*)? +[-.]' "$root/$f" |
    grep -vE '^[0-9]+: *(step [a-z-]+ |run: )\./scripts/each-module\.sh ' || true)"
  check "$f runs govulncheck, staticcheck and gosec through each-module.sh" "$bare" ""
done
for f in scripts/ci-local.sh .github/workflows/ci.yml .github/workflows/weekly.yml; do
  n="$(grep -c 'each-module\.sh .*govulncheck' "$root/$f" || true)"
  check "$f runs govulncheck in every module" "$n" 1
done
finished
exit "$failed"
