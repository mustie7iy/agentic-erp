#!/usr/bin/env bash
# Decides whether CI's suite has to run for this event (06 §6.5) and writes
# run=true|false and reason=... for GITHUB_OUTPUT. The suite is skipped when
#   - only documentation changed (docs/, Markdown files): no test reads it,
#     and secret scanning runs on every event regardless; or
#   - a push to main brings exactly the tree that the merged pull request's
#     last green run tested (that run saved the tree it tested as the
#     artifact "tested-tree"), so running it again proves nothing new.
# Anything it cannot establish runs the suite.
#
# EVENT is pull_request or push, BASE the commit to compare with (the pull
# request's base, or the commit main was at before the push), HEAD the
# commit under test (default HEAD). GH (default gh) and GITHUB_REPOSITORY
# find the pull request and its run; tests replace GH.
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

# tested_tree prints the tree the merged pull request of commit $1 had a
# green run on, or fails when there is none.
tested_tree() {
  local pr_head run dir
  pr_head="$($GH api "repos/${GITHUB_REPOSITORY:?}/commits/$1/pulls" --jq '[.[] | select(.merged_at != null)][0].head.sha // empty')"
  [ -n "$pr_head" ] || return 1
  run="$($GH run list --repo "$GITHUB_REPOSITORY" --workflow ci.yml --event pull_request --commit "$pr_head" \
    --status success --limit 1 --json databaseId --jq '.[0].databaseId // empty')"
  [ -n "$run" ] || return 1
  dir="$(mktemp -d)"
  trap 'rm -rf "$dir"' RETURN
  $GH run download "$run" --repo "$GITHUB_REPOSITORY" --name tested-tree --dir "$dir" >/dev/null 2>&1 || return 1
  cat "$dir/tree"
}

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
if [ "$event" = push ]; then
  # A pull request runs the workflow as it changes it, so one that changes
  # CI could vouch for a tree it did not test: such a push always runs.
  if grep -Eq '^(\.github/|scripts/ci-)' <<<"$files"; then
    decide true "CI itself changed"
  fi
  if tested="$(tested_tree "$head")" && [ "$tested" = "$(git rev-parse "${head}^{tree}")" ]; then
    decide false "the merged pull request's green run tested this exact tree"
  fi
fi
decide true "code changed"
