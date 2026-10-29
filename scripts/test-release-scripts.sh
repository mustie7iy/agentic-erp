#!/usr/bin/env bash
# Tests next-version.sh and the changelog (cliff.toml) on throwaway git
# repositories (ADR-0032): each case commits on top of a tag and checks the
# version the rules give; the changelog case checks the grouping by kind and
# tracker item and what is left out. GIT_CLIFF names the git-cliff binary;
# without one the changelog case is skipped (CI installs it).
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
next="$root/scripts/next-version.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
failed=0

# repo makes an empty repository in $work/$1 tagged $2 (none when empty).
repo() {
  local dir="$work/$1"
  mkdir -p "$dir"
  git -C "$dir" init -q -b main
  git -C "$dir" config user.email test@example.com
  git -C "$dir" config user.name test
  git -C "$dir" commit -q --allow-empty -m "Plan: start (T1)"
  [ -z "$2" ] || git -C "$dir" tag "$2"
  echo "$dir"
}

# commit adds a commit with subject $2 to repository $1, touching the
# file $3 when given.
commit() {
  if [ -n "${3:-}" ]; then
    mkdir -p "$(dirname "$1/$3")"
    echo "$2" >>"$1/$3"
    git -C "$1" add "$3"
    git -C "$1" commit -q -m "$2"
  else
    git -C "$1" commit -q --allow-empty -m "$2"
  fi
}

# expect checks that next-version.sh in repository $2 prints $3 (case $1).
expect() {
  local got
  got="$(cd "$2" && "$next" 2>&1)" || got="error: $got"
  if [ "$got" != "$3" ]; then
    echo "FAIL $1: got $got, want $3" >&2
    failed=1
  else
    echo "ok   $1: $got"
  fi
}

r="$(repo first "")"
expect "first release" "$r" v0.1.0

r="$(repo fix v0.1.0)"
commit "$r" "Fix: refuse a bad thing (T2)"
commit "$r" "Refactor: one place for it (T2)"
commit "$r" "Tests: cover it (T2)"
commit "$r" "Release: changelog for v0.1.0"
expect "fixes, refactors, tests and a changelog are a patch" "$r" v0.1.1

r="$(repo feature v0.1.3)"
commit "$r" "Fix: refuse a bad thing (T2)"
commit "$r" "A closed period locks for good (T3)"
expect "a feature is a minor" "$r" v0.2.0

r="$(repo migration v0.2.0)"
commit "$r" "Fix: the index (T4)" db/migrations/000099_x.up.sql
expect "a migration is a minor, even in a fix" "$r" v0.3.0

r="$(repo contract v0.2.0)"
commit "$r" "Refactor: a description (T4)" api/openapi.yaml
expect "a contract change is a minor" "$r" v0.3.0

r="$(repo break0 v0.4.2)"
commit "$r" "Fix: the shape (T5)" api/breaking-accepted.txt
expect "an accepted break before 1.0 is a minor" "$r" v0.5.0

r="$(repo break1 v1.4.2)"
commit "$r" "Fix: the shape (T5)" api/breaking-accepted.txt
expect "an accepted break from 1.0 is a major" "$r" v2.0.0

r="$(repo comment v1.4.2)"
mkdir -p "$r/api" && printf '# only a comment\n' >"$r/api/breaking-accepted.txt"
git -C "$r" add api/breaking-accepted.txt && git -C "$r" commit -q -m "Fix: note (T5)"
expect "a comment in the accepted breaks is no break" "$r" v1.4.3

r="$(repo addremove v1.4.2)"
commit "$r" "Fix: the shape (T5)" api/breaking-accepted.txt
git -C "$r" rm -q api/breaking-accepted.txt && git -C "$r" commit -q -m "Fix: main carries it now (T5)"
expect "a break added and removed in one release is still a major" "$r" v2.0.0

r="$(repo oldtest v0.6.0)"
commit "$r" "Test: an older prefix (T6)"
expect "the older Test: prefix is a patch" "$r" v0.6.1

r="$(repo prerelease v0.6.0)"
commit "$r" "Fix: something (T6)"
git -C "$r" tag v0.7.0-rc.1
commit "$r" "Fix: something else (T6)"
expect "a pre-release tag is passed over" "$r" v0.6.1

