#!/usr/bin/env bash
# Decides whether CI's jobs beyond the secret scan have to run for this
# event (06 §6.5) and writes run=true|false and reason=... for
# GITHUB_OUTPUT. They are skipped when only documentation changed (docs/,
# Markdown files): no test reads it, and secret scanning runs on every
# event regardless. Anything it cannot establish runs them.
#
# EVENT is pull_request or push, BASE the commit to compare with (the pull
# request's base), HEAD the commit under test (default HEAD). A push to
# main is compared with the last commit whose run on main passed, not with
# the commit before it: a newer push cancels a running one (ci.yml), so a
# documentation merge right after a code merge would otherwise skip the
# only full run the code would get. GH (default gh) and GITHUB_REPOSITORY
# find that run; tests replace GH.
set -euo pipefail
event="${EVENT:?EVENT: pull_request or push}"
head="${HEAD:-$(git rev-parse HEAD)}"
base="${BASE:-}"
GH="${GH:-gh}"

decide() {
  echo "run=$1"
  echo "reason=$2"
  exit 0
}

if [ "$event" = push ]; then
  base="$($GH run list --repo "${GITHUB_REPOSITORY:?}" --workflow ci.yml --branch main --event push \
    --status success --limit 1 --json headSha --jq '.[0].headSha // empty')" || base=""
  if [ -n "$base" ] && ! git merge-base --is-ancestor "$base" "$head" 2>/dev/null; then
    decide true "the last green commit on main is not an ancestor"
  fi
fi
if [ -z "$base" ] || [[ "$base" =~ ^0+$ ]] || ! git cat-file -e "${base}^{commit}" 2>/dev/null; then
  decide true "no base commit to compare with"
fi
if [ "$event" = pull_request ]; then
  files="$(git diff --name-only "${base}...${head}")"
else
  files="$(git diff --name-only "$base" "$head")"
fi
code="$(grep -Ev '^docs/|\.md$' <<<"$files" || true)"
if [ -n "$files" ] && [ -z "$code" ]; then
  decide false "only documentation changed"
fi
decide true "code changed"
