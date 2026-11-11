#!/usr/bin/env bash
# Merges pull request $1 with a merge commit once both halves of the hybrid
# check passed for its current head (06 §6.5), so no merge depends on
# someone remembering to look:
#   - GitHub's check: ci-ok succeeded on the head commit;
#   - the local gates: scripts/ci-local.sh left a passing report for the
#     head's tree, and the head contains main as it is now, so the merge
#     holds exactly the tree the report tested. The report is posted on
#     the pull request as the record, unless an earlier run of this script
#     posted it already. A light report (make ci-local-light) left the race
#     suite, the coverage gate and fuzzing to GitHub, so it counts only
#     when every ci-ok run of the head was a run of that very commit in
#     which the race shards, the coverage shards, the coverage gate and
#     fuzzing each ran and succeeded (the pull request's run, or a manual
#     run of the branch, which runs them too). The jobs' conclusions are
#     read from the run, not inferred from ci-ok: a skipped job reads as
#     passed to a required check. They may be skipped only for a change
#     scripts/ci-changes.sh, run here on the same commits, calls
#     documentation alone;
#   - main is not red, and its full run is not under way (the red-main rule,
#     06 §6.5): the latest push run on main that finished without being
#     cancelled succeeded, and the latest full run (the manual one the
#     coordinator starts after each batch) finished green. A full run still
#     queued or in progress refuses the merge as a red one does (10-11 Nov
#     2026: a batch merged while the run on the batch before it was still
#     going, and that run then failed). A full run counts as green when it
#     succeeded, or when its ci-ok never ran and every other job, the test
#     job among them, succeeded or was skipped (11 Nov: an outage left
#     ci-ok unqueued and GitHub called the run failed). --fix-forward as
#     the second argument merges regardless: the pull request that repairs
#     main is the one thing that merges while main is red;
#   - a pull request changing deploy/environments (or carrying the
#     promotion workflow's jobs): every one of those passed or was skipped
#     on the head (the checks, the production approval, the images), and
#     where an approver must approve, promotion-approvals.sh still finds
#     the approval now, so a later change request or dismissal counts
#     (ADR-0035).
# The merge names the head it checked, so a push in between fails it.
set -euo pipefail
pr="${1:?usage: merge-pr.sh <pull request number> [--fix-forward]}"
fix_forward=false
case "${2:-}" in
"") ;;
--fix-forward) fix_forward=true ;;
*)
  echo "usage: merge-pr.sh <pull request number> [--fix-forward]" >&2
  exit 2
  ;;
esac
GH="${GH:-gh}"