r="$(repo merges v0.6.0)"
git -C "$r" switch -q -c side && commit "$r" "Fix: on a branch (T6)" && git -C "$r" switch -q main
git -C "$r" merge -q --no-ff -m "Merge branch side" side
expect "a merged branch counts by its commits" "$r" v0.6.1

r="$(repo named v0.6.0)"
commit "$r" "Fix: something (T6)"
expect_named() {
  local got
  got="$(cd "$r" && "$next" --after "$2" 2>&1)" || got="error: $got"
  if [ "$got" != "$3" ]; then
    echo "FAIL $1: got $got, want $3" >&2
    failed=1
  else
    echo "ok   $1: $got"
  fi
}
expect_named "a named version after the last release" v0.8.0 v0.8.0
expect_named "a named version not after the last release" v0.6.0 "error: next-version: v0.6.0 is not after the last release v0.6.0"
expect_named "a named version that is not vX.Y.Z" 0.8 "error: next-version: 0.8 is not vX.Y.Z"

r="$(repo override v1.4.2)"
commit "$r" "Fix: something (T6)"
ERP_RELEASE_BUMP=major expect "ERP_RELEASE_BUMP overrides" "$r" v2.0.0

r="$(repo nothing v1.0.0)"
expect "nothing since the tag" "$r" "error: next-version: nothing was committed since v1.0.0"

# A git that fails is an error, never a first release or a smaller bump:
# each subcommand next-version.sh reads fails in turn, the others are real.
mkdir -p "$work/failing"
printf '#!/usr/bin/env bash\n[ "$1" != "$FAIL_GIT" ] || { echo "git $1: failed" >&2; exit 128; }\nexec "%s" "$@"\n' "$(command -v git)" >"$work/failing/git"
chmod +x "$work/failing/git"
r="$(repo failing v0.6.0)"
commit "$r" "Fix: the index (T4)" db/migrations/000099_x.up.sql
for sub in tag describe rev-list log diff; do
  if got="$(cd "$r" && PATH="$work/failing:$PATH" FAIL_GIT="$sub" "$next" 2>&1)"; then
    echo "FAIL a failing git $sub: printed $got, want an error" >&2
    failed=1
  else
    echo "ok   a failing git $sub is an error"
  fi
done

