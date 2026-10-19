#!/usr/bin/env bash
# Tests promotion-checks.sh on a throwaway git repository (ADR-0035,
# T40d): each case is a branch off main promoting, rolling back or
# recording a drill, and must be accepted or refused, the refusal naming
# its reason; an accepted promotion's OUT lists its images by digest.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
repo="$work/repo"
mkdir -p "$repo/scripts" "$repo/deploy/environments"
cp "$root/scripts/promotion-checks.sh" "$repo/scripts/"
cp "$root/deploy/environments/path.json" "$repo/deploy/environments/"
git -C "$repo" init -q -b main
git -C "$repo" config user.email test@example.com
git -C "$repo" config user.name test
git -C "$repo" add .
git -C "$repo" commit -q -m "the promotion path"
failed=0

# release VERSION DIGIT: a release.json whose commit and digests repeat DIGIT.
release() {
  local d c
  d="$(printf "%064d" 0 | tr 0 "$2")" c="$(printf "%040d" 0 | tr 0 "$2")"
  printf '{"release": {"version": "%s", "commit": "%s"}, "images": {' "$1" "$c"
  printf '"%s": {"repository": "111111111111.dkr.ecr.eu-central-1.amazonaws.com/erp-%s", "digest": "sha256:%s"}, ' \
    server server "$d" router router "$d" pgbouncer pgbouncer "$d" nats nats "$d"
  printf '"temporal": {"repository": "111111111111.dkr.ecr.eu-central-1.amazonaws.com/erp-temporal", "digest": "sha256:%s"}}}\n' "$d"
}
# put PATH CONTENT MESSAGE: commits CONTENT at PATH on the current branch.
put() {
  mkdir -p "$(dirname "$repo/$1")"
  printf '%s\n' "$2" >"$repo/$1"
  git -C "$repo" add "$1"
  git -C "$repo" commit -q -m "$3"
}
env_file() { echo "deploy/environments/$1/release.json"; }
# drill VERSION DIGIT ROLLED_BACK_TO RESULT: a drill's record of the release whose commit repeats DIGIT.
drill() { printf '{"release": "%s", "commit": "%s", "rolled_back_to": "%s", "result": "%s", "run": "https://ci.example/run/1"}' "$1" "$(printf "%040d" 0 | tr 0 "$2")" "$3" "$4"; }

# main: v1.0.0 everywhere, v1.1.0 promoted up to staging, whose drill passed.
for cell in dev/dev1 test/test1 staging/stg1 production/prod1 production/prod2; do put "$(env_file $cell)" "$(release v1.0.0 1)" "v1.0.0 to $cell"; done
for cell in dev/dev1 test/test1 staging/stg1; do put "$(env_file $cell)" "$(release v1.1.0 2)" "v1.1.0 to $cell"; done
put deploy/environments/staging/stg1/drills/v1.1.0.json "$(drill v1.1.0 2 v1.0.0 passed)" "v1.1.0's drill"
put "$(env_file dev/dev1)" "$(release v1.2.0 3)" "v1.2.0 to dev"
# faildrill: the same, but the drill failed.
git -C "$repo" checkout -q -b faildrill
put deploy/environments/staging/stg1/drills/v1.1.0.json "$(drill v1.1.0 2 v1.0.0 failed)" "v1.1.0's drill, again"
# repinned: v1.1.0 re-pinned to another commit up to staging, its drill that of the first.
git -C "$repo" checkout -q -b repinned main
for cell in dev/dev1 test/test1 staging/stg1; do put "$(env_file $cell)" "$(release v1.1.0 5)" "v1.1.0 again to $cell"; done
git -C "$repo" checkout -q main

# case NAME WANT GREP STEPS: STEPS (shell) on a branch off BASE (main by
# default); WANT is ok or refused, GREP what the output must say.
case_() {
  local name="$1" want="$2" grep="$3" got base="${BASE:-main}"
  git -C "$repo" checkout -q -B "$name" "$base"
  (cd "$repo" && eval "$4")
  if OUT="$work/out" "$repo/scripts/promotion-checks.sh" "$base" >"$work/log" 2>&1; then got=ok; else got=refused; fi
  if [ "$got" != "$want" ] || { [ -n "$grep" ] && ! grep -q "$grep" "$work/log"; }; then
    echo "FAIL $name: got $got, want $want${grep:+ ($grep)}" >&2
    cat "$work/log" >&2
    failed=1
  else
    echo "ok   $name"
  fi
  git -C "$repo" checkout -q main
}

