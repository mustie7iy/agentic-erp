#!/usr/bin/env bash
# Merges pull request $1 with a merge commit once both halves of the hybrid
# check passed for its current head (06 §6.5), so no merge depends on
# someone remembering to look; it stands in for branch protection, which a
# private repository on this plan does not enforce:
#   - GitHub's short check: ci-ok succeeded on the head commit;
#   - the full local gates: scripts/ci-local.sh left a passing report for
#     the head's tree. The report is posted on the pull request as the
#     record, unless an earlier run of this script posted it already;
#   - a pull request changing deploy/environments (or carrying the
#     promotion workflow's jobs): every one of those passed or was skipped
#     on the head (the checks, the production approval, the images), and
#     where an approver must approve, promotion-approvals.sh still finds
#     the approval now, so a later change request or dismissal counts
#     (ADR-0035).
# The merge names the head it checked, so a push in between fails it.
set -euo pipefail
pr="${1:?usage: merge-pr.sh <pull request number>}"
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

git fetch -q origin "$head"
if [ "$promotion" = SUCCESS ]; then
  git fetch -q origin main
  approval="$(mktemp)"
  APPROVAL_OUT="$approval" "$(dirname "$0")/promotion-checks.sh" origin/main "$head" >/dev/null
  if grep -q '^production$' "$approval"; then
    GH_TOKEN="$($GH auth token)" "$(dirname "$0")/promotion-approvals.sh" "$($GH repo view --json nameWithOwner --jq .nameWithOwner)" "$pr" "$head"
  fi
  rm -f "$approval"
fi
tree="$(git rev-parse "$head^{tree}")"
report="$(CDPATH='' cd "$(git rev-parse --git-common-dir)" && pwd)/ci-local/$tree.md"
if ! head -1 "$report" 2>/dev/null | grep -qx "ci-local PASS tree $tree"; then
  echo "merge-pr: no passing scripts/ci-local.sh report for tree $tree (head $head)" >&2
  exit 1
fi
comments="$($GH pr view "$pr" --json comments --jq '.comments[].body')"
if ! grep -qx "ci-local PASS tree $tree" <<<"$comments"; then
  $GH pr comment "$pr" --body-file "$report"
fi
$GH pr merge "$pr" --merge --match-head-commit "$head"
