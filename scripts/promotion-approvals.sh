#!/usr/bin/env bash
# Refuses a production promotion (ADR-0035, T40d4) unless someone other
# than its author approved its current head, and is listed in
# .github/approvers (product owner: our own check, no CODEOWNERS):
#   - the pull request's head is HEAD_SHA, the commit the checks judged;
#   - each reviewer's latest decisive review (an approval, changes
#     requested, a dismissal) counts, a comment changing nothing; an
#     approval counts only for the commit it was given on;
#   - the approver is not the pull request's author, and is on the list as
#     the base branch has it (the workflow runs BASE's checkout).
# It reads the pull request and its reviews with GH_TOKEN, the workflow's
# read-only token, from GITHUB_API_URL (GitHub's by default); APPROVERS
# names the list (default .github/approvers).
#   scripts/promotion-approvals.sh owner/repo 42 <head sha>
set -euo pipefail
cd "$(dirname "$0")/.."
python3 - "${1:?usage: promotion-approvals.sh REPO NUMBER HEAD_SHA}" "${2:?usage: promotion-approvals.sh REPO NUMBER HEAD_SHA}" \
  "${3:?usage: promotion-approvals.sh REPO NUMBER HEAD_SHA}" "${APPROVERS:-.github/approvers}" \
  "${GITHUB_API_URL:-https://api.github.com}" <<'PY'
import json, os, sys, urllib.request

repo, number, head, listed, api = sys.argv[1:6]
token = os.environ.get("GH_TOKEN") or sys.exit("promotion-approvals: GH_TOKEN is not set")

def fail(msg):
    sys.exit("promotion-approvals: " + msg)

def get(path):
    req = urllib.request.Request(api.rstrip("/") + path, headers={
        "Authorization": "Bearer " + token, "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28"})
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return json.load(r)
    except OSError as e:
        fail("reading %s: %s" % (path, e))

approvers = {l.strip().lower() for l in open(listed) if l.strip() and not l.lstrip().startswith("#")}
pr = get("/repos/%s/pulls/%s" % (repo, number))
author = pr["user"]["login"].lower()
if pr["head"]["sha"] != head:
    fail("pull request %s is at %s, not %s, the commit the checks judged" % (number, pr["head"]["sha"], head))

reviews, page = [], 1
while True:
    batch = get("/repos/%s/pulls/%s/reviews?per_page=100&page=%d" % (repo, number, page))
    reviews += batch
    if len(batch) < 100:
        break
    page += 1

# GitHub lists reviews oldest first: each reviewer's last decisive one stands.
latest = {}
for r in reviews:
    if r.get("user") and r["state"] in ("APPROVED", "CHANGES_REQUESTED", "DISMISSED"):
        latest[r["user"]["login"].lower()] = r
approved = sorted(who for who, r in latest.items()
                  if r["state"] == "APPROVED" and r.get("commit_id") == head and who != author and who in approvers)
if approved:
    print("promotion-approvals: %s approved %s for production" % (", ".join(approved), head))
    sys.exit()
others = sorted(approvers - {author})
fail("a production promotion needs an approval of %s by an approver other than its author %s; %s"
     % (head, author, "listed: " + ", ".join(others) if others else
        "%s lists no one else, so no production promotion can merge until a second approver is listed" % listed))
PY
