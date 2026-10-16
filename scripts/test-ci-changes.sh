#!/usr/bin/env bash
# Tests scripts/ci-changes.sh on a throwaway repository with a stand-in for
# gh: when CI's suite runs and when it is skipped. What only GitHub can show
# (the artifact really saved and found, the skipped job counting as passed)
# is left to the first runs there.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
failed=0

# A stand-in for gh: answers from FAKE_PR_HEAD (the merged pull request's
# head), FAKE_RUN (its green run) and FAKE_TREE (the tree that run saved).
mkdir -p "$work/bin"
cat >"$work/bin/gh" <<'EOF'
#!/usr/bin/env bash
case "$1 $2" in
"api repos/"*) echo "${FAKE_PR_HEAD:-}" ;;
"run list") echo "${FAKE_RUN:-}" ;;
"run download")
  [ -n "${FAKE_TREE:-}" ] || exit 1
  while [ $# -gt 0 ]; do [ "$1" = --dir ] && dir="$2"; shift; done
  echo "$FAKE_TREE" >"$dir/tree"
  ;;
*) exit 2 ;;
esac
EOF
chmod +x "$work/bin/gh"

repo="$work/repo"
git init -q -b main "$repo"
git -C "$repo" config user.email test@example.com
git -C "$repo" config user.name test
change() { # change adds a commit touching the files $@ and prints it
  local f
  for f in "$@"; do
    mkdir -p "$(dirname "$repo/$f")"
    echo "$RANDOM" >>"$repo/$f"
    git -C "$repo" add "$f"
  done
  git -C "$repo" commit -q -m "change $*"
  git -C "$repo" rev-parse HEAD
}
base="$(change main.go)"

# expect runs ci-changes.sh with the environment given and checks its run=.
expect() {
  local name="$1" want="$2" got
  shift 2
  got="$(cd "$repo" && env PATH="$work/bin:$PATH" GH=gh GITHUB_REPOSITORY=o/r "$@" "$root/scripts/ci-changes.sh")"
  if [ "$(sed -n 's/^run=//p' <<<"$got")" != "$want" ]; then
    echo "FAIL $name: got $(tr '\n' ' ' <<<"$got"), want run=$want" >&2
    failed=1
  else
    echo "ok   $name: $(sed -n 's/^reason=//p' <<<"$got")"
  fi
}

docs="$(change docs/design/x.md README.md)"
expect "a pull request changing only documentation" false EVENT=pull_request BASE="$base" HEAD="$docs"
code="$(change internal/x.go)"
expect "a pull request changing code" true EVENT=pull_request BASE="$base" HEAD="$code"
tracker="$(change docs/tracker.json)"
expect "the tracker is documentation" false EVENT=pull_request BASE="$code" HEAD="$tracker"
expect "a push without a base (a new branch)" true EVENT=push BASE=0000000000000000000000000000000000000000 HEAD="$code"
expect "a push of documentation" false EVENT=push BASE="$code" HEAD="$tracker"

merged="$(change cmd/y.go)"
tree="$(git -C "$repo" rev-parse "${merged}^{tree}")"
expect "a push whose tree the pull request's green run tested" false \
  EVENT=push BASE="$tracker" HEAD="$merged" FAKE_PR_HEAD=abc FAKE_RUN=42 FAKE_TREE="$tree"
expect "a push whose tree differs (main moved since that run)" true \
  EVENT=push BASE="$tracker" HEAD="$merged" FAKE_PR_HEAD=abc FAKE_RUN=42 FAKE_TREE=0123456789abcdef
expect "a push with no merged pull request" true EVENT=push BASE="$tracker" HEAD="$merged"
expect "a push whose pull request has no green run" true EVENT=push BASE="$tracker" HEAD="$merged" FAKE_PR_HEAD=abc
expect "a push whose run saved no tree" true EVENT=push BASE="$tracker" HEAD="$merged" FAKE_PR_HEAD=abc FAKE_RUN=42
ci="$(change .github/workflows/ci.yml)"
citree="$(git -C "$repo" rev-parse "${ci}^{tree}")"
expect "a push changing CI itself, though its tree was tested" true \
  EVENT=push BASE="$merged" HEAD="$ci" FAKE_PR_HEAD=abc FAKE_RUN=42 FAKE_TREE="$citree"
expect "a pull request is never skipped for a tested tree" true \
  EVENT=pull_request BASE="$tracker" HEAD="$merged" FAKE_PR_HEAD=abc FAKE_RUN=42 FAKE_TREE="$tree"
exit "$failed"