if [ -n "${GIT_CLIFF:-}" ]; then
  r="$(repo changelog v0.1.0)"
  cp "$root/cliff.toml" "$r/"
  commit "$r" "A closed period locks for good (T32a)"
  commit "$r" "Fix: refuse a bad thing (T32a)"
  commit "$r" "Refactor: one place for the problems (F7 follow-up, T31)"
  commit "$r" "Tests: cover the lock (T32a)"
  commit "$r" "Plan: the next steps (T40)"
  commit "$r" "Something without an item"
  git -C "$r" commit -q --allow-empty -m "A body names no item" -m "It was discussed under (T9)"
  commit "$r" "Two items at once (T40a, T40b)"
  commit "$r" "Squashed with its pull request (T41) (#12)"
  commit "$r" "Refactor: a refactoring item (R70)"
  commit "$r" "Release: changelog for v0.1.0"
  notes="$(cd "$r" && $GIT_CLIFF --config cliff.toml --unreleased --tag v0.2.0 --strip header 2>/dev/null)"
  notes_failed=0
  check() {
    if grep -qF -- "$2" <<<"$notes"; then
      [ "$1" = has ] || { echo "FAIL changelog: has \"$2\"" >&2; notes_failed=1; }
    else
      [ "$1" = lacks ] || { echo "FAIL changelog: lacks \"$2\"" >&2; notes_failed=1; }
    fi
  }
  check has "## v0.2.0"
  check has "### Added"
  check has "#### T32a"
  check has "- A closed period locks for good (T32a)"
  check has "### Fixed"
  check has "- Refuse a bad thing (T32a)"
  check has "### Changed"
  check has "#### T31"
  check has "- One place for the problems (F7 follow-up, T31)"
  check has "#### No tracker item"
  check has "- Something without an item"
  check lacks "cover the lock"
  check lacks "the next steps"
  check lacks "changelog for"
  check lacks "#### T9"
  check has "#### T40a"
  check lacks "#### T40b"
  check has "#### T41"
  check has "#### R70"
  if [ "$notes_failed" -eq 0 ]; then
    echo "ok   changelog: grouped by kind and tracker item, tests and plans left out"
  else
    failed=1
  fi

  # changelog.sh writes the notes and creates CHANGELOG.md, then adds the
  # next release's entry on top of it.
  mkdir -p "$r/scripts"
  cp "$root/scripts/changelog.sh" "$r/scripts/"
  (cd "$r" && GIT_CLIFF="$GIT_CLIFF" ./scripts/changelog.sh v0.2.0 dist/notes.md >/dev/null 2>&1)
  cp "$r/CHANGELOG.md" "$work/changelog-v0.2.0.md" # main's, as a re-run's checkout has it
  git -C "$r" tag v0.2.0
  commit "$r" "Fix: a later fix (T33)"
  (cd "$r" && GIT_CLIFF="$GIT_CLIFF" ./scripts/changelog.sh v0.2.1 dist/notes.md >/dev/null 2>&1)
  newer="$(grep -n '^## v0.2.1 ' "$r/CHANGELOG.md" | cut -d: -f1)"
  older="$(grep -n '^## v0.2.0 ' "$r/CHANGELOG.md" | cut -d: -f1)"
  if [ -z "$newer" ] || [ -z "$older" ] || [ "$newer" -gt "$older" ]; then
    echo "FAIL changelog.sh: CHANGELOG.md has v0.2.1 at line ${newer:-none} and v0.2.0 at ${older:-none}, want v0.2.1 above" >&2
    failed=1
  elif ! grep -q "A later fix (T33)" "$r/dist/notes.md" || grep -q "locks for good" "$r/dist/notes.md"; then
    echo "FAIL changelog.sh: the v0.2.1 notes are not that release's alone:" >&2
    cat "$r/dist/notes.md" >&2
    failed=1
  else
    echo "ok   changelog.sh: notes of the release alone, CHANGELOG.md newest first"
  fi

  # A release run again after its tag was pushed: the notes are that
  # release's still, and CHANGELOG.md does not gain the entry twice.
  git -C "$r" tag v0.2.1
  (cd "$r" && GIT_CLIFF="$GIT_CLIFF" ./scripts/changelog.sh v0.2.1 dist/again.md >/dev/null 2>&1)
  if ! grep -q "A later fix (T33)" "$r/dist/again.md" || [ "$(grep -c '^## v0.2.1 ' "$r/CHANGELOG.md")" -ne 1 ]; then
    echo "FAIL changelog.sh after the tag: notes and CHANGELOG.md entries:" >&2
    cat "$r/dist/again.md" >&2
    grep '^## v' "$r/CHANGELOG.md" >&2
    failed=1
  else
    echo "ok   changelog.sh after the tag: the release's notes again, one entry in CHANGELOG.md"
  fi
  # The re-run in CI starts from main's CHANGELOG.md, which lacks the entry
  # while the earlier run's pull request is unmerged: it is added once.
  cp "$work/changelog-v0.2.0.md" "$r/CHANGELOG.md"
  (cd "$r" && GIT_CLIFF="$GIT_CLIFF" ./scripts/changelog.sh v0.2.1 dist/again.md >/dev/null 2>&1)
  newer="$(grep -n '^## v0.2.1 ' "$r/CHANGELOG.md" | cut -d: -f1)"
  older="$(grep -n '^## v0.2.0 ' "$r/CHANGELOG.md" | cut -d: -f1)"
  if [ "$(grep -c '^## v0.2.1 ' "$r/CHANGELOG.md")" -ne 1 ] || [ -z "$older" ] || [ "$newer" -gt "$older" ] ||
    ! grep -q "A later fix (T33)" "$r/CHANGELOG.md"; then
    echo "FAIL changelog.sh re-run from main's CHANGELOG.md:" >&2
    cat "$r/CHANGELOG.md" >&2
    failed=1
  else
    echo "ok   changelog.sh re-run from main's CHANGELOG.md: the entry added once, on top"
  fi
else
  echo "skip changelog: GIT_CLIFF is not set"
fi
exit "$failed"
