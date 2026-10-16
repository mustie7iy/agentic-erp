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

r="$(repo override v1.4.2)"
commit "$r" "Fix: something (T6)"
ERP_RELEASE_BUMP=major expect "ERP_RELEASE_BUMP overrides" "$r" v2.0.0

r="$(repo nothing v1.0.0)"
expect "nothing since the tag" "$r" "error: next-version: nothing was committed since v1.0.0"

if [ -n "${GIT_CLIFF:-}" ]; then
  r="$(repo changelog v0.1.0)"
  cp "$root/cliff.toml" "$r/"
  commit "$r" "A closed period locks for good (T32a)"
  commit "$r" "Fix: refuse a bad thing (T32a)"
  commit "$r" "Refactor: one place for the problems (F7 follow-up, T31)"
  commit "$r" "Tests: cover the lock (T32a)"
  commit "$r" "Plan: the next steps (T40)"
  commit "$r" "Something without an item"
  commit "$r" "Release: changelog for v0.1.0"
  notes="$(cd "$r" && $GIT_CLIFF --config cliff.toml --unreleased --tag v0.2.0 --strip header 2>/dev/null)"
  check() {
    if printf '%s\n' "$notes" | grep -qF -- "$2"; then
      [ "$1" = has ] || { echo "FAIL changelog: has \"$2\"" >&2; failed=1; }
    else
      [ "$1" = lacks ] || { echo "FAIL changelog: lacks \"$2\"" >&2; failed=1; }
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
  [ "$failed" -ne 0 ] || echo "ok   changelog: grouped by kind and tracker item, tests and plans left out"

  # changelog.sh writes the notes and creates CHANGELOG.md, then adds the
  # next release's entry on top of it.
  mkdir -p "$r/scripts"
  cp "$root/scripts/changelog.sh" "$r/scripts/"
  (cd "$r" && GIT_CLIFF="$GIT_CLIFF" ./scripts/changelog.sh v0.2.0 dist/notes.md >/dev/null 2>&1)
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
else
  echo "skip changelog: GIT_CLIFF is not set"
fi
exit "$failed"
