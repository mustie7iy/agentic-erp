#!/usr/bin/env bash
# Decides whether CI's jobs beyond the secret scan have to run for this
# event (06 §6.5) and writes run=true|false and reason=... for
# GITHUB_OUTPUT. They are skipped when only documentation changed (docs/,
# Markdown files): no test of the suite reads it; secret scanning, and the
# tracker's own check (docs/tracker_test.go, ci.yml's tracker job), run on
# every event regardless. Anything it cannot establish runs them. When they run,
# it also writes images=true|false and release=true|false: whether the
# image reproducibility gate and the signing round trip, and the release
# scripts' tests, have to run, which they do when their inputs changed
# (the weekly run does them regardless). It always writes deploy=true|false
# and promotion=true|false: whether a change reaches the deploy workflow's
# paths (deploy.yml's on.push.paths, kept equal to the pattern below) or
# the promotion self-test's (promotion-self-test.yml), documentation
# included, as those workflows' path filters count it. scripts/ci-local.sh
# runs those checks on the same classes, before a merge.
#
# EVENT is pull_request, push, schedule or workflow_dispatch (the nightly
# and manual runs, which run everything, but for a nightly of a main whose
# latest full run passed at that commit), BASE the commit to compare with
# (the pull request's base), HEAD the commit under test
# (default HEAD). A push to main is compared with the last commit whose
# push run on main passed, not with the commit before it: a newer push
# cancels a running one (ci.yml), so a documentation merge right after a
# code merge would otherwise skip the only push run (fuzzing, signing, the
# release scripts) the code would get; the full suite runs nightly. GH
# (default gh) and GITHUB_REPOSITORY find that run; tests replace GH.
set -euo pipefail
event="${EVENT:?EVENT: pull_request, push, schedule or workflow_dispatch}"
head="${HEAD:-$(git rev-parse HEAD)}"
base="${BASE:-}"
GH="${GH:-gh}"

images=true
release=true
deploy=true
promotion=true
decide() {
  echo "run=$1"
  echo "reason=$2"
  echo "deploy=$deploy"
  echo "promotion=$promotion"
  if [ "$1" = true ]; then
    echo "images=$images"
    echo "release=$release"
  else
    echo "images=false"
    echo "release=false"
  fi
  exit 0
}

# changed prints true when a changed file (of $2, default all) matches the
# pattern $1.
changed() {
  if grep -Eq "$1" <<<"${2-$files}"; then echo true; else echo false; fi
}

# A nightly run of a main whose latest finished full run (nightly or manual)
# passed at this very commit runs nothing: the suite takes about three hours on
# the runner and the Actions budget is small (PO 2026-10-26). The latest such
# run decides, not the last green one, so a red or cancelled run (a timed-out
# suite ends cancelled) is never hidden by an older green one at the same
# commit. A manual run never skips. No run found (beyond the last 100 runs on
# main, or gh failing), a red one or another commit runs everything.
if [ "$event" = schedule ]; then
  full="$($GH run list --repo "${GITHUB_REPOSITORY:?}" --workflow ci.yml --branch main \
    --limit 100 --json headSha,event,status,conclusion \
    --jq '[.[] | select((.event == "schedule" or .event == "workflow_dispatch") and .status == "completed")][0] | select(.conclusion == "success") | .headSha // empty')" || full=""
  if [ -n "$full" ] && [ "$full" = "$(git rev-parse "$head")" ]; then
    decide false "main has not moved since its latest full run, which passed"
  fi
  [ -n "$full" ] || decide true "no passing latest full run on main to compare with"
  decide true "a nightly run of a main that has moved"
fi
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
images="$(changed '^(go\.(mod|sum)|\.ko\.yaml|\.dockerignore|deploy/images/|Makefile|cmd/|release/|scripts/(image|test-images|lib/sbom-root|sign|verify-image|provenance|free-disk)|\.github/workflows/(ci|weekly)\.yml)')"
release="$(changed '^(scripts/(next-version|changelog|test-release-scripts)\.sh|cliff\.toml|Makefile|\.github/workflows/(ci|release|weekly)\.yml)')"
# deploy.yml's paths: deploy/ but its environments (promotion.yml's), the
# scripts it runs, temporal/, scripts/lib/ and the workflows it lints.
deploy="$(changed '^(deploy/|temporal/|scripts/lib/|scripts/(deploy-checks|helm-checks|admission-checks|mirror-images|test-mirror-images|nats-checks|temporal-checks)\.sh$|\.github/workflows/)' "$(grep -v '^deploy/environments/' <<<"$files" || true)")"
promotion="$(changed '^(scripts/(promotion-checks|test-promotion-checks|promotion-approvals|test-promotion-approvals|verify-image|rollback-drill|test-rollback-drill|restore-drill|test-restore-drill)\.sh|\.github/approvers|\.github/workflows/promotion(-self-test|-review)?\.yml)$')"
code="$(grep -Ev '^docs/|\.md$' <<<"$files" || true)"
if [ -n "$files" ] && [ -z "$code" ]; then
  decide false "only documentation changed"
fi
decide true "code changed"
