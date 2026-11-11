#!/usr/bin/env bash
# Tests scripts/merge-pr.sh on a throwaway repository with a stand-in for
# gh: which pull requests it merges and which it refuses (06 §6.5). The
# stand-in answers from JSON files with the --jq expression the script
# gives, so the expressions are tested too. What only GitHub can show (the
# shape of its answers) is left to the first merges there.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
# shellcheck source=scripts/lib/on-exit.sh
. "$root/scripts/lib/on-exit.sh"
on_exit 'rm -rf "$work"'
failed=0
fake="$work/fake"
mkdir -p "$work/bin" "$fake"

# The stand-in: FAKE holds pr.json (gh pr view), runs-<event>.json (gh run
# list), run-<id>.json and jobs-<id>.json (gh api); comments and the merge
# are appended to FAKE/done.
cat >"$work/bin/gh" <<'EOF2'
#!/usr/bin/env bash
set -euo pipefail
expr="."
args=("$@")
for i in "${!args[@]}"; do
  if [ "${args[$i]}" = --jq ]; then expr="${args[$((i + 1))]}"; fi
done
all=" $* "
case "$1 $2" in
"pr view") jq -r "$expr" "$FAKE/pr.json" ;;
"repo view") echo o/r ;;
"run list")
  [[ "$all" == *" --branch main "* && "$all" == *" --workflow ci.yml "* ]] || exit 2
  event="$(sed -n 's/.* --event \([a-z_]*\) .*/\1/p' <<<"$all")"
  jq -r "$expr" "$FAKE/runs-$event.json"
  ;;
"pr comment") echo "comment $(head -1 "${args[4]}")" >>"$FAKE/done" ;;
"pr merge") echo "merge ${args[5]}" >>"$FAKE/done" ;;
api\ *)
  path="$(printf '%s\n' "$@" | grep '^repos/o/r/actions/runs/')"
  id="$(sed -E 's#^repos/o/r/actions/runs/([0-9]+).*#\1#' <<<"$path")"
  case "$path" in
  */jobs\?per_page=100) jq -r "$expr" "$FAKE/jobs-$id.json" ;;
  *) jq -r "$expr" "$FAKE/run-$id.json" ;;
  esac
  ;;
*) exit 2 ;;
esac
EOF2
chmod +x "$work/bin/gh"

# A repository with main and its origin, which serves any commit by name
# as GitHub does.
git init -q --bare -b main "$work/origin.git"
git -C "$work/origin.git" config uploadpack.allowAnySHA1InWant true
git clone -q "$work/origin.git" "$work/repo" 2>/dev/null
repo="$work/repo"
git -C "$repo" config user.email test@example.com
git -C "$repo" config user.name test
commit() { # commit FILE adds a commit touching FILE and prints it
  mkdir -p "$(dirname "$repo/$1")"
  echo "$RANDOM$RANDOM" >>"$repo/$1"
  git -C "$repo" add "$1"
  git -C "$repo" commit -q -m "change $1"
  git -C "$repo" rev-parse HEAD
}
commit main.go >/dev/null
git -C "$repo" push -q origin main
git -C "$repo" checkout -q -b change
code="$(commit internal/x.go)"
git -C "$repo" checkout -q -b docs main
docs="$(commit docs/x.md)"
git -C "$repo" checkout -q main
git -C "$repo" push -q origin change docs
reports="$(cd "$repo" && cd "$(git rev-parse --git-common-dir)" && pwd)/ci-local"
mkdir -p "$reports"

# report COMMIT FORM-LINE writes a passing report for the commit's tree;
# without a second argument, one from before the forms.
report() {
  local tree
  tree="$(git -C "$repo" rev-parse "$1^{tree}")"
  {
    echo "ci-local PASS tree $tree"
    [ "$#" -lt 2 ] || echo "$2"
    echo
    echo "Commit $1"
  } >"$reports/$tree.md"
}

# pull HEAD CONCLUSION RUN... is the pull request: its head, and one ci-ok
# check of that conclusion per run.
pull() {
  local head="$1" conclusion="$2" checks="" run
  shift 2
  for run in "$@"; do
    checks+="{\"name\":\"ci-ok\",\"conclusion\":\"$conclusion\",\"workflowName\":\"ci\",\"detailsUrl\":\"https://github.com/o/r/actions/runs/$run/job/1\"},"
  done
  echo "{\"headRefOid\":\"$head\",\"statusCheckRollup\":[${checks%,}],\"files\":[{\"path\":\"internal/x.go\"}],\"comments\":[]}" >"$fake/pr.json"
}

