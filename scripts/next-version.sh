#!/usr/bin/env bash
# Prints the next release version for REV (default HEAD) by ADR-0032's
# rules, from the commits since the last vX.Y.Z tag reachable from it:
#   - major: api/breaking-accepted.txt gained an entry (an accepted contract
#     break); before 1.0.0 that is a minor bump, as semantic versioning
#     allows while the API has no external consumers;
#   - minor: a migration, a change to a contract (api/*.yaml), or a commit
#     that is not a Fix:, Refactor:, Tests: (Test:), CI:, Plan:, Tracker: or
#     Process: commit (or the Release: commit of a changelog);
#   - patch: anything else.
# The first release is v0.1.0. It fails when nothing was committed since the
# last tag. ERP_RELEASE_BUMP (major, minor or patch) overrides the rules.
set -euo pipefail
rev="${1:-HEAD}"
last="$(git describe --tags --abbrev=0 --match 'v[0-9]*.[0-9]*.[0-9]*' "$rev" 2>/dev/null || true)"
if [ -z "$last" ]; then
  echo v0.1.0
  exit 0
fi
if ! [[ "$last" =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
  echo "next-version: the last tag $last is not vX.Y.Z" >&2
  exit 1
fi
major="${BASH_REMATCH[1]}" minor="${BASH_REMATCH[2]}" patch="${BASH_REMATCH[3]}"
range="$last..$rev"
if [ -z "$(git rev-list --no-merges "$range")" ]; then
  echo "next-version: nothing was committed since $last" >&2
  exit 1
fi
bump=patch
if [ -n "$(git diff --name-only "$range" -- db/migrations 'api/*.yaml')" ] ||
  git log --no-merges --format=%s "$range" | grep -Evq '^(Fix|Refactor|Tests?|CI|Plan|Tracker|Process|Release):'; then
  bump=minor
fi
if git diff "$range" -- api/breaking-accepted.txt | grep -Eq '^\+[^+#[:space:]]'; then
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
