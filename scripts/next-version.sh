#!/usr/bin/env bash
# Prints the next release version for REV (default HEAD) by ADR-0032's
# rules, from the commits since the last vX.Y.Z tag reachable from it:
#   - major: a commit added or changed an entry of api/breaking-accepted.txt
#     (an accepted contract break); before 1.0.0 that is a minor bump, as
#     semantic versioning allows while the API has no external consumers;
#   - minor: a migration, a change to a contract (api/*.yaml), or a commit
#     that is not a Fix:, Refactor:, Tests: (Test:), CI:, Plan:, Tracker: or
#     Process: commit (or the Release: commit of a changelog);
#   - patch: anything else.
# Pre-release tags (v1.3.0-rc.1) are not releases and are passed over. The
# first release is v0.1.0. It fails when nothing was committed since the
# last tag. ERP_RELEASE_BUMP (major, minor or patch) overrides the rules.
# With --after VERSION it only checks that VERSION (vX.Y.Z) is greater than
# the last release, for a version the person releasing names.
set -euo pipefail
named=""
if [ "${1:-}" = --after ]; then
  named="${2:?usage: next-version.sh --after vX.Y.Z}"
  shift 2
fi
rev="${1:-HEAD}"
last="$(git describe --tags --abbrev=0 --match 'v[0-9]*.[0-9]*.[0-9]*' --exclude '*-*' "$rev" 2>/dev/null || true)"
if [ -n "$named" ] && ! [[ "$named" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "next-version: $named is not vX.Y.Z" >&2
  exit 1
fi
if [ -z "$last" ]; then
  echo "${named:-v0.1.0}"
  exit 0
fi
if ! [[ "$last" =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
  echo "next-version: the last tag $last is not vX.Y.Z" >&2
  exit 1
fi
major="${BASH_REMATCH[1]}" minor="${BASH_REMATCH[2]}" patch="${BASH_REMATCH[3]}"
if [ -n "$named" ]; then
  [[ "$named" =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+)$ ]] # checked above; this fills BASH_REMATCH
  if [ "${BASH_REMATCH[1]}" -lt "$major" ] ||
    { [ "${BASH_REMATCH[1]}" -eq "$major" ] && [ "${BASH_REMATCH[2]}" -lt "$minor" ]; } ||
    { [ "${BASH_REMATCH[1]}" -eq "$major" ] && [ "${BASH_REMATCH[2]}" -eq "$minor" ] && [ "${BASH_REMATCH[3]}" -le "$patch" ]; }; then
    echo "next-version: $named is not after the last release $last" >&2
    exit 1
  fi
  echo "$named"
  exit 0
fi
range="$last..$rev"
if [ -z "$(git rev-list --no-merges "$range")" ]; then
  echo "next-version: nothing was committed since $last" >&2
  exit 1
fi
# The history is read whole before it is searched: grep -q stopping at the
# first match would end git's output early, and under pipefail the SIGPIPE
# that git gets would read as "no match".
subjects="$(git log --no-merges --format=%s "$range")"
# An entry added to the accepted breaks counts even when a later commit of
# the same release removed it again (the file only lists what main has not
# absorbed yet), so each commit's change is read, not the net difference.
accepted="$(git log --no-merges -p --format= "$range" -- api/breaking-accepted.txt)"
bump=patch
if [ -n "$(git diff --name-only "$range" -- db/migrations 'api/*.yaml')" ] ||
  grep -Evq '^(Fix|Refactor|Tests?|CI|Plan|Tracker|Process|Release):' <<<"$subjects"; then
  bump=minor
fi
if grep -Eq '^\+[^+#[:space:]]' <<<"$accepted"; then
  bump=major
  [ "$major" -ge 1 ] || bump=minor
fi
bump="${ERP_RELEASE_BUMP:-$bump}"
case "$bump" in
major) echo "v$((major + 1)).0.0" ;;
minor) echo "v$major.$((minor + 1)).0" ;;
patch) echo "v$major.$minor.$((patch + 1))" ;;
*)
  echo "next-version: ERP_RELEASE_BUMP=$bump: want major, minor or patch" >&2
  exit 1
  ;;
esac