# run_ ID SHA EVENT JOB=CONCLUSION... is one run and its jobs. A race or
# coverage-shard job stands for two shards, the first green, as a matrix
# does; skipped, for the one job a matrix skipped whole leaves.
run_() {
  local id="$1" sha="$2" event="$3" jobs="" j name
  shift 3
  echo "{\"head_sha\":\"$sha\",\"event\":\"$event\"}" >"$fake/run-$id.json"
  for j in "$@"; do
    name="${j%%=*}"
    j="${j#*=}"
    if [ "$j" = none ]; then j=null; else j="\"$j\""; fi
    case "$name:$j" in
    race:\"skipped\" | coverage-shard:\"skipped\") jobs+="{\"name\":\"$name\",\"conclusion\":$j}," ;;
    race:* | coverage-shard:*) jobs+="{\"name\":\"$name (1)\",\"conclusion\":\"success\"},{\"name\":\"$name (2)\",\"conclusion\":$j}," ;;
    *) jobs+="{\"name\":\"$name\",\"conclusion\":$j}," ;;
    esac
  done
  echo "{\"jobs\":[${jobs%,}]}" >"$fake/jobs-$id.json"
}

# main_runs EVENT STATUS:CONCLUSION:ID... are main's runs of an event,
# latest first.
main_runs() {
  local event="$1" runs="" r status conclusion id
  shift
  for r in "$@"; do
    IFS=: read -r status conclusion id <<<"$r"
    if [ "$conclusion" = none ]; then conclusion=null; else conclusion="\"$conclusion\""; fi
    runs+="{\"databaseId\":$id,\"status\":\"$status\",\"conclusion\":$conclusion},"
  done
  echo "[${runs%,}]" >"$fake/runs-$event.json"
}

# expect NAME WANT PATTERN [ARG...] runs merge-pr.sh 7 ARG... and checks
# whether it merged (merged, refused or usage) and that its output or what
# it did has a line matching PATTERN.
expect() {
  local name="$1" want="$2" pattern="$3" status=0 out got
  shift 3
  : >"$fake/done"
  out="$(cd "$repo" && env PATH="$work/bin:$PATH" FAKE="$fake" "$root/scripts/merge-pr.sh" 7 "$@" 2>&1)" || status=$?
  out+=$'\n'"$(cat "$fake/done")"
  case "$status:$(grep -c '^merge ' "$fake/done" || true)" in
  0:1) got=merged ;;
  1:0) got=refused ;;
  2:0) got=usage ;;
  *) got="status $status" ;;
  esac
  if [ "$got" != "$want" ]; then
    echo "FAIL $name: got $got, want $want: $out" >&2
    failed=1
  elif ! grep -Eq -- "$pattern" <<<"$out"; then
    echo "FAIL $name: got $out, want a line matching $pattern" >&2
    failed=1
  else
    echo "ok   $name"
  fi
}

green() { # main green: a push run and a full run, both succeeded
  main_runs push completed:success:1
  main_runs workflow_dispatch completed:success:2
}
all=(changes=success tracker=success short=success race=success coverage-shard=success coverage=success fuzz=success ci-ok=success)