# One call, so the checks read are the head's. A head can carry several
# ci-ok runs (a re-run, a reopened pull request): every one must pass.
read -r head state promotion < <($GH pr view "$pr" --json headRefOid,statusCheckRollup,files --jq '.headRefOid + " " +
  ([.statusCheckRollup[] | select(.name == "ci-ok") | (.conclusion // "") | if . == "" then "PENDING" else . end]
   | if length == 0 then "MISSING" elif all(. == "SUCCESS") then "SUCCESS" else map(select(. != "SUCCESS"))[0] end) + " " +
  (if any(.files[]; .path | startswith("deploy/environments/")) or any(.statusCheckRollup[]; .workflowName == "promotion") then
    ([.statusCheckRollup[] | select(.workflowName == "promotion") | (.conclusion // "") | if . == "" then "PENDING" else . end]
     | if length == 0 then "MISSING" elif all(. == "SUCCESS" or . == "SKIPPED") then "SUCCESS" else map(select(. != "SUCCESS" and . != "SKIPPED"))[0] end)
   else "NONE" end)')
if [ "$state" != SUCCESS ]; then
  echo "merge-pr: ci-ok on $head is $state, not SUCCESS" >&2
  exit 1
fi
if [ "$promotion" != SUCCESS ] && [ "$promotion" != NONE ]; then
  echo "merge-pr: the promotion workflow on $head is $promotion, not SUCCESS" >&2
  exit 1
fi
repo="$($GH repo view --json nameWithOwner --jq .nameWithOwner)"

# jobs RUN prints "name<TAB>conclusion" for every job of a run's latest
# attempt; a job without a conclusion yet reads as PENDING.
jobs_of() {
  $GH api --paginate "repos/$repo/actions/runs/$1/jobs?per_page=100" --jq '.jobs[] | .name + "\t" + (.conclusion // "PENDING")'
}

# main_is_green fails, saying why, while main is red or its full run is
# still going (see the top).
main_is_green() {
  local push full id status conclusion jobs bad
  push="$($GH run list --repo "$repo" --workflow ci.yml --branch main --event push --limit 20 --json status,conclusion \
    --jq '[.[] | select(.status == "completed" and .conclusion != "cancelled")][0].conclusion // "none"')" || push=unreadable
  if [ "$push" != success ] && [ "$push" != none ]; then
    echo "merge-pr: main is red: its latest finished push run ended $push; only the fix forward merges (--fix-forward)" >&2
    return 1
  fi
  full="$($GH run list --repo "$repo" --workflow ci.yml --branch main --event workflow_dispatch --limit 1 \
    --json databaseId,status,conclusion --jq '.[0] // {databaseId: 0, status: "none"} | "\(.databaseId) \(.status) \(.conclusion // "" | if . == "" then "none" else . end)"')" || full=""
  read -r id status conclusion <<<"$full" || true # an empty answer leaves them empty, refused below
  if [ -z "$conclusion" ]; then
    echo "merge-pr: main's full runs could not be read" >&2
    return 1
  fi
  if [ "$status" = none ] || [ "$conclusion" = success ]; then
    return 0
  fi
  if [ "$status" != completed ]; then
    echo "merge-pr: main's latest full run ($id) is $status: wait for it, since nothing merges on top of a batch whose full run has not finished; only the fix forward merges (--fix-forward)" >&2
    return 1
  fi
  # Not a success by GitHub's word. It is still green when ci-ok never ran
  # and every job that did is green; a ci-ok that ran and failed, or any
  # other job failed, cancelled or unfinished, is red.
  jobs="$(jobs_of "$id")" || jobs=$'unreadable\tunreadable'
  bad="$(awk -F'\t' '$1 == "ci-ok" { if ($2 == "failure") print $1 ": " $2; next }
    $2 != "success" && $2 != "skipped" { print $1 ": " $2 }' <<<"$jobs")"
  if [ -z "$bad" ] && grep -qx $'test\tsuccess' <<<"$jobs"; then
    echo "merge-pr: main's latest full run ($id) ended $conclusion without its ci-ok having run; every other job is green, so it counts as green"
    return 0
  fi
  echo "merge-pr: main is red: its latest full run ($id) ended $conclusion ($(tr '\n' ' ' <<<"${bad:-the test job did not succeed}" | sed 's/ $//')); only the fix forward merges (--fix-forward)" >&2
  return 1
}

# github_held_the_gates fails unless every ci-ok run of the head is a run
# of the head commit in which the gates a light report leaves out ran and
# succeeded (or, for documentation alone, were skipped).
github_held_the_gates() {
  local runs run sha jobs docs=false rc=0 gate
  runs="$($GH pr view "$pr" --json statusCheckRollup --jq '[.statusCheckRollup[] | select(.name == "ci-ok") | (.detailsUrl // "")
    | capture("/actions/runs/(?<run>[0-9]+)") | .run] | unique | .[]')"
  if [ -z "$runs" ]; then
    echo "merge-pr: no GitHub run found behind ci-ok on $head" >&2
    return 1
  fi
  if [ "$(EVENT=pull_request BASE="$(git merge-base origin/main "$head")" HEAD="$head" "$(dirname "$0")/ci-changes.sh" | sed -n 's/^run=//p')" = false ]; then
    docs=true
  fi
  for run in $runs; do
    sha="$($GH api "repos/$repo/actions/runs/$run" --jq .head_sha)" || sha=unreadable
    if [ "$sha" != "$head" ]; then
      echo "merge-pr: ci-ok's run $run is a run of $sha, not of $head" >&2
      rc=1
      continue
    fi
    jobs="$(jobs_of "$run")" || jobs="" # no job read: every gate missing, refused below
    # A matrix's jobs are named "race (3)", and a matrix skipped whole is one
    # job named "race"; the coverage gate is "coverage".
    for gate in '^race( [(]|$)' '^coverage-shard( [(]|$)' '^coverage$' '^fuzz$'; do
      if ! awk -F'\t' -v gate="$gate" -v docs="$docs" '
          $1 ~ gate { n++; if ($2 != "success" && !(docs == "true" && $2 == "skipped")) bad = 1 }
          END { exit !(n > 0 && !bad) }' <<<"$jobs"; then
        echo "merge-pr: run $run did not hold the gate a light report leaves to GitHub: jobs matching $gate are $(awk -F'\t' -v gate="$gate" '$1 ~ gate { printf "%s%s: %s", sep, $1, $2; sep = ", " } END { if (!sep) printf "missing" }' <<<"$jobs")" >&2
        rc=1
      fi
    done
  done
  return "$rc"
}

git fetch -q origin "$head" main
# The head must contain main as it is now. The local gates run on the
# head's tree, not on GitHub's merge of the head into main; on a head that
# contains main the merge's tree is the head's, so the report vouches for
# what main will hold, by gates (ci-local.sh, the checks it runs, the tools
# the workflows pin) at main's latest version.
if ! git merge-base --is-ancestor origin/main "$head"; then
  echo "merge-pr: $head does not contain origin/main ($(git rev-parse --short origin/main)): rebase on main and run make ci-local again" >&2
  exit 1
fi
if [ "$fix_forward" = true ]; then
  echo "merge-pr: --fix-forward: merging whatever main's runs say"
else
  main_is_green
fi
if [ "$promotion" = SUCCESS ]; then
  approval="$(mktemp)"
  APPROVAL_OUT="$approval" "$(dirname "$0")/promotion-checks.sh" origin/main "$head" >/dev/null
  if grep -q '^production$' "$approval"; then
    GH_TOKEN="$($GH auth token)" "$(dirname "$0")/promotion-approvals.sh" "$repo" "$pr" "$head"
  fi
  rm -f "$approval"
fi
tree="$(git rev-parse "$head^{tree}")"
report="$(CDPATH='' cd "$(git rev-parse --git-common-dir)" && pwd)/ci-local/$tree.md"
if ! head -1 "$report" 2>/dev/null | grep -qx "ci-local PASS tree $tree"; then
  echo "merge-pr: no passing scripts/ci-local.sh report for tree $tree (head $head)" >&2
  exit 1
fi
# A report without the line is from before the forms: it ran everything.
if sed -n 2p "$report" | grep -q '^Form: light'; then
  if ! github_held_the_gates; then
    echo "merge-pr: the report for tree $tree is light: run make ci-local (the full form), or let GitHub's run hold the race, coverage and fuzz gates on $head" >&2
    exit 1
  fi
elif ! sed -n 2p "$report" | grep -Eq '^(Form: full)?$'; then
  echo "merge-pr: the report for tree $tree names a form this script does not know: $(sed -n 2p "$report")" >&2
  exit 1
fi
comments="$($GH pr view "$pr" --json comments --jq '.comments[].body')"
if ! grep -qx "ci-local PASS tree $tree" <<<"$comments"; then
  $GH pr comment "$pr" --body-file "$report"
fi
$GH pr merge "$pr" --merge --match-head-commit "$head"
