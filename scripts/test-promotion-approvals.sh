#!/usr/bin/env bash
# Tests promotion-approvals.sh (ADR-0035, T40d4) against a local stand-in
# for GitHub's API: each case serves a pull request and its reviews, and
# the check must accept or refuse it, the refusal naming its reason.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
head="$(printf "%040d" 0 | tr 0 a)" old="$(printf "%040d" 0 | tr 0 b)"
printf '# the list\nowner\nSecond\n' >"$work/approvers"
printf 'owner\n' >"$work/owner-only"

# The stand-in serves $work/pr.json and $work/reviews.json, the reviews
# paged by 100 as GitHub pages them, and needs the token.
cat >"$work/api.py" <<'PY'
import json, os, sys, urllib.parse
from http.server import BaseHTTPRequestHandler, HTTPServer
work = sys.argv[1]
class H(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.headers.get("Authorization") != "Bearer t":
            self.send_response(401); self.end_headers(); return
        url = urllib.parse.urlsplit(self.path)
        if url.path == "/repos/o/r/pulls/7":
            body = open(work + "/pr.json").read()
        elif url.path == "/repos/o/r/pulls/7/reviews":
            q = urllib.parse.parse_qs(url.query)
            page, size = int(q["page"][0]), int(q["per_page"][0])
            body = json.dumps(json.load(open(work + "/reviews.json"))[(page - 1) * size:page * size])
        else:
            self.send_response(404); self.end_headers(); return
        self.send_response(200); self.end_headers(); self.wfile.write(body.encode())
    def log_message(self, *a):
        pass
# It ends once the test that started it has.
parent = os.getppid()
s = HTTPServer(("127.0.0.1", 0), H)
s.timeout = 1
open(work + "/port", "w").write(str(s.server_port))
while os.getppid() == parent and os.path.isdir(work):
    s.handle_request()
PY
python3 "$work/api.py" "$work" &
for _ in $(seq 50); do [ -s "$work/port" ] && break; python3 -c 'import time; time.sleep(0.1)'; done
api="http://127.0.0.1:$(cat "$work/port")"
failed=0

# review LOGIN STATE COMMIT: one review, as GitHub lists it.
review() { printf '{"user": {"login": "%s"}, "state": "%s", "commit_id": "%s"}' "$1" "$2" "$3"; }
# case NAME WANT GREP AUTHOR REVIEWS...: the pull request by AUTHOR at
# $head (PR_HEAD overrides), its reviews oldest first.
case_() {
  local name="$1" want="$2" grep="$3" author="$4" got
  shift 4
  printf '{"user": {"login": "%s"}, "head": {"sha": "%s"}}' "$author" "${PR_HEAD:-$head}" >"$work/pr.json"
  (IFS=,; printf '[%s]' "$*") >"$work/reviews.json"
  if GH_TOKEN=t GITHUB_API_URL="$api" APPROVERS="${LIST:-$work/approvers}" \
    "$root/scripts/promotion-approvals.sh" o/r 7 "$head" >"$work/log" 2>&1; then got=ok; else got=refused; fi
  if [ "$got" != "$want" ] || { [ -n "$grep" ] && ! grep -q "$grep" "$work/log"; }; then
    echo "FAIL $name: got $got, want $want${grep:+ ($grep)}" >&2
    cat "$work/log" >&2
    failed=1
  else
    echo "ok   $name"
  fi
}

case_ approved ok "second approved" owner "$(review second APPROVED "$head")"
case_ login-case ok "" owner "$(review SECOND APPROVED "$head")"
case_ no-review refused "needs an approval" owner
case_ by-the-author refused "other than its author" second "$(review second APPROVED "$head")"
case_ unlisted refused "listed: second" owner "$(review stranger APPROVED "$head")"
case_ an-older-commit refused "needs an approval" owner "$(review second APPROVED "$old")"
case_ then-changes-requested refused "needs an approval" owner "$(review second APPROVED "$head")" "$(review second CHANGES_REQUESTED "$head")"
case_ then-dismissed refused "needs an approval" owner "$(review second APPROVED "$head")" "$(review second DISMISSED "$head")"
case_ then-a-comment ok "" owner "$(review second APPROVED "$head")" "$(review second COMMENTED "$head")"
case_ changes-then-approved ok "" owner "$(review second CHANGES_REQUESTED "$old")" "$(review second APPROVED "$head")"
PR_HEAD="$old" case_ head-moved refused "not $head" owner "$(review second APPROVED "$old")"
LIST="$work/owner-only" case_ owner-alone refused "until a second approver is listed" owner "$(review second APPROVED "$head")"
comments=()
for _ in $(seq 100); do comments+=("$(review third COMMENTED "$head")"); done
case_ second-page ok "second approved" owner "${comments[@]}" "$(review second APPROVED "$head")"
GH_TOKEN=wrong GITHUB_API_URL="$api" APPROVERS="$work/approvers" "$root/scripts/promotion-approvals.sh" o/r 7 "$head" >"$work/log" 2>&1 &&
  { echo "FAIL bad-token: accepted" >&2; failed=1; } || echo "ok   bad-token"
# The repository's own list is well formed: logins, one per line.
if grep -v '^#' "$root/.github/approvers" | grep -vqE '^[A-Za-z0-9-]+$'; then
  echo "FAIL approvers: .github/approvers holds a line that is not a login" >&2
  failed=1
else
  echo "ok   approvers"
fi
exit "$failed"