# The full report, as before the forms.
green
pull "$code" SUCCESS 10
run_ 10 "$code" pull_request "${all[@]}"
report "$code"
expect "a report from before the forms merges as a full one" merged "^merge $code\$"
report "$code" "Form: full"
expect "a full report merges, and is posted" merged "^comment ci-local PASS tree "
run_ 10 "$code" pull_request changes=success short=success ci-ok=success
expect "a full report merges whatever jobs the run had" merged "^merge $code\$"
pull "$code" FAILURE 10
expect "a failed ci-ok refuses" refused "ci-ok on $code is FAILURE"
pull "$code" "" 10
expect "a ci-ok still running refuses" refused "ci-ok on $code is PENDING"
echo "{\"headRefOid\":\"$code\",\"statusCheckRollup\":[],\"files\":[],\"comments\":[]}" >"$fake/pr.json"
expect "no ci-ok refuses" refused "ci-ok on $code is MISSING"
pull "$code" SUCCESS 10
rm -f "${reports:?}"/*.md
expect "no report refuses" refused "no passing scripts/ci-local.sh report"
report "$code" "Form: quick"
expect "a form the script does not know refuses" refused "names a form this script does not know: Form: quick"
expect "a second argument it does not know is a usage error" usage "usage: merge-pr.sh" --force

# The light report: GitHub's run must have held the three gates.
report "$code" "Form: light (without the race suite, the coverage gate and fuzzing)"
run_ 10 "$code" pull_request "${all[@]}"
expect "a light report merges when the run held race, coverage and fuzz" merged "^merge $code\$"
run_ 10 "$code" pull_request changes=success short=success race=skipped coverage-shard=success coverage=success fuzz=success ci-ok=success
expect "a skipped race matrix refuses a light report" refused "jobs matching \^race.* are race: skipped"
run_ 10 "$code" pull_request changes=success short=success coverage-shard=success coverage=success fuzz=success ci-ok=success
expect "a run without race jobs refuses a light report" refused "jobs matching \^race.* are missing"
run_ 10 "$code" pull_request changes=success race=failure coverage-shard=success coverage=success fuzz=success ci-ok=success
expect "one failed race shard refuses a light report" refused "race \(1\): success, race \(2\): failure"
run_ 10 "$code" pull_request changes=success race=success coverage-shard=success coverage=skipped fuzz=success ci-ok=success
expect "a skipped coverage gate refuses a light report" refused "jobs matching \^coverage\\$ are coverage: skipped"
run_ 10 "$code" pull_request changes=success race=success coverage-shard=cancelled coverage=success fuzz=success ci-ok=success
expect "a cancelled coverage shard refuses a light report" refused "coverage-shard \(2\): cancelled"
run_ 10 "$code" pull_request changes=success race=success coverage-shard=success coverage=success fuzz=none ci-ok=success
expect "fuzzing unfinished refuses a light report" refused "jobs matching \^fuzz\\$ are fuzz: PENDING"
run_ 10 "$code" pull_request changes=success race=success coverage-shard=success coverage=success ci-ok=success
expect "a run without fuzzing refuses a light report" refused "jobs matching \^fuzz\\$ are missing"
expect "and says what to do" refused "run make ci-local \(the full form\)"
run_ 10 "$docs" pull_request "${all[@]}"
expect "a run of another commit refuses a light report" refused "run 10 is a run of $docs, not of $code"
run_ 10 "$code" workflow_dispatch "${all[@]}"
expect "a manual run of the commit that held the gates counts" merged "^merge $code\$"
run_ 10 "$code" pull_request "${all[@]}"
run_ 11 "$code" pull_request changes=success short=success ci-ok=success
pull "$code" SUCCESS 10 11
expect "every ci-ok run of the head must have held the gates" refused "run 11 did not hold the gate"
echo "{\"headRefOid\":\"$code\",\"statusCheckRollup\":[{\"name\":\"ci-ok\",\"conclusion\":\"SUCCESS\",\"workflowName\":\"ci\"}],\"files\":[],\"comments\":[]}" >"$fake/pr.json"
expect "a ci-ok that names no run refuses a light report" refused "no GitHub run found behind ci-ok"
pull "$code" SUCCESS 10
jq -c '.statusCheckRollup += [{name: "ci-ok", conclusion: "SUCCESS", workflowName: "ci"}]' "$fake/pr.json" >"$fake/pr2.json"
mv "$fake/pr2.json" "$fake/pr.json"
expect "one ci-ok that names no run refuses though another names a run that held the gates" refused "no GitHub run found behind ci-ok"

# Documentation alone: the suite's jobs are rightly skipped.
pull "$docs" SUCCESS 12
report "$docs" "Form: light"
run_ 12 "$docs" pull_request changes=success tracker=success short=skipped race=skipped coverage-shard=skipped coverage=skipped fuzz=skipped ci-ok=success
expect "a light report merges documentation whose gates were skipped" merged "^merge $docs\$"
pull "$code" SUCCESS 10
run_ 10 "$code" pull_request changes=success short=success race=skipped coverage-shard=skipped coverage=skipped fuzz=skipped ci-ok=success
expect "the same skips refuse a change to code" refused "race: skipped"

# The red-main rule.
run_ 10 "$code" pull_request "${all[@]}"
main_runs workflow_dispatch in_progress:none:20
expect "a full run on main in progress refuses" refused "main's latest full run \(20\) is in_progress: wait for it"
main_runs workflow_dispatch queued:none:20 completed:success:19
expect "a full run on main queued refuses, whatever the one before" refused "main's latest full run \(20\) is queued"
expect "the fix forward merges while main's full run is going" merged "^merge $code\$" --fix-forward
main_runs workflow_dispatch completed:failure:21
full=(changes=success tracker=success shards=success race=success coverage-shard=success coverage=success fuzz=success test=success signing=success release-scripts=skipped short=skipped)
run_ 21 "$code" workflow_dispatch "${full[@]/test=success/test=failure}" ci-ok=failure
expect "a red full run refuses" refused "main is red: its latest full run \(21\) ended failure \(test: failure ci-ok: failure\)"
expect "the fix forward merges on a red main" merged "--fix-forward: merging whatever main's runs say" --fix-forward
# The outage of 11 Nov: GitHub calls the run failed, ci-ok never ran. It is
# green only when every job ci-ok requires of a manual run ran and passed.
run_ 21 "$code" workflow_dispatch "${full[@]}" ci-ok=none
expect "a full run GitHub failed with ci-ok unqueued and every required job green counts as green" merged "ended failure without its ci-ok having run"
run_ 21 "$code" workflow_dispatch "${full[@]}"
expect "the same with no ci-ok job at all" merged "ended failure without its ci-ok having run"
run_ 21 "$code" workflow_dispatch "${full[@]}" ci-ok=failure
expect "a ci-ok that ran and failed is red" refused "ended failure \(ci-ok: failure\)"
run_ 21 "$code" workflow_dispatch "${full[@]}" ci-ok=cancelled
expect "a ci-ok that was cancelled is red" refused "ended failure \(ci-ok: cancelled\)"
# without JOB... is the green full run less the jobs named.
without() {
  local keep=() j drop
  for j in "${full[@]}"; do
    drop=false
    for name in "$@"; do [ "${j%%=*}" != "$name" ] || drop=true; done
    [ "$drop" = true ] || keep+=("$j")
  done
  run_ 21 "$code" workflow_dispatch "${keep[@]}" ci-ok=none
}
for job in changes tracker shards race coverage-shard coverage fuzz test; do
  without "$job"
  expect "a full run without its $job job is not green" refused "ended failure \($job: missing\)"
done
without race coverage
expect "every missing job is named" refused "\(race: missing coverage: missing\)"
without fuzz
run_ 21 "$code" workflow_dispatch "${full[@]/fuzz=success/fuzz=skipped}" ci-ok=none
expect "a required job skipped is not green" refused "ended failure \(fuzz: skipped\)"
run_ 21 "$code" workflow_dispatch "${full[@]/race=success/race=skipped}" ci-ok=none
expect "a race matrix skipped whole is not green" refused "ended failure \(race: skipped\)"
run_ 21 "$code" workflow_dispatch "${full[@]/coverage-shard=success/coverage-shard=failure}" ci-ok=none
expect "one failed coverage shard is red" refused "ended failure \(coverage-shard \(2\): failure\)"
run_ 21 "$code" workflow_dispatch "${full[@]/signing=success/signing=failure}" ci-ok=none
expect "another job failed is red" refused "ended failure \(signing: failure\)"
run_ 21 "$code" workflow_dispatch "${full[@]/test=success/test=none}" ci-ok=none
expect "a job without a conclusion is red" refused "ended failure \(test: PENDING\)"
main_runs workflow_dispatch completed:cancelled:22
run_ 22 "$code" workflow_dispatch changes=success test=cancelled race=cancelled ci-ok=failure
expect "a cancelled full run refuses: main's state is not known" refused "ended cancelled \(test: cancelled race \(2\): cancelled ci-ok: failure"
echo "[]" >"$fake/runs-workflow_dispatch.json"
expect "no full run yet merges" merged "^merge $code\$"
green
main_runs push completed:failure:1
expect "a red push run refuses" refused "its latest finished push run ended failure"
main_runs push in_progress:none:3 completed:cancelled:2 completed:success:1
expect "a push run in progress or cancelled by a newer push is passed over" merged "^merge $code\$"
main_runs push completed:cancelled:2 completed:failure:1
expect "the push run before a cancelled one still counts" refused "its latest finished push run ended failure"
echo "[]" >"$fake/runs-push.json"
expect "no push run yet merges" merged "^merge $code\$"
green

# The record and the head.
jq -c --arg body "ci-local PASS tree $(git -C "$repo" rev-parse "$code^{tree}")" '.comments = [{body: $body}]' "$fake/pr.json" >"$fake/pr2.json"
mv "$fake/pr2.json" "$fake/pr.json"
expect "a report posted already merges" merged "^merge $code\$"
if grep -q '^comment ' "$fake/done"; then
  echo "FAIL a report posted already is not posted again: it was" >&2
  failed=1
else
  echo "ok   a report posted already is not posted again"
fi
commit later.go >/dev/null
git -C "$repo" push -q origin main
expect "a head that does not contain main refuses" refused "does not contain origin/main"
finished
exit "$failed"