case_ dev-takes-any ok "" 'put "$(env_file dev/dev1)" "$(release v1.3.0 4)" p'
case_ test-takes-devs ok "" 'put "$(env_file test/test1)" "$(release v1.2.0 3)" p'
case_ test-skips-nothing refused "may take only the release dev/dev1 runs" 'put "$(env_file test/test1)" "$(release v1.3.0 4)" p'
case_ canary-after-drill ok "" 'put "$(env_file production/prod1)" "$(release v1.1.0 2)" p'
BASE=faildrill case_ canary-after-a-failed-drill refused "rollback drill" 'put "$(env_file production/prod1)" "$(release v1.1.0 2)" p'
BASE=repinned case_ canary-of-a-repinned-version refused "for that commit" 'put "$(env_file production/prod1)" "$(release v1.1.0 5)" p'
case_ release-and-script refused "nothing else" 'put "$(env_file test/test1)" "$(release v1.2.0 3)" p && put scripts/verify-image.sh "exit 0" q'
case_ rename-onto-a-release refused "nothing else" 'mkdir -p deploy/environments/production/prod3 && git mv "$(env_file production/prod2)" "$(env_file production/prod3)" && git commit -q -m p'
case_ drill-without-commit refused "must be" 'put deploy/environments/staging/stg1/drills/v1.2.0.json "$(drill v1.2.0 3 v1.1.0 passed | sed "s/\"commit\": \"3*\", //")" p'
case_ bad-path refused "path.json must" 'put deploy/environments/path.json "{\"path\": [\"dev/dev1\"], \"drill\": \"staging/stg1\"}" p'
case_ cells-follow-the-canary refused "may take only the release production/prod1 runs" 'put "$(env_file production/prod2)" "$(release v1.1.0 2)" p'
case_ rollback ok "rolls back" 'put "$(env_file staging/stg1)" "$(release v1.0.0 1)" p'
case_ two-cells refused "one cell" 'put "$(env_file test/test1)" "$(release v1.2.0 3)" p && put "$(env_file staging/stg1)" "$(release v1.0.0 1)" q'
case_ release-and-drill refused "one cell" 'put "$(env_file test/test1)" "$(release v1.2.0 3)" p && put deploy/environments/staging/stg1/drills/v1.2.0.json "$(drill v1.2.0 3 v1.1.0 passed)" q'
case_ drill-record ok "records the drill" 'put deploy/environments/staging/stg1/drills/v1.2.0.json "$(drill v1.2.0 3 v1.1.0 failed)" p'
case_ bad-drill-record refused "must be" 'put deploy/environments/staging/stg1/drills/v1.2.0.json "$(drill v1.3.0 3 v1.1.0 passed)" p'
case_ by-tag refused "not a sha256 digest" 'put "$(env_file dev/dev1)" "$(release v1.3.0 4 | sed "s/sha256:4*\"/latest\"/")" p'
case_ foreign-repository refused "repository" 'put "$(env_file dev/dev1)" "$(release v1.3.0 4 | sed "s|amazonaws.com/erp-nats|amazonaws.com/nats|")" p'
case_ missing-image refused "images must be exactly" 'put "$(env_file dev/dev1)" "$(release v1.3.0 4 | python3 -c "import json,sys; d=json.load(sys.stdin); del d[\"images\"][\"temporal\"]; print(json.dumps(d))")" p'
case_ short-commit refused "full commit" 'put "$(env_file dev/dev1)" "$(release v1.3.0 4 | sed "s/\"commit\": \"4*\"/\"commit\": \"4444\"/")" p'
case_ cell-values ok "changes the values" 'put deploy/environments/dev/dev1/cell.json "{\"environment\": \"dev\", \"cell\": \"dev1\"}" p'
case_ cell-values-of-another-cell refused "naming its cell" 'put deploy/environments/dev/dev1/cell.json "{\"environment\": \"dev\", \"cell\": \"dev2\"}" p'
case_ cell-values-with-images refused "pins no image" 'put deploy/environments/dev/dev1/cell.json "{\"environment\": \"dev\", \"cell\": \"dev1\", \"erpCell\": {\"images\": {}}}" p'
case_ cell-values-and-release refused "one cell" 'put deploy/environments/dev/dev1/cell.json "{\"environment\": \"dev\", \"cell\": \"dev1\"}" p && put "$(env_file dev/dev1)" "$(release v1.3.0 4)" q'
case_ unknown-file refused "only cells" 'put deploy/environments/notes.txt hello p'
case_ off-the-path refused "not on" 'put "$(env_file sandbox/sbx1)" "$(release v1.2.0 3)" p'

# An accepted promotion lists its five images, by digest, with the commit.
git -C "$repo" checkout -q -B listed main
(cd "$repo" && put "$(env_file test/test1)" "$(release v1.2.0 3)" p)
# As the workflow runs it: BASE's copy of the checks, on the PR's commit.
git -C "$repo" checkout -q main
OUT="$work/out" "$repo/scripts/promotion-checks.sh" main listed >/dev/null
if [ "$(grep -c "^$(printf "%040d" 0 | tr 0 3) .*@sha256:$(printf "%064d" 0 | tr 0 3)$" "$work/out")" -ne 5 ]; then
  echo "FAIL listed: OUT does not list the five images with the release's commit" >&2
  cat "$work/out" >&2
  failed=1
else
  echo "ok   listed"
fi
# The promoted cell, which decides whether an approver must approve.
OUT="$work/out" CELL_OUT="$work/cell" "$repo/scripts/promotion-checks.sh" main listed >/dev/null
if [ "$(cat "$work/cell")" != test/test1 ]; then
  echo "FAIL cell: CELL_OUT names $(cat "$work/cell"), want test/test1" >&2
  failed=1
else
  echo "ok   cell"
fi
exit "$failed"
