#!/usr/bin/env bash
# The pilot walkthrough (make walkthrough): a Canadian company's story told
# against the real binaries, for the product owner to watch. It builds
# erp-server and erp, starts the server with the development issuer on
# DATABASE_URL (migrated on start), provisions a tenant from the mock SAP
# extract and drives everything else through the API with `erp call`, as
# the people of the story. No test harness and no mocks beyond the mock SAP
# extract and a stand-in for the model provider (internal/llm/llmtest/
# modelstub), which answers a capture's extraction as the uploaded document
# prints it, without a live model or its key. The event bus is the
# embedded NATS JetStream the tests run (natsstub), and a webhook
# subscriber on this machine (webhookstub) is the one loopback address the
# server may deliver to, named by ERP_WALKTHROUGH_RECEIVER, which only a
# development server accepts (ADR-0041 As built, product owner
# 2026-11-04). A step that needs a capability not built yet says so and is
# skipped.
#
# Each step prints a heading, the commands it runs and an excerpt of the
# answers, and checks what it expects: any unexpected answer stops the run
# with the reason, the answer and the server's last log lines and exits 1,
# as does a stop the script did not ask for: a command that failed or an
# unbound variable, in the script's own shell (scripts/lib/on-exit.sh) or
# in a command substitution whose status the caller drops (stop_unseen),
# or a step that ended without passing or skipping (review:T61j). The
# summary at the end lists the steps that passed and those skipped; it
# says none failed only when every step of the story ran.
#
#   WALKTHROUGH_PAUSE=1   wait for Enter after each step
#   WALKTHROUGH_API_ADDR, WALKTHROUGH_ADMIN_ADDR   listeners (127.0.0.1:18080, 127.0.0.1:19090)
#   WALKTHROUGH_MODEL_ADDR   the model stand-in's listener (127.0.0.1:18081)
#   WALKTHROUGH_SFTP_ADDR    the treasury system's SFTP stand-in's listener (127.0.0.1:18022)
#   WALKTHROUGH_NATS_ADDR    the event bus's listener (127.0.0.1:18222)
#   WALKTHROUGH_HOOK_ADDR    the webhook subscriber's listener (127.0.0.1:18443)
#   WALKTHROUGH_PROVIDER_ADDR   the e-invoicing provider's stand-in's listener (127.0.0.1:18444)
#   WALKTHROUGH_DIR       work directory (default: a temporary one, removed after)
#   WALKTHROUGH_BIN       directory with erp and erp-server already built (default: build them)
#   WALKTHROUGH_KEEP=1    keep the server running at the end, to look around with erp call
#   WALKTHROUGH_UNTIL     stop after this step (its function name, e.g. step_migrate);
#                         the summary says how many of the story's steps ran
#
# Needs jq, which reads the ids out of the answers, curl and PostgreSQL
# (make db-up). Every run provisions a tenant of its own; tenants are
# isolated, so runs never see each other's data. The mock SAP extract is
# the same on every run (fixed seed, January to September of this year);
# the pilot's documents are dated today, the pilot company's date in its
# time zone (scripts/walkthrough/dates.sh).
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
DATABASE_URL="${DATABASE_URL:-postgres://postgres:dev@localhost:55432/postgres?sslmode=disable}"
API_ADDR="${WALKTHROUGH_API_ADDR:-127.0.0.1:18080}"
ADMIN_ADDR="${WALKTHROUGH_ADMIN_ADDR:-127.0.0.1:19090}"
MODEL_ADDR="${WALKTHROUGH_MODEL_ADDR:-127.0.0.1:18081}"
SFTP_ADDR="${WALKTHROUGH_SFTP_ADDR:-127.0.0.1:18022}"
NATS_ADDR="${WALKTHROUGH_NATS_ADDR:-127.0.0.1:18222}"
HOOK_ADDR="${WALKTHROUGH_HOOK_ADDR:-127.0.0.1:18443}"
PROVIDER_ADDR="${WALKTHROUGH_PROVIDER_ADDR:-127.0.0.1:18444}"
PAUSE="${WALKTHROUGH_PAUSE:-0}"
# shellcheck source=walkthrough/dates.sh
. "$ROOT/scripts/walkthrough/dates.sh"
# shellcheck source=lib/on-exit.sh
. "$ROOT/scripts/lib/on-exit.sh"
TODAY=$(company_day)
# A payment run is dated TODAY, the company's day. The migration (step 3)
# loads the suppliers' bank accounts valid from the day before UTC's
# (partner.Migration's one-day exemption from the backdating control,
# ADR-0051 As built; PO 2026-11-02), never later than the company's day in
# Toronto, so a Toronto evening's run finds them, on 31 December too.
YEAR=${TODAY:0:4}
PREV=$((YEAR - 1))
CC=3000 # the pilot's SAP company code, Maple Industries Canada in the mock extract
AE=5000 # the UAE pilot company of step 41 (T62f), which SAP's extract does not hold

if [ -t 1 ]; then
  B=$'\033[1m' DIM=$'\033[2m' RED=$'\033[31m' GREEN=$'\033[32m' YELLOW=$'\033[33m' R=$'\033[0m'
else
  B='' DIM='' RED='' GREEN='' YELLOW='' R=''
fi

for tool in jq curl ssh-keygen shasum; do
  command -v "$tool" >/dev/null || { echo "walkthrough: $tool is needed (brew install $tool, apt-get install $tool)" >&2; exit 2; }
done

if [ -n "${WALKTHROUGH_DIR:-}" ]; then
  WORK=$WALKTHROUGH_DIR
else
  WORK=$(mktemp -d "${TMPDIR:-/tmp}/walkthrough.XXXXXX")
fi
mkdir -p "$WORK/data/objects" "$WORK/tokens"
WORK=$(cd "$WORK" && pwd) # absolute: the commands run in it (start)
echo '{"items": []}' >"$WORK/accounts.json"
rm -f "$WORK/failed" # an earlier run's in a WALKTHROUGH_DIR (fail)
SERVER_PID=''
MODEL_PID=''
SFTP_PID=''
NATS_PID=''
HOOK_PID=''
PROVIDER_PID=''

cleanup() {
  if [ -n "$MODEL_PID" ]; then
    kill "$MODEL_PID" 2>/dev/null || true # it may have exited already: then there is nothing to stop
    wait "$MODEL_PID" 2>/dev/null || true # its exit status after a kill says nothing about the walkthrough
  fi
  if [ -n "$SFTP_PID" ]; then
    kill "$SFTP_PID" 2>/dev/null || true # as the model stand-in
    wait "$SFTP_PID" 2>/dev/null || true # as the model stand-in
  fi
  if [ -n "$SERVER_PID" ]; then
    kill "$SERVER_PID" 2>/dev/null || true # it may have exited already: then there is nothing to stop
    wait "$SERVER_PID" 2>/dev/null || true # its exit status after a kill says nothing about the walkthrough
  fi
  # The bus and the stand-ins stop after the server, which uses them.
  for pid in "$HOOK_PID" "$PROVIDER_PID" "$NATS_PID"; do
    if [ -n "$pid" ]; then
      kill "$pid" 2>/dev/null || true # as the model stand-in
      wait "$pid" 2>/dev/null || true # as the model stand-in
    fi
  done
  if [ -z "${WALKTHROUGH_DIR:-}" ]; then rm -rf "$WORK"; fi
}
on_exit cleanup

# tax_first is the GST/HST registration's first period (step 11): the
# quarter before the latest one ended on the pilot company's date in
# Toronto, so two periods have ended and the earlier is filed first.
# Earlier periods were filed before the cutover, outside the ERP.
tax_first() {
  local today y m
  today=$(company_day)
  y=${today:0:4} m=$((10#${today:5:2}))
  m=$(((m - 1) / 3 * 3 + 1 - 6))
  if [ "$m" -lt 1 ]; then
    m=$((m + 12)) y=$((y - 1))
  fi
  printf '%s-%02d-01' "$y" "$m"
}

# uae_first is the UAE registration's first period (step 41, T61k): the
# latest quarter ended on the UAE company's date in Dubai, its cutover, so
# its return is the first the ERP files for it.
uae_first() {
  local today y m
  today=$(ZONE=Asia/Dubai company_day)
  y=${today:0:4} m=$((10#${today:5:2}))
  m=$(((m - 1) / 3 * 3 + 1 - 3))
  if [ "$m" -lt 1 ]; then
    m=$((m + 12)) y=$((y - 1))
  fi
  printf '%s-%02d-01' "$y" "$m"
}

# month_end is the last day of this month.
month_end() {
  local first="${TODAY:0:7}-01"
  if date -u -d "$first" +%F >/dev/null 2>&1; then
    date -u -d "$first +1 month -1 day" +%F
  else
    date -u -j -v+1m -v-1d -f %F "$first" +%F
  fi
}

# ---------------------------------------------------------------- output

STEPS=() RESULTS=()
CURRENT=''

heading() {
  CURRENT="$1"
  printf '\n%s━━ %s%s\n' "$B" "$1" "$R"
}

say() { printf '   %s\n' "$*"; }

# show prints a command as the reader would type it, ids shortened.
show() {
  local line="$*"
  line=$(printf '%s' "$line" | sed -E 's/([0-9a-f]{8})-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/\1…/g')
  if [ ${#line} -gt 200 ]; then line="${line:0:197}..."; fi
  printf '   %s$ %s%s\n' "$DIM" "$line" "$R"
}

# excerpt prints the last answer through a jq filter, indented; in the
# filter, acct names an account id by its number and name.
excerpt() {
  jq -r --slurpfile acc "$WORK/accounts.json" \
    'def acct: . as $id | ([$acc[0].items[] | select(.id == $id) | "\(.number) \(.name)"] | first) // $id; '"$1" \
    "$WORK/last.json" | sed 's/^/     /'
}

passed() {
  STEPS+=("$CURRENT") RESULTS+=("passed")
  printf '   %s✓ passed%s\n' "$GREEN" "$R"
  pause
}

skipped() {
  STEPS+=("$CURRENT") RESULTS+=("skipped: $1")
  printf '   %s↷ skipped: %s%s\n' "$YELLOW" "$1" "$R"
  pause
}

pause() {
  if [ "$PAUSE" = 1 ]; then
    read -r -p "   (Enter for the next step) " _ </dev/tty
  fi
}

# fail stops the run. It may run in a command substitution, whose exit the
# caller does not always see (expect "..." "$(account 4711)" ...): the mark
# it leaves in the work directory stops the run after the step (ran).
fail() {
  : >"$WORK/failed"
  printf '\n   %sUNEXPECTED in "%s":%s %s\n' "$RED" "$CURRENT" "$R" "$*" >&2
  if [ -s "$WORK/last.json" ]; then
    printf '   the last answer:\n' >&2
    head -c 3000 "$WORK/last.json" | sed 's/^/     /' >&2
    echo >&2
  fi
  if [ -s "$WORK/server.log" ]; then
    printf '   the server log, last lines:\n' >&2
    tail -n 15 "$WORK/server.log" | sed 's/^/     /' >&2
  fi
  exit 1
}

# stop_unseen makes a failed command or an unbound variable inside a
# command substitution stop the run (review:T61j). bash clears set -e in a
# substitution (macOS's bash 3.2, which runs this, has no inherit_errexit),
# and its caller sees its status only in a plain assignment, x=$(f): not in
# local x=$(f), an argument, expect "..." "$(f)" ..., or printed text,
# say "$(f)". So every failed command, here or in any subshell (set -E),
# stops the run through fail, and so does a substitution that stops with a
# status other than 0, as an unbound variable stops it
# (on_substitution_exit, scripts/lib/on-exit.sh); either leaves fail's
# mark, and ran stops the run after the step. ( ... ) and pipelines keep
# set -e, which stops them as before. A substitution whose failure is
# expected says so inside it, $(f || true), in a condition too: bash 3.2
# runs the ERR trap in $(f) || true and in if [ "$(f)" ... ], and a
# substitution ending with a status other than 0, $(grep -c x f) finding
# none, stops the run on both bash 3.2 and 5.
stop_unseen() {
  set -E
  trap 'command_failed $? "$LINENO" "$BASH_COMMAND"' ERR
  on_substitution_exit substitution_ended
}

# command_failed STATUS LINE COMMAND stops the run; once, when the command
# is a substitution's caller, x=$(f), whose f failed already.
command_failed() {
  if [ -e "$WORK/failed" ]; then exit 1; fi
  fail "the command \`$3\` failed with status $1 (line $2)"
}

# substitution_ended STATUS stops the run when a command substitution
# stopped with STATUS other than 0 and no check reported why (fail's mark).
substitution_ended() {
  if [ "$1" != 0 ] && [ ! -e "$WORK/failed" ]; then
    fail "a command substitution stopped with status $1 (an unbound variable reported above, or its last command failed)"
  fi
  exit "$1"
}

# ran STEP BEFORE checks what STEP left, BEFORE the number of results
# recorded until it began: it stops the run unless no check failed inside
# it unseen (fail) and it passed or skipped exactly once.
ran() {
  if [ -e "$WORK/failed" ]; then exit 1; fi
  if [ "${#STEPS[@]}" != $(($2 + 1)) ]; then
    fail "$1 ended having recorded $((${#STEPS[@]} - $2)) results, not one (passed or skipped)"
  fi
}

# summary STOPPED lists the steps; STOPPED names the step WALKTHROUGH_UNTIL
# stopped after, if it did.
summary() {
  local i n=0 s=0 stopped=$1 defined
  defined=$(wc -w <<<"$STORY" | tr -d ' ')
  printf '\n%s━━ Summary%s\n' "$B" "$R"
  for i in "${!STEPS[@]}"; do
    case "${RESULTS[$i]}" in
    passed)
      n=$((n + 1))
      printf '   %s✓%s %s\n' "$GREEN" "$R" "${STEPS[$i]}"
      ;;
    *)
      s=$((s + 1))
      printf '   %s↷%s %s\n     %s%s%s\n' "$YELLOW" "$R" "${STEPS[$i]}" "$DIM" "${RESULTS[$i]}" "$R"
      ;;
    esac
  done
  if [ -n "$stopped" ] && [ "${#STEPS[@]}" != "$defined" ]; then
    printf '\n   %sStopped after %s (WALKTHROUGH_UNTIL): %d of the %d steps ran, %d passed, %d skipped; the rest did not run.%s\n' \
      "$YELLOW" "$stopped" "${#STEPS[@]}" "$defined" "$n" "$s" "$R"
    return
  fi
  if [ "${#STEPS[@]}" != "$defined" ]; then fail "${#STEPS[@]} of the story's $defined steps ran"; fi
  printf '\n   %d steps passed, %d skipped, none failed.\n' "$n" "$s"
}

# ---------------------------------------------------------------- the API

# as makes the named person the caller of what follows.
as() {
  ERP_TOKEN=$(cat "$WORK/tokens/$1")
  export ERP_TOKEN
  printf '   %s(as %s)%s\n' "$DIM" "$(cat "$WORK/tokens/$1.name")" "$R"
}

# api WANT FILTER OPERATION [ARGS...] calls an operation with erp call as
# the current person: it keeps the answer in last.json and its ETag in
# ETAG, stops unless the status is WANT and prints the answer through
# FILTER ("" prints nothing). QUIET=1 leaves the command out.
api() {
  local want=$1 filter=$2 out status
  shift 2
  if [ -z "${QUIET:-}" ]; then show erp call "$@"; fi
  out=$("$ERP" call "$@" 2>"$WORK/stderr" || true) # 4xx answers exit 1: the status is checked below
  # The first line is the status and the ETag, the rest the answer.
  status=${out%%$'\n'*}
  case "$out" in *$'\n'*) printf '%s\n' "${out#*$'\n'}" >"$WORK/last.json" ;; *) : >"$WORK/last.json" ;; esac
  ETAG=''
  case "$status" in *' '*) ETAG=${status#* } ;; esac
  status=${status%% *}
  if [ "$status" != "$want" ]; then
    fail "erp call $1 answered ${status:-nothing} ($(cat "$WORK/stderr")), not $want"
  fi
  if [ -n "$filter" ]; then excerpt "$filter"; fi
}

# refused WANT CODE OPERATION [ARGS...] calls an operation that must be
# refused with the status and problem code, and prints the problem.
refused() {
  local want=$1 code=$2
  shift 2
  api "$want" '"refused: \(.status) \(.code)\n  \(.detail)"' "$@"
  expect "problem code" "$(field .code)" "$code"
}

# field reads one value of the last answer.
field() { jq -r "$1" "$WORK/last.json"; }

# expect WHAT GOT WANT stops unless GOT is WANT.
expect() {
  if [ "$2" != "$3" ]; then fail "$1: got '$2', expected '$3'"; fi
}

# expect_today LABEL DATE BEFORE checks a date the server took as Maple's
# today (a default date, a read date): the company's day read just before
# the request (BEFORE), or, when Toronto's midnight passed during it, the
# day now. Never TODAY: the script fixes that at its start, and a run that
# crosses midnight finds the server a day ahead of it (ci:batch56).
expect_today() {
  [ "$2" = "$3" ] || expect "$1" "$2" "$(company_day)"
}

# wait_op waits for the operation in the last answer to finish and stops
# unless it succeeded; the finished operation is left in last.json.
wait_op() {
  local id status i
  id=$(field .id)
  for i in $(seq 1 240); do
    QUIET=1 api 200 '' getOperation -p "id=$id"
    status=$(field .status)
    case "$status" in
    succeeded) return 0 ;;
    failed | cancelled) fail "operation $id $status" ;;
    esac
    sleep 0.25
  done
  fail "operation $id still $status after a minute (attempt $i)"
}

# approve PERSON REQUEST [COMMENT] approves an approval request as PERSON.
approve() {
  as "$1"
  QUIET=1 api 200 '' getApprovalRequest -p "id=$2"
  api 200 '"approval request \(.object_type): \(.status)"' approveApprovalRequest -p "id=$2" -if-match "$ETAG" \
    -body "{\"comment\": \"${3:-Checked.}\"}"
}

# account NUMBER is the id of an account of company $CC.
account() {
  local id
  id=$(jq -r --arg n "$1" '.items[] | select(.number == $n) | .id' "$WORK/accounts.json")
  [ -n "$id" ] || fail "no account $1 in company $CC"
  printf '%s' "$id"
}

accounts() {
  QUIET=1 api 200 '' listAccounts -p "legal_entity_id=$LE" -p page_size=200
  cp "$WORK/last.json" "$WORK/accounts.json"
}

token() {
  "$ERP" token -tenant "$TENANT" -subject "$1" >"$WORK/tokens/$1" 2>"$WORK/stderr" ||
    fail "no token for $1 ($(cat "$WORK/stderr")): is the server running with ERP_DEV_ISSUER=true?"
  printf '%s' "$2" >"$WORK/tokens/$1.name"
}

# cents AMOUNT is a two-decimal amount in whole cents, for the
# arithmetic of a statement's balances (integers, never floating point).
cents() {
  local a=$1 sign=''
  case "$a" in -*) sign=- a=${a#-} ;; esac
  printf '%s%d' "$sign" "$((10#${a%.*} * 100 + 10#${a#*.}))"
}

# amount CENTS is the two-decimal amount.
amount() {
  local c=$1 sign=''
  if [ "$c" -lt 0 ]; then sign=- c=$((-c)); fi
  printf '%s%d.%02d' "$sign" "$((c / 100))" "$((c % 100))"
}

# ---------------------------------------------------------------- start

start() {
  heading "The system under test"
  if [ -n "${WALKTHROUGH_BIN:-}" ]; then
    BIN=$(cd "$WALKTHROUGH_BIN" 2>/dev/null && pwd || true) # a missing one is reported below, once
    [ -n "$BIN" ] || fail "no directory $WALKTHROUGH_BIN"
  else
    BIN="$WORK/bin"
    say "Building erp-server and the erp command line from this checkout..."
    (cd "$ROOT" && go build -o "$BIN/erp" ./cmd/erp && go build -o "$BIN/erp-server" ./cmd/erp-server) || fail "the build failed"
  fi
  ERP="$BIN/erp"
  latest_pack
  say "Version: $(cd "$ROOT" && git describe --tags --always --dirty 2>/dev/null || echo unknown) (git describe)"
  # Files the commands name are the work directory's, as they are shown.
  cd "$WORK"
  say "Today is $TODAY, Maple's date in $ZONE: the pilot's documents are dated today."
  say ""
  say "Not built yet, so not shown:"
  say "  - the web screens: only the workspace skeleton exists, sign-in comes with T73b, the screens with T73c-T73m"
  say "  - the FPZ-500 for a Quebec registrant (the GST34's lines are signed off in Canada $CA_PACK, the UAE VAT 201"
  say "    in UAE pack 2026.5, which step 41 files): step 11 prepares,"
  say "    approves and files the GST/HST returns in order with the pack's lines, the tax agent explaining the draft and"
  say "    proposing an adjustment entry and a tax code correction before Carla prepares it"
  say "  - the liquidity forecast's SAP migration (T53k): step 13 shows the"
  say "    daily cash position with a memo record and the forecast of the supplier open items, payment runs, parked"
  say "    invoices, down payment requests and blocked suppliers; step 21 its rows by liquidity item; step 23 its opening,"
  say "    memo records and payments in transit, conversion and drill-down; step 26 its snapshots; step 27 the cash actuals;"
  say "    step 28 the forecast's variance, step 30 a person's comment on it; step 33 its purchase orders; step 35 its"
  say "    treasury connector over SFTP and its webhook; step 36 the forecast across companies in group currency; step 37 the"
  say "    actuals in analytics, step 38 the forecast, its variance and accuracy in analytics; step 39 its agent, through a"
  say "    model stand-in"
  say "  - cost center postings, reports and planning (T56d-T56j)"
  say "  - accounts receivable, the consolidated statements, eliminations and finalization (T68e-T68h)"
  say "  - natural-language questions run on a live model (erp ask); here the same question is asked as a governed query"

  # ERP_EVAL_CELL: the close agent's principal runs at level 1 before its
  # type is certified, as an evaluation cell raises its agents (ADR-0058
  # §12, ADR-0039); production refuses it.
  export DATABASE_URL ERP_ENV=development ERP_DEV_ISSUER=true ERP_MIGRATE_ON_START=true ERP_EVAL_CELL=true
  export ERP_HTTP_ADDR="$API_ADDR" ERP_ADMIN_ADDR="$ADMIN_ADDR"
  export ERP_API_URL="http://$API_ADDR" ERP_ADMIN_URL="http://$ADMIN_ADDR" ERP_CONTROL_URL="http://$ADMIN_ADDR"
  export ERP_EXTRACT_DIR="$WORK/data" ERP_OBJECT_DIR="$WORK/data/objects"
  model_stub
  bus_stub
  hook_stub
  provider_stub
  unset ERP_PACKS_DIR # the released packs alone
  ERP_PAGE_TOKEN_KEY=$(head -c 32 /dev/urandom | base64)
  ERP_FIELD_KEY=$(head -c 32 /dev/urandom | base64)
  export ERP_PAGE_TOKEN_KEY ERP_FIELD_KEY
  if curl -s -o /dev/null "http://$API_ADDR/" || curl -s -o /dev/null "http://$ADMIN_ADDR/"; then
    fail "something already listens on $API_ADDR or $ADMIN_ADDR: set WALKTHROUGH_API_ADDR and WALKTHROUGH_ADMIN_ADDR"
  fi
  "$BIN/erp-server" >"$WORK/server.log" 2>&1 &
  SERVER_PID=$!
  local i
  for i in $(seq 1 240); do
    if curl -sf "http://$ADMIN_ADDR/readyz" >/dev/null; then break; fi
    kill -0 "$SERVER_PID" 2>/dev/null || fail "erp-server exited at start"
    sleep 0.25
  done
  curl -sf "http://$ADMIN_ADDR/readyz" >/dev/null || fail "erp-server is not ready after a minute ($i checks)"
  model_price
  say ""
  say "erp-server is up (development, development issuer on): API $ERP_API_URL, admin $ERP_ADMIN_URL."
  say "Its events go through the embedded event bus at $ERP_NATS_URL; webhooks reach public addresses and,"
  say "in this walkthrough alone, the subscriber on this machine at https://$HOOK_ADDR (ERP_WALKTHROUGH_RECEIVER)."
}

# stub NAME PKG sets STUB to the stand-in NAME, built from the package PKG
# unless WALKTHROUGH_BIN has it.
stub() {
  STUB="$BIN/$1"
  if [ ! -x "$STUB" ]; then
    STUB="$WORK/bin/$1"
    (cd "$ROOT" && go build -o "$STUB" "./$2") || fail "the $1 stand-in did not build"
  fi
}

# wait_file FILE PID NAME LOG HINT waits for FILE, which the stand-in NAME
# (process PID, its output in LOG) writes once it listens.
wait_file() {
  local file=$1 pid=$2 name=$3 log=$4 hint=$5 i
  for i in $(seq 1 100); do
    if [ -s "$file" ]; then return 0; fi
    kill -0 "$pid" 2>/dev/null || fail "$name exited at start ($(cat "$log")): $hint"
    sleep 0.1
  done
  fail "$name is not listening after ten seconds ($i checks)"
}

# bus_stub starts the event bus, the embedded NATS JetStream of the tests
# (natstest), and points erp-server's outbox relay at it (ERP_NATS_URL): the
# events are relayed and fanned out to webhook subscriptions (ADR-0007,
# ADR-0041).
bus_stub() {
  stub natsstub internal/platform/natsbus/natstest/natsstub
  rm -rf "$WORK/nats" "$WORK/nats.url"
  "$STUB" -addr "$NATS_ADDR" -store "$WORK/nats" -ready "$WORK/nats.url" >"$WORK/nats.log" 2>&1 &
  NATS_PID=$!
  wait_file "$WORK/nats.url" "$NATS_PID" "the event bus" "$WORK/nats.log" "is $NATS_ADDR free? set WALKTHROUGH_NATS_ADDR"
  ERP_NATS_URL=$(cat "$WORK/nats.url")
  export ERP_NATS_URL
}

# hook_stub starts the webhook subscriber (step 35) on this machine and
# names it to erp-server as the walkthrough's receiver, with the
# certificate it made (ERP_WALKTHROUGH_RECEIVER, _CA; development only). It
# verifies each delivery with the secret step 35 writes to hook/secret and
# keeps it in hook/received.
hook_stub() {
  stub webhookstub internal/platform/webhook/webhooktest/webhookstub
  rm -rf "$WORK/hook"
  mkdir -p "$WORK/hook/received"
  "$STUB" -addr "$HOOK_ADDR" -ca "$WORK/hook/receiver.pem" -secret "$WORK/hook/secret" -out "$WORK/hook/received" >"$WORK/hook.log" 2>&1 &
  HOOK_PID=$!
  wait_file "$WORK/hook/receiver.pem" "$HOOK_PID" "the webhook subscriber" "$WORK/hook.log" "is $HOOK_ADDR free? set WALKTHROUGH_HOOK_ADDR"
  export ERP_WALKTHROUGH_RECEIVER="$HOOK_ADDR" ERP_WALKTHROUGH_RECEIVER_CA="$WORK/hook/receiver.pem"
}

# provider_stub starts the e-invoicing provider's stand-in (step 43,
# ADR-0061 §3) on this machine and names it to erp-server, with the
# certificate it made (ERP_WALKTHROUGH_PROVIDER, _CA; development only). It
# serves the documents of provider/inbox to requests bearing the token in
# provider/token, which the connector's secret_ref names, and records each
# acknowledgement there.
provider_stub() {
  stub providerstub internal/einvoicing/einvoicetest/providerstub
  rm -rf "$WORK/provider"
  mkdir -p "$WORK/provider/inbox"
  head -c 24 /dev/urandom | base64 | tr -d '/+=' >"$WORK/provider/token"
  "$STUB" -addr "$PROVIDER_ADDR" -ca "$WORK/provider/provider.pem" -token "$WORK/provider/token" -inbox "$WORK/provider/inbox" >"$WORK/provider.log" 2>&1 &
  PROVIDER_PID=$!
  wait_file "$WORK/provider/provider.pem" "$PROVIDER_PID" "the e-invoicing provider's stand-in" "$WORK/provider.log" "is $PROVIDER_ADDR free? set WALKTHROUGH_PROVIDER_ADDR"
  export ERP_WALKTHROUGH_PROVIDER="$PROVIDER_ADDR" ERP_WALKTHROUGH_PROVIDER_CA="$WORK/provider/provider.pem"
}

# model_stub starts the stand-in for the model provider (step 18's capture,
# the tax agent's run in step 11, the forecast agent's in step 39, the close
# agent's in step_close) and points erp-server's model gateway at it. While
# model-script.json exists it answers each call with its next turn (the
# tax agent's, which tax_agent writes and removes, the forecast agent's,
# which step 39 writes and removes, and the close agent's, which
# close_accrual_agent and close_variance_agent write and remove);
# otherwise with the text of model-answer.json, which step 18 writes.
model_stub() {
  local stub="$BIN/modelstub"
  if [ ! -x "$stub" ]; then
    stub="$WORK/bin/modelstub"
    (cd "$ROOT" && go build -o "$stub" ./internal/llm/llmtest/modelstub) || fail "the model stand-in did not build"
  fi
  if curl -s -o /dev/null "http://$MODEL_ADDR/"; then
    fail "something already listens on $MODEL_ADDR: set WALKTHROUGH_MODEL_ADDR"
  fi
  echo '{}' >"$WORK/model-answer.json"
  "$stub" -addr "$MODEL_ADDR" -answer "$WORK/model-answer.json" -script "$WORK/model-script.json" >"$WORK/model.log" 2>&1 &
  MODEL_PID=$!
  export ERP_MODEL_API_KEY=walkthrough ERP_MODEL_ENDPOINT="http://$MODEL_ADDR"
}

# model_price records a price for the model extraction routes to, which the
# gateway needs before it forwards a call (ADR-0036): zero, the stand-in's
# cost, not a published price.
model_price() {
  printf '[{"model": "claude-opus-5-5", "effective_from": "%s", "currency": "USD", "input": "0", "output": "0", "cache_write": "0", "cache_read": "0"}]\n' \
    "$PREV-01-01" >"$WORK/prices.json"
  # Recorded once per database: a price stands, so a later run on it finds it there.
  "$BIN/erp-server" prices load -file "$WORK/prices.json" -by walkthrough >"$WORK/prices.out" 2>&1 ||
    grep -q "already recorded" "$WORK/prices.out" || fail "the model's price was not recorded ($(cat "$WORK/prices.out"))"
}

# latest_pack sets CA_PACK to the latest released Canada pack's version,
# the one step 2 applies: since T61j (2026.6) it defines the GST/HST
# return's lines, so the walkthrough serves no unreleased copy of its own
# any more (ADR-0049 §11 As built, T61h, T61j).
latest_pack() {
  CA_PACK=$(cd "$ROOT/packs/ca" && ls -d 20*.* | sort -t. -k1,1n -k2,2n | tail -1)
  [ -f "$ROOT/packs/ca/$CA_PACK/returns.yaml" ] || fail "the latest Canada pack, $CA_PACK, defines no tax return"
}

# ---------------------------------------------------------------- the people

# The permissions of everyone's day-to-day work: all but administering
# access and the organisation, activating configuration, the AI policy,
# releasing payments and deciding approvals, which the approver roles hold.
STAFF_DENIED=' identity.access.manage tenancy.org.manage config.change.activate agents.ai_policy.manage agents.model.invoke payments.run.release payments.file.transmit workflow.request.decide sales.order.credit_release '

# grant ROLE_ID PERMISSION... grants a role the permissions.
grant() {
  local role=$1 perm
  shift
  for perm in "$@"; do
    QUIET=1 api 201 '' grantRolePermission -p "id=$role" -body "{\"permission_code\": \"$perm\"}"
  done
}

# role CODE is the id of a role.
role() { jq -r --arg c "$1" '.items[] | select(.code == $c) | .id' "$WORK/roles.json"; }

people() {
  say "Ada, the administrator, sets up the team. Day-to-day work is one role, finance_staff; deciding"
  say "approvals and releasing payments belong to the roles the configuration names, given to approvers."
  local staff perm code n=0
  QUIET=1 api 200 '' listRoles -p page_size=100
  cp "$WORK/last.json" "$WORK/roles.json"
  QUIET=1 api 200 '' listRolePermissions -p "id=$(role tenant_admin)" -p page_size=200
  field '.items[].permission_code' >"$WORK/permissions"
  api 201 '"role \(.code), \(.name)"' createRole -body '{"code": "finance_staff", "name": "Finance staff"}'
  staff=$(field .id)
  while read -r perm; do
    case "$STAFF_DENIED" in *" $perm "*) continue ;; esac
    case "$perm" in *.approve) continue ;; esac
    grant "$staff" "$perm"
    n=$((n + 1))
  done <"$WORK/permissions"
  show erp call grantRolePermission -p "id=$staff" -body '{"permission_code": "..."}' "# $n permissions"
  for code in Z_PURCH_MGR process_owner controller solution_architect Z_TREASURY_MGR vendor_master_approver tax_manager; do
    # shellcheck disable=SC2046 # permission codes are single words
    grant "$(role "$code")" workflow.request.decide $(grep '\.approve$' "$WORK/permissions")
  done
  grant "$(role Z_TREASURY_MGR)" payments.run.release payments.file.transmit
  say "  the approver roles Z_PURCH_MGR, process_owner, controller, solution_architect, vendor_master_approver and"
  say "  tax_manager (who approves tax returns, ADR-0049 §11)"
  say "  decide approval requests; Z_TREASURY_MGR also releases payment runs and transmits bank files."
  QUIET=1 api 200 '' listRoles -p page_size=100
  cp "$WORK/last.json" "$WORK/roles.json"
  person ben "Ben Buyer" finance_staff
  person alex "Alex Payables" finance_staff
  person priya "Priya Purchasing Manager" finance_staff Z_PURCH_MGR process_owner
  person carla "Carla Controller" finance_staff controller vendor_master_approver
  person sam "Sam Solution Architect" finance_staff solution_architect
  person tom "Tom Treasurer" finance_staff Z_TREASURY_MGR
  person tara "Tara Treasury Manager" finance_staff Z_TREASURY_MGR
  person fiona "Fiona Finance Director" finance_staff Z_TREASURY_MGR tax_manager
}

# person SUBJECT NAME ROLE... creates a person who may act in every
# company, gives them the roles and mints their token.
person() {
  local subject=$1 name=$2 id code
  shift 2
  QUIET=1 api 201 '' createPrincipal -body "{\"kind\": \"human\", \"external_subject\": \"$subject\", \"display_name\": \"$name\", \"all_companies\": true}"
  id=$(field .id)
  for code in "$@"; do
    QUIET=1 api 201 '' createRoleAssignment -body "{\"principal_id\": \"$id\", \"role_id\": \"$(role "$code")\"}"
  done
  token "$subject" "$name"
  printf '%s\t%s\n' "$id" "$name" >>"$WORK/names"
  say "  $name ($subject): $*"
}

# ---------------------------------------------------------------- the story

step_tenant() {
  heading "Step 1 — A tenant for the Canadian pilot company, configured from its SAP system"
  say "The mock SAP extract has a US company 1000, a German 2000 and the pilot, Maple Industries Canada,"
  say "company $CC in CAD, with Canadian suppliers and Canadian house bank accounts in CAD and USD (T72d)."
  # A fixed seed and month: the mock's documents are then the same every
  # run, whatever today is (its random draws depend on -through below 6).
  show erp gen-mock-sap -out data/extract.json -year "$YEAR" -through 9 -seed 42
  "$ERP" gen-mock-sap -out "$WORK/data/extract.json" -year "$YEAR" -through 9 -seed 42 >/dev/null || fail "gen-mock-sap failed"
  jq -r '.T001[] | "     SAP company \(.BUKRS) \(.BUTXT), \(.WAERS), \(.LAND1)"' "$WORK/data/extract.json"
  say "SAP's extract has no time zone for a company code, and the configuration requires one (product owner"
  say "2026-10-31): a draft without it names the company and key to set."
  show erp config generate -source file:data/extract.json -company "$CC" -year "$YEAR" -out config.yaml
  if "$ERP" config generate -source "file:$WORK/data/extract.json" -company "$CC" -year "$YEAR" -out "$WORK/config.yaml" >"$WORK/generate.out" 2>&1; then
    fail "a draft without the company's time zone validated"
  fi
  grep -q "\[error\] companies\[$CC\]\.time_zone: time zone is required" "$WORK/generate.out" ||
    { sed 's/^/     /' "$WORK/generate.out"; fail "the draft without a time zone was not refused on companies[$CC].time_zone"; }
  grep "\[error\] companies\[$CC\]\.time_zone: time zone is required" "$WORK/generate.out" | sed 's/^ */     /'
  say "The company does business in Eastern Time: its business dates (today in its cash position) are in"
  say "America/Toronto, given as the draft is generated (T72n1)."
  say "The cutover date (-cutover) picks each account's cost element from SAP's CSKB slice valid then (A12(d))."
  show erp config generate -source file:data/extract.json -company "$CC" -year "$YEAR" -time-zone "$CC=America/Toronto" -cutover "$YEAR-01-01" -out config.yaml
  if "$ERP" config generate -source "file:$WORK/data/extract.json" -company "$CC" -year "$YEAR" -time-zone "$CC=America/Toronto" \
    -cutover "$YEAR-01-01" -out "$WORK/config.yaml" >"$WORK/generate.out" 2>&1; then
    fail "a draft with a USD house bank whose account is not revalued validated"
  fi
  say "SAP's house banks (T012K) are Canadian: no IBAN, each account by institution, transit and account number,"
  say "in its own currency (WAERS): MAIN in CAD and MUSD in USD."
  jq -r --arg cc "$CC" '.T012K[] | select(.BUKRS == $cc) | "     T012K \(.HBKID): bank \(.BANKS) \(.BANKL), account \(.BANKN) in \(.WAERS), IBAN \(if .IBAN == "" then "none" else .IBAN end)"' "$WORK/data/extract.json"
  grep -A 6 '^    house_bank:$' "$WORK/config.yaml" | sed 's/^/     /'
  grep -A 6 '^    house_bank:$' "$WORK/config.yaml" | grep -q 'institution_number: "003"' || fail "the house bank was not generated with its institution number"
  grep -A 9 '^    house_banks:$' "$WORK/config.yaml" | sed 's/^/     /'
  grep -A 9 '^    house_banks:$' "$WORK/config.yaml" | grep -q '^        currency: USD$' || fail "the USD house bank was not generated in USD"
  say "SAP's payment methods are set up per country (T042Z) and company code (T042E), and its bank selection (T042A)"
  say "ranks, per method and currency, the house banks that pay: the draft gives the company bank transfer, through"
  say "MAIN in CAD and MUSD in USD (ADR-0038)."
  jq -r '.T042Z[] | select(.LAND1 == "CA") | "     T042Z country \(.LAND1), method \(.ZLSCH): \(.TEXT1)"' "$WORK/data/extract.json"
  jq -r --arg cc "$CC" '.T042E[] | select(.ZBUKR == $cc) | "     T042E company \(.ZBUKR), method \(.ZLSCH)"' "$WORK/data/extract.json"
  jq -r --arg cc "$CC" '.T042A[] | select(.ZBUKR == $cc) | "     T042A method \(.ZLSCH), \(.WAERS), rank \(.RANGF): house bank \(.HBKID)"' "$WORK/data/extract.json"
  grep -A 9 '^    payment_methods:$' "$WORK/config.yaml" | sed 's/^/     /'
  expect "the generated method's ranking" "$(grep -A 9 '^    payment_methods:$' "$WORK/config.yaml" | grep -cE '^ +- (MAIN|MUSD)$')" 2
  say "SAP's cost element categories (CSKB-KATYP; T56h2) give the accounts' cost elements: sales revenue a revenue"
  say "element (11), the expenses primary costs (01), and the exchange rate accounts, no cost elements in SAP, none,"
  say "so their lines take no cost object (ADR-0050 §4)."
  jq -r '.CSKB[] | select(.KSTAR == "0000400000" or .KSTAR == "0000510000") | "     CSKB \(.KOKRS)/\(.KSTAR): KATYP \(.KATYP)"' "$WORK/data/extract.json"
  grep -E -A 3 '^      - number: "(400000|510000|680000)"$' "$WORK/config.yaml" | grep -E 'number|cost_element' | sed 's/^/     /'
  local account element
  for account in 400000:revenue 510000:primary_cost 680000:none; do
    element=$(awk -v n="      - number: \"${account%%:*}\"" '$0 == n { f = 1; next } f && /^      - number:/ { exit } f && /cost_element:/ { print $2; exit }' "$WORK/config.yaml")
    expect "account ${account%%:*}'s cost element from KATYP" "$element" "${account#*:}"
  done
  default_assignments
  say "The USD account's ledger account must be revalued at month end (ADR-0042), which SAP's extract cannot"
  say "tell, so the draft names it and does not validate until a reviewer flags it with its gain and loss accounts:"
  grep "\[error\]" "$WORK/generate.out" | sed 's/^ */     /'
  grep -q "\[error\] companies\[$CC\]\.house_banks\[MUSD\]\.gl_account: house bank MUSD is in USD: account 102000 must be revalue_foreign_currency" "$WORK/generate.out" ||
    { sed 's/^/     /' "$WORK/generate.out"; fail "the draft did not ask for the USD bank's account to be revalued"; }
  expect "the draft's validation errors" "$(grep -c '\[error\]' "$WORK/generate.out")" 1
  say "SAP also holds an open supplier invoice in USD (ACDOCA RWCUR), which loads in USD: the reviewer has the payables"
  say "account 211000 revalued too, its differences on a payables valuation account 211099 (ADR-0042)."
  jq -r --arg cc "$CC" '.ACDOCA[] | select(.RBUKRS == $cc and .RWCUR != null and .RWCUR != "CAD" and .LIFNR != "" and .AUGBL == "")
    | "     ACDOCA \(.BELNR)/\(.DOCLN): account \(.RACCT), vendor \(.LIFNR), \(.WSL) \(.RWCUR) (\(.HSL) CAD)"' "$WORK/data/extract.json"
  awk '{ print } /^      - number: "102000"$/ { usd = 1 } usd && /^        type: asset$/ { print "        revalue_foreign_currency: true"; usd = 0
      print "      - number: \"211099\"\n        name: Payables valuation\n        type: liability" }
    /^    fiscal_year_start_month: 1$/ { print "    fx_valuation:\n      accounts:\n        - {account: \"102000\", unrealised_gain: \"480000\", unrealised_loss: \"680000\"}"
      print "        - {account: \"211000\", adjustment: \"211099\", unrealised_gain: \"480000\", unrealised_loss: \"680000\"}" }' \
    "$WORK/config.yaml" >"$WORK/config.tmp"
  mv "$WORK/config.tmp" "$WORK/config.yaml"
  show erp config validate -file config.yaml
  "$ERP" config validate -file "$WORK/config.yaml" >"$WORK/validate.out" 2>&1 || { sed 's/^/     /' "$WORK/validate.out"; fail "the reviewed configuration does not validate"; }
  grep -q 'Validation: no findings' "$WORK/validate.out" || { sed 's/^/     /' "$WORK/validate.out"; fail "the reviewed configuration has findings"; }
  say "  with accounts 102000 and 211000 revalued into 480000 and 680000, the configuration validates with no findings."
  say "A second ledger, 2L for local GAAP (ASPE), is added beside the leading IFRS ledger 0L before"
  say "provisioning: parallel ledgers are set up while the company has no postings (ADR-0045)."
  awk '{ print } /^    fiscal_year_start_month: 1$/ { print "    ledgers: [\"0L\", \"2L\"]" }' "$WORK/config.yaml" >"$WORK/config.tmp"
  mv "$WORK/config.tmp" "$WORK/config.yaml"
  cat >>"$WORK/config.yaml" <<'YAML'
ledgers:
  - {code: "0L", name: Leading ledger (IFRS), accounting_principle: IFRS}
  - {code: "2L", name: Local GAAP (ASPE), accounting_principle: ASPE}
YAML
  grep -q 'ledgers: \["0L", "2L"\]' "$WORK/config.yaml" || fail "the company's ledgers were not added to the configuration"
  grep -q '^    time_zone: America/Toronto$' "$WORK/config.yaml" || fail "the company's time zone is not in the configuration"
  show erp-server provision -name "Maple pilot" -region ca-central -currency CAD -admin-subject ada -admin-name "Ada Admin" -config config.yaml
  "$BIN/erp-server" provision -name "Maple pilot" -region ca-central -currency CAD -admin-subject ada -admin-name "Ada Admin" \
    -config "$WORK/config.yaml" >"$WORK/last.json" 2>"$WORK/stderr" || fail "provision failed: $(cat "$WORK/stderr")"
  excerpt "\"tenant \\(.tenant_id), company $CC is legal entity \\(.legal_entities[\"$CC\"]), configuration version \\(.configuration_version)\""
  TENANT=$(field .tenant_id)
  LE=$(field ".legal_entities[\"$CC\"]")
  printf '%s\tAda Admin\n' "$(field .administrator_id)" >"$WORK/names"
  token ada "Ada Admin"
  as ada
  api 200 '.items[] | "\(.code) \(.name): \(.country_code), \(.local_currency), \(.status)"' listLegalEntities
  expect "company $CC's currency" "$(field '.items[0].local_currency')" CAD
  api 200 '.items[] | "ledger \(.code) \(.name)\(if .is_leading then ", leading" else "" end)"' listLedgers
  expect "ledgers" "$(field '[.items[].code] | sort | join(" ")')" "0L 2L"
  say "The house banks loaded from SAP, each in its account's currency:"
  api 200 '.items[] | "house bank \(.code): \(.currency), account ending \(.account_last4)"' listHouseBanks -p "legal_entity_id=$LE"
  expect "the house banks' currencies" "$(field '[.items[] | "\(.code) \(.currency)"] | sort | join(", ")')" "MAIN CAD, MUSD USD"
  expect "the USD account's last four digits" "$(field '.items[] | select(.code == "MUSD") | .account_last4')" 2346
  say "And its payment methods from SAP, in the active configuration:"
  api 200 '.items[0].document.companies[] | select(.code == "'"$CC"'") | .payment_methods[] |
    "payment method \(.code) \(.description): \([.ranking[] | "\(.currency) through \(.house_banks | join(", "))"] | join("; "))"' \
    listConfigVersions -p is_active=true
  expect "the company's payment method rankings" \
    "$(field '[.items[0].document.companies[] | select(.code == "'"$CC"'") | .payment_methods[] | .code as $m | .ranking[] | "\($m) \(.currency) \(.house_banks | join(" "))"] | join(", ")')" \
    "T CAD MAIN, T USD MUSD"
  people
  passed
}

# default_assignments shows SAP's default account assignments (OKB9:
# TKA3A with its details TKA3C and TKA3P; T56h3) as the draft step 1
# generated notes them: the general assignment of salaries to cost center
# 3100 is the entry to propose once the migration has loaded 3100 (the
# ledger activates a default only for a cost center it holds, ADR-0050
# §4), so it is not in the tenant's first configuration; every other row
# is noted with why. migrated_default_assignment proposes it in step 3.
default_assignments() {
  say "SAP's default account assignments (OKB9, TKA3A) give the company's cost elements a cost center, an order or"
  say "a profit center, or say SAP finds the object per valuation area, business area or profit center (REGEL"
  say "1, 2, 3; TKA3C and TKA3P). The platform keeps one default cost center per company and account (T56h3):"
  jq -r --arg cc "$CC" '.TKA3A[] | select(.BUKRS == $cc) | "     TKA3A \(.KOKRS)/\(.KSTAR): \([(.KOSTL // empty | "cost center \(.)"),
      (.AUFNR // empty | "order \(.)"), (.PRCTR // empty | "profit center \(.)"), (.REGEL // empty | "REGEL \(.)"), (.BSSUBST // empty | "BSSUBST \(.)")] | join(", "))"' \
    "$WORK/data/extract.json"
  say "The draft notes each row. Salaries' general assignment to cost center 3100, which the migration loads, is the"
  say "entry to propose once 3100 is migrated, as the ledger activates a default only for a cost center it holds;"
  say "every other row is noted with why, and TKA3A's own cost center is never taken when REGEL is set:"
  grep "companies\[$CC\]\.default_assignments" "$WORK/generate.out" | sed 's/^ */     /'
  if grep -q 'default_assignments' "$WORK/config.yaml"; then fail "the draft holds default assignments before their cost centers are migrated"; fi
  expect "the notes on rows not proposed" "$(grep -c "companies\[$CC\]\.default_assignments\[[0-9]*\]: TKA3A .*; not proposed$" "$WORK/generate.out")" 8
  grep -q "default_assignments\[540000\]: .*REGEL 3, .*TKA3P profit center P3100: cost center 3120; TKA3P profit center P3200: cost center 3200).*TKA3A's own cost center 3120 is not taken as a fallback" \
    "$WORK/generate.out" || fail "the REGEL 3 row's note does not name its TKA3P rows"
  sed -nE "s/^ *companies\[$CC\]\.default_assignments\[[0-9]+\]: propose after the migration: \{account: \"([0-9]+)\", cost_center: \"([^\"]+)\"\}.*/\1 \2/p" \
    "$WORK/generate.out" >"$WORK/default_assignment"
  expect "the default to propose after the migration" "$(cat "$WORK/default_assignment")" "610000 3100"
}

# migrated_default_assignment proposes the default the draft of step 1
# noted (T56h3), now that the run has loaded its cost center: a
# posting-related change (tier 3), replayed and approved by the process
# owner and the controller, after which the active configuration gives
# account 610000 cost center 3100 as SAP's OKB9 did.
migrated_default_assignment() {
  local account center base cr request
  read -r account center <"$WORK/default_assignment"
  as ada
  say "With cost center $center migrated, Ada proposes the default the draft noted in step 1: account $account takes"
  say "cost center $center when a line names none, as SAP's OKB9 had it (T56h3). It is posting-related (tier 3)."
  QUIET=1 api 200 '' listConfigVersions -p is_active=true
  base=$(field '.items[0].number')
  jq --arg cc "$CC" --arg a "$account" --arg c "$center" --argjson base "$base" '{title: "Default cost center of account \($a) from SAP",
    rationale: "OKB9 (TKA3A), proposed once the migration loaded cost center \($c) (T56h3)", base_version: $base,
    proposed: (.items[0].document | .companies |= map(if .code == $cc then .default_assignments = ((.default_assignments // []) + [{account: $a, cost_center: $c}]) else . end))}' \
    "$WORK/last.json" >"$WORK/default.json"
  api 201 '"change request \(.number), \(.status): \(.title)"' createChangeRequest -body @default.json
  cr=$(field .id)
  api 200 '"\(.status), tier \(.tier): \([.items[].path] | join(", "))"' submitChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "the change's tier" "$(field .tier)" 3
  QUIET=1 api 201 '' createRegressionSuite -body '{"name": "Before the default cost center", "window_days": 366}'
  QUIET=1 api 200 '' getChangeRequest -p "id=$cr"
  api 202 '"\(.kind) \(.status)"' simulateChangeRequest -p "id=$cr" -if-match "$ETAG"
  wait_op
  api 200 '.evidence[] | "evidence \(.kind): \(if .passed then "passed" else "FAILED" end), \(.summary)"' getChangeRequest -p "id=$cr"
  expect "status after the evidence" "$(field .status)" pending_approval
  request=$(field .workflow_request_id)
  approve priya "$request" "SAP's default cost center for salaries"
  approve carla "$request" "Cost center $center is migrated and open for actual costs"
  as ada
  QUIET=1 api 200 '' getChangeRequest -p "id=$cr"
  api 200 '"\(.status): configuration version \(.activated_version)"' activateChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "status after activation" "$(field .status)" active
  api 200 '.items[0].document.companies[] | select(.code == "'"$CC"'") | .default_assignments[] | "default assignment: account \(.account), cost center \(.cost_center)"' \
    listConfigVersions -p is_active=true
  expect "the active default assignments" \
    "$(field '[.items[0].document.companies[] | select(.code == "'"$CC"'") | .default_assignments[] | "\(.account) \(.cost_center)"] | join(", ")')" "$account $center"
}

step_migrate() {
  heading "Step 3 — The company's SAP data is migrated and reconciled"
  as ada
  say "The periods of $YEAR are opened in both ledgers, for SAP's documents and the pilot's."
  local p
  QUIET=1 api 200 '' listFiscalPeriods -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR" -p page_size=100
  cp "$WORK/last.json" "$WORK/periods.json"
  for p in $(jq -r '.items[] | select(.status == "closed" and (.is_special | not)) | "\(.id)/\(.version)"' "$WORK/periods.json"); do
    QUIET=1 api 200 '' openFiscalPeriod -p "id=${p%/*}" -if-match "\"${p#*/}\""
  done
  show erp call openFiscalPeriod -p "id=..." -if-match '"1"' "# $(jq '[.items[] | select(.status == "closed" and (.is_special | not))] | length' "$WORK/periods.json") periods"
  api 201 '"connector \(.id): \(.kind), \(.name)"' createConnector -body '{"kind": "sap_file", "name": "SAP PRD", "settings": {"path": "extract.json"}}'
  local connector
  connector=$(field .id)
  migration_group_company
  parallel_run_extract
  small_item_refused "$connector"
  yen_item_rehearsed "$connector"
  balances_by_partner_rehearsed "$connector"
  say "A rehearsal first: load, reconcile and roll back."
  show erp migrate -connector "$connector" -legal-entities "$LE" -year "$YEAR" -rehearsal
  "$ERP" migrate -connector "$connector" -legal-entities "$LE" -year "$YEAR" -rehearsal >"$WORK/rehearsal.out" 2>&1 ||
    { sed 's/^/     /' "$WORK/rehearsal.out"; fail "the rehearsal did not reconcile"; }
  grep -E '^Migration run' "$WORK/rehearsal.out" | sed 's/^/     /'
  say "Then the load itself, which fails unless the run reconciles:"
  show erp migrate -connector "$connector" -legal-entities "$LE" -year "$YEAR"
  "$ERP" migrate -connector "$connector" -legal-entities "$LE" -year "$YEAR" >"$WORK/migrate.out" 2>&1 ||
    { sed 's/^/     /' "$WORK/migrate.out"; fail "the migration did not reconcile"; }
  sed 's/^/     /' "$WORK/migrate.out"
  grep -q ': reconciled$' "$WORK/migrate.out" || fail "the migration report does not say reconciled"
  for check in 'Trial balance: accounts with differences' 'Open payables total' 'Open payables: vendors with differences' \
    'Open payable items' 'Open payable items: vendors with differences' 'Parked supplier invoices' 'Parked supplier credit memos' 'Parked subsequent debits' 'Parked subsequent credits' \
    'Vendor bank accounts: vendors with differences' 'Quantities invoiced in SAP: order items with differences' \
    'Open down payments total' 'Open down payments: vendors with differences' 'Open items in USD total' 'Open items in USD: vendors with differences' \
    'Open items fixing payment method T' 'Open items fixing payment method T total' 'Open items fixing payment method T: vendors with differences' \
    'Open payables by trading partner total' 'Open payables by trading partner: trading partners with differences' \
    'Account balances by trading partner total' 'Account balances by trading partner: balances with differences' \
    'Open GR/IR total' 'Open GR/IR in USD total' 'Open GR/IR: order items with differences' \
    'Cost centers' 'Cost centers: centers with differences' 'Profit centers' 'Profit centers: centers with differences' \
    'Cost center totals: centers with differences' 'Profit center totals: centers with differences' 'Segment totals: segments with differences'; do
    grep "$check" "$WORK/migrate.out" | grep -q 'PASS$' || fail "the reconciliation check '$check' did not pass"
  done
  delta_migration "$connector"
  say "Each run names its connector by name beside its id, so a list of runs needs no call per run:"
  api 200 '.items[] | "migration run \(.mode)\(if .is_rehearsal then " rehearsal" else "" end) from \(.connector_name): \(.status)"' listMigrationRuns -p page_size=10
  expect "the connector the migration runs name" "$(field '[.items[].connector_name] | unique | join(" ")')" "SAP PRD"
  say "The trial balance and the open payables agree with SAP's, recomputed from the raw extract, and so"
  say "does each supplier's bank account. A Canadian supplier's account came over with its bank (T72d):"
  say "scheme ca, the institution and transit numbers from SAP's bank key (LFBK-BANKL), the account sealed."
  api 200 '.items[] | "supplier \(.number) \(.name), \(.country_code)"' listBusinessPartners -p name_prefix=Laurentian
  local laurentian
  laurentian=$(field '.items[0].id')
  if [ -z "$laurentian" ] || [ "$laurentian" = null ]; then fail "the migrated supplier Laurentian Paper is not there"; fi
  say "A journal line names its partner by id; the workspace names it by the partners' ids (T73h2):"
  api 200 '.items[] | "partner \(.id | .[0:8])... is \(.number) \(.name)"' listBusinessPartners -p "ids=$laurentian"
  expect "the supplier named by its id" "$(field '.items[0].name')" "Laurentian Paper"
  api 200 '.items[] | "bank account \(.purpose): \(.account_scheme), bank \(.bank_country_code) \(.institution_number)-\(.transit_number), account ending \(.account_last4), valid from \(.valid_from)"' \
    listBankAccounts -p "id=$laurentian"
  expect "the Canadian account's scheme and bank" "$(field '.items[0] | "\(.account_scheme) \(.institution_number) \(.transit_number)"')" "ca 001 00010"
  say "Its payment methods came over from its company data (LFB1-ZWELS), each a method of the company's (step 1):"
  api 200 '.supplier_companies[] | "company data: payment terms \(.payment_term_code), payment methods \(.payment_methods // [] | join(", "))"' \
    getBusinessPartner -p "id=$laurentian"
  expect "the migrated supplier's payment methods" "$(field '[.supplier_companies[].payment_methods // [] | .[]] | join(" ")')" T
  say "SAP's parked corrections load as their kind (RBKP-XRECH, RSEG-TBTKZ; T72b), never as invoices to pay:"
  say "amounts stay positive and the kind gives the direction; parked, they hold no open item until posted."
  local kind
  for kind in credit_memo subsequent_debit subsequent_credit; do
    api 200 '.items[] | "\(.kind) \(.document_number) \(.supplier_reference): \(.status), \(.gross_amount.amount) \(.gross_amount.currency), from \(.source), supplier \(.supplier_id)"' \
      listSupplierInvoices -p "legal_entity_id=$LE" -p "kind=$kind" -p status=parked | name_partners
    expect "parked migrated documents of kind $kind" "$(field '[.items[] | select(.source == "migration")] | length')" 1
  done
  say "SAP had invoiced those orders already, in invoices it posted, which come over as journal entries. The load"
  say "took over what they invoiced and credited on each order item (posted RSEG, by RBKP-XRECH; T72b2), and the"
  say "reconciliation agrees with SAP per order item. So Alex matches the migrated credit memo and subsequent"
  say "debit clean and posts them, with no exception to release: the credit memo credits less than SAP invoiced,"
  say "the subsequent debit revalues an item SAP invoiced."
  as alex
  local doc
  for kind in credit_memo subsequent_debit; do
    QUIET=1 api 200 '' listSupplierInvoices -p "legal_entity_id=$LE" -p "kind=$kind" -p status=parked
    doc=$(field '.items[0].id')
    api 200 "$MATCHED" matchSupplierInvoice -p "id=$doc" -if-match '"1"'
    expect "the match of the migrated $kind" "$(field .is_clean)" true
    api 200 '"\(.kind) \(.document_number) \(.supplier_reference): \(.status), journal entry \(.journal_entry_id)"' \
      postSupplierInvoice -p "id=$doc" -if-match '"1"' -body "{\"posting_date\": \"$TODAY\"}"
    expect "the migrated $kind's status" "$(field .status)" posted
  done
  migrated_trading_partner
  migrated_centers
  migrated_default_assignment
  migrated_line_centers
  migrated_down_payment
  migrated_foreign_item
  migrated_grir
  migrated_foreign_grir
  passed
}

# migration_group_company is the legal entity SAP's trading partner of
# Maple's intercompany vendor names (ADR-0059 §4 and As built, the SAP
# specialist's A2), created before the migration: the mock's Muskoka
# Office Supply is a company of the group, its vendor master naming the
# company ID (LFA1-VBUND) that SAP's company code 2000, Acme GmbH, is
# assigned to (T001-RCOMP). The German company is not migrated and has no
# configuration: its legal entity alone is what a supplier links to. It
# keeps the vendor's name in IC_NAME and the legal entity in IC_COMPANY.
migration_group_company() {
  local lifnr vbund code cal
  lifnr=$(jq -r --arg cc "$CC" '[.LFB1[] | select(.BUKRS == $cc) | .LIFNR] as $own | [.LFA1[] | select((.VBUND // "") != "" and (.LIFNR | IN($own[])))] | first | .LIFNR' "$WORK/data/extract.json")
  IC_NAME=$(jq -r --arg v "$lifnr" '.LFA1[] | select(.LIFNR == $v) | .NAME1' "$WORK/data/extract.json")
  vbund=$(jq -r --arg v "$lifnr" '.LFA1[] | select(.LIFNR == $v) | .VBUND' "$WORK/data/extract.json")
  code=$(jq -r --arg id "$vbund" '[.T001[] | select(.RCOMP == $id) | .BUKRS] | join(" ")' "$WORK/data/extract.json")
  say "One of Maple's vendors in SAP is a company of the group. Its vendor master names its trading partner, a"
  say "company ID (LFA1-VBUND), and SAP's company codes say which company code that is (T001-RCOMP):"
  jq -r --arg v "$lifnr" '.LFA1[] | select(.LIFNR == $v) | "     LFA1 \(.LIFNR) \(.NAME1): trading partner \(.VBUND)"' "$WORK/data/extract.json"
  jq -r --arg id "$vbund" '.T001[] | select(.RCOMP == $id) | "     T001 company code \(.BUKRS) \(.BUTXT): company ID \(.RCOMP)"' "$WORK/data/extract.json"
  expect "the company code of the vendor's trading partner" "$code" 2000
  say "The group's German company is not migrated yet. Ada records its legal entity, so that the migration can link"
  say "the supplier to it (ADR-0059 §4); without it the run would warn and load the supplier unlinked."
  QUIET=1 api 200 '' getLegalEntity -p "id=$LE"
  cal=$(field .fiscal_calendar_id)
  api 201 '"legal entity \(.code) \(.name): \(.country_code), \(.local_currency), \(.status)"' createLegalEntity \
    -body "{\"code\": \"$code\", \"name\": \"Acme GmbH\", \"country_code\": \"DE\", \"local_currency\": \"EUR\", \"fiscal_calendar_id\": \"$cal\"}"
  IC_COMPANY=$(field .id)
}

# migrated_trading_partner shows SAP's trading partner on the migrated
# intercompany documents (ADR-0059 §4 and As built): the supplier the load
# created for Muskoka Office Supply names the legal entity of
# migration_group_company as its group company, so the ledger gave every
# line of its migrated documents that trading partner, the vendor lines,
# which SAP's lines name it on (ACDOCA-RASSC), and the other lines of its
# invoices, which SAP left without one; the credit memo Alex just posted
# through payables carries it too; and the reconciliation compared the
# open payables per trading partner with SAP's. Then the line's own
# (migrated_line_partner).
migrated_trading_partner() {
  local lifnr documents with without
  as carla
  say "SAP's trading partner (ADR-0059 §4): the migration linked $IC_NAME to the group's company, as its vendor"
  say "master names it. The supplier shows its group company:"
  api 200 '.items[] | "supplier \(.number) \(.name): group company \(.group_company.code) \(.group_company.name)"' listBusinessPartners -p "name_prefix=$IC_NAME"
  expect "the migrated supplier's group company" "$(field '.items[0] | "\(.group_company_id) \(.group_company.code) \(.group_company.name)"')" "$IC_COMPANY 2000 Acme GmbH"
  lifnr=$(jq -r --arg n "$IC_NAME" '.LFA1[] | select(.NAME1 == $n) | .LIFNR' "$WORK/data/extract.json")
  documents=$(jq -r --arg cc "$CC" --arg v "$lifnr" '[.ACDOCA[] | select(.RBUKRS == $cc and .LIFNR == $v) | .BELNR] | unique | length' "$WORK/data/extract.json")
  with=$(jq -r --arg cc "$CC" --arg v "$lifnr" '([.ACDOCA[] | select(.RBUKRS == $cc and .LIFNR == $v) | .BELNR] | unique) as $d
    | [.ACDOCA[] | select(.RBUKRS == $cc and (.BELNR | IN($d[])) and (.RASSC // "") != "")] | length' "$WORK/data/extract.json")
  without=$(jq -r --arg cc "$CC" --arg v "$lifnr" '([.ACDOCA[] | select(.RBUKRS == $cc and .LIFNR == $v) | .BELNR] | unique) as $d
    | [.ACDOCA[] | select(.RBUKRS == $cc and (.BELNR | IN($d[])) and (.RASSC // "") == "")] | length' "$WORK/data/extract.json")
  say "SAP names the trading partner on the vendor lines alone (ACDOCA-RASSC): of the lines of its $documents documents,"
  say "$with carry it and $without do not (the expense and tax lines invoice verification posts; the SAP specialist, A2)."
  if [ "$without" -eq 0 ]; then fail "the extract's intercompany documents have no line without a trading partner"; fi
  say "The load does not copy SAP's lines: the ledger derives each line's trading partner from the supplier and gives"
  say "the document's other lines the same, as for every posting on the platform. The journal lists Maple's entries"
  say "naming company 2000, and every line of the vendor's documents carries it (the entry with a line naming none"
  say "is the sale without a vendor line, below):"
  api 200 '.items[] | "  entry \(.document_number) of \(.posting_date), \(.header_text): \(.lines | length) lines, trading partner \([.lines[] | .trading_partner.code // "none"] | unique | join(", "))"' \
    listJournalEntries -p "legal_entity_id=$LE" -p "trading_partner_id=$IC_COMPANY" -p page_size=100
  cp "$WORK/last.json" "$WORK/partner_entries.json"
  expect "the vendor's migrated documents among the entries naming company 2000" \
    "$(field '[.items[] | select(((.header_text // "") | startswith("SAP document")) and ([.lines[] | select(.partner_id)] | length) > 0)] | length')" "$documents"
  expect "the trading partners of the lines of the entries with a supplier line" \
    "$(field '[.items[] | select([.lines[] | select(.partner_id)] | length > 0) | .lines[] | .trading_partner.code // "none"] | unique | join(" ")')" 2000
  expect "the lines SAP left without a trading partner, now carrying it" \
    "$(field '[.items[] | select(((.header_text // "") | startswith("SAP document")) and ([.lines[] | select(.partner_id)] | length) > 0) | .lines | length] | add')" "$((with + without))"
  say "The reconciliation compared the open payables per trading partner, SAP's as the vendor masters give them:"
  grep -E 'Open payables by trading partner' "$WORK/migrate.out" | sed 's/^/     /'
  migrated_line_partner
  as ada
}

# balances_by_partner_rehearsed CONNECTOR rehearses the scope in balances
# mode before the load (po:2026-11-10): the opening entry groups each
# account's balance by the trading partner its SAP lines name
# (ACDOCA-RASSC), so the revenue of the mock's intercompany sale, a
# document without a vendor line, arrives as a balance with company 2000,
# and the reconciliation's balances by account and trading partner show
# it equal to SAP's. The rehearsal is rolled back; the load that follows
# is in documents mode.
balances_by_partner_rehearsed() {
  local balance
  balance=$(jq -r --arg cc "$CC" '[.ACDOCA[] | select(.RBUKRS == $cc and (.LIFNR // "") == "" and (.RASSC // "") != "") | .HSL] | join(" ")' "$WORK/data/extract.json")
  say "A cutover that takes over balances instead of documents loads one opening entry. An intercompany G/L balance"
  say "then arrives with its trading partner too. SAP's lines without a vendor that name one (ACDOCA-RASSC):"
  jq -r --arg cc "$CC" '.ACDOCA[] | select(.RBUKRS == $cc and (.LIFNR // "") == "" and (.RASSC // "") != "")
    | "     ACDOCA \(.BELNR)/\(.DOCLN) \(.XBLNR): account \(.RACCT), \(.HSL) CAD, trading partner \(.RASSC)"' "$WORK/data/extract.json"
  say "A rehearsal in balances mode groups each account's balance by the trading partner its lines name, and the"
  say "reconciliation compares the balances per account and trading partner with SAP's:"
  show erp migrate -connector "$1" -legal-entities "$LE" -year "$YEAR" -mode balances -rehearsal
  "$ERP" migrate -connector "$1" -legal-entities "$LE" -year "$YEAR" -mode balances -rehearsal >"$WORK/balances.out" 2>&1 ||
    { sed 's/^/     /' "$WORK/balances.out"; fail "the rehearsal in balances mode did not reconcile"; }
  grep -E '^Migration run|by trading partner' "$WORK/balances.out" | sed 's/^/     /'
  grep -E "Account balances by trading partner total +$balance CAD +$balance CAD +PASS$" "$WORK/balances.out" >/dev/null ||
    { sed 's/^/     /' "$WORK/balances.out"; fail "the balance by trading partner did not reconcile as $balance CAD"; }
  grep -E 'Account balances by trading partner: balances with differences +0 +0 +PASS$' "$WORK/balances.out" >/dev/null ||
    fail "the balances by trading partner differ"
}

# migrated_line_partner shows the trading partner a migrated line names
# itself (ACDOCA-RASSC; po:2026-11-10): the mock's sale of January is an
# intercompany document without a vendor line, whose revenue line names
# company 2000's company ID and whose bank line names none. The load posted
# the revenue line with that trading partner and the bank line without
# one, and the reconciliation compared the balances per account and
# trading partner with SAP's, the lines the vendor's documents gave their
# trading partner included. It reads the entries naming company 2000 that
# migrated_trading_partner listed.
migrated_line_partner() {
  local belnr sale
  belnr=$(jq -r --arg cc "$CC" '[.ACDOCA[] | select(.RBUKRS == $cc and (.LIFNR // "") == "" and (.RASSC // "") != "") | .BELNR] | unique | join(" ")' "$WORK/data/extract.json")
  say "An intercompany document without a vendor line carries its trading partner too. In SAP a sale to the group's"
  say "German company was posted between G/L accounts; its revenue line names the trading partner itself (RASSC):"
  jq -r --arg cc "$CC" --arg d "$belnr" '.ACDOCA[] | select(.RBUKRS == $cc and .BELNR == $d)
    | "     ACDOCA \(.BELNR)/\(.DOCLN) \(.XBLNR): account \(.RACCT), \(.HSL) CAD, trading partner \(if (.RASSC // "") == "" then "none" else .RASSC end)"' "$WORK/data/extract.json"
  expect "the vendor lines of SAP's document" "$(jq -r --arg cc "$CC" --arg d "$belnr" '[.ACDOCA[] | select(.RBUKRS == $cc and .BELNR == $d and (.LIFNR // "") != "")] | length' "$WORK/data/extract.json")" 0
  say "The load takes a line's own trading partner where it posts the line without a supplier, so the migrated"
  say "entry's revenue line names company 2000 and its bank line none:"
  jq -r --arg h "SAP document $belnr" '.items[] | select(.header_text == $h) | "  entry \(.document_number) of \(.posting_date), \(.header_text) (\(.reference)):",
    (.lines[] | "    line \(.line_no): \(.amount_lc.amount) \(.amount_lc.currency), trading partner \(if .trading_partner then "\(.trading_partner.code) \(.trading_partner.name)" else "none" end)")' \
    "$WORK/partner_entries.json" | sed 's/^/   /'
  sale=$(jq -r --arg h "SAP document $belnr" '[.items[] | select(.header_text == $h) | .lines[] | .trading_partner.code // "none"] | join(" ")' "$WORK/partner_entries.json")
  expect "the trading partners of the migrated sale's lines" "$sale" "none 2000"
  say "The reconciliation compared the balances per account and trading partner: SAP's lines under the trading"
  say "partner they name, or else their document's vendor line's, against the platform's journal lines:"
  grep -E 'Account balances by trading partner' "$WORK/migrate.out" | sed 's/^/     /'
}

# migrated_centers shows SAP's controlling master data as the load brought
# it over (ADR-0050 §10, T56h1): segment SEG-CAN, profit centers P3100 and
# P3200 (locked from 1 October) and four cost centers, active at once with
# SAP's time slices as their periods, each created by an applied center
# change naming the run, with no approval request of its own (the run is
# their approval), each at its leaf of SAP's groups (migrated_groups). The
# reconciliation compared them per center. Step 12 builds its structure in
# the standard hierarchies.
migrated_centers() {
  say "SAP's cost and profit centers and its segment came over first (CSKS, CEPC, FAGL_SEGM with their texts;"
  say "ADR-0050 §10): each SAP time slice a period. Cost center 3120 moved to profit center P3200 on 1 July:"
  jq -r --arg cc "$CC" '.CSKS[] | select(.BUKRS == $cc and .KOSTL == "3120") | "     CSKS \(.KOKRS)/\(.KOSTL) \(.DATAB)-\(.DATBI): \(.KOSAR), profit center \(.PRCTR)"' \
    "$WORK/data/extract.json"
  grep -E 'Cost centers|Profit centers' "$WORK/migrate.out" | sed 's/^/     /'
  as carla
  api 200 '.items[] | "segment \(.code) \(.name)"' listSegments -p code=SEG-CAN
  expect "the migrated segment" "$(field '.items | length')" 1
  api 200 '.items[] | "profit center \(.code) \(.name), responsible \(.responsible): \(.status), \([.periods[] | "\(.valid_from)..\(.valid_to // "") \(if .locked then "locked" else "open" end)"] | join(", "))"' \
    listProfitCenters -p status=active
  expect "the migrated profit centers" "$(field '[.items[] | select(.code | startswith("P3")) | .code] | sort | join(" ")')" "P3100 P3200"
  expect "P3200's lock from 1 October" "$(field '.items[] | select(.code == "P3200") | [.periods[] | "\(.valid_from) \(.locked)"] | join(", ")')" \
    "2020-01-01 false, $YEAR-10-01 true"
  api 200 '.items[] | "cost center \(.code) \(.name), responsible \(.responsible): \(.status), \([.periods[] | "\(.valid_from)..\(.valid_to // "") \(.category)"] | join(", "))"' \
    listCostCenters -p "legal_entity_id=$LE"
  expect "the migrated cost centers" "$(field '[.items[] | .code] | sort | join(" ")')" "3100 3120 3200 3300"
  expect "cost center 3100's name, its English text (CSKT)" "$(field '.items[] | select(.code == "3100") | .name')" Finance
  local cc3100 cc3120 pc3200
  cc3100=$(field '.items[] | select(.code == "3100") | .id')
  cc3120=$(field '.items[] | select(.code == "3120") | .id')
  api 200 '"cost center \(.code): \([.periods[] | "\(.valid_from)..\(.valid_to // "") profit center \(.profit_center_id | .[0:8])..."] | join(", "))"' getCostCenter -p "id=$cc3120"
  QUIET=1 api 200 '' listProfitCenters -p code=P3200
  pc3200=$(field '.items[0].id')
  QUIET=1 api 200 '' getCostCenter -p "id=$cc3120"
  expect "3120's profit center from 1 July" "$(field '.periods[1] | "\(.valid_from) \(.profit_center_id)"')" "$YEAR-07-01 $pc3200"
  say "Its creation is a center change applied by the run, with no approval request of its own:"
  api 200 '.items[] | "center change \(.kind): \(.status), approval request \(.workflow_request_id // "none"); \(.reason)"' \
    listCenterChanges -p "cost_center_id=$cc3120"
  expect "the migrated cost center's change" "$(field '.items[0] | "\(.kind) \(.status) \(.workflow_request_id // "none")"')" "create applied none"
  migrated_groups "$cc3120" "$pc3200" "$cc3100"
  as ada
}

# migrated_line_centers shows SAP's account assignment on the migrated
# journal lines (ADR-0050 §10, T56h4): the mock books payroll on cost
# center 3100 and purchases on 3120, each with its profit center and
# segment (ACDOCA RCNTR, PRCTR, SEGMENT), and the documents loaded with
# them, checked by the posting. The reconciliation compared the totals
# per cost center, profit center and segment with the center reports,
# and the cost center report's total of 3100 is SAP's, to the cent.
migrated_line_centers() {
  say "SAP's journal lines carry their cost center, profit center and segment (ACDOCA RCNTR, PRCTR, SEGMENT; T56h4):"
  jq -r --arg cc "$CC" '[.ACDOCA[] | select(.RBUKRS == $cc and (.RCNTR // "") != "")] | group_by(.RCNTR, .RACCT)[]
    | "     ACDOCA cost center \(.[0].RCNTR), profit center \(.[0].PRCTR), segment \(.[0].SEGMENT): account \(.[0].RACCT), \(length) lines"' \
    "$WORK/data/extract.json"
  say "The documents loaded with them, each checked by the posting as any entry is (ADR-0050 §5):"
  as carla
  local cc3100 cc3120 sap platform
  QUIET=1 api 200 '' listCostCenters -p "legal_entity_id=$LE"
  cc3100=$(field '.items[] | select(.code == "3100") | .id')
  cc3120=$(field '.items[] | select(.code == "3120") | .id')
  api 200 '[.items[].lines[] | select(.cost_center_code == "3120")] | first
    | "entry line \(.line_no): \(.amount.amount) \(.amount.currency), cost center \(.cost_center_code), profit center \(.profit_center_code), segment \(.segment_code)"' \
    listJournalEntries -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR" -p "cost_center_id=$cc3120" -p page_size=10
  expect "a migrated line's centers" "$(field '[.items[].lines[] | select(.cost_center_code == "3120")] | first | "\(.profit_center_code) \(.segment_code)"')" \
    "P3100 SEG-CAN"
  say "The reconciliation compared the totals per cost center, profit center and segment with the center reports:"
  grep -E 'center totals|Segment totals' "$WORK/migrate.out" | sed 's/^/     /'
  sap=$(jq -r --arg cc "$CC" '[.ACDOCA[] | select(.RBUKRS == $cc and .RCNTR == "3100") | .HSL | tonumber * 100 | round] | add' "$WORK/data/extract.json")
  api 200 '"cost center report, 3100: \([.items[] | (.debit_lc.amount | tonumber) - (.credit_lc.amount | tonumber)] | add) \(.items[0].debit_lc.currency) over \(.items | length) rows"' \
    listCostCenterBalances -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR" -p "cost_center_id=$cc3100" -p page_size=100
  platform=$(field '[.items[] | ((.debit_lc.amount | tonumber) - (.credit_lc.amount | tonumber)) * 100 | round] | add')
  expect "cost center 3100's total in cents, SAP's against the report's" "$platform" "$sap"
  as ada
}

# migrated_groups CC3120 PC3200 CC3100 shows SAP's cost and profit center
# groups as the load brought them over (ADR-0050 §10, T56h2): the standard
# hierarchy H1000 of the controlling area (TKA01-KHINR) and the profit
# centers' PH1000 below the standard hierarchies' root, each center
# created at its group, the ranges of the groups' values expanded against
# SAP's centers (3101 to 3199 holds 3120), 3100 at Canada H3000, a group
# holding it beside its subgroups (the SAP specialist's A10(d)), and the
# alternative hierarchy REGIONS, whose Quebec takes 3100 to 3299 without
# 3100 and 3300, with 3120 and 3200 in it.
migrated_groups() {
  say "SAP's cost and profit center groups came over too (sets 0101 and 0106: SETHEADER, SETNODE, SETLEAF; T56h2):"
  say "the controlling area's standard hierarchy (TKA01-KHINR) and the profit centers' below the standard hierarchies'"
  say "root, each node a change the run applied, and the groups no group includes as alternative hierarchies."
  jq -r '.TKA01[] | "     TKA01 \(.KOKRS): standard hierarchy \(.KHINR)"' "$WORK/data/extract.json"
  jq -r '.SETLEAF[] | select(.SETNAME == "H3000" or .SETNAME == "H3100" or .SETNAME == "QC")
    | "     SETLEAF \(.SETCLASS)/\(.SETNAME): \(if .VALSIGN == "I" then "include" else "exclude" end) \(.VALFROM)\(if .VALOPTION == "BT" then " to \(.VALTO)" else "" end)"' \
    "$WORK/data/extract.json"
  grep '^Loaded:' "$WORK/migrate.out" | sed 's/^/     /'
  expect "the groups loaded" "$(grep '^Loaded:' "$WORK/migrate.out" | sed 's/^Loaded: //' | jq -r '"\(.hierarchies) \(.hierarchy_nodes) \(.hierarchy_places)"')" "1 10 10"
  say "A group's values are ranges, expanded against SAP's centers: H3100's 3101 to 3199 holds 3120, and the"
  say "alternative REGIONS' Quebec takes 3100 to 3299 without 3100 and 3300. Each center was created at its group:"
  api 200 '.items[] | "\(.hierarchy_code): \([.nodes[].code] | join(" > ")) > 3120"' listCostCenterPaths -p "id=$1"
  expect "3120's places" "$(field '[.items[] | "\(.hierarchy_code):\([.nodes[].code] | join("/"))"] | sort | join(" ")')" \
    "REGIONS:REGIONS/QC STANDARD:STANDARD/H1000/H3000/H3100"
  api 200 '.items[] | "\(.hierarchy_code): \([.nodes[].code] | join(" > ")) > P3200"' listProfitCenterPaths -p "id=$2"
  expect "P3200's place" "$(field '[.items[] | "\(.hierarchy_code):\([.nodes[].code] | join("/"))"] | join(" ")')" "STANDARD:STANDARD/PH1000/PH-QC"
  say "SAP's Canada group H3000 holds cost center 3100 beside its subgroups H3100 and H3200 (KHINR and a value of H3000):"
  say "a group may hold centers and subgroups alike (the SAP specialist's answer A10(d)), and so may the platform's node."
  api 200 '.items[] | "\(.hierarchy_code): \([.nodes[].code] | join(" > ")) > 3100"' listCostCenterPaths -p "id=$3"
  expect "3100's places" "$(field '[.items[] | "\(.hierarchy_code):\([.nodes[].code] | join("/"))"] | sort | join(" ")')" \
    "REGIONS:REGIONS/ON STANDARD:STANDARD/H1000/H3000"
  QUIET=1 api 200 '' listHierarchies -p kind=cost_center -p code=REGIONS
  api 200 '"hierarchy \(.code) \(.name), standard: \(.is_standard)", (.nodes[] | "  \("  " * (.level - 1))\(.code) \(.name): \(.center_ids // [] | length) centers")' \
    getHierarchy -p "id=$(field '.items[0].id')"
  expect "Quebec's centers" "$(field '[.nodes[] | select(.code == "QC") | .center_ids // [] | length] | first')" 2
  say "Each node is a change the run applied, with no approval request of its own:"
  api 200 '.items[] | select(.kind == "node_create") | "center change \(.kind) \(.payload.code): \(.status), approval request \(.workflow_request_id // "none")"' \
    listCenterChanges -p "hierarchy_id=$(field .id)"
  expect "the alternative hierarchy's applied node changes" \
    "$(field '[.items[] | select(.kind == "node_create" and .status == "applied" and .workflow_request_id == null)] | length')" 2
}

# migrated_foreign_item shows SAP's open invoice in USD of the CAD company
# (the mock's foreignInvoice) as it loaded, in its document currency
# (ADR-0005 As built, product owner 2026-10-31): an open item of USD
# 1,000.00 worth CAD 1,350.00 as SAP booked it, its entry in USD at the
# rate SAP's amounts imply, and the reconciliation's USD controls; and
# the payment method SAP fixed on it (ZLSCH), kept with the open item
# (ADR-0038 As built), with the reconciliation's controls per method. Step
# 9 revalues it. It keeps the item's entry in FX_ENTRY.
migrated_foreign_item() {
  local lifnr name supplier
  lifnr=$(jq -r --arg cc "$CC" '[.ACDOCA[] | select(.RBUKRS == $cc and .RWCUR == "USD" and .LIFNR != "" and .AUGBL == "")] | first | .LIFNR' "$WORK/data/extract.json")
  name=$(jq -r --arg v "$lifnr" '.LFA1[] | select(.LIFNR == $v) | .NAME1' "$WORK/data/extract.json")
  say "SAP's open invoice from $name is in USD, the company's currency being CAD: each line carries its document"
  say "currency and amount (RWCUR, WSL) beside its company code amount (HSL), and the document loaded in USD,"
  say "as the platform posts an invoice in USD: an open item that a USD payment clears and month end revalues."
  QUIET=1 api 200 '' listBusinessPartners -p "name_prefix=$name"
  supplier=$(field '.items[0].id')
  api 200 '.items[] | select(.amount.currency != "CAD") | "open item \(.document_number): \(.amount.amount) \(.amount.currency), worth \(.amount_lc.amount) \(.amount_lc.currency)"' \
    listOpenItems -p "partner_id=$supplier" -p page_size=100
  expect "the open USD item of $name" \
    "$(field '[.items[] | select(.amount.currency != "CAD") | "\(.amount.amount) \(.amount.currency) \(.amount_lc.amount) \(.amount_lc.currency)"] | join(", ")')" \
    "-1000.00 USD -1350.00 CAD"
  FX_ENTRY=$(field '.items[] | select(.amount.currency == "USD") | .journal_entry_id')
  api 200 '"journal entry \(.document_number): \(.currency), \(.rate_tc_lc) \(.local_currency) per \(.currency), \([.lines[] | "\(.amount.amount) \(.amount.currency) = \(.amount_lc.amount) \(.amount_lc.currency)"] | join(" / "))"' \
    getJournalEntry -p "id=$FX_ENTRY"
  expect "the USD invoice's entry rate, SAP's" "$(field .rate_tc_lc)" 1.350000000000
  say "The reconciliation agreed in CAD and compared the USD items in USD:"
  grep -E 'Open items in USD' "$WORK/migrate.out" | sed 's/^/     /'
  say "SAP fixed the invoice's payment method on the item (ZLSCH, ADR-0038): bank transfer T, whatever order the"
  say "company's runs try their methods in. It came over with the open item, so a payment run pays the item by T"
  say "alone, in a payment of its own, as it pays an invoice entered here that fixes its method (step 7):"
  jq -r --arg cc "$CC" --arg v "$lifnr" '.ACDOCA[] | select(.RBUKRS == $cc and .LIFNR == $v and .RWCUR == "USD" and .AUGBL == "")
    | "     ACDOCA \(.BELNR)/\(.DOCLN): vendor \(.LIFNR), \(.WSL) \(.RWCUR), ZLSCH \(.ZLSCH)"' "$WORK/data/extract.json"
  api 200 '.items[] | select(.amount.currency != "CAD") | "open item \(.document_number): \(.amount.amount) \(.amount.currency), fixed payment method \(.payment_method // "none")"' \
    listOpenItems -p "partner_id=$supplier" -p page_size=100
  expect "the payment method the migrated USD item fixes" "$(field '[.items[] | select(.amount.currency != "CAD") | .payment_method // "none"] | join(", ")')" T
  say "The reconciliation counted and summed the open items fixing each method on both sides:"
  grep -E 'Open items fixing payment method' "$WORK/migrate.out" | sed 's/^/     /'
}

# parallel_run_extract splits SAP's receipt of the eight shelving units
# (T72f) in two, as SAP stood during the parallel run and at cutover: the
# extract the first run loads (extract.json) has five received on 20 June,
# their document crediting the GR/IR account 750.00; the final delta run's
# (delta.json) has three more received on 25 June, in a receipt and a
# document of their own crediting it 450.00, both lines naming the order
# item. SAP's latest vendor payment is in the delta only: the first
# extract has the invoice it pays open and no payment, so the delta run
# clears the open item the first run loaded (review:batch22); it keeps
# the payment's document number in PAID_LATER. An order SAP valued
# through GR/IR with nothing open at the first run is received on again
# in the delta (closed_order_extract). Account balances are the mock's at
# cutover.
parallel_run_extract() {
  local grir
  grir=$(jq -r --arg cc "$CC" '.T030[] | select(.BUKRS == $cc and .KTOSL == "WRX") | .KONTS' "$WORK/data/extract.json")
  jq --arg cc "$CC" --arg a "$grir" --arg d "${YEAR}0625" '
    def value(v): if startswith("-") then "-" + v else v end;
    ([.ACDOCA[] | select(.RBUKRS == $cc and .RACCT == $a)] | first) as $g
    | ([.ACDOCA[] | select(.RBUKRS == $cc and .GJAHR == $g.GJAHR and .BELNR == $g.BELNR)]) as $doc
    | ([.MSEG[] | select(.EBELN == $g.EBELN)] | first) as $m
    | .MSEG = [.MSEG[] | if .MBLNR == $m.MBLNR then .MENGE = "5" else . end] + [$m + {MBLNR: "5099999999", MENGE: "3", BUDAT: $d}]
    | .ACDOCA = [.ACDOCA[] | if .RBUKRS == $cc and .GJAHR == $g.GJAHR and .BELNR == $g.BELNR then .HSL |= value("750.00") | .WSL |= value("750.00") else . end]
      + [$doc[] | .BELNR = "0199999999" | .BUDAT = $d | .HSL |= value("450.00") | .WSL |= value("450.00")]' \
    "$WORK/data/extract.json" >"$WORK/data/delta.json" || fail "the delta extract could not be made"
  closed_order_extract "$grir"
  PAID_LATER=$(jq -r --arg cc "$CC" '[.ACDOCA[] | select(.RBUKRS == $cc and .BLART == "KZ" and .LIFNR != "" and .AUGBL == .BELNR)]
    | max_by(.BUDAT) | .BELNR' "$WORK/data/delta.json")
  [ -n "$PAID_LATER" ] && [ "$PAID_LATER" != null ] || fail "the extract has no vendor payment to load in the delta run"
  jq --arg cc "$CC" --arg p "$PAID_LATER" '.MSEG = [.MSEG[] | select(.MBLNR != "5099999999" and .MBLNR != "5099999998")]
    | .ACDOCA = [.ACDOCA[] | select(.BELNR != "0199999999" and .BELNR != "0199999997" and (.RBUKRS != $cc or .BELNR != $p))
      | if .RBUKRS == $cc and .AUGBL == $p then .AUGBL = "" else . end]' \
    "$WORK/data/delta.json" >"$WORK/data/extract.json" || fail "the parallel run's extract could not be made"
}

# small_item_refused shows the quality gate refusing an open vendor item
# with an amount in its document currency and none in company currency
# (review:batch31): SAP's extract with document 0199999990 added, a USD
# vendor item of 0.01 beside a line in CAD, so the document posts in CAD,
# where the load would leave a line of no CAD amount out; the gate refuses
# it as an open item of a document not all in its currency (an item in
# CAD of a document amount alone has a refusal of its own, tested in
# internal/integration). Its third line, a G/L line of USD 0.01 and no CAD
# amount, is no item, and the load would leave it out too: the gate refuses
# it as a line of a document amount alone (review:batch31, hsl0b). The
# rehearsal fails naming the document and both lines, and the extract is
# put back.
small_item_refused() {
  cp "$WORK/data/extract.json" "$WORK/data/extract.good.json"
  jq --arg cc "$CC" '([.ACDOCA[] | select(.RBUKRS == $cc and .LIFNR != "" and .AUGBL == "" and .RWCUR == "CAD"
      and (.UMSKZ // "") == "" and (.EBELN // "") == "")] | first) as $v
    | ([.ACDOCA[] | select(.RBUKRS == $cc and .GJAHR == $v.GJAHR and .BELNR == $v.BELNR and .LIFNR == "" and (.EBELN // "") == "")] | first) as $o
    | .ACDOCA += [($o | .BELNR = "0199999990" | .DOCLN = "000001" | .HSL = "0.00" | .WSL = "0.00" | .RWCUR = "CAD"),
      ($v | .BELNR = "0199999990" | .DOCLN = "000002" | .HSL = "0.00" | .WSL = "-0.01" | .RWCUR = "USD" | .XBLNR = "SMALL-USD"),
      ($o | .BELNR = "0199999990" | .DOCLN = "000003" | .HSL = "0.00" | .WSL = "0.01" | .RWCUR = "USD")]' \
    "$WORK/data/extract.good.json" >"$WORK/data/extract.json" || fail "the extract with a small USD item could not be made"
  say "Suppose SAP's extract held a USD vendor item of 0.01 that is 0.00 in CAD, and its USD offset on a G/L account,"
  say "in a document with a line in CAD, so the document posts in CAD. The load posts no line of no CAD amount, so"
  say "both would vanish; the quality gate refuses the run instead, naming the document and each line:"
  jq -r --arg cc "$CC" '.ACDOCA[] | select(.RBUKRS == $cc and .BELNR == "0199999990")
    | "     ACDOCA \(.BELNR)/\(.DOCLN): account \(.RACCT)\(if .LIFNR != "" then ", vendor \(.LIFNR)" else "" end), \(.WSL) \(.RWCUR) (\(.HSL) CAD)"' \
    "$WORK/data/extract.json"
  show erp migrate -connector "$1" -legal-entities "$LE" -year "$YEAR" -rehearsal
  if "$ERP" migrate -connector "$1" -legal-entities "$LE" -year "$YEAR" -rehearsal >"$WORK/small.out" 2>&1; then
    sed 's/^/     /' "$WORK/small.out"
    fail "the run with a USD item of no CAD amount was not refused"
  fi
  grep -E '^Migration run|0199999990' "$WORK/small.out" | sed 's/^/     /'
  grep -q '\[error\] ACDOCA [^ ]*/0199999990/000002: open item of SAP document 0199999990 is in document currency USD, and not every line' "$WORK/small.out" ||
    { sed 's/^/     /' "$WORK/small.out"; fail "the quality gate did not name the USD item of no CAD amount"; }
  grep -q '\[error\] ACDOCA [^ ]*/0199999990/000003: line of SAP document 0199999990 has a document amount (WSL "0.01", RWCUR "USD")' "$WORK/small.out" ||
    { sed 's/^/     /' "$WORK/small.out"; fail "the quality gate did not name the USD G/L line of no CAD amount"; }
  mv "$WORK/data/extract.good.json" "$WORK/data/extract.json"
}

# yen_item_rehearsed shows SAP's internal amount form (the pilot customer
# questionnaire's D10 planning default, po:2026-11-09): a table export
# stores every amount with two decimals, shifted by TCURX-CURRDEC, so an
# open invoice of JPY 100,000 (CAD 920.00) from Tyrell Office Products, a
# copy of the mock's USD invoice (TY-USD-1) in yen, is written "1000.00".
# A rehearsal with it converts the yen with the extract's TCURX and
# reconciles them with SAP; without TCURX the quality gate refuses the run
# naming the yen instead of loading JPY 1,000. The extract is put back.
yen_item_rehearsed() {
  cp "$WORK/data/extract.json" "$WORK/data/extract.good.json"
  jq --arg cc "$CC" '[.ACDOCA[] | select(.RBUKRS == $cc and .XBLNR == "TY-USD-1")] as $doc
    | .ACDOCA += [$doc[] | .BELNR = "0199999980" | .XBLNR = "TY-JPY-1" | .RWCUR = "JPY"
        | .HSL = (if (.HSL | startswith("-")) then "-920.00" else "920.00" end)
        | .WSL = (if (.WSL | startswith("-")) then "-1000.00" else "1000.00" end)]' \
    "$WORK/data/extract.good.json" >"$WORK/data/extract.json" || fail "the extract with a yen invoice could not be made"
  say "SAP stores every amount with two decimals and shifts a currency of other decimals by TCURX (D10, po:2026-11-09):"
  say "suppose the extract held an open invoice of JPY 100,000, CAD 920.00, which a table export writes as 1000.00."
  jq -r --arg cc "$CC" '(.TCURX[] | select(.CURRKEY == "JPY") | "     TCURX \(.CURRKEY): CURRDEC \(.CURRDEC)"),
    (.ACDOCA[] | select(.RBUKRS == $cc and .BELNR == "0199999980")
      | "     ACDOCA \(.BELNR)/\(.DOCLN): account \(.RACCT)\(if .LIFNR != "" then ", vendor \(.LIFNR)" else "" end), WSL \(.WSL) \(.RWCUR), HSL \(.HSL) CAD")' \
    "$WORK/data/extract.json"
  say "The rehearsal converts the yen with the extract's TCURX, and the reconciliation agrees with SAP in yen:"
  show erp migrate -connector "$1" -legal-entities "$LE" -year "$YEAR" -rehearsal
  "$ERP" migrate -connector "$1" -legal-entities "$LE" -year "$YEAR" -rehearsal >"$WORK/yen.out" 2>&1 ||
    { sed 's/^/     /' "$WORK/yen.out"; fail "the rehearsal with a yen invoice did not reconcile"; }
  grep -E '^Migration run|Open items in JPY' "$WORK/yen.out" | sed 's/^/     /'
  grep -E 'Open items in JPY total +-100000 JPY +-100000 JPY +PASS$' "$WORK/yen.out" >/dev/null ||
    { sed 's/^/     /' "$WORK/yen.out"; fail "the yen invoice did not reconcile as JPY 100,000"; }
  jq 'del(.TCURX)' "$WORK/data/extract.json" >"$WORK/data/extract.yen.json" || fail "the yen extract without TCURX could not be made"
  mv "$WORK/data/extract.yen.json" "$WORK/data/extract.json"
  say "Without TCURX the run cannot tell what 1000.00 yen are, so the quality gate refuses it, naming the currency:"
  show erp migrate -connector "$1" -legal-entities "$LE" -year "$YEAR" -rehearsal
  if "$ERP" migrate -connector "$1" -legal-entities "$LE" -year "$YEAR" -rehearsal >"$WORK/yen.out" 2>&1; then
    sed 's/^/     /' "$WORK/yen.out"
    fail "the run with yen and no TCURX was not refused"
  fi
  grep -E '^Migration run|\[error\] TCURX' "$WORK/yen.out" | sed 's/^/     /'
  grep -q '\[error\] TCURX : the extract.s amounts are in SAP.s internal form .*it holds no TCURX to convert the amounts in JPY (0 decimals)' "$WORK/yen.out" ||
    { sed 's/^/     /' "$WORK/yen.out"; fail "the quality gate did not name the yen without TCURX"; }
  mv "$WORK/data/extract.good.json" "$WORK/data/extract.json"
}

# closed_order_extract makes an order of the pilot's that SAP received and
# invoiced in full one valued through GR/IR (review:batch23), in both
# extracts: the first single-item order received in one receipt and
# invoiced in one posted invoice (CLOSED, its item's price CLOSED_PRICE)
# gets a receipt document debiting the item's account and crediting the
# GR/IR account (GR_IR account $1), and its invoice debits the GR/IR
# account instead of the item's, both on the order item, its GR/IR lines
# without the item's centers (T56h4); so the first run
# loads it with no position open. Its order is for one more unit, which
# SAP receives on 25 June in the delta only (receipt 5099999998, document
# 0199999997), so the delta run tops a position up the order never had.
closed_order_extract() {
  CLOSED=$(jq -r --arg cc "$CC" '. as $x | [.RBKP[] | select(.BUKRS == $cc and .RBSTAT == "5") | .BELNR] as $posted
    | [.EKKO[] | select(.BUKRS == $cc) | .EBELN as $o
      | ([$x.EKPO[] | select(.EBELN == $o)]) as $items | ([$x.MSEG[] | select(.EBELN == $o)]) as $receipts
      | ([$x.RSEG[] | select(.EBELN == $o)]) as $invoiced
      | select(($items | length) == 1 and ($receipts | length) == 1 and ($invoiced | length) == 1 and $items[0].PEINH == "1"
          and ($posted | index($invoiced[0].BELNR)) != null and $receipts[0].MENGE == $items[0].MENGE and $invoiced[0].MENGE == $items[0].MENGE)
      | $o] | first // ""' "$WORK/data/delta.json")
  [ -n "$CLOSED" ] || fail "the extract has no order received and invoiced in full to value through GR/IR"
  CLOSED_PRICE=$(jq -r --arg o "$CLOSED" '.EKPO[] | select(.EBELN == $o) | .NETPR' "$WORK/data/delta.json")
  jq --arg cc "$CC" --arg a "$1" --arg o "$CLOSED" --arg d "${YEAR}0625" '
    def neg: if startswith("-") then .[1:] else "-" + . end;
    def grir: .RACCT = $a | del(.RCNTR, .PRCTR, .SEGMENT);
    def we(belnr; date): (.BELNR = belnr | .BLART = "WE" | .XBLNR = "" | .BUDAT = date)
      | (.DOCLN = "000001"), (.DOCLN = "000002" | grir | .HSL |= neg | .WSL |= neg);
    ([.EKPO[] | select(.EBELN == $o)] | first) as $i
    | ([.MSEG[] | select(.EBELN == $o)] | first) as $m
    | ([.RSEG[] | select(.EBELN == $o)] | first) as $s
    | ([.RBKP[] | select(.BELNR == $s.BELNR and .GJAHR == $s.GJAHR)] | first) as $r
    | ([.ACDOCA[] | select(.RBUKRS == $cc and .BLART == "RE" and .XBLNR == $r.XBLNR and .RACCT == $i.SAKTO)] | first) as $x
    | ($x | .EBELN = $o | .EBELP = $i.EBELP) as $line
    | .EKPO = [.EKPO[] | if .EBELN == $o then .MENGE = ((.MENGE | tonumber) + 1 | tostring) else . end]
    | .MSEG += [$m + {MBLNR: "5099999998", MENGE: "1", BUDAT: $d}]
    | .ACDOCA = [.ACDOCA[] | if . == $x then $line | grir else . end]
      + [$line | we("0199999998"; $m.BUDAT)] + [$line | .HSL = $i.NETPR | .WSL = $i.NETPR | we("0199999997"; $d)]' \
    "$WORK/data/delta.json" >"$WORK/data/closed.json" && mv "$WORK/data/closed.json" "$WORK/data/delta.json" ||
    fail "the order received and invoiced in full could not be valued through GR/IR"
}

# delta_migration runs the final delta migration at cutover (docs/migration.md
# §4): SAP received the last three shelving units after the first run
# loaded their order, so the run loads the receipt and its document and
# tops the order item's migrated GR/IR position up to SAP's (review:batch21),
# and the open GR/IR check passes again. SAP also received one more unit
# on an order the first run loaded with no GR/IR position open
# (closed_order_extract): the run that loaded it, of the same fiscal year,
# tells the delta run its history is in the extract, so it tops that
# position up too (review:batch23). SAP also paid an invoice the first
# run loaded open: the run loads the payment and clears the invoice's open
# item with it (review:batch22), and the open payable items agree.
delta_migration() {
  say "That first run loaded SAP as it stood during the parallel run. SAP then received three more of the"
  say "shelving units before postings were frozen for cutover: a receipt and a document of their own, on an"
  say "order the platform already has. The final delta run loads what is new and tops the item's migrated"
  say "GR/IR position up to SAP's, from what the first run took over:"
  jq -r --arg cc "$CC" '.ACDOCA[] | select(.RBUKRS == $cc and .BELNR == "0199999999")
    | "     ACDOCA \(.BELNR)/\(.DOCLN) \(.BLART): account \(.RACCT) \(.HSL), order item \(.EBELN)/\(.EBELP)"' "$WORK/data/delta.json"
  say "SAP had received and invoiced order $CLOSED in full through GR/IR before the first run, so that run loaded"
  say "it with no position open; SAP then received one more unit on it. The run that loaded the order is of the"
  say "same fiscal year, so the delta run tops up a position the order never had:"
  jq -r --arg cc "$CC" --arg o "$CLOSED" '.ACDOCA[] | select(.RBUKRS == $cc and .EBELN == $o)
    | "     ACDOCA \(.BELNR)/\(.DOCLN) \(.BLART): account \(.RACCT) \(.HSL), order item \(.EBELN)/\(.EBELP)\(if .BELNR == "0199999997" then " (delta)" else "" end)"' \
    "$WORK/data/delta.json"
  say "SAP also paid an invoice the first run loaded as an open item. The payment clears both vendor lines"
  say "(AUGBL), and the delta run clears the platform's open item with the payment it loads:"
  jq -r --arg cc "$CC" --arg p "$PAID_LATER" '.ACDOCA[] | select(.RBUKRS == $cc and .AUGBL == $p)
    | "     ACDOCA \(.BELNR)/\(.DOCLN) \(.BLART): vendor \(.LIFNR) \(.HSL), cleared by \(.AUGBL)"' "$WORK/data/delta.json"
  cp "$WORK/data/delta.json" "$WORK/data/extract.json"
  show erp migrate -connector "$1" -legal-entities "$LE" -year "$YEAR"
  "$ERP" migrate -connector "$1" -legal-entities "$LE" -year "$YEAR" >"$WORK/delta.out" 2>&1 ||
    { sed 's/^/     /' "$WORK/delta.out"; fail "the delta migration did not reconcile"; }
  grep -E '^(Migration run|Loaded)|Open GR/IR|Open payable items' "$WORK/delta.out" | sed 's/^/     /'
  grep -q '"gr_ir_top_ups":2' "$WORK/delta.out" || fail "the delta run did not top up both GR/IR positions"
  grep -q '"clearings":1' "$WORK/delta.out" || fail "the delta run did not clear the invoice the first run loaded"
  local check
  for check in 'Open GR/IR total' 'Open GR/IR: order items with differences' 'Trial balance: accounts with differences' \
    'Open payables total' 'Open payable items' 'Open payable items: vendors with differences'; do
    grep "$check" "$WORK/delta.out" | grep -q 'PASS$' || fail "the delta run's check '$check' did not pass"
  done
  if grep -q "order $CLOSED" "$WORK/delta.out"; then fail "the delta run warned about order $CLOSED instead of topping it up"; fi
  say "Order $CLOSED's position, topped up by the delta run to SAP's, one unit received and not invoiced:"
  QUIET=1 api 200 '' listGrIrBalances -p "legal_entity_id=$LE"
  jq -r '.items[] | select(.migrated_quantity.value == "-1")
    | "     order \(.purchase_order_number) item \(.po_item_no): migrated \(.migrated_quantity.value), balance \(.balance.amount) \(.balance.currency)"' "$WORK/last.json"
  expect "the topped-up position of order $CLOSED" "$(jq -r '[.items[] | select(.migrated_quantity.value == "-1") | .balance.amount] | join(" ")' "$WORK/last.json")" "-$CLOSED_PRICE"
}

# migrated_grir shows the GR/IR position SAP held open for an order item it
# received and was not yet invoiced for (T72f), as the load took it over:
# the order valued through GR/IR and the position on the platform's order
# item. It keeps the order (GRIR_PO), its supplier (GRIR_SUPPLIER,
# GRIR_NAME) and SAP's balance (GRIR_BALANCE) for step 5.
migrated_grir() {
  local ebeln lifnr grir
  grir=$(jq -r --arg cc "$CC" '.T030[] | select(.BUKRS == $cc and .KTOSL == "WRX") | .KONTS' "$WORK/data/extract.json")
  ebeln=$(jq -r --arg cc "$CC" --arg a "$grir" '[.ACDOCA[] | select(.RBUKRS == $cc and .RACCT == $a) | .EBELN] | first' "$WORK/data/extract.json")
  lifnr=$(jq -r --arg o "$ebeln" '.EKKO[] | select(.EBELN == $o) | .LIFNR' "$WORK/data/extract.json")
  GRIR_NAME=$(jq -r --arg v "$lifnr" '.LFA1[] | select(.LIFNR == $v) | .NAME1' "$WORK/data/extract.json")
  say "SAP received $GRIR_NAME's shelving units on order $ebeln and has no invoice for them yet: each receipt's document"
  say "debited the item's account and credited SAP's GR/IR account (T030 WRX $grir), both lines naming the order item"
  say "(ACDOCA EBELN/EBELP):"
  jq -r --arg cc "$CC" --arg o "$ebeln" '.ACDOCA[] | select(.RBUKRS == $cc and .EBELN == $o)
    | "     ACDOCA \(.BELNR)/\(.DOCLN) \(.BLART): account \(.RACCT) \(.HSL), order item \(.EBELN)/\(.EBELP)"' "$WORK/data/extract.json"
  say "The order came over valued through GR/IR, its receipts recorded without posting them again (SAP's journal carries"
  say "them), and the open credit taken over onto the platform's order item, by the first run and topped up by the"
  say "delta run in an entry each, reconciled per order item with SAP's above. The GR/IR balances show it by number:"
  api 200 "$GRIR_POSITION" listGrIrBalances -p "legal_entity_id=$LE"
  expect "open GR/IR positions after the migration, the shelving units', the monitors' in USD and order $CLOSED's" "$(field '.items | length')" 3
  local shelving='[.items[] | select(.migrated_quantity.value == "-8")]'
  expect "the migrated positions of eight units" "$(field "$shelving | length")" 1
  GRIR_BALANCE=$(field "$shelving[0].balance.amount")
  expect "the migrated balance" "$GRIR_BALANCE" -1200.00
  GRIR_PO=$(field "$shelving[0].purchase_order_id")
  api 200 '"purchase order \(.document_number): \(.status), valued through \(.valuation)\([.items[] | ", item \(.item_no) \(.description): ordered \(.quantity.value), received \(.received_quantity.value)"] | join(""))"' \
    getPurchaseOrder -p "id=$GRIR_PO"
  expect "the migrated order's valuation" "$(field .valuation)" gr_ir
  GRIR_SUPPLIER=$(field .supplier_id)
}

# migrated_foreign_grir shows the GR/IR position SAP held open in USD for
# the CAD company's order of monitors (the mock's receivedInUSD), taken
# over in the order's currency (ADR-0034 As built, product owner
# 2026-10-31): the order in USD, its position USD 1,000.00 worth CAD
# 1,350.00 as SAP booked it, and the take-over entry in USD at the rate
# SAP's amounts imply; the reconciliation compared it in both currencies.
migrated_foreign_grir() {
  local ebeln grir
  grir=$(jq -r --arg cc "$CC" '.T030[] | select(.BUKRS == $cc and .KTOSL == "WRX") | .KONTS' "$WORK/data/extract.json")
  ebeln=$(jq -r --arg cc "$CC" '[.EKKO[] | select(.BUKRS == $cc and .WAERS == "USD") | .EBELN] | first // ""' "$WORK/data/extract.json")
  [ -n "$ebeln" ] || fail "the extract has no order in USD"
  say "SAP also received four monitors on order $ebeln, placed in USD, and has no invoice for them yet. The receipt's"
  say "document is in USD: each line carries its USD amount (RWCUR, WSL) beside its CAD amount (HSL):"
  jq -r --arg cc "$CC" --arg o "$ebeln" '.ACDOCA[] | select(.RBUKRS == $cc and .EBELN == $o)
    | "     ACDOCA \(.BELNR)/\(.DOCLN) \(.BLART): account \(.RACCT) \(.WSL) \(.RWCUR) = \(.HSL) CAD, order item \(.EBELN)/\(.EBELP)"' \
    "$WORK/data/extract.json"
  say "The order kept its currency, and its open position came over in it: SAP's USD amounts as its balance, its CAD"
  say "amounts kept beside them, so a USD invoice clears it as it clears a position the platform posted in USD:"
  QUIET=1 api 200 '' listGrIrBalances -p "legal_entity_id=$LE"
  local usd='[.items[] | select(.balance.currency == "USD")]'
  jq -r "$usd[] | \"     order \\(.purchase_order_number) item \\(.po_item_no): migrated \\(.migrated_quantity.value), balance \\(.balance.amount) \\(.balance.currency) = \\(.balance_lc.amount) \\(.balance_lc.currency)\"" "$WORK/last.json"
  expect "the migrated USD position" "$(field "$usd | map(\"\\(.migrated_quantity.value) \\(.balance.amount) \\(.balance.currency) \\(.balance_lc.amount) \\(.balance_lc.currency)\") | join(\", \")")" \
    "-4 -1000.00 USD -1350.00 CAD"
  local po
  po=$(field "$usd[0].purchase_order_id")
  api 200 '"purchase order \(.document_number): \(.currency), valued through \(.valuation)"' getPurchaseOrder -p "id=$po"
  expect "the migrated order's currency" "$(field .currency)" USD
  accounts
  api 200 '.items[] | select(.currency == "USD" and .reference == "'"$ebeln"'") | "journal entry \(.document_number): \(.currency), \(.rate_tc_lc) \(.local_currency) per \(.currency), \([.lines[] | "\(.amount.amount) \(.amount.currency) = \(.amount_lc.amount) \(.amount_lc.currency)"] | join(" / "))"' \
    listJournalEntries -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR" -p source_context=integration -p "account_id=$(account "$(sed 's/^0*//' <<<"$grir")")" -p page_size=500
  expect "the USD take-over's rate, SAP's" "$(field "[.items[] | select(.currency == \"USD\" and .reference == \"$ebeln\") | .rate_tc_lc] | join(\" \")")" 1.350000000000
}

# migrated_down_payment shows SAP's open down payment as the AP clerk sees
# it after the load (T72c): a paid down payment request of the supplier and
# its open special G/L item A, reconciled apart from the open payables. It
# keeps the supplier (DP_SUPPLIER, DP_NAME), the item and the supplier's
# ordinary open items (DP_PAYABLE, what a run owes it) for steps 7 and 16.
migrated_down_payment() {
  local lifnr owed
  lifnr=$(jq -r --arg cc "$CC" '.ACDOCA[] | select(.RBUKRS == $cc and .UMSKZ == "A" and .AUGBL == "") | .LIFNR' "$WORK/data/extract.json")
  DP_NAME=$(jq -r --arg v "$lifnr" '.LFA1[] | select(.LIFNR == $v) | .NAME1' "$WORK/data/extract.json")
  say "SAP had paid $DP_NAME a down payment it has not yet invoiced: a vendor line with special G/L indicator A"
  say "(UMSKZ) on the down payment account T074 names, its input tax on V1 beside it in SAP's net display:"
  jq -r --arg cc "$CC" '(.ACDOCA[] | select(.RBUKRS == $cc and .UMSKZ == "A")) as $d | .ACDOCA[] | select(.RBUKRS == $cc and .BELNR == $d.BELNR)
    | "     ACDOCA \(.BELNR)/\(.DOCLN): account \(.RACCT) \(.HSL)\(if .UMSKZ then ", special G/L \(.UMSKZ)" else "" end)\(if .MWSKZ then ", tax code \(.MWSKZ)" else "" end)"' \
    "$WORK/data/extract.json"
  say "It came over as the platform's own down payments are posted: its document an entry of its own, the down"
  say "payment net of tax as the supplier's special G/L item A, and a paid down payment request naming the entry."
  QUIET=1 api 200 '' listBusinessPartners -p "name_prefix=$DP_NAME"
  DP_SUPPLIER=$(field '.items[0].id')
  api 200 '.items[] | "down payment request \(.document_number): \(.status), \(.amount.amount) \(.amount.currency) with tax \(.tax_amount.amount) on \(.tax_code), from \(.reference), journal entry \(.journal_entry_id)"' \
    listDownPaymentRequests -p "legal_entity_id=$LE" -p status=paid -p "supplier_id=$DP_SUPPLIER"
  expect "the migrated down payment requests of $DP_NAME" "$(field '.items | length')" 1
  expect "the migrated down payment's tax" "$(field '.items[0].tax_amount.amount')" 100.00
  DP_ENTRY=$(field '.items[0].journal_entry_id')
  DP_REQUEST=$(field '.items[0].id')
  api 200 '.items[] | "open item \(.document_number): \(.amount.amount) \(.amount.currency)\(if .special_gl then ", special G/L \(.special_gl)" else "" end)"' \
    listOpenItems -p "partner_id=$DP_SUPPLIER" -p page_size=100
  expect "the open down payment of $DP_NAME" "$(jq -r --arg e "$DP_ENTRY" '[.items[] | select(.special_gl == "A" and .journal_entry_id == $e) | .amount.amount] | join(" ")' "$WORK/last.json")" 2000.00
  DP_LINE=$(jq -r --arg e "$DP_ENTRY" '.items[] | select(.journal_entry_id == $e) | "\(.fiscal_year) \(.line_no)"' "$WORK/last.json")
  owed=$(jq -r '[.items[] | select(.special_gl == null) | .amount.amount | sub("\\."; "") | tonumber] | add // 0' "$WORK/last.json")
  DP_PAYABLE=$(amount "$((0 - owed))")
  say "A payment run nets the supplier's ordinary items only, $DP_PAYABLE CAD; the down payment waits for its invoice."
}

# The pilot's own settings, proposed with the pack. The company keeps
# SAP's down payment account (T074, 159000; T72c), which the merge kept
# over the pack's, so the pack's own, 1400, is left out: a company has one
# down payment account (ADR-0037). Then a CAD and a USD house
# bank at a Canadian bank with their accounts, the USD accounts revalued
# at period end, statements matched by end-to-end id or amount and posted
# with the controller's approval, a cash discount term, the cost center
# categories, the liquidity items memo records name (T53d) and, as the
# Canada pack's tax codes are for purchases (T62g), one output tax code for
# the walkthrough's sales entry: GST at 5% (Excise Tax Act, Part IX, s. 165(1);
# CRA, "Charge and collect the tax - Which rate to charge",
# https://www.canada.ca/en/revenue-agency/services/tax/businesses/topics/gst-hst-businesses/charge-collect-which-rate.html),
# a number range for the customer invoices of step 48 (T75a; the packs
# number none yet), one for the quotations of step 62 (T78a), one for
# the sales orders of step 63 (T78b) and one for the billing documents of
# step 64 (T79a),
# and two codes of tax alone for the return's adjustments (kind adjustment,
# rate 0; ADR-0049 §11, keyed as Canada 2026.6's returns.yaml maps them):
# GA, an adjustment decreasing the net tax (line 107) on 1300, and GI, an
# instalment paid (line 110) on 2300.
# The code's rate starts a year back; the registration's first period,
# its cutover, is @TAXFIRST@ (tax_first), so two filing periods have
# ended, whatever today is (step 11).
# @YEAR@ is replaced with this year, @PREV@ with last year, @TAXFIRST@
# with tax_first and @CC@ with the company code.
PILOT_SETTINGS='
.payment_terms += [{"key": "2/10N30", "description": "2% in 10 days, 30 days net", "net_days": 30,
    "discounts": [{"days": 10, "percent": "2"}]}]
| .controlling = {"cost_center_categories": [{"code": "ADMIN", "name": "Administration"},
    {"code": "SALES", "name": "Sales", "revenue_allowed": true}]}
| .cash = {"liquidity_items": [{"code": "OPERATING", "name": "Operating", "direction": "both"},
    {"code": "REC_CUSTOMERS", "name": "Customer receipts", "direction": "inflow", "parent": "OPERATING"},
    {"code": "OP_SUPPLIERS", "name": "Supplier payments", "direction": "outflow", "parent": "OPERATING"}],
    "derivation": {"defaults": {"outflow": "OP_SUPPLIERS", "inflow": "REC_CUSTOMERS"}}}
| (.companies[] | select(.code == "@CC@")) |= (
    .postal_address = {"line1": "150 Bay Street", "line2": "Receiving, dock 2", "city": "Toronto", "region": "ON", "postal_code": "M5J 2R8",
      "country_code": "CA"}
    | .chart_of_accounts |= map(select(.number != "1400"))
    | .chart_of_accounts += [
      {"number": "1010", "name": "Bank clearing - RBC CAD", "type": "asset", "open_items": true},
      {"number": "1015", "name": "Bank incoming - RBC CAD", "type": "asset", "open_items": true},
      {"number": "1020", "name": "Bank clearing - RBC USD", "type": "asset", "open_items": true, "revalue_foreign_currency": true},
      {"number": "1030", "name": "Bank - RBC USD", "type": "asset", "revalue_foreign_currency": true},
      {"number": "1040", "name": "Bank incoming - RBC USD", "type": "asset", "open_items": true, "revalue_foreign_currency": true}]
    | .tax_codes = ((.tax_codes // []) + [{"code": "GO5", "description": "GST 5% on sales (walkthrough; the Canada pack codes purchases)",
        "direction": "output", "kind": "standard", "category": "S", "components": [{"key": "gst", "jurisdiction": "CA",
        "account_key": "output", "account": "2300", "rates": [{"rate_percent": "5", "valid_from": "@PREV@-01-01"}]}]},
      {"code": "GA", "description": "GST/HST adjustment decreasing the net tax (line 107)", "direction": "input", "kind": "adjustment",
        "components": [{"key": "gst", "jurisdiction": "CA", "account_key": "input", "account": "1300", "rates": [{"rate_percent": "0", "valid_from": "@PREV@-01-01"}]}]},
      {"code": "GI", "description": "GST/HST instalment paid (line 110)", "direction": "input", "kind": "adjustment",
        "components": [{"key": "instalment", "jurisdiction": "CA", "account_key": "input", "account": "2300", "rates": [{"rate_percent": "0", "valid_from": "@PREV@-01-01"}]}]}])
    | .house_banks = ((.house_banks // []) + [
      {"name": "RBC", "bic": "ROYCCAT2", "institution_number": "003", "transit_number": "00002", "account_number": "1000001",
       "gl_account": "1010", "main_gl_account": "1000", "incoming_gl_account": "1015"},
      {"name": "RBCUS", "bic": "ROYCCAT2", "institution_number": "003", "transit_number": "00002", "account_number": "4000001",
       "gl_account": "1020", "main_gl_account": "1030", "incoming_gl_account": "1040", "currency": "USD"}])
    | .tax_registrations = [{"code": "GSTHST", "authority": "CRA", "number": "123456789 RT 0001", "return_type": "GST34",
        "frequency": "quarterly", "first_period_start": "@TAXFIRST@"}]
    | .cash = {"payment_days": ["tue", "thu"]}
    | .payment_methods += [{"code": "W", "description": "Wire transfer", "order": 1,
        "ranking": [{"currency": "CAD", "house_banks": ["RBC"]}]}]
    | .release_dual_limit = "4000.00"
    | .service_posting_limit = "1000.00"
    | .fx_valuation.accounts += (["1020", "1030", "1040"] | map({"account": ., "unrealised_gain": "4900", "unrealised_loss": "6900"}))
    | .bank_statements = {"matching_rules": [{"kind": "end_to_end_id"}, {"kind": "amount", "date_window_days": 5, "counterparty": true}],
        "approval_tiers": [{"up_to": "10000.00", "steps": []}, {"steps": [{"role": "controller", "approvals": 1}]}]})
| .number_ranges += [{"company": "@CC@", "document_type": "customer_invoice", "fiscal_year": @YEAR@, "prefix": "CI", "first": 1, "last": 999999},
    {"company": "@CC@", "document_type": "rfq", "fiscal_year": @YEAR@, "prefix": "RQ", "first": 1, "last": 999999},
    {"company": "@CC@", "document_type": "journal_dr", "fiscal_year": @YEAR@, "first": 1800000000, "last": 1899999999},
    {"company": "@CC@", "document_type": "incoming_payment", "fiscal_year": @YEAR@, "prefix": "IP", "first": 1, "last": 999999},
    {"company": "@CC@", "document_type": "journal_dz", "fiscal_year": @YEAR@, "first": 1400000000, "last": 1499999999},
    {"company": "@CC@", "document_type": "journal_dg", "fiscal_year": @YEAR@, "first": 1600000000, "last": 1699999999},
    {"company": "@CC@", "document_type": "quotation", "fiscal_year": @YEAR@, "prefix": "QT", "first": 1, "last": 999999},
    {"company": "@CC@", "document_type": "sales_order", "fiscal_year": @YEAR@, "prefix": "SO", "first": 1, "last": 999999},
    {"company": "@CC@", "document_type": "billing_document", "fiscal_year": @YEAR@, "prefix": "BD", "first": 1, "last": 999999}]'

step_pack() {
  heading "Step 2 — The Canada country pack and the pilot's settings, before migration, through a governed change request"
  as ada
  api 200 '.items[] | "pack \(.country) \(.version): \(.contents.accounts) accounts, \(.contents.tax_codes) tax codes, \(.contents.holidays) holidays, \(.contents.number_ranges) number ranges; signed off: \(.signed_off)"' listCountryPacks
  local version
  version=$(field '[.items[] | select(.country == "CA") | .version] | sort_by(split(".") | map(tonumber)) | last')
  expect "the latest Canada pack" "$version" "$CA_PACK"
  say "CA $version defines the GST/HST return's lines (returns.yaml, T61j) for step 11, signed off by the tax specialist"
  say "(Musto Ibrahim, 2026-11-09), so production applies it. 2026.7 signed off 2026.6, which stays released unsigned;"
  say "2026.8 is 2026.7 with the lines' labels no longer marked pending the tax specialist (product owner, 2026-11-09)."
  expect "the latest Canada pack's sign-off" "$(field '.items[] | select(.country == "CA" and .version == "'"$version"'") | "\(.signed_off) \(.sign_off.tax_specialist) \(.sign_off.date)"')" \
    "true Musto Ibrahim 2026-11-09"
  say "Ada applies the latest Canada pack, $version, to company $CC. The merge keeps what the company"
  say "already has (SAP's chart and number ranges) and adds the rest; it becomes a draft change request."
  api 201 '"change request \(.number), \(.status): \(.title)"' applyCountryPack \
    -body "{\"country\": \"CA\", \"version\": \"$version\", \"company\": \"$CC\", \"applied_on\": \"$TODAY\", \"fiscal_year\": $YEAR}"
  local cr
  cr=$(field .id)
  expect "notes that the pack is not signed off" "$(field '[.notes[] | select((.path | startswith("packs")) and (.message | contains("signed")))] | length')" 0
  say "  ($(field '[.notes[] | select(.message | startswith("conflict"))] | length') more notes list where SAP's settings were kept over the pack's)"
  say "Ada adds the pilot's own settings to the proposal: RBC house banks in CAD and USD, USD accounts revalued"
  say "at month end, bank statement rules (match by end-to-end id, approval above CAD 10,000), payment runs"
  say "above CAD 4,000 released only after a second person approves, a wire method W through RBC that payment"
  say "runs try first (order 1), before SAP's bank transfer T (step 7), a 2/10 net 30 cash discount term,"
  say "cost center categories, liquidity items, an output tax code for GST and two for the return's adjustments,"
  say "the company's postal address in Toronto, where suppliers deliver (printed on its purchase orders),"
  say "the GST/HST registration and, for the"
  say "billing system's entries, a service posting limit of CAD 1,000 (step 11). The company keeps SAP's"
  say "down payment account (T074), so the pack's own, 1400, is left out: a company has one (ADR-0037)."
  local settings=${PILOT_SETTINGS//@YEAR@/$YEAR}
  settings=${settings//@PREV@/$PREV}
  settings=${settings//@TAXFIRST@/$(tax_first)}
  settings=${settings//@CC@/$CC}
  jq "{title: \"Canada pack $version and the pilot's settings\", rationale: \"Pilot company set-up\", proposed: (.proposed | $settings)}" \
    "$WORK/last.json" >"$WORK/proposal.json"
  api 200 '"\(.number): \(.title)"' updateChangeRequest -p "id=$cr" -if-match "$ETAG" -body @proposal.json
  api 200 '"\(.status), tier \(.tier) (structural): \(.items | length) changes"' submitChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "status after submit" "$(field .status)" awaiting_evidence
  say "A tier 4 change needs evidence: a regression suite of the company's invoice decisions, replayed under the"
  say "proposal. Nothing is migrated yet, so there are no invoices to capture:"
  refused 422 config.no_cases createRegressionSuite -body '{"name": "Before the Canada pack", "window_days": 366}'
  say "Ada records that there is nothing to replay: a suite of no cases, which the evidence and the approvers will show."
  api 201 '"regression suite \(.number): \(.case_count) cases"' createRegressionSuite \
    -body '{"name": "Before the Canada pack, no invoices yet", "window_days": 366, "allow_empty": true}'
  local suite
  suite=$(field .number)
  say "The simulation replays the invoices of the last 366 days, as far back as the suite looked when it was captured."
  QUIET=1 api 200 '' getChangeRequest -p "id=$cr"
  api 202 '"\(.kind) \(.status)"' simulateChangeRequest -p "id=$cr" -if-match "$ETAG"
  wait_op
  api 200 '.evidence[] | "evidence \(.kind): \(if .passed then "passed" else "FAILED" end), \(.summary)"' getChangeRequest -p "id=$cr"
  expect "status after the evidence" "$(field .status)" pending_approval
  expect "the regression evidence says no case was replayed" "$(field '.evidence[] | select(.kind == "regression") | .detail.no_cases')" true
  expect "the simulation's window is the suite's" "$(field '.evidence[] | select(.kind == "simulation") | "\(.detail.window_days) days of \(.detail.window_suite)"')" "366 days of $suite"
  local request
  request=$(field .workflow_request_id)
  say "Two approvers, neither of them its author: the solution architect, then the controller. Nothing was"
  say "replayed, so an approval without a comment acknowledging that is refused:"
  as sam
  QUIET=1 api 200 '' getApprovalRequest -p "id=$request"
  refused 422 workflow.acknowledgement_required approveApprovalRequest -p "id=$request" -if-match "$ETAG" -body '{}'
  approve sam "$request" "Structure checked; accepted without regression cases, as nothing is migrated yet"
  approve carla "$request" "Postings checked; accepted without regression cases, as nothing is migrated yet"
  as ada
  QUIET=1 api 200 '' getChangeRequest -p "id=$cr"
  api 200 '"\(.status): configuration version \(.activated_version)"' activateChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "status after activation" "$(field .status)" active
  api 200 '.items[] | "house bank \(.code): \(.currency), account ending \(.account_last4)\(if .institution_number then ", institution \(.institution_number), transit \(.transit_number)" else "" end)"' \
    listHouseBanks -p "legal_entity_id=$LE"
  expect "RBC's Canadian routing" "$(field '.items[] | select(.code == "RBC") | "\(.institution_number) \(.transit_number)"')" "003 00002"
  RBC=$(field '.items[] | select(.code == "RBC") | .id')
  RBCUS=$(field '.items[] | select(.code == "RBCUS") | .id')
  if [ -z "$RBC" ] || [ -z "$RBCUS" ]; then fail "the RBC house banks are not there"; fi
  next_year_ranges "$version"
  accounts
  passed
}

# next_year_ranges has Ada try the pack's number ranges for next fiscal
# year: created afresh, they would number next year's first payment run
# PY1 again, which this year's range issues too, and a run's number (and
# the end-to-end ids made from it) is unique across years, so the change
# is refused until the year is in their prefixes (review:T73g2).
next_year_ranges() {
  local version=$1 next=$((YEAR + 1)) cr
  say "Before the year ends Ada applies the pack for fiscal year $next, for its number ranges. The pack starts"
  say "each range again at 1 with the same prefix, so the year's numbers would repeat this year's: refused."
  api 201 '"change request \(.number), \(.status): \(.title)"' applyCountryPack \
    -body "{\"country\": \"CA\", \"version\": \"$version\", \"company\": \"$CC\", \"applied_on\": \"$TODAY\", \"fiscal_year\": $next}"
  cr=$(field .id)
  jq --arg next "$next" '{title: .title, rationale: "Number ranges of the next fiscal year", proposed: (.proposed | .number_ranges |= map(
      if .fiscal_year == ($next | tonumber) and (.document_type | startswith("journal_") | not) then .prefix = (.prefix // "") + $next + "-" else . end))}' \
    "$WORK/last.json" >"$WORK/next-year.json"
  QUIET=1 api 200 '' getChangeRequest -p "id=$cr"
  refused 422 config.validation_failed submitChangeRequest -p "id=$cr" -if-match "$ETAG"
  excerpt '.errors[] | select(.field == "proposed.number_ranges['"$CC"'/payment_run/'"$next"'].prefix") | "\(.field): \(.message)"'
  expect "the payment run range refused" "$(field '[.errors[] | select(.field == "proposed.number_ranges['"$CC"'/payment_run/'"$next"'].prefix")] | length')" 1
  say "With the year in each prefix but the journal entries', which are numbered per fiscal year, it is accepted."
  QUIET=1 api 200 '' getChangeRequest -p "id=$cr"
  api 200 '.proposed.number_ranges[] | select(.document_type == "payment_run" and .company == "'"$CC"'") | "payment run range of \(.fiscal_year): prefix \(.prefix)"' \
    updateChangeRequest -p "id=$cr" -if-match "$ETAG" -body @next-year.json
  api 200 '"\(.number): \(.status)"' submitChangeRequest -p "id=$cr" -if-match "$ETAG"
  say "The pilot takes next year's ranges on at year end, so Ada withdraws the request for now."
  api 200 '"\(.number): \(.status)"' withdrawChangeRequest -p "id=$cr" -if-match "$ETAG" -body '{"reason": "The next fiscal year ranges are set up at year end"}'
  expect "status after withdrawal" "$(field .status)" withdrawn
}

step_procure() {
  heading "Step 4 — Requisition, purchase order with its release strategy, its document for the supplier, a dearer change rejected, goods receipt"
  as ben
  api 200 '.items[] | "supplier \(.number) \(.name), \(.country_code), \(.status)"' listBusinessPartners -p name_prefix=Tyrell
  SUPPLIER=$(field '.items[0].id')
  if [ -z "$SUPPLIER" ] || [ "$SUPPLIER" = null ]; then fail "the migrated supplier Tyrell Office Products is not there"; fi
  local item expense
  expense=$(account 6000)
  item="{\"description\": \"Ergonomic office chairs\", \"quantity\": {\"value\": \"10\", \"unit\": \"EA\"}, \"unit_price\": {\"amount\": \"450.00\", \"currency\": \"CAD\"}, \"account_id\": \"$expense\"}"
  say "Ben asks for ten office chairs from Tyrell, a supplier migrated from SAP, needed in two weeks."
  say "The requisition is dated on Maple's date in Toronto, not UTC's: raised on a Toronto evening it is still"
  say "that day, and its amount is routed at that day's rate (T72n5)."
  NEEDED_BY=$(date_add "$TODAY" 14)
  local req request before
  before=$(company_day)
  api 201 '"requisition \(.id) of \(.requisition_date): \(.status), \(.total_amount.amount) \(.total_amount.currency), needed by \(.needed_by)"' createRequisition \
    -body "{\"legal_entity_id\": \"$LE\", \"currency\": \"CAD\", \"supplier_id\": \"$SUPPLIER\", \"needed_by\": \"$NEEDED_BY\", \"items\": [$item]}"
  expect_today "the requisition's date, Toronto's" "$(field .requisition_date)" "$before"
  req=$(field .id)
  api 200 '"\(.status): approval request \(.workflow_request_id)"' submitRequisition -p "id=$req" -if-match "$ETAG"
  request=$(field .workflow_request_id)
  say "Ben follows what he asked for: his own requests still waiting (requested_by=me, T73d2)."
  api 200 '.items[] | "approval request \(.object_type): \(.title), \(.status), step \(.current_step) of \(.steps | length)"' listApprovalRequests \
    -p requested_by=me -p status=pending
  expect "Ben's pending request" "$(field '[.items[] | select(.id == "'"$request"'")] | length')" 1
  say "Ben cannot decide his own request; Priya, the purchasing manager, approves it."
  QUIET=1 api 200 '' getApprovalRequest -p "id=$request"
  refused 403 identity.permission_missing approveApprovalRequest -p "id=$request" -if-match "$ETAG" -body '{}'
  approve priya "$request" "Budgeted"
  as ben
  say "Tyrell quotes CAD 470.00 a chair, more than the approved CAD 450.00: the order exceeds its requisition,"
  say "so the release strategy asks for approval before it goes to the supplier. It pays 2% in 10 days, net 30."
  say "Tyrell can deliver in ten days, before the chairs are needed: the item carries that delivery date (T53g)."
  DELIVERY=$(date_add "$TODAY" 10)
  local ordered=${item/450.00/470.00}
  ordered="${ordered%\}}, \"delivery_date\": \"$DELIVERY\"}"
  api 201 '"purchase order \(.document_number // .id): \(.status), \(.total_amount.amount) \(.total_amount.currency), terms \(.payment_term_code)", (.items[] | "  item \(.item_no): delivery \(.delivery_date)")' createPurchaseOrder \
    -body "{\"legal_entity_id\": \"$LE\", \"supplier_id\": \"$SUPPLIER\", \"currency\": \"CAD\", \"order_date\": \"$TODAY\", \"payment_term_code\": \"2/10N30\", \"requisition_id\": \"$req\", \"items\": [$ordered]}"
  expect "the item's delivery date" "$(field '.items[0].delivery_date')" "$DELIVERY"
  PO=$(field .id)
  api 200 '"\(.status): release request \(.release_request_id)"' releasePurchaseOrder -p "id=$PO" -if-match "$ETAG"
  expect "order status" "$(field .status)" pending_approval
  approve priya "$(field .release_request_id)" "Price increase accepted"
  as ben
  api 200 '"purchase order \(.document_number): \(.status), valued through \(.valuation)"' getPurchaseOrder -p "id=$PO"
  expect "order status" "$(field .status)" released
  order_document
  order_change
  say "The chairs arrive; Ben posts the goods receipt (debit expense, credit GR/IR)."
  api 201 '"goods receipt \(.id), journal entry \(.journal_entry_id)"' postGoodsReceipt \
    -body "{\"legal_entity_id\": \"$LE\", \"document_date\": \"$TODAY\", \"posting_date\": \"$TODAY\", \"items\": [{\"purchase_order_id\": \"$PO\", \"po_item_no\": 1, \"quantity\": {\"value\": \"10\", \"unit\": \"EA\"}}]}"
  RECEIPT=$(field .id)
  api 200 '.items[] | "item \(.item_no): ordered \(.quantity.value), received \(.received_quantity.value)"' getPurchaseOrder -p "id=$PO"
  passed
}

# order_document shows the released order's document for Tyrell (T72g,
# ADR-0054): Maple's postal address of step 2 as the buyer's and the
# address to deliver to, and Tyrell's street, which the migration took
# from SAP's central address (ADRC by LFA1-ADRNR, po:2026-11-09): the row
# valid today, not the old address whose row ended in 2019. Then Ben adds
# the address Tyrell takes orders at, which the document prints instead
# of its legal one.
order_document() {
  local number change utc_today want
  number=$(field .document_number)
  say "Ben fetches the order's document to send to Tyrell (by hand: email and EDI are post-pilot). It names Maple"
  say "with its configured postal address, delivers there, as no item names an address of its own, and prints"
  say "Tyrell's street, which the migration took from SAP's central address, ADRC (product owner 2026-11-04):"
  api 200 '' getPurchaseOrderDocument -p "id=$PO"
  sed 's/^/     /' "$WORK/last.json"
  for want in "PURCHASE ORDER $number" "Payment terms: 2/10N30" "10 EA at 470.00 CAD per 1 EA" "Delivery date $DELIVERY" "Total 4700.00 CAD"; do
    grep -qF "$want" "$WORK/last.json" || fail "the order's document does not show '$want'"
  done
  # The blocks are matched whole, each line in its place.
  local doc maple=$'  150 Bay Street\n  Receiving, dock 2\n  Toronto, ON, M5J 2R8\n  Country CA'
  doc=$(cat "$WORK/last.json")
  [[ $doc == *$'Buyer\n  Maple Industries Canada (company '"$CC"$')\n'"$maple"* ]] ||
    fail "the order's document does not show Maple's postal address as the buyer's"
  [[ $doc == *$'Deliver to\n  Maple Industries Canada\n'"$maple"* ]] || fail "the order's document does not deliver to Maple's postal address"
  [[ $doc == *$')\n  2019 Tyrell Plaza\n  Los Angeles, CA, 90012\n  Country US\n\nDeliver to'* ]] ||
    fail "the order's document does not show Tyrell's street from SAP"
  if grep -qF "Old Tyrell Road" "$WORK/last.json"; then fail "the order's document prints Tyrell's address of before 2020 from SAP"; fi
  # The item's own date is printed, not the requisition's: no item is left
  # to be delivered by it (ADR-0054 As built).
  if grep -qF "Needed by" "$WORK/last.json"; then fail "the order's document prints the requisition's date beside the item's own"; fi
  # The address starts on the database's day, UTC's: a business partner
  # belongs to no company and so has no zone, and its address may start no
  # earlier than that day (partner.backdated_address, database.Today). It
  # is read as the change is asked for, not when the run began, so a run
  # that crosses 00:00 UTC (19:00 or 20:00 in Toronto) asks for that day.
  utc_today=$(utc_day)
  say "Tyrell takes orders at its Toronto order desk. Ben asks for that address; Carla, who approves supplier"
  say "master data, approves it, and the document prints it instead of the legal address from SAP."
  api 201 '"partner change \(.id): \(.status)"' createPartnerChange -body "{\"partner_id\": \"$SUPPLIER\", \"kind\": \"update\", \"reason\": \"Order desk address confirmed by Tyrell\", \"payload\": {\"addresses\": [{\"purpose\": \"order_from\", \"line1\": \"220 King Street West\", \"line2\": \"Order desk, floor 3\", \"city\": \"Toronto\", \"region\": \"ON\", \"postal_code\": \"M5H 1K4\", \"country_code\": \"CA\", \"valid_from\": \"$utc_today\"}]}}"
  change=$(field .id)
  approve carla "$(field .workflow_request_id)" "Address confirmed"
  as ben
  QUIET=1 api 200 '' getPartnerChange -p "id=$change"
  expect "partner change status" "$(field .status)" applied
  QUIET=1 api 200 '' getPurchaseOrderDocument -p "id=$PO"
  sed -n '/^Supplier$/,/^$/p' "$WORK/last.json" | sed 's/^/     /'
  grep -qF "220 King Street West" "$WORK/last.json" || fail "the order's document does not show Tyrell's order desk address"
  if grep -qF "2019 Tyrell Plaza" "$WORK/last.json"; then fail "the order's document prints Tyrell's legal address beside its order desk's"; fi
}

# order_change has Ben ask for Tyrell's dearer price on the released
# order; the change waits for approval, and Priya reads it where its
# request places it (object_resource: through its order, as the workspace
# reads it, T73d3) and rejects it, so the order stays as released.
order_change() {
  local item change request
  item="{\"description\": \"Ergonomic office chairs\", \"quantity\": {\"value\": \"10\", \"unit\": \"EA\"}, \"unit_price\": {\"amount\": \"490.00\", \"currency\": \"CAD\"}, \"account_id\": \"$(account 6000)\"}"
  say "Tyrell asks CAD 490.00 a chair after all. Ben drafts the change of the released order and submits it; it"
  say "raises the order's total, so it waits for approval."
  api 201 '"order change \(.id): \(.status), \(.total_before.amount) -> \(.total_after.amount) \(.total_after.currency)"' createPurchaseOrderChange -p "id=$PO" \
    -body "{\"reason\": \"Tyrell raises the chairs by CAD 20.00 each\", \"items\": [$item]}"
  change=$(field .id)
  api 200 '"order change: \(.status), approval request \(.approval_request_id)"' submitPurchaseOrderChange -p "id=$PO" -p "change_id=$change" -if-match "$ETAG"
  expect "order change status" "$(field .status)" pending_approval
  request=$(field .approval_request_id)
  say "Priya opens the request. Its object is the change, read through its order: the request says where."
  as priya
  api 200 '"approval request \(.object_type) \(.object_id): \(.object_resource)"' getApprovalRequest -p "id=$request"
  expect "the request's object" "$(field .object_id)" "$change"
  expect "where the request places it" "$(field .object_resource)" "/v1/purchase-orders/$PO/changes/$change"
  api 200 '"order change: \(.reason); \(.total_before.amount) -> \(.total_after.amount) \(.total_after.currency), \(.status)"' getPurchaseOrderChange \
    -p "id=$PO" -p "change_id=$change"
  expect "the change's request" "$(field .approval_request_id)" "$request"
  say "The budget holds CAD 470.00 a chair. She rejects the change, and the order stays as it was released."
  QUIET=1 api 200 '' getApprovalRequest -p "id=$request"
  api 200 '"approval request \(.object_type): \(.status)"' rejectApprovalRequest -p "id=$request" -if-match "$ETAG" \
    -body '{"comment": "Not budgeted: keep CAD 470.00 or ask for another quote."}'
  as ben
  api 200 '"purchase order \(.document_number): \(.status), revision \(.revision), \(.total_amount.amount) \(.total_amount.currency)"' getPurchaseOrder -p "id=$PO"
  expect "order total after the rejected change" "$(field .total_amount.amount)" 4700.00
}

# invoice REFERENCE QUANTITY AMOUNT [KIND] enters a supplier invoice on
# the order's item 1, without tax (tax on invoices is T61d).
invoice() {
  api 201 '"\(.kind) \(.document_number) \(.supplier_reference): \(.status), \(.gross_amount.amount) \(.gross_amount.currency)"' createSupplierInvoice \
    -body "{\"kind\": \"${4:-invoice}\", \"legal_entity_id\": \"$LE\", \"supplier_id\": \"$SUPPLIER\", \"supplier_reference\": \"$1\", \"invoice_date\": \"$TODAY\", \"gross_amount\": {\"amount\": \"$3\", \"currency\": \"CAD\"}, \"tax_amount\": {\"amount\": \"0.00\", \"currency\": \"CAD\"}, \"purchase_order_id\": \"$PO\", \"items\": [{\"purchase_order_id\": \"$PO\", \"po_item_no\": 1, \"quantity\": {\"value\": \"$2\", \"unit\": \"EA\"}, \"amount\": {\"amount\": \"$3\", \"currency\": \"CAD\"}}]}"
}

MATCHED='"match: \(if .is_clean then "clean" else "issues" end)\(if .decision then ", \(.decision.outcome): \(.decision.reason)" else "" end)\([.issues[] | "\n  issue \(.code): \(.message)"] | join(""))"'

step_invoices() {
  heading "Step 5 — The invoice is matched and posted; a second one for more than was received is refused; an invoice for SAP's receipt clears its migrated GR/IR"
  as alex
  say "Tyrell's invoice for the ten chairs, and then the same invoice again under another reference."
  say "Alex matches both before posting either: each looks clean on its own (10 received, nothing invoiced yet)."
  invoice TY-5001 10 4700.00
  INVOICE=$(field .id)
  invoice TY-5001-B 10 4700.00
  local dup
  dup=$(field .id)
  api 200 "$MATCHED" matchSupplierInvoice -p "id=$INVOICE" -if-match '"1"'
  api 200 "$MATCHED" matchSupplierInvoice -p "id=$dup" -if-match '"1"'
  expect "the second match" "$(field .is_clean)" true
  api 200 '"\(.document_number): \(.status), journal entry \(.journal_entry_id)"' postSupplierInvoice -p "id=$INVOICE" -if-match '"1"' -body "{\"posting_date\": \"$TODAY\"}"
  expect "invoice status" "$(field .status)" posted
  INVOICE_ENTRY=$(field .journal_entry_id)
  say "Posting the second re-checks the match under the order's lock (T72a): 20 invoiced, 10 received."
  refused 409 payables.match_outdated postSupplierInvoice -p "id=$dup" -if-match '"1"' -body "{\"posting_date\": \"$TODAY\"}"
  say "The refusal carries the issue as fields (T73e3, ADR-0052 decision D3), which the web workspace names in the"
  say "clerk's language:"
  excerpt '.match_issues[] | "issue \(.code) on item \(.item_no): \(.message)"'
  expect "the refusal's issue" "$(field '[.match_issues[] | "\(.code) \(.item_no)"] | join(", ")')" "quantity_exceeds_received 1"
  say "Matched again it shows the quantity issue and goes on hold; Alex cancels the duplicate."
  api 200 "$MATCHED" matchSupplierInvoice -p "id=$dup" -if-match '"1"'
  expect "the issue" "$(field '.issues[0].code')" quantity_exceeds_received
  api 200 '"\(.document_number): \(.status)"' getSupplierInvoice -p "id=$dup"
  expect "the duplicate's status" "$(field .status)" on_hold
  api 200 '"\(.document_number): \(.status)"' cancelSupplierInvoice -p "id=$dup" -if-match "$ETAG" -body '{"reason": "Duplicate of TY-5001"}'
  DUPLICATE=$dup
  migrated_grir_invoice
  passed
}

# migrated_grir_invoice posts the invoice for the shelving units SAP
# received before cutover (step 3): it clears the migrated GR/IR credit at
# the order price, and books no expense, which SAP's receipt booked.
migrated_grir_invoice() {
  say "$GRIR_NAME invoices the eight shelving units SAP received before cutover (step 3), CAD 1,200.00. Alex enters it"
  say "on the migrated order, matches it (8 received in SAP, none invoiced) and posts it:"
  local body doc entry
  body="{\"legal_entity_id\": \"$LE\", \"supplier_id\": \"$GRIR_SUPPLIER\", \"supplier_reference\": \"SHELF-8\", \"invoice_date\": \"$TODAY\", \"gross_amount\": {\"amount\": \"1200.00\", \"currency\": \"CAD\"}, \"tax_amount\": {\"amount\": \"0.00\", \"currency\": \"CAD\"}, \"purchase_order_id\": \"$GRIR_PO\", \"items\": [{\"purchase_order_id\": \"$GRIR_PO\", \"po_item_no\": 1, \"quantity\": {\"value\": \"8\", \"unit\": \"EA\"}, \"amount\": {\"amount\": \"1200.00\", \"currency\": \"CAD\"}}]}"
  api 201 '"\(.kind) \(.document_number) \(.supplier_reference): \(.status), \(.gross_amount.amount) \(.gross_amount.currency)"' createSupplierInvoice -body "$body"
  doc=$(field .id)
  api 200 "$MATCHED" matchSupplierInvoice -p "id=$doc" -if-match '"1"'
  expect "the match of the invoice for SAP's receipt" "$(field .is_clean)" true
  api 200 '"\(.document_number): \(.status), journal entry \(.journal_entry_id)"' postSupplierInvoice -p "id=$doc" -if-match '"1"' -body "{\"posting_date\": \"$TODAY\"}"
  expect "the invoice's status" "$(field .status)" posted
  entry=$(field .journal_entry_id)
  say "Its entry debits the GR/IR account, clearing SAP's credit, and books no expense again:"
  api 200 '.lines[] | "\(.account_id | acct): \(.amount.amount) \(.amount.currency)"' getJournalEntry -p "id=$entry"
  expect "the invoice's lines" "$(field '.lines | length')" 2
  expect "the invoice's GR/IR line" "$(field "[.lines[] | select(.purchase_order_id == \"$GRIR_PO\") | .amount.amount] | join(\" \")")" 1200.00
  api 200 "$GRIR_POSITION" listGrIrBalances -p "legal_entity_id=$LE" -p "purchase_order_id=$GRIR_PO" -p open_only=false
  expect "the migrated position after the invoice" "$(field '.items[0].balance.amount')" 0.00
}

step_credit() {
  heading "Step 6 — A credit memo; a credit past its invoice is refused; the receipt it leaves invoiced cannot be reversed"
  as alex
  say "One chair arrived damaged; Tyrell credits it. A credit memo debits the supplier when posted."
  invoice TY-CM-77 1 470.00 credit_memo
  CREDIT=$(field .id)
  api 200 "$MATCHED" matchSupplierInvoice -p "id=$CREDIT" -if-match '"1"'
  say "Alex posts it without a posting date: it is dated Maple's date in Toronto, not UTC's, so on a Toronto"
  say "evening it is still that day, in that day's period (product owner 2026-10-31)."
  local before
  before=$(company_day)
  api 200 '"\(.kind) \(.document_number): \(.status) on \(.posting_date)"' postSupplierInvoice -p "id=$CREDIT" -if-match '"1"' -body '{}'
  expect "credit memo status" "$(field .status)" posted
  expect_today "the credit memo's posting date, Toronto's" "$(field .posting_date)" "$before"
  CREDIT_ENTRY=$(field .journal_entry_id)
  api 200 '.items[] | "open item \(.amount.amount) \(.amount.currency)"' listOpenItems -p "partner_id=$SUPPLIER"
  credit_limit
  receipt_held
  passed
}

# credit_limit has Tyrell credit invoice TY-5001 for a cent more than it
# invoiced, without the order, so no match holds the document: credits
# naming an invoice stay within its gross amount (product owner
# 2026-11-10, 03 §3.9), whatever is on an order.
credit_limit() {
  say "Tyrell sends a second credit memo, naming invoice TY-5001 (4,700.00) and not the order, for 4,700.01."
  say "A document without an order is not matched; the credits naming an invoice may still not exceed its gross"
  say "amount (product owner 2026-11-10), so posting it is refused, the refusal carrying the figures:"
  api 201 '"\(.kind) \(.document_number) \(.supplier_reference): \(.status), \(.gross_amount.amount) \(.gross_amount.currency)"' createSupplierInvoice \
    -body "{\"kind\": \"credit_memo\", \"reference_invoice_id\": \"$INVOICE\", \"legal_entity_id\": \"$LE\", \"supplier_id\": \"$SUPPLIER\", \"supplier_reference\": \"TY-CM-78\", \"invoice_date\": \"$TODAY\", \"gross_amount\": {\"amount\": \"4700.01\", \"currency\": \"CAD\"}, \"tax_amount\": {\"amount\": \"0.00\", \"currency\": \"CAD\"}, \"items\": [{\"account_id\": \"$(account 6000)\", \"description\": \"Chairs credited\", \"quantity\": {\"value\": \"1\", \"unit\": \"EA\"}, \"amount\": {\"amount\": \"4700.01\", \"currency\": \"CAD\"}}]}"
  local over
  over=$(field .id)
  refused 422 payables.credit_exceeds_invoice postSupplierInvoice -p "id=$over" -if-match '"1"' -body '{}'
  excerpt '.credit_limit | "invoice \(.invoice_number): gross \(.invoice_gross.amount) \(.invoice_gross.currency), already credited \(.credited.amount), refused \(.attempted.amount)"'
  expect "the refusal's figures: gross, credited, refused" "$(field '.credit_limit | "\(.invoice_gross.amount) \(.credited.amount) \(.attempted.amount) \(.attempted.currency)"')" "4700.00 0.00 4700.01 CAD"
  say "TY-CM-77 names no invoice, so it is not counted. Alex cancels the document."
  api 200 '"\(.document_number): \(.status)"' cancelSupplierInvoice -p "id=$over" -if-match '"1"' -body '{"reason": "Credits more than invoice TY-5001"}'
}

# receipt_held has Ben try to reverse the chairs' receipt of step 4 once
# nine of them are invoiced net of the credit memo: a receipt's reversal
# may not leave less received than invoiced (03 §3.9), or the order would
# be paid for goods it no longer shows received.
receipt_held() {
  say "Ben thinks the chairs' receipt was posted by mistake and tries to reverse it. Nine of the ten are"
  say "invoiced, net of the credit memo: the reversal would leave none received of nine paid for, so it is"
  say "refused, naming the receipt, the order item and both quantities. Tyrell's credit memo, or a return"
  say "of the goods (122), comes first."
  as ben
  QUIET=1 api 200 '' getGoodsReceipt -p "id=$RECEIPT"
  local number
  number=$(field .document_number)
  refused 409 procurement.reversal_below_invoiced reverseGoodsReceipt -p "id=$RECEIPT" -if-match "$ETAG" -body '{}'
  local detail
  detail=$(field .detail)
  case "$detail" in
    *"goods receipt $number "*"with 0 received of 9 invoiced"*) ;;
    *) fail "the refusal does not name the receipt $number and its quantities: $detail" ;;
  esac
  api 200 '.items[] | "item \(.item_no): ordered \(.quantity.value), received \(.received_quantity.value)"' getPurchaseOrder -p "id=$PO"
  expect "the chairs received after the refusal" "$(field '.items[0].received_quantity.value')" 10
}

# method_order shows which method a run without a house bank pays by
# (ADR-0038 amendment; SAP specialist 2026-11-09, A4(c)): the company
# tries wire W before bank transfer T (step 2 gave W order 1); Laurentian
# Paper may be paid by T or W and lists T first, and is paid by W, the
# run's first method it allows, while a supplier allowing T alone is paid
# by T. An item's fixed payment method wins over that order (ADR-0038,
# product owner 2026-11-10): Laurentian's freight invoice fixes T, so the
# run pays Laurentian twice, its other items by W from RBC and the
# freight invoice by T from MAIN, each payment saying why it is paid by
# its method. It wins over the supplier's own methods too (product owner
# 2026-11-10, as SAP's ZLSCH over ZWELS): Muskoka Office Supply may be
# paid by T alone, and its express charge, which fixes W, is paid by
# wire. Tara rejects the run, so its items are free for the run from RBC.
method_order() {
  local laurentian muskoka company change run etag
  as ben
  QUIET=1 api 200 '' listBusinessPartners -p name_prefix=Laurentian
  laurentian=$(field '.items[0].id')
  if [ -z "$laurentian" ] || [ "$laurentian" = null ]; then fail "the migrated supplier Laurentian Paper is not there"; fi
  QUIET=1 api 200 '' getBusinessPartner -p "id=$laurentian"
  company=$(jq -c --arg le "$LE" '[.supplier_companies[] | select(.legal_entity_id == $le)
    | {legal_entity_id, reconciliation_account_id, payment_term_code, payment_methods: ["T", "W"]}
      + (if .withholding_tax_codes then {withholding_tax_codes} else {} end) + (if .planning_group then {planning_group} else {} end)]' "$WORK/last.json")
  say "Laurentian Paper also takes wires. Ben asks for both methods on its company data, bank transfer first;"
  say "Carla approves. A supplier's list says which methods it may be paid by; its order decides nothing."
  api 201 '"partner change \(.id): \(.status)"' createPartnerChange \
    -body "{\"partner_id\": \"$laurentian\", \"kind\": \"update\", \"reason\": \"Laurentian Paper accepts wires\", \"payload\": {\"supplier_companies\": $company}}"
  change=$(field .id)
  approve carla "$(field .workflow_request_id)" "Confirmed with Laurentian Paper"
  as ben
  QUIET=1 api 200 '' getPartnerChange -p "id=$change"
  expect "partner change status" "$(field .status)" applied
  QUIET=1 api 200 '' getBusinessPartner -p "id=$laurentian"
  expect "Laurentian Paper's payment methods, in its own order" \
    "$(field '.supplier_companies[] | select(.legal_entity_id == "'"$LE"'") | .payment_methods | join(" ")')" "T W"
  as tom
  say "The company's payment runs try wire W first, then bank transfer T (the configuration's order, step 2):"
  api 200 '.items[0].document.companies[] | select(.code == "'"$CC"'") | .payment_methods | to_entries[] |
    "payment method \(.value.code) \(.value.description): \(if .value.order then "order \(.value.order)" else "no order, after those with one, as listed" end)"' \
    listConfigVersions -p is_active=true
  expect "the methods the company gives an order" \
    "$(field '[.items[0].document.companies[] | select(.code == "'"$CC"'") | .payment_methods[] | select(.order) | "\(.code) \(.order)"] | join(", ")')" "W 1"
  as alex
  say "Laurentian Paper asks for its freight charge LP-FREIGHT-1, CAD 250.00, by bank transfer, whatever the run would"
  say "choose. Alex fixes the payment method T on the invoice while it is parked, and posts it; its open item keeps T:"
  api 201 '"\(.kind) \(.document_number) \(.supplier_reference): \(.status), \(.gross_amount.amount) \(.gross_amount.currency), payment method fixed: \(.payment_method)"' createSupplierInvoice \
    -body "{\"legal_entity_id\": \"$LE\", \"supplier_id\": \"$laurentian\", \"supplier_reference\": \"LP-FREIGHT-1\", \"invoice_date\": \"$TODAY\", \"payment_method\": \"T\", \"gross_amount\": {\"amount\": \"250.00\", \"currency\": \"CAD\"}, \"tax_amount\": {\"amount\": \"0.00\", \"currency\": \"CAD\"}, \"items\": [{\"account_id\": \"$(account 6000)\", \"description\": \"Freight\", \"quantity\": {\"value\": \"1\", \"unit\": \"EA\"}, \"amount\": {\"amount\": \"250.00\", \"currency\": \"CAD\"}}]}"
  FIXED_INVOICE=$(field .id)
  expect "the payment method the invoice fixes" "$(field .payment_method)" T
  say "A method the company does not pay by is refused:"
  refused 422 payables.unknown_payment_method updateSupplierInvoice -p "id=$FIXED_INVOICE" -if-match "$ETAG" -body '{"payment_method": "Q"}'
  api 200 '"\(.document_number): \(.status), journal entry \(.journal_entry_id)"' postSupplierInvoice -p "id=$FIXED_INVOICE" -if-match '"1"' -body "{\"posting_date\": \"$TODAY\"}"
  FIXED_ENTRY=$(field .journal_entry_id)
  api 200 '.items[] | select(.payment_method) | "open item \(.document_number): \(.amount.amount) \(.amount.currency), payment method fixed: \(.payment_method)"' \
    listOpenItems -p "partner_id=$laurentian" -p page_size=100
  expect "the open item's fixed payment method" "$(field "[.items[] | select(.journal_entry_id == \"$FIXED_ENTRY\") | .payment_method] | join(\" \")")" T
  QUIET=1 api 200 '' listBusinessPartners -p name_prefix=Muskoka
  muskoka=$(field '.items[0].id')
  if [ -z "$muskoka" ] || [ "$muskoka" = null ]; then fail "the migrated supplier Muskoka Office Supply is not there"; fi
  QUIET=1 api 200 '' getBusinessPartner -p "id=$muskoka"
  expect "Muskoka Office Supply's payment methods: bank transfer alone" \
    "$(field '.supplier_companies[] | select(.legal_entity_id == "'"$LE"'") | .payment_methods | join(" ")')" T
  say "Muskoka Office Supply may be paid by bank transfer alone, yet asks for its express charge MO-EXPRESS-1, CAD 180.00,"
  say "by wire. The method an invoice fixes wins over its supplier's list, which restricts the run's order alone:"
  api 201 '"\(.kind) \(.document_number) \(.supplier_reference): \(.status), \(.gross_amount.amount) \(.gross_amount.currency), payment method fixed: \(.payment_method)"' createSupplierInvoice \
    -body "{\"legal_entity_id\": \"$LE\", \"supplier_id\": \"$muskoka\", \"supplier_reference\": \"MO-EXPRESS-1\", \"invoice_date\": \"$TODAY\", \"payment_method\": \"W\", \"gross_amount\": {\"amount\": \"180.00\", \"currency\": \"CAD\"}, \"tax_amount\": {\"amount\": \"0.00\", \"currency\": \"CAD\"}, \"items\": [{\"account_id\": \"$(account 6000)\", \"description\": \"Express delivery\", \"quantity\": {\"value\": \"1\", \"unit\": \"EA\"}, \"amount\": {\"amount\": \"180.00\", \"currency\": \"CAD\"}}]}"
  WIRED_INVOICE=$(field .id)
  api 200 '"\(.document_number): \(.status), journal entry \(.journal_entry_id)"' postSupplierInvoice -p "id=$WIRED_INVOICE" -if-match "$ETAG" -body "{\"posting_date\": \"$TODAY\"}"
  WIRED_ENTRY=$(field .journal_entry_id)
  as tom
  say "Tom proposes a run in CAD without a house bank: each supplier is paid by the first method of that order"
  say "it may be paid by, through the bank ranked for the method; items that fix a method are paid by it, apart."
  api 202 '"\(.kind) \(.status)"' proposePaymentRun \
    -body "{\"legal_entity_id\": \"$LE\", \"currency\": \"CAD\", \"run_date\": \"$TODAY\", \"due_by\": \"$(date_add "$TODAY" 30)\"}"
  wait_op
  run=$(field .resource | sed 's|.*/||')
  QUIET=1 api 200 '' getPaymentRun -p "id=$run"
  etag=$ETAG
  api 200 '.items[] | "payment \(.end_to_end_id) to \(.supplier_name): \(.amount.amount) \(.amount.currency) by method \(.payment_method) (\(.payment_method_source)) from \(.house_bank_code)"' \
    listPayments -p "id=$run" -p page_size=100
  expect "Laurentian Paper, allowing T and W, paid by the run's first method it allows" \
    "$(field '.items[] | select(.supplier_name == "Laurentian Paper" and .payment_method_source == "run_order") | "\(.payment_method) \(.house_bank_code)"')" "W RBC"
  say "Laurentian Paper is paid twice: its freight invoice alone by the method it fixes, the rest by the run's order."
  expect "Laurentian Paper's freight invoice, paid apart by the method it fixes" \
    "$(field '.items[] | select(.supplier_name == "Laurentian Paper" and .payment_method_source == "item") | "\(.payment_method) \(.house_bank_code) \(.amount.amount) \([.items[].journal_entry_id] | join(" "))"')" "T MAIN 250.00 $FIXED_ENTRY"
  expect "the freight invoice in no other payment" \
    "$(field "[.items[] | select(.payment_method_source != \"item\") | .items[] | select(.journal_entry_id == \"$FIXED_ENTRY\")] | length")" 0
  say "Muskoka Office Supply's express charge is paid by wire, the method it fixes, though Muskoka's own methods lack it:"
  expect "Muskoka Office Supply's express charge, paid by the method it fixes" \
    "$(field '.items[] | select(.supplier_name == "Muskoka Office Supply" and .payment_method_source == "item") | "\(.payment_method) \(.house_bank_code) \(.amount.amount) \([.items[].journal_entry_id] | join(" "))"')" "W RBC 180.00 $WIRED_ENTRY"
  expect "the express charge in no other payment" \
    "$(field "[.items[] | select(.payment_method_source != \"item\") | .items[] | select(.journal_entry_id == \"$WIRED_ENTRY\")] | length")" 0
  expect "the methods the run order pays the suppliers allowing bank transfer alone by" \
    "$(field '[.items[] | select(.supplier_name != "Laurentian Paper" and .payment_method_source == "run_order") | "\(.payment_method) \(.house_bank_code)"] | unique | join(", ")')" "T MAIN"
  say "The run's exclusions, each with its reason (a supplier no ranked bank could pay by a method of its own"
  say "would be here with no_house_bank; every supplier of this run allows T or W):"
  api 200 '.items[] | "excluded: \(.reason_code), \(.amount.amount) \(.amount.currency), supplier \(.supplier_name)"' listPaymentExclusions -p "id=$run" -p page_size=100
  expect "suppliers left out for want of a ranked house bank" "$(field '[.items[] | select(.reason_code == "no_house_bank")] | length')" 0
  say "The pilot pays from its RBC account, so Tara, who releases runs, rejects this one; its items are free again."
  as tara
  api 200 '"run \(.document_number): \(.status)"' rejectPaymentRun -p "id=$run" -if-match "$etag" -body '{"reason": "Shown for the method order; the run from RBC pays these"}'
  expect "the run without a house bank" "$(field .status)" rejected
}

step_payment() {
  heading "Step 7 — A payment run: the method a run without a house bank pays each supplier by, an invoice's fixed method paid apart; a run proposed with the cash discount, its release approved by a second person, released by a third, the bank file"
  method_order
  as tom
  say "Tom proposes a run from the RBC account for everything due within 30 days: Tyrell's invoice less"
  say "the credit memo and the 2% discount, and the open payables migrated from SAP that are overdue."
  api 202 '"\(.kind) \(.status)"' proposePaymentRun \
    -body "{\"legal_entity_id\": \"$LE\", \"house_bank_id\": \"$RBC\", \"currency\": \"CAD\", \"run_date\": \"$TODAY\", \"due_by\": \"$(date_add "$TODAY" 30)\"}"
  wait_op
  RUN=$(field .resource | sed 's|.*/||')
  api 200 '"run \(.document_number): \(.status), \(.total_amount.amount) \(.total_amount.currency) from \(.house_bank_code), proposed by Tom"' getPaymentRun -p "id=$RUN"
  local version_etag=$ETAG
  expect "the house bank the run names" "$(field .house_bank_code)" RBC
  api 200 '.items[] | "payment \(.end_to_end_id) to \(.supplier_name): \(.amount.amount) \(.amount.currency)\(if .discount_amount.amount != "0.00" then ", discount \(.discount_amount.amount) until \(.discount_until)" else "" end)"' listPayments -p "id=$RUN" -p page_size=100
  cp "$WORK/last.json" "$WORK/payments.json"
  local tyrell
  # The payment of the walkthrough's own invoice and credit memo.
  tyrell=$(jq -r --arg a "$INVOICE_ENTRY" --arg c "$CREDIT_ENTRY" '.items[] | select(any(.items[]; .journal_entry_id == $a))
    | "\(.amount.amount) \(.discount_amount.amount) \(any(.items[]; .journal_entry_id == $c))"' "$WORK/payments.json")
  expect "the payment of TY-5001 less TY-CM-77: amount, discount, credit memo in it" "$tyrell" "4136.00 94.00 true"
  expect "the payee of TY-5001, named by the API" "$(jq -r --arg a "$INVOICE_ENTRY" '.items[] | select(any(.items[]; .journal_entry_id == $a)) | .supplier_name' "$WORK/payments.json")" "Tyrell Office Products"
  say "Each item a payment settles names its journal entry's document number, as the ledger gives it:"
  api 200 '"TY-5001 is journal entry \(.document_number)"' getJournalEntry -p "id=$INVOICE_ENTRY"
  expect "the document number of TY-5001's item in its payment" \
    "$(jq -r --arg a "$INVOICE_ENTRY" '.items[].items[] | select(.journal_entry_id == $a) | .document_number' "$WORK/payments.json")" "$(field .document_number)"
  api 200 '.items[] | "excluded: \(.reason_code), \(.amount.amount) \(.amount.currency), supplier \(.supplier_name)\(if .payment_method then ", its items fixing payment method \(.payment_method)" else "" end)"' listPaymentExclusions -p "id=$RUN" -p page_size=100
  expect "the blocked supplier left out, named by the API" "$(field '[.items[] | select(.reason_code == "supplier_blocked") | .supplier_name] | join(", ")')" "Massive Dynamic"
  say "A run for one house bank pays by no method, so it leaves Laurentian's freight invoice, which fixes T, and Muskoka's"
  say "express charge, which fixes W, out with the reason, rather than pay them another way; their other items are in the run."
  expect "the invoices fixing a method left out of the run for one house bank" \
    "$(field '[.items[] | select(.reason_code == "fixed_method_in_bank_run") | "\(.supplier_name) \(.payment_method) \(.amount.amount)"] | sort | join(", ")')" \
    "Laurentian Paper T 250.00, Muskoka Office Supply W 180.00"
  expect "suppliers left out for want of a bank account" "$(field '[.items[] | select(.reason_code == "no_bank_account")] | length')" 0
  # After the checks that read the exclusions: field reads the last answer.
  as alex
  say "Laurentian and Muskoka withdraw their charges the same day; Alex reverses both invoices, so nothing waits for"
  say "a run without a house bank."
  local withdrawn
  for withdrawn in "$FIXED_INVOICE" "$WIRED_INVOICE"; do
    QUIET=1 api 200 '' getSupplierInvoice -p "id=$withdrawn"
    api 200 '"\(.document_number): \(.status), reversal entry \(.reversal_entry_id)"' reverseSupplierInvoice -p "id=$withdrawn" -if-match "$ETAG" \
      -body "{\"reason\": \"Charge withdrawn by the supplier\", \"posting_date\": \"$TODAY\"}"
    expect "the withdrawn invoice" "$(field .status)" reversed
  done
  as tom
  say "$DP_NAME's open down payment from SAP is not netted: the run pays the supplier's open invoices in full (T72c)."
  expect "the payment to $DP_NAME" "$(jq -r --arg n "$DP_NAME" '[.items[] | select(.supplier_name == $n) | .amount.amount] | join(" ")' "$WORK/payments.json")" "$DP_PAYABLE"
  say "The migrated Canadian suppliers are in the run, paid to their Canadian accounts, each payment naming its payee:"
  jq -r '.items[] | "\(.end_to_end_id) \(.supplier_name) \(.amount.amount) \(.amount.currency)"' "$WORK/payments.json" |
    grep -E 'Laurentian|Prairie|Rideau|Fundy|Muskoka|Cabot' | sed 's/^/     payment /' || fail "the run pays none of the migrated Canadian suppliers"
  say "Tom holds the release permission too, but whoever proposes a run can never release it:"
  refused 403 identity.sod_conflict releasePaymentRun -p "id=$RUN" -if-match "$version_etag" -body '{}'
  as tara
  say "The run is above company $CC's release_dual_limit of CAD 4,000.00, so Tara cannot release it alone:"
  refused 409 payments.release_approval_required releasePaymentRun -p "id=$RUN" -if-match "$version_etag" -body '{}'
  say "She asks for a second person's approval of the release."
  api 200 '"run \(.document_number): release approval \(.release_approval.status), \(.release_approval.requested_total.amount) \(.release_approval.requested_total.currency)"' \
    submitPaymentRun -p "id=$RUN" -if-match "$version_etag"
  local request
  request=$(field .release_approval.request_id)
  say "The request leads to the run it releases:"
  api 200 '"approval request \(.object_type): \(.object_resource)"' getApprovalRequest -p "id=$request"
  expect "the request's object" "$(field .object_resource)" "/v1/payment-runs/$RUN"
  say "Tara asked for it, so she cannot approve it; Tom proposed the run, so he cannot either:"
  refused 403 workflow.own_request approveApprovalRequest -p "id=$request" -if-match "$ETAG" -body '{}'
  as tom
  QUIET=1 api 200 '' getApprovalRequest -p "id=$request"
  refused 403 workflow.segregation_of_duties approveApprovalRequest -p "id=$request" -if-match "$ETAG" -body '{}'
  say "Fiona, the finance director, approves. The approval releases nothing, and she cannot release the run herself:"
  approve fiona "$request" "Within the cash plan"
  api 200 '"run \(.document_number): \(.status), release \(.release_approval.status)"' getPaymentRun -p "id=$RUN"
  expect "run status after the approval" "$(field .status)" proposed
  expect "release approval" "$(field .release_approval.status)" approved
  version_etag=$ETAG
  refused 403 identity.sod_conflict releasePaymentRun -p "id=$RUN" -if-match "$version_etag" -body '{}'
  say "Tara releases it."
  as tara
  api 202 '"\(.kind) \(.status)"' releasePaymentRun -p "id=$RUN" -if-match "$version_etag" -body '{"comment": "Checked against the cash plan"}'
  wait_op
  api 200 '"run \(.document_number): \(.status)"' getPaymentRun -p "id=$RUN"
  expect "run status" "$(field .status)" released
  say "Each payment, once posted, names its posting by its document number, its house bank by its code and the"
  say "supplier's account by its last four characters and its bank; the account number never appears:"
  api 200 '.items[] | select(.supplier_name == "Tyrell Office Products" or .supplier_name == "Laurentian Paper") | "payment \(.end_to_end_id) to \(.supplier_name): posted as \(.journal_entry_number), from \(.house_bank_code), to \(.bank_account_scheme) …\(.bank_account_last4)\(if .bank_account_institution_number then " at \(.bank_account_institution_number)-\(.bank_account_transit_number)" else "" end), holder \(.bank_account_holder_name)"' \
    listPayments -p "id=$RUN" -p page_size=100
  cp "$WORK/last.json" "$WORK/posted.json"
  expect "the house bank every payment names" "$(field '[.items[].house_bank_code] | unique | join(" ")')" RBC
  expect "payments without their posting's number" "$(field '[.items[] | select(.journal_entry_number == null)] | length')" 0
  expect "Laurentian Paper's account, by its scheme, bank and branch" \
    "$(field '.items[] | select(.supplier_name == "Laurentian Paper") | "\(.bank_account_scheme) \(.bank_account_institution_number) \(.bank_account_transit_number)"')" "ca 001 00010"
  local posting
  posting=$(jq -r --arg a "$INVOICE_ENTRY" '.items[] | select(any(.items[]; .journal_entry_id == $a)) | .journal_entry_id' "$WORK/posted.json")
  api 200 '"the payment of TY-5001 is journal entry \(.document_number)"' getJournalEntry -p "id=$posting"
  expect "the posting's number the payment of TY-5001 gives" \
    "$(jq -r --arg a "$INVOICE_ENTRY" '.items[] | select(any(.items[]; .journal_entry_id == $a)) | .journal_entry_number' "$WORK/posted.json")" "$(field .document_number)"
  api 200 '.items[] | "bank file \(.id): \(.format) from \(.house_bank_code), \(.status)"' listBankFiles -p "payment_run_id=$RUN"
  expect "the house bank every bank file names" "$(field '[.items[].house_bank_code] | unique | join(" ")')" RBC
  local file
  file=$(field '.items[0].id')
  say "The pain.001 file, read from the API and written to a file:"
  api 200 '' getBankFileContent -p "id=$file" -out pain001.xml
  grep -q 'urn:iso:std:iso:20022:tech:xsd:pain.001.001.03' "$WORK/pain001.xml" || fail "the bank file is not pain.001"
  grep -E '<(MsgId|NbOfTxs|CtrlSum|ReqdExctnDt)>' "$WORK/pain001.xml" | sed -n '1,4p' | sed -E 's/^ */     /'
  grep -A 12 "<EndToEndId>$CC-" "$WORK/pain001.xml" | grep -E 'EndToEndId|InstdAmt|<Nm>' | sed -n '1,9p' | sed -E 's/^ */       /'
  say "A Canadian supplier's bank is named by its CACPA member id, 0 + institution + transit (its account goes in Othr):"
  grep -B 12 '<Nm>Laurentian Paper</Nm>' "$WORK/pain001.xml" | grep -E '<EndToEndId>|<Cd>CACPA</Cd>|<MmbId>|<Nm>' | sed -E 's/^ */       /'
  grep -B 12 '<Nm>Laurentian Paper</Nm>' "$WORK/pain001.xml" | grep -q '<MmbId>000100010</MmbId>' ||
    fail "the bank file does not name Laurentian Paper's bank by its CACPA member id"
  passed
}

# statement writes a camt.053 statement of the RBC account for today: an
# opening balance and one debit per payment of the run, with its
# end-to-end id, as the bank reports them.
statement() {
  local opening=$1 total=0 closing a
  for a in $(jq -r '.items[].amount.amount' "$WORK/payments.json"); do
    total=$((total + $(cents "$a")))
  done
  closing=$(amount $(($(cents "$opening") - total)))
  {
    printf '<?xml version="1.0" encoding="UTF-8"?>\n'
    printf '<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.02"><BkToCstmrStmt>\n'
    printf '<GrpHdr><MsgId>RBC-%s</MsgId><CreDtTm>%sT23:00:00</CreDtTm></GrpHdr>\n' "$TODAY" "$TODAY"
    printf '<Stmt><Id>RBC-1000001-%s</Id><ElctrncSeqNb>1</ElctrncSeqNb><Acct><Id><Othr><Id>1000001</Id></Othr></Id><Ccy>CAD</Ccy></Acct>\n' "$TODAY"
    printf '<Bal><Tp><CdOrPrtry><Cd>OPBD</Cd></CdOrPrtry></Tp><Amt Ccy="CAD">%s</Amt><CdtDbtInd>CRDT</CdtDbtInd><Dt><Dt>%s</Dt></Dt></Bal>\n' "$opening" "$TODAY"
    printf '<Bal><Tp><CdOrPrtry><Cd>CLBD</Cd></CdOrPrtry></Tp><Amt Ccy="CAD">%s</Amt><CdtDbtInd>CRDT</CdtDbtInd><Dt><Dt>%s</Dt></Dt></Bal>\n' "$closing" "$TODAY"
    # The bank lost the last payment's end-to-end id (ISO 20022's NOTPROVIDED): no rule finds it (T73g2).
    jq -r --arg d "$TODAY" '(.items | length) as $n | .items | to_entries[] | (if .key == $n - 1 then "NOTPROVIDED" else .value.end_to_end_id end) as $e2e | .value
      | "<Ntry><Amt Ccy=\"CAD\">\(.amount.amount)</Amt><CdtDbtInd>DBIT</CdtDbtInd><Sts>BOOK</Sts><BookgDt><Dt>\($d)</Dt></BookgDt><ValDt><Dt>\($d)</Dt></ValDt><BkTxCd><Domn><Cd>PMNT</Cd><Fmly><Cd>ICDT</Cd><SubFmlyCd>ESCT</SubFmlyCd></Fmly></Domn></BkTxCd><NtryDtls><TxDtls><Refs><EndToEndId>\($e2e)</EndToEndId></Refs></TxDtls></NtryDtls></Ntry>"' "$WORK/payments.json"
    printf '</Stmt></BkToCstmrStmt></Document>\n'
  } >"$WORK/camt053.xml"
  CLOSING=$closing
}

# empty_statement DATE BALANCE writes RBC's camt.053 statement of the
# day with no entries, opening and closing at the balance.
empty_statement() {
  {
    printf '<?xml version="1.0" encoding="UTF-8"?>\n'
    printf '<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.02"><BkToCstmrStmt>\n'
    printf '<GrpHdr><MsgId>RBC-%s</MsgId><CreDtTm>%sT23:00:00</CreDtTm></GrpHdr>\n' "$1" "$1"
    printf '<Stmt><Id>RBC-1000001-%s</Id><ElctrncSeqNb>0</ElctrncSeqNb><Acct><Id><Othr><Id>1000001</Id></Othr></Id><Ccy>CAD</Ccy></Acct>\n' "$1"
    printf '<Bal><Tp><CdOrPrtry><Cd>OPBD</Cd></CdOrPrtry></Tp><Amt Ccy="CAD">%s</Amt><CdtDbtInd>CRDT</CdtDbtInd><Dt><Dt>%s</Dt></Dt></Bal>\n' "$2" "$1"
    printf '<Bal><Tp><CdOrPrtry><Cd>CLBD</Cd></CdOrPrtry></Tp><Amt Ccy="CAD">%s</Amt><CdtDbtInd>CRDT</CdtDbtInd><Dt><Dt>%s</Dt></Dt></Bal>\n' "$2" "$1"
    printf '</Stmt></BkToCstmrStmt></Document>\n'
  } >"$WORK/camt053-empty.xml"
}

# recurring_rent is step 8's recurring entry (ADR-0056): Tom saves the
# monthly rent as an entry template whose schedule starts on Maple's date
# in Toronto; the ERP parks that date's entry for him, once however often
# its worker runs, and he finds it among his entries and posts it.
recurring_rent() {
  say "Tom books Maple's office rent, CAD 4,500.00, every month. He saves it as an entry template with a monthly"
  say "schedule from today, Maple's date in Toronto. The ERP parks each month's entry for him and never posts it:"
  say "Tom posts it, within the CAD 10,000.00 limit, or someone else approves it (product owner 2026-11-01)."
  local toronto rent sap_bank tpl entry i n
  toronto=$(company_day)
  rent=$(account 6000)
  sap_bank=$(account 100000)
  api 201 '"template \(.name): \(.status), \(.schedule.frequency) on day \(.schedule.day_of_month) from \(.schedule.starts_on), next due \(.next_due_on)"' \
    createEntryTemplate -body "{\"legal_entity_id\": \"$LE\", \"name\": \"Office rent\", \"currency\": \"CAD\", \"header_text\": \"Monthly office rent\", \"schedule\": {\"frequency\": \"monthly\", \"day_of_month\": $((10#${toronto:8:2})), \"starts_on\": \"$toronto\"}, \"lines\": [{\"account_id\": \"$rent\", \"amount\": {\"amount\": \"4500.00\", \"currency\": \"CAD\"}}, {\"account_id\": \"$sap_bank\", \"amount\": {\"amount\": \"-4500.00\", \"currency\": \"CAD\"}}]}"
  tpl=$(field .id)
  expect "the template's first due date, Maple's today" "$(field .next_due_on)" "$toronto"
  say "Without a workflow engine the server's recurring entries worker parks due dates every second (with one,"
  say "the dispatcher starts a workflow per template and date, in date order). Today's entry appears, parked for Tom:"
  n=0
  for i in $(seq 1 120); do
    QUIET=1 api 200 '' listParkedJournalEntries -p "template_id=$tpl"
    n=$(field '.items | length')
    [ "$n" = 0 ] || break
    sleep 0.25
  done
  expect "entries parked from the template (attempt $i)" "$n" 1
  sleep 3 # three more of the worker's rounds: they find the date parked and park nothing
  api 200 '.items[] | "parked entry \(.id[0:8])…: \(.status), \(.created_by_kind) for the \(.occurrence_date) date, owner \(.owner_id[0:8])…, \(.lines[0].amount.amount) \(.lines[0].amount.currency)"' \
    listParkedJournalEntries -p "template_id=$tpl"
  expect "entries parked from the template after more rounds" "$(field '.items | length')" 1
  expect "the entry's date" "$(field '.items[0].occurrence_date')" "$toronto"
  expect "who parked it" "$(field '.items[0].created_by_kind')" system
  expect "the template it names" "$(field '.items[0].template_name')" "Office rent"
  entry=$(field '.items[0].id')
  api 200 '"template \(.name): last parked \(.last_occurrence_on), next due \(.next_due_on)"' getEntryTemplate -p "id=$tpl"
  expect "the template's last date" "$(field .last_occurrence_on)" "$toronto"
  say "Tom's inbox: his entries waiting for him to post them, the rent among them, besides the event"
  say "ParkedJournalEntryChanged that names the template and the date:"
  api 200 '.items[] | "parked entry \(.id[0:8])…: \(.header_text // "") \(.posting_date)\(if .template_id then ", from template \(.template_name)" else "" end)"' \
    listParkedJournalEntries -p owned_by=me -p status=parked
  expect "the rent in Tom's inbox" "$(jq -r --arg e "$entry" '[.items[] | select(.id == $e)] | length' "$WORK/last.json")" 1
  QUIET=1 api 200 '' getParkedJournalEntry -p "id=$entry"
  api 200 '"parked entry: \(.status), journal entry \(.journal_entry_id)"' postParkedJournalEntry -p "id=$entry" -if-match "$ETAG" -body '{}'
  expect "the rent entry's status" "$(field .status)" posted
}

# partners_named reads a posted and a parked entry with the partners
# their lines name (expand=partners, T73h3): by number and name, with no
# call per partner, as the workspace's entry pages read them. The parked
# entry is cancelled, so it books nothing.
partners_named() {
  say "A journal line names its partner by id. Read with its partners (expand=partners), an entry names each"
  say "by number and name, as the workspace's entry pages read it, with no call of their own (T73h3):"
  api 200 '.expanded.partners[] | "TY-5001'"'"'s entry: partner \(.number) \(.name)"' getJournalEntry -p "id=$INVOICE_ENTRY" -p expand=partners
  expect "the partner TY-5001's entry names" "$(field '[.expanded.partners[].name] | join(", ")')" "Tyrell Office Products"
  say "So does a parked entry: Tom parks an accrual for Tyrell, reads it with its partner, and cancels it."
  local accrual
  api 201 '"parked entry \(.id[0:8])…: \(.status)"' parkJournalEntry \
    -body "{\"legal_entity_id\": \"$LE\", \"document_date\": \"$TODAY\", \"posting_date\": \"$TODAY\", \"currency\": \"CAD\", \"header_text\": \"Accrual for Tyrell, shown and cancelled\", \"lines\": [{\"account_id\": \"$(account 6000)\", \"amount\": {\"amount\": \"75.00\", \"currency\": \"CAD\"}}, {\"account_id\": \"$(account 211000)\", \"amount\": {\"amount\": \"-75.00\", \"currency\": \"CAD\"}, \"partner_id\": \"$SUPPLIER\"}]}"
  accrual=$(field .id)
  api 200 '.expanded.partners[] | "the accrual: partner \(.number) \(.name)"' getParkedJournalEntry -p "id=$accrual" -p expand=partners
  expect "the partner the parked accrual names" "$(field '[.expanded.partners[].name] | join(", ")')" "Tyrell Office Products"
  api 200 '"parked entry: \(.status)"' cancelParkedJournalEntry -p "id=$accrual" -if-match "$ETAG" -body '{"reason": "Shown, not booked"}'
  expect "the accrual's status" "$(field .status)" cancelled
}

step_statement() {
  heading "Step 8 — The bank statement: imported, a gap accepted, matched by rules and by hand, posted with approval, reconciled"
  as tom
  local bank sap_bank
  bank=$(account 1000)
  sap_bank=$(account 100000)
  say "The RBC account is funded from SAP's operating bank account with CAD 500,000.00. A manual entry"
  say "above the CAD 10,000.00 limit is parked and posted only when the controller approves it."
  api 201 '"parked entry \(.id): \(.status)"' parkJournalEntry \
    -body "{\"legal_entity_id\": \"$LE\", \"document_date\": \"$TODAY\", \"posting_date\": \"$TODAY\", \"currency\": \"CAD\", \"header_text\": \"Fund the RBC operating account\", \"lines\": [{\"account_id\": \"$bank\", \"amount\": {\"amount\": \"500000.00\", \"currency\": \"CAD\"}}, {\"account_id\": \"$sap_bank\", \"amount\": {\"amount\": \"-500000.00\", \"currency\": \"CAD\"}}]}"
  local parked
  parked=$(field .id)
  api 200 '"\(.status): approval request \(.approval_request_id)"' postParkedJournalEntry -p "id=$parked" -if-match "$ETAG" -body '{}'
  expect "parked entry status" "$(field .status)" pending_approval
  approve carla "$(field .approval_request_id)" "Transfer agreed with the bank"
  as tom
  api 200 '"parked entry: \(.status), journal entry \(.journal_entry_id)"' getParkedJournalEntry -p "id=$parked"
  expect "parked entry status" "$(field .status)" posted
  recurring_rent
  partners_named
  local yesterday
  yesterday=$(date_add "$TODAY" -1)
  say "RBC's statement of $yesterday, before the funding, closes the empty account at 0.00. Tom imports it:"
  empty_statement "$yesterday" 0.00
  api 201 '.statements[] | "statement \(.statement_ref): \(.status), closing \(.closing_balance.amount) \(.closing_balance.currency)"' \
    importBankStatements -body "{\"house_bank_id\": \"$RBC\", \"format\": \"camt.053\"}" -file file=@camt053-empty.xml
  say "RBC's end-of-day statement (camt.053) shows the run's payments leaving the account."
  statement 500000.00
  say "  $(grep -c '<Ntry>' "$WORK/camt053.xml") entries, opening 500000.00, closing $CLOSING CAD"
  say "Tom imports it, the file as a part of the upload's form. It opens at 500,000.00, not at yesterday's 0.00:"
  say "the bank's statement that carried the funding never arrived, so it is in gap (ADR-0047 Continuity)."
  api 201 '' importBankStatements -body "{\"house_bank_id\": \"$RBC\", \"format\": \"camt.053\"}" -file file=@camt053.xml
  local st
  st=$(field '.statements[0].id')
  if [ -z "$st" ] || [ "$st" = null ]; then fail "the statement was not imported"; fi
  excerpt '.statements[] | "statement \(.statement_ref): \(.status), opening \(.opening_balance.amount), \(.line_count) lines"'
  expect "statement status" "$(field '.statements[0].status')" gap
  say "The rules matched each payment by its end-to-end id but the last, whose id the bank lost; nothing posts"
  say "for RBC while the gap stands:"
  api 200 '"statement \(.statement_ref): \(.status); lines \([.lines[]?.status] | group_by(.) | map("\(.[0]) \(length)") | join(", "))"' getBankStatement -p "id=$st"
  expect "lines not matched, none posted" "$(field '[.lines[] | select(.status != "matched") | .status] | join(",")')" unmatched
  refused 409 payments.statement_gap postBankStatementLine -p "id=$st" -p line_no=1 -if-match "$ETAG"
  say "The bank cannot send the missing statement again; the funding is in the ledger already. Tom accepts the gap"
  say "with the reason, audited and published (BankStatementGapAccepted), and agents could not have done it:"
  QUIET=1 api 200 '' getBankStatement -p "id=$st"
  api 200 '"statement \(.statement_ref): \(.status), gap accepted by \(.gap_accepted_by[0:8])…: \(.gap_reason)"' acceptBankStatementGap -p "id=$st" -if-match "$ETAG" \
    -body '{"reason": "RBC cannot redeliver the statement that carried the funding; the transfer is in the ledger (parked entry, approved by Carla)."}'
  expect "statement status after the acceptance" "$(field .status)" imported
  say "Tom runs the rules again: lines up to CAD 10,000.00 post at once; the others wait for the controller:"
  api 200 '"statement \(.statement_ref): lines \([.lines[]?.status] | group_by(.) | map("\(.[0]) \(length)") | join(", "))"' matchBankStatement -p "id=$st" -if-match "$ETAG"
  expect "lines still matched after the run" "$(field '[.lines[] | select(.status == "matched")] | length')" 0
  match_by_hand "$st" "$(field '.lines[] | select(.status == "unmatched") | .line_no')"
  api 200 '.items[] | "line \(.line_no): \(.status) \(if .rule then "by rule \(.rule)" else "by hand" end)\(if .approval_request_id then ", approval request \(.approval_request_id[0:8])…" else "" end)"' listBankStatementMatches -p "id=$st"
  local r requests
  requests=$(field '.items[] | select(.approval_request_id != null and .status == "matched") | .approval_request_id')
  clearing_object "$st" "${requests%%$'\n'*}"
  for r in $requests; do
    approve carla "$r" "Payment of run $RUN"
  done
  as tom
  api 200 '"statement \(.statement_ref): \(.status); lines \([.lines[]?.status] | group_by(.) | map("\(.[0]) \(length)") | join(", "))"' getBankStatement -p "id=$st"
  expect "statement status" "$(field .status)" reconciled
  expect "the house bank the statement names" "$(field .house_bank_code)" RBC
  say "The reconciliation report of the RBC account:"
  api 200 '.summary.figures | "as of \(.as_of): statement closing \(.closing_balance.amount), ledger \(.ledger_balance.amount), in transit \(.in_transit.amount), unexplained \(.unexplained.amount) \(.closing_balance.currency)"' getBankReconciliation -p "id=$RBC"
  expect "the house bank the report names" "$(field .house_bank_code)" RBC
  expect "unexplained difference" "$(field .summary.figures.unexplained.amount)" 0.00
  expect "ledger balance" "$(field .summary.figures.ledger_balance.amount)" "$CLOSING"
  passed
}

# clearing_object STATEMENT REQUEST has Carla read a bank clearing's
# object where its request places it: the match is read with its
# statement's (object_resource, as the workspace reads it, T73d3).
clearing_object() {
  local match
  say "Carla opens a posting's request. Its object is the line's match, read with its statement's matches:"
  as carla
  api 200 '"approval request \(.object_type) \(.object_id): \(.object_resource)"' getApprovalRequest -p "id=$2"
  match=$(field .object_id)
  expect "where the request places it" "$(field .object_resource)" "/v1/bank-statements/$1"
  api 200 '.items[] | select(.id == "'"$match"'") | "line \(.line_no): \(.status) by \(.kind), posting on \(.posting_date)"' listBankStatementMatches -p "id=$1"
  expect "the match's request" "$(field '.items[] | select(.id == "'"$match"'") | .approval_request_id')" "$2"
}

# match_by_hand STATEMENT LINE: Tom finds the payment the rules did not
# find among those the statement's lines may clear, matches the line to it
# by hand and posts it (T73g2, ADR-0047).
match_by_hand() {
  local st=$1 line=$2 amount pay
  amount=$(field ".lines[] | select(.line_no == $line) | .amount.amount")
  amount=${amount#-}
  say "Line $line, CAD $amount, is still unmatched. Tom searches the payments from RBC that no line holds, of that"
  say "amount, people only (agents propose from the rules' candidates):"
  api 200 '.items[] | "payment \(.end_to_end_id) to \(.supplier_name): \(.amount.amount) \(.amount.currency), journal entry \(.journal_entry_number)"' \
    listBankStatementPayments -p "id=$st" -p "amount_min=$amount" -p "amount_max=$amount"
  expect "payments of the amount no line holds" "$(field '.items | length')" 1
  expect "the payment found, the run's last" "$(field '.items[0].end_to_end_id')" "$(jq -r '.items[-1].end_to_end_id' "$WORK/payments.json")"
  pay=$(field '.items[0].id')
  QUIET=1 api 200 '' getBankStatement -p "id=$st"
  say "He matches the line to it by hand, with his reason; whoever matched may not approve its posting:"
  api 201 '"line \(.line_no): \(.status) by hand (\(.kind)), payment \(.targets[0].payment_id[0:8])…: \(.reason)"' matchBankStatementLine -p "id=$st" -p "line_no=$line" \
    -if-match "$ETAG" -body "{\"payment_ids\": [\"$pay\"], \"reason\": \"RBC dropped the end-to-end id; the amount and the payee's remittance name the payment.\"}"
  expect "the match's kind" "$(field .kind)" manual
  api 200 '"line \(.line_no): posted\(if .journal_entry_id then " in journal entry \(.journal_entry_id[0:8])…" else ", waiting for approval request \(.approval_request_id[0:8])…" end)"' \
    postBankStatementLine -p "id=$st" -p "line_no=$line" -if-match "$ETAG"
  say "Nothing is left for the line to clear: the search no longer offers the payment."
  QUIET=1 api 200 '' listBankStatementPayments -p "id=$st" -p "amount_min=$amount" -p "amount_max=$amount"
  expect "payments of the amount no line holds, after the match" "$(field '.items | length')" 0
}

step_fx() {
  heading "Step 9 — The USD house bank of the CAD company, and the month-end FX revaluation preview"
  as tom
  local first="${TODAY:0:7}-01" eom usd_bank sap_bank
  eom=$(month_end)
  usd_bank=$(account 1030)
  sap_bank=$(account 100000)
  say "The Bank of Canada's USD rate is maintained: 1.35 from January, 1.40 at the month's end."
  api 201 '"\(.from_currency)/\(.to_currency) \(.rate) from \(.valid_from) (\(.source))"' createExchangeRate \
    -body "{\"rate_type\": \"average\", \"from_currency\": \"USD\", \"to_currency\": \"CAD\", \"valid_from\": \"$YEAR-01-01\", \"rate\": \"1.35\", \"source\": \"BOC\"}"
  api 201 '"\(.from_currency)/\(.to_currency) \(.rate) from \(.valid_from) (\(.source))"' createExchangeRate \
    -body "{\"rate_type\": \"average\", \"from_currency\": \"USD\", \"to_currency\": \"CAD\", \"valid_from\": \"$eom\", \"rate\": \"1.40\", \"source\": \"BOC\"}"
  say "USD 5,000.00 goes into the RBC USD account, booked at 1.35:"
  api 201 '"journal entry \(.document_number): \([.lines[] | "\(.amount.amount) USD = \(.amount_lc.amount) CAD"] | join(" / "))"' postJournalEntry \
    -body "{\"legal_entity_id\": \"$LE\", \"document_date\": \"$first\", \"posting_date\": \"$first\", \"currency\": \"USD\", \"header_text\": \"Fund the RBC USD account\", \"lines\": [{\"account_id\": \"$usd_bank\", \"amount\": {\"amount\": \"5000.00\", \"currency\": \"USD\"}}, {\"account_id\": \"$sap_bank\", \"amount\": {\"amount\": \"-5000.00\", \"currency\": \"USD\"}}]}"
  say "The USD bank pays in USD only: a CAD run from it is refused."
  refused 422 payments.currency_mismatch proposePaymentRun \
    -body "{\"legal_entity_id\": \"$LE\", \"house_bank_id\": \"$RBCUS\", \"currency\": \"CAD\", \"run_date\": \"$TODAY\", \"due_by\": \"$TODAY\"}"
  say "At the month's end the account is worth USD 5,000.00 x 1.40 = CAD 7,000.00: the preview shows the gain."
  api 200 '.items[] | "account \(.account_id | acct): \(.amount.amount) \(.amount.currency), booked \(.amount_lc.amount), valued \(.valued_lc.amount) at \(.rate | sub("0+$"; "")): \(.direction) \(.difference_lc.amount) CAD"' previewRevaluation \
    -body "{\"legal_entity_id\": \"$LE\", \"key_date\": \"$eom\"}"
  expect "the bank account's revaluation" \
    "$(jq -r --arg a "$usd_bank" '.items[] | select(.account_id == $a) | "\(.direction) \(.difference_lc.amount)"' "$WORK/last.json")" "gain 250.00"
  say "SAP's USD invoice that the migration loaded in USD (step 3) is valued as one the platform posted: USD 1,000.00"
  say "booked at CAD 1,350.00 is owed at 1.40, CAD 1,400.00, a loss of CAD 50.00 on the payables account."
  expect "the migrated USD invoice's revaluation" \
    "$(jq -r --arg e "$FX_ENTRY" '.items[] | select(.journal_entry_id == $e) | "\(.amount.amount) \(.amount_lc.amount) \(.direction) \(.difference_lc.amount)"' "$WORK/last.json")" \
    "-1000.00 -1350.00 loss -50.00"
  local fx_item
  fx_item=$(jq -c --arg e "$FX_ENTRY" '.items[] | select(.journal_entry_id == $e)' "$WORK/last.json")
  say "The item names its entry by document number and its supplier by number and name (T73i2):"
  jq -r '"     entry \(.document_number) line \(.line_no), supplier \(.partner_number) \(.partner_name)"' <<<"$fx_item"
  QUIET=1 api 200 '' getJournalEntry -p "id=$FX_ENTRY"
  expect "the item's document number" "$(jq -r .document_number <<<"$fx_item")" "$(field .document_number)"
  QUIET=1 api 200 '' getBusinessPartner -p "id=$(jq -r .partner_id <<<"$fx_item")"
  expect "the item's supplier" "$(jq -r '"\(.partner_number) \(.partner_name)"' <<<"$fx_item")" "$(field '"\(.number) \(.name)"')"
  passed
}

step_ledgers() {
  heading "Step 10 — Parallel ledgers: the invoice's posting is in 0L and in 2L"
  as carla
  api 200 '.items[] | "\(.code) \(.id)"' listLedgers
  local l0 l2 je fees retained
  fees=$(account 6100)
  retained=$(account 3100)
  l0=$(field '.items[] | select(.code == "0L") | .id')
  l2=$(field '.items[] | select(.code == "2L") | .id')
  QUIET=1 api 200 '' getSupplierInvoice -p "id=$INVOICE"
  je=$(field .journal_entry_id)
  say "Tyrell's invoice of step 5 posted one entry to each ledger the company keeps, in one posting group:"
  api 200 '"0L: entry \(.document_number) \(.document_type), posting group \(.posting_group_id[0:8])…, \(.lines | length) lines"' getJournalEntry -p "id=$je"
  local group
  group=$(field .posting_group_id)
  expect "ledger of the entry" "$(field .ledger_id)" "$l0"
  api 200 '.items[] | "2L: entry \(.document_number) \(.document_type), posting group \(.posting_group_id[0:8])…, \(.lines | length) lines"' listJournalEntries \
    -p "legal_entity_id=$LE" -p "ledger_id=$l2" -p "source_document_id=$INVOICE"
  expect "2L's copy of the posting" "$(field '.items[0].posting_group_id')" "$group"
  say "A local-GAAP adjustment posts to 2L alone, and the ledger reconciliation shows the difference:"
  api 201 '"2L only: entry \(.document_number), \(.header_text)"' postJournalEntry \
    -body "{\"legal_entity_id\": \"$LE\", \"ledger_id\": \"$l2\", \"document_date\": \"$TODAY\", \"posting_date\": \"$TODAY\", \"currency\": \"CAD\", \"header_text\": \"ASPE: expense the implementation fees\", \"lines\": [{\"account_id\": \"$fees\", \"amount\": {\"amount\": \"1200.00\", \"currency\": \"CAD\"}}, {\"account_id\": \"$retained\", \"amount\": {\"amount\": \"-1200.00\", \"currency\": \"CAD\"}}]}"
  local adjustment
  adjustment=$(field .id)
  api 200 '.items[] | "account \(.account_number) \(.account_name): 0L \(.balance.amount), 2L \(.compare_balance.amount), difference \(.difference.amount) \(.currency)"' listLedgerReconciliation \
    -p "legal_entity_id=$LE" -p "compare_ledger_id=$l2" -p "fiscal_year=$YEAR"
  expect "accounts that differ" "$(field '.items | length')" 2
  expect "the fees account, named by the reconciliation (T73i2)" "$(field "[.items[] | select(.account_id == \"$fees\") | .account_number] | join(\" \")")" 6100
  say "The web workspace downloads the reconciliation as CSV (T73j4): it reads these rows in pages of 500 until"
  say "the last, every page before the file's first byte, so a file is never cut short:"
  api 200 '"\(.items | length) account(s) that differ on the page, \(if .next_page_token then "more pages to read" else "the last page" end)"' \
    listLedgerReconciliation -p "legal_entity_id=$LE" -p "compare_ledger_id=$l2" -p "fiscal_year=$YEAR" -p page_size=500
  expect "the download's pages after the first" "$(field '.next_page_token // "none"')" none
  say "The adjustment's audit history, which the workspace downloads too, its actors named in one read:"
  api 200 '.items | reverse | .[] | "\(.occurred_at)  \(.actor_id)  \(.action)  \(.outcome)"' listAuditEvents -p "object_id=$adjustment" \
    -p page_size=500 | name_actors
  expect "the adjustment's posting in its history" "$(field '[.items[] | select(.action == "ledger.journal_entry.post")] | length')" 1
  passed
}

# billing creates the pilot's billing system as a service principal that
# posts journal entries in every company, and mints its token.
billing() {
  as ada
  api 201 '"role \(.code), \(.name)"' createRole -body '{"code": "billing_integration", "name": "Billing integration"}'
  local role id
  role=$(field .id)
  grant "$role" ledger.entry.post
  api 201 '"principal \(.display_name): \(.kind)"' createPrincipal \
    -body '{"kind": "service", "external_subject": "billing", "display_name": "Maple billing system", "all_companies": true}'
  id=$(field .id)
  QUIET=1 api 201 '' createRoleAssignment -body "{\"principal_id\": \"$id\", \"role_id\": \"$role\"}"
  token billing "Maple billing system"
  printf '%s\tMaple billing system\n' "$id" >>"$WORK/names"
}

step_tax() {
  heading "Step 11 — Tax: the billing system's sales entries, their tax checked by the ledger, large ones parked for a person, and the return that files them, reviewed first by the tax agent"
  as carla
  api 200 '.items[] | select(.country == "CA") | "pack CA \(.version): \(.contents.tax_codes) tax codes; signed off: \(.sign_off // {} | if .tax_specialist then "\(.tax_specialist), \(.date)" else "no" end)"' listCountryPacks
  expect "Canada 2026.4's tax codes" "$(field '.items[] | select(.country == "CA" and .version == "2026.4") | .contents.tax_codes')" 10
  expect "Canada 2026.5's sign-off" "$(field '.items[] | select(.country == "CA" and .version == "2026.5") | "\(.signed_off) \(.sign_off.tax_specialist) \(.sign_off.date)"')" \
    "true Musto Ibrahim 2026-11-02"
  expect "Canada $CA_PACK's sign-off" "$(field '.items[] | select(.country == "CA" and .version == "'"$CA_PACK"'") | "\(.signed_off) \(.sign_off.tax_specialist) \(.sign_off.date)"')" \
    "true Musto Ibrahim 2026-11-09"
  say "Canada pack 2026.4 brought the purchase codes for GST/HST, QST and PST (T62g), each with all its dated rates,"
  say "backed by the pack's sources; 2026.5 is the same pack signed off by the tax specialist (T62h), so production"
  say "applies it. $CA_PACK carries the GST/HST return's lines (T61j), signed off too; step 2 applied it. Nova Scotia's HST:"
  api 200 '.items[0].document.companies[] | select(.code == "'"$CC"'") | .tax_codes[] | select(.code == "HNS") |
    "tax code \(.code) \(.description): \([.components[] | .key as $k | .account as $a | .rates[] | "\($k) \(.rate_percent)% from \(.valid_from)\(if .valid_to then " to \(.valid_to)" else "" end) to \($a)"] | join("; "))"' \
    listConfigVersions -p is_active=true
  expect "Nova Scotia's HST rates" \
    "$(field '[.items[0].document.companies[] | select(.code == "'"$CC"'") | .tax_codes[] | select(.code == "HNS") | .components[].rates[] | "\(.rate_percent) \(.valid_from) \(.valid_to // "open")"] | join(", ")')" \
    "15 2013-04-01 2025-03-31, 14 2025-04-01 open"
  say "The pack's codes are for purchases; step 2 configured one output code for the pilot's sales: GO5, GST at 5%"
  say "from 1 January last year, to the pack's 2300 GST/HST payable."
  say "The ERP issues no sales invoices before phase 3, so the pilot's billing system posts its sales as journal"
  say "entries (T61f), as a service principal of its own that holds only ledger.entry.post:"
  billing
  accounts
  as billing
  local cash sales gst
  cash=$(account 100000)
  sales=$(account 4000)
  gst=$(account 2300)
  # sale TEXT LINES [TAX_DATE] is a sales entry of today in CAD with the
  # lines' JSON, taxed on TAX_DATE when given.
  sale() {
    printf '{"legal_entity_id": "%s", "document_date": "%s", "posting_date": "%s", %s"currency": "CAD", "header_text": "%s", "lines": [%s]}' \
      "$LE" "${3:-$TODAY}" "$TODAY" "${3:+\"tax_date\": \"$3\", }" "$1" "$2"
  }
  line() {
    printf '{"account_id": "%s", "amount": {"amount": "%s", "currency": "CAD"}%s}' "$1" "$2" "${3:+, \"tax_code\": \"$3\"}"
  }
  local entry='"journal entry \(.document_number): \([.lines[] | "\(.amount.amount)\(if .tax_code then " \(.tax_code)" else "" end)"] | join(" / "))"'
  say "A daily summary of four invoices of CAD 100.10, each taxed 5.01 by the billing system, carries a base line"
  say "per invoice: its 20.04 is within one cent per base line of 5% of 400.40 (20.02), so it posts (product owner,"
  say "2026-10-22); its debits, CAD 420.44, are within the company's service posting limit of CAD 1,000.00."
  api 201 "$entry" postJournalEntry -body "$(sale "Billing: summary of invoices 1044-1047" "$(line "$cash" 420.44), $(line "$sales" -100.10 GO5), $(line "$sales" -100.10 GO5), $(line "$sales" -100.10 GO5), $(line "$sales" -100.10 GO5), $(line "$gst" -20.04 GO5)")"
  say "One base line of 400.40 with 20.04 would be refused, and the ledger calculates 5% of a base itself at"
  say "the rate of the tax date and holds the tax lines to it:"
  refused 422 ledger.output_tax_mismatch postJournalEntry \
    -body "$(sale "Billing: one base line" "$(line "$cash" 420.44), $(line "$sales" -400.40 GO5), $(line "$gst" -20.04 GO5)")"
  refused 422 ledger.output_tax_mismatch postJournalEntry \
    -body "$(sale "Billing: invoice 1043" "$(line "$cash" 1060.00), $(line "$sales" -1000.00 GO5), $(line "$gst" -60.00 GO5)")"
  say "Invoice 1042, CAD 1,000.00 of sales and its CAD 50.00 of GST, has debits of CAD 1,050.00, above the"
  say "service posting limit: a billing system cannot answer a refusal, so the ERP parks the entry for a person"
  say "instead of posting it (ADR-0049 §7, product owner 2026-10-22, T61n), saying why:"
  api 200 '"parked entry, \(.status), by a \(.created_by_kind) principal: \(.limit_exceeded.detail)"' postJournalEntry \
    -body "$(sale "Billing: invoice 1042" "$(line "$cash" 1050.00), $(line "$sales" -1000.00 GO5), $(line "$gst" -50.00 GO5)")"
  expect "the billing entry's status" "$(field .status)" parked
  expect "the limit it exceeded" "$(field '"\(.limit_exceeded.kind) \(.limit_exceeded.limit.amount) \(.limit_exceeded.limit.currency)"')" "service_posting_limit 1000.00 CAD"
  expect "its debits" "$(field '"\(.limit_exceeded.debits.amount) \(.limit_exceeded.debits.currency)"')" "1050.00 CAD"
  expect "its journal entry" "$(field '.journal_entry_id // "none"')" none
  local parked
  parked=$(field .id)
  say "Carla finds the entries integrations parked and posts this one; at CAD 1,050.00 it is within her company's"
  say "manual entry approval limit, so it posts at once, in her name:"
  as carla
  api 200 '.items[] | "parked by \(.created_by_kind): \(.header_text), \(.limit_exceeded.debits.amount) \(.limit_exceeded.debits.currency) (limit \(.limit_exceeded.limit.amount) \(.limit_exceeded.limit.currency))"' \
    listParkedJournalEntries -p "legal_entity_id=$LE" -p status=parked -p created_by_kind=service
  expect "the billing system's parked entries" "$(field '[.items[].id] | join(" ")')" "$parked"
  QUIET=1 api 200 '' getParkedJournalEntry -p "id=$parked"
  api 200 '"parked entry \(.status) as journal entry \(.journal_entry_id)"' postParkedJournalEntry -p "id=$parked" -if-match "$ETAG"
  expect "the billing entry after Carla posted it" "$(field .status)" posted
  api 200 "$entry" getJournalEntry -p "id=$(field .journal_entry_id)"
  say "The tax this posts is filed under the company's GST/HST registration with the CRA, configured in step 2"
  say "(T61g): the number is sealed and shown by its last four characters. The number and the quarterly"
  say "frequency here are illustrative; the pilot's own registration and filing frequency come from the pilot."
  api 200 '.items[] | "registration \(.code): \(.authority), number ending \(.number_last4), return \(.return_type), \(.frequency) from \(.first_period_start), company \(.company_code), \(.status)"' \
    listTaxRegistrations -p "legal_entity_id=$LE"
  expect "the registration" "$(field '.items[0] | "\(.code) \(.company_code) \(.number_last4) \(.frequency)"')" "GSTHST $CC 0001 quarterly"
  local registration
  registration=$(field '.items[0].id')
  say "Its filing periods follow from the frequency and the first period, apart from fiscal periods, up to the"
  say "one under way on the company's date in its time zone; the returns (T61h-T61i) are prepared for them. The first"
  say "period is the cutover: the CRA has the returns of the periods before it, filed before the ERP."
  api 200 '"periods starting by \(.start_date_to) (the company'"'"'s date in \(.time_zone)):", (.items[] | "  \(.start_date) to \(.end_date)")' \
    listTaxPeriods -p "id=$registration"
  expect "the first period, and the periods to date" "$(field '"\(.items[0].start_date) \(.items | length)"')" "$(tax_first) 3"
  expect "the time zone" "$(field .time_zone)" America/Toronto
  local start end before running
  start=$(field '.items[-2].start_date')
  end=$(field '.items[-2].end_date')
  before=$(field '.items[-3].end_date')
  running=$(field '.items[-1].start_date')
  say "A registration nobody may see, or none, is not found:"
  refused 404 request.not_found listTaxPeriods -p "id=00000000-0000-7000-8000-000000000000"
  tax_return "$cash" "$sales" "$gst" "$registration" "$start" "$end" "$before" "$running"
  tax_agent "$gst" "$registration" "$start" "$end"
  file_return "$registration" "$start" "$end"
  passed
}

# tax_return CASH SALES GST REGISTRATION START END BEFORE RUNNING is step
# 11's draft return (T61h) of the latest ended period, START to END: two
# late sales, one dated END and one BEFORE, the first period's end; an
# adjustment and an instalment (gst_adjustments); the GST34's lines
# computed twice; line 103 drilled down to its items;
# SAP's down payment left out; the period under way, RUNNING, refused;
# the return of START refused until the first period's is filed, which
# reports the late sale of BEFORE.
tax_return() {
  local cash=$1 sales=$2 gst=$3 registration=$4 start=$5 end=$6 before=$7 running=$8
  say "Two of the billing system's invoices arrive late, and Carla posts them today with their own tax date (P6):"
  say "1039 of $end, the last day of the latest ended period, and 1031 of $before, in the period before it."
  api 201 "$entry" postJournalEntry -body "$(sale "Billing: invoice 1039, sent late" "$(line "$cash" 1050.00), $(line "$sales" -1000.00 GO5), $(line "$gst" -50.00 GO5)" "$end")"
  api 201 "$entry" postJournalEntry -body "$(sale "Billing: invoice 1031, sent late" "$(line "$cash" 210.00), $(line "$sales" -200.00 GO5), $(line "$gst" -10.00 GO5)" "$before")"
  gst_adjustments "$end"
  say "Carla computes the draft return of $start to $end (T61h): the GST34's lines, as Canada $CA_PACK defines them"
  say "(T61j), sum the tax items no return has reported, dated by the period's end; those of earlier periods are"
  say "shown apart (P7). Nothing is stored or claimed: computed again, the draft is the same."
  api 200 '"draft \(.return_type) of \(.registration_code), \(.period_start) to \(.period_end), \(.currency), pack \(.pack.country) \(.pack.version): \(.item_count) items, \(.prior_period.item_count) of earlier periods", (.boxes[] | "  \(.code) \(.amount.amount)\(if .prior_period_amount.amount != "0.00" then ", of which earlier periods \(.prior_period_amount.amount)" else "" end): \(.label)")' \
    getTaxReturnDraft -p "id=$registration" -p "period_start=$start"
  expect "line 103, GST/HST collected, and its earlier period's part" "$(field '.boxes[] | select(.code == "103") | "\(.amount.amount) \(.prior_period_amount.amount)"')" "60.00 10.00"
  expect "the lines in the pack's order" "$(field '[.boxes[].code] | join(" ")')" "101 103 104 105 106 107 108 109 110 111 112 113a 205 405 113b 113c 114 115"
  expect "line 112's label, its sum signed off" "$(field '.boxes[] | select(.code == "112") | .label')" "Line 110 plus line 111 (a paper return's subtotal)"
  expect "lines 107 and 110, the adjustment parked and the instalment without its code" "$(field '[.boxes[] | select(.code == "107" or .code == "110") | .amount.amount] | join(" ")')" "0.00 0.00"
  local draft
  draft=$(jq -cS . "$WORK/last.json")
  QUIET=1 api 200 '' getTaxReturnDraft -p "id=$registration" -p "period_start=$start"
  expect "the draft computed again" "$(jq -cS . "$WORK/last.json")" "$draft"
  say "Line 103 drills down to the items it sums and the entries that wrote them:"
  api 200 '.items[] | "  \(.tax_date) \(.tax_code)/\(.component): tax \(.tax.amount), adds \(.box_amount.amount)\(if .prior_period then " (an earlier period'"'"'s)" else "" end), entry \(.document_number) of \(.posting_date): \(.header_text // "")"' \
    listTaxReturnDraftItems -p "id=$registration" -p "period_start=$start" -p box=103
  expect "line 103's items" "$(field '[.items[].box_amount.amount] | join(" ")')" "10.00 50.00"
  expect "the items' invoices" "$(field '[.items[].header_text] | join(" / ")')" "Billing: invoice 1031, sent late / Billing: invoice 1039, sent late"
  QUIET=1 api 200 '' listTaxReturnDraftItems -p "id=$registration" -p "period_start=$start" -p box=106 -p page_size=500
  expect "the input tax of SAP's down payment (step 3) in the draft" "$(jq -r --arg e "$DP_ENTRY" '[.items[] | select(.journal_entry_id == $e)] | length' "$WORK/last.json")" 0
  say "SAP's open down payment of step 3 carries input tax dated SAP's posting date, which SAP's own return reported:"
  say "the draft leaves the items of migrated entries out (ADR-0037); its clearing's reversal, posted here, is reported."
  say "The period under way has not ended, so its return cannot be drafted yet:"
  refused 409 tax.period_not_ended getTaxReturnDraft -p "id=$registration" -p "period_start=$running"
  say "Returns are prepared in period order, so each period's items are reported in its own return: the return of"
  say "$start to $end waits until the first period's is filed, and invoice 1031 is that period's to report."
  refused 409 tax.earlier_period_unfiled prepareTaxReturn -p "id=$registration" -body "{\"period_start\": \"$start\"}"
  file_earlier "$registration" "$before"
}

# gst_adjustments END has Carla enter, dated END, what is
# no document's tax (ADR-0049 §11, T61j): an adjustment decreasing the
# net tax (code GA, line 107) and an instalment Maple paid (code GI, line
# 110), each a manual entry of a code of tax alone. She parks the
# adjustment (GST_PARKED) and posts the instalment (GST_INSTALMENT, its
# document number), both without their codes: the tax agent proposes
# them (tax_agent, T61l2) and she posts what it proposed.
gst_adjustments() {
  local end=$1 itc payable expense cash
  itc=$(account 1300)
  payable=$(account 2300)
  expense=$(account 6000)
  cash=$(account 100000)
  say "The return also takes what is no document's tax: an adjustment decreasing the net tax (line 107) and the"
  say "instalment Maple paid towards it (line 110), each a manual entry of a code of tax alone, kind adjustment, whose"
  say "tax is what the entry posts (ADR-0049 §11). Carla enters both, dated $end: she parks the adjustment, to post it"
  say "once its figures are checked, and posts the instalment; she leaves out their codes, GA and GI, so no line of"
  say "the return reports them yet:"
  api 201 '"parked entry \(.status): \([.lines[] | "\(.amount.amount)\(if .tax_code then " \(.tax_code)" else "" end)"] | join(" / "))"' parkJournalEntry \
    -body "$(sale "GST/HST adjustment decreasing the net tax" "$(line "$itc" 5.00), $(line "$expense" -5.00)" "$end")"
  GST_PARKED=$(field .id)
  api 201 "$entry" postJournalEntry -body "$(sale "GST/HST instalment paid" "$(line "$payable" 100.00), $(line "$cash" -100.00)" "$end")"
  GST_INSTALMENT=$(field .document_number)
}

# tax_agent GST REGISTRATION START END is T61l1's and T61l2's part of step
# 11 (ADR-0049 §13): before Carla prepares the return of START to END, the
# tax agent, erp agent tax-draft through the model stand-in answering
# from a script of turns (its suite's replay, evals/tax), computes the
# draft, reads the first period's filed return, line 103's items, the
# GST/HST account's balances and the parked entries, and proposes for a
# person: an explanation of the draft (line 103 against the first
# period's, citing invoice 1039's entry; lines 107 and 110 empty), GA on
# Carla's parked adjustment (a code correction, a recommendation on the
# entry), and the instalment's GI as an adjustment entry it parks. It
# posts and prepares nothing: those are a person's, and the API refuses
# the agent's own calls. Carla accepts the proposals, puts GA on her
# entry and posts it and the agent's, and the draft's lines 107 and 110
# show them.
tax_agent() {
  local gst=$1 registration=$2 start=$3 end=$4 role agent got session first now prev doc posted adjustment
  as ada
  say "Ada sets up the tax agent (ADR-0049 §13): an agent principal of type tax at autonomy level 1 on erp-default, with"
  say "a monthly budget of USD 2.00; its role reads registrations, draft returns and returns, the ledger's entries,"
  say "accounts and balances and the parked entries and invoices, parks entries, and records its sessions and"
  say "recommendations. It holds no permission that posts an entry or moves a return."
  api 201 '"role \(.code), \(.name)"' createRole -body '{"code": "tax_agent", "name": "Tax agent"}'
  role=$(field .id)
  grant "$role" tax.registration.read tax.return.read ledger.entry.read ledger.account.read ledger.entry.park payables.invoice.read \
    agents.session.read agents.session.record agents.model.invoke
  api 201 '"\(.kind) \(.display_name) of type \(.agent_profile.agent_type), level \(.agent_profile.autonomy_level) on \(.agent_profile.model), budget \(.agent_profile.monthly_budget.amount) \(.agent_profile.monthly_budget.currency)"' \
    createPrincipal -body '{"kind": "agent", "external_subject": "tax-agent", "display_name": "Tax agent", "all_companies": true, "agent_profile": {"autonomy_level": 1, "agent_type": "tax", "model": "erp-default", "description": "Reviews draft tax returns and proposes adjustments and code corrections for people to decide (ADR-0049)", "monthly_budget": {"amount": "2.00", "currency": "USD"}}}'
  agent=$(field .id)
  QUIET=1 api 201 '' createRoleAssignment -body "{\"principal_id\": \"$agent\", \"role_id\": \"$role\"}"
  token tax-agent "Tax agent"
  printf '%s\tTax agent\n' "$agent" >>"$WORK/names"
  as carla
  QUIET=1 api 200 '' listTaxReturns -p "id=$registration" -p status=filed
  first=$(field '.items[0].id')
  prev=$(field '.items[0].boxes[] | select(.code == "103") | .amount.amount')
  QUIET=1 api 200 '' getTaxReturnDraft -p "id=$registration" -p "period_start=$start"
  now=$(field '.boxes[] | select(.code == "103") | .amount.amount')
  QUIET=1 api 200 '' listTaxReturnDraftItems -p "id=$registration" -p "period_start=$start" -p box=103
  doc=$(field '.items[] | select(.header_text == "Billing: invoice 1039, sent late") | .document_number')
  posted=$(field '.items[] | select(.header_text == "Billing: invoice 1039, sent late") | .posting_date')
  [ -n "$doc" ] || fail "invoice 1039 is not among line 103's items"
  say "The agent runs: erp agent tax-draft computes the draft of $start to $end and starts a session. The model is the"
  say "stand-in, answering from a script of turns as the agent's evaluation suite replays them (evals/tax): it reads the"
  say "draft, the first period's filed return, line 103's items, the GST/HST account's balances and the parked entries,"
  say "and proposes three things for a person: an explanation (line 103 is $now against $prev, the difference invoice"
  say "1039's entry $doc, posted on $posted with its tax date $end; lines 107 and 110 are empty); GA on Carla's parked"
  say "adjustment, a correction it recommends on the entry, which it cannot change; and, for the instalment's entry"
  say "$GST_INSTALMENT, an adjustment entry it parks: GI on 2300 and its reversal without a code, which leaves the"
  say "account's balance as it is and reports the instalment on line 110."
  jq -n --arg reg "$registration" --arg start "$start" --arg end "$end" --arg first "$first" --arg le "$LE" --arg gst "$gst" \
    --argjson year "${start%%-*}" --arg now "$now" --arg prev "$prev" --arg doc "$doc" --arg posted "$posted" --arg today "$TODAY" \
    --arg parked "$GST_PARKED" --arg inst "$GST_INSTALMENT" '{
    vars: {session: "session id is ([0-9a-f-]{36})"},
    turns: [
      {tool: "getTaxReturnDraft", input: {id: $reg, period_start: $start}},
      {tool: "listTaxReturns", input: {id: $reg, status: "filed"}},
      {tool: "getTaxReturn", input: {id: $first}},
      {tool: "listTaxReturnDraftItems", input: {id: $reg, period_start: $start, box: "103"}},
      {tool: "listBalances", input: {legal_entity_id: $le, fiscal_year: $year, account_id: $gst}},
      {tool: "listParkedJournalEntries", input: {legal_entity_id: $le, status: "parked"}},
      {tool: "getParkedJournalEntry", input: {id: $parked}},
      {tool: "createRecommendation", input: {body: {session_id: "{{session}}", object_type: "tax_registration", object_id: $reg,
        kind: "explain_draft_return", content: "GST34 of \($start) to \($end): line 103 \($now) against \($prev) in the first period'"'"'s filed return; the difference is invoice 1039 (entry \($doc)), posted on \($posted) with its tax date \($end), so 2300 GST/HST payable shows its tax in the fiscal period it was posted in, not in this tax period'"'"'s. Lines 107 and 110 are 0.00: the instalment paid (entry \($inst)) carries no tax code, so an adjustment entry with GI is parked for a person to post, and the adjustment decreasing the net tax waits parked without its code, GA, which is proposed on it. A person should confirm the late invoice, and post both entries, before preparing the return."}}},
      {tool: "createRecommendation", input: {body: {session_id: "{{session}}", object_type: "parked_journal_entry", object_id: $parked,
        kind: "correct_tax_code", content: "The adjustment decreasing the net tax, dated \($end), debits 1300 GST/HST input tax credits 5.00 without a tax code, so no line of the return would report it once posted: GA on line 1 (1300, 5.00), the code of an adjustment decreasing the net tax (line 107)."}}},
      {tool: "parkJournalEntry", input: {body: {legal_entity_id: $le, document_date: $end, posting_date: $today, tax_date: $end, currency: "CAD",
        header_text: "Tax agent: the instalment of entry \($inst) on line 110 with GI",
        lines: [{account_id: $gst, amount: {amount: "100.00", currency: "CAD"}, tax_code: "GI"}, {account_id: $gst, amount: {amount: "-100.00", currency: "CAD"}}]}}},
      {final: "Explained the draft, proposed GA on the parked adjustment and parked the instalment'"'"'s GI; a person posts them and prepares the return."}]}' >"$WORK/model-script.json"
  show ERP_AGENT_TOKEN=… erp agent tax-draft -registration GSTHST -period "$start"
  got=$(ERP_AGENT_TOKEN=$(cat "$WORK/tokens/tax-agent") "$ERP" agent tax-draft -registration GSTHST -period "$start" 2>"$WORK/stderr") ||
    { rm -f "$WORK/model-script.json"; fail "erp agent tax-draft failed ($(cat "$WORK/stderr"))"; }
  rm -f "$WORK/model-script.json"
  printf '%s\n' "$got" | sed 's/^/     /'
  session=$(printf '%s\n' "$got" | sed -nE '1s/^Session ([0-9a-f-]{36}): .*/\1/p')
  expect "what the agent proposed" "$(printf '%s\n' "$got" | sed -nE '1s/^Session [0-9a-f-]{36}: //p')" "recommendations made 2, entries parked 1, refused 0."
  adjustment=$(printf '%s\n' "$got" | sed -nE 's/^  parked entry ([0-9a-f-]{36})$/\1/p')
  as tax-agent
  say "Proposing is all it may do: posting an entry and preparing, approving, filing and withdrawing a return are a"
  say "person's (x-agent: false), whatever its level, and erp agent builds it no tool for them; the API refuses its own"
  say "calls:"
  refused 403 identity.agent_not_allowed prepareTaxReturn -p "id=$registration" -body "{\"period_start\": \"$start\"}"
  refused 403 identity.agent_not_allowed postParkedJournalEntry -p "id=$adjustment" -if-match '"1"' -body '{}'
  say "Its session, which the command named, records each tool call:"
  api 200 '"  session \(.id[0:8])… \(.status) on \(.model): \(.purpose)"' getAgentSession -p "id=$session"
  expect "the session" "$(field .status)" completed
  api 200 '"  its tool calls: \([.items[].tool] | join(", "))"' listAgentToolCalls -p "id=$session"
  expect "its tool calls" "$(field '[.items[].tool] | join(" ")')" \
    "getTaxReturnDraft listTaxReturns getTaxReturn listTaxReturnDraftItems listBalances listParkedJournalEntries getParkedJournalEntry createRecommendation createRecommendation parkJournalEntry"
  tax_agent_decided "$registration" "$start" "$session" "$doc" "$adjustment" "$got"
}

# tax_agent_decided REGISTRATION START SESSION DOC ADJUSTMENT OUTPUT is
# Carla deciding what the tax agent proposed in SESSION (OUTPUT, what erp
# agent tax-draft printed): she accepts its explanation of the draft,
# citing DOC, and its code correction of her parked adjustment, puts GA
# on the entry and posts it, and posts the adjustment entry it parked,
# ADJUSTMENT, taking it on; the draft's lines 107 and 110 then report them.
tax_agent_decided() {
  local registration=$1 start=$2 session=$3 doc=$4 adjustment=$5 got=$6 rec fix
  as carla
  say "Carla reads the agent's explanation on the registration; nothing was prepared:"
  api 200 '(.items[] | "  \(.kind) by \(.agent_id[0:8])…, \(.outcome // "open"): \(.content)")' listRecommendations -p "object_id=$registration"
  expect "the explanations of the registration's draft" "$(field '[.items[] | select(.kind == "explain_draft_return")] | length')" 1
  rec=$(field '.items[0].id')
  expect "the explanation, one the agent printed" "$(printf '%s\n' "$got" | grep -c "^  recommendation $rec\$")" 1
  expect "made in its session" "$(field '.items[0].session_id')" "$session"
  expect "citing invoice 1039's entry" "$(jq -r --arg d "$doc" '.items[0].content | contains("entry \($d)")' "$WORK/last.json")" true
  QUIET=1 api 200 '' listTaxReturns -p "id=$registration" -p "period_start=$start"
  expect "returns of $start after the agent ran" "$(field '.items | length')" 0
  say "She checked invoice 1039 and accepts the explanation; she prepares the return herself:"
  QUIET=1 api 200 '' getRecommendation -p "id=$rec"
  api 200 '"  recommendation \(.outcome) by a person"' decideRecommendation -p "id=$rec" -if-match "$ETAG" \
    -body '{"outcome": "accepted", "comment": "Invoice 1039 checked: it belongs to the period."}'
  expect "the explanation's outcome" "$(field .outcome)" accepted
  say "The agent's correction is on her parked adjustment, which it did not change:"
  api 200 '(.items[] | "  \(.kind) on the \(.object_type), \(.outcome // "open"): \(.content)")' listRecommendations -p "object_id=$GST_PARKED"
  expect "the corrections of her parked adjustment" "$(field '[.items[] | select(.kind == "correct_tax_code")] | length')" 1
  fix=$(field '.items[0].id')
  QUIET=1 api 200 '' getParkedJournalEntry -p "id=$GST_PARKED"
  expect "her adjustment as she parked it" "$(field '"\(.version) \([.lines[] | .tax_code // "none"] | join(" "))"')" "1 none none"
  say "She accepts it, puts GA on the entry's line on 1300 and posts it (within her approval limit):"
  QUIET=1 api 200 '' getRecommendation -p "id=$fix"
  api 200 '"  recommendation \(.outcome) by a person"' decideRecommendation -p "id=$fix" -if-match "$ETAG" \
    -body '{"outcome": "accepted", "comment": "GA added to the adjustment."}'
  QUIET=1 api 200 '' getParkedJournalEntry -p "id=$GST_PARKED"
  api 200 '"parked entry \(.status): \([.lines[] | "\(.amount.amount)\(if .tax_code then " \(.tax_code)" else "" end)"] | join(" / "))"' \
    updateParkedJournalEntry -p "id=$GST_PARKED" -if-match "$ETAG" -body "{\"lines\": [$(line "$(account 1300)" 5.00 GA), $(line "$(account 6000)" -5.00)]}"
  api 200 '"parked entry \(.status) as journal entry \(.journal_entry_id)"' postParkedJournalEntry -p "id=$GST_PARKED" -if-match "$ETAG" -body '{}'
  expect "her adjustment" "$(field .status)" posted
  say "The adjustment entry the agent parked waits for a person; Carla checks it and posts it, taking it on:"
  api 200 '.items[] | "  parked by \(.created_by_kind): \(.header_text), tax date \(.tax_date)"' listParkedJournalEntries -p "legal_entity_id=$LE" \
    -p status=parked -p created_by_kind=agent
  expect "the entries the agent parked" "$(field '[.items[].id] | join(" ")')" "$adjustment"
  QUIET=1 api 200 '' getParkedJournalEntry -p "id=$adjustment"
  api 200 '"parked entry \(.status) as journal entry \(.journal_entry_id)"' postParkedJournalEntry -p "id=$adjustment" -if-match "$ETAG" -body '{}'
  expect "the agent's adjustment" "$(field .status)" posted
  api 200 "$entry" getJournalEntry -p "id=$(field .journal_entry_id)"
  QUIET=1 api 200 '' getTaxReturnDraft -p "id=$registration" -p "period_start=$start"
  expect "lines 107 and 110, the adjustment and the instalment" "$(field '[.boxes[] | select(.code == "107" or .code == "110") | .amount.amount] | join(" ")')" "5.00 100.00"
}

# close_tax_periods START END BODY REGISTRATION [LE] closes the leading
# ledger's open fiscal periods of the tax period START to END of legal
# entity LE ($LE when not given), which preparing the return BODY needs
# (T61i), once preparing has been refused for them; LEADING is the
# leading ledger.
close_tax_periods() {
  local start=$1 end=$2 body=$3 registration=$4 le=${5:-$LE} open p
  QUIET=1 api 200 '' listLedgers
  LEADING=$(field '.items[] | select(.is_leading) | .id')
  QUIET=1 api 200 '' listFiscalPeriods -p "legal_entity_id=$le" -p "fiscal_year=${start%%-*}" -p "ledger_id=$LEADING" -p page_size=100
  open=$(jq -r --arg s "$start" --arg e "$end" '[.items[] | select((.is_special | not) and .status == "open" and .start_date <= $e and .end_date >= $s)
    | "\(.id)/\(.version)"] | join(" ")' "$WORK/last.json")
  if [ -n "$open" ]; then
    refused 409 tax.fiscal_period_open prepareTaxReturn -p "id=$registration" -body "$body"
    for p in $open; do
      QUIET=1 api 200 '' closeFiscalPeriod -p "id=${p%/*}" -if-match "\"${p#*/}\""
    done
    show erp call closeFiscalPeriod -p "id=..." -if-match '"2"' "# $(wc -w <<<"$open" | tr -d ' ') periods of the leading ledger"
  fi
}

# file_earlier REGISTRATION END is step 11's return of the registration's
# first period, ending END, prepared, approved by Fiona and filed before
# the latest ended period's: it reports invoice 1031.
file_earlier() {
  local registration=$1 end=$2 start ret
  QUIET=1 api 200 '' listTaxPeriods -p "id=$registration"
  start=$(field '.items[0].start_date')
  local body="{\"period_start\": \"$start\"}"
  say "Carla prepares the first period's return, $start to $end, once its fiscal periods are closed; Fiona approves it"
  say "and Carla records it filed:"
  close_tax_periods "$start" "$end" "$body" "$registration"
  api 201 '"return \(.return_type) of \(.registration_code), \(.period_start) to \(.period_end): \(.status), \(.item_count) items", (.boxes[] | "  \(.code) \(.amount.amount)")' \
    prepareTaxReturn -p "id=$registration" -body "$body"
  expect "the first period's line 103, invoice 1031's" "$(field '.boxes[] | select(.code == "103") | .amount.amount')" "10.00"
  ret=$(field .id)
  approve fiona "$(field .approval_request_id)" "Boxes agree with the tax accounts"
  as carla
  QUIET=1 api 200 '' getTaxReturn -p "id=$ret"
  api 200 '"return \(.status): confirmation \(.filing.confirmation_number) of \(.filing.filed_on)"' fileTaxReturn -p "id=$ret" -if-match "$ETAG" \
    -body "{\"confirmation_number\": \"WALKTHROUGH-0000\", \"filed_on\": \"$TODAY\"}"
  expect "the first period's return" "$(field .status)" filed
}

# file_return REGISTRATION START END is step 11's return of START to END
# (T61i): prepared once its fiscal periods are closed, claiming the
# draft's items; approved by Fiona, the tax manager, not Carla who
# prepared it; recorded as filed, which locks the periods; the period's
# draft then empty, and a manual entry's tax date in it refused (P6).
file_return() {
  local registration=$1 start=$2 end=$3 ret request
  local body="{\"period_start\": \"$start\"}"
  say "Carla prepares the return of $start to $end (T61i): a snapshot of the draft that claims its items, so no later"
  say "draft or return reports them again; invoice 1031 is the first period's now. The leading ledger's fiscal periods"
  say "of the tax period must be closed first, so no posting is under way in it:"
  close_tax_periods "$start" "$end" "$body" "$registration"
  api 201 '"return \(.return_type) of \(.registration_code), \(.period_start) to \(.period_end): \(.status), \(.item_count) items, hash \(.items_hash[0:12])...", (.boxes[] | "  \(.code) \(.amount.amount)")' \
    prepareTaxReturn -p "id=$registration" -body "$body"
  expect "the return's line 103, invoice 1039's" "$(field '.boxes[] | select(.code == "103") | .amount.amount')" "50.00"
  ret=$(field .id)
  request=$(field .approval_request_id)
  say "A second return of the period cannot be prepared while this one stands:"
  refused 409 tax.return_in_progress prepareTaxReturn -p "id=$registration" -body "$body"
  say "It waits for a tax manager's approval. Carla prepared it, so she cannot approve it (four eyes):"
  QUIET=1 api 200 '' getApprovalRequest -p "id=$request"
  refused 403 workflow.own_request approveApprovalRequest -p "id=$request" -if-match "$ETAG" -body '{"comment": "Mine"}'
  approve fiona "$request" "Boxes agree with the tax accounts"
  as carla
  say "Carla files it on the CRA's portal herself (P3: the ERP files nothing) and records the confirmation the portal"
  say "gives; the number here is illustrative. Recording it filed locks the fiscal periods of the tax period (T32)."
  QUIET=1 api 200 '' getTaxReturn -p "id=$ret"
  expect "approved, by another person" "$(field '"\(.status) \(.approved_by != .created_by)"')" "approved true"
  api 200 '"return \(.status): confirmation \(.filing.confirmation_number) of \(.filing.filed_on)"' fileTaxReturn -p "id=$ret" -if-match "$ETAG" \
    -body "{\"confirmation_number\": \"WALKTHROUGH-0001\", \"filed_on\": \"$TODAY\"}"
  QUIET=1 api 200 '' listFiscalPeriods -p "legal_entity_id=$LE" -p "fiscal_year=${start%%-*}" -p "ledger_id=$LEADING" -p page_size=100
  expect "the tax period's fiscal periods" "$(jq -r --arg s "$start" --arg e "$end" '[.items[] | select((.is_special | not) and .start_date <= $e
    and .end_date >= $s) | .status] | unique | join(" ")' "$WORK/last.json")" locked
  say "The items it reported are in no later draft: the period's draft is empty now, and the next return starts afresh."
  api 200 '"draft of \(.period_start) to \(.period_end): \(.item_count) items, line 103 \((.boxes[] | select(.code == "103")).amount.amount)"' \
    getTaxReturnDraft -p "id=$registration" -p "period_start=$start"
  expect "the period's draft after filing" "$(field .item_count)" 0
  say "A tax date is chosen only in a period no return reports yet (P6): invoice 1038, of $end too, arrives after the"
  say "filing, and Carla may not date its tax in the filed period; she would date it in the period under way, whose"
  say "return reports it."
  refused 409 ledger.tax_date_in_return postJournalEntry \
    -body "$(sale "Billing: invoice 1038, sent after the filing" "$(line "$cash" 105.00), $(line "$sales" -100.00 GO5), $(line "$gst" -5.00 GO5)" "$end")"
  expect "the refusal names the filed return" "$(jq -r --arg r "$ret" '.detail | contains("whose original return \($r) is filed")' "$WORK/last.json")" true
}

# center_approved REQUESTER has the controller approve the center change
# the last answer opened (a hierarchy change names its approval request,
# a new center its pending change), then makes REQUESTER the caller again.
center_approved() {
  local request
  request=$(field '.workflow_request_id // empty')
  if [ -z "$request" ]; then
    QUIET=1 api 200 '' getCenterChange -p "id=$(field .pending_change_id)"
    request=$(field .workflow_request_id)
  fi
  approve carla "$request" "Structure agreed"
  as "$1"
}

# add_node HIERARCHY PARENT CODE NAME adds a node below PARENT, approved by the
# controller, and sets NODE to its id.
add_node() {
  QUIET=1 api 200 '' getHierarchy -p "id=$1"
  api 200 '"center change \(.kind) \(.payload.code): \(.status)"' addHierarchyNode -p "id=$1" -p "node_id=$2" -if-match "$ETAG" \
    -body "{\"code\": \"$3\", \"name\": \"$4\", \"reason\": \"The pilot's reporting structure\"}"
  NODE=$(field .payload.node_id)
  center_approved sam
}

step_centers() {
  heading "Step 12 — Profit and cost centers in standard hierarchies"
  as sam
  say "Sam proposes the structure; each center and each change to a hierarchy waits for the controller."
  api 201 '"segment \(.code) \(.name)"' createSegment -body '{"code": "CA", "name": "Canada"}'
  local segment pch cch pc cc
  segment=$(field .id)
  say "The migration (step 3) loaded SAP's groups below the standard hierarchies' root, its centers at their groups"
  say "(T56h2); Sam builds the pilot's structure beside them."
  api 200 '.items[] | "hierarchy \(.kind) \(.code), standard: \(.is_standard)"' listHierarchies -p kind=profit_center -p is_standard=true
  pch=$(field '.items[0].id')
  QUIET=1 api 200 '' getHierarchy -p "id=$pch"
  add_node "$pch" "$(field .root_node_id)" EAST "Eastern Canada"
  api 201 '"profit center \(.code) \(.name): \(.status)"' createProfitCenter \
    -body "{\"code\": \"PC-MTL\", \"name\": \"Montreal office\", \"responsible\": \"Carla Controller\", \"segment_id\": \"$segment\", \"legal_entity_ids\": [\"$LE\"], \"valid_from\": \"$YEAR-01-01\", \"standard_node_id\": \"$NODE\", \"reason\": \"The pilot's office\"}"
  pc=$(field .id)
  center_approved sam
  api 200 '"profit center \(.code): \(.status) from \(.periods[0].valid_from)"' getProfitCenter -p "id=$pc"
  expect "profit center status" "$(field .status)" active
  api 200 '.items[] | "hierarchy \(.kind) \(.code), standard: \(.is_standard)"' listHierarchies -p kind=cost_center -p is_standard=true
  cch=$(field '.items[0].id')
  QUIET=1 api 200 '' getHierarchy -p "id=$cch"
  add_node "$cch" "$(field .root_node_id)" ADMIN Administration
  api 201 '"cost center \(.code) \(.name): \(.status)"' createCostCenter \
    -body "{\"legal_entity_id\": \"$LE\", \"code\": \"CC-1100\", \"name\": \"Finance office\", \"responsible\": \"Carla Controller\", \"category\": \"ADMIN\", \"profit_center_id\": \"$pc\", \"valid_from\": \"$YEAR-01-01\", \"standard_node_id\": \"$NODE\", \"reason\": \"The pilot's finance office\"}"
  cc=$(field .id)
  center_approved sam
  api 200 '"cost center \(.code): \(.status), category \(.periods[0].category), profit center PC-MTL"' getCostCenter -p "id=$cc"
  expect "cost center status" "$(field .status)" active
  expect "the cost center's profit center" "$(field '.periods[0].profit_center_id')" "$pc"
  api 200 '.items[] | "\(.hierarchy_code): \([.nodes[].code] | join(" > ")) > CC-1100"' listCostCenterPaths -p "id=$cc"
  api 200 '.items[] | "\(.hierarchy_code): \([.nodes[].code] | join(" > ")) > PC-MTL"' listProfitCenterPaths -p "id=$pc"
  say "Postings with cost centers and the reports on them are T56d-T56f, still to come."
  passed
}

step_cash() {
  heading "Step 13 — The cash position, with an expected receipt"
  as tom
  local due
  due=$(date_add "$TODAY" 3)
  say "Tom expects a customer's CAD 25,000.00 at RBC on $due and records it as a memo record (T53d)."
  api 201 '"memo record \(.reference): \(.amount.amount) \(.amount.currency) on \(.value_date) at \(.house_bank_code), \(.liquidity_item), \(.certainty), \(.status)"' createMemoRecord \
    -body "{\"legal_entity_id\": \"$LE\", \"house_bank_id\": \"$RBC\", \"amount\": {\"amount\": \"25000.00\", \"currency\": \"CAD\"}, \"value_date\": \"$due\", \"liquidity_item\": \"REC_CUSTOMERS\", \"description\": \"Northwind Traders, invoice 1042\", \"reference\": \"WT-1042\"}"
  expect "the house bank the memo record names" "$(field .house_bank_code)" RBC
  say "The position per bank and day from tomorrow (T53c): today's reconciled statement of step 8 opens it,"
  say "and the memo record flows on its value date (the next working day, if that is not one):"
  api 200 '.banks[] | select(.house_bank_code == "RBC") | "bank \(.house_bank_code) \(.currency): statement balance \(.opening.balance.amount) of \(.opening.statement_date // "none")",
    (.days[] | "  \(.date): \(.opening.amount) -> \(.closing.amount)\([.flows[]? | ", \(.source) \(.certainty) \(.amount.amount)"] | join(""))")' \
    getCashPosition -p "legal_entity_id=$LE" -p "from=$(date_add "$TODAY" 1)" -p days=7
  expect "the statement balance the position opens with" "$(field '.banks[] | select(.house_bank_code == "RBC") | .opening.balance.amount')" "$CLOSING"
  expect "the memo record's flow" "$(field '[.banks[] | select(.house_bank_code == "RBC") | .days[].flows[]? | select(.source == "memo") | .amount.amount] | join(" ")')" 25000.00
  say "Today for the position is Maple's date in Toronto, not UTC's: in a Toronto evening UTC is already tomorrow (T72n1)."
  local before
  before=$(company_day)
  api 200 '"read on \(.read_date) in \(.time_zone), from \(.from)"' getCashPosition -p "legal_entity_id=$LE" -p days=1
  expect "the zone the position is read in" "$(field .time_zone)" America/Toronto
  expect_today "the read date, Toronto's" "$(field .read_date)" "$before"
  expect "the first day, the read date" "$(field .from)" "$(field .read_date)"
  say "A client dates its forms on the same day: Maple's date today, in its zone (getLegalEntityToday), which the web"
  say "workspace pre-fills a manual entry's dates, a payment run's and the open items' key date with, not UTC's day:"
  before=$(company_day)
  api 200 '"today is \(.date) in \(.time_zone)"' getLegalEntityToday -p "id=$LE"
  expect "the zone Maple's today is in" "$(field .time_zone)" America/Toronto
  expect_today "Maple's today, Toronto's" "$(field .date)" "$before"
  local toronto judged status listed counted
  toronto=$(company_day)
  say "A receipt promised for today ($toronto in Toronto), its last day today, is active until Toronto's day ends:"
  say "a memo record's status is judged on Maple's date in its zone, as the position is, not on UTC's (T72n2)."
  api 201 '"memo record \(.reference): \(.status) on \(.read_date) in \(.time_zone), last day \(.expires_on)"' createMemoRecord \
    -body "{\"legal_entity_id\": \"$LE\", \"house_bank_id\": \"$RBC\", \"amount\": {\"amount\": \"1000.00\", \"currency\": \"CAD\"}, \"value_date\": \"$toronto\", \"expires_on\": \"$toronto\", \"liquidity_item\": \"REC_CUSTOMERS\", \"reference\": \"WT-1043\"}"
  expect "the zone the memo record's status is judged in" "$(field .time_zone)" America/Toronto
  judged=$(field .read_date)
  expect_today "the memo record's read date, Toronto's" "$judged" "$toronto"
  status=active
  [ "$judged" = "$toronto" ] || status=expired # Toronto's midnight passed between the two reads
  expect "the memo record's status on Maple's date" "$(field .status)" "$status"
  api 200 '.items[] | "  \(.reference // "-"): \(.status) on \(.read_date) in \(.time_zone)"' listMemoRecords -p "legal_entity_id=$LE" -p status=active
  listed=$(field '[.items[] | select(.reference == "WT-1043")] | length')
  api 200 '"position read on \(.read_date): memo flows at RBC \([.banks[] | select(.house_bank_code == "RBC") | .days[].flows[]? | select(.source == "memo") | .amount.amount] | join(" "))"' \
    getCashPosition -p "legal_entity_id=$LE" -p days=7
  counted=$(field '[.banks[] | select(.house_bank_code == "RBC") | .days[].flows[]? | select(.source == "memo" and .amount.amount == "1000.00")] | length')
  [ "$(field .read_date)" != "$toronto" ] || expect "the record listed active and counted in the position on Toronto's date" "$listed $counted" "1 1"
  forecast
  forecast_sources
  passed
}

# forecast has Tom read the liquidity forecast (T53e): the supplier open
# items still owed after step 7's run, each on the day a payment run
# would pay it. Maple runs payments on Tuesdays and Thursdays (step 2), so
# SAP's USD invoice migrated in step 3, overdue once its NT30 from 15 June
# has passed, is expected on the first of them from today.
forecast() {
  say "The liquidity forecast (T53e): what Maple will pay its suppliers, per currency in its buckets (14 days, 11"
  say "weeks, 9 months), each open item on the day a payment run would pay it and at what it would pay then. Maple"
  say "runs payments on Tuesdays and Thursdays: an overdue item is expected on the next of them, an item in a discount"
  say "tier on the tier's last payment day, else on the last payment day before it is due; step 7's run paid the rest."
  local before
  before=$(company_day)
  api 200 '"read on \(.read_date) in \(.time_zone), \(.buckets | length) buckets from \(.buckets[0].start) to \(.buckets[-1].end)",
    (.currencies[] | .currency as $c | "  \($c): total \(.total.amount), beyond the last bucket \(.beyond.amount.amount) (\(.beyond.count))",
      (.buckets[] | select(.flows | length > 0) | "    from \(.start): \([.flows[] | "\(.source) \(.certainty) \(.amount.amount) x\(.count)"] | join(", "))"))' \
    getLiquidityForecast -p "legal_entity_id=$LE"
  expect "the zone the forecast is read in" "$(field .time_zone)" America/Toronto
  expect_today "the forecast's read date, Toronto's" "$(field .read_date)" "$before"
  expect "the first bucket, the read date" "$(field '.buckets[0].start')" "$(field .read_date)"
  # A day's bucket starts on the day expected: the first Tuesday or Thursday
  # from today, or, when that is one of Maple's holidays, the next working
  # day (at most four days later: Christmas on a Thursday, Boxing Day on
  # the Friday); a week's or a month's bucket may start on another day.
  # Its own flow, not the bucket's total: SAP's open orders migrated in
  # step 3 are forecast too (purchase_order, T53g).
  expect "SAP's USD invoice (step 3), on the next payment day" \
    "$(field 'def epoch: strptime("%Y-%m-%d") | mktime;
      . as $f | ([$f.buckets[] | select(.grain == "day") | .start | select(epoch | strftime("%a") | IN("Tue", "Thu"))][0] | epoch) as $pay
      | [.currencies[] | select(.currency == "USD") | .buckets | to_entries[] | select(any(.value.flows[]; .source == "payable"))
      | "\([.value.flows[] | select(.source == "payable") | .amount.amount] | join(" ")) \(if $f.buckets[.key].grain == "day" then (.value.start | epoch) - $pay | (. >= 0 and . <= 4 * 86400) else true end)"]
      | join(", ")')" "-1000.00 true"
  expect "each currency's total, its buckets' totals added up" \
    "$(field '[.currencies[] | (.total.amount | sub("\\."; "") | tonumber) == ([.buckets[].total.amount | sub("\\."; "") | tonumber] | add)] | all')" true
}

# cad_cents SOURCE is the sum of the CAD flows of the source in the last
# forecast's buckets, in cents.
cad_cents() {
  jq -r --arg s "$1" '[.currencies[] | select(.currency == "CAD") | .buckets[].flows[] | select(.source == $s)
    | .amount.amount | sub("[.]"; "") | tonumber] | add // 0' "$WORK/last.json"
}

# cad_sum FIELDS... is the sum of the CAD figures at the paths (amounts
# under a currency, such as .total or .blocked.amount) in the last
# forecast, in cents.
cad_sum() {
  local paths
  paths=$(printf '(%s.amount | sub("[.]"; "") | tonumber), ' "$@")
  jq -r "[.currencies[] | select(.currency == \"CAD\") | ${paths%, }] | add // 0" "$WORK/last.json"
}

# forecast_sources has Tom read what the forecast takes beside the posted
# items (T53e2): Alex parks an invoice from Tyrell and requests a down
# payment to Tyrell, which the forecast shows as planned and expected; the
# items of Massive Dynamic, which no run pays (step 7), are shown apart;
# both documents are cancelled again, and leave the forecast.
forecast_sources() {
  say "What the forecast takes beside the posted items (T53e2): invoices parked or on hold (invoice_unposted,"
  say "planned) at what a run would pay once they are posted, by the terms they would post with; open down payment"
  say "requests (down_payment, expected) whole on their due dates; and what the suppliers no run pays are owed"
  say "(payable_blocked), apart from the totals unless Tom asks for it (include_blocked)."
  local due parked parked_etag request request_etag before parked0 requests0 cents
  due=$(date_add "$TODAY" 5)
  QUIET=1 api 200 '' getLiquidityForecast -p "legal_entity_id=$LE"
  before=$(field .read_date)
  parked0=$(cad_cents invoice_unposted)
  requests0=$(cad_cents down_payment)
  as alex
  say "Alex parks Tyrell's invoice TY-5009 for one more chair, CAD 470.00 on the order's 2/10 net 30, and requests a"
  say "down payment of CAD 300.00 to Tyrell, due $due:"
  invoice TY-5009 1 470.00
  parked=$(field .id)
  parked_etag=$ETAG
  api 201 '"down payment request \(.document_number): \(.status), \(.amount.amount) \(.amount.currency) due \(.due_date)"' createDownPaymentRequest \
    -body "{\"legal_entity_id\": \"$LE\", \"supplier_id\": \"$SUPPLIER\", \"amount\": {\"amount\": \"300.00\", \"currency\": \"CAD\"}, \"due_date\": \"$due\", \"reference\": \"Chairs for the second floor\"}"
  request=$(field .id)
  request_etag=$ETAG
  as tom
  api 200 '.currencies[] | select(.currency == "CAD") | "CAD: total \(.total.amount), blocked suppliers \(.blocked.amount.amount) (\(.blocked.count), not in the total)",
    (.buckets[] | select(any(.flows[]; .source == "invoice_unposted" or .source == "down_payment" or .source == "payable_blocked"))
      | "    from \(.start): \([.flows[] | select(.source == "invoice_unposted" or .source == "down_payment" or .source == "payable_blocked")
        | "\(.source) \(.certainty)\(if .late then " late" else "" end) \(.amount.amount) x\(.count)"] | join(", "))")' \
    getLiquidityForecast -p "legal_entity_id=$LE"
  if [ "$(field .read_date)" = "$before" ]; then # the same day: the same payment days and discount tiers
    expect "TY-5009, planned at what a run in its discount tier pays (470.00 less 2%)" "$(($(cad_cents invoice_unposted) - parked0))" -46060
  fi
  expect "the down payment request, expected whole" "$(($(cad_cents down_payment) - requests0))" -30000
  expect "Massive Dynamic's open items, shown apart" "$(field '[.currencies[] | select(.currency == "CAD") | .blocked.count > 0] | any')" true
  expect "blocked flows counted" "$(field .include_blocked)" false
  cents=$(cad_sum .total .beyond.amount .blocked.amount)
  say "With include_blocked the blocked suppliers' flows count in the totals (and beyond the last bucket):"
  api 200 '.currencies[] | select(.currency == "CAD") | "CAD: total \(.total.amount), beyond \(.beyond.amount.amount), of which blocked \(.blocked.amount.amount)"' \
    getLiquidityForecast -p "legal_entity_id=$LE" -p include_blocked=true
  expect "blocked flows counted" "$(field .include_blocked)" true
  expect "the total and beyond with the blocked suppliers', the total and beyond without them plus what they are owed" "$(cad_sum .total .beyond.amount)" "$cents"
  say "Alex cancels the invoice and the request again; the forecast no longer has them:"
  as alex
  api 200 '"\(.document_number): \(.status)"' cancelSupplierInvoice -p "id=$parked" -if-match "$parked_etag" -body '{"reason": "Entered twice; the forecast walkthrough"}'
  api 200 '"down payment request \(.document_number): \(.status)"' cancelDownPaymentRequest -p "id=$request" -if-match "$request_etag" \
    -body '{"reason": "Tyrell needs no down payment"}'
  as tom
  QUIET=1 api 200 '' getLiquidityForecast -p "legal_entity_id=$LE"
  if [ "$(field .read_date)" = "$before" ]; then
    expect "the parked invoices after TY-5009 was cancelled" "$(cad_cents invoice_unposted)" "$parked0"
  fi
  expect "the down payment requests after the request was cancelled" "$(cad_cents down_payment)" "$requests0"
}

step_reports() {
  heading "Step 14 — The trial balance, and an analytics question through the API"
  as carla
  api 200 '"\(.items | length) account balances (period totals per account and currency) in 0L"' listBalances \
    -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR" -p page_size=500
  say "Summed per account in CAD (whole cents, from the API's period totals):"
  jq -r --slurpfile acc "$WORK/accounts.json" '
    def c: sub("\\."; "") | tonumber;
    def fmt: (if . < 0 then "-" else "" end) as $s | (if . < 0 then -. else . end) as $v
      | "\($s)\(($v / 100) | floor).\((($v % 100) + 100) | tostring | .[1:])";
    ($acc[0].items | map({key: .id, value: "\(.number) \(.name)"}) | from_entries) as $names
    | [.items[] | {a: .account_id, d: (.debit_lc.amount | c), k: (.credit_lc.amount | c)}]
    | group_by(.a) | map({n: $names[.[0].a], d: (map(.d) | add), k: (map(.k) | add)}) | sort_by(.n)
    | (.[] | "\(.n | .[0:40] | . + (" " * (40 - length)))  debit \((.d | fmt) | (" " * (14 - length)) + .)  credit \((.k | fmt) | (" " * (14 - length)) + .)"),
      "\("TOTAL" + (" " * 35))  debit \((map(.d) | add | fmt) | (" " * (14 - length)) + .)  credit \((map(.k) | add | fmt) | (" " * (14 - length)) + .)"' \
    "$WORK/last.json" | sed 's/^/     /'
  local balanced
  balanced=$(jq '[.items[] | (.debit_lc.amount | sub("\\."; "") | tonumber) - (.credit_lc.amount | sub("\\."; "") | tonumber)] | add' "$WORK/last.json")
  expect "debits less credits" "$balanced" 0
  local row acc per cur net
  row=$(jq -c '([.items[] | select(.debit_lc.amount != "0.00" and .credit_lc.amount != "0.00")] | first) // .items[0]' "$WORK/last.json")
  acc=$(jq -r .account_id <<<"$row")
  per=$(jq -r .fiscal_period <<<"$row")
  cur=$(jq -r .currency <<<"$row")
  net=$(jq -r '(.debit_lc.amount | sub("\\."; "") | tonumber) - (.credit_lc.amount | sub("\\."; "") | tonumber)' <<<"$row")
  say "One account's row drilled down to its entries (T73j2: the journal list by account_id and fiscal_period):"
  api 200 '.items[] | "entry \(.document_number) of \(.posting_date): " + ([.lines[] | select(.account_id == "'"$acc"'") | "line \(.line_no) \(.amount_lc.amount)"] | join(", "))' \
    listJournalEntries -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR" -p "fiscal_period=$per" -p "account_id=$acc" -p page_size=500
  expect "entries without a line on the account" "$(field "[.items[] | select([.lines[].account_id] | index(\"$acc\") | not)] | length")" 0
  expect "the entries' lines on the account, in $cur, less the row's debits less credits" \
    "$(field "[.items[].lines[] | select(.account_id == \"$acc\" and .amount.currency == \"$cur\") | .amount_lc.amount | sub(\"\\\\.\"; \"\") | tonumber] | add - $net")" 0
  say "\"Revenue by year\": the governed revenue metric by fiscal year, read under Carla's access."
  show erp query -measures revenue -by fiscal_year -companies "$CC"
  "$ERP" query -measures revenue -by fiscal_year -companies "$CC" >"$WORK/query.out" 2>&1 || { sed 's/^/     /' "$WORK/query.out"; fail "the query failed"; }
  sed 's/^/     /' "$WORK/query.out"
  grep -q "$YEAR" "$WORK/query.out" || fail "the query has no row for $YEAR"
  say "Revenue is sales, SAP's 400000 and the pack's 4000 Sales of step 11; income adds what automatic postings"
  say "book on revenue accounts, such as the 94.00 cash discount of step 7 (account determination cash_discount):"
  show erp query -measures revenue,income -by fiscal_year -companies "$CC" -csv year.csv
  "$ERP" query -measures revenue,income -by fiscal_year -companies "$CC" -csv "$WORK/year.csv" >"$WORK/query.out" 2>&1 ||
    { sed 's/^/     /' "$WORK/query.out"; fail "the query failed"; }
  sed 's/^/     /' "$WORK/query.out"
  local revenue income
  revenue=$(awk -F, -v y="$YEAR" '$1 == y { print $2 }' "$WORK/year.csv")
  income=$(awk -F, -v y="$YEAR" '$1 == y { print $3 }' "$WORK/year.csv")
  expect "income less revenue in $YEAR" "$(amount $(($(cents "$income") - $(cents "$revenue"))))" 94.00
  say "Expenses by month, SAP's months and the pilot's own: months come in their order, 9 before 10, not 1, 10, 2:"
  show erp query -measures expenses -by fiscal_month -where "fiscal_year in $YEAR" -companies "$CC" -csv months.csv
  "$ERP" query -measures expenses -by fiscal_month -where "fiscal_year in $YEAR" -companies "$CC" -csv "$WORK/months.csv" >"$WORK/query.out" 2>&1 ||
    { sed 's/^/     /' "$WORK/query.out"; fail "the query failed"; }
  sed 's/^/     /' "$WORK/query.out"
  expect "the months' order" "$(awk -F, 'NR > 1 { printf "%s ", $1 }' "$WORK/months.csv")" "$(awk -F, 'NR > 1 { print $1 }' "$WORK/months.csv" | sort -n | tr '\n' ' ')"
  passed
}

# GR/IR position of an order as the balances list it.
GRIR_POSITION='(.items[] | "order \(.purchase_order_number) item \(.po_item_no) on \(.account_number) \(.account_name): received \(.received_quantity.value), invoiced \(.invoiced_quantity.value), cleared \(.cleared_quantity.value), migrated \(.migrated_quantity.value), balance \(.balance.amount) \(.balance.currency)"), "totals: \(.totals.positions) position(s), \(.totals.balance_lc.amount) \(.totals.balance_lc.currency)"'

step_grir() {
  heading "Step 15 — GR/IR maintenance before the month-end close: the closed order's residual position is cleared (SAP MR11)"
  as ben
  say "Tyrell credited the damaged chair (step 6) and the buyer keeps it: nothing more will be delivered or invoiced,"
  say "so Ben closes the order."
  QUIET=1 api 200 '' getPurchaseOrder -p "id=$PO"
  cp "$WORK/last.json" "$WORK/order.json"
  api 200 '"purchase order \(.document_number): \(.status)"' closePurchaseOrder -p "id=$PO" -if-match "$ETAG"
  expect "order status" "$(field .status)" closed
  as alex
  say "Before the close Alex reviews the open GR/IR positions: the order is left with ten chairs received and nine"
  say "invoiced net of the credit memo, a credit on the GR/IR account that no invoice will clear."
  api 200 "$GRIR_POSITION" listGrIrBalances -p "legal_entity_id=$LE" -p "purchase_order_id=$PO"
  expect "the residual balance" "$(field '.items[0].balance.amount')" -470.00
  expect "the order, named by its number" "$(field '.items[0].purchase_order_number')" "$(jq -r .document_number "$WORK/order.json")"
  expect "the order's total" "$(field '.totals.balance_lc.amount')" -470.00
  say "Every open position of the company, totalled by the report (no adding up):"
  api 200 '"\(.totals.positions) open position(s), \(.totals.balance_lc.amount) \(.totals.balance_lc.currency) (group \(.totals.balance_gc.amount) \(.totals.balance_gc.currency))\(if .totals.balance then ", \(.totals.balance.amount) \(.totals.balance.currency) in the orders'"'"' currency" else "" end)"' \
    listGrIrBalances -p "legal_entity_id=$LE" -p page_size=1
  local yesterday
  yesterday=$(date_add "$TODAY" -1)
  say "The period's close checks them as at its end (T73i2). As at yesterday, before today's invoices, the shelving"
  say "units SAP received were not yet invoiced, so their migrated credit was still open, and Tyrell's order had no position:"
  api 200 "$GRIR_POSITION" listGrIrBalances -p "legal_entity_id=$LE" -p "posting_date_to=$yesterday"
  expect "the shelving units' position as at yesterday" "$(field "[.items[] | select(.purchase_order_id == \"$GRIR_PO\") | .balance.amount] | join(\" \")")" "$GRIR_BALANCE"
  expect "Tyrell's order as at yesterday" "$(field "[.items[] | select(.purchase_order_id == \"$PO\")] | length")" 0
  say "She proposes clearing it to the price difference account; the controller decides."
  api 201 '"GR/IR clearing of \(.purchase_order_number): \(.status), \(.total.amount) \(.total.currency) against \(.offset_account.number) \(.offset_account.name)\([.items[] | "\n  item \(.po_item_no) \(.description): \(.quantity.value) \(.quantity.unit), \(.amount.amount) \(.amount.currency)"] | join(""))"' \
    createGrIrClearing -p "id=$PO" -body "{\"reason\": \"Damaged chair credited by Tyrell and kept; no further delivery or invoice\", \"posting_date\": \"$TODAY\"}"
  local clearing request body
  clearing=$(field .id)
  request=$(field .approval_request_id)
  body="{\"reason\": \"Again\", \"posting_date\": \"$TODAY\"}"
  say "While it waits, a second proposal for the order is refused, and Alex, who holds no approver role, cannot approve it"
  say "(nor could a controller who proposed it: the proposer is never the approver):"
  refused 409 procurement.gr_ir_clearing_pending createGrIrClearing -p "id=$PO" -body "$body"
  api 200 '"approval request \(.object_type): \(.object_resource)"' getApprovalRequest -p "id=$request"
  expect "the request's object" "$(field .object_resource)" "/v1/gr-ir-clearings/$clearing"
  refused 403 identity.permission_missing approveApprovalRequest -p "id=$request" -if-match "$ETAG" -body '{}'
  approve carla "$request" "Cleared before the month-end close"
  as alex
  api 200 '"GR/IR clearing: \(.status), journal entry \(.journal_entry.document_number) of \(.journal_entry.fiscal_year)"' \
    getGrIrClearing -p "id=$clearing"
  expect "clearing status" "$(field .status)" posted
  local entry
  entry=$(field .journal_entry.id)
  api 200 '.lines[] | "\(.account_id | acct): \(.amount.amount) \(.amount.currency)"' getJournalEntry -p "id=$entry"
  say "The GR/IR report shows the position cleared and no longer open:"
  api 200 "$GRIR_POSITION" listGrIrBalances -p "legal_entity_id=$LE" -p "purchase_order_id=$PO" -p open_only=false
  expect "the balance after clearing" "$(field '.items[0].balance.amount')" 0.00
  expect "the quantity cleared" "$(field '.items[0].cleared_quantity.value')" 1
  api 200 '"open positions of the order: \(.items | length)"' listGrIrBalances -p "legal_entity_id=$LE" -p "purchase_order_id=$PO"
  expect "open positions" "$(field '.items | length')" 0
  say "Proposing it again changes nothing: there is nothing left to clear."
  refused 409 procurement.nothing_to_clear createGrIrClearing -p "id=$PO" -body "$body"
  api 200 '.items | reverse | .[] | "\(.occurred_at)  \(.actor_id)  \(.action)  \(.outcome)"' listAuditEvents -p "object_id=$clearing" | name_actors
  passed
}

step_downpayment() {
  heading "Step 16 — SAP's open down payment is cleared against the supplier's final invoice (SAP F-54)"
  as alex
  say "$DP_NAME's final invoice arrives: CAD 5,000.00 plus 250.00 GST on V1. Alex enters it in a hurry, without"
  say "the item's description, and misreads the tax as 260.00."
  local item invoice fy line
  item="{\"account_id\": \"$(account 6000)\", \"quantity\": {\"value\": \"1\", \"unit\": \"EA\"}, \"amount\": {\"amount\": \"5000.00\", \"currency\": \"CAD\"}, \"tax_code\": \"V1\"}"
  api 201 '"\(.kind) \(.document_number) \(.supplier_reference): \(.status), \(.gross_amount.amount) \(.gross_amount.currency)"' createSupplierInvoice \
    -body "{\"legal_entity_id\": \"$LE\", \"supplier_id\": \"$DP_SUPPLIER\", \"supplier_reference\": \"FINAL-DP\", \"invoice_date\": \"$TODAY\", \"gross_amount\": {\"amount\": \"5260.00\", \"currency\": \"CAD\"}, \"tax_amount\": {\"amount\": \"260.00\", \"currency\": \"CAD\"}, \"items\": [$item]}"
  invoice=$(field .id)
  say "Posting calculates the tax at the codes' rates and refuses the invoice's 260.00. The problem carries the"
  say "figures as fields (T73e2, ADR-0052 decision D3), which the web workspace shows in the clerk's language:"
  refused 422 payables.tax_mismatch postSupplierInvoice -p "id=$invoice" -if-match '"1"' -body "{\"posting_date\": \"$TODAY\"}"
  excerpt '.calculated_tax | "stated \(.stated.amount) \(.stated.currency), calculated \(.calculated.amount) \(.calculated.currency): \([.codes[] | "\(.tax_code) \(.tax.amount)"] | join(", "))"'
  expect "the refused tax's figures" "$(field '.calculated_tax | "\(.stated.amount) \(.calculated.amount) \([.codes[] | "\(.tax_code) \(.tax.amount)"] | join(" "))"')" \
    "260.00 250.00 V1 250.00"
  say "Alex corrects the parked invoice's tax and gross (a merge patch of the two figures alone) and posts it."
  api 200 '"\(.document_number): \(.status), tax \(.tax_amount.amount), gross \(.gross_amount.amount)"' updateSupplierInvoice -p "id=$invoice" -if-match '"1"' \
    -body '{"gross_amount": {"amount": "5250.00", "currency": "CAD"}, "tax_amount": {"amount": "250.00", "currency": "CAD"}}'
  say "Read by itself, the invoice now states the tax its code's rate gives (T73e3: its tax per code outside a refusal)."
  api 200 '"\(.document_number): stated \(.calculated_tax.stated.amount), calculated \(.calculated_tax.calculated.amount): \([.calculated_tax.codes[] | "\(.tax_code) \(.tax.amount)"] | join(", "))"' \
    getSupplierInvoice -p "id=$invoice"
  expect "the invoice's tax per code" "$(field '.calculated_tax | "\(.stated.amount) \(.calculated.amount) \([.codes[] | "\(.tax_code) \(.tax.amount)"] | join(" "))"')" \
    "250.00 250.00 V1 250.00"
  api 200 '"\(.document_number): \(.status), journal entry \(.journal_entry_id)"' postSupplierInvoice -p "id=$invoice" -if-match "$ETAG" -body "{\"posting_date\": \"$TODAY\"}"
  expect "the final invoice's status" "$(field .status)" posted
  say "The posting checks the CRA's documentary requirements for the input tax credit, which the Canada pack"
  say "brings (SOR/91-45 s. 3, ADR-0049 §6): from CAD 100 the supplier's GST/HST number, on file from SAP; from"
  say "CAD 500 a description of each supply, which this item lacks. A warning on the invoice, not a refusal:"
  excerpt '.documentary_warnings[] | "warning \(.code): from \(.threshold.amount) \(.threshold.currency), \(.source)"'
  expect "the final invoice's documentary warnings" "$(field '[.documentary_warnings[] | "\(.code) \(.threshold.amount)"] | join(", ")')" \
    "item_description_missing 500.00"
  say "She clears the migrated down payment against it, as she would one the platform paid: the transfer credits"
  say "the down payment account, reverses the 100.00 input tax SAP claimed on it (the invoice claims the whole tax)"
  say "and debits the supplier's payables with the down payment, netted against the invoice in the next run."
  local tag="$ETAG" chosen
  say "She chooses it, as the web workspace's screen lists them (T73e4), from the supplier's open items with special"
  say "G/L indicator A, the API's own filter: the down payment alone, none of the supplier's ordinary items."
  api 200 '.items[] | "open item \(.document_number), \(.posting_date): \(.amount.amount) \(.amount.currency), special G/L \(.special_gl)"' \
    listOpenItems -p "legal_entity_id=$LE" -p "partner_id=$DP_SUPPLIER" -p special_gl=A -p page_size=100
  expect "the supplier's open down payments" "$(field '[.items[] | "\(.journal_entry_id) \(.special_gl)"] | join(", ")')" "$DP_ENTRY A"
  read -r fy line <<<"$DP_LINE"
  expect "the down payment's item" "$(field '.items[0] | "\(.fiscal_year) \(.line_no)"')" "$fy $line"
  chosen=$(field '[.items[] | {fiscal_year, journal_entry_id, line_no}] | tojson')
  api 200 '"\(.document_number): \(.status)"' clearSupplierInvoiceDownPayments -p "id=$invoice" -if-match "$tag" \
    -body "{\"posting_date\": \"$TODAY\", \"items\": $chosen}"
  api 200 '"down payment request \(.document_number): \(.status), cleared against \(.cleared_invoice_id) by entry \(.clearing_entry_id)"' \
    getDownPaymentRequest -p "id=$DP_REQUEST"
  expect "the migrated down payment request" "$(field .status)" cleared
  api 200 '.lines[] | "\(.account_id | acct): \(.amount.amount) \(.amount.currency)\(if .tax_code then ", tax code \(.tax_code)" else "" end)\(if .special_gl then ", special G/L \(.special_gl)" else "" end)"' \
    getJournalEntry -p "id=$(field .clearing_entry_id)"
  expect "the input tax reversed" "$(field '[.lines[] | select(.tax_code == "V1" and .special_gl == null) | .amount.amount] | join(" ")')" -100.00
  api 200 '"open down payments of the supplier: \(.items | length)"' listOpenItems -p "legal_entity_id=$LE" -p "partner_id=$DP_SUPPLIER" -p special_gl=A -p page_size=100
  expect "open down payments left" "$(field '.items | length')" 0
  passed
}

step_audit() {
  heading "Step 17 — The audit trail: who did what to Tyrell's invoices"
  as ada
  local inv
  for inv in "$INVOICE" "$DUPLICATE" "$CREDIT"; do
    api 200 '.items | reverse | .[] | "\(.occurred_at)  \(.actor_id)  \(.action)  \(.outcome)\(if .detail.reason then ": \(.detail.reason)" else "" end)"' \
      listAuditEvents -p "object_id=$inv" | name_actors
  done
  api 200 '.items | reverse | .[] | "\(.occurred_at)  \(.actor_id)  \(.action)  \(.outcome)"' listAuditEvents -p "object_id=$RUN" | name_actors
  say "The tenant's chain of audit records is verified, each record against the hash of the one before:"
  api 202 '"\(.kind) \(.status)"' verifyAuditChain -body '{}'
  wait_op
  excerpt '"audit chain \(if .result.verified then "intact" else "BROKEN" end): \(.result.checked) records checked, \(.result.anchors_checked) anchors"'
  expect "audit chain verified" "$(field .result.verified)" true
  passed
}

step_capture() {
  heading "Step 18 — A scanned invoice above the request limit is captured by upload, through the API's own signed path"
  as alex
  say "Tyrell sends an invoice for one unit of item 1 of another of its open orders, migrated from SAP (the chairs'"
  say "order was closed in step 15, and an invoice names a released order), as a 6 MB scan, above the 5 MB a request"
  say "carries. The web workspace never reaches the object store (ADR-0052 §7, product owner 2026-11-01): it asks for"
  say "the API's own signed path (via api) and streams the bytes there, and the API streams them into the store, up to 50 MB."
  local number up url size invoice order price item
  api 200 '"\(.items | length) released order(s) of Tyrell, the first \(.items[0].document_number)"' listPurchaseOrders \
    -p "legal_entity_id=$LE" -p "supplier_id=$SUPPLIER" -p status=released -p page_size=1
  order=$(field '.items[0].id')
  [ -n "$order" ] && [ "$order" != null ] || fail "Tyrell has no released order to invoice"
  QUIET=1 api 200 '' getPurchaseOrder -p "id=$order"
  number=$(field .document_number)
  price=$(field '.items[] | select(.item_no == 1) | .unit_price.amount')
  item=$(field '.items[] | select(.item_no == 1) | .description')
  [ "$(field '.items[] | select(.item_no == 1) | .price_unit')" = 1 ] || fail "item 1 of $number is not priced per unit"
  { printf '%%PDF-1.7\n%% Tyrell Office Products, invoice TY-5002 for order %s\n' "$number"; head -c 6291456 /dev/zero | tr '\0' ' '; printf '\n%%%%EOF\n'; } >"$WORK/TY-5002.pdf"
  size=$(wc -c <"$WORK/TY-5002.pdf" | tr -d ' ')
  jq -n --arg po "$number" --arg date "$TODAY" --arg price "$price" --arg item "$item" '{document_kind: "invoice", credited_invoice_number: "",
    vendor_name: "Tyrell Office Products", vendor_tax_id: "", invoice_number: "TY-5002", invoice_date: $date, purchase_order: $po, currency: "CAD",
    lines: [{description: $item, quantity: "1", amount: $price, po_line: 1, tax_rate: ""}],
    tax_amount: "0.00", gross_amount: $price, confidence: "high"} | tojson' -r >"$WORK/model-answer.json"
  api 201 '"upload \(.id): \(.status), via \(.via), at most \(.max_bytes) bytes of \(.content_type) until \(.expires_at)"' createUpload \
    -body '{"purpose": "supplier_invoice", "content_type": "application/pdf", "via": "api"}'
  expect "the upload's way" "$(field .via)" api
  up=$(field .id)
  url=$(field .url)
  case "$url" in "/v1/uploads/$up/content?"*) ;; *) fail "the upload's URL is not the API's signed path: $url" ;; esac
  say "The URL is a path on the API, signed for the tenant and this upload until it expires. The bytes go there"
  say "as they are read (chunked: the API learns the size as it receives them), with the content type promised:"
  show curl -X PUT -H "Content-Type: application/pdf" -H "Transfer-Encoding: chunked" -T TY-5002.pdf "\$ERP_API_URL${url%%\?*}?…"
  local code
  code=$(curl -s -o "$WORK/put.out" -w '%{http_code}' -X PUT -H "Authorization: Bearer $ERP_TOKEN" -H "Content-Type: application/pdf" \
    -H "Transfer-Encoding: chunked" -T "$WORK/TY-5002.pdf" "$ERP_API_URL$url")
  expect "the signed path's answer" "$code" 204
  api 200 '"upload \(.id): \(.status), \(.size) bytes"' getUpload -p "id=$up"
  expect "the upload's status" "$(field .status)" received
  expect "the bytes received" "$(field .size)" "$size"
  say "The path takes one document: the same URL again is refused, the upload already holds its bytes."
  code=$(curl -s -o "$WORK/put.out" -w '%{http_code}' -X PUT -H "Content-Type: application/pdf" --data-binary @"$WORK/TY-5002.pdf" "$ERP_API_URL$url")
  expect "a second PUT" "$code $(jq -r .code "$WORK/put.out")" "409 payables.upload_closed"
  say "  refused: $code $(jq -r .code "$WORK/put.out")"
  say "Alex captures the invoice from the upload. Reading the scan is the model's (here the stand-in's); the"
  say "capture runs as an operation, polled until it names the new invoice:"
  api 202 '"operation \(.kind): \(.status)"' captureSupplierInvoice -body "{\"legal_entity_id\": \"$LE\", \"upload_id\": \"$up\"}"
  wait_op
  invoice=$(field '.result.supplier_invoice_id')
  api 200 '"\(.document_number) \(.supplier_reference): \(.status), source \(.source), \(.gross_amount.amount) \(.gross_amount.currency)"' getSupplierInvoice -p "id=$invoice"
  expect "the captured invoice" "$(field '"\(.supplier_reference) \(.status) \(.source)"')" "TY-5002 parked capture"
  QUIET=1 api 200 '' getUpload -p "id=$up"
  expect "the upload, consumed" "$(field '"\(.status) \(.used_by)"')" "used $invoice"
  show curl -s -H "Authorization: Bearer …" "\$ERP_API_URL/v1/supplier-invoices/$invoice/document" "| cmp - TY-5002.pdf"
  curl -s -H "Authorization: Bearer $ERP_TOKEN" "$ERP_API_URL/v1/supplier-invoices/$invoice/document" | cmp -s - "$WORK/TY-5002.pdf" ||
    fail "the invoice's document is not the scan uploaded"
  say "  the invoice's document is the scan, byte for byte ($size bytes)"
  passed
}

# ASSET_SETTINGS gives company @CC@ the asset class MACHINES (ADR-0057 §3,
# T57a): straight-line over ten years in the leading ledger and declining
# balance at twice the rate in every other ledger it keeps; its accounts,
# the acquisition cost and accumulated depreciation flagged as asset
# reconciliation accounts; and this fiscal year's number ranges for assets
# and their entries (AA, AF). @YEAR@ is replaced with this year.
ASSET_SETTINGS='
.asset_classes = [{"code": "MACHINES", "name": "Machinery and equipment",
    "terms": [(.companies[] | select(.code == "@CC@") | .ledgers // ["0L"])[] |
      if . == "0L" then {"ledger": ., "method": "straight_line", "useful_life_months": 120}
      else {"ledger": ., "method": "declining_balance", "useful_life_months": 120, "declining_factor": "2"} end]},
  {"code": "TOOLS", "name": "Tools and fixtures",
    "terms": [(.companies[] | select(.code == "@CC@") | .ledgers // ["0L"])[] | {"ledger": ., "method": "straight_line", "useful_life_months": 60}]}]
| (.companies[] | select(.code == "@CC@")) |= (
    .chart_of_accounts += [
      {"number": "1600", "name": "Machinery", "type": "asset", "asset_reconciliation": true},
      {"number": "1609", "name": "Machinery, accumulated depreciation", "type": "asset", "asset_reconciliation": true},
      {"number": "1690", "name": "Asset clearing", "type": "asset"},
      {"number": "1695", "name": "Asset sale clearing", "type": "asset"},
      {"number": "4950", "name": "Gain on asset retirements", "type": "revenue"},
      {"number": "6250", "name": "Depreciation of machinery", "type": "expense"},
      {"number": "6950", "name": "Loss on asset retirements", "type": "expense"}]
    | .asset_accounts = (["MACHINES", "TOOLS"] | map({"class": ., "acquisition_cost": "1600", "accumulated_depreciation": "1609", "depreciation": "6250",
        "clearing": "1690", "gain": "4950", "loss": "6950", "sale_clearing": "1695"})))
| .number_ranges += (["asset", "journal_aa", "journal_af"] | map({"company": "@CC@", "document_type": ., "fiscal_year": @YEAR@, "first": 1, "last": 999999}))'

step_assets() {
  heading "Step 19 — Asset accounting: an asset class and its accounts through a governed change; the cost account takes only assets' postings"
  as ada
  say "Ada proposes the pilot's first asset class (ADR-0057 §3): machinery, depreciated straight-line over ten"
  say "years in the leading ledger and at twice the declining rate in 2L, with its accounts. The acquisition cost"
  say "and accumulated depreciation accounts are asset reconciliation accounts, which only the assets context"
  say "posts to, so their balances are the assets' values. The company numbers its assets and their entries"
  say "(AA, AF) in this fiscal year's ranges. A second class, tools and fixtures over five years, shares the accounts."
  api 200 '"active configuration version \(.items[0].number)"' listConfigVersions -p is_active=true
  local base settings cr request
  base=$(field '.items[0].number')
  settings=${ASSET_SETTINGS//@YEAR@/$YEAR}
  settings=${settings//@CC@/$CC}
  jq "{title: \"Asset class MACHINES and its accounts\", rationale: \"Asset accounting for the pilot (T57a)\", base_version: $base,
    proposed: (.items[0].document | $settings)}" "$WORK/last.json" >"$WORK/assets.json"
  api 201 '"change request \(.number), \(.status): \(.title)"' createChangeRequest -body @assets.json
  cr=$(field .id)
  api 200 '"\(.status), tier \(.tier) (structural): \([.items[].path] | join(", "))"' submitChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "the change's tier" "$(field .tier)" 4
  expect "the asset class among the changes" "$(field '[.items[] | select(.path == "asset_classes[MACHINES]")] | length')" 1
  say "New accounts change the chart, a tier 4 change: the invoices of the last year are replayed under it."
  api 201 '"regression suite \(.number): \(.case_count) cases"' createRegressionSuite -body '{"name": "Before asset accounting", "window_days": 366}'
  QUIET=1 api 200 '' getChangeRequest -p "id=$cr"
  api 202 '"\(.kind) \(.status)"' simulateChangeRequest -p "id=$cr" -if-match "$ETAG"
  wait_op
  api 200 '.evidence[] | "evidence \(.kind): \(if .passed then "passed" else "FAILED" end), \(.summary)"' getChangeRequest -p "id=$cr"
  expect "status after the evidence" "$(field .status)" pending_approval
  request=$(field .workflow_request_id)
  approve sam "$request" "Asset class and accounts checked"
  approve carla "$request" "The asset accounts are new and unposted"
  as ada
  QUIET=1 api 200 '' getChangeRequest -p "id=$cr"
  api 200 '"\(.status): configuration version \(.activated_version)"' activateChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "status after activation" "$(field .status)" active
  as carla
  say "Carla reads the class as the assets context gives it: its terms per ledger and Maple's accounts, named:"
  api 200 '.items[] | "class \(.code) \(.name):", (.terms[] | "  \(.ledger_code) \(.ledger_name): \(.method), \(.useful_life_months) months\(if .declining_factor then ", factor \(.declining_factor)" else "" end)"),
    (.companies[] | "  company \(.company_code) \(.company_name): cost \(.accounts.acquisition_cost.number) \(.accounts.acquisition_cost.name), accumulated depreciation \(.accounts.accumulated_depreciation.number), depreciation \(.accounts.depreciation.number), clearing \(.accounts.clearing.number)")' \
    listAssetClasses -p "legal_entity_id=$LE"
  expect "the classes of $CC" "$(field '[.items[] | .code] | join(" ")')" "MACHINES TOOLS"
  expect "the class's cost account in $CC" "$(field '.items[0].companies[] | select(.company_code == "'"$CC"'") | "\(.accounts.acquisition_cost.number) \(.accounts.acquisition_cost.name)"')" \
    "1600 Machinery"
  expect "the leading ledger's terms" "$(field '.items[0].terms[] | select(.ledger_code == "0L") | "\(.method) \(.useful_life_months)"')" "straight_line 120"
  accounts
  local cost clearing
  cost=$(account 1600)
  clearing=$(account 1690)
  api 200 '"account \(.number) \(.name): reconciliation kind \(.reconciliation_kind)"' getAccount -p "id=$cost"
  expect "the cost account's reconciliation kind" "$(field .reconciliation_kind)" asset
  say "A manual entry moving a machine's cost from the clearing account onto the cost account is refused: only an"
  say "acquisition of an asset (T57c) posts there, so the account's balance stays the assets' cost."
  refused 422 ledger.asset_reconciliation_account postJournalEntry \
    -body "{\"legal_entity_id\": \"$LE\", \"document_date\": \"$TODAY\", \"posting_date\": \"$TODAY\", \"currency\": \"CAD\", \"header_text\": \"Machine bought in error by hand\", \"lines\": [{\"account_id\": \"$cost\", \"amount\": {\"amount\": \"5000.00\", \"currency\": \"CAD\"}}, {\"account_id\": \"$clearing\", \"amount\": {\"amount\": \"-5000.00\", \"currency\": \"CAD\"}}]}"
  expect "the refused line" "$(field '[.errors[].field] | join(" ")')" "lines[0].account_id"
  passed
}

step_asset_master() {
  heading "Step 20 — The asset master: a machine on its cost center with its terms per ledger; an asset created in error cancelled"
  as carla
  say "Carla creates the company's first machine as an asset of class MACHINES on the finance office's cost center"
  say "(ADR-0057 §5, §8). It is numbered from the company's asset range, takes its class's depreciation terms in every"
  say "ledger the company keeps, and its profit center from the cost center today. It is planned: an acquisition gives"
  say "it its value (T57c)."
  local cc asset second ledgers
  QUIET=1 api 200 '' listCostCenters -p "legal_entity_id=$LE" -p code=CC-1100
  cc=$(field '.items[0].id')
  QUIET=1 api 200 '' listAssetClasses -p "legal_entity_id=$LE"
  ledgers=$(field '.items[] | select(.code == "MACHINES") | .terms | length')
  api 201 '"asset \(.company_code)/\(.asset_number) \(.description): \(.status)"' createAsset \
    -body "{\"legal_entity_id\": \"$LE\", \"class_code\": \"MACHINES\", \"description\": \"CNC milling machine\", \"cost_center_id\": \"$cc\"}"
  asset=$(field .id)
  api 200 '"asset \(.asset_number) \(.description), class \(.class_code) \(.class_name), \(.status)",
    "  cost center \(.cost_center_code) \(.cost_center_name), profit center \(.profit_center_code) \(.profit_center_name)",
    (.terms[] | "  \(.ledger_code) \(.ledger_name): \(.method), \(.useful_life_months) months\(if .declining_factor then ", factor \(.declining_factor)" else "" end), residual value \(.residual_value.amount) \(.residual_value.currency)")' \
    getAsset -p "id=$asset"
  expect "the asset's class" "$(field '"\(.class_code) \(.class_name)"')" "MACHINES Machinery and equipment"
  expect "the asset's status" "$(field .status)" planned
  expect "the asset's cost center" "$(field .cost_center_code)" CC-1100
  expect "the profit center, the cost center's" "$(field .profit_center_code)" PC-MTL
  expect "terms in every ledger the class has for the company" "$(field '.terms | length')" "$ledgers"
  expect "the leading ledger's terms" "$(field '.terms[] | select(.ledger_code == "0L") | "\(.method) \(.useful_life_months)"')" "straight_line 120"
  say "The machine goes into use next month, so its depreciation starts then in every ledger; a residual value of"
  say "500.00 is kept in the leading ledger."
  local next leading y=${TODAY:0:4} m=$((10#${TODAY:5:2} + 1))
  if [ "$m" -gt 12 ]; then m=1 y=$((y + 1)); fi
  next=$(printf '%s-%02d-01' "$y" "$m")
  leading=$(field '.terms[] | select(.ledger_code == "0L") | .ledger_id')
  api 200 '(.terms[] | "  \(.ledger_code): from \(.depreciation_start), residual value \(.residual_value.amount) \(.residual_value.currency)")' updateAsset \
    -p "id=$asset" -if-match "$ETAG" \
    -body "{\"depreciation_start\": \"$next\", \"terms\": [{\"ledger_id\": \"$leading\", \"residual_value\": {\"amount\": \"500.00\", \"currency\": \"CAD\"}}]}"
  expect "the depreciation start" "$(field '[.terms[].depreciation_start] | unique | join(" ")')" "$next"
  expect "the leading ledger's residual value" "$(field '.terms[] | select(.ledger_code == "0L") | .residual_value.amount')" "500.00"
  say "A second machine was entered twice by mistake; Carla cancels the copy. It keeps its number, and a cancelled"
  say "asset is final."
  api 201 '"asset \(.company_code)/\(.asset_number) \(.description): \(.status)"' createAsset \
    -body "{\"legal_entity_id\": \"$LE\", \"class_code\": \"MACHINES\", \"description\": \"CNC milling machine (entered twice)\", \"cost_center_id\": \"$cc\"}"
  second=$(field .id)
  api 200 '"asset \(.asset_number): \(.status)"' cancelAsset -p "id=$second" -if-match "$ETAG"
  expect "the copy's status" "$(field .status)" cancelled
  refused 409 assets.asset_not_planned cancelAsset -p "id=$second" -if-match "$ETAG"
  api 200 '.items[] | "  \(.company_code)/\(.asset_number) \(.class_code) \(.description): \(.status)"' listAssets -p "legal_entity_id=$LE"
  expect "the company's assets" "$(field '[.items[].status] | join(" ")')" "planned cancelled"
  passed
}

# step_cash_items is the liquidity forecast by liquidity item (T53e3,
# ADR-0051 §4, §9): Alex parks an invoice for implementation services on
# 6100; Ada proposes an item for professional services and a rule taking
# 6100 to it, a tier 2 change whose simulation lists the open flows the
# rule moves; once the process owner approves and Ada activates it, Tom
# reads the forecast's rows by item and its hierarchy. The invoice is
# cancelled again.
step_cash_items() {
  heading "Step 21 — The liquidity forecast by liquidity item; a derivation rule simulated on the open flows it moves"
  as alex
  say "Alex parks an invoice from Tyrell for implementation services, CAD 800.00 on 6100, not on an order:"
  local item invoice invoice_etag cr base request
  item="{\"account_id\": \"$(account 6100)\", \"description\": \"Implementation services\", \"quantity\": {\"value\": \"1\", \"unit\": \"EA\"}, \"amount\": {\"amount\": \"800.00\", \"currency\": \"CAD\"}}"
  api 201 '"\(.kind) \(.document_number) \(.supplier_reference): \(.status), \(.gross_amount.amount) \(.gross_amount.currency)"' createSupplierInvoice \
    -body "{\"legal_entity_id\": \"$LE\", \"supplier_id\": \"$SUPPLIER\", \"supplier_reference\": \"TY-SVC-1\", \"invoice_date\": \"$TODAY\", \"gross_amount\": {\"amount\": \"800.00\", \"currency\": \"CAD\"}, \"tax_amount\": {\"amount\": \"0.00\", \"currency\": \"CAD\"}, \"items\": [$item]}"
  invoice=$(field .id)
  invoice_etag=$ETAG
  as tom
  say "Tom reads the forecast by liquidity item (T53e3): each flow is split over the items its documents' lines"
  say "derive; with only the default rule of step 2 every supplier payment is Supplier payments, under Operating, and"
  say "step 13's memo records are the Customer receipts they name (T53e4):"
  api 200 '.currencies[] | .currency as $c | (.items[] | "  \($c) \("  " * .level)\(.liquidity_item // "unclassified") \(.name // ""): total \(.total.amount), beyond \(.beyond.amount)")' \
    getLiquidityForecast -p "legal_entity_id=$LE"
  expect "the top rows add up to each bucket's total" \
    "$(field '[.currencies[] | . as $c | range(0; $c.buckets | length) as $i
      | ([$c.items[] | select(.level == 0) | .buckets[$i].amount | sub("\\."; "") | tonumber] | add // 0) == ($c.buckets[$i].total.amount | sub("\\."; "") | tonumber)] | all')" true
  expect "the CAD items" "$(field '[.currencies[] | select(.currency == "CAD") | .items[] | .liquidity_item] | join(" ")')" "OPERATING OP_SUPPLIERS REC_CUSTOMERS"
  as ada
  say "Ada proposes an item for professional services under Operating, and a rule taking account 6100 to it. A"
  say "derivation rule is tier 2 and needs a simulation: the open flows whose items the rule moves."
  api 200 '"active configuration version \(.items[0].number)"' listConfigVersions -p is_active=true
  base=$(field '.items[0].number')
  jq "{title: \"Liquidity item for professional services\", rationale: \"Treasury reports services apart (T53e3)\", base_version: $base,
    proposed: (.items[0].document | .cash.liquidity_items += [{\"code\": \"OP_SERVICES\", \"name\": \"Professional services\", \"direction\": \"outflow\", \"parent\": \"OPERATING\"}]
      | .cash.derivation.accounts = [{\"from\": \"6100\", \"to\": \"6100\", \"item\": \"OP_SERVICES\"}])}" "$WORK/last.json" >"$WORK/items.json"
  api 201 '"change request \(.number), \(.status): \(.title)"' createChangeRequest -body @items.json
  cr=$(field .id)
  api 200 '"\(.status), tier \(.tier): \([.items[].path] | join(", "))"' submitChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "the change's tier" "$(field .tier)" 2
  expect "status after submit" "$(field .status)" awaiting_evidence
  api 202 '"\(.kind) \(.status)"' simulateChangeRequest -p "id=$cr" -if-match "$ETAG"
  wait_op
  api 200 '.evidence[] | "evidence \(.kind): \(if .passed then "passed" else "FAILED" end), \(.summary)",
    (.detail.liquidity_items_by_company[]? | select(.moved > 0) | "  company \(.company): \(.moved) moved",
      (.flows[] | "    \(.value_date) \(.source) \(.certainty) \(.amount.amount) \(.amount.currency): \([.base[] | "\(.liquidity_item) \(.amount.amount)"] | join(", ")) -> \([.proposed[] | "\(.liquidity_item) \(.amount.amount)"] | join(", "))"),
      (.totals[] | "    \(.base.currency) \(.liquidity_item): \(.base.amount) -> \(.proposed.amount)"))' getChangeRequest -p "id=$cr"
  expect "status after the evidence" "$(field .status)" pending_approval
  expect "the parked invoice's flow moves to OP_SERVICES" \
    "$(field '[.evidence[].detail.liquidity_items_by_company[]? | select(.company == "'"$CC"'") | .flows[] | select(.source == "invoice_unposted")
      | any(.proposed[]; .liquidity_item == "OP_SERVICES")] | any')" true
  expect "no document or supplier in a moved flow" \
    "$(field '[.evidence[].detail.liquidity_items_by_company[]?.flows[] | keys | join(",")] | unique | join(" ")')" "amount,base,certainty,proposed,source,value_date"
  request=$(field .workflow_request_id)
  approve priya "$request" "Services reported apart, as treasury asked"
  as ada
  QUIET=1 api 200 '' getChangeRequest -p "id=$cr"
  api 200 '"\(.status): configuration version \(.activated_version)"' activateChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "status after activation" "$(field .status)" active
  as tom
  say "The forecast now gives the services their own row under Operating, which adds up its children:"
  api 200 '.currencies[] | select(.currency == "CAD") | (.items[] | ("  " * .level) as $in | "  CAD \($in)\(.liquidity_item) \(.name): total \(.total.amount)",
    (.flows[] | "  CAD \($in)    \(.source) \(.certainty): \(.total.amount)"))' getLiquidityForecast -p "legal_entity_id=$LE"
  expect "the CAD items" "$(field '[.currencies[] | select(.currency == "CAD") | .items[] | "\(.level) \(.liquidity_item)"] | join(", ")')" \
    "0 OPERATING, 1 OP_SERVICES, 1 OP_SUPPLIERS, 1 REC_CUSTOMERS"
  expect "the services' row, the parked invoice at what a run would pay" \
    "$(field '.currencies[] | select(.currency == "CAD") | .items[] | select(.liquidity_item == "OP_SERVICES") | [.flows[] | "\(.source) \(.certainty) \(.total.amount | startswith("-"))"] | join(", ")')" \
    "invoice_unposted planned true"
  expect "Operating, its children added up" \
    "$(field '.currencies[] | select(.currency == "CAD") | .items as $r | ($r[] | select(.level == 0) | .total.amount | sub("\\."; "") | tonumber) == ([$r[] | select(.level == 1) | .total.amount | sub("\\."; "") | tonumber] | add)')" true
  as alex
  say "Alex cancels the invoice again:"
  api 200 '"\(.document_number): \(.status)"' cancelSupplierInvoice -p "id=$invoice" -if-match "$invoice_etag" -body '{"reason": "Entered for the liquidity item walkthrough"}'
  passed
}

step_asset_acquisitions() {
  heading "Step 22 — Asset acquisitions: an invoice item on the asset clearing account split onto two machines; the clearing account at zero"
  as alex
  say "Tyrell invoices two machines, CAD 12,000.00 plus 600.00 GST on V1. Alex posts the item on the asset"
  say "clearing account 1690 (ADR-0057 §6): the asset accountant settles it onto the assets, so payables stays as it is."
  local item invoice entry mill lathe first second l
  item="{\"account_id\": \"$(account 1690)\", \"description\": \"CNC milling machine and CNC lathe\", \"quantity\": {\"value\": \"2\", \"unit\": \"EA\"}, \"amount\": {\"amount\": \"12000.00\", \"currency\": \"CAD\"}, \"tax_code\": \"V1\"}"
  api 201 '"\(.kind) \(.document_number) \(.supplier_reference): \(.status), \(.gross_amount.amount) \(.gross_amount.currency)"' createSupplierInvoice \
    -body "{\"legal_entity_id\": \"$LE\", \"supplier_id\": \"$SUPPLIER\", \"supplier_reference\": \"LATHE-2\", \"invoice_date\": \"$TODAY\", \"gross_amount\": {\"amount\": \"12600.00\", \"currency\": \"CAD\"}, \"tax_amount\": {\"amount\": \"600.00\", \"currency\": \"CAD\"}, \"items\": [$item]}"
  invoice=$(field .id)
  api 200 '"\(.document_number): \(.status), journal entry \(.journal_entry_id)"' postSupplierInvoice -p "id=$invoice" -if-match '"1"' -body "{\"posting_date\": \"$TODAY\"}"
  entry=$(field .journal_entry_id)
  as carla
  say "Carla's worklist shows the line on the clearing account with all of it left, and the invoice it came from:"
  api 200 '.items[] | "  entry \(.document_number) line \(.line_no), \(.supplier_invoice.supplier_name // .source_context) \(.supplier_invoice.supplier_reference // ""): \(.amount.amount) \(.amount.currency), left \(.remaining.amount) (local \(.remaining_lc.amount) \(.remaining_lc.currency), group \(.remaining_gc.amount) \(.remaining_gc.currency))"' \
    listAssetClearingItems -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR" -p remaining=true
  expect "the invoice's line on the worklist" "$(field "[.items[] | select(.entry_id == \"$entry\") | \"\(.supplier_invoice.supplier_reference) \(.remaining.amount)\"] | join(\" \")")" \
    "LATHE-2 12000.00"
  l=$(field "[.items[] | select(.entry_id == \"$entry\") | .line_no][0]")
  say "One is the milling machine of step 20; she creates the lathe and splits the line: 7,000.00 onto the milling"
  say "machine, the rest onto the lathe, which takes the rest of the line's local and group amounts too."
  QUIET=1 api 200 '' listAssets -p "legal_entity_id=$LE" -p status=planned
  mill=$(field '[.items[] | select(.description == "CNC milling machine") | .id][0]')
  QUIET=1 api 200 '' listCostCenters -p "legal_entity_id=$LE" -p code=CC-1100
  api 201 '"asset \(.company_code)/\(.asset_number) \(.description): \(.status)"' createAsset \
    -body "{\"legal_entity_id\": \"$LE\", \"class_code\": \"MACHINES\", \"description\": \"CNC lathe\", \"cost_center_id\": \"$(field '.items[0].id')\"}"
  lathe=$(field .id)
  api 201 '"acquisition onto asset \(.asset_number): \(.amount.amount) \(.amount.currency), local \(.amount_lc.amount) \(.amount_lc.currency), group \(.amount_gc.amount) \(.amount_gc.currency); entries \([.entries[] | "\(.ledger_code) \(.document_number)"] | join(", "))"' \
    createAssetAcquisition -body "{\"asset_id\": \"$mill\", \"source\": {\"entry_id\": \"$entry\", \"line_no\": $l}, \"amount\": {\"amount\": \"7000.00\", \"currency\": \"CAD\"}}"
  first=$(field .id)
  expect "the first acquisition's source" "$(field '"\(.source.supplier_invoice.supplier_reference) \(.source.source_context)"')" "LATHE-2 payables"
  api 201 '"acquisition onto asset \(.asset_number): \(.amount.amount) \(.amount.currency), local \(.amount_lc.amount) \(.amount_lc.currency), group \(.amount_gc.amount) \(.amount_gc.currency); entries \([.entries[] | "\(.ledger_code) \(.document_number)"] | join(", "))"' \
    createAssetAcquisition -body "{\"asset_id\": \"$lathe\", \"source\": {\"entry_id\": \"$entry\", \"line_no\": $l}}"
  second=$(field .id)
  expect "the rest of the line" "$(field .amount.amount)" 5000.00
  expect "an entry in every ledger the company keeps" "$(field '[.entries[].ledger_code] | join(" ")')" "0L 2L"
  say "Both machines are active now, capitalised on the invoice's posting date, each with its cost in every ledger:"
  for asset in "$mill" "$lathe"; do
    api 200 '"  asset \(.asset_number) \(.description): \(.status), capitalised on \(.capitalised_on); \([.values[] | "\(.ledger_code) cost \(.cost.amount) \(.cost.currency) (group \(.cost_gc.amount) \(.cost_gc.currency))"] | join(", "))"' getAsset -p "id=$asset"
    expect "the asset's status" "$(field .status)" active
  done
  expect "the lathe's cost per ledger" "$(field '[.values[] | "\(.ledger_code) \(.cost.amount)"] | join(", ")')" "0L 5000.00, 2L 5000.00"
  api 200 '.items[] | "  \(.asset_number): \(.amount.amount) \(.amount.currency) from entry \(.source.document_number) line \(.source.line_no)"' \
    listAssetAcquisitions -p "legal_entity_id=$LE" -p "source_entry_id=$entry"
  expect "the line's acquisitions" "$(field '[.items[].id] | sort | join(" ")')" "$(printf '%s\n' "$first" "$second" | sort | paste -sd' ' -)"
  say "Nothing is left of the line, and the clearing account is at zero in every ledger and in all three currencies:"
  api 200 '"lines with something left: \([.items[] | select(.entry_id == "'"$entry"'")] | length)"' \
    listAssetClearingItems -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR" -p remaining=true
  expect "the line left on the worklist" "$(field "[.items[] | select(.entry_id == \"$entry\")] | length")" 0
  QUIET=1 api 200 '' listLedgers
  for l in $(field '.items[] | "\(.code)=\(.id)"'); do
    api 200 '"  '"${l%%=*}"' clearing 1690: \([.items[] | (.debit.amount | tonumber) - (.credit.amount | tonumber)] | add // 0) in the transaction currency, \([.items[] | (.debit_lc.amount | tonumber) - (.credit_lc.amount | tonumber)] | add // 0) local, \([.items[] | (.debit_gc.amount | tonumber) - (.credit_gc.amount | tonumber)] | add // 0) group"' \
      listBalances -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR" -p "ledger_id=${l#*=}" -p "account_id=$(account 1690)" -p page_size=500
    expect "the clearing account in ledger ${l%%=*}" "$(field '[.items[] | (.debit.amount | tonumber) - (.credit.amount | tonumber), (.debit_lc.amount | tonumber) - (.credit_lc.amount | tonumber), (.debit_gc.amount | tonumber) - (.credit_gc.amount | tonumber)] | map(. * 100 | round) | unique | join(" ")')" 0
  done
  passed
}

# step_cash_forecast is the liquidity forecast's opening and closing, its
# memo records and payments in transit, one grain over a range, its
# conversion and its drill-down (T53e4, ADR-0051 §7, §9, §10).
step_cash_forecast() {
  heading "Step 23 — The liquidity forecast opened, converted and drilled down to its documents"
  as tom
  local next to memo memo_etag cells read
  next=$(date_add "$TODAY" 40)
  say "Tom expects a US customer's USD 2,000.00 on $next and records it without a house bank: a memo record the"
  say "forecast counts and the cash position does not (T53e4)."
  api 201 '"memo record \(.reference): \(.amount.amount) \(.amount.currency) on \(.value_date), \(.house_bank_code // "no house bank"), \(.liquidity_item)"' \
    createMemoRecord -body "{\"legal_entity_id\": \"$LE\", \"amount\": {\"amount\": \"2000.00\", \"currency\": \"USD\"}, \"value_date\": \"$next\", \"liquidity_item\": \"REC_CUSTOMERS\", \"description\": \"Acme Corp, order 77\", \"reference\": \"WT-2001\"}"
  memo=$(field .id)
  memo_etag=$ETAG
  say "The forecast opens at the cash position's first day (each currency's house banks' statement balances), closes each"
  say "bucket at the opening plus the totals so far, counts the memo records (with a bank or none) and the payments in"
  say "transit, and, asked for a display currency, converts each currency at Maple's planning rate (else the average):"
  api 200 '.currencies[] | "  \(.currency): opens at \(.opening.amount.amount) (\(.opening.banks) banks\(if .opening.stale then ", stale" else "" end)), closes at \(.buckets[-1].closing.amount) after \(.total.amount)",
      "    memo records \([.buckets[].flows[] | select(.source == "memo") | .amount.amount] | join(", ")); in transit \([.buckets[].flows[] | select(.source == "in_transit") | .amount.amount] | join(", "))"' \
    getLiquidityForecast -p "legal_entity_id=$LE" -p display_currency=CAD
  excerpt '.converted | "  in \(.currency): opens at \(.opening.amount), closes at \(.buckets[-1].closing.amount); rates \([.rates[] | "\(.currency) \(.rate_type) \(.rate)\(if .stale then " stale" else "" end)"] | join(", ")); without a rate: \(.incomplete_currencies | join(", "))"'
  expect "each bucket closes at the opening plus the totals so far" \
    "$(field 'def c: sub("\\."; "") | tonumber; [.currencies[] | (.opening.amount.amount | c) as $o | [foreach .buckets[] as $b ($o; . + ($b.total.amount | c); .)] == [.buckets[].closing.amount | c]] | all')" true
  expect "WT-1042 (step 13, at RBC) and WT-2001 (no bank), memo flows" \
    "$(field '[.currencies[] | .currency as $c | .buckets[].flows[] | select(.source == "memo") | "\($c) \(.amount.amount)"] | map(select(. == "CAD 25000.00" or . == "USD 2000.00")) | unique | join(", ")')" \
    "CAD 25000.00, USD 2000.00"
  expect "the converted closing, the converted opening plus the converted totals" \
    "$(field 'def c: sub("\\."; "") | tonumber; .converted | ((.opening.amount | c) + ([.buckets[].total.amount | c] | add)) == (.buckets[-1].closing.amount | c)')" true
  expect "USD converted or listed without a rate, never as zero" \
    "$(field '.converted | ([.rates[].currency] + .incomplete_currencies) | index("USD") != null')" true
  cells=$(field '[.currencies[] | select(.currency == "CAD") | .items[] | select(.liquidity_item == "REC_CUSTOMERS") | .buckets[0:14][].amount | sub("\\."; "") | tonumber] | add // 0')
  # The forecast's buckets start on its read date, the server's today:
  # the ranges below are counted from it, not from TODAY, which a run
  # that crossed Toronto's midnight left a day behind (ci:batch56).
  read=$(field .read_date)
  to=$(date_add "$read" 55)
  say "One grain over a range instead of Maple's buckets: weeks up to $to."
  api 200 '"\(.buckets | length) \(.buckets[0].grain)s from \(.buckets[0].start) to \(.buckets[-1].end)"' getLiquidityForecast -p "legal_entity_id=$LE" -p grain=week -p "to=$to"
  expect "the weeks up to the one holding $to" "$(field '.buckets | length')" 8
  say "Tom drills the first two weeks of Customer receipts down to their flows and the documents behind them, each"
  say "to read in its own context (getMemoRecord here), and the drill-down totals the row's cells:"
  api 200 '"  \(.count) flows, total \(.total.amount) \(.total.currency)", (.items[] | "  \(.date) \(.source) \(.certainty) \(.share.amount): \([.documents[] | "\(.kind) \(.number // "-") \(.amount.amount)"] | join(", ")) (\(.document_count))")' \
    listLiquidityForecastFlows -p "legal_entity_id=$LE" -p currency=CAD -p "to=$(date_add "$read" 13)" -p liquidity_item=REC_CUSTOMERS
  expect "the drill-down's total, the row's cells of the two weeks" "$(field '.total.amount | sub("\\."; "") | tonumber')" "$cells"
  expect "WT-1042 named by its reference" "$(field '[.items[].documents[] | select(.kind == "memo_record") | .number] | index("WT-1042") != null')" true
  as ada
  say "Ada gives Rita in reporting the forecast alone (cash.forecast.read): she sees the flows, never the documents or"
  say "suppliers, which need their own context's read permission; they are left out, not refused."
  api 201 '"role \(.code), \(.name)"' createRole -body '{"code": "cash_reporting", "name": "Cash reporting"}'
  grant "$(field .id)" cash.forecast.read
  QUIET=1 api 200 '' listRoles -p page_size=100
  cp "$WORK/last.json" "$WORK/roles.json"
  person rita "Rita Reporting" cash_reporting
  as rita
  api 200 '"  \(.count) flows, total \(.total.amount) \(.total.currency)", (.items[] | "  \(.date) \(.source) \(.share.amount): \(.documents | length) of \(.document_count) documents shown, supplier \(.supplier.name // "not shown")")' \
    listLiquidityForecastFlows -p "legal_entity_id=$LE" -p currency=CAD -p "to=$(date_add "$read" 13)" -p liquidity_item=REC_CUSTOMERS
  expect "Rita's total, Tom's" "$(field '.total.amount | sub("\\."; "") | tonumber')" "$cells"
  expect "documents shown to Rita" "$(field '[.items[].documents[]] | length')" 0
  expect "documents counted for Rita" "$(field '[.items[].document_count] | add > 0')" true
  as tom
  say "Tom cancels WT-2001 again:"
  api 200 '"memo record \(.reference): \(.status)"' cancelMemoRecord -p "id=$memo" -if-match "$memo_etag"
  passed
}

step_asset_reversal() {
  heading "Step 24 — Asset corrections: a press contributed against share capital, one acquisition reversed; a duplicate invoice's line taken off the press"
  as carla
  say "Maple's shareholder contributes a hydraulic press in kind. No supplier invoice brings it, so Carla capitalises it by"
  say "manual entry against share capital 3000 (ADR-0057 §6, T57d): Dr machinery 1600 / Cr 3000 in every ledger the"
  say "company keeps, the press's cost the ledger's own amounts."
  local cc press manual press_cost item invoice entry l rev rl yesterday values
  accounts
  QUIET=1 api 200 '' listCostCenters -p "legal_entity_id=$LE" -p code=CC-1100
  cc=$(field '.items[0].id')
  api 201 '"asset \(.company_code)/\(.asset_number) \(.description): \(.status)"' createAsset \
    -body "{\"legal_entity_id\": \"$LE\", \"class_code\": \"MACHINES\", \"description\": \"Hydraulic press\", \"cost_center_id\": \"$cc\"}"
  press=$(field .id)
  say "The clearing account is no offset: its lines are the worklist's, settled by acquisitions from them."
  refused 422 assets.offset_account_invalid createAssetAcquisition \
    -body "{\"asset_id\": \"$press\", \"offset_account_id\": \"$(account 1690)\", \"amount\": {\"amount\": \"8500.00\", \"currency\": \"CAD\"}}"
  api 201 '"acquisition onto asset \(.asset_number) against \(.offset_account.number) \(.offset_account.name): \(.amount.amount) \(.amount.currency), group \(.amount_gc.amount) \(.amount_gc.currency); entries \([.entries[] | "\(.ledger_code) \(.document_number)"] | join(", "))"' \
    createAssetAcquisition -body "{\"asset_id\": \"$press\", \"offset_account_id\": \"$(account 3000)\", \"amount\": {\"amount\": \"8500.00\", \"currency\": \"CAD\"}, \"text\": \"Press contributed in kind\"}"
  manual=$(field .id)
  expect "the offset account" "$(field '"\(.offset_account.number) \(.source // "no source")"')" "3000 no source"
  values='"  asset \(.asset_number) \(.description): \(.status), capitalised on \(.capitalised_on); \([.values[] | "\(.ledger_code) cost \(.cost.amount) \(.cost.currency)"] | join(", "))"'
  api 200 "$values" getAsset -p "id=$press"
  expect "the press's status" "$(field .status)" active
  say "The valuation said 7,500.00, not 8,500.00. Carla reverses the acquisition; a reversal is never dated before what"
  say "it reverses:"
  yesterday=$(date_add "$TODAY" -1)
  QUIET=1 api 200 '' getAssetAcquisition -p "id=$manual"
  refused 422 assets.reversal_before_original reverseAssetAcquisition -p "id=$manual" -if-match "$ETAG" \
    -body "{\"reason\": \"Valued at 8,500.00 in error\", \"posting_date\": \"$yesterday\"}"
  QUIET=1 api 200 '' getAssetAcquisition -p "id=$manual"
  api 200 '"acquisition \(.status) on \(.reversal.posting_date): \(.reversal.reason); \([.entries[] | "\(.ledger_code) \(.document_number) reversed by \(.reversal_document_number)"] | join(", "))"' \
    reverseAssetAcquisition -p "id=$manual" -if-match "$ETAG" -body "{\"reason\": \"Valued at 8,500.00 in error\"}"
  expect "the acquisition's status" "$(field .status)" reversed
  expect "a reversal entry in every ledger" "$(field '[.entries[] | select(.reversal_journal_entry_id != null)] | length')" "$(field '.entries | length')"
  say "With its only acquisition reversed the press is planned again, at no cost:"
  api 200 "$values" getAsset -p "id=$press"
  expect "the press, planned again" "$(field '"\(.status) \(.capitalised_on) \([.values[].cost.amount] | unique | join(" "))"')" "planned null 0.00"
  api 201 '"acquisition onto asset \(.asset_number) against \(.offset_account.number): \(.amount.amount) \(.amount.currency)"' createAssetAcquisition \
    -body "{\"asset_id\": \"$press\", \"offset_account_id\": \"$(account 3000)\", \"amount\": {\"amount\": \"7500.00\", \"currency\": \"CAD\"}, \"text\": \"Press contributed in kind\"}"

  as alex
  say "Tyrell invoices the press's tooling, CAD 1,200.00 plus GST, on the asset clearing account; Carla capitalises it onto"
  say "the press. The invoice turns out to duplicate one already settled: Alex reverses it in payables, which is not told"
  say "about the asset."
  item="{\"account_id\": \"$(account 1690)\", \"description\": \"Press tooling\", \"quantity\": {\"value\": \"1\", \"unit\": \"EA\"}, \"amount\": {\"amount\": \"1200.00\", \"currency\": \"CAD\"}, \"tax_code\": \"V1\"}"
  api 201 '"\(.kind) \(.document_number) \(.supplier_reference): \(.status), \(.gross_amount.amount) \(.gross_amount.currency)"' createSupplierInvoice \
    -body "{\"legal_entity_id\": \"$LE\", \"supplier_id\": \"$SUPPLIER\", \"supplier_reference\": \"TOOLS-7\", \"invoice_date\": \"$TODAY\", \"gross_amount\": {\"amount\": \"1260.00\", \"currency\": \"CAD\"}, \"tax_amount\": {\"amount\": \"60.00\", \"currency\": \"CAD\"}, \"items\": [$item]}"
  invoice=$(field .id)
  api 200 '"\(.document_number): \(.status), journal entry \(.journal_entry_id)"' postSupplierInvoice -p "id=$invoice" -if-match '"1"' -body "{\"posting_date\": \"$TODAY\"}"
  entry=$(field .journal_entry_id)
  as carla
  QUIET=1 api 200 '' listAssetClearingItems -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR" -p remaining=true
  l=$(field "[.items[] | select(.entry_id == \"$entry\") | .line_no][0]")
  api 201 '"acquisition onto asset \(.asset_number): \(.amount.amount) \(.amount.currency) from entry \(.source.document_number) line \(.source.line_no)"' \
    createAssetAcquisition -body "{\"asset_id\": \"$press\", \"source\": {\"entry_id\": \"$entry\", \"line_no\": $l}}"
  press_cost=$(field .id)
  as alex
  QUIET=1 api 200 '' getSupplierInvoice -p "id=$invoice"
  api 200 '"\(.document_number): \(.status), reversal entry \(.reversal_entry_id)"' reverseSupplierInvoice -p "id=$invoice" -if-match "$ETAG" \
    -body "{\"reason\": \"Posted twice\", \"posting_date\": \"$TODAY\"}"
  as carla
  say "Carla's worklist shows the invoice's line reversed, with its reversal's line beside it offering the press what it"
  say "holds from the line:"
  api 200 '.items[] | select(.entry_id == "'"$entry"'") | "  entry \(.document_number) line \(.line_no): reversed \(.source_reversed); reversal entry \(.reversal.document_number) line \(.reversal.line_no) offers \([.reversal.offers[] | "\(.asset_number) \(.asset_description) \(.amount.amount) \(.amount.currency)"] | join(", "))"' \
    listAssetClearingItems -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR" -p remaining=true
  rev=$(field ".items[] | select(.entry_id == \"$entry\") | .reversal.entry_id")
  rl=$(field ".items[] | select(.entry_id == \"$entry\") | .reversal.line_no")
  expect "the press's offer" "$(field ".items[] | select(.entry_id == \"$entry\") | [.reversal.offers[] | .amount.amount] | join(\" \")")" "-1200.00"
  say "She takes the cost off the press through the reversal's line, at most what the press holds from the invoice:"
  refused 422 assets.amount_exceeds_line createAssetAcquisition \
    -body "{\"asset_id\": \"$press\", \"source\": {\"entry_id\": \"$rev\", \"line_no\": $rl}, \"amount\": {\"amount\": \"-1500.00\", \"currency\": \"CAD\"}}"
  api 201 '"acquisition onto asset \(.asset_number): \(.amount.amount) \(.amount.currency), local \(.amount_lc.amount), group \(.amount_gc.amount) \(.amount_gc.currency), from reversal entry \(.source.document_number)"' \
    createAssetAcquisition -body "{\"asset_id\": \"$press\", \"source\": {\"entry_id\": \"$rev\", \"line_no\": $rl}}"
  expect "the take-off" "$(field .amount.amount)" "-1200.00"
  say "While the take-off stands, the tooling's own acquisition is not reversed: the take-off is bounded by it."
  QUIET=1 api 200 '' getAssetAcquisition -p "id=$press_cost"
  refused 409 assets.reversal_line_capitalised reverseAssetAcquisition -p "id=$press_cost" -if-match "$ETAG" -body '{"reason": "Duplicate invoice"}'
  api 200 "$values" getAsset -p "id=$press"
  expect "the press's cost in every ledger" "$(field '[.values[].cost.amount] | unique | join(" ")')" "7500.00"
  api 200 '"the reversed line on the worklist with something left: \([.items[] | select(.entry_id == "'"$entry"'")] | length)"' \
    listAssetClearingItems -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR" -p remaining=true
  expect "the reversed line, nothing left to offer" "$(field "[.items[] | select(.entry_id == \"$entry\")] | length")" 0
  QUIET=1 api 200 '' listAssetAcquisitions -p "asset_id=$press" -p status=reversed
  expect "the press's reversed acquisitions" "$(field '[.items[].id] | join(" ")')" "$manual"
  QUIET=1 api 200 '' listLedgers
  for l in $(field '.items[] | "\(.code)=\(.id)"'); do
    QUIET=1 api 200 '' listBalances -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR" -p "ledger_id=${l#*=}" -p "account_id=$(account 1690)" -p page_size=500
    expect "the clearing account in ledger ${l%%=*}" "$(field '[.items[] | (.debit.amount | tonumber) - (.credit.amount | tonumber), (.debit_lc.amount | tonumber) - (.credit_lc.amount | tonumber), (.debit_gc.amount | tonumber) - (.credit_gc.amount | tonumber)] | map(. * 100 | round) | unique | join(" ")')" 0
  done
  say "  the clearing account is at zero in every ledger and all three currencies"
  passed
}

step_asset_depreciation() {
  heading "Step 25 — Depreciation schedule and preview: the lathe's first period in each ledger, before any run posts"
  as carla
  say "Carla checks what depreciation will post before the period's run (ADR-0057 §7, T57e). The lathe of step 22 was"
  say "capitalised today at 5,000.00, so its depreciation starts in this period: straight-line over 120 months in the"
  say "leading ledger, 5,000.00 / 120 = 41.67, and declining balance at twice the rate in 2L, 5,000.00 x 2 / 120 = 83.33."
  say "Nothing is stored as a plan: the schedule is computed from the asset's terms and movements at every read."
  local lathe period l code want
  QUIET=1 api 200 '' listAssets -p "legal_entity_id=$LE" -p status=active
  lathe=$(field '[.items[] | select(.description == "CNC lathe") | .id][0]')
  api 200 '.items[] | "  \(.seq) \(.kind) \(.ledger_code) \(.posting_date): cost \(.cost.amount) \(.cost.currency) (group \(.cost_gc.amount) \(.cost_gc.currency)), entry \(.document_number)"' \
    listAssetMovements -p "id=$lathe"
  expect "the lathe's movements" "$(field '[.items[] | "\(.kind) \(.ledger_code) \(.cost.amount)"] | join(", ")')" "acquisition 0L 5000.00, acquisition 2L 5000.00"
  api 200 '"asset \(.asset_number) \(.description) in \(.ledger_code), \(.method) over \(.useful_life_months) months from \(.depreciation_start):",
    (.periods[] | select(.planned.amount != "0.00" or .net_book_value.amount != "0.00") | "  \(.fiscal_year)/\(.period): planned \(.planned.amount), booked \(.booked.amount), net book value \(.net_book_value.amount) \(.net_book_value.currency)"),
    "  the year: planned \(.totals.planned.amount), booked \(.totals.booked.amount)"' \
    getAssetSchedule -p "id=$lathe" -p "fiscal_year=$YEAR"
  period=$(field ".periods[] | select(.start_date <= \"$TODAY\" and .end_date >= \"$TODAY\") | .period")
  expect "the lathe's first period" "$(field ".periods[] | select(.period == $period) | .planned.amount")" 41.67
  QUIET=1 api 200 '' getAsset -p "id=$lathe"
  for l in $(field '.terms[] | "\(.ledger_code)=\(.ledger_id)"'); do
    code=${l%%=*}
    want=83.33
    if [ "$code" = 0L ]; then want=41.67; fi
    say "The run of period $period in $code would post, per asset, the planned depreciation through the period less what is"
    say "booked, on the class's accounts and the asset's centers:"
    api 200 '"  \(.company_code) \(.ledger_code) \(.fiscal_year)/\(.period) (\(.period_start)..\(.period_end)), \(.totals.assets) assets:",
      (.items[] | "  asset \(.asset_number) \(.description): planned \(.planned.amount), booked \(.booked.amount), difference \(.difference.amount) \(.difference.currency) on \(.depreciation_account.number), \(.cost_center_code // "no cost center")"),
      (.totals.lines[] | "  line \(.account_number) \(.account_name) \(.cost_center_code // "") \(.profit_center_code // ""): \(.amount.amount) \(.amount.currency)")' \
      previewDepreciationRun -body "{\"legal_entity_id\": \"$LE\", \"ledger_id\": \"${l#*=}\", \"fiscal_year\": $YEAR, \"period\": $period}"
    expect "the lathe's depreciation in $code" "$(field "[.items[] | select(.asset_id == \"$lathe\") | .difference.amount][0]")" "$want"
    expect "the totals in $code balance" "$(field '[.totals.lines[].amount.amount | tonumber] | add * 100 | round')" 0
  done
  say "A special period takes no depreciation:"
  refused 422 assets.special_period previewDepreciationRun -body "{\"legal_entity_id\": \"$LE\", \"fiscal_year\": $YEAR, \"period\": 13}"
  passed
}

# CONSOLIDATION_SETTINGS give the tenant group reporting (ADR-0059 §3,
# T68a): the group chart, one account per type for what no elimination
# holds, the intercompany accounts of the two eliminations, investments,
# the four role accounts a run computes and the account differences go
# to; company @CC@'s intercompany receivables and revenue and its
# investment in Acme Corp US; Acme Corp US (1000, in USD, the legal entity
# of step 36) in the configuration, its chart as SAP's company 1000 has it
# with intercompany payables and expenses, in the leading ledger alone;
# every account of both companies mapped to the group account of its type,
# but the intercompany accounts and the investment, which have their own;
# Acme Corp US's number ranges as Maple's this year, and its journal
# entries' in the previous fiscal year too, whose periods it then has
# (ADR-0059 §Steps); and the group MAPLE, Maple holding the whole of Acme
# Corp US, which it incorporated (the pilot's structure, D11; ADR-0059 As
# built T68h), from period @FIRST@, Maple's intercompany receivables from
# it designated as part of Maple's net investment in it (po:2026-11-10;
# IAS 21.32). Maple's intercompany receivables are kept in
# foreign currency and revalued (ADR-0042) into its unrealised gain and
# loss, as its USD bank accounts are. @CC@, @YEAR@ and @FIRST@ are
# replaced.
CONSOLIDATION_SETTINGS='(.consolidation = {
  "accounts": [
    {"number": "G1000", "name": "Other assets", "type": "asset"},
    {"number": "G1250", "name": "Intercompany receivables", "type": "asset"},
    {"number": "G1700", "name": "Investments in subsidiaries", "type": "asset"},
    {"number": "G1900", "name": "Goodwill", "type": "asset", "role": "goodwill"},
    {"number": "G2000", "name": "Liabilities", "type": "liability"},
    {"number": "G2250", "name": "Intercompany payables", "type": "liability"},
    {"number": "G3000", "name": "Equity", "type": "equity"},
    {"number": "G3800", "name": "Retained earnings", "type": "equity", "role": "retained_earnings"},
    {"number": "G3900", "name": "Currency translation reserve", "type": "equity", "role": "cta"},
    {"number": "G3950", "name": "Non-controlling interests", "type": "equity", "role": "nci"},
    {"number": "G4000", "name": "Revenue", "type": "revenue"},
    {"number": "G4250", "name": "Intercompany revenue", "type": "revenue"},
    {"number": "G5000", "name": "Expenses", "type": "expense"},
    {"number": "G5250", "name": "Intercompany expenses", "type": "expense"},
    {"number": "G6950", "name": "Intercompany differences", "type": "expense"}],
  "eliminations": [
    {"code": "IC_BALANCES", "name": "Intercompany receivables and payables", "accounts": ["G1250", "G2250"], "difference_account": "G6950", "tolerance": "10.00"},
    {"code": "IC_RESULTS", "name": "Intercompany revenue and expenses", "accounts": ["G4250", "G5250"], "difference_account": "G6950", "tolerance": "1.00"}],
  "groups": [{"code": "MAPLE", "name": "Maple group", "parent": "@CC@",
    "members": [{"company": "1000", "ownership_percent": "100", "origin": "incorporated", "first_period": "@FIRST@", "investment_account": "1700"}],
    "net_investments": [{"company": "@CC@", "account": "1250", "partner": "1000"}]}]})
| (.companies[] | select(.code == "@CC@") | .chart_of_accounts) += [
    {"number": "1250", "name": "Intercompany receivables", "type": "asset", "revalue_foreign_currency": true},
    {"number": "1700", "name": "Investment in Acme Corp US", "type": "asset"},
    {"number": "4250", "name": "Intercompany revenue", "type": "revenue"}]
| (.companies[] | select(.code == "@CC@") | .fx_valuation.accounts) += [{"account": "1250", "unrealised_gain": "4900", "unrealised_loss": "6900"}]
| .companies += [{"code": "1000", "name": "Acme Corp US", "currency": "USD", "country": "US", "fiscal_year_start_month": 1,
    "time_zone": "America/New_York",
    "chart_of_accounts": [
      {"number": "100000", "name": "Bank - Operating", "type": "asset", "revalue_foreign_currency": true},
      {"number": "140000", "name": "Input Tax", "type": "asset"},
      {"number": "211000", "name": "Accounts Payable - Domestic", "type": "liability", "vendor_reconciliation": true},
      {"number": "225000", "name": "Intercompany payables", "type": "liability", "vendor_reconciliation": true},
      {"number": "300000", "name": "Share Capital", "type": "equity"},
      {"number": "400000", "name": "Sales Revenue", "type": "revenue"},
      {"number": "480000", "name": "Purchase discounts received", "type": "revenue"},
      {"number": "490000", "name": "Unrealised exchange gains", "type": "revenue"},
      {"number": "510000", "name": "Raw Materials Consumed", "type": "expense"},
      {"number": "520000", "name": "Office Supplies", "type": "expense"},
      {"number": "525000", "name": "Intercompany services", "type": "expense"},
      {"number": "530000", "name": "IT Services", "type": "expense"},
      {"number": "690000", "name": "Unrealised exchange losses", "type": "expense"}],
    "fx_valuation": {"rate_type": "bank_selling", "accounts": [{"account": "100000", "unrealised_gain": "490000", "unrealised_loss": "690000"}]},
    "account_determination": {"ap_reconciliation": "211000", "input_tax": "140000", "bank": "100000", "cash_discount": "480000"},
    "tax_codes": [{"code": "V0", "description": "Input tax exempt", "rate_percent": "0", "account": "140000"}],
    "invoice_tolerance": {"price_percent": 2, "price_absolute": "5.00"},
    "auto_post_limit": "50000.00", "manual_entry_approval_limit": "10000.00", "gr_over_delivery_percent": 0,
    "house_bank": {"name": "MAIN", "iban": "GB33BUKB20201555555555", "bic": "BUKBGB22"}}]
| ({"1250": "G1250", "1700": "G1700", "4250": "G4250", "225000": "G2250", "525000": "G5250"}) as $own
| ({"asset": "G1000", "liability": "G2000", "equity": "G3000", "revenue": "G4000", "expense": "G5000"}) as $type
| (.companies[] | select(.code == "@CC@" or .code == "1000") | .chart_of_accounts[]) |= (.group_account = ($own[.number] // $type[.type]))
| .number_ranges += ([.number_ranges[] | select(.company == "@CC@" and .fiscal_year == @YEAR@
      and (.document_type | IN("asset", "journal_aa", "journal_af") | not)) | .company = "1000"]
    + [.number_ranges[] | select(.company == "@CC@" and .fiscal_year == @YEAR@ and (.document_type | startswith("journal_"))
      and (.document_type | IN("journal_aa", "journal_af") | not)) | .company = "1000" | .fiscal_year = @YEAR@ - 1])'

# step_consolidation is group reporting's configuration (T68a, ADR-0059
# §3, §12): Ada proposes the group chart, the mapping of both companies'
# accounts, the eliminations and the group MAPLE, Maple holding the whole
# of Acme Corp US, which it incorporated, from today's period, a tier 4 change with its evidence,
# approved and activated; Carla reads the group with its companies named
# and the group chart; Cory, who may act in Maple alone, sees no group.
step_consolidation() {
  heading "Step 40 — Group reporting: the group chart, the companies' mapping and the group MAPLE through a governed change, trading partners, a consolidation run, its intercompany eliminations, its consolidated statements, an intercompany balance revalued by trading partner and its exchange difference, on a net investment, moved out of the profit, a group-currency revaluation's translation part moved out of the profit and its open difference accepted"
  as ada
  local base settings first cr request
  first="$YEAR-${TODAY:5:2}"
  say "Maple incorporated Acme Corp US, its US subsidiary of step 36, and holds all of it. Ada proposes group reporting"
  say "(ADR-0059 §3): a group chart of accounts, each account of both companies mapped to it, the eliminations of"
  say "intercompany receivables and payables and of intercompany revenue and expenses, each with its tolerance in CAD,"
  say "and the group MAPLE, Maple holding Acme Corp US from period $first. Acme Corp US joins the configuration with its chart, in the leading"
  say "ledger alone, and Maple's chart gains its investment in it. Maple funds Acme Corp US through its intercompany"
  say "receivables and does not expect them settled: the group designates Maple's account 1250 with company 1000 as part"
  say "of its net investment in Acme Corp US (IAS 21.32). The chart, the mapping and the group decide what the"
  say "consolidated statements say: a structural change, tier 4."
  api 200 '"active configuration version \(.items[0].number)"' listConfigVersions -p is_active=true
  base=$(field '.items[0].number')
  settings=${CONSOLIDATION_SETTINGS//@YEAR@/$YEAR}
  settings=${settings//@CC@/$CC}
  settings=${settings//@FIRST@/$first}
  jq "{title: \"Group reporting: the group MAPLE\", rationale: \"Consolidation of Maple and Acme Corp US (T68a)\", base_version: $base,
    proposed: (.items[0].document | $settings)}" "$WORK/last.json" >"$WORK/consolidation.json"
  api 201 '"change request \(.number), \(.status): \(.title)"' createChangeRequest -body @consolidation.json
  cr=$(field .id)
  api 200 '"\(.status), tier \(.tier) (structural): \(.items | length) changes, among them \([.items[] | select(.path | startswith("consolidation")) | "\(.path) (tier \(.tier))"] | join(", "))"' \
    submitChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "the change's tier" "$(field .tier)" 4
  expect "the group among the changes" "$(field '[.items[] | select(.path == "consolidation.groups[MAPLE]") | .tier] | join(" ")')" 4
  expect "an elimination's tier" "$(field '[.items[] | select(.path == "consolidation.eliminations[IC_BALANCES]") | .tier] | join(" ")')" 3
  say "The invoices of the last year are replayed under it, against a regression suite of the active version:"
  api 201 '"regression suite \(.number): \(.case_count) cases"' createRegressionSuite -body '{"name": "Before group reporting", "window_days": 366}'
  QUIET=1 api 200 '' getChangeRequest -p "id=$cr"
  api 202 '"\(.kind) \(.status)"' simulateChangeRequest -p "id=$cr" -if-match "$ETAG"
  wait_op
  api 200 '.evidence[] | "evidence \(.kind): \(if .passed then "passed" else "FAILED" end), \(.summary)"' getChangeRequest -p "id=$cr"
  expect "status after the evidence" "$(field .status)" pending_approval
  request=$(field .workflow_request_id)
  approve sam "$request" "Group chart and mapping checked"
  approve carla "$request" "The group's structure is the shareholder register's"
  as ada
  QUIET=1 api 200 '' getChangeRequest -p "id=$cr"
  api 200 '"\(.status): configuration version \(.activated_version)"' activateChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "status after activation" "$(field .status)" active
  as carla
  say "Carla reads the group as the consolidation context gives it: its companies by code, name and currency, the"
  say "ownership, the period Acme Corp US is first consolidated in and Maple's investment account, by number and name:"
  api 200 '"group \(.code) \(.name), in \(.group_currency): parent \(.parent.company_code) \(.parent.company_name) (\(.parent.local_currency))",
    (.members[] | "  member \(.company_code) \(.company_name) (\(.local_currency)): \(.ownership_percent)%, \(.origin), from \(.first_fiscal_year)-\(.first_fiscal_period), investment \(.investment_account.number) \(.investment_account.name)")' \
    getConsolidationGroup -p code=MAPLE
  expect "the parent" "$(field '"\(.parent.company_code) \(.parent.company_name) \(.parent.legal_entity_id)"')" "$CC Maple Industries Canada $LE"
  expect "the member" "$(field '.members[] | "\(.company_code) \(.company_name) \(.local_currency) \(.ownership_percent) \(.origin) \(.first_fiscal_year)-\(.first_fiscal_period)"')" \
    "1000 Acme Corp US USD 100 incorporated $YEAR-$((10#${TODAY:5:2}))"
  expect "the investment account" "$(field '.members[0].investment_account | "\(.number) \(.name)"')" "1700 Investment in Acme Corp US"
  say "and the monetary items designated as part of a net investment, by company, account and trading partner:"
  api 200 '.net_investments[] | "  \(.company.company_code) \(.company.company_name): \(.account.number) \(.account.name) with \(.partner.company_code) \(.partner.company_name)"' \
    getConsolidationGroup -p code=MAPLE
  expect "the net investment designations" \
    "$(field '[.net_investments[] | "\(.company.company_code) \(.company.legal_entity_id) \(.account.number) \(.account.name) \(.partner.company_code) \(.partner.company_name)"] | join(", ")')" \
    "$CC $LE 1250 Intercompany receivables 1000 Acme Corp US"
  api 200 '"groups: \([.items[].code] | join(", "))"' listConsolidationGroups
  expect "the groups Carla sees" "$(field '[.items[].code] | join(" ")')" MAPLE
  say "The group chart: each account's type, the role a consolidation run gives it, the elimination it belongs to and"
  say "how many company accounts map to it:"
  api 200 '.items[] | "  \(.number) \(.name) (\(.type)\(if .role then ", role \(.role)" else "" end))\(if .elimination then ", elimination \(.elimination.code)" else "" end): \(.mapped_accounts) accounts"' \
    listGroupAccounts
  expect "the role accounts" "$(field '[.items[] | select(.role) | "\(.number)=\(.role)"] | join(" ")')" "G1900=goodwill G3800=retained_earnings G3900=cta G3950=nci"
  expect "a role account takes no company account" "$(field '[.items[] | select(.role) | .mapped_accounts] | add')" 0
  api 200 '.items[] | "  \(.number) \(.name): \(.elimination.name), \(.mapped_accounts) account"' listGroupAccounts -p elimination=IC_BALANCES
  expect "the intercompany balances' accounts" "$(field '[.items[] | "\(.number):\(.mapped_accounts)"] | join(" ")')" "G1250:1 G2250:1"
  as ada
  say "Cory in reporting (step 36) is given the consolidation read and the ledger's entry read; he may act in Maple"
  say "alone. A group's figures are only meaningful whole, so a group with a member he may not act in is unknown to"
  say "him, never shown in part (ADR-0059 §12):"
  grant "$(role cash_reporting)" consolidation.read ledger.entry.read
  as cory
  api 200 '"groups: \(.items | length)"' listConsolidationGroups
  expect "the groups Cory sees" "$(field '.items | length')" 0
  refused 404 consolidation.group_unknown getConsolidationGroup -p code=MAPLE
  consolidation_trading_partner
  consolidation_run
  consolidation_eliminations
  consolidation_statements
  consolidation_revaluation
  consolidation_revaluation_translation
  consolidation_accept
  as tom
  passed
}

# consolidation_trading_partner is the trading partner on journal lines
# (T68b, ADR-0059 §4), part of step_consolidation: Maple's USD invoice to
# its sister Acme Corp US (company 1000), posted by hand on the month's
# first day as an intercompany receivable (1250) against intercompany
# revenue (4250), names company 1000 on both lines; the entry reads back
# with the company's code and name, the journal lists it by trading
# partner, Cory (Maple alone) reads it with the trading partner's id
# alone (product owner 2026-11-05), and a line naming Maple itself is
# refused. Then the trading partner derived from the business partner
# (T68c): Maple's business partner record of Acme Corp US, a supplier of
# Maple's, is linked to company 1000, and its USD invoice, posted through
# payables without naming a trading partner, carries 1000 on every line.
consolidation_trading_partner() {
  local first="${TODAY:0:7}-01" receivable revenue us je body
  say "The trading partner on journal lines (T68b, ADR-0059 §4): an intercompany line names the group company on its"
  say "other side, so that a consolidation run can read each company's balances per trading partner."
  as carla
  accounts
  receivable=$(account 1250)
  revenue=$(account 4250)
  QUIET=1 api 200 '' listLegalEntities -p page_size=100
  us=$(field '.items[] | select(.code == "1000") | .id')
  say "Carla posts Maple's invoice to Acme Corp US, USD 2,000.00 dated $first (booked at 1.35), as a manual entry:"
  say "the intercompany receivable against intercompany revenue, each line naming company 1000 as its trading partner (ADR-0059 §4)."
  body="{\"legal_entity_id\": \"$LE\", \"document_date\": \"$first\", \"posting_date\": \"$first\", \"currency\": \"USD\", \"header_text\": \"Engineering services to Acme Corp US\", \"lines\": [{\"account_id\": \"$receivable\", \"amount\": {\"amount\": \"2000.00\", \"currency\": \"USD\"}, \"trading_partner_id\": \"$us\"}, {\"account_id\": \"$revenue\", \"amount\": {\"amount\": \"-2000.00\", \"currency\": \"USD\"}, \"trading_partner_id\": \"$us\"}]}"
  api 201 '"journal entry \(.document_number): \([.lines[] | "\(.amount.amount) USD = \(.amount_lc.amount) CAD, trading partner \(.trading_partner.code) \(.trading_partner.name)"] | join(" / "))"' \
    postJournalEntry -body "$body"
  je=$(field .id)
  say "She reads it back: each line names its trading partner by company code and name, beside its id."
  api 200 '.lines[] | "  line \(.line_no): \(.amount.amount) \(.amount.currency), trading partner \(.trading_partner.code) \(.trading_partner.name) (\(.trading_partner.id[0:8])…)"' \
    getJournalEntry -p "id=$je"
  expect "the lines' trading partners" "$(field '[.lines[].trading_partner | "\(.code) \(.name)"] | unique | join(", ")')" "1000 Acme Corp US"
  expect "the trading partner's id" "$(field '[.lines[].trading_partner.id] | unique | join(" ")')" "$us"
  say "The journal lists Maple's entries with company 1000 as trading partner: this invoice alone so far."
  api 200 '.items[] | "  entry \(.document_number) of \(.posting_date): \(.header_text)"' listJournalEntries -p "legal_entity_id=$LE" -p "trading_partner_id=$us"
  expect "the entries naming Acme Corp US" "$(field '[.items[].id] | join(" ")')" "$je"
  say "Cory, who may act in Maple alone, reads the same entry: its trading partner is a company he may not act in, so"
  say "he is shown its id alone, without its code or name (product owner 2026-11-05, ADR-0010):"
  as cory
  api 200 '.lines[] | "  line \(.line_no): trading partner \(.trading_partner.id[0:8])…, code \(.trading_partner.code // "not shown"), name \(.trading_partner.name // "not shown")"' \
    getJournalEntry -p "id=$je"
  expect "the trading partner Cory sees" "$(field '[.lines[].trading_partner | "\(.id) \(.code) \(.name)"] | unique | join(", ")')" "$us null null"
  as carla
  say "A trading partner is another company of the group: a line naming Maple itself is refused on that line."
  refused 422 ledger.trading_partner_invalid postJournalEntry -body "${body%\"$us\"\}\]\}}\"$LE\"}]}"
  expect "the refused line" "$(field '[.errors[] | "\(.field) \(.code)"] | join(", ")')" "lines[1].trading_partner_id own_company"
  trading_partner_derived "$us" "$first"
}

# trading_partner_derived US FIRST is T68c (ADR-0059 §4): Ben creates
# Maple's business partner record of Acme Corp US (company US), a supplier
# set up in Maple as Tyrell is, linked to the company; Carla approves it;
# Alex posts its USD invoice of day FIRST through payables without naming
# a trading partner, and every line of its entry carries company 1000.
trading_partner_derived() {
  local us=$1 first=$2 recon term partner invoice entry item
  as ben
  QUIET=1 api 200 '' getBusinessPartner -p "id=$SUPPLIER"
  recon=$(field ".supplier_companies[] | select(.legal_entity_id == \"$LE\") | .reconciliation_account_id")
  term=$(field ".supplier_companies[] | select(.legal_entity_id == \"$LE\") | .payment_term_code")
  say "Acme Corp US also invoices Maple, for engineering it does for its sister. Ben creates its business partner"
  say "record as a supplier of Maple and links it to company 1000, the group company it is (ADR-0059 §4):"
  api 201 '"business partner \(.number) \(.name): \(.status), group company \(.group_company.code) \(.group_company.name)"' createBusinessPartner \
    -body "{\"kind\": \"organization\", \"name\": \"Acme Corp US\", \"country_code\": \"US\", \"roles\": [\"supplier\"], \"supplier_companies\": [{\"legal_entity_id\": \"$LE\", \"reconciliation_account_id\": \"$recon\", \"payment_term_code\": \"$term\"}], \"group_company_id\": \"$us\", \"reason\": \"Our US sister invoices engineering to Maple\"}"
  partner=$(field .id)
  expect "the partner's group company" "$(field '"\(.group_company_id) \(.group_company.code) \(.group_company.name)"')" "$us 1000 Acme Corp US"
  QUIET=1 api 200 '' listPartnerChanges -p "partner_id=$partner" -p kind=create
  approve carla "$(field '.items[0].workflow_request_id')" "Our own US company, checked against the group's companies"
  as alex
  QUIET=1 api 200 '' getBusinessPartner -p "id=$partner"
  expect "the partner's status" "$(field .status)" active
  say "Alex posts its invoice, USD 1,500.00 of engineering on 6100 dated $first, naming no trading partner:"
  item="{\"account_id\": \"$(account 6100)\", \"description\": \"Engineering services\", \"quantity\": {\"value\": \"1\", \"unit\": \"EA\"}, \"amount\": {\"amount\": \"1500.00\", \"currency\": \"USD\"}}"
  api 201 '"\(.kind) \(.document_number) \(.supplier_reference): \(.status), \(.gross_amount.amount) \(.gross_amount.currency)"' createSupplierInvoice \
    -body "{\"legal_entity_id\": \"$LE\", \"supplier_id\": \"$partner\", \"supplier_reference\": \"ACME-US-7\", \"invoice_date\": \"$first\", \"gross_amount\": {\"amount\": \"1500.00\", \"currency\": \"USD\"}, \"tax_amount\": {\"amount\": \"0.00\", \"currency\": \"USD\"}, \"items\": [$item]}"
  invoice=$(field .id)
  api 200 '"\(.document_number): \(.status), journal entry \(.journal_entry_id)"' postSupplierInvoice -p "id=$invoice" -if-match '"1"' -body "{\"posting_date\": \"$first\"}"
  entry=$(field .journal_entry_id)
  say "The ledger took the trading partner from the business partner: the supplier line, and the expense line with it."
  api 200 '.lines[] | "  line \(.line_no): \(.account_id | acct) \(.amount.amount) \(.amount.currency), trading partner \(.trading_partner.code) \(.trading_partner.name)"' \
    getJournalEntry -p "id=$entry"
  expect "the invoice's lines' trading partners" "$(field '[.lines[] | "\(.trading_partner.code) \(.trading_partner.name)"] | unique | join(", ")')" "1000 Acme Corp US"
  expect "the invoice's lines" "$(field '.lines | length')" 2
}

# consolidation_run is the consolidation run and its currency translation
# (T68d, ADR-0059 §6, §7), part of step_consolidation. Acme Corp US's
# period of today opens; Ben records Maple Industries Canada as a supplier
# of Acme Corp US, its intercompany payables account 225000 and linked to
# company 3000; Alex posts Maple's USD 2,000.00 invoice for engineering at
# Acme Corp US on the month's first day (at 1.35, step 9's rate). Carla
# runs the group MAPLE for today's period: the run is computed; Acme Corp
# US's payable at the closing rate 1.40 (step 9's rate from the month's
# end, so any day of the month gives these figures), its expense at the
# booked 1.35, the difference on the currency translation reserve; Maple,
# in the group currency, at rate 1 with no difference.
consolidation_run() {
  local first="${TODAY:0:7}-01" period=$((10#${TODAY:5:2})) us p recon term partner item invoice run
  as carla
  QUIET=1 api 200 '' listLegalEntities -p page_size=100
  us=$(field '.items[] | select(.code == "1000") | .id')
  say "Acme Corp US's periods were set up closed (T68a). Carla opens today's period, $YEAR-$period, in its leading ledger"
  say "for the sister's invoice:"
  QUIET=1 api 200 '' listFiscalPeriods -p "legal_entity_id=$us" -p "fiscal_year=$YEAR" -p "ledger_id=$LEADING" -p page_size=100
  p=$(field ".items[] | select(.period == $period) | \"\(.id)/\(.version)\"")
  api 200 '"period \(.fiscal_year)-\(.period) of Acme Corp US: \(.status)"' openFiscalPeriod -p "id=${p%/*}" -if-match "\"${p#*/}\""
  as ben
  QUIET=1 api 200 '' listAccounts -p "legal_entity_id=$us" -p page_size=200
  cp "$WORK/last.json" "$WORK/us-accounts.json"
  recon=$(jq -r '.items[] | select(.number == "225000") | .id' "$WORK/us-accounts.json")
  QUIET=1 api 200 '' getBusinessPartner -p "id=$SUPPLIER"
  term=$(field ".supplier_companies[] | select(.legal_entity_id == \"$LE\") | .payment_term_code")
  say "Maple invoices its sister for engineering too. Ben records Maple Industries Canada as a supplier of Acme Corp US,"
  say "on its intercompany payables account 225000, linked to company $CC, the group company it is (ADR-0059 §4):"
  api 201 '"business partner \(.number) \(.name): \(.status), group company \(.group_company.code) \(.group_company.name)"' createBusinessPartner \
    -body "{\"kind\": \"organization\", \"name\": \"Maple Industries Canada\", \"country_code\": \"CA\", \"roles\": [\"supplier\"], \"supplier_companies\": [{\"legal_entity_id\": \"$us\", \"reconciliation_account_id\": \"$recon\", \"payment_term_code\": \"$term\"}], \"group_company_id\": \"$LE\", \"reason\": \"Our Canadian sister invoices engineering to Acme Corp US\"}"
  partner=$(field .id)
  QUIET=1 api 200 '' listPartnerChanges -p "partner_id=$partner" -p kind=create
  approve carla "$(field '.items[0].workflow_request_id')" "Our own Canadian company, checked against the group's companies"
  as alex
  say "Alex posts Maple's invoice at Acme Corp US, USD 2,000.00 of intercompany services on 525000 dated $first:"
  item="{\"account_id\": \"$(jq -r '.items[] | select(.number == "525000") | .id' "$WORK/us-accounts.json")\", \"description\": \"Engineering services\", \"quantity\": {\"value\": \"1\", \"unit\": \"EA\"}, \"amount\": {\"amount\": \"2000.00\", \"currency\": \"USD\"}}"
  api 201 '"\(.kind) \(.document_number) \(.supplier_reference): \(.status), \(.gross_amount.amount) \(.gross_amount.currency)"' createSupplierInvoice \
    -body "{\"legal_entity_id\": \"$us\", \"supplier_id\": \"$partner\", \"supplier_reference\": \"MAPLE-CA-11\", \"invoice_date\": \"$first\", \"gross_amount\": {\"amount\": \"2000.00\", \"currency\": \"USD\"}, \"tax_amount\": {\"amount\": \"0.00\", \"currency\": \"USD\"}, \"items\": [$item]}"
  invoice=$(field .id)
  api 200 '"\(.document_number): \(.status), journal entry \(.journal_entry_id)"' postSupplierInvoice -p "id=$invoice" -if-match '"1"' -body "{\"posting_date\": \"$first\"}"
  consolidation_investment "$us"
  as carla
  say "Carla runs the group MAPLE for $YEAR-$period (ADR-0059 §6): a run of a group and period is a long-running operation"
  say "that reads each company's leading-ledger balances through the period and translates them into the group currency,"
  say "CAD (§7, a method to be confirmed by an accounting specialist): assets and liabilities at the closing rate, income,"
  say "expenses and equity at their booked group amounts, earlier results on retained earnings, the rest on the reserve."
  api 202 '"operation \(.kind) \(.status): \(.resource)"' createConsolidationRun -body "{\"group_code\": \"MAPLE\", \"fiscal_year\": $YEAR, \"period\": $period}"
  run=$(field '.resource | split("/") | last')
  wait_op
  api 200 '"run of \(.group_code) \(.group_name) for \(.fiscal_year)-\(.period) (\(.period_start) to \(.period_end)): \(.status), in \(.group_currency) at \(.rate_type) rates, configuration version \(.config_version)",
    (.members[] | "  \(.role) \(.company_code) \(.company_name) (\(.local_currency)): closing rate \(.closing_rate)\(if .rate_valid_from then " from \(.rate_valid_from)" else "" end), period \(.period_status) at version \(.period_version)")' \
    getConsolidationRun -p "id=$run"
  expect "the run" "$(field .status)" computed
  expect "the closing rates" "$(field '[.members[] | "\(.company_code)=\(.closing_rate)"] | join(" ")')" "$CC=1 1000=1.4"
  say "Acme Corp US's translated balances: its payable to Maple at the closing 1.40, its expense at the booked 1.35, and"
  say "the difference on the currency translation reserve, so its translated figures sum to zero:"
  api 200 '.items[] | "  \(.line_no) \(.kind) \(.group_account.number) \(.group_account.name)\(if .account then " / \(.account.number) \(.account.name)" else "" end)\(if .trading_partner then ", trading partner \(.trading_partner.code)" else "" end): \(if .amount_lc then "\(.amount_lc.amount) \(.amount_lc.currency), booked \(.amount_gc_booked.amount), " else "" end)translated \(.amount_gc.amount) \(.amount_gc.currency)\(if .closing_rate then " at \(.closing_rate)" else "" end)"' \
    listConsolidationBalances -p "id=$run" -p "legal_entity_id=$us"
  # The bank's USD 5,800.00 at the closing 1.40, the payable at 1.40, the
  # share capital, the earlier sales and the expense as booked; the rest
  # on the reserve.
  local cta=$((-(812000 - 280000 - CAPITAL - RETAINED + 270000))) rows
  rows="balance 100000 5800.00 8120.00, balance 225000 -2000.00 -2800.00, balance 300000 -5000.00 -$(amount "$CAPITAL"), prior_results - -800.00 -$(amount "$RETAINED")"
  [ "$cta" = 0 ] || rows="$rows, cta - - $(amount "$cta")"
  expect "Acme Corp US's rows" "$(field '[.items[] | "\(.kind) \(.account.number // "-") \(.amount_lc.amount // "-") \(.amount_gc.amount)"] | join(", ")')" \
    "$rows, balance 525000 2000.00 2700.00"
  expect "Acme Corp US's translated total" "$(field .totals.group.amount)" 0.00
  say "Maple keeps its books in the group currency: rate 1, and no translation difference:"
  api 200 '"  Maple: \(.items | length) translation differences; its rows sum to \(.totals.group.amount) \(.totals.group.currency)"' \
    listConsolidationBalances -p "id=$run" -p "legal_entity_id=$LE" -p kind=cta
  expect "Maple's translation differences" "$(field '.items | length')" 0
  say "Acme Corp US's acquisition (ADR-0059 §9): Maple incorporated it, so there is no goodwill and no non-controlling"
  say "interest, and Maple's investment equals its share capital as booked, which alone it is eliminated against (B5(a));"
  say "its net assets at the end of the period before its first, its earlier sales among them, are shown at that period's"
  say "closing rate:"
  api 200 '.members[] | select(.acquisition) | "  \(.company_code) at \(.acquisition.fiscal_year)-\(.acquisition.period), rate \(.acquisition.closing_rate) from \(.acquisition.rate_valid_from): net assets \(.acquisition.net_assets.amount) \(.acquisition.net_assets.currency) = \(.acquisition.net_assets_group.amount) \(.acquisition.net_assets_group.currency), investment \(.acquisition.investment.amount), goodwill \(.acquisition.goodwill.amount), non-controlling interest \(.acquisition.non_controlling_interest.amount), frozen \(.acquisition.frozen)"' \
    getConsolidationRun -p "id=$run"
  expect "Acme Corp US's acquisition" "$(field '.members[] | select(.acquisition) | .acquisition | "\(.net_assets.amount) \(.net_assets_group.amount) \(.investment.amount) \(.goodwill.amount) \(.non_controlling_interest.amount) \(.frozen)"')" \
    "5800.00 $(amount $((5800 * ${CAPITAL} / 5000))) $(amount "$CAPITAL") 0.00 0.00 false"
}

# consolidation_investment US is the investment a run eliminates (T68h,
# ADR-0059 §9): Maple incorporated Acme Corp US (company US), whose share
# capital of USD 5,000.00 is posted on the last day of the period before
# its first, at the rate Tom records from that day, so that the period has
# a closing rate from within it (1.35; in January the 1.38 of December's
# last day, which step_year_end records too, so that the final run of
# step_close measures the acquisition at the same rate), with USD 800.00
# of sales on the same day, its retained earnings before the group's
# first period, which stay the group's (T68h2: an incorporated subsidiary's
# investment is eliminated against its share capital alone, B5(a) and B8,
# po:2026-11-09); and Maple's investment, the share capital's CAD amount,
# on the month's first day against its bank. CAPITAL and RETAINED are
# those CAD amounts in cents, ACQ_PERIOD that period (YYYY-MM).
consolidation_investment() {
  local us=$1 first="${TODAY:0:7}-01" end rate=135 p year period
  end=$(date_add "$first" -1)
  [ "${TODAY:5:2}" = 01 ] && rate=138
  year=$((10#${end:0:4})) period=$((10#${end:5:2}))
  CAPITAL=$((5000 * rate)) RETAINED=$((800 * rate)) ACQ_PERIOD=${end:0:7}
  as tom
  say "Maple founded Acme Corp US with USD 5,000.00 of share capital, booked on $end, the last day of the period before"
  say "the group's first; Tom records the USD rate of that day, so that the period has a closing rate:"
  usd_rate "$end" "${rate:0:1}.${rate:1}"
  as carla
  QUIET=1 api 200 '' listFiscalPeriods -p "legal_entity_id=$us" -p "fiscal_year=$year" -p "ledger_id=$LEADING" -p page_size=100
  p=$(field ".items[] | select(.period == $period) | \"\(.id)/\(.version)\"")
  api 200 '"period \(.fiscal_year)-\(.period) of Acme Corp US: \(.status)"' openFiscalPeriod -p "id=${p%/*}" -if-match "\"${p#*/}\""
  api 201 '"journal entry \(.document_number) of \(.posting_date): \([.lines[] | "\(.amount.amount) \(.amount.currency)"] | join(" / "))"' postJournalEntry \
    -body "{\"legal_entity_id\": \"$us\", \"document_date\": \"$end\", \"posting_date\": \"$end\", \"currency\": \"USD\", \"header_text\": \"Share capital paid in by Maple\", \"lines\": [{\"account_id\": \"$(jq -r '.items[] | select(.number == "100000") | .id' "$WORK/us-accounts.json")\", \"amount\": {\"amount\": \"5000.00\", \"currency\": \"USD\"}}, {\"account_id\": \"$(jq -r '.items[] | select(.number == "300000") | .id' "$WORK/us-accounts.json")\", \"amount\": {\"amount\": \"-5000.00\", \"currency\": \"USD\"}}]}"
  say "Acme Corp US sold USD 800.00 of engineering on the same day, a result before the group's first period:"
  api 201 '"journal entry \(.document_number) of \(.posting_date): \([.lines[] | "\(.amount.amount) \(.amount.currency)"] | join(" / "))"' postJournalEntry \
    -body "{\"legal_entity_id\": \"$us\", \"document_date\": \"$end\", \"posting_date\": \"$end\", \"currency\": \"USD\", \"header_text\": \"Engineering sold before the group's first period\", \"lines\": [{\"account_id\": \"$(jq -r '.items[] | select(.number == "100000") | .id' "$WORK/us-accounts.json")\", \"amount\": {\"amount\": \"800.00\", \"currency\": \"USD\"}}, {\"account_id\": \"$(jq -r '.items[] | select(.number == "400000") | .id' "$WORK/us-accounts.json")\", \"amount\": {\"amount\": \"-800.00\", \"currency\": \"USD\"}}]}"
  accounts
  say "Maple books its investment, CAD $(amount "$CAPITAL"), on $first:"
  api 201 '"journal entry \(.document_number) of \(.posting_date): \([.lines[] | "\(.amount.amount) \(.amount.currency)"] | join(" / "))"' postJournalEntry \
    -body "{\"legal_entity_id\": \"$LE\", \"document_date\": \"$first\", \"posting_date\": \"$first\", \"currency\": \"CAD\", \"header_text\": \"Investment in Acme Corp US\", \"lines\": [{\"account_id\": \"$(account 1700)\", \"amount\": {\"amount\": \"$(amount "$CAPITAL")\", \"currency\": \"CAD\"}}, {\"account_id\": \"$(account 1000)\", \"amount\": {\"amount\": \"-$(amount "$CAPITAL")\", \"currency\": \"CAD\"}}]}"
}

# usd_rate DAY RATE records the average USD/CAD rate RATE valid from DAY,
# unless a step before recorded it (step_consolidation records 31 December
# of PREV in January, step_year_end records it every month), which it then
# checks is RATE.
usd_rate() {
  local day=$1 rate=$2
  QUIET=1 api 200 '' listExchangeRates -p rate_type=average -p from_currency=USD -p to_currency=CAD -p page_size=100
  if [ "$(field "[.items[] | select(.valid_from == \"$day\")] | length")" != 0 ]; then
    say "  USD/CAD from $day is recorded already:"
    expect "the rate from $day" "$(field "[.items[] | select(.valid_from == \"$day\") | .rate | tonumber == $rate] | all")" true
    return 0
  fi
  api 201 '"\(.from_currency)/\(.to_currency) \(.rate) from \(.valid_from) (\(.source))"' createExchangeRate \
    -body "{\"rate_type\": \"average\", \"from_currency\": \"USD\", \"to_currency\": \"CAD\", \"valid_from\": \"$day\", \"rate\": \"$rate\", \"source\": \"BOC\"}"
}

# consolidation_eliminations is intercompany matching and eliminations
# (T68f, ADR-0059 §8), part of step_consolidation: consolidation_run's run
# matched Maple's intercompany revenue with Acme Corp US's intercompany
# expense, both booked at 1.35 (2,700.00 CAD each), and found the
# receivable and payable pair open: Maple's USD receivable as booked at
# 1.35 (2,700.00 CAD), Acme Corp US's payable at the closing 1.40
# (-2,800.00 CAD), a difference of -100.00 above IC_BALANCES' 10.00. Each
# pair's entry reverses its rows, the difference on G6950, so the
# consolidated statements show no balance between the two companies.
consolidation_eliminations() {
  local run
  as carla
  QUIET=1 api 200 '' listConsolidationRuns -p group_code=MAPLE -p status=computed
  run=$(field '.items[0].id')
  say "The run matched the group's intercompany balances (ADR-0059 §8): for each elimination, the two companies' rows"
  say "naming each other, in CAD as translated: balance sheet rows at the closing rate, income and expenses as booked."
  api 200 '"run \(.status): \([.difference_counts[] | "\(.count) \(.status)"] | join(", "))"' getConsolidationRun -p "id=$run"
  api 200 '(.items[] | "  \(.line_no) \(.elimination.code) \(.elimination.name): \(.company_code) \(.company_name) and \(.partner.code) \(.partner.name): \(.amount.amount) + \(.partner_amount.amount) = \(.difference.amount) \(.difference.currency) against a tolerance of \(.tolerance.amount), \(.status)"),
    (.totals[] | "  total \(.status): \(.count), difference \(.difference.amount // "-")")' listIntercompanyDifferences -p "id=$run"
  expect "the differences" "$(field '[.items[] | "\(.elimination.code) \(.company_code)/\(.partner.code) \(.amount.amount) \(.partner_amount.amount) \(.difference.amount) \(.status)"] | join(", ")')" \
    "IC_BALANCES 1000/$CC -2800.00 2700.00 -100.00 open, IC_RESULTS 1000/$CC 2700.00 -2700.00 0.00 matched"
  say "The payable at the closing 1.40 and the receivable as booked at 1.35 differ by more than the tolerance: the"
  say "difference is open, and stops only finalizing the run until it is accepted or corrected (T68g). Each pair is"
  say "eliminated: its rows reversed, its difference on the intercompany differences account:"
  api 200 '.items[] | "  entry \(.entry_no) \(.kind)\(if .elimination then " \(.elimination.code)" else "" end): \(.company_code)\(if .partner then " and \(.partner.code)" else "" end), debits \(.debits.amount) \(.debits.currency)",
    (.lines[] | "    \(.group_account.number) \(.group_account.name)\(if .company_code then " / \(.company_code)\(if .account then " \(.account.number) \(.account.name)" else "" end)" else "" end)\(if .trading_partner then ", trading partner \(.trading_partner.code)" else "" end)\(if .component then " (\(.component))" else "" end): \(.amount.amount)")' \
    listConsolidationEntries -p "id=$run"
  expect "the entries' lines" "$(field '[.items[] | "\(.entry_no):" + ([.lines[] | "\(.group_account.number)=\(.amount.amount)"] | join(" "))] | join(", ")')" \
    "1:G1250=-2700.00 G2250=2800.00 G6950=-100.00, 2:G4250=2700.00 G5250=-2700.00, 3:G1700=-$(amount "$CAPITAL") G3000=$(amount "$CAPITAL")"
  say "Maple's investment is eliminated against Acme Corp US's share capital alone (ADR-0059 §9, As built T68h2):"
  expect "the investment entry" "$(field '[.items[] | select(.kind == "investment") | .lines[] | "\(.component) \(.company_code) \(.account.number // "-") \(.amount.amount)"] | join(", ")')" \
    "investment $CC 1700 -$(amount "$CAPITAL"), acquisition_equity 1000 - $(amount "$CAPITAL")"
  expect "the entries' debits" "$(field '[.totals[] | "\(.kind) \(.count) \(.debits.amount)"] | join(", ")')" "intercompany 2 5500.00, investment 1 $(amount "$CAPITAL")"
}

# consolidation_statements is the consolidated statements of a run (T68e,
# ADR-0059 §11), part of step_consolidation: Carla reads the balance sheet
# and the profit and loss of consolidation_run's run, one row per group
# account with the companies' translated figures summed, the eliminations
# (T68f's intercompany entries, T68h's investment against the share
# capital alone, which leaves Acme Corp US's earlier sales on retained
# earnings, T68h2) and the consolidated figure; the balance sheet's
# assets, liabilities and equity and the fiscal year's profit sum to zero,
# the profit is the profit and loss's, and each row's reported figure is
# the balances list's total for its group account.
consolidation_statements() {
  local run statement number reported total us differ="" cents='(.amount | tonumber * 100 | round)'
  as carla
  QUIET=1 api 200 '' listConsolidationRuns -p group_code=MAPLE -p status=computed
  run=$(field '.items[0].id')
  say "Carla reads the run's consolidated balance sheet (ADR-0059 §11): each account of the group chart, the companies'"
  say "translated figures summed, the eliminations and the consolidated figure, in CAD and debit positive, so a credit"
  say "balance, and a profit, is negative. The eliminations are the run's intercompany entries (T68f):"
  api 200 '"\(.statement) of \(.group_code) \(.group_name) at \(.period_end), run \(.run_status), in \(.group_currency):",
    (.items[] | "  \(.number) \(.name) (\(.type)\(if .role then ", \(.role)" else "" end)): reported \(.reported.amount), eliminations \(.eliminations.amount), consolidated \(.consolidated.amount)"),
    (.totals | to_entries[] | "  total \(.key): reported \(.value.reported.amount), eliminations \(.value.eliminations.amount), consolidated \(.value.consolidated.amount)")' \
    getConsolidatedStatement -p "id=$run" -p statement=balance_sheet -p page_size=500
  cp "$WORK/last.json" "$WORK/balance-sheet.json"
  expect "the balance sheet's totals" "$(field '.totals | keys | join(" ")')" "assets equity liabilities profit"
  expect "assets, liabilities, equity and the profit, consolidated" "$(field "[.totals[].consolidated | $cents] | add")" 0
  expect "assets, liabilities, equity and the profit, reported" "$(field "[.totals[].reported | $cents] | add")" 0
  expect "the intercompany accounts, consolidated" "$(field '[.items[] | select(.number | IN("G1250", "G2250")) | "\(.number) \(.consolidated.amount)"] | join(", ")')" \
    "G1250 0.00, G2250 0.00"
  expect "the investment, eliminated" "$(field '[.items[] | select(.number | IN("G1700", "G1900", "G3950")) | "\(.number) \(.reported.amount) \(.eliminations.amount) \(.consolidated.amount)"] | join(", ")')" \
    "G1700 $(amount "$CAPITAL") -$(amount "$CAPITAL") 0.00, G1900 0.00 0.00 0.00, G3950 0.00 0.00 0.00"
  say "Acme Corp US's sales before the group's first period are a result since Maple incorporated it (B8): they stay on the"
  say "group's retained earnings, uneliminated, as booked (B3):"
  QUIET=1 api 200 '' listLegalEntities -p page_size=100
  us=$(field '.items[] | select(.code == "1000") | .id')
  api 200 '.items[] | "  \(.company_code) \(.kind) \(.group_account.number) \(.group_account.name): \(.amount_lc.amount) \(.amount_lc.currency), translated \(.amount_gc.amount) \(.amount_gc.currency)"' \
    listConsolidationBalances -p "id=$run" -p group_account=G3800 -p "legal_entity_id=$us"
  expect "Acme Corp US's retained earnings" "$(field '[.items[] | "\(.kind) \(.amount_gc.amount)"] | join(", ")')" "prior_results -$(amount "$RETAINED")"
  expect "the retained earnings, consolidated" "$(jq -r '.items[] | select(.number == "G3800") | "\(.eliminations.amount) \(.consolidated.amount == .reported.amount)"' "$WORK/balance-sheet.json")" "0.00 true"
  say "And the profit and loss, whose profit closes the balance sheet:"
  api 200 '"\(.statement) of \(.group_code) for \(.fiscal_year)-\(.period):",
    (.items[] | "  \(.number) \(.name) (\(.type)): reported \(.reported.amount), eliminations \(.eliminations.amount), consolidated \(.consolidated.amount)"),
    (.totals | to_entries[] | "  total \(.key): reported \(.value.reported.amount), consolidated \(.value.consolidated.amount)")' \
    getConsolidatedStatement -p "id=$run" -p statement=profit_and_loss -p page_size=500
  cp "$WORK/last.json" "$WORK/profit-and-loss.json"
  expect "the profit of both statements" "$(field .totals.profit.consolidated.amount)" "$(jq -r .totals.profit.consolidated.amount "$WORK/balance-sheet.json")"
  expect "revenue and expenses make the profit" "$(field "[.totals.revenue, .totals.expenses | .reported | $cents] | add")" "$(field ".totals.profit.reported | $cents")"
  say "The profit is the owners' of the parent: Maple holds the whole of Acme Corp US, so no part of it is a non-controlling interest's:"
  expect "the profit's attribution" "$(field '"\(.totals.non_controlling_interests.consolidated.amount) \(.totals.owners_of_the_parent.consolidated.amount == .totals.profit.consolidated.amount)"')" "0.00 true"
  consolidation_comprehensive_income
  expect "the intercompany results and their difference" \
    "$(field '[.items[] | select(.number | IN("G4250", "G5250", "G6950")) | "\(.number) \(.eliminations.amount) \(.consolidated.amount)"] | join(", ")')" \
    "G4250 2700.00 0.00, G5250 -2700.00 0.00, G6950 -100.00 -100.00"
  say "Each row's reported figure is the run's translated balances on its group account (the drill-down):"
  for statement in balance-sheet profit-and-loss; do
    while read -r number reported; do
      QUIET=1 api 200 '' listConsolidationBalances -p "id=$run" -p "group_account=$number" -p page_size=1
      total=$(field .totals.group.amount)
      [ "$total" = "$reported" ] || differ="$differ $number:$reported/$total"
    done < <(jq -r '.items[] | "\(.number) \(.reported.amount)"' "$WORK/$statement.json")
  done
  say "  $(jq '.items | length' "$WORK/balance-sheet.json") balance sheet and $(jq '.items | length' "$WORK/profit-and-loss.json") profit and loss rows checked against the balances list"
  expect "rows whose reported figure differs from the balances list's total" "$differ" ""
}

# consolidation_accept is accepting an intercompany difference (T68g,
# ADR-0059 §8, §10), part of step_consolidation: Carla accepts the run's
# open IC_BALANCES difference (the payable at the closing 1.40 against the
# receivable as booked at 1.35) with her reason; the run's version moves
# with it. Finalizing the run is then refused: the companies' periods of
# today were open when the run read their figures, and a run is final only
# on locked periods. step_close finalizes the group after the sign-off.
consolidation_accept() {
  local run difference tag
  as carla
  QUIET=1 api 200 '' listConsolidationRuns -p group_code=MAPLE -p status=computed
  run=$(field '.items[0].id')
  QUIET=1 api 200 '' listIntercompanyDifferences -p "id=$run" -p status=open
  difference=$(field '.items[0].id')
  tag="\"$(field '.items[0].version')\""
  say "The open difference is the rates' (ADR-0059 §8): Maple's receivable is booked at the invoice date's 1.35, Acme"
  say "Corp US's payable translated at the closing 1.40; neither company's postings are wrong. Carla accepts it with her"
  say "reason, a person's decision (agents are refused), so that it no longer stops finalizing the run:"
  api 200 '"\(.elimination.code) \(.company_code)/\(.partner.code): \(.difference.amount) \(.difference.currency), \(.status) by \(.accepted_by_name): \(.reason)"' \
    acceptIntercompanyDifference -p "id=$run" -p "difference_id=$difference" -if-match "$tag" \
    -body '{"reason": "Rate difference: the receivable is booked at the invoice date'"'"'s rate, the payable translated at the closing rate"}'
  expect "the accepted difference" "$(field '"\(.status) \(.accepted_by_name) \(.version)"')" "accepted Carla Controller 2"
  api 200 '"run \(.status) at version \(.version): \([.difference_counts[] | "\(.count) \(.status)"] | join(", ")), stale \(.stale)"' getConsolidationRun -p "id=$run"
  expect "the run's differences" "$(field '[.difference_counts[] | "\(.count) \(.status)"] | join(", ")')" "1 matched, 1 accepted"
  say "Finalizing the run as the group's consolidated figures of $YEAR-$((10#${TODAY:5:2})) is refused: the companies' periods"
  say "were open when it read their figures. A run is final only on locked periods; the group is run again after the"
  say "close's sign-off (step_close):"
  refused 409 consolidation.period_not_locked finalizeConsolidationRun -p "id=$run" -if-match "$ETAG"
}

# consolidation_revaluation is the revaluation of intercompany items by
# trading partner (T68b2, ADR-0059 §4, ADR-0042), part of
# step_consolidation: Maple's USD 2,000.00 receivable from Acme Corp US,
# booked at 1.35, is worth 1.40 at the month's end, a gain of CAD 100.00
# that the valuation entry posts on 1250 and 4900 with trading partner
# 1000, so a new run of the group finds the receivable and payable pair
# matched at 2,800.00, where the gain without a trading partner would have
# shown as no_trading_partner. A second run of the key date is refused.
# The receivable is designated as part of Maple's net investment in Acme
# Corp US (the accounting specialist's B4 correction 2, po:2026-11-09;
# identification per account and trading partner in the group's
# configuration, po:2026-11-10; IAS 21.32; ADR-0059 As built), so the run
# takes the gain on it out of the consolidated profit: its row on 4900
# with trading partner 1000 is booked CAD 100.00 and translated 0.00,
# Maple's translation reserve shows it (net_investment) and so do the
# totals, in other comprehensive income, while the differences on Maple's
# other items stay in the profit and Maple's own entries are as posted.
# The month is still running, so Carla cancels the revaluation, which
# reverses both its entries, and discards the new group run: the later
# steps see the month as booked.
consolidation_revaluation() {
  local eom period=$((10#${TODAY:5:2})) body reval valuation run us differences
  local cents='(.amount | tonumber * 100 | round)'
  eom=$(month_end)
  next_year_open
  as carla
  accounts
  say "Maple's intercompany receivable is in USD: Carla revalues Maple's foreign-currency items at $eom (ADR-0042). The"
  say "receivable's USD 2,000.00, booked at 1.35, is worth 1.40: a gain of CAD 100.00. A balance is valued per trading"
  say "partner, and each total's two lines carry it (ADR-0059 §4):"
  body="{\"legal_entity_id\": \"$LE\", \"key_date\": \"$eom\"}"
  api 201 '"run \(.id[0:8])… at \(.key_date): \(.status), \(.item_count) items",
    (.totals[] | "  \(.account_id | acct) \(.currency) \(.direction) \(.difference_lc.amount) CAD\(if .trading_partner then ", trading partner \(.trading_partner.code) \(.trading_partner.name)" else "" end)")' \
    postRevaluationRun -body "$body"
  reval=$(field .id)
  valuation=$(field .valuation_entry_id)
  differences=$(field "[.totals[].difference_lc | $cents] | add")
  expect "the intercompany receivable's total" \
    "$(jq -r --arg a "$(account 1250)" '[.totals[] | select(.account_id == $a) | "\(.direction) \(.amount.amount) \(.difference_lc.amount) \(.trading_partner.code)"] | join(", ")' "$WORK/last.json")" \
    "gain 2000.00 100.00 1000"
  say "Its valuation entry posts the gain on the receivable and on the unrealised gain, both naming company 1000:"
  api 200 '.lines[] | select(.trading_partner) | "  line \(.line_no): \(.account_id | acct) \(.amount_lc.amount) CAD, trading partner \(.trading_partner.code)"' \
    getJournalEntry -p "id=$valuation"
  expect "the receivable's revaluation lines" \
    "$(jq -r --arg r "$(account 1250)" --arg g "$(account 4900)" '[.lines[] | select(.trading_partner and (.account_id == $r or .account_id == $g)) | "\(.amount_lc.amount) \(.trading_partner.code)"] | join(", ")' "$WORK/last.json")" \
    "100.00 1000, -100.00 1000"
  say "A second run at the same key date is refused, naming the first:"
  refused 409 ledger.revaluation_exists postRevaluationRun -body "$body"
  say "Carla runs the group again: Maple's receivable is now CAD 2,800.00 with trading partner 1000, the payable's figure:"
  api 202 '"operation \(.kind) \(.status): \(.resource)"' createConsolidationRun -body "{\"group_code\": \"MAPLE\", \"fiscal_year\": $YEAR, \"period\": $period}"
  run=$(field '.resource | split("/") | last')
  wait_op
  api 200 '.items[] | "  \(.elimination.code): \(.company_code) and \(.partner.code): \(.amount.amount) + \(.partner_amount.amount) = \(.difference.amount) \(.difference.currency), \(.status)"' \
    listIntercompanyDifferences -p "id=$run"
  expect "the differences after the revaluation" \
    "$(field '[.items[] | "\(.elimination.code) \(.company_code)/\(.partner.code) \(.amount.amount) \(.partner_amount.amount) \(.difference.amount) \(.status)"] | join(", ")')" \
    "IC_BALANCES 1000/$CC -2800.00 2800.00 0.00 matched, IC_RESULTS 1000/$CC 2700.00 -2700.00 0.00 matched"
  say "The receivable is part of Maple's net investment in Acme Corp US (the group's net_investments): in the consolidated"
  say "statements the exchange difference on it is other comprehensive income, not profit (IAS 21.32). Maple's ledger keeps"
  say "the gain as posted; the run takes it off the gain's row with trading partner 1000:"
  QUIET=1 api 200 '' listLegalEntities -p page_size=100
  us=$(field '.items[] | select(.code == "1000") | .id')
  api 200 '.items[] | "  \(.company_code) \(.account.number) \(.account.name), trading partner \(.trading_partner.code): booked \(.amount_gc_booked.amount), translated \(.amount_gc.amount) \(.amount_gc.currency)"' \
    listConsolidationBalances -p "id=$run" -p "legal_entity_id=$LE" -p "account_id=$(account 4900)" -p "trading_partner_id=$us"
  expect "the gain's row" "$(field '[.items[] | "\(.trading_partner.code) \(.amount_gc_booked.amount) \(.amount_gc.amount)"] | join(", ")')" "1000 -100.00 0.00"
  say "The profit and loss shows it on Maple's translation reserve, per company and in its totals. The differences on"
  say "Maple's other foreign-currency items, CAD $(amount $((differences - 10000))) in all, stay in the profit:"
  api 200 '(.translation_reserve[] | "  \(.company_code) \(.company_name): reserve \(.closing.amount), net investment differences moved out of the profit \(.net_investment.amount) \(.net_investment.currency)"),
    (.totals | "  total net_investment: reported \(.net_investment.reported.amount), eliminations \(.net_investment.eliminations.amount), consolidated \(.net_investment.consolidated.amount)",
      "  total profit: reported \(.profit.reported.amount), consolidated \(.profit.consolidated.amount); other comprehensive income \(.other_comprehensive_income.consolidated.amount)")' \
    getConsolidatedStatement -p "id=$run" -p statement=profit_and_loss -p page_size=500
  expect "what each company's net investments moved out of its profit" "$(field '[.translation_reserve[] | "\(.company_code) \(.net_investment.amount)"] | join(", ")')" \
    "1000 0.00, $CC -100.00"
  expect "the total moved out of the profit" \
    "$(field '.totals.net_investment | "\(.reported.amount) \(.eliminations.amount) \(.consolidated.amount)"')" "-100.00 0.00 -100.00"
  # Reported, not consolidated: the first run's eliminations held the pair's difference of CAD 100.00, now matched.
  expect "the companies' profit against the first run's: the revaluation's other differences, without the receivable's gain" \
    "$(($(field ".totals.profit.reported | $cents") - $(jq -r ".totals.profit.reported | $cents" "$WORK/profit-and-loss.json")))" "$((10000 - differences))"
  expect "other comprehensive income against the first run's: the receivable's gain" \
    "$(($(field ".totals.other_comprehensive_income.consolidated | $cents") - $(jq -r ".totals.other_comprehensive_income.consolidated | $cents" "$WORK/profit-and-loss.json")))" -10000
  QUIET=1 api 200 '' getConsolidatedStatement -p "id=$run" -p statement=balance_sheet -p page_size=500
  expect "assets, liabilities, equity and the profit, consolidated" "$(field "[.totals[].consolidated | $cents] | add")" 0
  say "The month is still running: Carla discards this run and cancels the revaluation, which reverses both its entries,"
  say "so the steps that follow see the month as booked."
  QUIET=1 api 200 '' getConsolidationRun -p "id=$run"
  api 200 '"run \(.id[0:8])…: \(.status)"' discardConsolidationRun -p "id=$run" -if-match "$ETAG"
  expect "the group run" "$(field .status)" discarded
  QUIET=1 api 200 '' getRevaluationRun -p "id=$reval"
  api 200 '"revaluation run \(.id[0:8])…: \(.status)"' cancelRevaluationRun -p "id=$reval" -if-match "$ETAG" \
    -body '{"reason": "The month is still running; it is revalued at its end"}'
  expect "the revaluation run" "$(field .status)" cancelled
}

# consolidation_revaluation_translation is the translation part of a
# group-currency revaluation, moved out of the profit (the accounting
# specialist's B4 correction 1, po:2026-11-09; IAS 21.39(c); ADR-0042, the
# group-currency difference in consolidation; ADR-0059 As built), part of
# step_consolidation. Acme Corp US, whose books are in USD, sold EUR
# 1,000.00 of engineering on the last day of the period before the
# group's first, booked at the day's average rates: USD 1,100.00 at 1.10
# and, in group currency, that at the day's USD/CAD rate. Carla revalues
# Acme Corp US's euros at that day's bank selling rates: 1.12 in USD, a
# gain of USD 20.00, and 1.50 in CAD on its own (ADR-0042), CAD 1,500.00
# against the booked group amount. So the gain's entries book a group
# amount that is not USD 20.00 translated. The reset, on the first day of
# the group's period, is in the year's profit: a new run of the group
# keeps its USD 20.00 at the entry's 1.35, CAD 27.00, there and shows the
# rest of its booked group amount on Acme Corp US's translation reserve
# (revaluation_translation), in other comprehensive income; the valuation
# itself is before the group's first period, so its part comes off the
# earlier results. Then Carla discards the run, cancels the revaluation
# and reverses the sale, so the steps that follow see the month as
# booked.
consolidation_revaluation_translation() {
  local first="${TODAY:0:7}-01" end period=$((10#${TODAY:5:2})) rate=135 us booked difference moved earlier sale reval run row
  local cents='(.amount | tonumber * 100 | round)'
  end=$(date_add "$first" -1)
  [ "${TODAY:5:2}" = 01 ] && rate=138
  booked=$((1100 * rate))             # USD 1,100.00 at the day's USD/CAD rate, in cents
  difference=$((150000 - booked))     # the group difference: EUR 1,000.00 at 1.50 less the booked group amount
  moved=$((difference - 2700))        # the reset's group amount less its USD 20.00 at the reset's 1.35
  earlier=$((20 * rate - difference)) # the valuation's: its USD 20.00 at the day's rate less its group amount, a credit
  as tom
  say "Acme Corp US sells in euros too. Tom records the euro's rates from $end: the average rate in USD, which a posting"
  say "converts at, and the bank selling rates in USD and in CAD, which Acme Corp US's revaluation values at (ADR-0042):"
  for row in "average USD 1.10" "bank_selling USD 1.12" "bank_selling CAD 1.50"; do
    # shellcheck disable=SC2086 # three words: the rate type, the currency and the rate
    set -- $row
    api 201 '"\(.rate_type) \(.from_currency)/\(.to_currency) \(.rate) from \(.valid_from) (\(.source))"' createExchangeRate \
      -body "{\"rate_type\": \"$1\", \"from_currency\": \"EUR\", \"to_currency\": \"$2\", \"valid_from\": \"$end\", \"rate\": \"$3\", \"source\": \"ECB\"}"
  done
  as carla
  QUIET=1 api 200 '' listLegalEntities -p page_size=100
  us=$(field '.items[] | select(.code == "1000") | .id')
  say "Acme Corp US sold EUR 1,000.00 of engineering on $end, paid into its bank: USD 1,100.00, and in group currency"
  say "that at the day's USD/CAD rate:"
  api 201 '"journal entry \(.document_number) of \(.posting_date): \([.lines[] | "\(.amount.amount) \(.amount.currency) = \(.amount_lc.amount) \(.amount_lc.currency) = \(.amount_gc.amount) \(.amount_gc.currency)"] | join(" / "))"' postJournalEntry \
    -body "{\"legal_entity_id\": \"$us\", \"document_date\": \"$end\", \"posting_date\": \"$end\", \"currency\": \"EUR\", \"header_text\": \"Engineering sold in euros\", \"lines\": [{\"account_id\": \"$(jq -r '.items[] | select(.number == "100000") | .id' "$WORK/us-accounts.json")\", \"amount\": {\"amount\": \"1000.00\", \"currency\": \"EUR\"}}, {\"account_id\": \"$(jq -r '.items[] | select(.number == "400000") | .id' "$WORK/us-accounts.json")\", \"amount\": {\"amount\": \"-1000.00\", \"currency\": \"EUR\"}}]}"
  sale=$(field .id)
  expect "the sale's local and group amounts" "$(field '.lines[0] | "\(.amount_lc.amount) \(.amount_gc.amount)"')" "1100.00 $(amount "$booked")"
  say "Carla revalues Acme Corp US's foreign-currency items at $end. The euros are worth USD 1,120.00 at the bank selling"
  say "rate, a gain of USD 20.00; group currency is valued on its own, CAD 1,500.00 against the booked $(amount "$booked"):"
  api 201 '"run \(.id[0:8])… at \(.key_date): \(.status), \(.item_count) item",
    (.totals[] | "  \(.currency) \(.amount.amount): \(.direction) \(.difference_lc.amount) \(.difference_lc.currency), in group currency \(.difference_gc.amount) \(.difference_gc.currency)")' \
    postRevaluationRun -body "{\"legal_entity_id\": \"$us\", \"key_date\": \"$end\"}"
  reval=$(field .id)
  cp "$WORK/last.json" "$WORK/euro-revaluation.json"
  expect "the euros' differences" "$(field '[.totals[] | "\(.direction) \(.amount.amount) \(.difference_lc.amount) \(.difference_gc.amount)"] | join(", ")')" \
    "gain 1000.00 20.00 $(amount "$difference")"
  say "USD 20.00 translated is not CAD $(amount "$difference"): the rest of the group difference is a translation effect, which"
  say "belongs in other comprehensive income (IAS 21.39(c)). Carla runs the group again:"
  api 202 '"operation \(.kind) \(.status): \(.resource)"' createConsolidationRun -body "{\"group_code\": \"MAPLE\", \"fiscal_year\": $YEAR, \"period\": $period}"
  run=$(field '.resource | split("/") | last')
  wait_op
  say "The reset of the gain, dated $first, is in the year's profit. Its row keeps USD 20.00 at the entry's 1.35, CAD 27.00,"
  say "where CAD $(amount "$difference") is booked:"
  api 200 '.items[] | "  \(.company_code) \(.account.number) \(.account.name): \(.amount_lc.amount) \(.amount_lc.currency), booked \(.amount_gc_booked.amount), translated \(.amount_gc.amount) \(.amount_gc.currency)"' \
    listConsolidationBalances -p "id=$run" -p "legal_entity_id=$us" -p "account_id=$(jq -r '.items[] | select(.number == "490000") | .id' "$WORK/us-accounts.json")"
  expect "the reset's row" "$(field '[.items[] | "\(.amount_lc.amount) \(.amount_gc_booked.amount) \(.amount_gc.amount)"] | join(", ")')" "20.00 $(amount "$difference") 27.00"
  expect "the part moved off the row" "$(field "[.items[] | (.amount_gc_booked | $cents) - (.amount_gc | $cents)] | add")" "$moved"
  say "The valuation itself, on $end, is a result before the group's first period: the earlier results keep its USD 20.00"
  say "at that day's rate."
  QUIET=1 api 200 '' listConsolidationBalances -p "id=$run" -p "legal_entity_id=$us" -p kind=prior_results
  expect "the part moved off the earlier results" "$(field "[.items[] | (.amount_gc_booked | $cents) - (.amount_gc | $cents)] | add")" "$earlier"
  say "The profit and loss shows what the run moved out of the profit, per company and in its totals. The profit is"
  say "CAD 27.00 more debit than the first run's, the reset as the run keeps it; as booked it would be CAD $(amount "$difference"):"
  api 200 '(.translation_reserve[] | "  \(.company_code) \(.company_name): reserve \(.closing.amount), moved out of the profit \(.revaluation_translation.amount) \(.revaluation_translation.currency)"),
    (.totals | "  total revaluation_translation: reported \(.revaluation_translation.reported.amount), eliminations \(.revaluation_translation.eliminations.amount), consolidated \(.revaluation_translation.consolidated.amount)",
      "  total profit: consolidated \(.profit.consolidated.amount); other comprehensive income \(.other_comprehensive_income.consolidated.amount)")' \
    getConsolidatedStatement -p "id=$run" -p statement=profit_and_loss -p page_size=500
  expect "what each company moved out of its profit" "$(field '[.translation_reserve[] | "\(.company_code) \(.revaluation_translation.amount)"] | join(", ")')" \
    "1000 $(amount "$moved"), $CC 0.00"
  expect "the total moved out of the profit" \
    "$(field '.totals.revaluation_translation | "\(.reported.amount) \(.eliminations.amount) \(.consolidated.amount)"')" "$(amount "$moved") 0.00 $(amount "$moved")"
  expect "the profit against the first run's" \
    "$(($(field ".totals.profit.consolidated | $cents") - $(jq -r ".totals.profit.consolidated | $cents" "$WORK/profit-and-loss.json")))" 2700
  expect "the reset's booked group amount is what the profit keeps and what the run moved" \
    "$((2700 + $(field ".totals.revaluation_translation.reported | $cents")))" \
    "$(jq -r ".totals[] | select(.direction == \"gain\") | .difference_gc | $cents" "$WORK/euro-revaluation.json")"
  QUIET=1 api 200 '' getConsolidatedStatement -p "id=$run" -p statement=balance_sheet -p page_size=500
  expect "assets, liabilities, equity and the profit, consolidated" "$(field "[.totals[].consolidated | $cents] | add")" 0
  say "Carla discards this run, cancels the revaluation and reverses the sale: the steps that follow see the month as booked."
  QUIET=1 api 200 '' getConsolidationRun -p "id=$run"
  api 200 '"run \(.id[0:8])…: \(.status)"' discardConsolidationRun -p "id=$run" -if-match "$ETAG"
  expect "the group run" "$(field .status)" discarded
  QUIET=1 api 200 '' getRevaluationRun -p "id=$reval"
  api 200 '"revaluation run \(.id[0:8])…: \(.status)"' cancelRevaluationRun -p "id=$reval" -if-match "$ETAG" \
    -body '{"reason": "Shown for the group run; the euros are revalued at the month end"}'
  expect "the revaluation run" "$(field .status)" cancelled
  QUIET=1 api 200 '' getJournalEntry -p "id=$sale"
  api 200 '"journal entry \(.document_number) of \(.posting_date): the sale reversed"' reverseJournalEntry -p "id=$sale" -if-match "$ETAG" \
    -body '{"header_text": "The sale in euros, shown for the group run"}'
}

# next_year_open has, in December only, Ada set up next fiscal year (a
# journal number range of the company for it, which creates its periods,
# closed, as year_end_setup sets up PREV) and Carla open its first period
# in the leading ledger, where a month-end revaluation's reset entry, dated
# the next day, goes (ADR-0042).
next_year_open() {
  local next=$((YEAR + 1)) base cr p
  [ "${TODAY:5:2}" = 12 ] || return 0
  as ada
  say "The revaluation's reset is dated $next-01-01: Ada sets $next up, a journal number range of $CC for it."
  QUIET=1 api 200 '' listConfigVersions -p is_active=true
  base=$(field '.items[0].number')
  jq "{title: \"Fiscal year $next\", rationale: \"The reset of December's revaluation (ADR-0042)\", base_version: $base,
    proposed: (.items[0].document | .number_ranges += [.number_ranges[] | select(.company == \"$CC\" and .document_type == \"journal_sa\" and .fiscal_year == $YEAR)
      | .fiscal_year = $next])}" "$WORK/last.json" >"$WORK/next-year-sa.json"
  api 201 '"change request \(.number), \(.status): \(.title)"' createChangeRequest -body @next-year-sa.json
  cr=$(field .id)
  api 200 '"\(.status), tier \(.tier): \([.items[].path] | join(", "))"' submitChangeRequest -p "id=$cr" -if-match "$ETAG"
  QUIET=1 api 200 '' getChangeRequest -p "id=$cr"
  api 200 '"\(.status): configuration version \(.activated_version)"' activateChangeRequest -p "id=$cr" -if-match "$ETAG"
  as carla
  QUIET=1 api 200 '' listFiscalPeriods -p "legal_entity_id=$LE" -p "fiscal_year=$next" -p "ledger_id=$LEADING" -p page_size=100
  p=$(field '.items[] | select(.period == 1) | "\(.id)/\(.version)"')
  api 200 '"period \(.fiscal_year)/\(.period): \(.status)"' openFiscalPeriod -p "id=${p%/*}" -if-match "\"${p#*/}\""
}

# UAE_COMPANY is the UAE pilot company in the configuration before its
# country pack (T62f): what a company of a document needs, its name,
# currency, country, fiscal year and time zone, the accounts its account
# determination names (the cash discount account too, as the document's
# payment terms grant discounts), named and typed as the pack names them so
# that applying the pack keeps them without a conflict, its tolerances and
# limits, a house bank (the walkthrough's illustrative IBAN, as step 41
# needs none to pay), and its VAT registration with the FTA (T61k): the
# number illustrative, the VAT 201 its return, quarterly from @FIRST@, the
# latest quarter ended (uae_first). @AE@ and @FIRST@ are replaced.
UAE_COMPANY='{"code": "@AE@", "name": "Maple Gulf Trading", "currency": "AED", "country": "AE", "fiscal_year_start_month": 1,
  "time_zone": "Asia/Dubai",
  "chart_of_accounts": [
    {"number": "1000", "name": "Bank - operating", "type": "asset"},
    {"number": "1300", "name": "VAT recoverable (input tax)", "type": "asset"},
    {"number": "2000", "name": "Accounts payable - trade", "type": "liability", "vendor_reconciliation": true},
    {"number": "4800", "name": "Purchase discounts received", "type": "revenue"}],
  "account_determination": {"ap_reconciliation": "2000", "input_tax": "1300", "bank": "1000", "cash_discount": "4800"},
  "invoice_tolerance": {"price_percent": 2, "price_absolute": "20.00"},
  "auto_post_limit": "50000.00", "manual_entry_approval_limit": "50000.00", "gr_over_delivery_percent": 0,
  "house_bank": {"name": "MAIN", "iban": "GB33BUKB20201555555555", "bic": "BUKBGB22"},
  "tax_registrations": [{"code": "FTA_VAT", "authority": "FTA", "number": "100123456789003", "return_type": "VAT201", "frequency": "quarterly",
    "first_period_start": "@FIRST@"}]}'

# governed CR WHAT submits a structural change request, evidences it (a
# regression suite of the last year's invoices, replayed under the
# proposal), has the solution architect and the controller approve it and
# activates it, as Ada.
governed() {
  local cr=$1 request
  api 200 '"\(.status), tier \(.tier): \(.items | length) changes"' submitChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "$2: the change's tier" "$(field .tier)" 4
  api 201 '"regression suite \(.number): \(.case_count) cases"' createRegressionSuite -body "{\"name\": \"Before $2\", \"window_days\": 366}"
  QUIET=1 api 200 '' getChangeRequest -p "id=$cr"
  api 202 '"\(.kind) \(.status)"' simulateChangeRequest -p "id=$cr" -if-match "$ETAG"
  wait_op
  api 200 '.evidence[] | "evidence \(.kind): \(if .passed then "passed" else "FAILED" end), \(.summary)"' getChangeRequest -p "id=$cr"
  expect "$2: status after the evidence" "$(field .status)" pending_approval
  request=$(field .workflow_request_id)
  approve sam "$request" "Structure checked"
  approve carla "$request" "Postings checked"
  as ada
  QUIET=1 api 200 '' getChangeRequest -p "id=$cr"
  api 200 '"\(.status): configuration version \(.activated_version)"' activateChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "$2: status after activation" "$(field .status)" active
}

# step_uae is the UAE country pack (T62f, ADR-0048, ADR-0049 §6 As built):
# Ada creates the UAE pilot company, proposes it in the configuration and
# applies the UAE pack to it, 2026.5 (2026.4, signed off by the tax
# specialist, with the box labels' markers dropped), so without a note
# (2026.3 brought the emirate guard: S5 accepts the seven
# emirates alone); Alex posts a UAE supplier's invoice above AED 10,000
# and one below: the supplier has no TRN on file, so the first carries the
# documentary warning of the pack's tier, no longer pending, and the
# second none; the
# company's VAT 201 of the latest quarter ended is computed, prepared,
# approved and filed (T61k, uae_return), a sale naming a mistyped emirate
# refused first.
step_uae() {
  heading "Step 41 — The UAE country pack: the UAE pilot company applies it; an invoice above AED 10,000 without the supplier's TRN is flagged; a year-end adjustment carries VAT only with its tax document; its VAT 201 is filed"
  as ada
  local cal ae base cr p supplier expense recon item invoice company=${UAE_COMPANY//@AE@/$AE}
  say "The second pilot is a UAE company (07 §7.6). The image holds the UAE pack (T62f): its VAT codes and tax"
  say "settings as ADR-0049 records them for the UAE, a starter chart, number ranges and payment terms; no holidays"
  say "(no source is recorded); 2026.2 adds the VAT 201's boxes (T61k), and 2026.3 the emirate guard: the sales code"
  say "S5 accepts the seven emirates alone, their ISO codes confirmed by the tax specialist. 2026.4 is 2026.3 signed off"
  say "by the tax specialist, Musto Ibrahim, on 2026-11-09: the VAT codes, tax settings and the VAT 201 (the items of"
  say "packs/ae/TAX-PENDING-T62f.md and TAX-PENDING-T61k.md), so production applies it; the earlier versions stay unsigned."
  say "2026.5 is 2026.4 with the VAT 201's box labels no longer marked pending the tax specialist (product owner, 2026-11-09)."
  api 200 '.items[] | select(.country == "AE") | "pack \(.country) \(.version): \(.contents.accounts) accounts, \(.contents.tax_codes) tax codes, \(.contents.holidays) holidays, \(.contents.number_ranges) number ranges; signed off: \(.signed_off)"' listCountryPacks
  expect "the UAE packs" "$(field '[.items[] | select(.country == "AE") | "\(.version) \(.contents.tax_codes) \(.signed_off)"] | join(", ")')" "2026.1 9 false, 2026.2 9 false, 2026.3 9 false, 2026.4 9 true, 2026.5 9 true"
  expect "UAE 2026.5's sign-off" "$(field '.items[] | select(.country == "AE" and .version == "2026.5") | "\(.sign_off.tax_specialist) \(.sign_off.date)"')" \
    "Musto Ibrahim 2026-11-09"
  say "Ada creates the UAE company's legal entity, Maple Gulf Trading ($AE, in AED), and activates it."
  QUIET=1 api 200 '' getLegalEntity -p "id=$LE"
  cal=$(field .fiscal_calendar_id)
  api 201 '"legal entity \(.code) \(.name): \(.country_code), \(.local_currency), \(.status)"' createLegalEntity \
    -body "{\"code\": \"$AE\", \"name\": \"Maple Gulf Trading\", \"country_code\": \"AE\", \"local_currency\": \"AED\", \"fiscal_calendar_id\": \"$cal\"}"
  ae=$(field .id)
  api 200 '"legal entity \(.code): \(.status)"' activateLegalEntity -p "id=$ae" -if-match "$ETAG"
  say "A pack is applied to a company of the configuration, so she first proposes the company: its currency,"
  say "country and time zone (Asia/Dubai), its house bank and the accounts its account determination needs, as"
  say "the pack names them, and its VAT registration with the FTA (T61g), whose return is the VAT 201 and whose first"
  say "period, the cutover, is the latest quarter ended. A new company is a structural change, tier 4, with its"
  say "evidence and two approvers."
  api 200 '"active configuration version \(.items[0].number)"' listConfigVersions -p is_active=true
  base=$(field '.items[0].number')
  jq "{title: \"The UAE company $AE\", rationale: \"The UAE pilot company (T62f)\", base_version: $base,
    proposed: (.items[0].document | .companies += [${company//@FIRST@/$(uae_first)}])}" "$WORK/last.json" >"$WORK/uae-company.json"
  api 201 '"change request \(.number), \(.status): \(.title)"' createChangeRequest -body @uae-company.json
  governed "$(field .id)" "the UAE company"
  say "Ada applies the latest UAE pack, 2026.5, to $AE for $YEAR. The merge keeps the company's accounts, which are"
  say "the pack's, and adds the rest; signed off, the pack brings no note saying otherwise:"
  api 201 '"change request \(.number), \(.status): \(.title)"' applyCountryPack \
    -body "{\"country\": \"AE\", \"version\": \"2026.5\", \"company\": \"$AE\", \"applied_on\": \"$TODAY\", \"fiscal_year\": $YEAR}"
  cr=$(field .id)
  excerpt '.notes[] | "note \(.path): \(.message)"'
  expect "notes that the pack is not signed off" "$(field '[.notes[] | select(.path == "packs['"$AE"']" and (.message | contains("signed")))] | length')" 0
  expect "conflicts with the company's own settings" "$(field '[.notes[] | select((.path | startswith("companies['"$AE"']")) and (.message | startswith("conflict")))] | length')" 0
  say "The company takes the pack's VAT codes, each with its UN/CEFACT category, and its tax settings:"
  excerpt '.proposed.companies[] | select(.code == "'"$AE"'") | (.tax_codes[] | "  \(.code) \(.direction) \(.kind) \(.category // "-"): \(.description)"),
    (.tax_settings | "  rate type \(.rate_type), rounding per \(.rounding)", (.documentary_requirements[] | "  above AED \(.above): \(.requires | join(", ")), pending sign-off \(.pending_sign_off // false) (\(.source))"))'
  expect "the documentary tier" "$(field '.proposed.companies[] | select(.code == "'"$AE"'") | .tax_settings.documentary_requirements | map("\(.above) \(.requires | join(" ")) \(.pending_sign_off // false)") | join(", ")')" \
    "10000 supplier_tax_id false"
  say "S5, the sales code levying the emirate each line names, lists the emirates it accepts (the emirate guard):"
  excerpt '.proposed.companies[] | select(.code == "'"$AE"'") | .tax_codes[] | select(.code == "S5") | .components[] | "  S5 \(.key): \(.subdivisions | join(", "))"'
  expect "the emirates S5 accepts" "$(field '.proposed.companies[] | select(.code == "'"$AE"'") | .tax_codes[] | select(.code == "S5") | .components[0].subdivisions | join(" ")')" \
    "AE-AZ AE-DU AE-SH AE-AJ AE-UQ AE-RK AE-FU"
  governed "$cr" "the UAE pack"
  say "The pack's number ranges gave $AE the periods of $YEAR, closed; Ada opens the one of today."
  QUIET=1 api 200 '' listFiscalPeriods -p "legal_entity_id=$ae" -p "fiscal_year=$YEAR" -p page_size=100
  for p in $(jq -r --arg d "$TODAY" '.items[] | select((.is_special | not) and .start_date <= $d and .end_date >= $d and .status == "closed") | "\(.id)/\(.version)"' "$WORK/last.json"); do
    api 200 '"period \(.fiscal_year)/\(.period) \(.start_date) to \(.end_date): \(.status)"' openFiscalPeriod -p "id=${p%/*}" -if-match "\"${p#*/}\""
  done
  expect "today's period of $AE open" "$(field .status)" open
  as tom
  say "The tenant's currency is CAD, and the ledger converts $AE's postings into it too: Tom records an AED"
  say "rate, an illustrative one for the walkthrough, not a published rate."
  api 201 '"\(.from_currency)/\(.to_currency) \(.rate) from \(.valid_from) (\(.source))"' createExchangeRate \
    -body "{\"rate_type\": \"average\", \"from_currency\": \"AED\", \"to_currency\": \"CAD\", \"valid_from\": \"$YEAR-01-01\", \"rate\": \"0.37\", \"source\": \"walkthrough\"}"
  as alex
  QUIET=1 api 200 '' listAccounts -p "legal_entity_id=$ae" -p page_size=200
  expense=$(field '.items[] | select(.number == "6000") | .id')
  recon=$(field '.items[] | select(.number == "2000") | .id')
  say "Alex sets up a UAE supplier for $AE, Gulf Office Interiors. Its TRN has not been received, so none is on"
  say "file; Carla, who approves supplier master data, approves it."
  api 201 '"supplier \(.name), \(.country_code), \(.status)"' createBusinessPartner \
    -body "{\"kind\": \"organization\", \"name\": \"Gulf Office Interiors\", \"country_code\": \"AE\", \"roles\": [\"supplier\"], \"supplier_companies\": [{\"legal_entity_id\": \"$ae\", \"reconciliation_account_id\": \"$recon\", \"payment_term_code\": \"NT30\"}], \"reason\": \"Office fit-out supplier of the UAE company\"}"
  supplier=$(field .id)
  QUIET=1 api 200 '' listPartnerChanges -p "partner_id=$supplier"
  approve carla "$(field '.items[0].workflow_request_id')" "Supplier confirmed by call-back"
  as alex
  say "Its tax invoice for office furniture: AED 12,000.00 plus 600.00 VAT on V5, the pack's standard purchase code,"
  say "AED 12,600.00 in all. Posting calculates the VAT at the code's 5% and checks the documentary requirement the"
  say "pack brings (Executive Regulation Art. 59, as ADR-0049 records it): above AED 10,000 the supplier's TRN, a tax"
  say "identifier of the UAE that the supplier does not have. A warning, signed off with the pack; not a refusal:"
  item="{\"account_id\": \"$expense\", \"description\": \"Office furniture\", \"quantity\": {\"value\": \"1\", \"unit\": \"EA\"}, \"amount\": {\"amount\": \"12000.00\", \"currency\": \"AED\"}, \"tax_code\": \"V5\"}"
  api 201 '"\(.kind) \(.supplier_reference): \(.status), \(.gross_amount.amount) \(.gross_amount.currency)"' createSupplierInvoice \
    -body "{\"legal_entity_id\": \"$ae\", \"supplier_id\": \"$supplier\", \"supplier_reference\": \"GOI-1001\", \"invoice_date\": \"$TODAY\", \"gross_amount\": {\"amount\": \"12600.00\", \"currency\": \"AED\"}, \"tax_amount\": {\"amount\": \"600.00\", \"currency\": \"AED\"}, \"items\": [$item]}"
  invoice=$(field .id)
  api 200 '"\(.document_number): \(.status), journal entry \(.journal_entry_id)", (.documentary_warnings[] | "warning \(.code): above \(.threshold.amount) \(.threshold.currency), pending sign-off \(.pending_sign_off), \(.source)")' \
    postSupplierInvoice -p "id=$invoice" -if-match "$ETAG" -body "{\"posting_date\": \"$TODAY\"}"
  expect "the invoice's status" "$(field .status)" posted
  expect "the invoice's documentary warnings" \
    "$(field '[.documentary_warnings[] | "\(.code) \(.threshold.amount) \(.threshold.currency) \(.threshold_inclusive) \(.pending_sign_off)"] | join(", ")')" \
    "supplier_tax_id_missing 10000.00 AED false false"
  say "Its next invoice, AED 4,000.00 plus 200.00 VAT, is below the threshold: no warning."
  api 201 '"\(.kind) \(.supplier_reference): \(.status), \(.gross_amount.amount) \(.gross_amount.currency)"' createSupplierInvoice \
    -body "{\"legal_entity_id\": \"$ae\", \"supplier_id\": \"$supplier\", \"supplier_reference\": \"GOI-1002\", \"invoice_date\": \"$TODAY\", \"gross_amount\": {\"amount\": \"4200.00\", \"currency\": \"AED\"}, \"tax_amount\": {\"amount\": \"200.00\", \"currency\": \"AED\"}, \"items\": [${item//12000.00/4000.00}]}"
  invoice=$(field .id)
  api 200 '"\(.document_number): \(.status), \(.documentary_warnings | length) documentary warnings"' \
    postSupplierInvoice -p "id=$invoice" -if-match "$ETAG" -body "{\"posting_date\": \"$TODAY\"}"
  expect "the smaller invoice's status" "$(field .status)" posted
  expect "the smaller invoice's documentary warnings" "$(field '.documentary_warnings | length')" 0
  uae_return "$ae" "$supplier" "$expense"
  passed
}

# uae_year_end_adjustment AE BANK SALES VAT END is a UAE year-end
# adjustment carrying VAT (ADR-0060 §3 and question 2; the UAE tax
# specialist's answer C10 of 2026-11-09): Ada sets PREV up for the UAE
# company, Tom records an AED rate for its last month and Carla opens its
# special period 13. An adjustment with a VAT code is refused there without
# a tax document; naming the tax credit note it rests on, dated END, the
# quarter's last day, it posts into PREV/13 with the credit note's date as
# its tax date, so uae_return's VAT 201 of that quarter reports it. Carla
# closes the period again.
uae_year_end_adjustment() {
  local ae=$1 bank=$2 sales=$3 vat=$4 end=$5 base cr p lines
  as ada
  say "The auditors of Maple Gulf Trading ask for an adjustment to $PREV: AED 100.00 of Dubai sales credited to a"
  say "customer, with its 5.00 of VAT. Ada sets $PREV up for $AE, a journal number range like any other change:"
  QUIET=1 api 200 '' listConfigVersions -p is_active=true
  base=$(field '.items[0].number')
  jq "{title: \"Fiscal year $PREV of $AE\", rationale: \"A year-end adjustment of $PREV (ADR-0060)\", base_version: $base,
    proposed: (.items[0].document | .number_ranges += [.number_ranges[] | select(.company == \"$AE\" and .document_type == \"journal_sa\" and .fiscal_year == $YEAR)
      | .fiscal_year = $PREV])}" "$WORK/last.json" >"$WORK/uae-prev-year.json"
  api 201 '"change request \(.number), \(.status): \(.title)"' createChangeRequest -body @uae-prev-year.json
  cr=$(field .id)
  api 200 '"\(.status), tier \(.tier): \([.items[].path] | join(", "))"' submitChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "the change" "$(field '[.items[].path] | join(" ")')" "number_ranges[$AE/journal_sa/$PREV]"
  QUIET=1 api 200 '' getChangeRequest -p "id=$cr"
  api 200 '"\(.status): configuration version \(.activated_version)"' activateChangeRequest -p "id=$cr" -if-match "$ETAG"
  as tom
  api 201 '"\(.from_currency)/\(.to_currency) \(.rate) from \(.valid_from) (\(.source))"' createExchangeRate \
    -body "{\"rate_type\": \"average\", \"from_currency\": \"AED\", \"to_currency\": \"CAD\", \"valid_from\": \"$PREV-12-01\", \"rate\": \"0.37\", \"source\": \"walkthrough\"}"
  as carla
  say "Carla opens special period 13 of $PREV, which shares December's dates:"
  QUIET=1 api 200 '' listFiscalPeriods -p "legal_entity_id=$ae" -p "fiscal_year=$PREV" -p page_size=100
  for p in $(field '.items[] | select(.period == 13) | "\(.id)/\(.version)"'); do
    api 200 '"  period \(.fiscal_year)/\(.period) (special: \(.is_special)): \(.status)"' openFiscalPeriod -p "id=${p%/*}" -if-match "\"${p#*/}\""
  done
  lines="{\"account_id\": \"$bank\", \"amount\": {\"amount\": \"-105.00\", \"currency\": \"AED\"}}, {\"account_id\": \"$sales\", \"amount\": {\"amount\": \"100.00\", \"currency\": \"AED\"}, \"tax_code\": \"S5\", \"tax_jurisdiction\": \"AE-DU\"}, {\"account_id\": \"$vat\", \"amount\": {\"amount\": \"5.00\", \"currency\": \"AED\"}, \"tax_code\": \"S5\", \"tax_jurisdiction\": \"AE-DU\"}"
  # uae_adjustment FIELDS is the adjustment's entry, dated the year's last
  # day into period 13, with the fields that date its tax.
  uae_adjustment() {
    printf '{"legal_entity_id": "%s", "document_date": "%s-12-31", "posting_date": "%s-12-31", "fiscal_period": 13, %s"currency": "AED", "header_text": "Year-end adjustment: Dubai sales credited", "lines": [%s]}' \
      "$ae" "$PREV" "$PREV" "$1, " "$lines"
  }
  say "A year-end adjustment carries VAT only when it rests on a tax invoice or tax credit note (the UAE tax"
  say "specialist, 2026-11-09): accruals, provisions and reclassifications carry none. Without one it is refused,"
  say "whatever tax date it names:"
  refused 422 ledger.special_period_tax_code postJournalEntry -body "$(uae_adjustment "\"tax_date\": \"$end\"")"
  expect "the refused field" "$(field '[.errors[].field] | join(" ")')" "lines[1].tax_code"
  # Built here, not inside the substitution: bash 3.2 expands the braces of
  # a quoted word nested in "$(...)".
  local document="\"tax_document\": {\"kind\": \"tax_credit_note\", \"number\": \"TCN-$PREV-0042\", \"date\": \"$end\"}"
  say "Carla names the tax credit note issued to the customer on $end. Its date is the entry's tax date: the VAT"
  say "belongs to the return of the period the credit note is dated in, not to December $PREV:"
  api 201 '"journal entry \(.document_number) of \(.posting_date), period \(.fiscal_year)/\(.fiscal_period), tax date \(.tax_date), \(.tax_document.kind) \(.tax_document.number) of \(.tax_document.date): \(.header_text)"' \
    postJournalEntry -body "$(uae_adjustment "$document")"
  expect "the adjustment's period, tax date and tax document" "$(field '"\(.fiscal_year)/\(.fiscal_period) \(.tax_date) \(.tax_document.kind) \(.tax_document.number) \(.tax_document.date)"')" \
    "$PREV/13 $end tax_credit_note TCN-$PREV-0042 $end"
  QUIET=1 api 200 '' listFiscalPeriods -p "legal_entity_id=$ae" -p "fiscal_year=$PREV" -p page_size=100
  for p in $(field '.items[] | select(.period == 13) | "\(.id)/\(.version)"'); do
    api 200 '"  period \(.fiscal_year)/\(.period): \(.status)"' closeFiscalPeriod -p "id=${p%/*}" -if-match "\"${p#*/}\""
  done
}

# uae_return AE SUPPLIER EXPENSE is step 41's VAT 201 (T61k) of legal
# entity AE's latest quarter ended: Carla posts its sales in Dubai and Abu
# Dhabi, a zero-rated export, an exempt supply and the import VAT of a
# customs declaration, and Alex the supplier's invoices of the domestic
# reverse charge and of blocked VAT, each dated the quarter's last day;
# Carla posts a year-end adjustment of PREV resting on a tax credit note of
# that day (uae_year_end_adjustment), which the return reports; she
# computes the draft and drills box 1b down, prepares the return,
# Fiona approves it and Carla records it filed.
uae_return() {
  local ae=$1 supplier=$2 expense=$3 registration start end bank sales vat input ret request
  as carla
  QUIET=1 api 200 '' listAccounts -p "legal_entity_id=$ae" -p page_size=200
  bank=$(field '.items[] | select(.number == "1000") | .id')
  sales=$(field '.items[] | select(.number == "4000") | .id')
  vat=$(field '.items[] | select(.number == "2300") | .id')
  input=$(field '.items[] | select(.number == "1300") | .id')
  say "Maple Gulf Trading's VAT is filed under its FTA registration (the number illustrative), its return the VAT 201"
  say "that UAE pack 2026.5 defines; its first period is the latest quarter ended, the one it files now:"
  api 200 '.items[] | "registration \(.code): \(.authority), number ending \(.number_last4), return \(.return_type), \(.frequency) from \(.first_period_start), company \(.company_code)"' \
    listTaxRegistrations -p "legal_entity_id=$ae"
  expect "the UAE registration" "$(field '.items[0] | "\(.code) \(.return_type) \(.first_period_start)"')" "FTA_VAT VAT201 $(uae_first)"
  registration=$(field '.items[0].id')
  QUIET=1 api 200 '' listTaxPeriods -p "id=$registration"
  start=$(field '.items[0].start_date')
  end=$(field '.items[0].end_date')
  expect "the time zone of its periods" "$(field .time_zone)" Asia/Dubai
  # uae_entry TEXT LINES is a journal entry of AE in AED posted today,
  # its document and tax date the quarter's last day.
  uae_entry() {
    printf '{"legal_entity_id": "%s", "document_date": "%s", "posting_date": "%s", "tax_date": "%s", "currency": "AED", "header_text": "%s", "lines": [%s]}' \
      "$ae" "$end" "$TODAY" "$end" "$1" "$2"
  }
  # uae_line ACCOUNT AMOUNT [CODE [EMIRATE [DECLARED BASE]]] is a line of it.
  uae_line() {
    local base=''
    if [ -n "${5:-}" ]; then base=", \"tax_base\": {\"amount\": \"$5\", \"currency\": \"AED\"}"; fi
    printf '{"account_id": "%s", "amount": {"amount": "%s", "currency": "AED"}%s%s%s}' "$1" "$2" "${3:+, \"tax_code\": \"$3\"}" \
      "${4:+, \"tax_jurisdiction\": \"$4\"}" "$base"
  }
  local entry='"journal entry \(.document_number), tax date \(.tax_date): \([.lines[] | "\(.amount.amount)\(if .tax_code then " \(.tax_code)" else "" end)\(if .tax_jurisdiction then "@\(.tax_jurisdiction)" else "" end)"] | join(" / "))"'
  say "Its sales for $start to $end, entered today with the quarter's last day as their tax date (P6): standard-rated"
  say "sales name the emirate of the establishment that made them (S5 levies the line's emirate, ADR-0049 §7), so the"
  say "VAT 201 reports them by emirate (boxes 1a-1g); a zero-rated export (Z0) and an exempt supply (E0):"
  api 201 "$entry" postJournalEntry -body "$(uae_entry "Sales of the Dubai branch" "$(uae_line "$bank" 10500.00), $(uae_line "$sales" -10000.00 S5 AE-DU), $(uae_line "$vat" -500.00 S5 AE-DU)")"
  api 201 "$entry" postJournalEntry -body "$(uae_entry "Sales of the Abu Dhabi branch" "$(uae_line "$bank" 4200.00), $(uae_line "$sales" -4000.00 S5 AE-AZ), $(uae_line "$vat" -200.00 S5 AE-AZ)")"
  say "A sale keyed with a mistyped emirate, AE-DB for Dubai, is refused: S5 accepts the seven emirates alone (the"
  say "emirate guard), so its VAT cannot fall outside boxes 1a-1g unreported:"
  refused 422 ledger.jurisdiction_not_levied postJournalEntry \
    -body "$(uae_entry "Sales of the Dubai branch, mistyped" "$(uae_line "$bank" 1050.00), $(uae_line "$sales" -1000.00 S5 AE-DB), $(uae_line "$vat" -50.00 S5 AE-DB)")"
  api 201 "$entry" postJournalEntry -body "$(uae_entry "Export, zero-rated" "$(uae_line "$bank" 3000.00), $(uae_line "$sales" -3000.00 Z0)")"
  api 201 "$entry" postJournalEntry -body "$(uae_entry "Exempt supply" "$(uae_line "$bank" 1500.00), $(uae_line "$sales" -1500.00 E0)")"
  say "Goods imported through customs: the declaration's AED 10,000.00, its import VAT of 500.00 accounted for in the"
  say "return, owed and recovered (IM5, ADR-0049 §9), an entry of tax lines that name the declared value:"
  api 201 "$entry" postJournalEntry -body "$(uae_entry "Customs declaration: import VAT" "$(uae_line "$input" 500.00 IM5 "" 10000.00), $(uae_line "$vat" -500.00 IM5 "" 10000.00)")"
  as alex
  say "Alex posts the supplier's invoices of the quarter's last day: mobile phones bought for resale, AED 2,000.00 under"
  say "the domestic reverse charge (DR5: Maple Gulf owes and recovers the 100.00), and staff entertainment, AED 1,000.00"
  say "with 50.00 of VAT that is blocked (VB5, Executive Regulation Art. 53 as ADR-0049 records it):"
  local ref amount gross tax code
  for ref in "GOI-1003 2000.00 2000.00 0.00 DR5" "GOI-1004 1000.00 1050.00 50.00 VB5"; do
    read -r ref amount gross tax code <<<"$ref"
    api 201 '"\(.kind) \(.supplier_reference) of \(.invoice_date): \(.status), \(.gross_amount.amount) \(.gross_amount.currency)"' createSupplierInvoice \
      -body "{\"legal_entity_id\": \"$ae\", \"supplier_id\": \"$supplier\", \"supplier_reference\": \"$ref\", \"invoice_date\": \"$end\", \"gross_amount\": {\"amount\": \"$gross\", \"currency\": \"AED\"}, \"tax_amount\": {\"amount\": \"$tax\", \"currency\": \"AED\"}, \"items\": [{\"account_id\": \"$expense\", \"description\": \"$code purchase\", \"quantity\": {\"value\": \"1\", \"unit\": \"EA\"}, \"amount\": {\"amount\": \"$amount\", \"currency\": \"AED\"}, \"tax_code\": \"$code\"}]}"
    api 200 '"\(.document_number): \(.status)"' postSupplierInvoice -p "id=$(field .id)" -if-match "$ETAG" -body "{\"posting_date\": \"$TODAY\"}"
  done
  uae_year_end_adjustment "$ae" "$bank" "$sales" "$vat" "$end"
  as carla
  say "Carla computes the draft VAT 201 (ADR-0049 §11): each box sums the items the pack maps to it. The boxes are"
  say "ADR-0049's record of the FTA's guide through secondary summaries, signed off by the tax specialist with the pack"
  say "(packs/ae/TAX-PENDING-T61k.md); 2026.5's labels carry no marker of the unsigned versions. Boxes not zero:"
  api 200 '"draft \(.return_type) of \(.registration_code), \(.period_start) to \(.period_end), \(.currency), pack \(.pack.country) \(.pack.version): \(.item_count) items", (.boxes[] | select(.amount.amount != "0.00") | "  \(.code) \(.amount.amount): \(.label)")' \
    getTaxReturnDraft -p "id=$registration" -p "period_start=$start"
  expect "the draft's pack" "$(field '"\(.pack.country) \(.pack.version)"')" "AE 2026.5"
  expect "boxes 1b and 8's VAT labels, without a marker" "$(field '[.boxes[] | select(.code == "1b_vat" or .code == "8_vat") | .label] | join(" / ")')" \
    "1b Standard-rated supplies in Dubai (AE-DU): VAT / 8 Totals: VAT, boxes 1a-1g, 2, 3, 6 and 7"
  expect "the boxes in the pack's order" "$(field '[.boxes[].code] | .[0:3] + .[-3:] | join(" ")')" "1a_amount 1a_vat 1a_adjustment 12 13 14"
  expect "boxes 1a and 1b, by emirate" "$(field '[.boxes[] | select(.code | test("^1[ab]_(amount|vat)$")) | .amount.amount] | join(" ")')" "4000.00 200.00 9900.00 495.00"
  expect "boxes 3, 4, 5 and 6" "$(field '[.boxes[] | select(.code | test("^[3-6]_")) | .amount.amount] | join(" ")')" "2000.00 100.00 3000.00 1500.00 10000.00 500.00"
  expect "boxes 9 and 10" "$(field '[.boxes[] | select(.code | test("^(9|10)_")) | .amount.amount] | join(" ")')" "1000.00 0.00 0.00 600.00"
  expect "boxes 12, 13 and 14: due, recoverable and payable" "$(field '[.boxes[] | select(.code | test("^1[234]$")) | .amount.amount] | join(" ")')" "1295.00 600.00 695.00"
  say "Box 1b drills down to the Dubai branch's sales and the year-end adjustment of $PREV, dated by its credit note:"
  api 200 '.items[] | "  \(.tax_date) \(.tax_code)/\(.component) \(.jurisdiction // ""): adds \(.box_amount.amount), entry \(.document_number): \(.header_text // "")"' \
    listTaxReturnDraftItems -p "id=$registration" -p "period_start=$start" -p box=1b_vat
  expect "box 1b's items" "$(field '[.items[] | "\(.header_text) \(.box_amount.amount)"] | join(", ")')" "Year-end adjustment: Dubai sales credited -5.00, Sales of the Dubai branch 500.00"
  local body="{\"period_start\": \"$start\"}"
  say "Carla prepares the return once the quarter's fiscal periods are closed, which claims its items:"
  close_tax_periods "$start" "$end" "$body" "$registration" "$ae"
  api 201 '"return \(.return_type) of \(.registration_code), \(.period_start) to \(.period_end): \(.status), \(.item_count) items", (.boxes[] | select(.amount.amount != "0.00") | "  \(.code) \(.amount.amount)")' \
    prepareTaxReturn -p "id=$registration" -body "$body"
  expect "the return's box 14" "$(field '.boxes[] | select(.code == "14") | .amount.amount')" "695.00"
  ret=$(field .id)
  request=$(field .approval_request_id)
  approve fiona "$request" "Boxes agree with the VAT accounts"
  as carla
  say "Carla files it on EmaraTax herself (P3) and records the portal's confirmation, illustrative here:"
  QUIET=1 api 200 '' getTaxReturn -p "id=$ret"
  api 200 '"return \(.status): confirmation \(.filing.confirmation_number) of \(.filing.filed_on)"' fileTaxReturn -p "id=$ret" -if-match "$ETAG" \
    -body "{\"confirmation_number\": \"WALKTHROUGH-AE-0001\", \"filed_on\": \"$TODAY\"}"
  expect "the VAT 201's status" "$(field .status)" filed
  QUIET=1 api 200 '' getTaxReturnDraft -p "id=$registration" -p "period_start=$start"
  expect "the quarter's draft after filing" "$(field .item_count)" 0
}

# step_einvoice is fetching e-invoices on demand (T65a; ADR-0061 §3, §4,
# §15): Ada connects the UAE company's accredited e-invoicing provider, an
# einvoice_provider connector to the provider's stand-in on this machine
# (the only adapter until the product owner chooses a provider, refused in
# production); the stand-in holds a supplier's e-invoice addressed to the
# company's participant; Alex fetches, finds it in the list received under
# the fetch's operation, its bytes the stand-in's, acknowledged; the
# stand-in loses the acknowledgement and lists it again, and a second
# fetch counts it as already recorded and acknowledges it again. T65b's
# (ADR-0061 §4, §8): Alex downloads the e-invoice as received, its SHA-256
# the stand-in's; the stand-in delivers it again under its id with other
# bytes, and a fetch counts it as changed, the document keeping its first
# bytes and naming the new ones' hash; the same bytes delivered once more
# are not a change again.
step_einvoice() {
  heading "Step 43 — E-invoices fetched: the UAE company's provider connected, a supplier's e-invoice recorded once and acknowledged, downloaded as received, a redelivery with other bytes recorded as its change once"
  local connector op doc sum changed
  as ada
  say "The UAE company $AE receives its suppliers' e-invoices through an accredited provider (ADR-0049, ADR-0061). No"
  say "provider is chosen yet (ADR-0061 Q1), so Ada connects the provider's stand-in on this machine: an einvoice_provider"
  say "connector, its company's participant identifier at the provider (illustrative; its scheme is to confirm), the"
  say "format PINT AE, and the credentials named by secret_ref. Production refuses the stand-in"
  say "(integration.adapter_not_certified); the ERP polls the provider and opens no endpoint for it to call."
  api 201 '"connector \(.name): \(.kind), \(.status), adapter \(.settings.adapter), \(.settings.format), participants \(.settings.participants | to_entries | map("\(.key) \(.value)") | join(", "))"' \
    createConnector -body "{\"kind\": \"einvoice_provider\", \"name\": \"Maple Gulf e-invoicing\", \"secret_ref\": \"file:$WORK/provider/token\", \"settings\": {\"adapter\": \"standin\", \"base_url\": \"https://$PROVIDER_ADDR\", \"participants\": {\"$AE\": \"walkthrough:$AE\"}, \"format\": \"pint_ae\"}}"
  connector=$(field .id)
  say "Gulf Office Interiors sends its next invoice as an e-invoice; the provider holds it for Maple Gulf Trading's"
  say "participant (an illustrative document: what it says is read when e-invoices are validated, T65c):"
  {
    printf '<?xml version="1.0" encoding="UTF-8"?>\n'
    printf '<!-- Illustrative only: not a validated PINT AE document. -->\n'
    printf '<Invoice xmlns="urn:oasis:names:specification:ubl:schema:xsd:Invoice-2"><ID>GOI-2001</ID><IssueDate>%s</IssueDate></Invoice>\n' "$TODAY"
  } >"$WORK/provider/inbox/GOI-2001.xml"
  printf 'walkthrough:%s\n' "$AE" >"$WORK/provider/inbox/GOI-2001.receiver"
  sum=$(shasum -a 256 "$WORK/provider/inbox/GOI-2001.xml" | cut -d' ' -f1)
  say "  message GOI-2001 for walkthrough:$AE, SHA-256 ${sum:0:16}…"
  as alex
  say "Alex fetches now: the fetch is an Operation; it records each document the provider holds once, keeps its bytes"
  say "write-once, and tells the provider it is kept after the commit."
  api 202 '"operation \(.kind): \(.status)"' fetchEInvoices -body "{\"connector_id\": \"$connector\"}"
  wait_op
  op=$(field .id)
  excerpt '"operation \(.status): recorded \(.result.recorded), already recorded \(.result.already_recorded); its documents at \(.resource)"'
  expect "the fetch's counts" "$(field '"\(.result.recorded) \(.result.already_recorded)"')" "1 0"
  api 200 '.items[] | "  \(.message_id) for \(.company_code) \(.company_name): \(.status), \(.size) bytes, received through \(.connector_name), acknowledged \(.acknowledged_at != null)"' \
    listEInvoices -p "fetch_operation_id=$op"
  expect "the fetch's documents" "$(field '[.items[] | "\(.message_id) \(.company_code) \(.status)"] | join(", ")')" "GOI-2001 $AE received"
  expect "its bytes, the stand-in's" "$(field '.items[0].sha256')" "$sum"
  expect "acknowledged to the provider" "$(field '.items[0].acknowledged_at != null')" true
  doc=$(field '.items[0].id')
  [ -s "$WORK/provider/inbox/GOI-2001.acknowledged" ] || fail "the stand-in recorded no acknowledgement of GOI-2001"
  say "The provider loses the acknowledgement and lists GOI-2001 again; the next fetch does not record it twice, and"
  say "acknowledges it again:"
  rm "$WORK/provider/inbox/GOI-2001.acknowledged"
  api 202 '"operation \(.kind): \(.status)"' fetchEInvoices -body "{\"connector_id\": \"$connector\"}"
  wait_op
  excerpt '"operation \(.status): recorded \(.result.recorded), already recorded \(.result.already_recorded)"'
  expect "the second fetch's counts" "$(field '"\(.result.recorded) \(.result.already_recorded)"')" "0 1"
  [ -s "$WORK/provider/inbox/GOI-2001.acknowledged" ] || fail "the second fetch did not acknowledge GOI-2001 again"
  api 200 '"  \(.message_id): \(.status), recorded by the first fetch \(.fetch_operation_id == "'"$op"'")"' getEInvoice -p "id=$doc"
  expect "still the first fetch's" "$(field .fetch_operation_id)" "$op"
  QUIET=1 api 200 '' listEInvoices -p "connector_id=$connector"
  expect "the connector's documents" "$(field '.items | length')" 1
  einvoice_original "$connector" "$doc" "$sum"
  passed
}

# einvoice_original is T65b's part of step_einvoice: the e-invoice
# downloaded as received, and the provider's redelivery under its id with
# other bytes recorded as its change, once.
einvoice_original() {
  local connector=$1 doc=$2 sum=$3 changed op
  say "Alex downloads GOI-2001 as received, the legal original kept write-once; its SHA-256 is the stand-in's, and the"
  say "server compares it with the hash recorded on every read:"
  api 200 '' getEInvoiceContent -p "id=$doc" -out "$WORK/provider/GOI-2001.downloaded.xml"
  say "  $(wc -c <"$WORK/provider/GOI-2001.downloaded.xml" | tr -d ' ') bytes, SHA-256 $(shasum -a 256 "$WORK/provider/GOI-2001.downloaded.xml" | cut -c1-16)…"
  expect "the download's SHA-256" "$(shasum -a 256 "$WORK/provider/GOI-2001.downloaded.xml" | cut -d' ' -f1)" "$sum"
  say "The provider delivers GOI-2001 again under the same id with other bytes (the supplier changed it after sending). The"
  say "next fetch does not replace the original: it records the new bytes' hash as the document's change, once, for a person"
  say "to take up with the supplier:"
  sed 's/<ID>GOI-2001<\/ID>/<ID>GOI-2001<\/ID><Note>Corrected<\/Note>/' "$WORK/provider/GOI-2001.downloaded.xml" >"$WORK/provider/inbox/GOI-2001.xml"
  rm "$WORK/provider/inbox/GOI-2001.acknowledged"
  changed=$(shasum -a 256 "$WORK/provider/inbox/GOI-2001.xml" | cut -d' ' -f1)
  [ "$changed" != "$sum" ] || fail "the redelivered GOI-2001 has the same bytes"
  api 202 '"operation \(.kind): \(.status)"' fetchEInvoices -body "{\"connector_id\": \"$connector\"}"
  wait_op
  excerpt '"operation \(.status): recorded \(.result.recorded), already recorded \(.result.already_recorded), changed \(.result.changed)"'
  expect "the redelivery's counts" "$(field '"\(.result.recorded) \(.result.already_recorded) \(.result.changed)"')" "0 0 1"
  op=$(field .id)
  say "The fetch's documents, its Operation's resource, are those it recorded or found changed:"
  api 200 '.items[] | "  \(.message_id): \(.status), its bytes \(.sha256[0:16])…, changed to \(.changed_sha256[0:16])… at \(.changed_at) by this fetch \(.changed_fetch_operation_id == "'"$op"'")"' \
    listEInvoices -p "fetch_operation_id=$op"
  expect "the fetch's changed documents" "$(field '[.items[].id] | join(" ")')" "$doc"
  expect "the bytes kept" "$(field '.items[0].sha256')" "$sum"
  expect "the change recorded" "$(field '.items[0].changed_sha256')" "$changed"
  expect "the fetch that found it" "$(field '.items[0].changed_fetch_operation_id')" "$op"
  QUIET=1 api 200 '' getEInvoiceContent -p "id=$doc" -out "$WORK/provider/GOI-2001.downloaded-again.xml"
  expect "the download after the change" "$(shasum -a 256 "$WORK/provider/GOI-2001.downloaded-again.xml" | cut -d' ' -f1)" "$sum"
  say "The provider loses that acknowledgement too and delivers the same changed bytes again; it is not a change again:"
  rm "$WORK/provider/inbox/GOI-2001.acknowledged"
  api 202 '"operation \(.kind): \(.status)"' fetchEInvoices -body "{\"connector_id\": \"$connector\"}"
  wait_op
  excerpt '"operation \(.status): recorded \(.result.recorded), already recorded \(.result.already_recorded), changed \(.result.changed)"'
  expect "the same changed bytes' counts" "$(field '"\(.result.recorded) \(.result.already_recorded) \(.result.changed)"')" "0 1 0"
  QUIET=1 api 200 '' getEInvoice -p "id=$doc"
  expect "still the one change" "$(field .changed_sha256)" "$changed"
}

# step_year_end is the year-end close of the fiscal year before today's,
# PREV (ADR-0060): the walkthrough's configuration has no number range for
# it until here, so no other step posts into it. Each slice of T64m adds
# its part in order; T64m1's: PREV set up through a change request, the
# business the later parts carry forward (an expense, a revenue, a USD
# bank posting, the USD rates on PREV's last day and PREV's year-end
# revaluation in every ledger), and an audit adjustment posted into
# special period 13, dated the year's last day. T64m2's: the retained
# earnings account set through a change request, PREV's periods closed
# and PREV carried forward into this year in every ledger. T64m3's: the
# runs listed and their balances read, a late adjustment into period 13
# making them stale, and the re-run superseding them. T64m4's: the year
# tasks through a change request, PREV's year close opened and a manual
# task of it assigned and completed. T64m5's: the year close's checks run,
# the carry-forward check failing after one more late adjustment and
# passing after the re-run. T64m6's: the year signed off, refused while
# its regular periods are not locked, then locking its special periods, so
# that every period of PREV is locked.
step_year_end() {
  heading "Step 42 — Year-end close of $PREV: the year set up, its business and year-end revaluation, an audit adjustment in special period 13, the year carried forward, a late adjustment and the re-run, the year close's checklist, its checks and its sign-off locking the year"
  year_end_setup
  year_end_business
  year_end_revaluation
  year_end_adjustment
  year_end_retained_earnings
  year_end_carry_forward
  year_end_carry_forward_balances
  year_end_late_adjustment
  year_end_rerun
  year_end_close
  year_end_checks
  year_end_sign_off
  passed
}

# year_end_setup has Ada set up PREV through a change request: a journal
# number range of the company for it, which creates its periods, closed,
# in every ledger the company keeps (ADR-0045); Carla opens period 12.
year_end_setup() {
  as ada
  say "Maple's books of $PREV were kept in SAP and its balances migrated (step 3); the auditors' adjustments to $PREV"
  say "are entered here. Ada sets $PREV up: a journal number range of $CC for it, a change request like any other."
  api 200 '"active configuration version \(.items[0].number)"' listConfigVersions -p is_active=true
  local base cr
  base=$(field '.items[0].number')
  jq "{title: \"Fiscal year $PREV\", rationale: \"The year-end close of $PREV (ADR-0060)\", base_version: $base,
    proposed: (.items[0].document | .number_ranges += [.number_ranges[] | select(.company == \"$CC\" and .document_type == \"journal_sa\" and .fiscal_year == $YEAR)
      | .fiscal_year = $PREV])}" "$WORK/last.json" >"$WORK/prev-year.json"
  api 201 '"change request \(.number), \(.status): \(.title)"' createChangeRequest -body @prev-year.json
  cr=$(field .id)
  api 200 '"\(.status), tier \(.tier): \([.items[].path] | join(", "))"' submitChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "the change" "$(field '[.items[].path] | join(" ")')" "number_ranges[$CC/journal_sa/$PREV]"
  QUIET=1 api 200 '' getChangeRequest -p "id=$cr"
  api 200 '"\(.status): configuration version \(.activated_version)"' activateChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "status after activation" "$(field .status)" active
  as carla
  say "$PREV's periods exist now in each ledger, closed. Carla opens period 12, December, in each, for the year's"
  say "last business and its revaluation:"
  QUIET=1 api 200 '' listFiscalPeriods -p "legal_entity_id=$LE" -p "fiscal_year=$PREV" -p page_size=100
  expect "$PREV's periods in 0L and 2L, 12 regular and 4 special each" \
    "$(field '"\([.items[] | select(.is_special | not)] | length) \([.items[] | select(.is_special)] | length) \([.items[].status] | unique | join(" "))"')" "24 8 closed"
  local p
  for p in $(field '.items[] | select(.period == 12) | "\(.id)/\(.version)"'); do
    api 200 '"  period \(.fiscal_year)/\(.period) of ledger \(.ledger_id[0:8])…: \(.status)"' openFiscalPeriod -p "id=${p%/*}" -if-match "\"${p#*/}\""
  done
}

# year_end_business posts PREV's last business in December, in every
# ledger: a revenue, an expense, and USD 2,000.00 into the RBC USD account
# at the month's rate.
year_end_business() {
  as tom
  local bank usd_bank sales expense
  bank=$(account 100000)
  usd_bank=$(account 1030)
  sales=$(account 4000)
  expense=$(account 6000)
  say "The Bank of Canada's USD rates of December $PREV: 1.36 from the 1st, 1.38 on the 31st."
  api 201 '"\(.from_currency)/\(.to_currency) \(.rate) from \(.valid_from) (\(.source))"' createExchangeRate \
    -body "{\"rate_type\": \"average\", \"from_currency\": \"USD\", \"to_currency\": \"CAD\", \"valid_from\": \"$PREV-12-01\", \"rate\": \"1.36\", \"source\": \"BOC\"}"
  usd_rate "$PREV-12-31" 1.38
  say "December's business: a sale, the month's rent, and USD 2,000.00 into the RBC USD account at 1.36."
  local filter='"journal entry \(.document_number) of \(.posting_date), period \(.fiscal_year)/\(.fiscal_period): \(.header_text)"'
  api 201 "$filter" postJournalEntry \
    -body "{\"legal_entity_id\": \"$LE\", \"document_date\": \"$PREV-12-05\", \"posting_date\": \"$PREV-12-05\", \"currency\": \"CAD\", \"header_text\": \"December sales\", \"lines\": [{\"account_id\": \"$bank\", \"amount\": {\"amount\": \"4000.00\", \"currency\": \"CAD\"}}, {\"account_id\": \"$sales\", \"amount\": {\"amount\": \"-4000.00\", \"currency\": \"CAD\"}}]}"
  api 201 "$filter" postJournalEntry \
    -body "{\"legal_entity_id\": \"$LE\", \"document_date\": \"$PREV-12-10\", \"posting_date\": \"$PREV-12-10\", \"currency\": \"CAD\", \"header_text\": \"December rent\", \"lines\": [{\"account_id\": \"$expense\", \"amount\": {\"amount\": \"1500.00\", \"currency\": \"CAD\"}}, {\"account_id\": \"$bank\", \"amount\": {\"amount\": \"-1500.00\", \"currency\": \"CAD\"}}]}"
  api 201 '"journal entry \(.document_number) of \(.posting_date), period \(.fiscal_year)/\(.fiscal_period): \(.header_text), \([.lines[] | "\(.amount.amount) USD = \(.amount_lc.amount) CAD"] | join(" / "))"' postJournalEntry \
    -body "{\"legal_entity_id\": \"$LE\", \"document_date\": \"$PREV-12-15\", \"posting_date\": \"$PREV-12-15\", \"currency\": \"USD\", \"header_text\": \"Fund the RBC USD account\", \"lines\": [{\"account_id\": \"$usd_bank\", \"amount\": {\"amount\": \"2000.00\", \"currency\": \"USD\"}}, {\"account_id\": \"$bank\", \"amount\": {\"amount\": \"-2000.00\", \"currency\": \"USD\"}}]}"
  expect "the USD posting's local amount" "$(field '.lines[0].amount_lc.amount')" 2720.00
}

# year_end_revaluation is PREV's year-end revaluation in each ledger the
# company keeps, keyed on PREV's last day: the RBC USD account's USD
# 2,000.00 booked at 1.36 is worth 1.38, a gain of CAD 40.00. Its reset
# entry is dated the next day, the first of this year, whose period must
# be open in the run's ledger (ADR-0042): a return filed for this year's
# first quarter in step 11 locked it in the leading ledger, from April to
# September, so Carla has it unlocked through an approved request (T32),
# opened, and closed and locked again after the run.
year_end_revaluation() {
  as carla
  QUIET=1 api 200 '' listLedgers
  local ledger code run first
  say "Carla revalues the foreign-currency balances at $PREV-12-31 in each ledger: USD 2,000.00 booked at 1.36 is"
  say "worth 1.38, a gain of CAD 40.00, and its reset is dated $YEAR-01-01."
  for ledger in $(field '.items[] | "\(.code)=\(.id)"'); do
    code=${ledger%%=*}
    first=$(reset_period_open "${ledger#*=}")
    as carla
    api 201 '"  '"$code"': run \(.id[0:8])… at \(.key_date), \(.status), \(.item_count) items: \([.items[] | "account \(.account_id | acct) \(.direction) \(.difference_lc.amount)"] | join(", "))"' \
      postRevaluationRun -body "{\"legal_entity_id\": \"$LE\", \"ledger_id\": \"${ledger#*=}\", \"key_date\": \"$PREV-12-31\"}"
    run=$(field .id)
    expect "$code's run" "$(field .status)" posted
    expect "$code's revaluation of the RBC USD account" \
      "$(jq -r --arg a "$(account 1030)" '.items[] | select(.account_id == $a) | "\(.direction) \(.difference_lc.amount)"' "$WORK/last.json")" "gain 40.00"
    if [ -n "$first" ]; then relock_period "$first"; fi
    printf '%s\t%s revaluation of %s\n' "$run" "$code" "$PREV" >>"$WORK/names"
  done
}

# reset_period_open LEDGER makes sure the ledger's period 1 of this year is
# open for a revaluation's reset entry, and prints its id/version when it
# had to be unlocked and opened (to lock it again after), nothing when it
# was open. A locked one is unlocked as T32 has it: Tom asks, Carla, the
# controller, approves, and Tom opens it.
reset_period_open() {
  local ledger=$1 period status request
  QUIET=1 api 200 '' listFiscalPeriods -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR" -p "ledger_id=$ledger" -p page_size=100
  period=$(field '.items[] | select(.period == 1) | .id')
  status=$(field '.items[] | select(.period == 1) | .status')
  case "$status" in
  open) return 0 ;;
  locked) ;;
  *) fail "period $YEAR/1 of ledger $ledger is $status, neither open nor locked" ;;
  esac
  {
    as tom
    say "Period $YEAR/1 is locked by the return filed for its quarter; Tom asks for it to be unlocked for the reset entry:"
    QUIET=1 api 200 '' getFiscalPeriod -p "id=$period"
    api 200 '"  period \(.fiscal_year)/\(.period): \(.status), unlock request \(.unlock_request_id[0:8])…"' unlockFiscalPeriod -p "id=$period" -if-match "$ETAG" \
      -body "{\"reason\": \"The reset of $PREV's year-end revaluation is dated $YEAR-01-01\"}"
    request=$(field .unlock_request_id)
    approve carla "$request" "For the year-end revaluation's reset only"
    as tom
    QUIET=1 api 200 '' getFiscalPeriod -p "id=$period"
    api 200 '"  period \(.fiscal_year)/\(.period): \(.status)"' openFiscalPeriod -p "id=$period" -if-match "$ETAG"
  } >&2
  printf '%s' "$period"
}

# relock_period PERIOD closes and locks again the period reset_period_open
# unlocked and opened.
relock_period() {
  as tom
  QUIET=1 api 200 '' getFiscalPeriod -p "id=$1"
  api 200 '"  period \(.fiscal_year)/\(.period): \(.status)"' closeFiscalPeriod -p "id=$1" -if-match "$ETAG"
  api 200 '"  period \(.fiscal_year)/\(.period): \(.status) again"' lockFiscalPeriod -p "id=$1" -if-match "$ETAG"
}

# year_end_adjustment is T64m1's capability (ADR-0060 §3): Carla opens
# special period 13 of PREV in each ledger and posts the auditors'
# adjustment into it, dated the year's last day; the trial balance shows
# it under period 13. A posting dated in period 11 naming period 13 is
# refused: a special period shares the year's last regular period's dates.
year_end_adjustment() {
  as carla
  local p bank expense
  bank=$(account 100000)
  expense=$(account 6000)
  say "The auditors find a December bank charge of CAD 750.00 never booked. Carla opens special period 13 of $PREV,"
  say "the first of the four year-end periods that share December's dates, in each ledger:"
  QUIET=1 api 200 '' listFiscalPeriods -p "legal_entity_id=$LE" -p "fiscal_year=$PREV" -p page_size=100
  for p in $(field '.items[] | select(.period == 13) | "\(.id)/\(.version)"'); do
    api 200 '"  period \(.fiscal_year)/\(.period) (special: \(.is_special)) of ledger \(.ledger_id[0:8])…: \(.status)"' openFiscalPeriod -p "id=${p%/*}" -if-match "\"${p#*/}\""
  done
  say "and posts the adjustment into it, dated $PREV-12-31 (fiscal_period 13):"
  api 201 '"journal entry \(.document_number) of \(.posting_date), period \(.fiscal_year)/\(.fiscal_period): \(.header_text)"' postJournalEntry \
    -body "{\"legal_entity_id\": \"$LE\", \"document_date\": \"$PREV-12-31\", \"posting_date\": \"$PREV-12-31\", \"fiscal_period\": 13, \"currency\": \"CAD\", \"header_text\": \"Audit adjustment: December bank charges\", \"lines\": [{\"account_id\": \"$expense\", \"amount\": {\"amount\": \"750.00\", \"currency\": \"CAD\"}}, {\"account_id\": \"$bank\", \"amount\": {\"amount\": \"-750.00\", \"currency\": \"CAD\"}}]}"
  expect "the adjustment's period" "$(field '"\(.fiscal_year)/\(.fiscal_period)"')" "$PREV/13"
  local adjustment
  adjustment=$(field .document_number)
  say "The trial balance of $PREV shows it under period 13, beside December's period 12:"
  api 200 '.items[] | "  period \(.fiscal_period) account \(.account_id | acct): debit \(.debit.amount), credit \(.credit.amount) \(.currency)"' \
    listBalances -p "legal_entity_id=$LE" -p "fiscal_year=$PREV" -p "account_id=$expense" -p page_size=500
  expect "the expense account's periods" "$(field '[.items[] | "\(.fiscal_period) \(.debit.amount)"] | sort | join(", ")')" "12 1500.00, 13 750.00"
  say "Its copy in 2L is in period 13 too, with the same document number:"
  QUIET=1 api 200 '' listLedgers
  local l2
  l2=$(field '.items[] | select(.code == "2L") | .id')
  api 200 '.items[] | "  2L: entry \(.document_number), period \(.fiscal_year)/\(.fiscal_period)"' listJournalEntries \
    -p "legal_entity_id=$LE" -p "fiscal_year=$PREV" -p fiscal_period=13 -p "ledger_id=$l2"
  expect "2L's copy" "$(field '[.items[] | "\(.document_number) \(.fiscal_period)"] | join(" ")')" "$adjustment 13"
  say "A posting into period 13 dated in November is refused: the special periods share December's dates."
  refused 422 ledger.special_period_date postJournalEntry \
    -body "{\"legal_entity_id\": \"$LE\", \"document_date\": \"$PREV-11-30\", \"posting_date\": \"$PREV-11-30\", \"fiscal_period\": 13, \"currency\": \"CAD\", \"header_text\": \"November charges\", \"lines\": [{\"account_id\": \"$expense\", \"amount\": {\"amount\": \"50.00\", \"currency\": \"CAD\"}}, {\"account_id\": \"$bank\", \"amount\": {\"amount\": \"-50.00\", \"currency\": \"CAD\"}}]}"
  expect "the refused field" "$(field '[.errors[].field] | join(" ")')" posting_date
}

# year_end_retained_earnings has Ada give the company its retained
# earnings account (ADR-0060 §4): a new equity account in the chart, a
# tier 4 change with its evidence, and the account the year's result is
# carried to, one for every ledger.
year_end_retained_earnings() {
  as ada
  say "A carry-forward carries each year's profit or loss to the company's retained earnings account, which $CC"
  say "does not have yet. Ada adds account 330000 to the chart and names it in year_end; a new account changes the"
  say "chart, so the change is tier 4 and the last year's invoices are replayed under it. $CC is in group MAPLE (step"
  say "40), so the account maps to the group's equity account G3000 (ADR-0059 §3)."
  api 200 '"active configuration version \(.items[0].number)"' listConfigVersions -p is_active=true
  local base cr request
  base=$(field '.items[0].number')
  jq "{title: \"Retained earnings of $CC\", rationale: \"The year's result is carried to it (ADR-0060 §4)\", base_version: $base,
    proposed: (.items[0].document | (.companies[] | select(.code == \"$CC\")) |= (.chart_of_accounts += [{\"number\": \"330000\", \"name\": \"Retained earnings\", \"type\": \"equity\"}
      + if any(.chart_of_accounts[]; .group_account) then {group_account: \"G3000\"} else {} end]
      | .year_end.retained_earnings_account = \"330000\"))}" "$WORK/last.json" >"$WORK/retained.json"
  api 201 '"change request \(.number), \(.status): \(.title)"' createChangeRequest -body @retained.json
  cr=$(field .id)
  api 200 '"\(.status), tier \(.tier): \([.items[].path] | join(", "))"' submitChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "the change's tier" "$(field .tier)" 4
  expect "the retained earnings account among the changes" "$(field '[.items[] | select(.path == "companies['"$CC"'].year_end.retained_earnings_account")] | length')" 1
  api 201 '"regression suite \(.number): \(.case_count) cases"' createRegressionSuite -body '{"name": "Before retained earnings", "window_days": 366}'
  QUIET=1 api 200 '' getChangeRequest -p "id=$cr"
  api 202 '"\(.kind) \(.status)"' simulateChangeRequest -p "id=$cr" -if-match "$ETAG"
  wait_op
  api 200 '.evidence[] | "evidence \(.kind): \(if .passed then "passed" else "FAILED" end), \(.summary)"' getChangeRequest -p "id=$cr"
  expect "status after the evidence" "$(field .status)" pending_approval
  request=$(field .workflow_request_id)
  approve sam "$request" "Retained earnings account checked"
  approve carla "$request" "The account is new and unposted"
  as ada
  QUIET=1 api 200 '' getChangeRequest -p "id=$cr"
  api 200 '"\(.status): configuration version \(.activated_version)"' activateChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "status after activation" "$(field .status)" active
}

# year_end_carry_forward is T64m2's capability (ADR-0060 §5): Carla closes
# PREV's open periods and carries PREV forward into this year in every
# ledger, stored as balances, not journal entries. Each run's rows sum to
# zero per currency (debits equal credits in every amount), and the
# leading ledger's result per currency is PREV's profit and loss in the
# trial balance.
year_end_carry_forward() {
  as carla
  accounts
  say "Carla closes $PREV's periods still open, December and special period 13, in each ledger: nothing may be"
  say "posted into what is carried."
  QUIET=1 api 200 '' listFiscalPeriods -p "legal_entity_id=$LE" -p "fiscal_year=$PREV" -p page_size=100
  local p run
  for p in $(field '.items[] | select(.status == "open") | "\(.id)/\(.version)"'); do
    api 200 '"  period \(.fiscal_year)/\(.period) of ledger \(.ledger_id[0:8])…: \(.status)"' closeFiscalPeriod -p "id=${p%/*}" -if-match "\"${p#*/}\""
  done
  say "She carries $PREV forward into $YEAR in every ledger the company keeps: the opening balances of $YEAR, per"
  say "account and currency, with $PREV's profit or loss carried to retained earnings. No journal entry is posted."
  api 201 '.items[] | "  \(.ledger_code): run \(.id[0:8])…, \(.from_fiscal_year) into \(.to_fiscal_year), \(.basis), \(.row_count) rows, result to \(.retained_earnings_account.number) \(.retained_earnings_account.name)"' \
    runBalanceCarryForward -body "{\"legal_entity_id\": \"$LE\", \"fiscal_year\": $PREV}"
  expect "the runs" "$(field '[.items[] | "\(.ledger_code) \(.basis) \(.status)"] | join(", ")')" "0L all_years current, 2L all_years current"
  cp "$WORK/last.json" "$WORK/carry-forward.json"
  say "Each run's opening balances sum to zero in every currency: its debits equal its credits in transaction,"
  say "local and group currency."
  for run in $(jq -r '.items[] | "\(.ledger_code)=\(.id)"' "$WORK/carry-forward.json"); do
    api 200 '"  \(.ledger_code), run by \(.created_by_name): result \(.result_lc.amount) \(.result_lc.currency), debits \(.debit_lc.amount) = credits \(.credit_lc.amount) in local currency, \(.debit_gc.amount) = \(.credit_gc.amount) in group currency", (.totals[] | "    \(.currency): result \(.result.amount) (\(.result_lc.amount) \(.result_lc.currency)), debit \(.debit.amount) = credit \(.credit.amount), local \(.debit_lc.amount) = \(.credit_lc.amount), group \(.debit_gc.amount) = \(.credit_gc.amount)")' \
      getBalanceCarryForward -p "id=${run#*=}"
    expect "${run%%=*}'s rows summing to zero in local and group currency" "$(field '"\(.debit_lc.amount == .credit_lc.amount) \(.debit_gc.amount == .credit_gc.amount)"')" "true true"
    expect "${run%%=*}'s rows summing to zero" "$(field '[.totals[] | select(.debit.amount != .credit.amount or .debit_lc.amount != .credit_lc.amount or .debit_gc.amount != .credit_gc.amount)] | length')" 0
    printf '%s\t%s carry-forward of %s\n' "${run#*=}" "${run%%=*}" "$PREV" >>"$WORK/names"
  done
  local result
  result=$(jq -r '[.items[0].totals[] | select(.result.amount != "0.00") | "\(.currency) \(.result.amount)"] | join(", ")' "$WORK/carry-forward.json")
  local local_result
  local_result=$(jq -r '.items[0] | "\(.result_lc.amount) \(.result_lc.currency)"' "$WORK/carry-forward.json")
  say "0L's result per currency, $result, $local_result in all, is $PREV's profit and loss in the trial balance"
  say "(revenue and expense accounts, debit positive):"
  api 200 '"  \(.items | length) balances of \(.items | map(.account_id) | unique | length) accounts"' listBalances -p "legal_entity_id=$LE" -p "fiscal_year=$PREV" -p page_size=500
  expect "the trial balance's profit and loss in local currency" "$(jq -r --slurpfile acc "$WORK/accounts.json" '($acc[0].items | map({(.id): .account_type}) | add) as $t
    | [.items[] | select($t[.account_id] == "revenue" or $t[.account_id] == "expense") | (.debit_lc.amount | tonumber) - (.credit_lc.amount | tonumber)]
    | add * 100 | round' "$WORK/last.json")" "$(jq -r '.items[0].result_lc.amount | tonumber * 100 | round' "$WORK/carry-forward.json")"
  expect "the trial balance's profit and loss" "$(jq -r --slurpfile acc "$WORK/accounts.json" '($acc[0].items | map({(.id): .account_type}) | add) as $t
    | [.items[] | select($t[.account_id] == "revenue" or $t[.account_id] == "expense")] | group_by(.currency)
    | map({c: .[0].currency, v: (map((.debit.amount | tonumber) - (.credit.amount | tonumber)) | add)}) | map(select(.v != 0))
    | map("\(.c) \(.v * 100 | round)") | join(", ")' "$WORK/last.json")" \
    "$(jq -r '[.items[0].totals[] | select(.result.amount != "0.00") | "\(.currency) \(.result.amount | tonumber * 100 | round)"] | join(", ")' "$WORK/carry-forward.json")"
}
# year_end_carry_forward_balances is T64m3's read (ADR-0060 §7): Carla
# lists PREV's runs, current and not stale, and reads the leading
# ledger's opening balances of this year: the retained earnings account's
# result row and the RBC USD account in USD and in CAD (the revaluation's
# gain, posted in local currency), with the whole run's totals on every
# page.
year_end_carry_forward_balances() {
  as carla
  say "Carla lists $PREV's carry-forward runs: each current, and not stale, no period of $PREV opened since."
  api 200 '.items[] | "  \(.ledger_code): run \(.id[0:8])…, \(.status), stale: \(.stale)"' listBalanceCarryForwards -p "legal_entity_id=$LE" -p "fiscal_year=$PREV"
  expect "$PREV's runs" "$(field '[.items[] | "\(.ledger_code) \(.status) \(.stale)"] | sort | join(", ")')" "0L current false, 2L current false"
  local run
  run=$(jq -r '.items[0].id' "$WORK/carry-forward.json")
  say "She reads 0L's opening balances of $YEAR, a page at a time, each page with the whole run's totals:"
  api 200 '(.items[] | "  \(.account.number) \(.account.name) (\(.account.type)), \(.kind), \(.currency): \(.amount.amount) = \(.amount_lc.amount) \(.amount_lc.currency)"), "  totals: debits \(.debit_lc.amount) = credits \(.credit_lc.amount) \(.local_currency), result \(.result_lc.amount)"' \
    listBalanceCarryForwardBalances -p "id=$run" -p page_size=500
  expect "the page's totals, the run's" "$(field '"\(.debit_lc.amount) \(.credit_lc.amount) \(.result_lc.amount)"')" \
    "$(jq -r '.items[0] | "\(.debit_lc.amount) \(.credit_lc.amount) \(.result_lc.amount)"' "$WORK/carry-forward.json")"
  expect "the retained earnings account's result" "$(field '[.items[] | select(.account.number == "330000" and .kind == "result") | "\(.currency) \(.amount.amount)"] | join(", ")')" \
    "$(jq -r '[.items[0].totals[] | select(.result.amount != "0.00") | "\(.currency) \(.result.amount)"] | join(", ")' "$WORK/carry-forward.json")"
  expect "the RBC USD account's opening balances" "$(field '[.items[] | select(.account.number == "1030") | "\(.currency) \(.amount.amount) \(.amount_lc.amount)"] | join(", ")')" \
    "CAD 40.00 40.00, USD 2000.00 2720.00"
}

# year_end_late_adjustment: the auditors find one more adjustment after
# the carry-forward. Carla opens special period 13 again and posts it;
# every run of PREV reads stale, naming the period, and re-running while
# the period is open is refused.
year_end_late_adjustment() {
  as carla
  local p bank expense
  bank=$(account 100000)
  expense=$(account 6000)
  say "The auditors send one more adjustment after the carry-forward: CAD 100.00 of audit fees for $PREV. Carla opens"
  say "special period 13 again in each ledger and posts it:"
  QUIET=1 api 200 '' listFiscalPeriods -p "legal_entity_id=$LE" -p "fiscal_year=$PREV" -p page_size=100
  for p in $(field '.items[] | select(.period == 13) | "\(.id)/\(.version)"'); do
    api 200 '"  period \(.fiscal_year)/\(.period) of ledger \(.ledger_id[0:8])…: \(.status)"' openFiscalPeriod -p "id=${p%/*}" -if-match "\"${p#*/}\""
  done
  api 201 '"journal entry \(.document_number) of \(.posting_date), period \(.fiscal_year)/\(.fiscal_period): \(.header_text)"' postJournalEntry \
    -body "{\"legal_entity_id\": \"$LE\", \"document_date\": \"$PREV-12-31\", \"posting_date\": \"$PREV-12-31\", \"fiscal_period\": 13, \"currency\": \"CAD\", \"header_text\": \"Late audit adjustment: audit fees\", \"lines\": [{\"account_id\": \"$expense\", \"amount\": {\"amount\": \"100.00\", \"currency\": \"CAD\"}}, {\"account_id\": \"$bank\", \"amount\": {\"amount\": \"-100.00\", \"currency\": \"CAD\"}}]}"
  say "$PREV's carry-forward no longer holds every posting of the year: each run reads stale, naming the period"
  say "opened since it ran."
  api 200 '.items[] | "  \(.ledger_code): stale: \(.stale), opened since: \([.opened_since[] | "\(.fiscal_year)/\(.period) in \(.ledger_code)"] | join(", "))"' \
    listBalanceCarryForwards -p "legal_entity_id=$LE" -p "fiscal_year=$PREV" -p status=current
  expect "the runs' staleness" "$(field '[.items[] | "\(.ledger_code) \(.stale) \([.opened_since[] | "\(.fiscal_year)/\(.period)"] | join(" "))"] | sort | join(", ")')" \
    "0L true $PREV/13, 2L true $PREV/13"
  say "Carrying $PREV forward again while period 13 is open is refused: nothing may be posted into what is carried."
  refused 409 ledger.carry_forward_periods_open runBalanceCarryForward -body "{\"legal_entity_id\": \"$LE\", \"fiscal_year\": $PREV}"
}

# year_end_rerun: Carla closes period 13 and carries PREV forward again;
# the new runs supersede the stale ones, are current and not stale, and
# 0L's result carries the late adjustment.
year_end_rerun() {
  as carla
  local p
  say "Carla closes period 13 again; the runs stay stale, the period having been opened since they ran:"
  QUIET=1 api 200 '' listFiscalPeriods -p "legal_entity_id=$LE" -p "fiscal_year=$PREV" -p page_size=100
  for p in $(field '.items[] | select(.status == "open") | "\(.id)/\(.version)"'); do
    api 200 '"  period \(.fiscal_year)/\(.period) of ledger \(.ledger_id[0:8])…: \(.status)"' closeFiscalPeriod -p "id=${p%/*}" -if-match "\"${p#*/}\""
  done
  local old
  old=$(jq -r '.items[0].id' "$WORK/carry-forward.json")
  api 200 '"  0L run \(.id[0:8])…: \(.status), stale: \(.stale)"' getBalanceCarryForward -p "id=$old"
  expect "0L's run with period 13 closed again" "$(field '"\(.status) \(.stale)"')" "current true"
  say "and carries $PREV forward again in every ledger:"
  api 201 '.items[] | "  \(.ledger_code): run \(.id[0:8])…, \(.basis), \(.status), stale: \(.stale), result \(.result_lc.amount) \(.result_lc.currency)"' \
    runBalanceCarryForward -body "{\"legal_entity_id\": \"$LE\", \"fiscal_year\": $PREV}"
  expect "the new runs" "$(field '[.items[] | "\(.ledger_code) \(.status) \(.stale)"] | join(", ")')" "0L current false, 2L current false"
  expect "0L's result with the late adjustment" "$(field '.items[0].result_lc.amount | tonumber * 100 | round')" \
    "$(jq -r '.items[0].result_lc.amount | tonumber * 100 + 10000 | round' "$WORK/carry-forward.json")"
  cp "$WORK/last.json" "$WORK/carry-forward-again.json"
  local run
  for run in $(jq -r '.items[] | "\(.ledger_code)=\(.id)"' "$WORK/carry-forward-again.json"); do
    printf '%s\t%s carry-forward of %s, again\n' "${run#*=}" "${run%%=*}" "$PREV" >>"$WORK/names"
  done
  say "The first runs read superseded by the new ones, which the list shows as $PREV's current carry-forward:"
  api 200 '"  0L run \(.id[0:8])…: \(.status) by \(.superseded_by_id[0:8])… at \(.superseded_at)"' getBalanceCarryForward -p "id=$old"
  expect "the first 0L run" "$(field '"\(.status) \(.superseded_by_id)"')" "superseded $(jq -r '.items[0].id' "$WORK/carry-forward-again.json")"
  api 200 '.items[] | "  \(.ledger_code): run \(.id[0:8])…, \(.status)"' listBalanceCarryForwards -p "legal_entity_id=$LE" -p "fiscal_year=$PREV" -p status=current
  expect "$PREV's current runs" "$(field '[.items[].id] | sort | join(" ")')" "$(jq -r '[.items[].id] | sort | join(" ")' "$WORK/carry-forward-again.json")"
}
# year_end_close is T64m4's part (ADR-0060 §8): Ada adds the year tasks to
# the close template, a tier 2 change the process owner approves; Carla
# opens the year close of PREV, its checklist the year tasks, covering
# every ledger with its periods of the year; she assigns the auditors'
# adjustments task to herself and completes it; a second year close of
# PREV is refused.
year_end_close() {
  as ada
  say "Ada adds the year tasks to the close template (ADR-0060 §8): the checklist of a company's year close, each"
  say "task due a number of business days after the fiscal year's last day, some waiting for others: manual tasks,"
  say "and checks the system runs (nothing of the year left parked, the year-end revaluation, every period's"
  say "depreciation, the carry-forward current). Like the monthly tasks they decide what the year's sign-off"
  say "requires, so the change is tier 2, approved by the process owner."
  api 200 '"active configuration version \(.items[0].number)"' listConfigVersions -p is_active=true
  local base cr request
  base=$(field '.items[0].number')
  jq "{title: \"Year-end checklist\", rationale: \"The year close's tasks (T64m4)\", base_version: $base,
    proposed: (.items[0].document | .close.year_tasks = [
      {\"code\": \"AUDIT_ADJUSTMENTS\", \"name\": \"Auditors' adjustments entered in special period 13\", \"kind\": \"manual\", \"due_day\": 20},
      {\"code\": \"EQUITY_REVIEW\", \"name\": \"Retained earnings and the carry-forward reviewed\", \"kind\": \"manual\", \"due_day\": 25,
        \"depends_on\": [\"AUDIT_ADJUSTMENTS\"]},
      {\"code\": \"NO_PARKED\", \"name\": \"No entry of the year left parked\", \"kind\": \"check\", \"check\": \"no_parked_entries\", \"due_day\": 20},
      {\"code\": \"REVALUED\", \"name\": \"Foreign currency revalued at the year's end\", \"kind\": \"check\", \"check\": \"revaluation_posted\", \"due_day\": 5},
      {\"code\": \"DEPRECIATED\", \"name\": \"Depreciation of every period posted\", \"kind\": \"check\", \"check\": \"depreciation_year_posted\", \"due_day\": 5},
      {\"code\": \"CARRIED_FORWARD\", \"name\": \"Balances carried forward and current\", \"kind\": \"check\", \"check\": \"carry_forward_current\",
        \"due_day\": 25, \"depends_on\": [\"AUDIT_ADJUSTMENTS\"]}])}" "$WORK/last.json" >"$WORK/year-tasks.json"
  api 201 '"change request \(.number), \(.status): \(.title)"' createChangeRequest -body @year-tasks.json
  cr=$(field .id)
  api 200 '"\(.status), tier \(.tier): \([.items[].path] | join(", "))"' submitChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "the change's tier" "$(field .tier)" 2
  expect "the year tasks among the changes" "$(field '[.items[] | select(.path | startswith("close.year_tasks["))] | length')" 6
  request=$(field .workflow_request_id)
  approve priya "$request" "The year-end checklist, as agreed with the controller"
  as ada
  QUIET=1 api 200 '' getChangeRequest -p "id=$cr"
  api 200 '"\(.status): configuration version \(.activated_version)"' activateChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "status after activation" "$(field .status)" active
  as carla
  local close tag carla
  carla=$(awk -F'\t' '$2 == "Carla Controller" { print $1 }' "$WORK/names")
  say "Carla opens the year close of $PREV: its checklist is the year tasks, copied with their due dates; it covers"
  say "every ledger the company keeps, each with its periods of $PREV and their status now."
  api 201 '"year close of \(.company_code) \(.fiscal_year), ending \(.year_end): \(.status), template of configuration version \(.config_version)",
    (.ledgers[] | "  covers \(.ledger_code) \(.ledger_name): \([.periods[] | "\(.period)\(if .is_special then "*" else "" end) \(.status)"] | join(", "))"),
    (.tasks[] | "  \(.code), due \(.due_on): \(.name)\(if .blocked_by == [] then "" else " (after " + (.blocked_by | join(", ")) + ")" end)")' \
    createYearClose -body "{\"legal_entity_id\": \"$LE\", \"fiscal_year\": $PREV}"
  close=$(field .id)
  tag=$ETAG
  printf '%s\tyear close of %s\n' "$close" "$PREV" >>"$WORK/names"
  expect "the year close's tasks" "$(field '[.tasks[].code] | join(" ")')" "AUDIT_ADJUSTMENTS EQUITY_REVIEW NO_PARKED REVALUED DEPRECIATED CARRIED_FORWARD"
  expect "its ledgers, the leading one first" "$(field '[.ledgers[].ledger_code] | join(" ")')" "0L 2L"
  expect "the year's end" "$(field .year_end)" "$PREV-12-31"
  expect "each ledger's periods of $PREV" "$(field '[.ledgers[] | .periods | length] | join(" ")')" "16 16"
  say "The equity review waits for the auditors' adjustments: completing it first is refused."
  refused 409 closing.task_blocked completeYearCloseTask -p "id=$close" -p code=EQUITY_REVIEW -if-match "$tag" -body '{}'
  say "Carla takes the auditors' adjustments task herself and completes it: both adjustments are in period 13."
  api 200 '"  \(.code) assigned to \(.assignee_name); the close is now at version \(.close_version)"' \
    updateYearCloseTask -p "id=$close" -p code=AUDIT_ADJUSTMENTS -if-match "$tag" -body "{\"assignee_id\": \"$carla\"}"
  tag=$ETAG
  api 200 '"  \(.code) \(.status) by \(.completed_by_name): \(.note)"' completeYearCloseTask -p "id=$close" -p code=AUDIT_ADJUSTMENTS \
    -if-match "$tag" -body '{"note": "The audit adjustment and the late audit fees posted into period 13"}'
  expect "the auditors' adjustments task" "$(field '"\(.status) \(.assignee_name)"')" "done Carla Controller"
  api 200 '"\(.company_code) \(.fiscal_year): \(.tasks_done) of \(.tasks_total) tasks done", (.tasks[] | "  \(.code) \(.status)\(if .blocked_by == [] then "" else " (after " + (.blocked_by | join(", ")) + ")" end)")' \
    getYearClose -p "id=$close"
  expect "the equity review, no longer blocked" "$(field '.tasks[] | select(.code == "EQUITY_REVIEW") | "\(.status) \(.blocked_by | length)"')" "open 0"
  say "A second year close of $PREV is refused, and names the first:"
  refused 409 closing.year_close_exists createYearClose -body "{\"legal_entity_id\": \"$LE\", \"fiscal_year\": $PREV}"
  api 200 '.items[] | "  \(.company_code) \(.fiscal_year): \(.status), \(.tasks_done) of \(.tasks_total) done"' listYearCloses -p "legal_entity_id=$LE"
  expect "$PREV's year closes" "$(field '[.items[] | select(.fiscal_year == '"$PREV"') | .id] | join(" ")')" "$close"
}

# year_end_checks is T64m5's part (ADR-0060 §8, §11): Carla runs the year
# close's checks on PREV. Nothing of PREV is left parked; each ledger's
# revaluation at PREV's last day has posted (T64m1's runs); no asset had
# anything to depreciate in PREV, so every period passes; the carry-forward
# is current after T64m3's re-run. The auditors send one more adjustment:
# Carla opens special period 13 again, posts it and closes the period; the
# carry-forward check fails, naming PREV/13 in each ledger, until she
# carries PREV forward again.
year_end_checks() {
  as carla
  local close tag p bank expense
  close=$(awk -F'\t' -v n="year close of $PREV" '$2 == n { print $1 }' "$WORK/names")
  QUIET=1 api 200 '' getYearClose -p "id=$close"
  tag=$ETAG
  local result='"  \(.code) \(.status): \(.check_result.summary)",
    (.check_result.ledgers[]? | "    \(.ledger_code)\(if .fiscal_period then " period \(.fiscal_period)" else "" end): \(if .posted then "run \(.revaluation_run_id // .carry_forward_run_id // .depreciation_run_id | .[0:8])…" else "no run" end)\(if .stale then ", stale: \([.opened_since[] | "\(.fiscal_year)/\(.period)"] | join(", ")) opened since" else "" end)")'
  say "Carla runs the year close's checks on $PREV (ADR-0060 §8). Nothing of the year, special periods included, is"
  say "left parked, and each ledger's revaluation at the year's last day has posted:"
  api 200 "$result" checkYearCloseTask -p "id=$close" -p code=NO_PARKED -if-match "$tag"
  tag=$ETAG
  expect "the parked-entries check" "$(field '"\(.check_result.passed) \(.check_result.parked_entries_count) \(.status)"')" "true 0 done"
  api 200 "$result" checkYearCloseTask -p "id=$close" -p code=REVALUED -if-match "$tag"
  tag=$ETAG
  expect "the revaluation check" "$(field '"\(.check_result.passed) \([.check_result.ledgers[] | "\(.ledger_code) \(.key_date) \(.posted)"] | join(", "))"')" \
    "true 0L $PREV-12-31 true, 2L $PREV-12-31 true"
  say "No asset had anything to depreciate in any period of $PREV, in either ledger:"
  api 200 "$result" checkYearCloseTask -p "id=$close" -p code=DEPRECIATED -if-match "$tag"
  tag=$ETAG
  expect "the depreciation check" "$(field '"\(.check_result.passed) \(.check_result.ledger_periods_count) \(.status)"')" "true 0 done"
  say "The carry-forward is current: $PREV was carried forward again after the late adjustment."
  api 200 "$result" checkYearCloseTask -p "id=$close" -p code=CARRIED_FORWARD -if-match "$tag"
  tag=$ETAG
  expect "the carry-forward check" "$(field '"\(.check_result.passed) \([.check_result.ledgers[] | "\(.ledger_code) \(.stale)"] | join(", "))"')" \
    "true 0L false, 2L false"
  bank=$(account 100000)
  expense=$(account 6000)
  say "The auditors send one more adjustment, CAD 50.00 of legal fees. Carla opens special period 13 in each ledger,"
  say "posts it and closes the period again:"
  QUIET=1 api 200 '' listFiscalPeriods -p "legal_entity_id=$LE" -p "fiscal_year=$PREV" -p page_size=100
  for p in $(field '.items[] | select(.period == 13) | "\(.id)/\(.version)"'); do
    api 200 '"  period \(.fiscal_year)/\(.period) of ledger \(.ledger_id[0:8])…: \(.status)"' openFiscalPeriod -p "id=${p%/*}" -if-match "\"${p#*/}\""
  done
  api 201 '"journal entry \(.document_number) of \(.posting_date), period \(.fiscal_year)/\(.fiscal_period): \(.header_text)"' postJournalEntry \
    -body "{\"legal_entity_id\": \"$LE\", \"document_date\": \"$PREV-12-31\", \"posting_date\": \"$PREV-12-31\", \"fiscal_period\": 13, \"currency\": \"CAD\", \"header_text\": \"Late audit adjustment: legal fees\", \"lines\": [{\"account_id\": \"$expense\", \"amount\": {\"amount\": \"50.00\", \"currency\": \"CAD\"}}, {\"account_id\": \"$bank\", \"amount\": {\"amount\": \"-50.00\", \"currency\": \"CAD\"}}]}"
  QUIET=1 api 200 '' listFiscalPeriods -p "legal_entity_id=$LE" -p "fiscal_year=$PREV" -p page_size=100
  for p in $(field '.items[] | select(.status == "open") | "\(.id)/\(.version)"'); do
    api 200 '"  period \(.fiscal_year)/\(.period) of ledger \(.ledger_id[0:8])…: \(.status)"' closeFiscalPeriod -p "id=${p%/*}" -if-match "\"${p#*/}\""
  done
  say "The carry-forward check now fails: each ledger's run is stale, period 13 having been opened since it ran."
  api 200 "$result" checkYearCloseTask -p "id=$close" -p code=CARRIED_FORWARD -if-match "$tag"
  tag=$ETAG
  expect "the carry-forward check after the late adjustment" \
    "$(field '"\(.check_result.passed) \(.status) \([.check_result.ledgers[] | "\(.ledger_code) \(.stale) \([.opened_since[] | "\(.fiscal_year)/\(.period)"] | join(" "))"] | join(", "))"')" \
    "false open 0L true $PREV/13, 2L true $PREV/13"
  say "Carla carries $PREV forward again, and the check passes:"
  api 201 '.items[] | "  \(.ledger_code): run \(.id[0:8])…, \(.status), stale: \(.stale), result \(.result_lc.amount) \(.result_lc.currency)"' \
    runBalanceCarryForward -body "{\"legal_entity_id\": \"$LE\", \"fiscal_year\": $PREV}"
  cp "$WORK/last.json" "$WORK/carry-forward-third.json"
  local run
  for run in $(jq -r '.items[] | "\(.ledger_code)=\(.id)"' "$WORK/carry-forward-third.json"); do
    printf '%s\t%s carry-forward of %s, third\n' "${run#*=}" "${run%%=*}" "$PREV" >>"$WORK/names"
  done
  api 200 "$result" checkYearCloseTask -p "id=$close" -p code=CARRIED_FORWARD -if-match "$tag"
  expect "the carry-forward check after the re-run" "$(field '"\(.check_result.passed) \(.status)"')" "true done"
  expect "the runs it names, the new ones" "$(field '[.check_result.ledgers[].carry_forward_run_id] | sort | join(" ")')" \
    "$(jq -r '[.items[].id] | sort | join(" ")' "$WORK/carry-forward-third.json")"
}

# year_end_sign_off is T64m6's part (ADR-0060 §8): Carla completes the
# equity review and signs the year close off. It is refused while PREV's
# regular periods are only closed: a year is signed off once every period
# close has locked its period. She locks them (lockFiscalPeriod) and signs
# off: the checks run again and pass, and each special period, closed, is
# locked through the period lock; every period of PREV then reads locked,
# and a posting into period 13 is refused.
year_end_sign_off() {
  as carla
  local close tag p
  close=$(awk -F'\t' -v n="year close of $PREV" '$2 == n { print $1 }' "$WORK/names")
  QUIET=1 api 200 '' getYearClose -p "id=$close"
  tag=$ETAG
  say "Carla reviews retained earnings and the carry-forward and completes the last manual task of the year close:"
  api 200 '"  \(.code) \(.status) by \(.completed_by_name): \(.note)"' completeYearCloseTask -p "id=$close" -p code=EQUITY_REVIEW -if-match "$tag" \
    -body '{"note": "Retained earnings agree with the result carried forward"}'
  tag=$ETAG
  say "She signs off the year close. It is refused: $PREV's regular periods are closed, not locked. A year is signed"
  say "off after each period's close has locked its period; the year's sign-off locks only the special periods."
  refused 409 closing.year_periods_not_locked signOffYearClose -p "id=$close" -if-match "$tag" -body '{}'
  say "No period close of $PREV was opened in the ERP, so nothing has locked its regular periods: Carla locks each"
  say "regular period of $PREV in each ledger herself (lockFiscalPeriod)."
  QUIET=1 api 200 '' listFiscalPeriods -p "legal_entity_id=$LE" -p "fiscal_year=$PREV" -p page_size=100
  for p in $(field '.items[] | select(.period <= 12 and .status == "closed") | "\(.id)/\(.version)"'); do
    QUIET=1 api 200 '' lockFiscalPeriod -p "id=${p%/*}" -if-match "\"${p#*/}\""
  done
  api 200 '"  regular periods of '"$PREV"' locked: \([.items[] | select(.period <= 12 and .status == "locked")] | length) of \([.items[] | select(.period <= 12)] | length)"' \
    listFiscalPeriods -p "legal_entity_id=$LE" -p "fiscal_year=$PREV" -p page_size=100
  expect "the regular periods locked" "$(field '[.items[] | select(.period <= 12) | .status] | unique | join(" ")')" locked
  say "and signs off: every check runs again and passes, and each special period, closed, is locked through the"
  say "period lock, by period, the leading ledger first."
  api 200 '"signed off by \(.signed_by_name) at \(.signed_at): \(.note)", (.ledgers[] | "  \(.ledger_code) \(.ledger_name): \([.periods[] | "\(.period) \(if .locked then "locked by the sign-off" else "locked already" end)"] | join(", "))")' \
    signOffYearClose -p "id=$close" -if-match "$tag" -body '{"note": "The year closed; the auditors'"'"' adjustments are in period 13 and the year is carried forward"}'
  expect "the special periods locked by the sign-off" "$(field '[.ledgers[] | "\(.ledger_code) \([.periods[] | "\(.period)=\(.locked)"] | join(" "))"] | join(", ")')" \
    "0L 13=true 14=true 15=true 16=true, 2L 13=true 14=true 15=true 16=true"
  expect "the signer" "$(field .signed_by_name)" "Carla Controller"
  api 200 '"\(.company_code) \(.fiscal_year): \(.status), by \(.signed_off_by_name)", (.ledgers[] | "  \(.ledger_code): \([.periods[].status] | unique | join(", "))")' \
    getYearClose -p "id=$close"
  expect "the year close" "$(field '"\(.status) \(.sign_offs | length)"')" "signed_off 1"
  expect "every period of $PREV" "$(field '[.ledgers[] | "\(.ledger_code) \([.periods[].status] | unique | join(" "))"] | join(", ")')" "0L locked, 2L locked"
  as tom
  say "An adjustment into period 13 of $PREV is refused now: the year is locked."
  refused 409 ledger.period_not_open postJournalEntry \
    -body "{\"legal_entity_id\": \"$LE\", \"document_date\": \"$PREV-12-31\", \"posting_date\": \"$PREV-12-31\", \"fiscal_period\": 13, \"currency\": \"CAD\", \"header_text\": \"Audit fees after the sign-off\", \"lines\": [{\"account_id\": \"$(account 6000)\", \"amount\": {\"amount\": \"10.00\", \"currency\": \"CAD\"}}, {\"account_id\": \"$(account 100000)\", \"amount\": {\"amount\": \"-10.00\", \"currency\": \"CAD\"}}]}"
}

# step_asset_estimate is the year-end review's change in estimate (T57j,
# ADR-0057 §7; IAS 16.51, IAS 8.36-38): the lathe of step 22, booked
# through this period in the leading ledger by step 29's run, gets a
# shorter useful life and a residual value from the next period on.
step_asset_estimate() {
  heading "Step 44 — A change in estimate: the lathe's useful life and residual value changed from the next period on, the periods booked untouched"
  as carla
  say "At the year-end review Carla finds the lathe will last five years in all, not ten, and fetch CAD 500.00 at"
  say "the end (IAS 16.51). Its depreciation is booked through this period in the leading ledger, so its terms there"
  say "change only by a change in estimate, prospectively from a later period (IAS 8.36-38): the periods before keep"
  say "what was planned and booked, and from the next period the net book value then is spread over the life left."
  local lathe period year next tag from
  QUIET=1 api 200 '' listAssets -p "legal_entity_id=$LE" -p status=active
  lathe=$(field '[.items[] | select(.description == "CNC lathe") | .id][0]')
  api 200 '(.terms[] | "  \(.ledger_code): \(.method) over \(.useful_life_months) months, residual value \(.residual_value.amount) \(.residual_value.currency), booked through \(.booked_through_year // "-")/\(.booked_through_period // "-")")' \
    getAsset -p "id=$lathe"
  tag=$ETAG
  year=$(field '.terms[] | select(.ledger_code == "0L") | .booked_through_year')
  period=$(field '.terms[] | select(.ledger_code == "0L") | .booked_through_period')
  expect "the lathe booked through this period" "$year/$period" "$YEAR/$((10#${TODAY:5:2}))"
  if [ "$period" -lt 12 ]; then next="$year, \"period\": $((period + 1))"; else next="$((year + 1)), \"period\": 1"; fi
  estimate_body() { echo "{\"terms\": [{\"ledger_id\": \"$LEADING\", \"useful_life_months\": 60, \"residual_value\": {\"amount\": \"500.00\", \"currency\": \"CAD\"}$1}]}"; }
  say "A change of the booked terms without the period it applies from is refused, and so is one from this period:"
  refused 409 assets.terms_in_use updateAsset -p "id=$lathe" -if-match "$tag" -body "$(estimate_body '')"
  # The member is built first: inside $(...), a {a, b} argument is brace-expanded.
  from=", \"effective_from\": {\"fiscal_year\": $year, \"period\": $period}"
  refused 422 assets.change_not_prospective updateAsset -p "id=$lathe" -if-match "$tag" -body "$(estimate_body "$from")"
  api 200 '(.terms[] | select(.ledger_code == "0L") | "  \(.ledger_code): \(.method) over \(.useful_life_months) months, residual value \(.residual_value.amount) \(.residual_value.currency)",
    (.estimate_changes[] | "    changed from \(.fiscal_year)/\(.period) (\(.effective_from)): was \(.before.useful_life_months) months, residual \(.before.residual_value.amount)"))' \
    updateAsset -p "id=$lathe" -if-match "$tag" -body "$(from=", \"effective_from\": {\"fiscal_year\": $next}"; estimate_body "$from")"
  expect "the change" "$(field '.terms[] | select(.ledger_code == "0L") | .estimate_changes | map("\(.before.useful_life_months)>\(.useful_life_months) \(.residual_value.amount)") | join(" ")')" "120>60 500.00"
  say "The schedule: this period keeps its 41.67, booked; the next one depreciates the net book value then, 4,958.33,"
  say "less the residual value over the 59 periods left of 60: (4,958.33 - 500.00) / 59 = 75.56."
  QUIET=1 api 200 '' getAssetSchedule -p "id=$lathe" -p "fiscal_year=$YEAR"
  expect "this period, planned and booked" "$(field ".periods[] | select(.period == $period) | \"\(.planned.amount) \(.booked.amount)\"")" "41.67 41.67"
  api 200 '.periods[] | select(.planned.amount != "0.00") | "  \(.fiscal_year)/\(.period): planned \(.planned.amount), booked \(.booked.amount), net book value \(.net_book_value.amount) \(.net_book_value.currency)"' \
    getAssetSchedule -p "id=$lathe" -p "fiscal_year=${next%%,*}"
  expect "the next period" "$(field ".periods[] | select(.period == ${next##* }) | .planned.amount")" 75.56
  passed
}

# step_asset_held_for_sale is depreciation stopped at classification as
# held for sale, and a transfer's receiver depreciating the transfer's
# month (T57k, ADR-0057 §7; IAS 16.55, IFRS 5.25): the tool set step 32
# split off the milling machine this period depreciates this period, and
# the milling machine, to be sold, is classified as held for sale in the
# leading ledger from the next period.
step_asset_held_for_sale() {
  heading "Step 45 — Held for sale: the milling machine's depreciation stops from the next period; the tool set split off it depreciates this period"
  as carla
  say "The monthly convention counts a period in full from its start (ADR-0057 §7). The tool set of step 32 received a"
  say "quarter of the milling machine this period, which the machine's share no longer depreciates in; the tool set"
  say "depreciates it, so no month is lost:"
  local tool mill period year tag next
  QUIET=1 api 200 '' listAssets -p "legal_entity_id=$LE" -p status=active
  tool=$(field '[.items[] | select(.description == "Milling tool set") | .id][0]')
  mill=$(field '[.items[] | select(.description == "CNC milling machine") | .id][0]')
  period=$((10#${TODAY:5:2}))
  api 200 '.periods[] | select(.period == '"$period"') | "  the tool set, \(.fiscal_year)/\(.period): planned \(.planned.amount) \(.net_book_value.currency)"' \
    getAssetSchedule -p "id=$tool" -p "fiscal_year=$YEAR"
  expect "the tool set depreciates the transfer's period" "$(field ".periods[] | select(.period == $period) | .planned.amount != \"0.00\"")" true
  say "Maple decides to sell the milling machine; it meets IFRS 5's criteria from the next period, so Carla classifies it"
  say "as held for sale in the leading ledger from then: its runs depreciate it no more (IAS 16.55)."
  if [ "$period" -lt 12 ]; then year=$YEAR; next=$((period + 1)); else year=$((YEAR + 1)); next=1; fi
  api 200 '(.terms[] | "  \(.ledger_code): booked through \(.booked_through_year // "-")/\(.booked_through_period // "-"), held for sale \(.held_for_sale.from // "no")")' getAsset -p "id=$mill"
  tag=$ETAG
  say "A period already booked is refused:"
  refused 422 assets.change_not_prospective updateAsset -p "id=$mill" -if-match "$tag" \
    -body "{\"terms\": [{\"ledger_id\": \"$LEADING\", \"held_for_sale_from\": {\"fiscal_year\": $(field '.terms[] | select(.ledger_code == "0L") | .booked_through_year'), \"period\": $(field '.terms[] | select(.ledger_code == "0L") | .booked_through_period')}}]}"
  api 200 '(.terms[] | "  \(.ledger_code): held for sale \(if .held_for_sale then "from \(.held_for_sale.fiscal_year)/\(.held_for_sale.period) (\(.held_for_sale.from))" else "no" end)")' \
    updateAsset -p "id=$mill" -if-match "$tag" -body "{\"terms\": [{\"ledger_id\": \"$LEADING\", \"held_for_sale_from\": {\"fiscal_year\": $year, \"period\": $next}}]}"
  expect "the classification" "$(field '.terms[] | select(.ledger_code == "0L") | "\(.held_for_sale.fiscal_year)/\(.held_for_sale.period)"')" "$year/$next"
  api 200 '.periods[] | select(.period == '"$next"') | "  the machine, \(.fiscal_year)/\(.period): planned \(.planned.amount), net book value \(.net_book_value.amount) \(.net_book_value.currency)"' \
    getAssetSchedule -p "id=$mill" -p "fiscal_year=$year"
  expect "no depreciation from the next period" "$(field ".periods[] | select(.period == $next) | .planned.amount")" 0.00
  say "Both apply from the next period, so this year's asset history sheet is as it was: the lathe (step 44) and the"
  say "machine, still active, stay on their rows with their cost, a classification as held for sale moving no value,"
  say "and each row reconciles from opening to closing (po:2026-11-09, B14):"
  api 200 '.items[] | select(.description == "CNC lathe" or .description == "CNC milling machine") | "  \(.asset_number) \(.description): cost \(.opening_cost.amount) + \(.acquisitions.amount) + \(.retirements.amount) + \(.transfers.amount) = \(.closing_cost.amount); accumulated depreciation \(.opening_accumulated.amount) + \(.depreciation.amount) + \(.retirements_accumulated.amount) + \(.transfers_accumulated.amount) = \(.closing_accumulated.amount); net book value \(.closing_net_book_value.amount) \(.closing_net_book_value.currency)"' \
    getAssetHistorySheet -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR" -p group_by=asset -p page_size=500
  expect "the lathe and the machine on the sheet" "$(field '[.items[] | select(.description == "CNC lathe" or .description == "CNC milling machine") | .description] | unique | join(", ")')" "CNC lathe, CNC milling machine"
  expect "the machine held for sale: active, its cost on the sheet" \
    "$(field '.items[] | select(.description == "CNC milling machine") | "\(.status) \(.closing_cost.amount == .closing_net_book_value.amount) \(.retirements.amount)"')" "active true 0.00"
  sheet_reconciles "by asset, the change in estimate and the classification recorded"
  passed
}

# step_asset_order_expenses is an asset bought on a purchase order at its
# full cost (accounting specialist B10, product owner 2026-11-10; ADR-0057
# §6; IAS 16.16): a machine ordered on the asset clearing account is
# invoiced above the order price with British Columbia PST, which is not
# recovered; both post to the asset clearing account beside the receipt's
# order price, to be capitalised with it, and the materiality report on
# what earlier postings expensed shows nothing newly expensed. A second
# machine, received and never invoiced, has its GR/IR position cleared
# (T72e) against the asset clearing account too, not into profit or loss.
step_asset_order_expenses() {
  heading "Step 46 — An asset bought on an order at its full cost: the tax not recovered and the price difference of a machine land on the asset clearing account, not in profit or loss"
  as ben
  local item po invoice entry extractor clearing request
  item="{\"description\": \"Band saw\", \"quantity\": {\"value\": \"1\", \"unit\": \"EA\"}, \"unit_price\": {\"amount\": \"8000.00\", \"currency\": \"CAD\"}, \"account_id\": \"$(account 1690)\", \"tax_code\": \"PBC\"}"
  say "Ben orders a band saw from Tyrell at CAD 8,000.00 on the asset clearing account 1690, with British Columbia PST"
  say "(PBC, 7%, not recoverable), and receives it: the receipt debits 1690 at the order price (ADR-0057 §6)."
  api 201 '"purchase order \(.document_number // .id): \(.status), \(.total_amount.amount) \(.total_amount.currency)", (.items[] | "  item \(.item_no) \(.description): \(.quantity.value) \(.quantity.unit) at \(.unit_price.amount), \(.tax_code)")' \
    createPurchaseOrder -body "{\"legal_entity_id\": \"$LE\", \"supplier_id\": \"$SUPPLIER\", \"currency\": \"CAD\", \"order_date\": \"$TODAY\", \"payment_term_code\": \"NT30\", \"items\": [$item]}"
  po=$(field .id)
  api 200 '"\(.status)\(if .release_request_id then ": release request \(.release_request_id)" else "" end)"' releasePurchaseOrder -p "id=$po" -if-match "$ETAG"
  if [ "$(field .status)" = pending_approval ]; then
    approve priya "$(field .release_request_id)" "Budgeted band saw"
    as ben
    QUIET=1 api 200 '' getPurchaseOrder -p "id=$po"
  fi
  expect "order status" "$(field .status)" released
  expect "valued through GR/IR" "$(field .valuation)" gr_ir
  api 201 '"goods receipt \(.id), journal entry \(.journal_entry_id)"' postGoodsReceipt \
    -body "{\"legal_entity_id\": \"$LE\", \"document_date\": \"$TODAY\", \"posting_date\": \"$TODAY\", \"items\": [{\"purchase_order_id\": \"$po\", \"po_item_no\": 1, \"quantity\": {\"value\": \"1\", \"unit\": \"EA\"}}]}"
  as alex
  say "Tyrell invoices CAD 8,100.00, 100.00 above the order, plus 567.00 PST. The invoice clears GR/IR at the order price;"
  say "the 100.00 and the 567.00 are part of what the saw cost (IAS 16.16), so they post as one line on the asset"
  say "clearing account 1690, where the receipt put the order price, and not on the price difference account:"
  api 201 '"\(.kind) \(.document_number) \(.supplier_reference): \(.status), \(.gross_amount.amount) \(.gross_amount.currency)"' createSupplierInvoice \
    -body "{\"legal_entity_id\": \"$LE\", \"supplier_id\": \"$SUPPLIER\", \"supplier_reference\": \"TY-SAW-1\", \"invoice_date\": \"$TODAY\", \"gross_amount\": {\"amount\": \"8667.00\", \"currency\": \"CAD\"}, \"tax_amount\": {\"amount\": \"567.00\", \"currency\": \"CAD\"}, \"purchase_order_id\": \"$po\", \"items\": [{\"purchase_order_id\": \"$po\", \"po_item_no\": 1, \"quantity\": {\"value\": \"1\", \"unit\": \"EA\"}, \"amount\": {\"amount\": \"8100.00\", \"currency\": \"CAD\"}, \"tax_code\": \"PBC\"}]}"
  invoice=$(field .id)
  api 200 "$MATCHED" matchSupplierInvoice -p "id=$invoice" -if-match '"1"'
  api 200 '"\(.document_number): \(.status), journal entry \(.journal_entry_id)"' postSupplierInvoice -p "id=$invoice" -if-match '"1"' -body "{\"posting_date\": \"$TODAY\"}"
  expect "invoice status" "$(field .status)" posted
  entry=$(field .journal_entry_id)
  api 200 '.lines[] | "  \(.account_id | acct): \(.amount.amount) \(.amount.currency)"' getJournalEntry -p "id=$entry"
  expect "one line for both, beside the GR/IR line and the supplier's" "$(field '[.lines[] | .amount.amount] | sort | join(" ")')" "-8667.00 667.00 8000.00"
  expect "the 667.00 on the asset clearing account" "$(field "[.lines[] | select(.account_id == \"$(account 1690)\") | .amount.amount] | join(\" \")")" "667.00"
  as carla
  say "Carla's worklist of the clearing account offers the invoice's line beside the receipt's, to capitalise onto the saw"
  say "as any line there (createAssetAcquisition, steps 20-22):"
  api 200 '.items[] | select(.entry_id == "'"$entry"'") | "  entry \(.document_number) line \(.line_no), \(.supplier_invoice.supplier_name // .source_context) \(.supplier_invoice.supplier_reference // ""): \(.amount.amount) \(.amount.currency), left \(.remaining.amount)"' \
    listAssetClearingItems -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR" -p remaining=true
  expect "the invoice's line on the worklist" "$(field "[.items[] | select(.entry_id == \"$entry\") | \"\(.supplier_invoice.supplier_reference) \(.remaining.amount)\"] | join(\" \")")" \
    "TY-SAW-1 667.00"
  say "The year's report on what was expensed on asset purchase orders is history now: it shows what earlier postings"
  say "put on the price difference account, for the accountants to judge and correct by journal. This invoice adds nothing:"
  api 200 '(.items[] | "  order \(.purchase_order_number) item \(.po_item_no) \(.description) on \(.account_number) \(.account_name): tax not recovered \(.tax_not_recovered.amount), price difference \(.price_difference.amount), expensed \(.expensed.amount) \(.expensed.currency)"),
    "  totals: tax not recovered \(.totals.tax_not_recovered.amount), price difference \(.totals.price_difference.amount), expensed \(.totals.expensed.amount) \(.totals.expensed.currency); put in profit or loss by GR/IR clearings \(.totals.gr_ir_cleared.amount)",
    "  unattributed (earlier postings, split unknown): \(.unattributed.expensed.amount) \(.unattributed.expensed.currency) (debits \(.unattributed.debits.amount), credits \(.unattributed.credits.amount)) in \(.unattributed.entries) entries"' \
    getAssetOrderExpenses -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR"
  expect "no row for the band saw: nothing of it was expensed" "$(field "[.items[] | select(.purchase_order_id == \"$po\")] | length")" 0
  expect "nothing expensed on asset order items, by invoices or by GR/IR clearings" "$(field '"\(.totals.tax_not_recovered.amount) \(.totals.price_difference.amount) \(.totals.expensed.amount) \(.totals.gr_ir_cleared.amount) \(.totals.expensed.currency)"')" "0.00 0.00 0.00 0.00 CAD"
  expect "nothing unattributed" "$(field '"\(.unattributed.expensed.amount) \(.unattributed.debits.amount) \(.unattributed.credits.amount) \(.unattributed.entries)"')" "0.00 0.00 0.00 0"
  as ben
  say "Ben also ordered a dust extractor at CAD 1,200.00 on 1690 and received it; Tyrell then gave it as a free addition"
  say "to the saw and will not invoice it, so Ben closes the order."
  api 201 '"purchase order \(.document_number // .id): \(.status), \(.total_amount.amount) \(.total_amount.currency)"' \
    createPurchaseOrder -body "{\"legal_entity_id\": \"$LE\", \"supplier_id\": \"$SUPPLIER\", \"currency\": \"CAD\", \"order_date\": \"$TODAY\", \"payment_term_code\": \"NT30\", \"items\": [{\"description\": \"Dust extractor\", \"quantity\": {\"value\": \"1\", \"unit\": \"EA\"}, \"unit_price\": {\"amount\": \"1200.00\", \"currency\": \"CAD\"}, \"account_id\": \"$(account 1690)\", \"tax_code\": \"PBC\"}]}"
  extractor=$(field .id)
  api 200 '"\(.status)\(if .release_request_id then ": release request \(.release_request_id)" else "" end)"' releasePurchaseOrder -p "id=$extractor" -if-match "$ETAG"
  if [ "$(field .status)" = pending_approval ]; then
    approve priya "$(field .release_request_id)" "Budgeted dust extractor"
    as ben
    QUIET=1 api 200 '' getPurchaseOrder -p "id=$extractor"
  fi
  expect "the extractor's order status" "$(field .status)" released
  expect "the extractor's order valued through GR/IR" "$(field .valuation)" gr_ir
  api 201 '"goods receipt \(.id), journal entry \(.journal_entry_id)"' postGoodsReceipt \
    -body "{\"legal_entity_id\": \"$LE\", \"document_date\": \"$TODAY\", \"posting_date\": \"$TODAY\", \"items\": [{\"purchase_order_id\": \"$extractor\", \"po_item_no\": 1, \"quantity\": {\"value\": \"1\", \"unit\": \"EA\"}}]}"
  QUIET=1 api 200 '' getPurchaseOrder -p "id=$extractor"
  api 200 '"purchase order \(.document_number): \(.status)"' closePurchaseOrder -p "id=$extractor" -if-match "$ETAG"
  expect "order status" "$(field .status)" closed
  as alex
  say "The receipt's CAD 1,200.00 stays open on GR/IR. Alex proposes clearing it (SAP MR11, step 15). The extractor is an"
  say "asset's order item, so the clearing shows the asset clearing account 1690 for it, not the price difference account:"
  api 201 '"GR/IR clearing of \(.purchase_order_number): \(.status), \(.total.amount) \(.total.currency)\([.items[] | "\n  item \(.po_item_no) \(.description): \(.quantity.value) \(.quantity.unit), \(.amount.amount) \(.amount.currency) against \(.offset_account.number) \(.offset_account.name)"] | join(""))"' \
    createGrIrClearing -p "id=$extractor" -body "{\"reason\": \"Free addition to the band saw; Tyrell will not invoice it\", \"posting_date\": \"$TODAY\"}"
  clearing=$(field .id)
  request=$(field .approval_request_id)
  expect "the item clears against the asset clearing account" "$(field '[.items[] | "\(.po_item_no) \(.amount.amount) \(.offset_account.number)"] | join("; ")')" "1 -1200.00 1690"
  approve carla "$request" "Not invoiced: off the asset's cost"
  as alex
  api 200 '"GR/IR clearing: \(.status), journal entry \(.journal_entry.document_number) of \(.journal_entry.fiscal_year)"' getGrIrClearing -p "id=$clearing"
  expect "clearing status" "$(field .status)" posted
  api 200 '.lines[] | "  \(.account_id | acct): \(.amount.amount) \(.amount.currency)"' getJournalEntry -p "id=$(field .journal_entry.id)"
  expect "the clearing credits the asset clearing account" "$(field "[.lines[] | select(.account_id == \"$(account 1690)\") | .amount.amount] | join(\" \")")" "-1200.00"
  as carla
  say "The extractor cost nothing, and nothing went to profit or loss: the report's figure for GR/IR clearings stays zero."
  api 200 '"  put in profit or loss by GR/IR clearings of asset order items: \(.totals.gr_ir_cleared.amount) \(.totals.gr_ir_cleared.currency)"' \
    getAssetOrderExpenses -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR"
  expect "nothing put in profit or loss by the clearing" "$(field '"\(.totals.gr_ir_cleared.amount) \([.items[] | select(.purchase_order_id == "'"$extractor"'")] | length)"')" "0.00 0"
  passed
}

# step_customer is the customer with its data in a company (T74a, 03
# §3.6): Ada adds Maple's trade receivables account, a customer
# reconciliation account, through a governed change of the chart; Ruth, the
# AR accountant, creates Northwind Traders as a customer of Maple with its
# receivables account and payment term, a payables account refused there;
# Carla approves it as she does suppliers, and Ruth reads the customer back,
# active, with its company data. CUSTOMER is the customer for later steps.
step_customer() {
  heading "Step 47 — A customer with its data in a company: the receivables account through a governed change; the customer created, approved and read back"
  as ada
  say "A customer's receivables post to a customer reconciliation account, whose every line names its customer."
  say "Maple's chart has none yet, so Ada proposes account 1200, a change of the chart, tier 4, with its evidence"
  say "and two approvers:"
  api 200 '"active configuration version \(.items[0].number)"' listConfigVersions -p is_active=true
  local base ar ap body
  base=$(field '.items[0].number')
  jq "{title: \"Trade receivables account\", rationale: \"Customers of the pilot company (T74a)\", base_version: $base,
    proposed: (.items[0].document | (.companies[] | select(.code == \"$CC\")) |= (.chart_of_accounts += [{\"number\": \"1200\", \"name\": \"Trade receivables\", \"type\": \"asset\", \"customer_reconciliation\": true}
      + if any(.chart_of_accounts[]; .group_account) then {group_account: \"G1000\"} else {} end]))}" "$WORK/last.json" >"$WORK/receivables.json"
  api 201 '"change request \(.number), \(.status): \(.title)"' createChangeRequest -body @receivables.json
  governed "$(field .id)" "the receivables account"
  accounts
  ar=$(account 1200)
  api 200 '"account \(.number) \(.name): reconciliation kind \(.reconciliation_kind)"' getAccount -p "id=$ar"
  expect "the receivables account's reconciliation kind" "$(field .reconciliation_kind)" customer
  say "Ada adds Ruth, the AR accountant, to the team:"
  person ruth "Ruth Receivables" finance_staff
  as ruth
  QUIET=1 api 200 '' getBusinessPartner -p "id=$SUPPLIER"
  ap=$(field ".supplier_companies[] | select(.legal_entity_id == \"$LE\") | .reconciliation_account_id")
  say "Ruth creates Northwind Traders as a customer of Maple: its receivables account and its payment term, net 30"
  say "days. Naming Maple's payables account instead is refused, the field named:"
  body="{\"kind\": \"organization\", \"name\": \"Northwind Traders\", \"country_code\": \"CA\", \"roles\": [\"customer\"], \"customer_companies\": [{\"legal_entity_id\": \"$LE\", \"reconciliation_account_id\": \"@ACCOUNT@\", \"payment_term_code\": \"NT30\"}], \"reason\": \"Buys machined parts from Maple\"}"
  refused 422 partner.invalid_reconciliation_account createBusinessPartner -body "${body//@ACCOUNT@/$ap}"
  expect "the refused field" "$(field '[.errors[].field] | join(" ")')" "customer_companies[0].reconciliation_account_id"
  api 201 '"business partner \(.number) \(.name): \(.status), \(.roles | join(", "))", (.customer_companies[] | "  in company '"$CC"': account \(.reconciliation_account_number), terms \(.payment_term_code), posting blocked \(.is_posting_blocked)")' \
    createBusinessPartner -body "${body//@ACCOUNT@/$ar}"
  CUSTOMER=$(field .id)
  expect "the new customer's status" "$(field .status)" pending
  QUIET=1 api 200 '' listPartnerChanges -p "partner_id=$CUSTOMER" -p kind=create
  say "Its create change waits for the approver of partner master data, Carla, as a supplier's does:"
  approve carla "$(field '.items[0].workflow_request_id')" "Customer confirmed against its purchase order"
  as ruth
  api 200 '"business partner \(.number) \(.name): \(.status), \(.roles | join(", "))", (.customer_companies[] | "  in company '"$CC"': account \(.reconciliation_account_number), terms \(.payment_term_code), posting blocked \(.is_posting_blocked)")' \
    getBusinessPartner -p "id=$CUSTOMER"
  expect "the customer's status" "$(field .status)" active
  expect "the customer's company data" \
    "$(field '[.customer_companies[] | "\(.legal_entity_id) \(.reconciliation_account_id) \(.reconciliation_account_number) \(.payment_term_code) \(.is_posting_blocked)"] | join(", ")')" \
    "$LE $ar 1200 NT30 false"
  expect "no supplier data on a customer" "$(field '.supplier_companies | length')" 0
  api 200 '.items[] | "customer \(.number) \(.name), \(.country_code), \(.status)"' listBusinessPartners -p role=customer -p name_prefix=Northwind
  expect "the customers named Northwind" "$(field '[.items[].id] | join(" ")')" "$CUSTOMER"
  passed
}

# step_customer_invoice is T75a and T75b (ADR-0063 §2-§5): a customer
# invoice entered without a sales order and read back, nothing posted;
# then posted, once: its entry debits the customer's receivables account
# and credits revenue and output tax, and its debit line is the customer's
# open item with the customer's payment terms and net due date. The
# customer is step_customer's, CUSTOMER; the posted invoice is
# CUSTOMER_INVOICE, its entry CUSTOMER_INVOICE_ENTRY.
step_customer_invoice() {
  heading "Step 48 — Customer invoices: an invoice to a customer entered without a sales order, its GST calculated per item at the invoice date, read back; posted once as the customer's open item with its terms and due date"
  local customer=$CUSTOMER sales invoice number item1 item2 item3 entry due tag
  as alex
  say "Maple invoices Northwind Traders for October's consulting. The ERP issues no legal tax invoice and sends no"
  say "e-invoice (ADR-0049 §7): the customer invoice entered here is the accounting record of the invoice Maple"
  say "issued (ADR-0063 §2). The customer is the one Ruth created in step 47."
  sales=$(account 4000)
  say "Alex enters the invoice: two items on 4000 Sales with GO5, the GST output code of step 2 (5%), and a"
  say "recharge without tax. The tax date is the invoice date (ADR-0049 §4). 5% of 1,000.10 + 499.95 is 75.0025:"
  say "75.00 once for the code, and each item keeps its share, so the items' tax adds up to the invoice's."
  item1="{\"description\": \"Consulting, October\", \"account_id\": \"$sales\", \"net_amount\": {\"amount\": \"1000.10\", \"currency\": \"CAD\"}, \"tax_code\": \"GO5\"}"
  item2="{\"description\": \"Workshop\", \"account_id\": \"$sales\", \"net_amount\": {\"amount\": \"499.95\", \"currency\": \"CAD\"}, \"tax_code\": \"GO5\"}"
  item3="{\"description\": \"Courier recharged at cost\", \"account_id\": \"$sales\", \"net_amount\": {\"amount\": \"20.00\", \"currency\": \"CAD\"}}"
  api 201 '"customer invoice \(.document_number) to \(.customer_number) \(.customer_name), \(.company_code) \(.company_name): \(.status), net \(.net_amount.amount), tax \(.tax_amount.amount), gross \(.gross_amount.amount) \(.currency)", (.items[] | "  item \(.item_no) \(.description): \(.net_amount.amount) on \(.account_number) \(.account_name), \(.tax_code // "no tax code"), tax \(.tax_amount.amount), gross \(.gross_amount.amount)")' \
    createCustomerInvoice -body "{\"legal_entity_id\": \"$LE\", \"customer_id\": \"$customer\", \"invoice_date\": \"$TODAY\", \"currency\": \"CAD\", \"items\": [$item1, $item2, $item3]}"
  invoice=$(field .id)
  number=$(field .document_number)
  expect "the invoice's status" "$(field .status)" draft
  expect "net, tax and gross" "$(field '"\(.net_amount.amount) \(.tax_amount.amount) \(.gross_amount.amount) \(.gross_amount.currency)"')" "1520.05 75.00 1595.05 CAD"
  expect "each item's tax" "$(field '[.items[] | .tax_amount.amount] | join(" ")')" "50.00 25.00 0.00"
  expect "each item's gross" "$(field '[.items[] | .gross_amount.amount] | join(" ")')" "1050.10 524.95 20.00"
  expect "the customer by name" "$(field .customer_name)" "Northwind Traders"
  say "Read back, it is the same document:"
  api 200 '"customer invoice \(.document_number): \(.status), \(.kind), dated \(.invoice_date), gross \(.gross_amount.amount) \(.gross_amount.currency), \(.items | length) items"' \
    getCustomerInvoice -p "id=$invoice"
  expect "read back" "$(field '"\(.document_number) \(.status) \(.net_amount.amount) \(.tax_amount.amount) \(.gross_amount.amount)"')" "$number draft 1520.05 75.00 1595.05"
  api 200 '.items[] | "  \(.document_number) \(.customer_name): \(.status), \(.gross_amount.amount) \(.gross_amount.currency)"' \
    listCustomerInvoices -p "customer_id=$customer" -p status=draft
  expect "the customer's drafts" "$(field '[.items[].document_number] | join(" ")')" "$number"
  say "GA, an input tax code (the return's adjustment of step 2), is no output tax code; the refusal names the item's own field:"
  refused 422 receivables.unknown_tax_code createCustomerInvoice \
    -body "{\"legal_entity_id\": \"$LE\", \"customer_id\": \"$customer\", \"invoice_date\": \"$TODAY\", \"currency\": \"CAD\", \"items\": [${item1/GO5/GA}]}"
  expect "the field refused" "$(field '.errors[0].field')" "items[0].tax_code"
  say "Nothing is posted yet: the customer has no open item."
  QUIET=1 api 200 '' listOpenItems -p "legal_entity_id=$LE" -p "partner_id=$customer"
  expect "the customer's open items" "$(field '.items | length')" 0
  as ruth
  say "Ruth, the AR accountant, posts the invoice (T75b, ADR-0063 §5). It names no payment term, so it posts with"
  say "Northwind's in Maple, NT30, counted from the invoice date:"
  due=$(jq -rn --arg d "$TODAY" '$d | strptime("%Y-%m-%d") | mktime + 30 * 86400 | strftime("%Y-%m-%d")')
  QUIET=1 api 200 '' getCustomerInvoice -p "id=$invoice"
  tag=$ETAG
  api 200 '"customer invoice \(.document_number): \(.status) on \(.posting_date) as journal entry \(.journal_entry_number), terms \(.payment_term_code), due \(.net_due_date)"' \
    postCustomerInvoice -p "id=$invoice" -if-match "$tag" -body "{\"posting_date\": \"$TODAY\"}"
  expect "the posted invoice" "$(field '"\(.status) \(.posting_date) \(.payment_term_code) \(.net_due_date) \(.version)"')" "posted $TODAY NT30 $due 2"
  entry=$(field .journal_entry_id)
  CUSTOMER_INVOICE=$invoice CUSTOMER_INVOICE_ENTRY=$entry
  say "The entry debits Northwind on 1200 Trade receivables with the gross and credits 4000 Sales per item and"
  say "2300 GST/HST payable with the tax; it balances and names receivables as its source:"
  api 200 '"journal entry \(.document_number), \(.document_type), source \(.source_context)", (.lines[] | "  \(.account_id | acct): \(.amount.amount) \(.amount.currency)")' \
    getJournalEntry -p "id=$entry"
  expect "the entry's source" "$(field '"\(.document_type) \(.source_context) \(.source_document_id)"')" "DR receivables $invoice"
  expect "the entry's amounts" "$(field '[.lines[].amount.amount] | join(" ")')" "1595.05 -1000.10 -499.95 -20.00 -75.00"
  expect "the entry's accounts" "$(field '[.lines[].account_id] | join(" ")')" "$(account 1200) $sales $sales $sales $(account 2300)"
  expect "the receivable line's partner" "$(field '.lines[0].partner_id')" "$customer"
  say "The debit line is Northwind's open item, with the terms it was posted with and its net due date:"
  api 200 '.items[] | "  open item \(.document_number): \(.amount.amount) \(.amount.currency), terms \(.payment_terms.code), due \(.payment_terms.net_due_date)"' \
    listOpenItems -p "legal_entity_id=$LE" -p "partner_id=$customer"
  expect "the customer's open item" "$(field '[.items[] | "\(.journal_entry_id) \(.amount.amount) \(.amount.currency) \(.payment_terms.code) \(.payment_terms.net_due_date)"] | join(", ")')" \
    "$entry 1595.05 CAD NT30 $due"
  say "A second post is refused, and the ledger does not reverse the entry: a posted customer invoice is corrected"
  say "by a credit memo (ADR-0063 §5)."
  QUIET=1 api 200 '' getCustomerInvoice -p "id=$invoice"
  refused 409 receivables.invalid_status postCustomerInvoice -p "id=$invoice" -if-match "$ETAG" -body '{}'
  QUIET=1 api 200 '' getJournalEntry -p "id=$entry"
  refused 409 ledger.entry_owned_elsewhere reverseJournalEntry -p "id=$entry" -if-match "$ETAG" -body '{}'
  passed
}

# step_internal_order: internal orders (ADR-0064, T60a, T60b): Carla
# creates an order of Maple under the finance office's cost center and
# releases it for posting; Alex posts an expense to it on a journal entry
# line, and Carla reads it in the order's actual costs.
step_internal_order() {
  heading "Step 49 — Internal orders: an order of Maple created under a cost center and released; an expense posted to it shows in its actual costs"
  as carla
  local cc order
  QUIET=1 api 200 '' listCostCenters -p "legal_entity_id=$LE" -p code=CC-1100
  cc=$(field '.items[0].id')
  say "Carla creates an internal order for the year's trade fair: a cost object of its own beside the cost centers,"
  say "collecting what the fair costs. The finance office's cost center CC-1100 is responsible for it (ADR-0064 §1)."
  api 201 '"internal order \(.code) \(.name): \(.status), responsible cost center \(.responsible_cost_center_code)"' createInternalOrder \
    -body "{\"legal_entity_id\": \"$LE\", \"code\": \"FAIR-$YEAR\", \"name\": \"Trade fair $YEAR\", \"responsible_cost_center_id\": \"$cc\"}"
  order=$(field .id)
  expect "the new order's status" "$(field .status)" created
  expect "the order's responsible cost center" "$(field .responsible_cost_center_code)" CC-1100
  say "A code is the company's once:"
  refused 409 controlling.internal_order_exists createInternalOrder \
    -body "{\"legal_entity_id\": \"$LE\", \"code\": \"FAIR-$YEAR\", \"name\": \"Trade fair again\", \"responsible_cost_center_id\": \"$cc\"}"
  say "Carla releases the order for posting, at the version she read; it is released once:"
  QUIET=1 api 200 '' getInternalOrder -p "id=$order"
  api 200 '"internal order \(.code): \(.status), version \(.version)"' releaseInternalOrder -p "id=$order" -if-match "$ETAG"
  expect "the released order's status" "$(field .status)" released
  refused 409 controlling.internal_order_released releaseInternalOrder -p "id=$order" -if-match "$ETAG"
  api 200 '.items[] | "  \(.code) \(.name): \(.status), cost center \(.responsible_cost_center_code)"' listInternalOrders -p "legal_entity_id=$LE" -p status=released
  expect "Maple's released orders" "$(field '[.items[].code] | join(" ")')" "FAIR-$YEAR"
  as alex
  accounts
  local expense bank entry
  expense=$(account 510000)
  bank=$(account 100000)
  say "Alex posts the stand's materials, CAD 1,500.00, to the order on a journal entry line (T60b). A primary cost line"
  say "carries exactly one real cost object, a cost center or an order: this one names the order and no cost center,"
  say "and takes its profit center and segment from the order's responsible cost center CC-1100."
  api 201 '"journal entry \(.document_number): \([.lines[] | "\(.account_id | acct) \(.amount.amount) \(.amount.currency)\(if .internal_order_code then " on order \(.internal_order_code), profit center \(.profit_center_code)" else "" end)"] | join(" / "))"' postJournalEntry \
    -body "{\"legal_entity_id\": \"$LE\", \"document_date\": \"$TODAY\", \"posting_date\": \"$TODAY\", \"currency\": \"CAD\", \"header_text\": \"Trade fair stand materials\", \"lines\": [{\"account_id\": \"$expense\", \"amount\": {\"amount\": \"1500.00\", \"currency\": \"CAD\"}, \"internal_order_id\": \"$order\"}, {\"account_id\": \"$bank\", \"amount\": {\"amount\": \"-1500.00\", \"currency\": \"CAD\"}}]}"
  entry=$(field .id)
  expect "the expense line's order, cost center and profit center" \
    "$(field '.lines[0] | "\(.internal_order_code) \(.cost_center_id // "none") \(.profit_center_code)"')" "FAIR-$YEAR none PC-MTL"
  say "An order beside a cost center on one line is refused:"
  refused 422 ledger.internal_order_invalid postJournalEntry \
    -body "{\"legal_entity_id\": \"$LE\", \"document_date\": \"$TODAY\", \"posting_date\": \"$TODAY\", \"currency\": \"CAD\", \"lines\": [{\"account_id\": \"$expense\", \"amount\": {\"amount\": \"10.00\", \"currency\": \"CAD\"}, \"internal_order_id\": \"$order\", \"cost_center_id\": \"$cc\"}, {\"account_id\": \"$bank\", \"amount\": {\"amount\": \"-10.00\", \"currency\": \"CAD\"}}]}"
  expect "why the line is refused" "$(field '.errors[0] | "\(.field) \(.code)"')" "lines[0].internal_order_id with_cost_center"
  as carla
  say "Carla reads the order's actual costs by account and period: they are the lines posted to it."
  api 200 '"order \(.internal_order_code) \(.internal_order_name), \(.fiscal_year):", (.items[] | "  \(.account_number) \(.account_name), period \(.fiscal_period): debit \(.debit.amount), credit \(.credit.amount), balance \(.balance.amount) \(.balance.currency)"), "  totals: debit \(.totals.debit.amount), credit \(.totals.credit.amount), balance \(.totals.balance.amount) \(.totals.balance.currency)"' \
    listInternalOrderBalances -p "internal_order_id=$order" -p "fiscal_year=$YEAR"
  expect "the order's actuals" "$(field '[.items[] | "\(.account_number) \(.balance.amount)"] | join("; ")')" "510000 1500.00"
  expect "the order's total actual costs" "$(field '"\(.totals.debit.amount) \(.totals.credit.amount) \(.totals.balance.amount) \(.totals.balance.currency)"')" "1500.00 0.00 1500.00 CAD"
  QUIET=1 api 200 '' getJournalEntry -p "id=$entry"
  expect "the entry read again names the order" "$(field '.lines[0].internal_order_code')" "FAIR-$YEAR"
  passed
}

# step_order_budget: an order's budget against its actual costs (ADR-0064
# §3, T60c): Carla gives the trade fair's order a budget, one overall
# amount in Maple's currency, and reads what remains of it after the
# expense step_internal_order posted. A budget below the actual costs is
# shown as exceeded; nothing is refused for it.
step_order_budget() {
  heading "Step 50 — An order's budget: the trade fair's order is given a budget; what remains is the budget less the costs posted to the order, and a budget below them is shown as exceeded"
  as carla
  local order line tag
  line='"order \(.internal_order_code) \(.internal_order_name): budget \(.budget.amount // "none"), actual \(.actual.amount), remaining \(.remaining.amount // "none") \(.actual.currency)\(if .exceeded then ", exceeded" else "" end) (version \(.version))"'
  QUIET=1 api 200 '' listInternalOrders -p "legal_entity_id=$LE" -p "code=FAIR-$YEAR"
  order=$(field '.items[0].id')
  say "The trade fair's order has no budget yet. Its actual costs are the CAD 1,500.00 Alex posted to it, over every"
  say "fiscal year (ADR-0064 §3):"
  api 200 "$line" getInternalOrderBudget -p "id=$order"
  expect "the order before a budget" "$(field '"\(.version) \(.budget.amount // "none") \(.actual.amount) \(.actual.currency) \(.exceeded)"')" "0 none 1500.00 CAD false"
  say "Carla sets the budget, one overall amount in Maple's currency, at the version she read. She first enters"
  say "CAD 1,000.00, below what the fair has already cost: the budget is taken and shown as exceeded. A budget"
  say "refuses nothing; it is compared with the actual costs when it is read."
  api 200 "$line" updateInternalOrderBudget -p "id=$order" -if-match "$ETAG" -body '{"budget": {"amount": "1000.00", "currency": "CAD"}}'
  tag=$ETAG
  expect "a budget below the actual costs" "$(field '"\(.budget.amount) \(.actual.amount) \(.remaining.amount) \(.remaining.currency) \(.exceeded)"')" "1000.00 1500.00 -500.00 CAD true"
  say "A change at a version already replaced is refused, as is a budget in another currency than the company's:"
  refused 412 request.version_mismatch updateInternalOrderBudget -p "id=$order" -if-match '"0"' -body '{"budget": {"amount": "5000.00", "currency": "CAD"}}'
  refused 422 controlling.order_budget_invalid updateInternalOrderBudget -p "id=$order" -if-match "$tag" -body '{"budget": {"amount": "5000.00", "currency": "USD"}}'
  expect "why the budget is refused" "$(field '.errors[0].field')" "budget.currency"
  say "She corrects it to CAD 5,000.00. What remains is the budget less the actual costs:"
  api 200 "$line" updateInternalOrderBudget -p "id=$order" -if-match "$tag" -body '{"budget": {"amount": "5000.00", "currency": "CAD"}}'
  api 200 "$line" getInternalOrderBudget -p "id=$order"
  expect "budget, actual and what remains" "$(field '"\(.version) \(.budget.amount) \(.actual.amount) \(.remaining.amount) \(.remaining.currency) \(.exceeded)"')" "2 5000.00 1500.00 3500.00 CAD false"
  passed
}

# center_costs CENTER ACCOUNT is what the cost center's report shows on the
# account this year, the debits less the credits, in whole cents of the
# local currency: jq reads each two-decimal amount as a number and rounds
# it to whole cents before adding, as the reports' other checks do.
center_costs() {
  QUIET=1 api 200 '' listCostCenterBalances -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR" -p "cost_center_id=$1" -p page_size=100
  field "[.items[] | select(.account_id == \"$2\") | ((.debit_lc.amount | tonumber) - (.credit_lc.amount | tonumber)) * 100 | round] | add // 0"
}

# cents_plain CENTS is whole cents, zero or more, as an amount with two
# decimals.
cents_plain() { printf '%d.%02d' $(($1 / 100)) $(($1 % 100)); }

# step_order_settlement: an order's settlement rule and its settlement
# (ADR-0064 §3, T60d, T60e): Carla names the cost center that will receive
# the trade fair's costs when the order is settled, one receiver taking
# the whole. The rule is replaced with the order's version. She then
# settles the order up to this period: one entry moves the order's costs
# to the receiver on the account they were posted on, the order's balance
# is zero and the cost center's report shows the cost; settling again
# posts nothing. The settled order is closed and takes no posting.
step_order_settlement() {
  heading "Step 51 — An order's settlement: the trade fair's order names the cost center that receives its costs, is settled to it on the original account, and is closed"
  as carla
  local order cc line tag period expense bank before after settlement entry
  line='"order \(.internal_order_code) \(.internal_order_name): receiver \(.receiver_cost_center_code // "none") (order version \(.version))"'
  QUIET=1 api 200 '' listInternalOrders -p "legal_entity_id=$LE" -p "code=FAIR-$YEAR"
  order=$(field '.items[0].id')
  QUIET=1 api 200 '' listCostCenters -p "legal_entity_id=$LE" -p code=CC-1100
  cc=$(field '.items[0].id')
  say "The trade fair's order has no settlement rule yet:"
  api 200 "$line" getInternalOrderSettlementRule -p "id=$order"
  expect "the order before a rule" "$(field '.receiver_cost_center_code // "none"')" none
  tag=$ETAG
  say "A receiver is an active cost center of the order's company that is valid today; another is refused:"
  refused 422 controlling.settlement_rule_receiver_invalid updateInternalOrderSettlementRule -p "id=$order" -if-match "$tag" \
    -body "{\"receiver_cost_center_id\": \"$order\"}"
  expect "why the receiver is refused" "$(field '.errors[0] | "\(.field) \(.code)"')" "receiver_cost_center_id unknown"
  say "Carla names the finance office's cost center CC-1100 as the receiver, at the order's version she read. It takes"
  say "the whole of what the order is settled for (ADR-0064 §3); the order's version moves with its rule."
  api 200 "$line" updateInternalOrderSettlementRule -p "id=$order" -if-match "$tag" -body "{\"receiver_cost_center_id\": \"$cc\"}"
  expect "the order's receiver" "$(field '"\(.receiver_cost_center_code) \(.receiver_cost_center_id)"')" "CC-1100 $cc"
  say "A change at the version the rule replaced is refused:"
  refused 412 request.version_mismatch updateInternalOrderSettlementRule -p "id=$order" -if-match "$tag" -body "{\"receiver_cost_center_id\": \"$cc\"}"
  api 200 "$line" getInternalOrderSettlementRule -p "id=$order"
  expect "the rule read again" "$(field .receiver_cost_center_code)" CC-1100
  tag=$ETAG
  accounts
  expense=$(account 510000)
  bank=$(account 100000)
  QUIET=1 api 200 '' listFiscalPeriods -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR" -p "ledger_id=$LEADING" -p page_size=100
  period=$(field "[.items[] | select(.is_special | not) | select(.start_date <= \"$TODAY\" and .end_date >= \"$TODAY\") | .period][0]")
  before=$(center_costs "$cc" "$expense")
  say "The order still holds the CAD 1,500.00 Alex posted to it, so it cannot be closed yet:"
  refused 409 controlling.internal_order_not_settled closeInternalOrder -p "id=$order" -if-match "$tag"
  say "Carla settles the order up to this period, $YEAR/$period (T60e), at the order's version she read with its rule. One"
  say "journal entry, dated the period's last day, credits the order and debits the receiving cost center CC-1100 on the"
  say "account the cost was posted on, so the account's balance is unchanged and the cost moves from the order to the"
  say "cost center (ADR-0064 §3):"
  api 200 '"order \(.internal_order_code) settled up to \(.fiscal_year)/\(.fiscal_period): \(.settlement.amount.amount) \(.settlement.amount.currency) to \(.settlement.receiver_cost_center_code) on \(.settlement.account_count) account, entry \(.settlement.journal_entry_number) dated \(.settlement.posting_date)"' \
    settleInternalOrder -p "id=$order" -if-match "$tag" -body "{\"fiscal_year\": $YEAR, \"fiscal_period\": $period}"
  expect "what the settlement moved" "$(field '"\(.settled) \(.settlement.amount.amount) \(.settlement.amount.currency) \(.settlement.receiver_cost_center_code) \(.settlement.account_count)"')" \
    "true 1500.00 CAD CC-1100 1"
  settlement=$(field .settlement.id)
  entry=$(field .settlement.journal_entry_id)
  api 200 '"entry \(.document_number) (\(.source_context)) on \(.posting_date): \([.lines[] | "\(.account_id | acct) \(.amount.amount) \(if .internal_order_code then "on order \(.internal_order_code)" else "on cost center \(.cost_center_code)" end)"] | join(" / "))"' \
    getJournalEntry -p "id=$entry"
  expect "the settlement's entry: the order credited, the cost center debited, on one account" \
    "$(field '[.lines[] | "\(.account_id) \(.amount.amount) \(.internal_order_code // .cost_center_code)"] | join("; ")')" \
    "$expense -1500.00 FAIR-$YEAR; $expense 1500.00 CC-1100"
  expect "the entry's source" "$(field .source_context)" controlling
  say "The order's balance is zero, and the cost center's report shows the cost:"
  api 200 '"order \(.internal_order_code), \(.fiscal_year): debit \(.totals.debit.amount), credit \(.totals.credit.amount), balance \(.totals.balance.amount) \(.totals.balance.currency)"' \
    listInternalOrderBalances -p "internal_order_id=$order" -p "fiscal_year=$YEAR"
  expect "the settled order's totals" "$(field '"\(.totals.debit.amount) \(.totals.credit.amount) \(.totals.balance.amount) \(.totals.balance.currency)"')" "1500.00 1500.00 0.00 CAD"
  after=$(center_costs "$cc" "$expense")
  say "cost center CC-1100 on account 510000: $(cents_plain $((after - before))) CAD more than before the settlement"
  expect "the cost the cost center's report gained, in cents" "$((after - before))" 150000
  say "Settling again finds nothing unsettled and posts nothing:"
  api 200 '"order \(.internal_order_code) settled up to \(.fiscal_year)/\(.fiscal_period): \(if .settled then "an entry posted" else "nothing to settle" end)"' \
    settleInternalOrder -p "id=$order" -if-match "$tag" -body "{\"fiscal_year\": $YEAR, \"fiscal_period\": $period}"
  expect "a second settlement" "$(field '"\(.settled) \(.settlement // "none")"')" "false none"
  api 200 '.items[] | "  settlement up to \(.fiscal_year)/\(.fiscal_period): \(.amount.amount) \(.amount.currency) to \(.receiver_cost_center_code), entry \(.journal_entry_number)"' \
    listInternalOrderSettlements -p "id=$order"
  expect "the order's settlements" "$(field '[.items[].id] | join(" ")')" "$settlement"
  say "Carla closes the settled order. A closed order takes no posting:"
  api 200 '"internal order \(.code): \(.status), version \(.version)"' closeInternalOrder -p "id=$order" -if-match "$tag"
  expect "the closed order's status" "$(field .status)" closed
  as alex
  refused 422 ledger.internal_order_invalid postJournalEntry \
    -body "{\"legal_entity_id\": \"$LE\", \"document_date\": \"$TODAY\", \"posting_date\": \"$TODAY\", \"currency\": \"CAD\", \"lines\": [{\"account_id\": \"$expense\", \"amount\": {\"amount\": \"10.00\", \"currency\": \"CAD\"}, \"internal_order_id\": \"$order\"}, {\"account_id\": \"$bank\", \"amount\": {\"amount\": \"-10.00\", \"currency\": \"CAD\"}}]}"
  expect "why a closed order takes no posting" "$(field '.errors[0] | "\(.field) \(.code)"')" "lines[0].internal_order_id closed"
  passed
}

# step_distribution: a distribution cycle and its run (ADR-0064 §5, T60f):
# Sam sets up a cost center for shared services and one for the sales
# office; Alex posts a cost to the shared one; Carla defines a cycle that
# sends its costs to the finance office and the sales office at 60 and 40
# percent and runs it for this period. One entry moves the cost on the
# account it was posted on, the shares adding up to it to the cent; the
# centers' reports show the shares, and the period is run once.
step_distribution() {
  heading "Step 52 — A distribution cycle: a shared cost center's costs of the period are moved to two others at 60 and 40 percent, on the account they were posted on"
  local first sender second expense bank cycle tag period entry before_first
  as sam
  QUIET=1 api 200 '' listCostCenters -p "legal_entity_id=$LE" -p code=CC-1100
  first=$(field '.items[0].id')
  say "Sam sets up two more cost centers of Maple, which Carla approves: CC-1200 for the shared services whose costs"
  say "the offices bear, and CC-1300 for the sales office."
  api 201 '"cost center \(.code) \(.name): \(.status)"' createCostCenter \
    -body "{\"legal_entity_id\": \"$LE\", \"code\": \"CC-1200\", \"name\": \"Shared services\", \"responsible\": \"Carla Controller\", \"category\": \"ADMIN\", \"valid_from\": \"$YEAR-01-01\", \"reason\": \"Costs shared by the offices\"}"
  sender=$(field .id)
  center_approved sam
  api 201 '"cost center \(.code) \(.name): \(.status)"' createCostCenter \
    -body "{\"legal_entity_id\": \"$LE\", \"code\": \"CC-1300\", \"name\": \"Sales office\", \"responsible\": \"Carla Controller\", \"category\": \"ADMIN\", \"valid_from\": \"$YEAR-01-01\", \"reason\": \"The pilot's sales office\"}"
  second=$(field .id)
  center_approved sam
  as alex
  accounts
  expense=$(account 510000)
  bank=$(account 100000)
  say "Alex posts this month's shared supplies, CAD 999.99, to the shared services' cost center:"
  api 201 '"journal entry \(.document_number): \([.lines[] | "\(.account_id | acct) \(.amount.amount) \(.amount.currency)\(if .cost_center_code then " on cost center \(.cost_center_code)" else "" end)"] | join(" / "))"' postJournalEntry \
    -body "{\"legal_entity_id\": \"$LE\", \"document_date\": \"$TODAY\", \"posting_date\": \"$TODAY\", \"currency\": \"CAD\", \"header_text\": \"Shared supplies\", \"lines\": [{\"account_id\": \"$expense\", \"amount\": {\"amount\": \"999.99\", \"currency\": \"CAD\"}, \"cost_center_id\": \"$sender\"}, {\"account_id\": \"$bank\", \"amount\": {\"amount\": \"-999.99\", \"currency\": \"CAD\"}}]}"
  as carla
  say "Carla defines a distribution cycle (ADR-0064 §5): one sender and receivers with fixed percentages, which must"
  say "add up to 100:"
  refused 422 controlling.allocation_cycle_invalid createAllocationCycle \
    -body "{\"legal_entity_id\": \"$LE\", \"code\": \"SHARED\", \"name\": \"Shared services to the offices\", \"kind\": \"distribution\", \"sender_cost_center_id\": \"$sender\", \"receivers\": [{\"cost_center_id\": \"$first\", \"percentage\": \"60\"}, {\"cost_center_id\": \"$second\", \"percentage\": \"30\"}]}"
  expect "why the cycle is refused" "$(field '.errors[0] | "\(.field) \(.code)"')" "receivers not_100"
  api 201 '"cycle \(.code) \(.name) (\(.kind)): from \(.sender_cost_center_code) to \([.receivers[] | "\(.cost_center_code) \(.percentage) %"] | join(", "))"' createAllocationCycle \
    -body "{\"legal_entity_id\": \"$LE\", \"code\": \"SHARED\", \"name\": \"Shared services to the offices\", \"kind\": \"distribution\", \"sender_cost_center_id\": \"$sender\", \"receivers\": [{\"cost_center_id\": \"$first\", \"percentage\": \"60\"}, {\"cost_center_id\": \"$second\", \"percentage\": \"40\"}]}"
  cycle=$(field .id)
  tag=$ETAG
  expect "the cycle's sender and receivers" "$(field '"\(.sender_cost_center_code) \([.receivers[] | "\(.cost_center_code) \(.percentage)"] | join(" "))"')" \
    "CC-1200 CC-1100 60.00 CC-1300 40.00"
  QUIET=1 api 200 '' listFiscalPeriods -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR" -p "ledger_id=$LEADING" -p page_size=100
  period=$(field "[.items[] | select(.is_special | not) | select(.start_date <= \"$TODAY\" and .end_date >= \"$TODAY\") | .period][0]")
  before_first=$(center_costs "$first" "$expense")
  say "She runs the cycle for this period, $YEAR/$period. One journal entry, dated the period's last day, credits the"
  say "sender with its cost and debits each receiver with its share on the same account. 60 percent of CAD 999.99 is"
  say "599.994 and 40 percent is 399.996: the shares are rounded so that they add up to the cost to the cent."
  api 200 '"cycle \(.allocation_cycle_code) run for \(.fiscal_year)/\(.fiscal_period): \(.run.amount.amount) \(.run.amount.currency) from \(.run.sender_cost_center_code) on \(.run.account_count) account, entry \(.run.journal_entry_number) dated \(.run.posting_date)"' \
    runAllocationCycle -p "id=$cycle" -if-match "$tag" -body "{\"fiscal_year\": $YEAR, \"fiscal_period\": $period}"
  expect "what the run moved" "$(field '"\(.posted) \(.run.amount.amount) \(.run.amount.currency) \(.run.sender_cost_center_code) \(.run.account_count) \(.run.kind)"')" \
    "true 999.99 CAD CC-1200 1 distribution"
  entry=$(field .run.journal_entry_id)
  api 200 '"entry \(.document_number) (\(.source_context)) on \(.posting_date): \([.lines[] | "\(.account_id | acct) \(.amount.amount) on cost center \(.cost_center_code)"] | join(" / "))"' \
    getJournalEntry -p "id=$entry"
  expect "the run's entry: the sender credited, the receivers debited with their shares, on one account" \
    "$(field '[.lines[] | "\(.account_id) \(.amount.amount) \(.cost_center_code)"] | join("; ")')" \
    "$expense -999.99 CC-1200; $expense 599.99 CC-1100; $expense 400.00 CC-1300"
  say "The centers' reports show the shares; the sender's cost of the period is gone:"
  say "  CC-1200: $(cents_plain "$(center_costs "$sender" "$expense")") CAD; CC-1100: $(cents_plain $(($(center_costs "$first" "$expense") - before_first))) CAD more than before the run; CC-1300: $(cents_plain "$(center_costs "$second" "$expense")") CAD"
  expect "the sender's cost left on the account, in cents" "$(center_costs "$sender" "$expense")" 0
  expect "the first receiver's share, in cents" "$(($(center_costs "$first" "$expense") - before_first))" 59999
  expect "the second receiver's share, in cents" "$(center_costs "$second" "$expense")" 40000
  say "A cycle is run once for a period:"
  refused 409 controlling.allocation_run_exists runAllocationCycle -p "id=$cycle" -if-match "$tag" -body "{\"fiscal_year\": $YEAR, \"fiscal_period\": $period}"
  api 200 '.items[] | "  run of \(.allocation_cycle_code) for \(.fiscal_year)/\(.fiscal_period): \(.amount.amount) \(.amount.currency) from \(.sender_cost_center_code), entry \(.journal_entry_number)"' \
    listAllocationRuns -p "allocation_cycle_id=$cycle"
  expect "the cycle's runs" "$(field '.items | length')" 1
  passed
}

# step_assessment: an assessment cycle and its run (ADR-0064 §4-§5, T60g):
# Ada adds a secondary cost element to Maple's chart through a governed
# change, an account only allocations post to; Sam sets up a cost center
# for the facilities, Alex posts its rent, and a manual entry on the new
# account is refused. Carla defines an assessment cycle that charges the
# facilities' costs to the finance office and the sales office at 70 and
# 30 percent and runs it for this period: one entry on the assessment
# account relieves the sender and charges the receivers, in one amount
# each, the shares adding up to the cost to the cent; the rent stays on
# its account. The centers' reports show the assessment account's lines,
# and a sender assessed for a period is not run for it again.
step_assessment() {
  heading "Step 53 — An assessment cycle: a cost center's costs of the period are charged to two others in one amount each on a secondary cost element, and stay on their accounts"
  local base first second sender expense bank secondary cycle tag period entry before_first before_second other
  as ada
  say "An assessment is posted on a secondary cost element (ADR-0064 §4): an account of the chart that only"
  say "allocations post to. Maple's chart has none, so Ada proposes account 6990, a change of the chart, tier 4, with"
  say "its evidence and two approvers:"
  api 200 '"active configuration version \(.items[0].number)"' listConfigVersions -p is_active=true
  base=$(field '.items[0].number')
  jq "{title: \"Assessment account\", rationale: \"Assessment cycles of the pilot company (T60g)\", base_version: $base,
    proposed: (.items[0].document | (.companies[] | select(.code == \"$CC\")) |= (.chart_of_accounts += [{\"number\": \"6990\", \"name\": \"Assessed facility costs\", \"type\": \"expense\", \"cost_element\": \"secondary\"}
      + if any(.chart_of_accounts[]; .group_account) then {group_account: \"G5000\"} else {} end]))}" "$WORK/last.json" >"$WORK/assessment.json"
  api 201 '"change request \(.number), \(.status): \(.title)"' createChangeRequest -body @assessment.json
  governed "$(field .id)" "the assessment account"
  as sam
  QUIET=1 api 200 '' listCostCenters -p "legal_entity_id=$LE" -p code=CC-1100
  first=$(field '.items[0].id')
  QUIET=1 api 200 '' listCostCenters -p "legal_entity_id=$LE" -p code=CC-1300
  second=$(field '.items[0].id')
  say "Sam sets up cost center CC-1400 for the facilities, whose costs the offices bear; Carla approves it."
  api 201 '"cost center \(.code) \(.name): \(.status)"' createCostCenter \
    -body "{\"legal_entity_id\": \"$LE\", \"code\": \"CC-1400\", \"name\": \"Facilities\", \"responsible\": \"Carla Controller\", \"category\": \"ADMIN\", \"valid_from\": \"$YEAR-01-01\", \"reason\": \"The offices' premises\"}"
  sender=$(field .id)
  center_approved sam
  as alex
  accounts
  expense=$(account 510000)
  bank=$(account 100000)
  secondary=$(account 6990)
  say "Alex posts this month's rent, CAD 1,000.01, to the facilities' cost center. A manual entry on the assessment"
  say "account is refused: only allocations post to it."
  api 201 '"journal entry \(.document_number): \([.lines[] | "\(.account_id | acct) \(.amount.amount) \(.amount.currency)\(if .cost_center_code then " on cost center \(.cost_center_code)" else "" end)"] | join(" / "))"' postJournalEntry \
    -body "{\"legal_entity_id\": \"$LE\", \"document_date\": \"$TODAY\", \"posting_date\": \"$TODAY\", \"currency\": \"CAD\", \"header_text\": \"Rent\", \"lines\": [{\"account_id\": \"$expense\", \"amount\": {\"amount\": \"1000.01\", \"currency\": \"CAD\"}, \"cost_center_id\": \"$sender\"}, {\"account_id\": \"$bank\", \"amount\": {\"amount\": \"-1000.01\", \"currency\": \"CAD\"}}]}"
  refused 422 ledger.secondary_account postJournalEntry \
    -body "{\"legal_entity_id\": \"$LE\", \"document_date\": \"$TODAY\", \"posting_date\": \"$TODAY\", \"currency\": \"CAD\", \"lines\": [{\"account_id\": \"$secondary\", \"amount\": {\"amount\": \"10.00\", \"currency\": \"CAD\"}, \"cost_center_id\": \"$first\"}, {\"account_id\": \"$bank\", \"amount\": {\"amount\": \"-10.00\", \"currency\": \"CAD\"}}]}"
  expect "the refused line" "$(field '.errors[0] | "\(.field) \(.code)"')" "lines[0].account_id secondary_cost_element"
  as carla
  say "Carla defines an assessment cycle: as a distribution's, one sender and receivers with percentages, and besides"
  say "the assessment account, which a cycle of this kind must name:"
  refused 422 controlling.allocation_cycle_invalid createAllocationCycle \
    -body "{\"legal_entity_id\": \"$LE\", \"code\": \"FACILITIES\", \"name\": \"Facilities to the offices\", \"kind\": \"assessment\", \"sender_cost_center_id\": \"$sender\", \"receivers\": [{\"cost_center_id\": \"$first\", \"percentage\": \"70\"}, {\"cost_center_id\": \"$second\", \"percentage\": \"30\"}]}"
  expect "why the cycle is refused" "$(field '.errors[0] | "\(.field) \(.code)"')" "assessment_account_id required"
  api 201 '"cycle \(.code) \(.name) (\(.kind) on account \(.assessment_account_number)): from \(.sender_cost_center_code) to \([.receivers[] | "\(.cost_center_code) \(.percentage) %"] | join(", "))"' createAllocationCycle \
    -body "{\"legal_entity_id\": \"$LE\", \"code\": \"FACILITIES\", \"name\": \"Facilities to the offices\", \"kind\": \"assessment\", \"assessment_account_id\": \"$secondary\", \"sender_cost_center_id\": \"$sender\", \"receivers\": [{\"cost_center_id\": \"$first\", \"percentage\": \"70\"}, {\"cost_center_id\": \"$second\", \"percentage\": \"30\"}]}"
  cycle=$(field .id)
  tag=$ETAG
  expect "the cycle's kind, account, sender and receivers" \
    "$(field '"\(.kind) \(.assessment_account_id) \(.assessment_account_number) \(.sender_cost_center_code) \([.receivers[] | "\(.cost_center_code) \(.percentage)"] | join(" "))"')" \
    "assessment $secondary 6990 CC-1400 CC-1100 70.00 CC-1300 30.00"
  QUIET=1 api 200 '' listFiscalPeriods -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR" -p "ledger_id=$LEADING" -p page_size=100
  period=$(field "[.items[] | select(.is_special | not) | select(.start_date <= \"$TODAY\" and .end_date >= \"$TODAY\") | .period][0]")
  before_first=$(center_costs "$first" "$secondary")
  before_second=$(center_costs "$second" "$secondary")
  say "She runs the cycle for this period, $YEAR/$period. One journal entry, dated the period's last day and all on the"
  say "assessment account, credits the sender with the total of its costs and debits each receiver with its share."
  say "70 percent of CAD 1,000.01 is 700.007 and 30 percent is 300.003: the shares are rounded so that they add up to"
  say "the total to the cent."
  api 200 '"cycle \(.allocation_cycle_code) run for \(.fiscal_year)/\(.fiscal_period): \(.run.amount.amount) \(.run.amount.currency) from \(.run.sender_cost_center_code), the costs of \(.run.account_count) account, entry \(.run.journal_entry_number) dated \(.run.posting_date)"' \
    runAllocationCycle -p "id=$cycle" -if-match "$tag" -body "{\"fiscal_year\": $YEAR, \"fiscal_period\": $period}"
  expect "what the run charged" "$(field '"\(.posted) \(.run.amount.amount) \(.run.amount.currency) \(.run.sender_cost_center_code) \(.run.account_count) \(.run.kind)"')" \
    "true 1000.01 CAD CC-1400 1 assessment"
  entry=$(field .run.journal_entry_id)
  api 200 '"entry \(.document_number) (\(.source_context)) on \(.posting_date): \([.lines[] | "\(.account_id | acct) \(.amount.amount) on cost center \(.cost_center_code)"] | join(" / "))"' \
    getJournalEntry -p "id=$entry"
  expect "the run's entry: the sender credited, the receivers debited with their shares, on the assessment account" \
    "$(field '[.lines[] | "\(.account_id) \(.amount.amount) \(.cost_center_code)"] | join("; ")')" \
    "$secondary -1000.01 CC-1400; $secondary 700.01 CC-1100; $secondary 300.00 CC-1300"
  say "The centers' reports show the assessment account's lines; the rent stays on its account, on the sender:"
  say "  CC-1400: account 510000 $(cents_plain "$(center_costs "$sender" "$expense")") CAD, account 6990 a credit of $(cents_plain $((-$(center_costs "$sender" "$secondary")))) CAD"
  say "  CC-1100: account 6990 $(cents_plain $(($(center_costs "$first" "$secondary") - before_first))) CAD; CC-1300: account 6990 $(cents_plain $(($(center_costs "$second" "$secondary") - before_second))) CAD"
  expect "the sender's rent left on its account, in cents" "$(center_costs "$sender" "$expense")" 100001
  expect "the sender relieved on the assessment account, in cents" "$(center_costs "$sender" "$secondary")" -100001
  expect "the first receiver charged on the assessment account, in cents" "$(($(center_costs "$first" "$secondary") - before_first))" 70001
  expect "the second receiver charged on the assessment account, in cents" "$(($(center_costs "$second" "$secondary") - before_second))" 30000
  say "The rent is still on the sender's account, so no cycle of the sender is run for the period again: a second"
  say "run, of this cycle or of a distribution of the same sender, would charge it twice."
  refused 409 controlling.allocation_run_exists runAllocationCycle -p "id=$cycle" -if-match "$tag" -body "{\"fiscal_year\": $YEAR, \"fiscal_period\": $period}"
  QUIET=1 api 201 '' createAllocationCycle \
    -body "{\"legal_entity_id\": \"$LE\", \"code\": \"FAC-MOVE\", \"name\": \"Facilities moved to the finance office\", \"kind\": \"distribution\", \"sender_cost_center_id\": \"$sender\", \"receivers\": [{\"cost_center_id\": \"$first\", \"percentage\": \"100\"}]}"
  other=$(field .id)
  refused 409 controlling.allocation_sender_assessed runAllocationCycle -p "id=$other" -if-match "$ETAG" -body "{\"fiscal_year\": $YEAR, \"fiscal_period\": $period}"
  api 200 '.items[] | "  run of \(.allocation_cycle_code) (\(.kind)) for \(.fiscal_year)/\(.fiscal_period): \(.amount.amount) \(.amount.currency) from \(.sender_cost_center_code), entry \(.journal_entry_number)"' \
    listAllocationRuns -p "allocation_cycle_id=$cycle"
  expect "the cycle's runs" "$(field '.items | length')" 1
  passed
}

# step_incoming_payment is T75c (ADR-0063 §6, ADR-0047): Northwind pays
# the invoice of step 48. RBC's third statement of the day shows the
# credit; Tom matches it as an incoming payment and posts it, so it waits
# as an open item on RBC's incoming payments clearing account 1015. Ruth
# finds it among the receipts not yet applied and applies it to the
# invoice, in full and exactly: one entry clears the receipt and the
# invoice, the customer has no open item left and nothing waits on 1015.
step_incoming_payment() {
  heading "Step 54 — Incoming payments: a statement credit matched as incoming waits on the bank's incoming account; applied in full to the customer's invoice, one entry clears both"
  local customer=$CUSTOMER invoice=$CUSTOMER_INVOICE item st receipt payment clearing body number reference
  [ -n "$customer" ] && [ -n "$invoice" ] && [ -n "$CUSTOMER_INVOICE_ENTRY" ] || fail "step 48's customer invoice is not there"
  as tom
  say "Northwind Traders pays the invoice of step 48, CAD 1,595.05. RBC's third statement of the day shows the credit;"
  say "Tom imports it:"
  rbc_incoming 1595.05
  api 201 '.statements[] | "statement \(.statement_ref): \(.status), \(.line_count) line, closing \(.closing_balance.amount) \(.closing_balance.currency)"' \
    importBankStatements -body "{\"house_bank_id\": \"$RBC\", \"format\": \"camt.053\"}" -file file=@camt053-incoming.xml
  st=$(field '.statements[0].id')
  if [ -z "$st" ] || [ "$st" = null ]; then fail "the statement was not imported"; fi
  expect "the statement follows the day's second" "$(field '.statements[0].status')" imported
  QUIET=1 api 200 '' getBankStatement -p "id=$st"
  say "No payment of Maple's explains a credit. Tom matches the line as an incoming payment and posts it: it debits"
  say "the RBC account 1000 and credits RBC's incoming payments clearing account 1015 (ADR-0047), where it waits"
  say "for accounts receivable. CAD 1,595.05 is within the limit that posts without the controller."
  api 201 '"line \(.line_no): \(.status) by hand (\(.kind)), incoming \(.targets[0].incoming): \(.reason)"' matchBankStatementLine -p "id=$st" -p line_no=1 \
    -if-match "$ETAG" -body '{"incoming": true, "reason": "Northwind Traders pays its invoice; the remittance names it."}'
  expect "the match's kind" "$(field .kind)" manual
  api 200 '"line \(.line_no): posted in journal entry \(.journal_entry_id[0:8])…"' postBankStatementLine -p "id=$st" -p line_no=1 -if-match "$ETAG"
  receipt=$(field .journal_entry_id)
  if [ -z "$receipt" ] || [ "$receipt" = null ]; then fail "the incoming line was not posted"; fi
  as ruth
  say "Ruth, the AR accountant, reads the receipts not yet applied (T75c): the statement's matches name each"
  say "posting, not whether receivables cleared it, so the open lines on the incoming accounts are listed here."
  say "Each names the statement line that posted it, where the payer and the remittance are read:"
  api 200 '.items[] | "  receipt \(.journal_entry_number): \(.amount.amount) \(.amount.currency) on \(.account_number) \(.account_name), house bank \(.house_bank_code), posted \(.posting_date), statement \(.bank_statement_ref) line \(.bank_statement_line_no)"' \
    listUnappliedReceipts -p "legal_entity_id=$LE" -p house_bank_code=RBC -p currency=CAD -p "posting_date_from=$TODAY"
  expect "the receipts not yet applied" "$(field '[.items[] | "\(.journal_entry_id) \(.amount.amount) \(.amount.currency) \(.account_number) \(.house_bank_code) \(.bank_statement_id) \(.bank_statement_line_no)"] | join(", ")')" \
    "$receipt 1595.05 CAD 1015 RBC $st 1"
  QUIET=1 api 200 '' getBankStatement -p "id=$st"
  say "  line 1 of that statement: $(field '.lines[0] | "\(.amount.amount) \(.amount.currency), \(.text // "")"')"
  item="{\"fiscal_year\": ${TODAY:0:4}, \"journal_entry_id\": \"$CUSTOMER_INVOICE_ENTRY\", \"line_no\": 1}"
  body="{\"legal_entity_id\": \"$LE\", \"customer_id\": \"@CUSTOMER@\", \"receipt_journal_entry_id\": \"$receipt\", \"posting_date\": \"$TODAY\", \"items\": [$item]}"
  say "A payment is applied to one customer's items. Naming a supplier as the customer is refused, the item named,"
  say "and nothing is posted:"
  refused 422 ledger.not_the_customers_item createIncomingPayment -body "${body//@CUSTOMER@/$SUPPLIER}"
  expect "the item refused" "$(field '.errors[0].field')" "items[0]"
  say "Ruth applies the receipt to Northwind's invoice. The invoice equals the receipt exactly (plan default 4: no"
  say "partial payment, difference or discount), so one entry clears both:"
  api 201 '"incoming payment \(.document_number) of \(.customer_number) \(.customer_name), \(.company_code) \(.company_name): \(.amount.amount) \(.amount.currency) on \(.posting_date)", "  receipt: journal entry \(.receipt_journal_entry_number) on \(.receipt_account_number) \(.receipt_account_name)", "  clearing entry: \(.journal_entry_number)", (.items[] | "  item \(.item_no): journal entry \(.journal_entry_number), reference \(.reference), \(.amount.amount) \(.amount.currency)")' \
    createIncomingPayment -body "${body//@CUSTOMER@/$customer}"
  payment=$(field .id)
  clearing=$(field .journal_entry_id)
  expect "the payment's amount" "$(field '"\(.amount.amount) \(.amount.currency) \(.posting_date)"')" "1595.05 CAD $TODAY"
  expect "the payment's customer and receipt" "$(field '"\(.customer_id) \(.customer_name) \(.receipt_journal_entry_id) \(.receipt_account_number) \(.receipt_bank_statement_id) \(.receipt_bank_statement_line_no)"')" \
    "$customer Northwind Traders $receipt 1015 $st 1"
  expect "the item applied" "$(field '[.items[] | "\(.journal_entry_id) \(.line_no) \(.amount.amount)"] | join(", ")')" "$CUSTOMER_INVOICE_ENTRY 1 1595.05"
  number=$(field .document_number)
  QUIET=1 api 200 '' getCustomerInvoice -p "id=$invoice"
  reference=$(field .document_number)
  say "Read back, it is the same document; its item names the invoice by its number:"
  api 200 '"incoming payment \(.document_number): \(.amount.amount) \(.amount.currency), item 1 reference \(.items[0].reference)"' getIncomingPayment -p "id=$payment"
  expect "read back" "$(field '"\(.document_number) \(.amount.amount) \(.journal_entry_id) \(.items[0].reference)"')" "$number 1595.05 $clearing $reference"
  say "The clearing entry, document type DZ, names receivables as its source and the payment as its document. It"
  say "debits 1015 and credits Northwind on 1200, each at the amount it was booked at:"
  api 200 '"journal entry \(.document_number), \(.document_type), source \(.source_context)", (.lines[] | "  \(.account_id | acct): \(.amount.amount) \(.amount.currency)")' \
    getJournalEntry -p "id=$clearing"
  expect "the clearing entry's source" "$(field '"\(.document_type) \(.source_context) \(.source_document_id)"')" "DZ receivables $payment"
  expect "the clearing entry's amounts" "$(field '[.lines[].amount.amount] | join(" ")')" "1595.05 -1595.05"
  expect "the clearing entry's accounts" "$(field '[.lines[].account_id] | join(" ")')" "$(account 1015) $(account 1200)"
  expect "the receivable line's partner" "$(field '.lines[1].partner_id')" "$customer"
  say "Northwind has no open item left, and nothing waits on the incoming account:"
  QUIET=1 api 200 '' listOpenItems -p "legal_entity_id=$LE" -p "partner_id=$customer"
  expect "the customer's open items" "$(field '.items | length')" 0
  QUIET=1 api 200 '' listUnappliedReceipts -p "legal_entity_id=$LE"
  expect "the receipts not yet applied" "$(field '.items | length')" 0
  say "Applying the same receipt again changes nothing: it is no longer open."
  refused 409 receivables.receipt_already_applied createIncomingPayment -body "${body//@CUSTOMER@/$customer}"
  api 200 '.items[] | "  \(.document_number) \(.customer_name): \(.amount.amount) \(.amount.currency), clearing entry \(.journal_entry_number)"' \
    listIncomingPayments -p "customer_id=$customer"
  expect "the customer's incoming payments" "$(field '[.items[].id] | join(" ")')" "$payment"
  passed
}

# step_customer_credit is T75d (ADR-0063 §7): Maple invoices Northwind a
# second time and credits part of it. The credit memo is the customer
# invoice document with the kind credit_memo, naming the invoice it
# credits; it posts with the signs turned, as a DG entry. Invoice and memo
# are Northwind's open items, net what Northwind owes. The credit memos
# naming an invoice may not exceed its gross amount. The invoice and the
# memo stay open for step_ar_aging: CREDITED_INVOICE and CREDIT_MEMO are
# the documents, CREDITED_INVOICE_ENTRY and CREDIT_MEMO_ENTRY their entries.
step_customer_credit() {
  heading "Step 55 — Customer credit memos: a second invoice partly credited by a credit memo naming it, posted with the signs turned; invoice and memo are the customer's open items, net what the customer owes"
  local customer=$CUSTOMER sales invoice number entry memo memo_entry over due item
  [ -n "$customer" ] || fail "step 47's customer is not there"
  as ruth
  sales=$(account 4000)
  due=$(jq -rn --arg d "$TODAY" '$d | strptime("%Y-%m-%d") | mktime + 30 * 86400 | strftime("%Y-%m-%d")')
  say "Maple invoices Northwind Traders for machined parts, CAD 2,000.00 and 5% GST. Ruth enters the invoice and"
  say "posts it:"
  item="{\"description\": \"Machined parts\", \"account_id\": \"$sales\", \"net_amount\": {\"amount\": \"@NET@\", \"currency\": \"CAD\"}, \"tax_code\": \"GO5\"}"
  api 201 '"customer invoice \(.document_number) to \(.customer_name): \(.status), net \(.net_amount.amount), tax \(.tax_amount.amount), gross \(.gross_amount.amount) \(.currency)"' \
    createCustomerInvoice -body "{\"legal_entity_id\": \"$LE\", \"customer_id\": \"$customer\", \"invoice_date\": \"$TODAY\", \"currency\": \"CAD\", \"items\": [${item/@NET@/2000.00}]}"
  invoice=$(field .id)
  number=$(field .document_number)
  api 200 '"customer invoice \(.document_number): \(.status) as journal entry \(.journal_entry_number), due \(.net_due_date)"' \
    postCustomerInvoice -p "id=$invoice" -if-match "$ETAG" -body "{\"posting_date\": \"$TODAY\"}"
  entry=$(field .journal_entry_id)
  expect "the posted invoice" "$(field '"\(.status) \(.gross_amount.amount) \(.net_due_date)"')" "posted 2100.00 $due"
  say "A fifth of the parts are returned. A posted customer invoice is not reversed; Ruth enters a credit memo, the"
  say "same document with the kind credit_memo, naming the invoice it credits (T75d). Its amounts are more than"
  say "zero, the kind gives their direction, and its tax is at its own date's rates:"
  item=${item/Machined parts/Machined parts returned}
  api 201 '"\(.kind) \(.document_number) to \(.customer_name), crediting invoice \(.reference_invoice_number): \(.status), net \(.net_amount.amount), tax \(.tax_amount.amount), gross \(.gross_amount.amount) \(.currency)"' \
    createCustomerInvoice -body "{\"legal_entity_id\": \"$LE\", \"customer_id\": \"$customer\", \"kind\": \"credit_memo\", \"reference_invoice_id\": \"$invoice\", \"invoice_date\": \"$TODAY\", \"currency\": \"CAD\", \"items\": [${item/@NET@/400.00}]}"
  memo=$(field .id)
  expect "the credit memo" "$(field '"\(.kind) \(.status) \(.reference_invoice_id) \(.reference_invoice_number) \(.net_amount.amount) \(.tax_amount.amount) \(.gross_amount.amount) \(.gross_amount.currency)"')" \
    "credit_memo draft $invoice $number 400.00 20.00 420.00 CAD"
  say "Ruth posts it. It names no payment term, so its open item keeps its invoice's and is due when the invoice is:"
  api 200 '"\(.kind) \(.document_number): \(.status) on \(.posting_date) as journal entry \(.journal_entry_number), terms \(.payment_term_code), due \(.net_due_date)"' \
    postCustomerInvoice -p "id=$memo" -if-match "$ETAG" -body "{\"posting_date\": \"$TODAY\"}"
  expect "the posted credit memo" "$(field '"\(.status) \(.posting_date) \(.payment_term_code) \(.net_due_date) \(.gross_amount.amount)"')" "posted $TODAY NT30 $due 420.00"
  memo_entry=$(field .journal_entry_id)
  say "Its entry, document type DG, turns the invoice's signs: it credits Northwind on 1200 Trade receivables with"
  say "the gross and debits 4000 Sales and 2300 GST/HST payable:"
  api 200 '"journal entry \(.document_number), \(.document_type), source \(.source_context)", (.lines[] | "  \(.account_id | acct): \(.amount.amount) \(.amount.currency)")' \
    getJournalEntry -p "id=$memo_entry"
  expect "the entry's source" "$(field '"\(.document_type) \(.source_context) \(.source_document_id)"')" "DG receivables $memo"
  expect "the entry's amounts" "$(field '[.lines[].amount.amount] | join(" ")')" "-420.00 400.00 20.00"
  expect "the entry's accounts" "$(field '[.lines[].account_id] | join(" ")')" "$(account 1200) $sales $(account 2300)"
  expect "the receivable line's partner" "$(field '.lines[0].partner_id')" "$customer"
  say "Invoice and credit memo are Northwind's open items; together they are what Northwind owes, CAD 1,680.00:"
  api 200 '(.items[] | "  open item \(.document_number): \(.amount.amount) \(.amount.currency), due \(.payment_terms.net_due_date)"), "  net: \([.items[].amount.amount | sub("\\."; "") | tonumber] | add | tostring | .[:-2] + "." + .[-2:]) CAD"' \
    listOpenItems -p "legal_entity_id=$LE" -p "partner_id=$customer"
  expect "the customer's open items" "$(field '[.items[] | "\(.journal_entry_id) \(.amount.amount) \(.amount.currency) \(.payment_terms.net_due_date)"] | sort | join(", ")')" \
    "$(printf '%s\n' "$entry 2100.00 CAD $due" "$memo_entry -420.00 CAD $due" | sort | paste -sd, - | sed 's/,/, /g')"
  expect "what the customer owes" "$(field '[.items[].amount.amount | sub("\\."; "") | tonumber] | add | tostring | .[:-2] + "." + .[-2:]')" 1680.00
  say "The credit memos naming an invoice may not exceed its gross amount (plan default 5, as the product owner"
  say "decided for supplier credit memos). A second memo of CAD 1,680.01 gross would credit 2,100.01 against the"
  say "invoice's 2,100.00: it is entered, and refused when it posts, with the figures:"
  QUIET=1 api 201 '' createCustomerInvoice \
    -body "{\"legal_entity_id\": \"$LE\", \"customer_id\": \"$customer\", \"kind\": \"credit_memo\", \"reference_invoice_id\": \"$invoice\", \"invoice_date\": \"$TODAY\", \"currency\": \"CAD\", \"items\": [{\"account_id\": \"$sales\", \"net_amount\": {\"amount\": \"1680.01\", \"currency\": \"CAD\"}}]}"
  over=$(field .id)
  refused 422 receivables.credit_exceeds_invoice postCustomerInvoice -p "id=$over" -if-match "$ETAG" -body "{\"posting_date\": \"$TODAY\"}"
  expect "the refusal's figures" "$(field '.credit_limit | "\(.invoice_number) \(.invoice_gross.amount) \(.credited.amount) \(.attempted.amount) \(.attempted.currency)"')" \
    "$number 2100.00 420.00 1680.01 CAD"
  api 200 '.items[] | "  \(.kind) \(.document_number): \(.status), \(.gross_amount.amount) \(.gross_amount.currency)"' \
    listCustomerInvoices -p "reference_invoice_id=$invoice" -p kind=credit_memo
  expect "the credit memos naming the invoice" "$(field '[.items[] | "\(.status) \(.id)"] | sort | join(", ")')" "draft $over, posted $memo"
  CREDITED_INVOICE=$invoice CREDITED_INVOICE_ENTRY=$entry CREDIT_MEMO=$memo CREDIT_MEMO_ENTRY=$memo_entry
  passed
}

# step_ar_aging is T75e (ADR-0063 §10): the aging of Maple's customer open
# items on a key date, per customer and in total, not yet due and in
# buckets of days overdue. The invoice and the credit memo of
# step_customer_credit are Northwind's open items, due together 30 days
# from today; the key dates are counted from that day, so the step never
# depends on the day it runs. The totals are the open items of the
# customer reconciliation account 1200.
step_ar_aging() {
  heading "Step 56 — AR aging: the customer open items on a key date, per customer and in total, not yet due and in buckets of days overdue; the credit memo reduces its customer's bucket and total"
  local customer=$CUSTOMER due later row cents
  [ -n "$customer" ] && [ -n "$CREDITED_INVOICE_ENTRY" ] && [ -n "$CREDIT_MEMO_ENTRY" ] || fail "step 55's invoice and credit memo are not there"
  as ruth
  due=$(jq -rn --arg d "$TODAY" '$d | strptime("%Y-%m-%d") | mktime + 30 * 86400 | strftime("%Y-%m-%d")')
  later=$(jq -rn --arg d "$TODAY" '$d | strptime("%Y-%m-%d") | mktime + 45 * 86400 | strftime("%Y-%m-%d")')
  row='"  \(.customer_number) \(.customer_name): \(.item_count) items, \(.amount.amount) \(.amount.currency)", (.buckets[] | "    \(.bucket)\(if .days_from then " \(.days_from)-\(.days_to // "") days" else "" end): \(.item_count) items, \(.amount.amount) \(.amount.currency)")'
  say "Ruth reads the aging of Maple's customer open items (T75e). It is computed on read from the ledger's open"
  say "items on customer reconciliation accounts; each item falls in one bucket by its net due date. On $due,"
  say "the day Northwind's invoice of step 55 and its credit memo are due, neither is overdue: an item due on the"
  say "key date is not yet due. The credit memo reduces Northwind's bucket and total to what it owes, CAD 1,680.00:"
  api 200 '"aging of \(.company_code) \(.company_name) on \(.key_date), in \(.currency), buckets to \(.aging_days | map(tostring) | join(", ")) days", (.items[] | '"$row"')' \
    getReceivablesAging -p "legal_entity_id=$LE" -p "key_date=$due" -p aging_days=30,60 -p "customer_id=$customer"
  expect "the report's company and currency" "$(field '"\(.company_code) \(.key_date) \(.currency) \(.aging_days | map(tostring) | join(","))"')" "$CC $due CAD 30,60"
  expect "Northwind's row" "$(field '.items[] | "\(.customer_id) \(.customer_name) \(.item_count) \(.amount.amount) \(.amount.currency)"')" \
    "$customer Northwind Traders 2 1680.00 CAD"
  expect "Northwind's buckets" "$(field '[.items[0].buckets[] | "\(.bucket) \(.days_from // "-") \(.days_to // "-") \(.item_count) \(.amount.amount)"] | join(", ")')" \
    "not_due - - 2 1680.00, overdue 1 30 0 0.00, overdue 31 60 0 0.00, overdue 61 - 0 0.00, no_due_date - - 0 0.00"
  expect "the totals with one customer selected" "$(field '.totals | "\(.customer_count) \(.item_count) \(.amount.amount) \(.amount.currency)"')" "1 2 1680.00 CAD"
  say "Fifteen days later, on $later, both are 15 days overdue and fall in the bucket of 1 to 30 days. Without a"
  say "customer the report has a row per customer with open items and the totals of them all:"
  api 200 '"aging of \(.company_code) on \(.key_date)", (.items[] | '"$row"'), "  totals: \(.totals.customer_count) customers, \(.totals.item_count) items, \(.totals.amount.amount) \(.totals.amount.currency)", (.totals.buckets[] | "    \(.bucket)\(if .days_from then " \(.days_from)-\(.days_to // "") days" else "" end): \(.item_count) items, \(.amount.amount) \(.amount.currency)")' \
    getReceivablesAging -p "legal_entity_id=$LE" -p "key_date=$later" -p aging_days=30,60
  expect "Northwind's row, 15 days overdue" \
    "$(field ".items[] | select(.customer_id == \"$customer\") | [.buckets[] | \"\(.bucket) \(.days_from // \"-\") \(.days_to // \"-\") \(.item_count) \(.amount.amount)\"] | join(\", \")")" \
    "not_due - - 0 0.00, overdue 1 30 2 1680.00, overdue 31 60 0 0.00, overdue 61 - 0 0.00, no_due_date - - 0 0.00"
  expect "the buckets add up to the total" "$(field '([.totals.buckets[].amount.amount | sub("\\."; "") | tonumber] | add) == (.totals.amount.amount | sub("\\."; "") | tonumber)')" true
  expect "the customers' rows add up to the total" "$(field '([.items[].amount.amount | sub("\\."; "") | tonumber] | add) == (.totals.amount.amount | sub("\\."; "") | tonumber)')" true
  cents=$(field '.totals.amount.amount | sub("\\."; "") | tonumber')
  say "The totals are the open items of the customer reconciliation account 1200, as the open items list ages them:"
  api 200 '.aging[] | "  open items on 1200, \(.bucket)\(if .days_from then " \(.days_from)-\(.days_to // "") days" else "" end): \(.item_count) items, \(.amount_lc.amount) \(.currency)"' \
    listOpenItems -p "legal_entity_id=$LE" -p "account_id=$(account 1200)" -p "aging_key_date=$later" -p aging_days=30,60
  expect "the open items of account 1200" "$(field '[.aging[].amount_lc.amount | sub("\\."; "") | tonumber] | add')" "$cents"
  say "A key date outside the years 1900 to 2999 is refused, the field named:"
  refused 422 receivables.key_date_out_of_range getReceivablesAging -p "legal_entity_id=$LE" -p key_date=3000-01-01
  expect "the field refused" "$(field '.errors[0].field')" key_date
  passed
}

# rbc_incoming AMOUNT [SEQUENCE OPENING] writes a camt.053 statement of RBC
# of today: a customer's payment of the amount. By default it is the
# day's third, from the second statement's closing balance (rbc_receipt's:
# step 8's closing and Northwind's CAD 25,000.00); a later one names its
# sequence number and its opening balance, the closing of the one before.
rbc_incoming() {
  local opening closing seq=${2:-3}
  opening=${3:-$(amount $(($(cents "$CLOSING") + 2500000)))}
  closing=$(amount $(($(cents "$opening") + $(cents "$1"))))
  {
    printf '<?xml version="1.0" encoding="UTF-8"?>\n'
    printf '<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.02"><BkToCstmrStmt>\n'
    printf '<GrpHdr><MsgId>RBC-%s-%s</MsgId><CreDtTm>%sT23:45:00</CreDtTm></GrpHdr>\n' "$TODAY" "$seq" "$TODAY"
    printf '<Stmt><Id>RBC-1000001-%s-%s</Id><ElctrncSeqNb>%s</ElctrncSeqNb><Acct><Id><Othr><Id>1000001</Id></Othr></Id><Ccy>CAD</Ccy></Acct>\n' "$TODAY" "$seq" "$seq"
    printf '<Bal><Tp><CdOrPrtry><Cd>OPBD</Cd></CdOrPrtry></Tp><Amt Ccy="CAD">%s</Amt><CdtDbtInd>CRDT</CdtDbtInd><Dt><Dt>%s</Dt></Dt></Bal>\n' "$opening" "$TODAY"
    printf '<Bal><Tp><CdOrPrtry><Cd>CLBD</Cd></CdOrPrtry></Tp><Amt Ccy="CAD">%s</Amt><CdtDbtInd>CRDT</CdtDbtInd><Dt><Dt>%s</Dt></Dt></Bal>\n' "$closing" "$TODAY"
    printf '<Ntry><Amt Ccy="CAD">%s</Amt><CdtDbtInd>CRDT</CdtDbtInd><Sts>BOOK</Sts><BookgDt><Dt>%s</Dt></BookgDt><ValDt><Dt>%s</Dt></ValDt><BkTxCd><Domn><Cd>PMNT</Cd><Fmly><Cd>RCDT</Cd><SubFmlyCd>ESCT</SubFmlyCd></Fmly></Domn></BkTxCd><NtryDtls><TxDtls><RltdPties><Dbtr><Nm>Northwind Traders</Nm></Dbtr></RltdPties><RmtInf><Ustrd>Customer invoice</Ustrd></RmtInf></TxDtls></NtryDtls></Ntry>\n' "$1" "$TODAY" "$TODAY"
    printf '</Stmt></BkToCstmrStmt></Document>\n'
  } >"$WORK/camt053-incoming.xml"
}

# step_dunning is T76a and T76b (ADR-0065 §2-§5, §8): Ruth posts a second
# invoice to Northwind; Ada gives Maple its dunning levels
# through a tier 2 change, a run before them refused; Ruth starts a
# dunning run at a key date ten days past the invoice's due date and reads
# its proposal, one notice for Northwind at level 1 with the invoice's open
# item among its items; a run at the due date itself proposes nothing; the customer's
# open items are unchanged. She confirms the run, once: a second
# confirmation is refused, and so is a second proposal of the same key
# date, stale now that the first recorded the items' levels. A run twenty
# days later proposes the invoice's item at level 2. The key dates are the
# requests': nothing here depends on today's date. The step duns its own
# invoice, so it holds whether or not the invoice of step 48 is still open.
step_dunning() {
  heading "Step 57 — Dunning: the company's dunning levels through a governed change; a dunning run at a key date after a customer invoice's due date proposes one notice for the customer at level 1; nothing changes on the open items; the run is confirmed once, a stale proposal is refused, and the next run proposes level 2"
  local customer=$CUSTOMER entry due key later base cr request run before twin twin_tag
  due=$(jq -rn --arg d "$TODAY" '$d | strptime("%Y-%m-%d") | mktime + 30 * 86400 | strftime("%Y-%m-%d")')
  key=$(jq -rn --arg d "$TODAY" '$d | strptime("%Y-%m-%d") | mktime + 40 * 86400 | strftime("%Y-%m-%d")')
  later=$(jq -rn --arg d "$TODAY" '$d | strptime("%Y-%m-%d") | mktime + 60 * 86400 | strftime("%Y-%m-%d")')
  as ruth
  say "Ruth enters and posts another invoice to Northwind Traders, for a spare part, on Northwind's terms, NT30:"
  QUIET=1 api 201 '' createCustomerInvoice -body "{\"legal_entity_id\": \"$LE\", \"customer_id\": \"$customer\", \"invoice_date\": \"$TODAY\", \"currency\": \"CAD\",
    \"items\": [{\"description\": \"Spindle bearing\", \"account_id\": \"$(account 4000)\", \"net_amount\": {\"amount\": \"400.00\", \"currency\": \"CAD\"}}]}"
  api 200 '"customer invoice \(.document_number) to \(.customer_name): \(.status), \(.gross_amount.amount) \(.gross_amount.currency), due \(.net_due_date)"' \
    postCustomerInvoice -p "id=$(field .id)" -if-match "$ETAG" -body "{\"posting_date\": \"$TODAY\"}"
  expect "the invoice's due date" "$(field .net_due_date)" "$due"
  entry=$(field .journal_entry_id)
  say "It is due on $due. Ruth, who also chases Maple's customers, wants to see what a dunning run would propose"
  say "ten days later, on $key. Maple has no dunning levels yet, so it has no dunning:"
  refused 422 collections.no_dunning_levels createDunningRun -body "{\"legal_entity_id\": \"$LE\", \"key_date\": \"$key\"}"
  as ada
  say "Dunning levels are the tenant's own configuration (ADR-0065 §2); no country pack ships any. Ada proposes"
  say "Maple's: level 1 from 5 days overdue, level 2 from 20, level 3 from 40. They post nothing, so the change is"
  say "tier 2, approved by the process owner:"
  QUIET=1 api 200 '' listConfigVersions -p is_active=true
  base=$(field '.items[0].number')
  jq "{title: \"Dunning levels\", rationale: \"Maple's dunning (T76a)\", base_version: $base,
    proposed: (.items[0].document | .companies |= map(if .code == \"$CC\" then .dunning = {levels: [
      {level: 1, days_overdue: 5}, {level: 2, days_overdue: 20}, {level: 3, days_overdue: 40}]} else . end))}" "$WORK/last.json" >"$WORK/dunning.json"
  api 201 '"change request \(.number), \(.status): \(.title)"' createChangeRequest -body @dunning.json
  cr=$(field .id)
  api 200 '"\(.status), tier \(.tier): \([.items[].path] | join(", "))"' submitChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "the change's tier" "$(field .tier)" 2
  expect "the levels among the changes" "$(field '[.items[] | select(.path | startswith("companies['"$CC"'].dunning.levels["))] | length')" 3
  request=$(field .workflow_request_id)
  approve priya "$request" "The dunning levels, as agreed with the controller"
  as ada
  QUIET=1 api 200 '' getChangeRequest -p "id=$cr"
  api 200 '"\(.status): configuration version \(.activated_version)"' activateChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "status after activation" "$(field .status)" active
  as ruth
  QUIET=1 api 200 '' listOpenItems -p "legal_entity_id=$LE" -p "partner_id=$customer"
  before=$(field '[.items[] | "\(.journal_entry_id) \(.amount.amount) \(.amount.currency) \(.payment_terms.net_due_date)"] | join(", ")')
  say "Ruth starts the run at the key date $key. It is a proposal: per customer, the open items overdue on the key"
  say "date and the dunning level each would reach. Nothing is sent and nothing changes on the items:"
  api 201 '"dunning run of \(.company_code) \(.company_name) at \(.key_date): \(.status), \(.notice_count) notice, \(.item_count) item(s), \(.overdue_amount.amount) \(.overdue_amount.currency) overdue"' \
    createDunningRun -body "{\"legal_entity_id\": \"$LE\", \"key_date\": \"$key\"}"
  run=$(field .id)
  expect "the run" "$(field '"\(.status) \(.key_date) \(.notice_count) \(.overdue_amount.currency)"')" "proposed $key 1 CAD"
  say "Its one notice is Northwind's. The new invoice's open item, ten days overdue, has reached the first level's"
  say "five days. An item never dunned reaches level 1 however long it is overdue; it rises one level per confirmed run:"
  api 200 '.items[] | "notice for \(.customer_number) \(.customer_name): level \(.level), \(.item_count) item(s), \(.overdue_amount.amount) \(.overdue_amount.currency)", (.items[] | "  item \(.item_no): entry \(.document_number) (\(.reference)), due \(.net_due_date), \(.days_overdue) days overdue, \(.amount.amount) \(.amount.currency), level \(.previous_level) -> \(.level)")' \
    listDunningNotices -p "id=$run"
  expect "the notices" "$(field '[.items[] | "\(.customer_id) \(.customer_name) \(.level) \(.item_count == (.items | length)) \(.overdue_amount.currency)"] | join(", ")')" \
    "$customer Northwind Traders 1 true CAD"
  expect "the invoice's item in the notice" \
    "$(field "[.items[].items[] | select(.journal_entry_id == \"$entry\") | \"\(.net_due_date) \(.days_overdue) \(.amount.amount) \(.amount.currency) \(.local_amount.amount) \(.previous_level) \(.level)\"] | join(\", \")")" \
    "$due 10 400.00 CAD 400.00 0 1"
  expect "every item at level 1" "$(field '[.items[].items[].level] | unique | join(" ")')" 1
  say "Read back, the run is the same, and it is among Maple's runs:"
  api 200 '"dunning run at \(.key_date): \(.status), \(.notice_count) notice"' getDunningRun -p "id=$run"
  expect "read back" "$(field '"\(.id) \(.status) \(.notice_count)"')" "$run proposed 1"
  QUIET=1 api 200 '' listDunningRuns -p "legal_entity_id=$LE" -p "key_date_from=$key" -p "key_date_to=$key"
  expect "Maple's runs at the key date" "$(field '[.items[].id] | join(" ")')" "$run"
  say "On the due date itself the invoice is not overdue, so a run at $due proposes nothing:"
  api 201 '"dunning run at \(.key_date): \(.status), \(.notice_count) notices"' createDunningRun -body "{\"legal_entity_id\": \"$LE\", \"key_date\": \"$due\"}"
  expect "the run at the due date" "$(field '"\(.notice_count) \(.item_count) \(.overdue_amount.amount)"')" "0 0 0.00"
  say "The customer's open items are as they were before the runs:"
  api 200 '.items[] | "  open item \(.document_number): \(.amount.amount) \(.amount.currency), due \(.payment_terms.net_due_date)"' \
    listOpenItems -p "legal_entity_id=$LE" -p "partner_id=$customer"
  expect "the customer's open items" "$(field '[.items[] | "\(.journal_entry_id) \(.amount.amount) \(.amount.currency) \(.payment_terms.net_due_date)"] | join(", ")')" "$before"
  say "A colleague had started a proposal of her own at the same key date. Ruth confirms hers (T76b, ADR-0065 §8):"
  say "each item's level and its dunning date, the key date, are recorded, and one event per customer is published,"
  say "naming the customer by id and number. Nothing is posted, and no letter is sent:"
  QUIET=1 api 201 '' createDunningRun -body "{\"legal_entity_id\": \"$LE\", \"key_date\": \"$key\"}"
  twin=$(field .id) twin_tag=$ETAG
  QUIET=1 api 200 '' getDunningRun -p "id=$run"
  api 200 '"dunning run at \(.key_date): \(.status), \(.notice_count) notice, \(.item_count) item(s), version \(.version)"' \
    confirmDunningRun -p "id=$run" -if-match "$ETAG"
  expect "the confirmed run" "$(field '"\(.id) \(.status) \(.version)"')" "$run confirmed 2"
  # What step_collections reads back: the customer dunned, when, and for how much.
  DUNNED_CUSTOMER=$customer DUNNING_KEY=$key DUNNING_DUE=$due DUNNED_AMOUNT=$(field '"\(.overdue_amount.amount) \(.overdue_amount.currency)"')
  say "A run is confirmed once: confirming it again records and publishes nothing."
  refused 409 collections.invalid_status confirmDunningRun -p "id=$run" -if-match "$ETAG"
  say "The colleague's proposal still says Northwind's items were never dunned. It is stale, and a new run is started instead:"
  refused 409 collections.proposal_stale confirmDunningRun -p "id=$twin" -if-match "$twin_tag"
  QUIET=1 api 200 '' getDunningRun -p "id=$twin"
  expect "the stale run" "$(field .status)" proposed
  say "Twenty days later, on $later, the invoice is 30 days overdue: past the second level's 20 days. The next run"
  say "reads the level recorded for each item and proposes the next one, a level per confirmed run at most:"
  api 201 '"dunning run at \(.key_date): \(.status), \(.notice_count) notice, \(.overdue_amount.amount) \(.overdue_amount.currency) overdue"' \
    createDunningRun -body "{\"legal_entity_id\": \"$LE\", \"key_date\": \"$later\"}"
  api 200 '.items[] | "notice for \(.customer_number) \(.customer_name): level \(.level)", (.items[] | "  item \(.item_no): entry \(.document_number) (\(.reference)), \(.days_overdue) days overdue, level \(.previous_level) -> \(.level)")' \
    listDunningNotices -p "id=$(field .id)" -p "customer_id=$customer"
  expect "Northwind's notice in the second run" "$(field '[.items[] | "\(.customer_id) \(.level)"] | join(", ")')" "$customer 2"
  expect "the invoice's item in the second run" \
    "$(field "[.items[].items[] | select(.journal_entry_id == \"$entry\") | \"\(.days_overdue) \(.previous_level) \(.level)\"] | join(\", \")")" "30 1 2"
  passed
}

# step_collections is T76c (ADR-0065 §9): Ruth reads Maple's collection
# worklist at the key date of the dunning run she confirmed in
# step_dunning: Northwind, the customer dunned, leads it with the overdue
# amount of its notice, the due date of its items, dunning level 1 and
# the run's key date as its last dunning date; the worklist's totals
# count every customer listed. On the due date itself nothing of
# Northwind's is overdue, so it is not listed. The key dates are the
# requests'.
step_collections() {
  heading "Step 58 — The collection worklist: the company's customers with overdue items on a key date, the largest overdue amount first, each with its open and overdue amounts, oldest due date, dunning level and last dunning date"
  local customer=$DUNNED_CUSTOMER key=$DUNNING_KEY
  as ruth
  say "Ruth reads whom to chase: Maple's customers with items overdue on $key, the largest overdue amount first."
  say "The worklist is computed from the ledger's open items and the dunning levels recorded; it stores nothing:"
  api 200 '"worklist of \(.company_code) \(.company_name) at \(.key_date): \(.customer_count) customer(s), \(.open_amount.amount) \(.open_amount.currency) open, \(.overdue_amount.amount) \(.overdue_amount.currency) overdue", (.items[] | "  \(.customer_number) \(.customer_name): \(.overdue_amount.amount) \(.overdue_amount.currency) overdue of \(.open_amount.amount) open, oldest due \(.oldest_due_date), dunning level \(.dunning_level), last dunned \(.last_dunning_date // "never")")' \
    getCollectionWorklist -p "legal_entity_id=$LE" -p "key_date=$key"
  expect "the customer leading the worklist" "$(field '.items[0] | "\(.customer_id) \(.customer_name)"')" "$customer Northwind Traders"
  expect "its overdue amount, the confirmed notice's" "$(field '.items[0] | "\(.overdue_amount.amount) \(.overdue_amount.currency)"')" "$DUNNED_AMOUNT"
  expect "its oldest due date, dunning level and last dunning date" \
    "$(field '.items[0] | "\(.oldest_due_date) \(.dunning_level) \(.last_dunning_date)"')" "$DUNNING_DUE 1 $key"
  expect "the largest overdue amount first" "$(field '[.items[].overdue_amount.amount | tonumber] | . == (sort | reverse)')" true
  expect "the totals count every customer" "$(field '.customer_count == (.items | length) and .currency == "CAD"')" true
  expect "its open amount, the overdue amount less the credit memo of step 55" "$(cents "$(field '.items[0].open_amount.amount')")" \
    "$(($(cents "${DUNNED_AMOUNT% *}") - 42000))"
  say "Northwind's open amount nets the credit memo of step 55, CAD 420.00, which reduces nothing overdue (ADR-0065 §4),"
  say "so less is open than overdue. On the due date itself, $DUNNING_DUE, nothing of Northwind's is overdue yet, and a"
  say "customer with nothing overdue is not on the worklist:"
  api 200 '"worklist at \(.key_date): \(.customer_count) customer(s), \(.overdue_amount.amount) \(.overdue_amount.currency) overdue"' \
    getCollectionWorklist -p "legal_entity_id=$LE" -p "key_date=$DUNNING_DUE"
  expect "Northwind on the worklist at the due date" "$(field "[.items[] | select(.customer_id == \"$customer\")] | length")" 0
  passed
}

# step_credit_limit is T77a (ADR-0066): Ruth reads what Northwind owes
# Maple, its credit exposure, which is what its open items add up to;
# without a limit the customer is not restricted, and the answer says none
# is set. She sets a limit 500.00 above the exposure, once: a second limit
# for the customer is refused. An amount of 400.00 is then within the
# limit and 600.00 is not; the check changes nothing, the open items and
# the limit are as before. She raises the limit with her version of it,
# to 5000.00 above the exposure, and the larger amount is within it. The
# amounts follow from the exposure read, so the step holds whatever
# earlier steps left open. The raised limit leaves room for the sales
# order of step_sales_order (1824.41 CAD with its GST), which is checked
# against it when it is confirmed (T77b): with less room that order would
# be confirmed blocked and take no delivery.
step_credit_limit() {
  heading "Step 59 — Credit limits: a customer's credit exposure is its open items; a limit is set for the customer in the company; one amount is within the limit and a larger one is not; the check changes nothing; the limit is changed with the caller's version"
  local customer=$CUSTOMER exposure limit raised id tag before
  as ruth
  say "Ruth looks after Maple's credit as well. Before more is sold to Northwind Traders on account she reads what"
  say "it owes: its credit exposure, the customer's open items in Maple's currency, credits netted (ADR-0066 §3)."
  say "No limit is set for it yet, so it is not restricted, and the answer says so:"
  api 200 '"\(.customer_number) \(.customer_name) in \(.company_code) \(.company_name): exposure \(.exposure.amount) \(.exposure.currency) from \(.open_item_count) open item(s); limit set: \(.has_limit); within the limit: \(.is_within_limit)"' \
    getCreditExposure -p "legal_entity_id=$LE" -p "customer_id=$customer"
  expect "no limit set, and so within it" "$(field '"\(.has_limit) \(.is_within_limit) \(has("limit")) \(has("available")) \(.currency)"')" "false true false false CAD"
  exposure=$(field .exposure.amount)
  QUIET=1 api 200 '' listOpenItems -p "legal_entity_id=$LE" -p "partner_id=$customer"
  before=$(field '[.items[] | "\(.journal_entry_id) \(.amount_lc.amount)"] | join(", ")')
  expect "the exposure, the customer's open items added up" "$(field '[.items[].amount_lc.amount | tonumber] | add * 100 | round')" \
    "$(jq -rn --arg x "$exposure" '$x | tonumber * 100 | round')"
  expect "an exposure above zero" "$(jq -rn --arg x "$exposure" '($x | tonumber) > 0')" true
  limit=$(awk -v x="$exposure" 'BEGIN { printf "%.2f", x + 500 }')
  raised=$(awk -v x="$exposure" 'BEGIN { printf "%.2f", x + 5000 }')
  say "She sets Northwind's limit in Maple at $limit CAD, 500.00 above what it owes. A limit is one amount per"
  say "customer and company in the company's currency; it holds at once, and nothing is posted or blocked:"
  api 201 '"credit limit of \(.customer_number) \(.customer_name) in \(.company_code): \(.limit.amount) \(.limit.currency), version \(.version)"' \
    createCreditLimit -body "{\"legal_entity_id\": \"$LE\", \"customer_id\": \"$customer\", \"limit\": {\"amount\": \"$limit\", \"currency\": \"CAD\"}}"
  id=$(field .id) tag=$ETAG
  expect "the limit" "$(field '"\(.customer_id) \(.customer_name) \(.limit.amount) \(.limit.currency) \(.version)"')" "$customer Northwind Traders $limit CAD 1"
  say "A customer has one limit in a company; a second is refused, and a limit in another currency too:"
  refused 409 credit.limit_exists createCreditLimit -body "{\"legal_entity_id\": \"$LE\", \"customer_id\": \"$customer\", \"limit\": {\"amount\": \"1.00\", \"currency\": \"CAD\"}}"
  refused 422 credit.currency_mismatch updateCreditLimit -p "id=$id" -if-match "$tag" -body '{"limit": {"amount": "1.00", "currency": "USD"}}'
  QUIET=1 api 200 '' listCreditLimits -p "legal_entity_id=$LE" -p "customer_id=$customer"
  expect "the customer's limits in Maple" "$(field '[.items[].id] | join(" ")')" "$id"
  say "Now the exposure is read against the limit: what is available is the limit less the exposure."
  api 200 '"exposure \(.exposure.amount) \(.exposure.currency), limit \(.limit.amount), available \(.available.amount); within the limit: \(.is_within_limit)"' \
    getCreditExposure -p "legal_entity_id=$LE" -p "customer_id=$customer"
  expect "the exposure against the limit" "$(field '"\(.exposure.amount) \(.has_limit) \(.credit_limit_id) \(.limit.amount) \(.available.amount) \(.available.currency) \(.is_within_limit)"')" \
    "$exposure true $id $limit 500.00 CAD true"
  say "Any API consumer, an order entry screen or an agent, asks whether an amount is within the limit: the"
  say "exposure and the amount together may be no more than it. An order of 400.00 CAD is within Northwind's limit:"
  api 200 '"\(.amount.amount) \(.amount.currency) on top of \(.exposure.amount): within the limit of \(.limit.amount): \(.is_within_limit)"' \
    getCreditExposure -p "legal_entity_id=$LE" -p "customer_id=$customer" -p amount=400.00
  expect "400.00 within the limit" "$(field '"\(.amount.amount) \(.amount.currency) \(.is_within_limit)"')" "400.00 CAD true"
  say "One of 600.00 CAD is not. The check only answers: it blocks nothing and changes nothing."
  api 200 '"\(.amount.amount) \(.amount.currency) on top of \(.exposure.amount): within the limit of \(.limit.amount): \(.is_within_limit)"' \
    getCreditExposure -p "legal_entity_id=$LE" -p "customer_id=$customer" -p amount=600.00
  expect "600.00 not within the limit" "$(field '"\(.amount.amount) \(.is_within_limit) \(.exposure.amount) \(.available.amount)"')" "600.00 false $exposure 500.00"
  QUIET=1 api 200 '' listOpenItems -p "legal_entity_id=$LE" -p "partner_id=$customer"
  expect "the customer's open items after the checks" "$(field '[.items[] | "\(.journal_entry_id) \(.amount_lc.amount)"] | join(", ")')" "$before"
  QUIET=1 api 200 '' getCreditLimit -p "id=$id"
  expect "the limit after the checks" "$(field '"\(.limit.amount) \(.version)"') $ETAG" "$limit 1 $tag"
  say "Ruth agrees a higher limit with sales and changes it, with her version of the limit: $raised CAD. The audit"
  say "trail and the event CreditLimitChanged carry the limit before and after; the event names the customer by"
  say "id and number, never by name:"
  api 200 '"credit limit of \(.customer_number): \(.limit.amount) \(.limit.currency), version \(.version)"' \
    updateCreditLimit -p "id=$id" -if-match "$tag" -body "{\"limit\": {\"amount\": \"$raised\", \"currency\": \"CAD\"}}"
  expect "the raised limit" "$(field '"\(.limit.amount) \(.version)"')" "$raised 2"
  say "Her earlier version of it is stale now, and a change with it is refused:"
  refused 412 request.version_mismatch updateCreditLimit -p "id=$id" -if-match "$tag" -body '{"limit": {"amount": "1.00", "currency": "CAD"}}'
  api 200 '"\(.amount.amount) \(.amount.currency) on top of \(.exposure.amount): within the limit of \(.limit.amount): \(.is_within_limit)"' \
    getCreditExposure -p "legal_entity_id=$LE" -p "customer_id=$customer" -p amount=600.00
  expect "600.00 within the raised limit" "$(field '"\(.limit.amount) \(.available.amount) \(.is_within_limit)"')" "$raised 5000.00 true"
  QUIET=1 api 200 '' listAuditEvents -p object_type=credit.credit_limit -p "object_id=$id"
  expect "the limit's audit trail" "$(field '[.items[].action] | sort | join(" ")')" "credit.credit_limit.change credit.credit_limit.create"
  passed
}

# step_rfq is T82a and T82b (ADR-0069 §1-§5, §7): Ben creates a request
# for quotation for two items, inviting Tyrell and the supplier of SAP's
# down payment (step 3's DP_SUPPLIER), and publishes it; he then records
# the quotation each of them sent, the second cheaper in total, and reads
# their comparison (T82c), which ranks the cheaper first and marks the
# lowest amount of each item. The due date is the request's, two weeks
# after the company's date today. It keeps the RFQ (RFQ).
step_rfq() {
  heading "Step 60 — Sourcing: a request for quotation for two items is created with two suppliers invited and published; a quotation is recorded for each supplier, one cheaper in total, and the comparison ranks it first"
  as ben
  local due expense body valid quote line
  due=$(date_add "$(company_day)" 14)
  expense=$(account 6000)
  say "Ben wants prices for the new office's chairs and desks before he commits to a supplier. He creates a request"
  say "for quotation (ADR-0069): what Maple asks for, with quantities and no prices, the currency quotations are to"
  say "be given in, the date they are due, and the suppliers invited, Tyrell and $DP_NAME:"
  body="{\"legal_entity_id\": \"$LE\", \"currency\": \"CAD\", \"due_date\": \"$due\", \"items\": [
    {\"material_key\": \"CHAIR-ERGO\", \"description\": \"Ergonomic office chairs\", \"quantity\": {\"value\": \"40\", \"unit\": \"EA\"}, \"account_id\": \"$expense\"},
    {\"material_key\": \"DESK-ADJ\", \"description\": \"Height-adjustable desks\", \"quantity\": {\"value\": \"12\", \"unit\": \"EA\"}, \"account_id\": \"$expense\"}],
    \"supplier_ids\": [\"$SUPPLIER\", \"$DP_SUPPLIER\"]}"
  api 201 '"RFQ \(.document_number) of \(.company_code) \(.company_name): \(.status), quotations in \(.currency) due \(.due_date)", (.items[] | "  item \(.item_no) \(.material_key) \(.description): \(.quantity.value) \(.quantity.unit), on \(.account_number) \(.account_name)"), (.suppliers[] | "  invited: \(.supplier_number) \(.supplier_name)")' \
    createRfq -body "$body"
  RFQ=$(field .id)
  expect "the new RFQ" "$(field '"\(.status) \(.company_code) \(.currency) \(.due_date) \(.items | length) \([.items[].account_number] | unique | join(" "))"')" "draft $CC CAD $due 2 6000"
  expect "the suppliers invited, in the order named" "$(field '[.suppliers[] | "\(.supplier_id) \(.supplier_name)"] | join(", ")')" \
    "$SUPPLIER Tyrell Office Products, $DP_SUPPLIER $DP_NAME"
  say "A supplier is invited once, and a due date that has passed is refused, naming Maple's date today:"
  refused 422 procurement.rfq_supplier_repeated createRfq -body "${body/\"$DP_SUPPLIER\"]/\"$SUPPLIER\"]}"
  expect "the supplier named twice" "$(field '.errors[0].field')" "supplier_ids[1]"
  refused 422 procurement.rfq_due_date_past createRfq -body "${body/$due/$(date_add "$(company_day)" -1)}"
  say "Ben publishes the RFQ at the version he read. From then on the suppliers' quotations are recorded against"
  say "it, and nothing changes it; it is published once:"
  QUIET=1 api 200 '' getRfq -p "id=$RFQ"
  api 200 '"RFQ \(.document_number): \(.status), version \(.version)"' publishRfq -p "id=$RFQ" -if-match "$ETAG"
  expect "the published RFQ" "$(field '"\(.status) \(.version)"')" "published 2"
  refused 409 procurement.invalid_status publishRfq -p "id=$RFQ" -if-match "$ETAG"
  say "It is among Maple's published RFQs, and among those Tyrell is invited to:"
  api 200 '.items[] | "  RFQ \(.document_number): \(.status), due \(.due_date), \(.items | length) items, \(.suppliers | length) suppliers"' \
    listRfqs -p "legal_entity_id=$LE" -p status=published -p "supplier_id=$SUPPLIER"
  expect "Maple's published RFQs Tyrell is invited to" "$(field '[.items[].id] | join(" ")')" "$RFQ"
  valid=$(date_add "$(company_day)" 60)
  line='"quotation \(.supplier_reference) of \(.supplier_number) \(.supplier_name) for RFQ \(.rfq_document_number): \(.status), valid until \(.valid_until), total \(.total.amount) \(.total.currency), complete: \(.complete)", (.items[] | "  item \(.rfq_item_no) \(.material_key): \(.quantity.value) \(.quantity.unit) at \(.unit_price.amount) per \(.price_unit) = \(.amount.amount) \(.amount.currency)")'
  say "The quotations come in by e-mail. Ben records each against the RFQ (ADR-0069 §7): the supplier's reference,"
  say "how long it is valid, and a price for each item, in the RFQ's currency. Tyrell quotes 450.00 a chair and"
  say "980.00 a desk; each item's value is the RFQ's quantity at that price, and the total is exact to the cent:"
  api 201 "$line" createSupplierQuotation -p "id=$RFQ" -body "{\"supplier_id\": \"$SUPPLIER\", \"supplier_reference\": \"TY-Q-118\", \"valid_until\": \"$valid\", \"items\": [
    {\"rfq_item_no\": 1, \"unit_price\": {\"amount\": \"450.00\", \"currency\": \"CAD\"}},
    {\"rfq_item_no\": 2, \"unit_price\": {\"amount\": \"980.00\", \"currency\": \"CAD\"}}]}"
  expect "Tyrell's quotation" "$(field '"\(.supplier_name) \(.status) \(.complete) \(.total.amount) \(.total.currency) \([.items[].amount.amount] | join(" "))"')" \
    "Tyrell Office Products recorded true 29760.00 CAD 18000.00 11760.00"
  say "$DP_NAME quotes 425.00 a chair and prices its desks by threes, 2,850.00 for three:"
  quote="{\"supplier_id\": \"$DP_SUPPLIER\", \"supplier_reference\": \"CB-7731\", \"valid_until\": \"$valid\", \"items\": [
    {\"rfq_item_no\": 1, \"unit_price\": {\"amount\": \"425.00\", \"currency\": \"CAD\"}},
    {\"rfq_item_no\": 2, \"unit_price\": {\"amount\": \"2850.00\", \"currency\": \"CAD\"}, \"price_unit\": 3}]}"
  api 201 "$line" createSupplierQuotation -p "id=$RFQ" -body "$quote"
  expect "$DP_NAME's quotation" "$(field '"\(.supplier_name) \(.status) \(.complete) \(.total.amount) \(.total.currency) \([.items[].amount.amount] | join(" "))"')" \
    "$DP_NAME recorded true 28400.00 CAD 17000.00 11400.00"
  say "A supplier has one quotation for an RFQ, and only a supplier the RFQ invites has one:"
  refused 409 procurement.quotation_exists createSupplierQuotation -p "id=$RFQ" -body "$quote"
  refused 422 procurement.supplier_not_invited createSupplierQuotation -p "id=$RFQ" -body "${quote/$DP_SUPPLIER/$CUSTOMER}"
  say "The RFQ's quotations, as recorded: $DP_NAME's is the cheaper in total, by 1,360.00:"
  api 200 '.items[] | "  \(.supplier_reference) of \(.supplier_number) \(.supplier_name): total \(.total.amount) \(.total.currency), complete: \(.complete)"' \
    listSupplierQuotations -p "id=$RFQ"
  expect "the RFQ's quotations" "$(field '[.items[] | "\(.supplier_id) \(.total.amount) \(.total.currency)"] | join(", ")')" \
    "$SUPPLIER 29760.00 CAD, $DP_SUPPLIER 28400.00 CAD"
  say "Ben reads the comparison (ADR-0069 §7, T82c). It is computed on each read, by price alone: the quotations"
  say "that price every item and are valid on Maple's date today are ranked by total, the lowest first, and the"
  say "lowest amount quoted for each item is marked. $DP_NAME is first, and has the lowest amount of both items:"
  api 200 '"RFQ \(.rfq_document_number) of \(.company_code) \(.company_name) compared on \(.as_of): \(.invited_count) invited, \(.answered_count) answered, \(.ranked_count) ranked, amounts in \(.currency)", (.suppliers[] | "  rank \(.rank): \(.supplier_number) \(.supplier_name), \(.supplier_reference), total \(.total.amount) \(.total.currency)", (.prices[] | "    item \(.rfq_item_no): \(.unit_price.amount) per \(.price_unit) = \(.amount.amount) \(.amount.currency)\(if .lowest then "  <- lowest" else "" end)")), (.items[] | "  lowest for item \(.rfq_item_no) \(.material_key): \(.lowest_amount.amount) \(.lowest_amount.currency)")' \
    getRfqComparison -p "id=$RFQ"
  expect "the comparison's day and counts" "$(field '"\(.as_of) \(.invited_count) \(.answered_count) \(.ranked_count)"')" "$(company_day) 2 2 2"
  expect "the ranking, the cheaper supplier first" "$(field '[.suppliers[] | "\(.rank) \(.supplier_id) \(.supplier_name) \(.total.amount) \(.total.currency)"] | join(", ")')" \
    "1 $DP_SUPPLIER $DP_NAME 28400.00 CAD, 2 $SUPPLIER Tyrell Office Products 29760.00 CAD"
  expect "the lowest amount of each item" "$(field '[.items[] | "\(.rfq_item_no) \(.lowest_amount.amount) \(.lowest_amount.currency)"] | join(", ")')" \
    "1 17000.00 CAD, 2 11400.00 CAD"
  expect "the prices marked as the lowest" "$(field '[.suppliers[] | .supplier_id as $s | .prices[] | select(.lowest) | "\($s) \(.rfq_item_no) \(.amount.amount)"] | join(", ")')" \
    "$DP_SUPPLIER 1 17000.00, $DP_SUPPLIER 2 11400.00"
  passed
}

# step_rfq_award is T82d (ADR-0069 §7): Ben awards the RFQ of step_rfq
# (RFQ) to the quotation its comparison ranks first, the cheaper one of
# step 3's DP_SUPPLIER. The award creates a draft contract with that
# supplier at the quoted prices, valid for a year from the company's date
# today, which Ben releases as any contract.
step_rfq_award() {
  heading "Step 66 — Award: the RFQ is awarded to the cheaper quotation; the draft contract carries its prices and supplier and is released"
  as ben
  local today until winner body contract tag
  today=$(company_day)
  until=$(date_add "$today" 364)
  say "Ben decides for the quotation the comparison ranks first, $DP_NAME's. An award goes to one quotation as a"
  say "whole (ADR-0069 §7): it must price every item and be valid on Maple's date today. A person awards, never an"
  say "agent; the RFQ is awarded once, at the version Ben read:"
  QUIET=1 api 200 '' getRfqComparison -p "id=$RFQ"
  winner=$(field '.suppliers[] | select(.rank == 1) | .supplier_quotation_id')
  expect "the quotation ranked first is $DP_NAME's" "$(field '.suppliers[] | select(.rank == 1) | .supplier_id')" "$DP_SUPPLIER"
  QUIET=1 api 200 '' getRfq -p "id=$RFQ"
  tag=$ETAG
  body="{\"quotation_id\": \"$winner\", \"valid_from\": \"$today\", \"valid_to\": \"$until\"}"
  say "The contract's validity must be a period:"
  refused 422 procurement.invalid_validity awardRfq -p "id=$RFQ" -if-match "$tag" -body "${body/$until/$(date_add "$today" -1)}"
  api 200 '"RFQ \(.document_number): \(.status), version \(.version), awarded to draft contract \(.contract_document_number)"' \
    awardRfq -p "id=$RFQ" -if-match "$tag" -body "$body"
  contract=$(field .contract_id) tag=$ETAG
  expect "the awarded RFQ" "$(field '"\(.status) \(.version) \(.contract_document_number | startswith("CT"))"')" "awarded 3 true"
  say "A second award finds the RFQ awarded, and no further quotation is recorded for it:"
  refused 409 procurement.invalid_status awardRfq -p "id=$RFQ" -if-match "$tag" -body "$body"
  say "The quotations show the outcome: $DP_NAME's is awarded, Tyrell's is not:"
  api 200 '.items[] | "  \(.supplier_reference) of \(.supplier_number) \(.supplier_name): \(.status), total \(.total.amount) \(.total.currency)"' \
    listSupplierQuotations -p "id=$RFQ"
  expect "the quotations' outcomes" "$(field '[.items[] | "\(.supplier_id) \(.status)"] | join(", ")')" "$SUPPLIER not_awarded, $DP_SUPPLIER awarded"
  say "The award created a draft contract with $DP_NAME: the RFQ's items at the quoted prices, its target value the"
  say "quotation's total. It names the RFQ it came from:"
  api 200 '"contract \(.document_number) from RFQ \(.rfq_document_number): \(.status), \(.valid_from) to \(.valid_to), target value \(.target_value.amount) \(.target_value.currency)", (.items[] | "  item \(.item_no) \(.material_key) \(.description): \(.unit_price.amount) \(.unit_price.currency) per \(.price_unit) \(.uom_code)")' \
    getContract -p "id=$contract"
  expect "the draft contract" "$(field '"\(.status) \(.supplier_id) \(.rfq_id) \(.valid_from) \(.valid_to) \(.target_value.amount) \(.target_value.currency)"')" \
    "draft $DP_SUPPLIER $RFQ $today $until 28400.00 CAD"
  expect "the contract's items at the quoted prices" "$(field '[.items[] | "\(.material_key) \(.unit_price.amount)/\(.price_unit) \(.uom_code)"] | join(", ")')" \
    "CHAIR-ERGO 425.00/1 EA, DESK-ADJ 2850.00/3 EA"
  say "Ben releases the contract as any other; orders can now be placed against it:"
  api 200 '"contract \(.document_number): \(.status)"' releaseContract -p "id=$contract" -if-match "$ETAG"
  expect "the released contract" "$(field '"\(.status) \(.rfq_id)"')" "active $RFQ"
  passed
}

# step_catalog: catalog items (ADR-0071, T84a). Priya, the purchasing
# manager, loads items of Tyrell into Maple's catalog; Ben, a requester,
# finds them by a word of their description. The catalog is the tenant's
# own table: nothing is fetched from a supplier's system. Then (T84b) Ben
# raises a requisition from the chair with a quantity: its description,
# unit, price and supplier are the catalog's, and it is submitted and
# approved as any other.
step_catalog() {
  heading "Step 61 — Catalog items: two items of a supplier loaded into the company's catalog and found by a search on a word of their description; a retired item is no longer found; a requisition from a catalog item takes its price and supplier"
  as priya
  local chair desk arm req request expense
  say "Priya loads Tyrell's items into Maple's catalog, one at a time: an item key, a description, a unit and one"
  say "price each (ADR-0071). The supplier must be one Maple can order from, as a purchase order's must be."
  api 201 '"catalog item \(.item_key) of \(.supplier_number) \(.supplier_name): \(.description), \(.unit_price.amount) \(.unit_price.currency) per \(.price_unit) \(.unit), \(.status)"' createCatalogItem \
    -body "{\"legal_entity_id\": \"$LE\", \"supplier_id\": \"$SUPPLIER\", \"item_key\": \"CHAIR-ERGO\", \"description\": \"Ergonomic office chair, mesh back\", \"unit\": \"EA\", \"unit_price\": {\"amount\": \"450.00\", \"currency\": \"CAD\"}}"
  chair=$(field .id)
  expect "the chair in the catalog" "$(field '"\(.status) \(.supplier_id) \(.unit_price.amount) \(.unit_price.currency)"')" "active $SUPPLIER 450.00 CAD"
  api 201 '"catalog item \(.item_key) of \(.supplier_number) \(.supplier_name): \(.description), \(.unit_price.amount) \(.unit_price.currency) per \(.price_unit) \(.unit), \(.status)"' createCatalogItem \
    -body "{\"legal_entity_id\": \"$LE\", \"supplier_id\": \"$SUPPLIER\", \"item_key\": \"DESK-OAK\", \"description\": \"Standing desk, oak, with office cable tray\", \"unit\": \"EA\", \"unit_price\": {\"amount\": \"890.00\", \"currency\": \"CAD\"}}"
  desk=$(field .id)
  QUIET=1 api 201 '' createCatalogItem \
    -body "{\"legal_entity_id\": \"$LE\", \"supplier_id\": \"$SUPPLIER\", \"item_key\": \"ARM-01\", \"description\": \"Monitor arm for an office desk\", \"unit\": \"EA\", \"unit_price\": {\"amount\": \"75.00\", \"currency\": \"CAD\"}}"
  arm=$(field .id)
  say "An item key is the supplier's once among Maple's active items:"
  refused 409 procurement.catalog_item_exists createCatalogItem \
    -body "{\"legal_entity_id\": \"$LE\", \"supplier_id\": \"$SUPPLIER\", \"item_key\": \"CHAIR-ERGO\", \"description\": \"Another chair\", \"unit\": \"EA\", \"unit_price\": {\"amount\": \"1.00\", \"currency\": \"CAD\"}}"
  say "Tyrell no longer makes the monitor arm. Priya retires it, at the version she read; an item is retired once:"
  QUIET=1 api 200 '' getCatalogItem -p "id=$arm"
  api 200 '"catalog item \(.item_key): \(.status), version \(.version)"' retireCatalogItem -p "id=$arm" -if-match "$ETAG"
  expect "the arm's status" "$(field .status)" retired
  refused 409 procurement.invalid_status retireCatalogItem -p "id=$arm" -if-match "$ETAG"
  as ben
  say "Ben needs furniture for a new colleague. He searches Maple's catalog for a word, whatever its case; the"
  say "search reads item keys and descriptions and returns active items, each with its supplier's number and name:"
  api 200 '.items[] | "  \(.item_key) \(.description): \(.unit_price.amount) \(.unit_price.currency) per \(.unit), from \(.supplier_number) \(.supplier_name)"' \
    listCatalogItems -p "legal_entity_id=$LE" -p text=Office
  expect "the items found by a word of their description" "$(field '[.items[] | "\(.id) \(.status)"] | join(", ")')" "$chair active, $desk active"
  expect "each with its supplier's name" "$(field '[.items[].supplier_name] | unique | length')" 1
  api 200 '.items[] | "  \(.item_key) \(.description)"' listCatalogItems -p "legal_entity_id=$LE" -p text=chair -p "supplier_id=$SUPPLIER"
  expect "the chair found by text and supplier" "$(field '[.items[].id] | join(" ")')" "$chair"
  say "The retired arm is found only when retired items are asked for:"
  api 200 '.items[] | "  \(.item_key) \(.description): \(.status)"' listCatalogItems -p "legal_entity_id=$LE" -p text=office -p status=retired
  expect "the retired items" "$(field '[.items[].id] | join(" ")')" "$arm"
  accounts
  expense=$(account 6000)
  say "Ben asks for two of the chairs. He names the catalog item, a quantity and the account to charge; the"
  say "description, the unit, the price and the supplier are the catalog's (ADR-0071 §6, T84b):"
  api 201 '"requisition \(.document_number): \(.status), \(.total_amount.amount) \(.total_amount.currency)", (.items[] | "  item \(.item_no): \(.quantity.value) \(.quantity.unit) of \(.item_key) \(.description) at \(.unit_price.amount) \(.unit_price.currency)")' createRequisition \
    -body "{\"legal_entity_id\": \"$LE\", \"currency\": \"CAD\", \"items\": [{\"catalog_item_id\": \"$chair\", \"quantity\": {\"value\": \"2\", \"unit\": \"EA\"}, \"account_id\": \"$expense\"}]}"
  req=$(field .id)
  say "A price of his own is not his to give for a catalog item:"
  refused 422 procurement.catalog_item_override createRequisition \
    -body "{\"legal_entity_id\": \"$LE\", \"currency\": \"CAD\", \"items\": [{\"catalog_item_id\": \"$chair\", \"quantity\": {\"value\": \"2\", \"unit\": \"EA\"}, \"unit_price\": {\"amount\": \"400.00\", \"currency\": \"CAD\"}, \"account_id\": \"$expense\"}]}"
  expect "the refused field" "$(field '[.errors[].field] | join(" ")')" "items[0].unit_price"
  say "Nor is a retired item to be had:"
  refused 422 procurement.invalid_catalog_item createRequisition \
    -body "{\"legal_entity_id\": \"$LE\", \"currency\": \"CAD\", \"items\": [{\"catalog_item_id\": \"$arm\", \"quantity\": {\"value\": \"1\", \"unit\": \"EA\"}, \"account_id\": \"$expense\"}]}"
  say "Read back, the requisition has the catalog's price and Tyrell as its supplier:"
  api 200 '"requisition \(.document_number) for \(.expanded.supplier.number) \(.expanded.supplier.name): \(.items[0].item_key) at \(.items[0].unit_price.amount) \(.items[0].unit_price.currency), total \(.total_amount.amount) \(.total_amount.currency)"' \
    getRequisition -p "id=$req" -p expand=supplier
  expect "the requisition's item from the catalog" \
    "$(field '.items[0] | "\(.catalog_item_id) \(.item_key) \(.description) \(.quantity.value) \(.quantity.unit) \(.unit_price.amount) \(.unit_price.currency)"')" \
    "$chair CHAIR-ERGO Ergonomic office chair, mesh back 2 EA 450.00 CAD"
  expect "the requisition's supplier and total" "$(field '"\(.supplier_id) \(.total_amount.amount) \(.total_amount.currency)"')" "$SUPPLIER 900.00 CAD"
  say "Ben submits it; it goes to approval as any other requisition, and Priya approves it:"
  api 200 '"\(.status): approval request \(.workflow_request_id)"' submitRequisition -p "id=$req" -if-match "$ETAG"
  expect "the submitted requisition" "$(field .status)" pending_approval
  request=$(field .workflow_request_id)
  approve priya "$request" "Furniture for the new colleague"
  as ben
  api 200 '"requisition \(.document_number): \(.status)"' getRequisition -p "id=$req"
  expect "the approved requisition" "$(field .status)" approved
  passed
}

# step_quotation is T78a (ADR-0067 §2-§4): Sally, the sales
# representative, makes a quotation to the customer of step_customer,
# valid for thirty days, and reads it back with each item's value and its
# net value; an input tax code and an unknown unit are refused naming the
# item's own field; she records that Northwind accepted it, once. Nothing
# is posted. QUOTATION is the accepted quotation for later steps.
step_quotation() {
  heading "Step 62 — Quotations: a quotation to a customer created with its items priced net of tax, read back with its net value; the customer's acceptance recorded once; nothing posted"
  local customer=$CUSTOMER sales until before item1 item2 quotation number open
  as ada
  say "Ada adds Sally, the sales representative, to the team:"
  person sally "Sally Sales" finance_staff
  as sally
  QUIET=1 api 200 '' listOpenItems -p "legal_entity_id=$LE" -p "partner_id=$customer"
  open=$(field '[.items[] | "\(.journal_entry_id) \(.amount.amount)"] | join(", ")')
  sales=$(account 4000)
  until=$(jq -rn --arg d "$TODAY" '$d | strptime("%Y-%m-%d") | mktime + 30 * 86400 | strftime("%Y-%m-%d")')
  say "Northwind Traders asks for a price for 120 machined brackets and their installation. Sally makes a"
  say "quotation, valid until $until. An item names what is sold by a description and Maple's own item key (there is"
  say "no material master), with its quantity, its price, the revenue account its billing will credit and the"
  say "output tax code it will be billed with. Amounts are net: the GST is calculated when the order is billed"
  say "(ADR-0067 §3). 2.5 hours at 95.01 are 237.525: 237.53, rounded half away from zero."
  item1="{\"item_key\": \"BRACKET-M8\", \"description\": \"Machined bracket M8\", \"quantity\": {\"value\": \"120\", \"unit\": \"EA\"}, \"unit_price\": {\"amount\": \"12.50\", \"currency\": \"CAD\"}, \"account_id\": \"$sales\", \"tax_code\": \"GO5\"}"
  item2="{\"description\": \"Installation on site\", \"quantity\": {\"value\": \"2.5\", \"unit\": \"H\"}, \"unit_price\": {\"amount\": \"95.01\", \"currency\": \"CAD\"}, \"account_id\": \"$sales\", \"tax_code\": \"GO5\"}"
  before=$(company_day)
  api 201 '"quotation \(.document_number) to \(.customer_number) \(.customer_name), \(.company_code) \(.company_name): \(.status), made \(.quotation_date), valid until \(.valid_until), net \(.net_amount.amount) \(.net_amount.currency)", (.items[] | "  item \(.item_no) \(.item_key // "-") \(.description): \(.quantity.value) \(.quantity.unit) at \(.unit_price.amount) per \(.price_unit) = \(.net_amount.amount), on \(.account_number) \(.account_name), \(.tax_code // "no tax code")")' \
    createQuotation -body "{\"legal_entity_id\": \"$LE\", \"customer_id\": \"$customer\", \"valid_until\": \"$until\", \"currency\": \"CAD\", \"items\": [$item1, $item2]}"
  quotation=$(field .id)
  number=$(field .document_number)
  expect "the quotation's status" "$(field .status)" open
  expect_today "the quotation's date, Maple's today" "$(field .quotation_date)" "$before"
  expect "each item's net value" "$(field '[.items[] | .net_amount.amount] | join(" ")')" "1500.00 237.53"
  expect "the net value" "$(field '"\(.net_amount.amount) \(.net_amount.currency)"')" "1737.53 CAD"
  expect "the customer by name" "$(field .customer_name)" "Northwind Traders"
  say "Read back, it is the same quotation, and it is among Northwind's open ones:"
  api 200 '"quotation \(.document_number): \(.status), valid until \(.valid_until), net \(.net_amount.amount) \(.net_amount.currency), \(.items | length) items"' \
    getQuotation -p "id=$quotation"
  expect "read back" "$(field '"\(.document_number) \(.status) \(.valid_until) \(.net_amount.amount) \(.version)"')" "$number open $until 1737.53 1"
  api 200 '.items[] | "  \(.document_number) \(.customer_name): \(.status), \(.net_amount.amount) \(.net_amount.currency)"' \
    listQuotations -p "customer_id=$customer" -p status=open
  expect "the customer's open quotations" "$(field '[.items[].document_number] | join(" ")')" "$number"
  say "GA, an input tax code (the return's adjustment of step 2), is no output tax code, and XX is no unit of measure;"
  say "each refusal names the item's own field:"
  refused 422 sales.unknown_tax_code createQuotation \
    -body "{\"legal_entity_id\": \"$LE\", \"customer_id\": \"$customer\", \"valid_until\": \"$until\", \"currency\": \"CAD\", \"items\": [${item1/GO5/GA}]}"
  expect "the field refused" "$(field '.errors[0].field')" "items[0].tax_code"
  refused 422 sales.unknown_unit createQuotation \
    -body "{\"legal_entity_id\": \"$LE\", \"customer_id\": \"$customer\", \"valid_until\": \"$until\", \"currency\": \"CAD\", \"items\": [$item1, ${item2/\"H\"/\"XX\"}]}"
  expect "the field refused" "$(field '.errors[0].field')" "items[1].quantity.unit"
  say "Northwind accepts. Sally records the answer at the version she read; it is recorded once:"
  QUIET=1 api 200 '' getQuotation -p "id=$quotation"
  api 200 '"quotation \(.document_number): \(.status), version \(.version)"' acceptQuotation -p "id=$quotation" -if-match "$ETAG"
  expect "the accepted quotation" "$(field '"\(.status) \(.version) \(.net_amount.amount)"')" "accepted 2 1737.53"
  refused 409 sales.invalid_status declineQuotation -p "id=$quotation" -if-match "$ETAG"
  QUIET=1 api 200 '' listQuotations -p "customer_id=$customer" -p status=accepted
  expect "the customer's accepted quotations" "$(field '[.items[].document_number] | join(" ")')" "$number"
  QUOTATION=$quotation
  say "A quotation posts nothing: Northwind's open items are as they were."
  QUIET=1 api 200 '' listOpenItems -p "legal_entity_id=$LE" -p "partner_id=$customer"
  expect "the customer's open items" "$(field '[.items[] | "\(.journal_entry_id) \(.amount.amount)"] | join(", ")')" "$open"
  passed
}
# step_sales_order is T78b (ADR-0067 §5): Sally creates a sales order from
# the accepted quotation of step_quotation, which copies its customer,
# currency and items and takes Northwind's payment term; a second order
# from the quotation is refused; she confirms the order, once. T78c
# (ADR-0067 §8): she confirms its items as delivered in two deliveries; a
# quantity above what is open is refused naming the item, and the order
# reads as fully delivered. Nothing is posted and no stock moves.
# SALES_ORDER is the confirmed, fully delivered order for later steps.
step_sales_order() {
  heading "Step 63 — Sales orders: an order created from the accepted quotation with its customer, currency and items, on the customer's payment term; one order per quotation; confirmed once; its items confirmed as delivered; nothing posted"
  local customer=$CUSTOMER quotation=$QUOTATION before order number open day
  [ -n "$quotation" ] || fail "step 62's quotation is not there"
  as sally
  QUIET=1 api 200 '' listOpenItems -p "legal_entity_id=$LE" -p "partner_id=$customer"
  open=$(field '[.items[] | "\(.journal_entry_id) \(.amount.amount)"] | join(", ")')
  say "Northwind accepted the quotation of step 62, so Sally creates the sales order from it. The order copies the"
  say "quotation's customer, currency and items as they were offered; its payment term is Northwind's in Maple,"
  say "NT30, since the request names none (ADR-0067 §5). Each item shows what is ordered, delivered and billed:"
  before=$(company_day)
  api 201 '"sales order \(.document_number) of \(.customer_number) \(.customer_name), \(.company_code) \(.company_name): \(.status), from quotation \(.quotation_number), dated \(.order_date), terms \(.payment_term_code), net \(.net_amount.amount) \(.net_amount.currency)", (.items[] | "  item \(.item_no) \(.item_key // "-") \(.description): ordered \(.quantity.value) \(.quantity.unit) at \(.unit_price.amount) = \(.net_amount.amount), delivered \(.delivered_quantity.value), billed \(.billed_quantity.value)")' \
    createSalesOrder -body "{\"quotation_id\": \"$quotation\"}"
  order=$(field .id)
  number=$(field .document_number)
  expect "the order's status" "$(field .status)" draft
  expect_today "the order's date, Maple's today" "$(field .order_date)" "$before"
  expect "the order's customer, currency, term and net value" "$(field '"\(.customer_id) \(.currency) \(.payment_term_code) \(.net_amount.amount)"')" "$customer CAD NT30 1737.53"
  expect "the order's quotation" "$(field .quotation_id)" "$quotation"
  expect "each item's ordered, delivered and billed quantity" \
    "$(field '[.items[] | "\(.quantity.value) \(.quantity.unit) \(.delivered_quantity.value) \(.billed_quantity.value) \(.net_amount.amount)"] | join(", ")')" \
    "120 EA 0 0 1500.00, 2.5 H 0 0 237.53"
  say "A quotation gives one order: a second is refused."
  refused 409 sales.quotation_ordered createSalesOrder -body "{\"quotation_id\": \"$quotation\"}"
  say "Sally confirms the order at the version she read; it is confirmed once:"
  QUIET=1 api 200 '' getSalesOrder -p "id=$order"
  api 200 '"sales order \(.document_number): \(.status), version \(.version)"' confirmSalesOrder -p "id=$order" -if-match "$ETAG"
  expect "the confirmed order" "$(field '"\(.status) \(.version) \(.net_amount.amount)"')" "confirmed 2 1737.53"
  refused 409 sales.invalid_status confirmSalesOrder -p "id=$order" -if-match "$ETAG"
  say "Read back, it is the quotation's order, among Northwind's confirmed ones:"
  api 200 '.items[] | "  \(.document_number) \(.customer_name): \(.status), from \(.quotation_number), \(.net_amount.amount) \(.net_amount.currency)"' \
    listSalesOrders -p "quotation_id=$quotation"
  expect "the quotation's order" "$(field '[.items[] | "\(.document_number) \(.status)"] | join(" ")')" "$number confirmed"
  QUIET=1 api 200 '' listSalesOrders -p "customer_id=$customer" -p status=confirmed
  expect "the customer's confirmed orders" "$(field '[.items[].document_number] | join(" ")')" "$number"
  SALES_ORDER=$order
  say "The brackets leave in two shipments. There is no stock in the ERP yet, so a delivery is a confirmation by a"
  say "person: Sally records what was delivered and when, against the confirmed order (ADR-0067 §8)."
  say "The first shipment is 100 of the 120 brackets:"
  day=$(company_day)
  api 201 '"delivery \(.delivery_no) of sales order \(.sales_order_number) on \(.delivery_date)", (.items[] | "  item \(.item_no) \(.item_key // "-") \(.description): \(.quantity.value) \(.quantity.unit)")' \
    confirmSalesDelivery -p "id=$order" -body "{\"delivery_date\": \"$day\", \"items\": [{\"item_no\": 1, \"quantity\": {\"value\": \"100\", \"unit\": \"EA\"}}]}"
  expect "the first delivery" "$(field '"\(.delivery_no) \(.sales_order_id) \(.sales_order_number) \(.delivery_date)"')" "1 $order $number $day"
  say "The order shows what is delivered and, against what was ordered, what is open; its version moved:"
  api 200 '"sales order \(.document_number), version \(.version)", (.items[] | "  item \(.item_no) \(.description): ordered \(.quantity.value) \(.quantity.unit), delivered \(.delivered_quantity.value)")' \
    getSalesOrder -p "id=$order"
  expect "the order after the first delivery" "$(field '"\(.version) " + ([.items[] | "\(.quantity.value)/\(.delivered_quantity.value)"] | join(" "))')" "3 120/100 2.5/0"
  say "Twenty brackets are open. A delivery of more is refused, naming the item and what is open of it; so is a date"
  say "after Maple's today:"
  refused 422 sales.exceeds_open_quantity confirmSalesDelivery -p "id=$order" \
    -body "{\"delivery_date\": \"$day\", \"items\": [{\"item_no\": 1, \"quantity\": {\"value\": \"21\", \"unit\": \"EA\"}}]}"
  expect "the field refused" "$(field '.errors[0].field')" "items[0].quantity.value"
  refused 422 sales.delivery_date_in_future confirmSalesDelivery -p "id=$order" \
    -body "{\"delivery_date\": \"2099-12-31\", \"items\": [{\"item_no\": 1, \"quantity\": {\"value\": \"20\", \"unit\": \"EA\"}}]}"
  say "The second shipment brings the last twenty, and the installation is done: 2.5 hours, confirmed as made available."
  api 201 '"delivery \(.delivery_no) of sales order \(.sales_order_number) on \(.delivery_date)", (.items[] | "  item \(.item_no) \(.item_key // "-") \(.description): \(.quantity.value) \(.quantity.unit)")' \
    confirmSalesDelivery -p "id=$order" \
    -body "{\"delivery_date\": \"$day\", \"items\": [{\"item_no\": 1, \"quantity\": {\"value\": \"20\", \"unit\": \"EA\"}}, {\"item_no\": 2, \"quantity\": {\"value\": \"2.5\", \"unit\": \"H\"}}]}"
  expect "the second delivery" "$(field '"\(.delivery_no) " + ([.items[] | "\(.item_no):\(.quantity.value) \(.quantity.unit)"] | join(", "))')" "2 1:20 EA, 2:2.5 H"
  say "The order reads as fully delivered, nothing billed yet, and lists its two deliveries:"
  api 200 '"sales order \(.document_number): \(.status), version \(.version)", (.items[] | "  item \(.item_no) \(.description): ordered \(.quantity.value) \(.quantity.unit), delivered \(.delivered_quantity.value), billed \(.billed_quantity.value)")' \
    getSalesOrder -p "id=$order"
  expect "each item's ordered, delivered and billed quantity" \
    "$(field '[.items[] | "\(.quantity.value) \(.delivered_quantity.value) \(.billed_quantity.value)"] | join(", ")')" "120 120 0, 2.5 2.5 0"
  expect "the order's status and version" "$(field '"\(.status) \(.version)"')" "confirmed 4"
  api 200 '.items[] | "  delivery \(.delivery_no) on \(.delivery_date): " + ([.items[] | "item \(.item_no) \(.quantity.value) \(.quantity.unit)"] | join(", "))' \
    listSalesDeliveries -p "id=$order"
  expect "the order's deliveries" "$(field '[.items[] | "\(.delivery_no):" + ([.items[] | "\(.item_no)=\(.quantity.value)"] | join("+"))] | join(" ")')" "1:1=100 2:1=20+2=2.5"
  say "Nothing more is open: another delivery is refused."
  refused 422 sales.exceeds_open_quantity confirmSalesDelivery -p "id=$order" \
    -body "{\"delivery_date\": \"$day\", \"items\": [{\"item_no\": 2, \"quantity\": {\"value\": \"0.5\", \"unit\": \"H\"}}]}"
  say "An order and its deliveries post nothing: Northwind's open items are as they were."
  QUIET=1 api 200 '' listOpenItems -p "legal_entity_id=$LE" -p "partner_id=$customer"
  expect "the customer's open items" "$(field '[.items[] | "\(.journal_entry_id) \(.amount.amount)"] | join(", ")')" "$open"
  passed
}
# step_billing is T79a (ADR-0067 §9): Ruth, the receivables clerk of
# step_customer_invoice, creates a billing document for the delivered
# order of step_sales_order. Its items are the order's delivered
# quantities at the order's prices and its net value the order's; the
# order shows everything billed; a second document is refused, since
# nothing is left to bill. Nothing is posted until, T79b (ADR-0067 §9),
# Ruth posts the document: one customer invoice is entered and posted in
# receivables in the same transaction, naming the document as its source;
# receivables, revenue and output tax move by its amounts and its debit
# line is Northwind's open item on the order's terms; a second post is
# refused. Northwind pays the gross: a statement credit is matched as
# incoming and applied to the invoice as step_incoming_payment does, so
# the step leaves Northwind's open items and the unapplied receipts as it
# found them, whatever earlier steps left there. The statement follows
# RBC's latest, read from the API. BILLING_DOCUMENT is the posted
# document for later steps.
step_billing() {
  heading "Step 64 — Billing documents: a billing document created for the delivered sales order, its items the delivered quantities at the order's prices and its net value the order's; nothing billed twice; posted once as a customer invoice in receivables, the customer's open item; paid by an incoming payment"
  local customer=$CUSTOMER order=$SALES_ORDER before doc number open billed tag invoice entry due st receipt last receipts
  [ -n "$order" ] || fail "step 63's sales order is not there"
  as ruth
  QUIET=1 api 200 '' listOpenItems -p "legal_entity_id=$LE" -p "partner_id=$customer"
  open=$(field '[.items[] | "\(.journal_entry_id) \(.amount.amount)"] | join(", ")')
  QUIET=1 api 200 '' listUnappliedReceipts -p "legal_entity_id=$LE"
  receipts=$(field '[.items[].journal_entry_id] | join(" ")')
  say "The order of step 63 is delivered in full. Ruth bills it: a billing document takes what is delivered of each"
  say "item and not yet billed, at the order's prices (ADR-0067 §9). She gives no date, so it is dated Maple's today:"
  before=$(company_day)
  api 201 '"billing document \(.document_number) for sales order \(.sales_order_number) of \(.customer_number) \(.customer_name), \(.company_code) \(.company_name): \(.status), dated \(.billing_date), terms \(.payment_term_code), net \(.net_amount.amount) \(.net_amount.currency)", (.items[] | "  item \(.item_no) (order item \(.order_item_no)) \(.item_key // "-") \(.description): \(.quantity.value) \(.quantity.unit) at \(.unit_price.amount) = \(.net_amount.amount), account \(.account_number) \(.account_name), tax code \(.tax_code // "-")")' \
    createBillingDocument -body "{\"sales_order_id\": \"$order\"}"
  doc=$(field .id)
  number=$(field .document_number)
  billed=$(field .billing_date)
  expect "the document's status" "$(field .status)" created
  expect_today "the billing date, Maple's today" "$billed" "$before"
  expect "the document's order, customer, currency, term and net value" \
    "$(field '"\(.sales_order_id) \(.customer_id) \(.currency) \(.payment_term_code) \(.net_amount.amount)"')" "$order $customer CAD NT30 1737.53"
  expect "each item's order item, quantity and amount" \
    "$(field '[.items[] | "\(.order_item_no): \(.quantity.value) \(.quantity.unit) \(.net_amount.amount)"] | join(", ")')" \
    "1: 120 EA 1500.00, 2: 2.5 H 237.53"
  say "Its items and its net value are the order's. The order shows everything billed; its version moved:"
  api 200 '"sales order \(.document_number): \(.status), version \(.version), net \(.net_amount.amount) \(.net_amount.currency)", (.items[] | "  item \(.item_no) \(.description): ordered \(.quantity.value) \(.quantity.unit), delivered \(.delivered_quantity.value), billed \(.billed_quantity.value)")' \
    getSalesOrder -p "id=$order"
  expect "each item's ordered, delivered and billed quantity" \
    "$(field '[.items[] | "\(.quantity.value) \(.delivered_quantity.value) \(.billed_quantity.value)"] | join(", ")')" "120 120 120, 2.5 2.5 2.5"
  expect "the order's version and net value" "$(field '"\(.version) \(.net_amount.amount)"')" "5 1737.53"
  say "Nothing is left to bill, so a second document is refused; no quantity is billed twice:"
  refused 422 sales.nothing_to_bill createBillingDocument -body "{\"sales_order_id\": \"$order\"}"
  expect "the field refused" "$(field '.errors[0].field')" sales_order_id
  say "Read back, it is the order's one billing document:"
  QUIET=1 api 200 '' getBillingDocument -p "id=$doc"
  expect "the document read back" "$(field '"\(.document_number) \(.status) \(.version) \(.net_amount.amount)"')" "$number created 1 1737.53"
  api 200 '.items[] | "  \(.document_number) \(.customer_name): \(.status), sales order \(.sales_order_number), \(.net_amount.amount) \(.net_amount.currency)"' \
    listBillingDocuments -p "sales_order_id=$order"
  expect "the order's billing documents" "$(field '[.items[] | "\(.document_number) \(.status)"] | join(" ")')" "$number created"
  BILLING_DOCUMENT=$doc
  say "A billing document posts nothing until it is posted as a customer invoice: Northwind's open items are as they were."
  QUIET=1 api 200 '' listOpenItems -p "legal_entity_id=$LE" -p "partner_id=$customer"
  expect "the customer's open items" "$(field '[.items[] | "\(.journal_entry_id) \(.amount.amount)"] | join(", ")')" "$open"
  say "Ruth posts the billing document at the version she read (T79b, ADR-0067 §9). One customer invoice is entered in"
  say "accounts receivable and posted in the same transaction: dated the billing date, which is its tax date, on the"
  say "order's term NT30, its items the document's, one for one. The GST is calculated now: 5% of 1,737.53 is"
  say "86.8765, so 86.88, and the customer owes 1,824.41. Revenue is recognized as billed."
  due=$(jq -rn --arg d "$billed" '$d | strptime("%Y-%m-%d") | mktime + 30 * 86400 | strftime("%Y-%m-%d")')
  QUIET=1 api 200 '' getBillingDocument -p "id=$doc"
  tag=$ETAG
  before=$(company_day)
  api 200 '"billing document \(.document_number): \(.status) on \(.posting_date), posted as customer invoice \(.customer_invoice_number), journal entry \(.journal_entry_number) of \(.journal_entry_year)"' \
    postBillingDocument -p "id=$doc" -if-match "$tag"
  expect "the posted document" "$(field '"\(.document_number) \(.status) \(.version) \(.net_amount.amount)"')" "$number posted 2 1737.53"
  expect_today "the posting date, Maple's today" "$(field .posting_date)" "$before"
  invoice=$(field .customer_invoice_id)
  entry=$(field .journal_entry_id)
  if [ -z "$invoice" ] || [ "$invoice" = null ] || [ -z "$entry" ] || [ "$entry" = null ]; then fail "the posted document names no customer invoice or entry"; fi
  say "The customer invoice names the billing document as its source. Its net amount is the document's to the cent,"
  say "and its gross is net and tax exactly:"
  api 200 '"customer invoice \(.document_number) to \(.customer_number) \(.customer_name): \(.status), from \(.source_document_number), dated \(.invoice_date), terms \(.payment_term_code), due \(.net_due_date), net \(.net_amount.amount), tax \(.tax_amount.amount), gross \(.gross_amount.amount) \(.currency)", (.items[] | "  item \(.item_no) \(.description): \(.net_amount.amount) on \(.account_number) \(.account_name), \(.tax_code // "no tax code"), tax \(.tax_amount.amount)")' \
    getCustomerInvoice -p "id=$invoice"
  expect "the invoice's source, status, customer and term" \
    "$(field '"\(.source_document_id) \(.source_document_number) \(.status) \(.customer_id) \(.payment_term_code)"')" "$doc $number posted $customer NT30"
  expect "the invoice's date and due date" "$(field '"\(.invoice_date) \(.net_due_date)"')" "$billed $due"
  expect "net, tax and gross" "$(field '"\(.net_amount.amount) \(.tax_amount.amount) \(.gross_amount.amount) \(.gross_amount.currency)"')" "1737.53 86.88 1824.41 CAD"
  expect "each item's account, amount and tax code" "$(field '[.items[] | "\(.account_number) \(.net_amount.amount) \(.tax_code)"] | join(", ")')" \
    "4000 1500.00 GO5, 4000 237.53 GO5"
  expect "the invoice's entry" "$(field .journal_entry_id)" "$entry"
  say "The entry debits Northwind on 1200 Trade receivables with the gross and credits 4000 Sales per item and 2300"
  say "GST/HST payable with the tax: receivables, revenue and output tax moved by exactly the document's amounts."
  api 200 '"journal entry \(.document_number), \(.document_type), source \(.source_context)", (.lines[] | "  \(.account_id | acct): \(.amount.amount) \(.amount.currency)")' \
    getJournalEntry -p "id=$entry"
  expect "the entry's source" "$(field '"\(.document_type) \(.source_context) \(.source_document_id)"')" "DR receivables $invoice"
  expect "the entry's amounts" "$(field '[.lines[].amount.amount] | join(" ")')" "1824.41 -1500.00 -237.53 -86.88"
  expect "the entry's accounts" "$(field '[.lines[].account_id] | join(" ")')" "$(account 1200) $(account 4000) $(account 4000) $(account 2300)"
  expect "the receivable line's partner" "$(field '.lines[0].partner_id')" "$customer"
  say "The debit line is Northwind's open item for the billed amount, on the order's terms:"
  api 200 ".items[] | select(.journal_entry_id == \"$entry\") | \"  open item \(.document_number): \(.amount.amount) \(.amount.currency), terms \(.payment_terms.code), due \(.payment_terms.net_due_date)\"" \
    listOpenItems -p "legal_entity_id=$LE" -p "partner_id=$customer"
  expect "the order's open item" \
    "$(field "[.items[] | select(.journal_entry_id == \"$entry\") | \"\(.amount.amount) \(.amount.currency) \(.payment_terms.code) \(.payment_terms.net_due_date)\"] | join(\", \")")" \
    "1824.41 CAD NT30 $due"
  say "A billing document posts once: a second post is refused at the version Ruth read and at the new one, and the"
  say "ledger does not reverse the invoice's entry (reversing a posted billing document comes later)."
  refused 412 request.version_mismatch postBillingDocument -p "id=$doc" -if-match "$tag"
  QUIET=1 api 200 '' getBillingDocument -p "id=$doc"
  refused 409 sales.invalid_status postBillingDocument -p "id=$doc" -if-match "$ETAG"
  QUIET=1 api 200 '' getJournalEntry -p "id=$entry"
  refused 409 ledger.entry_owned_elsewhere reverseJournalEntry -p "id=$entry" -if-match "$ETAG" -body '{}'
  QUIET=1 api 200 '' listBillingDocuments -p "sales_order_id=$order" -p status=posted
  expect "the order's posted billing documents" "$(field '[.items[] | "\(.document_number) \(.customer_invoice_id)"] | join(" ")')" "$number $invoice"
  as tom
  say "Northwind pays the invoice, CAD 1,824.41. RBC's next statement of the day shows the credit; Tom imports it,"
  say "matches the line as an incoming payment and posts it, as in step 54:"
  QUIET=1 api 200 '' listBankStatements -p "house_bank_id=$RBC"
  last=$(field '[.items[] | select(.closing_date == "'"$TODAY"'")] | max_by(.sequence | tonumber) | "\(.sequence) \(.closing_balance.amount)"')
  rbc_incoming 1824.41 $((${last%% *} + 1)) "${last##* }"
  api 201 '.statements[] | "statement \(.statement_ref): \(.status), \(.line_count) line, closing \(.closing_balance.amount) \(.closing_balance.currency)"' \
    importBankStatements -body "{\"house_bank_id\": \"$RBC\", \"format\": \"camt.053\"}" -file file=@camt053-incoming.xml
  st=$(field '.statements[0].id')
  if [ -z "$st" ] || [ "$st" = null ]; then fail "the statement was not imported"; fi
  expect "the statement follows RBC's latest" "$(field '.statements[0].status')" imported
  QUIET=1 api 200 '' getBankStatement -p "id=$st"
  api 201 '"line \(.line_no): \(.status) by hand (\(.kind)), incoming \(.targets[0].incoming): \(.reason)"' matchBankStatementLine -p "id=$st" -p line_no=1 \
    -if-match "$ETAG" -body '{"incoming": true, "reason": "Northwind Traders pays the invoice of its sales order; the remittance names it."}'
  api 200 '"line \(.line_no): posted in journal entry \(.journal_entry_id[0:8])…"' postBankStatementLine -p "id=$st" -p line_no=1 -if-match "$ETAG"
  receipt=$(field .journal_entry_id)
  if [ -z "$receipt" ] || [ "$receipt" = null ]; then fail "the incoming line was not posted"; fi
  as ruth
  say "Ruth applies the receipt to the invoice the billing document posted, in full and exactly:"
  api 201 '"incoming payment \(.document_number) of \(.customer_number) \(.customer_name): \(.amount.amount) \(.amount.currency) on \(.posting_date), clearing entry \(.journal_entry_number)", (.items[] | "  item \(.item_no): journal entry \(.journal_entry_number), reference \(.reference), \(.amount.amount) \(.amount.currency)")' \
    createIncomingPayment -body "{\"legal_entity_id\": \"$LE\", \"customer_id\": \"$customer\", \"receipt_journal_entry_id\": \"$receipt\", \"posting_date\": \"$TODAY\", \"items\": [{\"fiscal_year\": ${TODAY:0:4}, \"journal_entry_id\": \"$entry\", \"line_no\": 1}]}"
  expect "the payment's amount" "$(field '"\(.amount.amount) \(.amount.currency)"')" "1824.41 CAD"
  expect "the item applied" "$(field '[.items[] | "\(.journal_entry_id) \(.line_no) \(.amount.amount)"] | join(", ")')" "$entry 1 1824.41"
  say "Northwind has no open item left from the order, and no receipt waits that did not wait before:"
  QUIET=1 api 200 '' listOpenItems -p "legal_entity_id=$LE" -p "partner_id=$customer"
  expect "the customer's open items" "$(field '[.items[] | "\(.journal_entry_id) \(.amount.amount)"] | join(", ")')" "$open"
  QUIET=1 api 200 '' listUnappliedReceipts -p "legal_entity_id=$LE"
  expect "the receipts not yet applied" "$(field '[.items[].journal_entry_id] | join(" ")')" "$receipts"
  passed
}
# step_price_list is T80a (ADR-0067 §10): Paula, the pricing analyst,
# records a general price and Northwind's own price for one item key;
# Sally's quotation item that names the key without a price takes
# Northwind's price, another customer's takes the general one, and a
# price entered with an item stays; an item with neither is refused
# naming its price; a later record does not change an item priced
# before. T80b (ADR-0067 §11): Ada activates a pricing procedure, a price
# and a customer discount, through a governed change; Paula records
# Northwind's discount of five percent; a new quotation item shows the
# price, the discount and the net value after it. T80c (product owner
# 2026-11-12): a price Sally enters for the same item is its final net
# price, with no discount. T80d: a price unit sent without a price is
# refused naming it.
step_price_list() {
  heading "Step 65 — Sales prices: a general price and a customer's price recorded for an item key; a quotation item without a price takes the customer's, another customer's the general one; an entered price stays; the price is fixed on the item; a pricing procedure through a governed change, a customer's discount, and an item's conditions with its net value after them; an entered price is final, with no discount; a price unit without a price is refused"
  local customer=$CUSTOMER sales ar until from item manual quotation contoso base cr request entered
  [ -n "$customer" ] || fail "step 47's customer is not there"
  as ada
  say "Ada adds Paula, the pricing analyst, to the team:"
  person paula "Paula Pricing" finance_staff
  as paula
  sales=$(account 4000)
  ar=$(account 1200)
  from="$YEAR-01-01"
  until=$(jq -rn --arg d "$TODAY" '$d | strptime("%Y-%m-%d") | mktime + 30 * 86400 | strftime("%Y-%m-%d")')
  say "Maple sells its machined bracket, item key BRACKET-M8, at 12.50 CAD each. Paula records that as the general"
  say "price, for every customer, valid from $from, and Northwind's own price, 1174.99 CAD per 100 (ADR-0067 §10):"
  api 201 '"price of \(.item_key) for every customer, \(.company_code) \(.company_name): \(.unit_price.amount) \(.unit_price.currency) per \(.price_unit) \(.unit), valid from \(.valid_from)"' \
    createSalesPrice -body "{\"legal_entity_id\": \"$LE\", \"item_key\": \"BRACKET-M8\", \"unit\": \"EA\", \"unit_price\": {\"amount\": \"12.50\", \"currency\": \"CAD\"}, \"valid_from\": \"$from\"}"
  expect "the general price names no customer" "$(field '.customer_id // "none"')" none
  api 201 '"price of \(.item_key) for \(.customer_number) \(.customer_name): \(.unit_price.amount) \(.unit_price.currency) per \(.price_unit) \(.unit), valid from \(.valid_from)"' \
    createSalesPrice -body "{\"legal_entity_id\": \"$LE\", \"item_key\": \"BRACKET-M8\", \"customer_id\": \"$customer\", \"unit\": \"EA\", \"unit_price\": {\"amount\": \"1174.99\", \"currency\": \"CAD\"}, \"price_unit\": 100, \"valid_from\": \"$from\"}"
  expect "the customer's price by name" "$(field '"\(.customer_name) \(.unit_price.amount) \(.price_unit)"')" "Northwind Traders 1174.99 100"
  api 200 '.items[] | "  \(.item_key) \(.customer_number // "every customer"): \(.unit_price.amount) \(.unit_price.currency) per \(.price_unit) \(.unit) from \(.valid_from)"' \
    listSalesPrices -p "legal_entity_id=$LE" -p item_key=BRACKET-M8 -p "valid_on=$TODAY" -p order_by=valid_from
  expect "the prices of the key valid today" "$(field '[.items[] | .unit_price.amount] | sort | join(" ")')" "1174.99 12.50"
  as sally
  say "Sally quotes Northwind 250 brackets without a price, and the installation at a price she enters. The bracket"
  say "takes Northwind's price: 250 at 1174.99 per 100 are 2937.475, 2937.48 rounded half away from zero. The"
  say "installation keeps her price:"
  item="{\"item_key\": \"BRACKET-M8\", \"description\": \"Machined bracket M8\", \"quantity\": {\"value\": \"250\", \"unit\": \"EA\"}, \"account_id\": \"$sales\", \"tax_code\": \"GO5\"}"
  manual="{\"description\": \"Installation on site\", \"quantity\": {\"value\": \"2.5\", \"unit\": \"H\"}, \"unit_price\": {\"amount\": \"95.01\", \"currency\": \"CAD\"}, \"account_id\": \"$sales\", \"tax_code\": \"GO5\"}"
  api 201 '"quotation \(.document_number) to \(.customer_number) \(.customer_name): net \(.net_amount.amount) \(.net_amount.currency)", (.items[] | "  item \(.item_no) \(.item_key // "-") \(.description): \(.quantity.value) \(.quantity.unit) at \(.unit_price.amount) per \(.price_unit) (\(.price_source)) = \(.net_amount.amount)")' \
    createQuotation -body "{\"legal_entity_id\": \"$LE\", \"customer_id\": \"$customer\", \"valid_until\": \"$until\", \"currency\": \"CAD\", \"items\": [$item, $manual]}"
  expect "each item's price, its source and its value" \
    "$(field '[.items[] | "\(.unit_price.amount) per \(.price_unit) \(.price_source) \(.net_amount.amount)"] | join(", ")')" \
    "1174.99 per 100 price_list 2937.48, 95.01 per 1 manual 237.53"
  expect "the net value" "$(field '"\(.net_amount.amount) \(.net_amount.currency)"')" "3175.01 CAD"
  say "Contoso, a new customer, has no price of its own. Ruth creates it and Carla approves it, as Northwind was:"
  as ruth
  api 201 '"business partner \(.number) \(.name): \(.status), \(.roles | join(", "))"' createBusinessPartner \
    -body "{\"kind\": \"organization\", \"name\": \"Contoso Ltd\", \"country_code\": \"CA\", \"roles\": [\"customer\"], \"customer_companies\": [{\"legal_entity_id\": \"$LE\", \"reconciliation_account_id\": \"$ar\", \"payment_term_code\": \"NT30\"}], \"reason\": \"Buys machined parts from Maple\"}"
  contoso=$(field .id)
  QUIET=1 api 200 '' listPartnerChanges -p "partner_id=$contoso" -p kind=create
  approve carla "$(field '.items[0].workflow_request_id')" "Customer confirmed against its purchase order"
  as sally
  say "The same item for Contoso takes the general price:"
  api 201 '"quotation \(.document_number) to \(.customer_number) \(.customer_name): net \(.net_amount.amount) \(.net_amount.currency)", (.items[] | "  item \(.item_no) \(.item_key) \(.description): \(.quantity.value) \(.quantity.unit) at \(.unit_price.amount) per \(.price_unit) (\(.price_source)) = \(.net_amount.amount)")' \
    createQuotation -body "{\"legal_entity_id\": \"$LE\", \"customer_id\": \"$contoso\", \"valid_until\": \"$until\", \"currency\": \"CAD\", \"items\": [$item]}"
  quotation=$(field .id)
  expect "the general price on Contoso's item" "$(field '.items[0] | "\(.unit_price.amount) per \(.price_unit) \(.price_source) \(.net_amount.amount)"')" \
    "12.50 per 1 price_list 3125.00"
  say "WASHER-M8 has no price record, and an item without a key cannot be looked up: each is refused naming the"
  say "price the item lacks:"
  refused 422 sales.no_price createQuotation \
    -body "{\"legal_entity_id\": \"$LE\", \"customer_id\": \"$contoso\", \"valid_until\": \"$until\", \"currency\": \"CAD\", \"items\": [${item/BRACKET-M8/WASHER-M8}]}"
  expect "the field refused" "$(field '.errors[0].field')" "items[0].unit_price"
  refused 422 sales.no_price createQuotation \
    -body "{\"legal_entity_id\": \"$LE\", \"customer_id\": \"$contoso\", \"valid_until\": \"$until\", \"currency\": \"CAD\", \"items\": [$item, ${item/\"item_key\": \"BRACKET-M8\", /}]}"
  expect "the field refused" "$(field '.errors[0].field')" "items[1].unit_price"
  say "A price unit belongs to a price the item sends: one sent without a price is refused naming it, since the"
  say "record's price comes with its own (product owner 2026-11-12, T80d):"
  refused 422 request.validation_failed createQuotation \
    -body "{\"legal_entity_id\": \"$LE\", \"customer_id\": \"$contoso\", \"valid_until\": \"$until\", \"currency\": \"CAD\", \"items\": [${item/\"account_id\"/\"price_unit\": 100, \"account_id\"}]}"
  expect "the field refused" "$(field '.errors[0].field')" "items[0].price_unit"
  say "Paula raises the general price to 13.25 from today. A record is never changed: the new price is a new"
  say "record, and Contoso's quotation keeps the price it was made with:"
  as paula
  api 201 '"price of \(.item_key) for every customer: \(.unit_price.amount) \(.unit_price.currency) per \(.price_unit) \(.unit), valid from \(.valid_from)"' \
    createSalesPrice -body "{\"legal_entity_id\": \"$LE\", \"item_key\": \"BRACKET-M8\", \"unit\": \"EA\", \"unit_price\": {\"amount\": \"13.25\", \"currency\": \"CAD\"}, \"valid_from\": \"$TODAY\"}"
  as sally
  api 200 '"quotation \(.document_number): item 1 at \(.items[0].unit_price.amount) per \(.items[0].price_unit) = \(.items[0].net_amount.amount), net \(.net_amount.amount) \(.net_amount.currency)"' \
    getQuotation -p "id=$quotation"
  expect "the item priced before" "$(field '.items[0] | "\(.unit_price.amount) \(.net_amount.amount)"')" "12.50 3125.00"
  say "Maple now gives Northwind a discount. Discounts need a pricing procedure (ADR-0067 §11): the condition types"
  say "and the order they apply in are the tenant's configuration, and none is shipped. Ada proposes a price, PR00,"
  say "and a customer discount, K007, a percentage of the value before it. They post nothing, so the change is"
  say "tier 2, approved by the process owner:"
  as ada
  QUIET=1 api 200 '' listConfigVersions -p is_active=true
  base=$(field '.items[0].number')
  jq "{title: \"Pricing procedure\", rationale: \"Maple's condition types and their order (T80b)\", base_version: $base,
    proposed: (.items[0].document | .pricing = {condition_types: [{code: \"PR00\", name: \"Price\", kind: \"price\"},
      {code: \"K007\", name: \"Customer discount\", kind: \"discount_percent\"}], procedure: [{condition_type: \"PR00\"}, {condition_type: \"K007\"}]})}" \
    "$WORK/last.json" >"$WORK/pricing.json"
  api 201 '"change request \(.number), \(.status): \(.title)"' createChangeRequest -body @pricing.json
  cr=$(field .id)
  api 200 '"\(.status), tier \(.tier): \([.items[].path] | join(", "))"' submitChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "the change's tier" "$(field .tier)" 2
  expect "the pricing settings among the changes" "$(field '[.items[] | select(.path | startswith("pricing."))] | length')" 4
  request=$(field .workflow_request_id)
  approve priya "$request" "The pricing procedure, as agreed with sales"
  as ada
  QUIET=1 api 200 '' getChangeRequest -p "id=$cr"
  api 200 '"\(.status): configuration version \(.activated_version)"' activateChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "status after activation" "$(field .status)" active
  as paula
  say "Paula records Northwind's discount on the bracket, five percent. A record of a condition type the"
  say "configuration does not have is refused:"
  api 201 '"\(.kind) \(.condition_type) on \(.item_key) for \(.customer_number) \(.customer_name): \(.percent) percent, valid from \(.valid_from)"' \
    createSalesPrice -body "{\"legal_entity_id\": \"$LE\", \"item_key\": \"BRACKET-M8\", \"customer_id\": \"$customer\", \"condition_type\": \"K007\", \"percent\": \"5\", \"valid_from\": \"$from\"}"
  expect "the discount" "$(field '"\(.kind) \(.condition_type) \(.percent) \(.customer_name)"')" "discount_percent K007 5 Northwind Traders"
  refused 422 sales.unknown_condition_type createSalesPrice \
    -body "{\"legal_entity_id\": \"$LE\", \"item_key\": \"BRACKET-M8\", \"customer_id\": \"$customer\", \"condition_type\": \"ZZ00\", \"percent\": \"5\", \"valid_from\": \"$from\"}"
  expect "the field refused" "$(field '.errors[0].field')" "condition_type"
  as sally
  say "A new quotation to Northwind shows each condition applied to the item and the net value after it: the price,"
  say "2937.48, less five percent of it, 146.874: 146.87 rounded half away from zero, leaves 2790.61:"
  api 201 '"quotation \(.document_number) to \(.customer_number) \(.customer_name): net \(.net_amount.amount) \(.net_amount.currency)", (.items[] | "  item \(.item_no) \(.item_key) \(.description): \(.quantity.value) \(.quantity.unit) at \(.unit_price.amount) per \(.price_unit) (\(.price_source)), net \(.net_amount.amount)", (.conditions[] | "    \(.condition_type) \(.name): \(if .kind == "price" then "\(.unit_price.amount) per \(.price_unit)" else "\(.percent) percent" end) = \(.amount.amount) \(.amount.currency)"))' \
    createQuotation -body "{\"legal_entity_id\": \"$LE\", \"customer_id\": \"$customer\", \"valid_until\": \"$until\", \"currency\": \"CAD\", \"items\": [$item]}"
  expect "the conditions applied" "$(field '[.items[0].conditions[] | "\(.condition_type) \(.kind) \(.amount.amount)"] | join(", ")')" \
    "PR00 price 2937.48, K007 discount_percent -146.87"
  expect "the discount's rate" "$(field '.items[0].conditions[1].percent')" 5
  expect "the item's net value after the discount" "$(field '"\(.items[0].net_amount.amount) \(.net_amount.amount) \(.net_amount.currency)"')" "2790.61 2790.61 CAD"
  say "For a rush order Sally agrees 11.00 CAD a bracket with Northwind and enters it. A price a person enters is the"
  say "final net price (product owner 2026-11-12, T80c): Northwind's discount is not taken off it, and the item lists"
  say "its price alone, 250 at 11.00, 2750.00:"
  entered="{\"item_key\": \"BRACKET-M8\", \"description\": \"Machined bracket M8\", \"quantity\": {\"value\": \"250\", \"unit\": \"EA\"}, \"unit_price\": {\"amount\": \"11.00\", \"currency\": \"CAD\"}, \"account_id\": \"$sales\", \"tax_code\": \"GO5\"}"
  api 201 '"quotation \(.document_number) to \(.customer_number) \(.customer_name): net \(.net_amount.amount) \(.net_amount.currency)", (.items[] | "  item \(.item_no) \(.item_key) \(.description): \(.quantity.value) \(.quantity.unit) at \(.unit_price.amount) per \(.price_unit) (\(.price_source)), net \(.net_amount.amount)", (.conditions[] | "    \(.condition_type) \(.name): \(if .kind == "price" then "\(.unit_price.amount) per \(.price_unit)" else "\(.percent) percent" end) = \(.amount.amount) \(.amount.currency)"))' \
    createQuotation -body "{\"legal_entity_id\": \"$LE\", \"customer_id\": \"$customer\", \"valid_until\": \"$until\", \"currency\": \"CAD\", \"items\": [$entered]}"
  expect "the entered price's source and conditions" "$(field '.items[0] | "\(.price_source): \([.conditions[] | "\(.condition_type) \(.kind) \(.amount.amount)"] | join(", "))"')" \
    "manual: PR00 price 2750.00"
  expect "the entered price in full" "$(field '"\(.items[0].net_amount.amount) \(.net_amount.amount) \(.net_amount.currency)"')" "2750.00 2750.00 CAD"
  passed
}
# step_revenue_contract is T81a (ADR-0068 §3-§4): Sally confirms a second
# sales order, of two items, goods and a support service for the fiscal
# year; the revenue context reads the order's event and creates its
# revenue contract by itself. Sally enters the brackets' price, which is
# final (T80c): Northwind's discount of step_price_list is not taken off
# it. Nora, the revenue accountant, reads it: a
# draft with the order's net value, without tax, as its transaction price
# and one performance obligation per order item, each satisfied at a
# point in time with the item's net value as its stand-alone selling
# price. A request for the same order is refused, since an order has one
# contract, and so is one for an order that is not confirmed. T81b
# (ADR-0068 §7): Nora sets the support service to over time for the fiscal
# year with a stand-alone selling price above its order value and
# activates the contract; the transaction price is allocated to the two
# obligations in proportion to their stand-alone selling prices, the
# amounts adding up to it to the cent; an active contract is fixed. The
# contract of step 63's order, activated with the prices it started with,
# is allocated the order's item values. Nothing is posted. REVENUE_ORDER
# and REVENUE_CONTRACT are the order and its active contract for later
# steps.
step_revenue_contract() {
  heading "Step 67 — Revenue contracts: a confirmed sales order gets its revenue contract by itself, one performance obligation per order item, the order's net value without tax as its transaction price; one contract per order; the price allocated in proportion to stand-alone selling prices when the contract is activated; nothing posted"
  local customer=$CUSTOMER sales item1 item2 order number contract open i draft year first
  [ -n "$SALES_ORDER" ] || fail "step 63's sales order is not there"
  as ada
  say "Ada adds Nora, the revenue accountant, to the team:"
  person nora "Nora Revenue" finance_staff
  as sally
  QUIET=1 api 200 '' listOpenItems -p "legal_entity_id=$LE" -p "partner_id=$customer"
  open=$(field '[.items[] | "\(.journal_entry_id) \(.amount.amount)"] | join(", ")')
  sales=$(account 4000)
  say "Northwind orders 120 more brackets and a year of support for them. Sally creates the order directly and"
  say "confirms it. The order's amounts are net: the GST is calculated when it is billed (ADR-0067 §3). Since step 65"
  say "Northwind has five percent off the bracket's recorded price; Sally enters the price here, and an entered price"
  say "is final (ADR-0067 §11, T80c): 120 at 12.50 are 1500.00; with the support, 2700.00."
  item1="{\"item_key\": \"BRACKET-M8\", \"description\": \"Machined bracket M8\", \"quantity\": {\"value\": \"120\", \"unit\": \"EA\"}, \"unit_price\": {\"amount\": \"12.50\", \"currency\": \"CAD\"}, \"account_id\": \"$sales\", \"tax_code\": \"GO5\"}"
  item2="{\"item_key\": \"SUPPORT-1Y\", \"description\": \"Support for the fiscal year\", \"quantity\": {\"value\": \"1\", \"unit\": \"EA\"}, \"unit_price\": {\"amount\": \"1200.00\", \"currency\": \"CAD\"}, \"account_id\": \"$sales\", \"tax_code\": \"GO5\"}"
  api 201 '"sales order \(.document_number) of \(.customer_number) \(.customer_name): \(.status), net \(.net_amount.amount) \(.net_amount.currency)", (.items[] | "  item \(.item_no) \(.item_key // "-") \(.description): \(.quantity.value) \(.quantity.unit) at \(.unit_price.amount) = \(.net_amount.amount)")' \
    createSalesOrder -body "{\"legal_entity_id\": \"$LE\", \"customer_id\": \"$customer\", \"currency\": \"CAD\", \"items\": [$item1, $item2]}"
  order=$(field .id)
  number=$(field .document_number)
  expect "the order's net value, the entered prices without a discount" "$(field '"\(.net_amount.amount) \(.net_amount.currency)"')" "2700.00 CAD"
  expect "each item's net value" "$(field '[.items[].net_amount.amount] | join(" ")')" "1500.00 1200.00"
  api 200 '"sales order \(.document_number): \(.status), version \(.version)"' confirmSalesOrder -p "id=$order" -if-match "$ETAG"
  expect "the confirmed order" "$(field .status)" confirmed
  as nora
  say "Confirming the order published SalesOrderChanged through the sales context's outbox. The revenue context"
  say "reads it from the event bus and creates the order's revenue contract, once per order (ADR-0068 §4): nobody"
  say "asks for it, and the sales context does not know of it. Nora finds it by its order:"
  for i in $(seq 1 240); do
    QUIET=1 api 200 '' listRevenueContracts -p "sales_order_id=$order"
    if [ "$(field '.items | length')" = 1 ]; then break; fi
    sleep 0.25
  done
  api 200 '.items[] | "revenue contract of sales order \(.sales_order_number), \(.customer_number) \(.customer_name), \(.company_code) \(.company_name): \(.status), transaction price \(.transaction_price.amount) \(.transaction_price.currency)", (.obligations[] | "  obligation \(.obligation_no) (order item \(.order_item_no)) \(.item_key // "-") \(.description): \(.timing), order value \(.net_amount.amount), stand-alone selling price \(.standalone_selling_price.amount) \(.standalone_selling_price.currency)")' \
    listRevenueContracts -p "sales_order_id=$order"
  expect "the order's revenue contracts (after $i checks)" "$(field '.items | length')" 1
  contract=$(field '.items[0].id')
  expect "the contract's order, customer, currency and status" \
    "$(field '.items[0] | "\(.sales_order_id) \(.sales_order_number) \(.customer_id) \(.currency) \(.status) \(.version)"')" "$order $number $customer CAD draft 1"
  say "Its transaction price is the order's net value, without the GST the order will be billed with; it has one"
  say "performance obligation per order item, each starting as satisfied at a point in time with the item's net"
  say "value as its stand-alone selling price. What is a distinct obligation, when it is satisfied and what its"
  say "stand-alone selling price is are Nora's judgement, not the system's (ADR-0068 §2):"
  expect "the transaction price, the order's net value" "$(field '.items[0].transaction_price | "\(.amount) \(.currency)"')" "2700.00 CAD"
  expect "each obligation's order item, timing, order value and stand-alone selling price" \
    "$(field '[.items[0].obligations[] | "\(.obligation_no)=\(.order_item_no) \(.timing) \(.net_amount.amount) \(.standalone_selling_price.amount)"] | join(", ")')" \
    "1=1 point_in_time 1500.00 1500.00, 2=2 point_in_time 1200.00 1200.00"
  say "Read back by its id, it is the same contract; the system created it, not a person:"
  api 200 '"revenue contract of sales order \(.sales_order_number): \(.status), version \(.version), transaction price \(.transaction_price.amount) \(.transaction_price.currency), \(.obligations | length) obligations"' \
    getRevenueContract -p "id=$contract"
  expect "the contract read back" "$(field '"\(.id) \(.status) \(.version) \(.transaction_price.amount) \(.obligations | length)"')" "$contract draft 1 2700.00 2"
  if cut -f1 "$WORK/names" | grep -qx "$(field .created_by)"; then fail "the contract was created by a person, not by the system"; fi
  say "A person can ask for the contract of a confirmed order that has none yet (one confirmed before contracts were"
  say "created by themselves). This order has its contract, and an order has one, so Nora's request is refused:"
  refused 409 revenue.contract_exists createRevenueContract -body "{\"sales_order_id\": \"$order\"}"
  QUIET=1 api 200 '' listRevenueContracts -p "sales_order_id=$order"
  expect "the order's revenue contracts" "$(field '[.items[].id] | join(" ")')" "$contract"
  say "An order that is not confirmed has no contract and gets none on request:"
  as sally
  QUIET=1 api 201 '' createSalesOrder -body "{\"legal_entity_id\": \"$LE\", \"customer_id\": \"$customer\", \"currency\": \"CAD\", \"items\": [$item1]}"
  draft=$(field .id)
  as nora
  refused 422 revenue.order_not_confirmed createRevenueContract -body "{\"sales_order_id\": \"$draft\"}"
  expect "the field refused" "$(field '.errors[0].field')" sales_order_id
  say "The brackets are goods; the support is a service Northwind receives over the fiscal year. That is Nora's"
  say "judgement to make: she sets the support's obligation to over time with its service period. She also knows that"
  say "Maple sells a year of support on its own for 1300.00, above the 1200.00 on this order, and enters that as the"
  say "obligation's stand-alone selling price (ADR-0068 §2, §7):"
  year=$(company_day)
  year=${year:0:4}
  QUIET=1 api 200 '' getRevenueContract -p "id=$contract"
  api 200 '"revenue contract of sales order \(.sales_order_number): \(.status), version \(.version)", (.obligations[] | "  obligation \(.obligation_no) \(.description): \(.timing)" + (if .service_from then " from \(.service_from) to \(.service_to)" else "" end) + ", stand-alone selling price \(.standalone_selling_price.amount) \(.standalone_selling_price.currency)")' \
    updateRevenueContract -p "id=$contract" -if-match "$ETAG" \
    -body "{\"obligations\": [{\"obligation_no\": 2, \"timing\": \"over_time\", \"service_from\": \"$year-01-01\", \"service_to\": \"$year-12-31\", \"standalone_selling_price\": {\"amount\": \"1300.00\", \"currency\": \"CAD\"}}]}"
  expect "the obligations after the change" \
    "$(field '[.obligations[] | "\(.timing) \(.service_from // "-") \(.service_to // "-") \(.standalone_selling_price.amount)"] | join(", ")')" \
    "point_in_time - - 1500.00, over_time $year-01-01 $year-12-31 1300.00"
  expect "the draft's version and allocated amounts" "$(field '"\(.status) \(.version) " + ([.obligations[] | .allocated_amount.amount // "-"] | join(" "))')" "draft 2 - -"
  say "An over-time obligation needs its service period: setting the brackets to over time without one is refused."
  refused 422 revenue.service_period_required updateRevenueContract -p "id=$contract" -if-match "$ETAG" \
    -body '{"obligations": [{"obligation_no": 1, "timing": "over_time"}]}'
  expect "the field refused" "$(field '.errors[0].field')" "obligations[0].service_from"
  say "Nora activates the contract. The system allocates the transaction price to the obligations in proportion to"
  say "their stand-alone selling prices, 1500.00 and 1300.00: 2700.00 x 1500 / 2800 is 1446.428..., 2700.00 x 1300 / 2800"
  say "is 1253.571...; the whole cents leave one cent over, which goes to the larger remainder, the brackets'. That"
  say "arithmetic is all the system does:"
  QUIET=1 api 200 '' getRevenueContract -p "id=$contract"
  api 200 '"revenue contract of sales order \(.sales_order_number): \(.status), version \(.version), transaction price \(.transaction_price.amount) \(.transaction_price.currency)", (.obligations[] | "  obligation \(.obligation_no) \(.description): order value \(.net_amount.amount), stand-alone selling price \(.standalone_selling_price.amount), allocated \(.allocated_amount.amount) \(.allocated_amount.currency)")' \
    activateRevenueContract -p "id=$contract" -if-match "$ETAG"
  expect "the active contract" "$(field '"\(.status) \(.version)"')" "active 3"
  expect "the allocated amounts" "$(field '[.obligations[] | .allocated_amount.amount] | join(" ")')" "1446.43 1253.57"
  expect "the allocated amounts' sum in cents, the transaction price's" \
    "$(field '[.obligations[] | .allocated_amount.amount | sub("\\."; "") | tonumber] | add')" "$(field '.transaction_price.amount | sub("\\."; "") | tonumber')"
  say "The two amounts add up to the transaction price, 2700.00, to the cent, and differ from the order's item values,"
  say "1500.00 and 1200.00, as the proportion gives. An active contract is fixed: a change is refused, and so is a"
  say "second activation."
  refused 409 revenue.invalid_status updateRevenueContract -p "id=$contract" -if-match "$ETAG" \
    -body '{"obligations": [{"obligation_no": 1, "standalone_selling_price": {"amount": "1.00", "currency": "CAD"}}]}'
  QUIET=1 api 200 '' getRevenueContract -p "id=$contract"
  refused 409 revenue.invalid_status activateRevenueContract -p "id=$contract" -if-match "$ETAG"
  say "The order of step 63 got its contract the same way when Sally confirmed it. Nobody changed its obligations, so"
  say "they have the prices they started with, the order items' net values, and activating it allocates those values:"
  QUIET=1 api 200 '' listRevenueContracts -p "sales_order_id=$SALES_ORDER"
  expect "the revenue contracts of step 63's order" "$(field '.items | length')" 1
  first=$(field '.items[0].id')
  QUIET=1 api 200 '' getRevenueContract -p "id=$first"
  api 200 '"revenue contract of sales order \(.sales_order_number): \(.status), transaction price \(.transaction_price.amount) \(.transaction_price.currency)", (.obligations[] | "  obligation \(.obligation_no) \(.description): order value \(.net_amount.amount), allocated \(.allocated_amount.amount) \(.allocated_amount.currency)")' \
    activateRevenueContract -p "id=$first" -if-match "$ETAG"
  expect "the allocated amounts, the order's item values" \
    "$(field '[.obligations[] | "\(.net_amount.amount)=\(.allocated_amount.amount)"] | join(" ")')" "1500.00=1500.00 237.53=237.53"
  REVENUE_ORDER=$order
  REVENUE_CONTRACT=$contract
  say "A revenue contract posts nothing: Northwind's open items are as they were."
  QUIET=1 api 200 '' listOpenItems -p "legal_entity_id=$LE" -p "partner_id=$customer"
  expect "the customer's open items" "$(field '[.items[] | "\(.journal_entry_id) \(.amount.amount)"] | join(", ")')" "$open"
  passed
}

# step_credit_block is T77b (ADR-0066 and ADR-0067, As built T77b): with
# Northwind's limit of step_credit_limit, Sally confirms an order within
# what is available, without a credit block, and a larger one, which is
# confirmed blocked: its delivery is refused. Releasing the block is the
# credit manager's (sales.order.credit_release, which finance_staff does
# not hold): Sally is refused, Ruth releases it, once, and the delivery
# is confirmed. Throughout, the exposure shows Northwind's confirmed
# orders not yet billed, gross, beside its open items; a blocked order
# counts only once it is released. The amounts follow from the exposure
# read, so the step holds whatever earlier steps left open. Of the earlier
# orders, step 63's no longer counts, since step 64 posted its billing
# document as a customer invoice (T79b), which Northwind paid; step 67's
# (REVENUE_ORDER), confirmed and not billed, counts at its gross value.
step_credit_block() {
  heading "Step 68 — Order credit blocks: a sales order's gross value checked against the customer's credit limit when it is confirmed; an order within the limit confirmed without a block, a larger one blocked and refused a delivery; the block released by the credit manager, once; confirmed orders not yet billed in the exposure beside the open items"
  local customer=$CUSTOMER sales role ruth items orders count exposure limit available small large number day tag
  [ -n "$customer" ] || fail "step 47's customer is not there"
  [ -n "$SALES_ORDER" ] && [ -n "$REVENUE_ORDER" ] || fail "the sales orders of steps 63 and 67 are not there"
  as ada
  say "Releasing a credit block is a credit manager's decision, not day-to-day work: finance_staff does not hold"
  say "sales.order.credit_release. Ada creates the role and gives it to Ruth, who looks after Maple's credit:"
  api 201 '"role \(.code), \(.name)"' createRole -body '{"code": "credit_manager", "name": "Credit manager"}'
  role=$(field .id)
  grant "$role" sales.order.credit_release
  ruth=$(awk -F'\t' '$2 == "Ruth Receivables" { print $1 }' "$WORK/names")
  [ -n "$ruth" ] || fail "step 47's Ruth is not there"
  QUIET=1 api 201 '' createRoleAssignment -body "{\"principal_id\": \"$ruth\", \"role_id\": \"$role\"}"
  as ruth
  sales=$(account 4000)
  say "Since step 59 Northwind has ordered twice (steps 63 and 67). Its exposure now has two parts (ADR-0066, As built"
  say "T77b): its open items, and its confirmed sales orders not yet billed at their gross value, net plus GST. An"
  say "order counts until its billing document is posted as a customer invoice, which is then the open item:"
  api 200 '"\(.customer_number) \(.customer_name): open items \(.open_items.amount) (\(.open_item_count)) + open orders \(.open_orders.amount) (\(.open_order_count)) = exposure \(.exposure.amount) \(.exposure.currency); limit \(.limit.amount), available \(.available.amount)"' \
    getCreditExposure -p "legal_entity_id=$LE" -p "customer_id=$customer"
  items=$(field .open_items.amount) orders=$(field .open_orders.amount) count=$(field .open_order_count)
  exposure=$(field .exposure.amount) limit=$(field .limit.amount) available=$(field .available.amount)
  expect "the exposure, its two parts together" "$exposure" "$(awk -v a="$items" -v b="$orders" 'BEGIN { printf "%.2f", a + b }')"
  expect "what is available, the limit less the exposure" "$available" "$(awk -v a="$limit" -v b="$exposure" 'BEGIN { printf "%.2f", a - b }')"
  say "Step 63's order no longer counts: step 64 posted its billing document as a customer invoice, the open item"
  say "Northwind then paid. Step 67's order is confirmed and not billed: 2700.00 net with 5% GST, 135.00, counts in full."
  QUIET=1 api 200 '' listBillingDocuments -p "sales_order_id=$SALES_ORDER"
  expect "step 63's order, billed in full by a posted document" "$(field '[.items[].status] | join(" ")')" posted
  QUIET=1 api 200 '' listBillingDocuments -p "sales_order_id=$REVENUE_ORDER"
  expect "step 67's order, not billed" "$(field '.items | length')" 0
  expect "Northwind's open orders: step 67's alone, at its gross value" "$count $orders" "1 2835.00"
  as sally
  say "Sally takes an order for 100 brackets M10 at 10.00: 1000.00 net, 1050.00 with 5% GST, within what is"
  say "available. Confirming it checks the gross value against the limit less the exposure; it is confirmed without a"
  say "credit block:"
  QUIET=1 api 201 '' createSalesOrder -body "{\"legal_entity_id\": \"$LE\", \"customer_id\": \"$customer\", \"currency\": \"CAD\", \"items\": [{\"description\": \"Machined bracket M10\", \"quantity\": {\"value\": \"100\", \"unit\": \"EA\"}, \"unit_price\": {\"amount\": \"10.00\", \"currency\": \"CAD\"}, \"account_id\": \"$sales\", \"tax_code\": \"GO5\"}]}"
  small=$(field .id)
  api 200 '"sales order \(.document_number): \(.status), net \(.net_amount.amount) \(.net_amount.currency), credit block \(.credit_block)"' \
    confirmSalesOrder -p "id=$small" -if-match "$ETAG"
  expect "the order within the limit" "$(field '"\(.status) \(.credit_block) \(.net_amount.amount)"')" "confirmed none 1000.00"
  as ruth
  api 200 '"open orders \(.open_orders.amount) (\(.open_order_count)), exposure \(.exposure.amount), available \(.available.amount) \(.available.currency)"' \
    getCreditExposure -p "legal_entity_id=$LE" -p "customer_id=$customer"
  expect "the order in the exposure at its gross value, 1050.00" "$(field '"\(.open_order_count) \(.open_orders.amount) \(.exposure.amount) \(.available.amount)"')" \
    "$((count + 1)) $(awk -v a="$orders" 'BEGIN { printf "%.2f", a + 1050 }') $(awk -v a="$exposure" 'BEGIN { printf "%.2f", a + 1050 }') $(awk -v a="$available" 'BEGIN { printf "%.2f", a - 1050 }')"
  available=$(field .available.amount)
  expect "less than 5250.00 available" "$(jq -rn --arg a "$available" '($a | tonumber) < 5250')" true
  as sally
  say "A second order, 500 brackets, is 5000.00 net and 5250.00 with GST: more than the $available CAD available. It is"
  say "confirmed all the same, with a credit block, and SalesOrderCreditBlocked tells whoever listens:"
  QUIET=1 api 201 '' createSalesOrder -body "{\"legal_entity_id\": \"$LE\", \"customer_id\": \"$customer\", \"currency\": \"CAD\", \"items\": [{\"description\": \"Machined bracket M10\", \"quantity\": {\"value\": \"500\", \"unit\": \"EA\"}, \"unit_price\": {\"amount\": \"10.00\", \"currency\": \"CAD\"}, \"account_id\": \"$sales\", \"tax_code\": \"GO5\"}]}"
  large=$(field .id)
  number=$(field .document_number)
  api 200 '"sales order \(.document_number): \(.status), net \(.net_amount.amount) \(.net_amount.currency), credit block \(.credit_block)"' \
    confirmSalesOrder -p "id=$large" -if-match "$ETAG"
  tag=$ETAG
  expect "the order over the limit" "$(field '"\(.status) \(.credit_block) \(.net_amount.amount) \(has("credit_released_by"))"')" "confirmed blocked 5000.00 false"
  say "A blocked order takes no delivery, and so nothing of it is billed:"
  day=$(company_day)
  refused 409 sales.order_credit_blocked confirmSalesDelivery -p "id=$large" \
    -body "{\"delivery_date\": \"$day\", \"items\": [{\"item_no\": 1, \"quantity\": {\"value\": \"500\", \"unit\": \"EA\"}}]}"
  say "The credit manager finds the orders waiting for her, and sees that a blocked order takes none of the"
  say "customer's room: the exposure is as it was."
  as ruth
  api 200 '.items[] | "  \(.document_number) \(.customer_number) \(.customer_name): \(.net_amount.amount) \(.net_amount.currency), credit block \(.credit_block)"' \
    listSalesOrders -p "legal_entity_id=$LE" -p credit_block=blocked
  expect "the blocked orders" "$(field '[.items[].document_number] | join(" ")')" "$number"
  QUIET=1 api 200 '' getCreditExposure -p "legal_entity_id=$LE" -p "customer_id=$customer"
  expect "what is available with the order blocked" "$(field .available.amount)" "$available"
  say "Sally may not release the block herself:"
  as sally
  refused 403 identity.permission_missing releaseSalesOrderCreditBlock -p "id=$large" -if-match "$tag"
  as ruth
  say "Ruth speaks to Northwind, which has always paid, and releases the block at the version she read. It is"
  say "released once, with who released it and when, audited and published as SalesOrderCreditReleased:"
  api 200 '"sales order \(.document_number): \(.status), credit block \(.credit_block) by \(.credit_released_by_name) at \(.credit_released_at), version \(.version)"' \
    releaseSalesOrderCreditBlock -p "id=$large" -if-match "$tag"
  expect "the released order" "$(field '"\(.status) \(.credit_block) \(.credit_released_by) \(.credit_released_by_name) \(.version)"')" "confirmed released $ruth Ruth Receivables 3"
  refused 409 sales.order_not_credit_blocked releaseSalesOrderCreditBlock -p "id=$large" -if-match "$ETAG"
  api 200 '"open orders \(.open_orders.amount) (\(.open_order_count)), exposure \(.exposure.amount), limit \(.limit.amount), available \(.available.amount); within the limit: \(.is_within_limit)"' \
    getCreditExposure -p "legal_entity_id=$LE" -p "customer_id=$customer"
  expect "the released order in the exposure, now over the limit" "$(field '"\(.open_order_count) \(.available.amount) \(.is_within_limit)"')" \
    "$((count + 2)) $(awk -v a="$available" 'BEGIN { printf "%.2f", a - 5250 }') false"
  as sally
  say "Now the order is delivered as any confirmed order:"
  api 201 '"delivery \(.delivery_no) of sales order \(.sales_order_number) on \(.delivery_date): " + ([.items[] | "item \(.item_no) \(.quantity.value) \(.quantity.unit)"] | join(", "))' \
    confirmSalesDelivery -p "id=$large" \
    -body "{\"delivery_date\": \"$day\", \"items\": [{\"item_no\": 1, \"quantity\": {\"value\": \"500\", \"unit\": \"EA\"}}]}"
  expect "the delivery of the released order" "$(field '"\(.delivery_no) \(.sales_order_number)"')" "1 $number"
  as ruth
  QUIET=1 api 200 '' listAuditEvents -p object_type=sales.sales_order -p "object_id=$large"
  expect "the order's audit trail" "$(field '[.items[].action] | sort | join(" ")')" \
    "sales.sales_order.confirm sales.sales_order.create sales.sales_order.release_credit_block"
  passed
}

# schedule_line_body DATE QUANTITY UNIT is the request for a schedule line
# of an agreement's item 1.
schedule_line_body() {
  echo "{\"contract_item_no\": 1, \"delivery_date\": \"$1\", \"quantity\": {\"value\": \"$2\", \"unit\": \"$3\"}}"
}

# step_scheduling_agreement is T83a and T83b (ADR-0070): Ben creates a
# scheduling agreement with Tyrell, a contract of its own kind, for one
# item, releases it as any contract and enters its delivery schedule: two
# lines for the coming two months, in the agreement's unit and within its
# validity, a year from the company's date today. He then calls the first
# line off (T83b): a draft purchase order against the agreement at its
# price, for the line's quantity and date, which he releases as any
# order; the release consumes the agreement's target value. It keeps the
# agreement (AGREEMENT).
step_scheduling_agreement() {
  heading "Step 69 — Scheduling agreements: a scheduling agreement with one item is created and released, and two schedule lines are entered for the coming two months; the first is called off as a purchase order, whose release consumes the agreement"
  as ben
  local today until first second expense body plain tag shown po
  today=$(company_day)
  until=$(date_add "$today" 364)
  first=$(date_add "$today" 30)
  second=$(date_add "$today" 60)
  expense=$(account 6000)
  say "Maple uses toner steadily and wants it delivered month by month at one price. Ben creates a scheduling"
  say "agreement with Tyrell (ADR-0070): a contract of its own kind, priced and valid for a year as any contract:"
  body="{\"kind\": \"scheduling_agreement\", \"legal_entity_id\": \"$LE\", \"supplier_id\": \"$SUPPLIER\", \"currency\": \"CAD\",
    \"valid_from\": \"$today\", \"valid_to\": \"$until\", \"target_value\": {\"amount\": \"50000.00\", \"currency\": \"CAD\"}, \"items\": [
    {\"material_key\": \"TONER-XL\", \"description\": \"Toner cartridges, XL\", \"unit_price\": {\"amount\": \"85.00\", \"currency\": \"CAD\"}, \"uom_code\": \"EA\", \"account_id\": \"$expense\"}]}"
  api 201 '"\(.kind) \(.document_number): \(.status), \(.valid_from) to \(.valid_to), target value \(.target_value.amount) \(.target_value.currency)", (.items[] | "  item \(.item_no) \(.material_key) \(.description): \(.unit_price.amount) \(.unit_price.currency) per \(.price_unit) \(.uom_code)")' \
    createContract -body "$body"
  AGREEMENT=$(field .id) tag=$ETAG
  expect "the new agreement" "$(field '"\(.kind) \(.status) \(.supplier_id) \(.target_value.amount) \(.target_value.currency)"')" \
    "scheduling_agreement draft $SUPPLIER 50000.00 CAD"
  say "Its schedule is entered once it is released; a draft takes no line. Ben releases it:"
  refused 409 procurement.invalid_status createScheduleLine -p "id=$AGREEMENT" -body "$(schedule_line_body "$first" 60 EA)"
  api 200 '"\(.kind) \(.document_number): \(.status)"' releaseContract -p "id=$AGREEMENT" -if-match "$tag"
  expect "the released agreement" "$(field '"\(.kind) \(.status)"')" "scheduling_agreement active"
  say "It is Maple's only scheduling agreement; the contract the RFQ's award created in step 66 is a contract,"
  say "and takes no schedule line:"
  api 200 '.items[] | "  \(.kind) \(.document_number): \(.status)"' listContracts -p "legal_entity_id=$LE" -p kind=scheduling_agreement
  expect "Maple's scheduling agreements" "$(field '[.items[].id] | join(" ")')" "$AGREEMENT"
  QUIET=1 api 200 '' listContracts -p "legal_entity_id=$LE" -p kind=contract -p "supplier_id=$DP_SUPPLIER" -p status=active
  plain=$(field '.items[0].id')
  expect "the contract of step 66 is of the kind contract" "$(field '.items[0] | "\(.kind) \(.rfq_id)"')" "contract $RFQ"
  refused 409 procurement.not_a_scheduling_agreement createScheduleLine -p "id=$plain" -body "$(schedule_line_body "$first" 60 EA)"
  say "Ben enters the delivery schedule: 60 cartridges in a month, 40 in two. Each line names the agreement's"
  say "item, in its unit, on a date within the agreement's validity; its value is its quantity at the agreement's"
  say "price, exact to the cent:"
  shown='"line \(.line_no) of \(.contract_document_number): \(.quantity.value) \(.quantity.unit) of \(.material_key) due \(.delivery_date), at \(.unit_price.amount) per \(.price_unit) = \(.amount.amount) \(.amount.currency)"'
  api 201 "$shown" createScheduleLine -p "id=$AGREEMENT" -body "$(schedule_line_body "$first" 60 EA)"
  expect "the first line" "$(field '"\(.line_no) \(.material_key) \(.delivery_date) \(.quantity.value) \(.quantity.unit) \(.amount.amount) \(.amount.currency)"')" \
    "1 TONER-XL $first 60 EA 5100.00 CAD"
  api 201 "$shown" createScheduleLine -p "id=$AGREEMENT" -body "$(schedule_line_body "$second" 40 EA)"
  expect "the second line" "$(field '"\(.line_no) \(.delivery_date) \(.quantity.value) \(.quantity.unit) \(.amount.amount) \(.amount.currency)"')" \
    "2 $second 40 EA 3400.00 CAD"
  say "A date after the agreement ends is refused, naming its validity, and so is another unit than the item's:"
  refused 422 procurement.schedule_line_outside_validity createScheduleLine -p "id=$AGREEMENT" -body "$(schedule_line_body "$(date_add "$until" 1)" 60 EA)"
  expect "the field refused" "$(field '.errors[0].field')" "delivery_date"
  refused 422 procurement.invalid_contract_item createScheduleLine -p "id=$AGREEMENT" -body "$(schedule_line_body "$first" 60 BOX)"
  say "The agreement's schedule, by delivery date:"
  api 200 '.items[] | "  line \(.line_no): \(.quantity.value) \(.quantity.unit) of \(.material_key) due \(.delivery_date), \(.amount.amount) \(.amount.currency)"' \
    listScheduleLines -p "id=$AGREEMENT"
  expect "the schedule" "$(field '[.items[] | "\(.line_no) \(.status) \(.delivery_date) \(.quantity.value) \(.amount.amount) \(.amount.currency)"] | join(", ")')" \
    "1 scheduled $first 60 5100.00 CAD, 2 scheduled $second 40 3400.00 CAD"
  tag="\"$(field '.items[0].version')\""
  say "The first delivery is to be ordered. Ben calls its line off (ADR-0070 §4), at the version he read: a purchase"
  say "order is created against the agreement, as if he had entered it, and the line names it:"
  api 200 '"line \(.line_no) of \(.contract_document_number): \(.status) as purchase order \(.purchase_order_document_number), version \(.version)"' \
    callOffScheduleLine -p "id=$AGREEMENT" -p line_no=1 -if-match "$tag"
  po=$(field .purchase_order_id)
  expect "the line called off" "$(field '"\(.status) \(.version) \(.purchase_order_document_number | startswith("PO"))"')" "called_off 2 true"
  say "A line is called off once:"
  refused 409 procurement.schedule_line_called_off callOffScheduleLine -p "id=$AGREEMENT" -p line_no=1 -if-match "$ETAG"
  say "The order is a draft of Maple's with Tyrell, dated today: the agreement's item at the agreement's price, in"
  say "the line's quantity, due on the line's date:"
  api 200 '"purchase order \(.document_number): \(.status), dated \(.order_date), \(.total_amount.amount) \(.total_amount.currency), terms \(.payment_term_code)", (.items[] | "  item \(.item_no) \(.description): \(.quantity.value) \(.quantity.unit) at \(.unit_price.amount) per \(.price_unit), delivery \(.delivery_date), agreement item \(.contract_item_no)")' \
    getPurchaseOrder -p "id=$po"
  expect "the draft order" "$(field '"\(.status) \(.legal_entity_id) \(.supplier_id) \(.contract_id) \(.order_date) \(.total_amount.amount) \(.total_amount.currency)"')" \
    "draft $LE $SUPPLIER $AGREEMENT $today 5100.00 CAD"
  expect "its item" "$(field '[.items[] | "\(.quantity.value) \(.quantity.unit) \(.unit_price.amount)/\(.price_unit) \(.account_id) \(.contract_item_no) \(.delivery_date)"] | join(", ")')" \
    "60 EA 85.00/1 $expense 1 $first"
  say "From here it is an order like any other. Ben releases it; an order of this value without a requisition is"
  say "released by the release strategy, so it may wait for an approval first:"
  api 200 '"\(.status)\(if .release_request_id then ": release request \(.release_request_id)" else "" end)"' releasePurchaseOrder -p "id=$po" -if-match "$ETAG"
  if [ "$(field .status)" = pending_approval ]; then
    approve priya "$(field .release_request_id)" "Toner as scheduled"
    as ben
    QUIET=1 api 200 '' getPurchaseOrder -p "id=$po"
  fi
  expect "order status" "$(field .status)" released
  say "The release consumed the agreement's target value by the order's value, exactly; the second line is still"
  say "to call off:"
  api 200 '"\(.kind) \(.document_number): consumed \(.consumed_value.amount) of \(.target_value.amount) \(.target_value.currency)"' getContract -p "id=$AGREEMENT"
  expect "the agreement's consumed value" "$(field '"\(.consumed_value.amount) \(.consumed_value.currency) \(.target_value.amount)"')" "5100.00 CAD 50000.00"
  api 200 '.items[] | "  line \(.line_no): \(.status)\(if .purchase_order_document_number then " as purchase order \(.purchase_order_document_number)" else "" end), \(.quantity.value) \(.quantity.unit) due \(.delivery_date)"' \
    listScheduleLines -p "id=$AGREEMENT"
  expect "the schedule after the call-off" "$(field '[.items[] | "\(.line_no) \(.status) \(.purchase_order_id // "-")"] | join(", ")')" "1 called_off $po, 2 scheduled -"
  QUIET=1 api 200 '' listScheduleLines -p "id=$AGREEMENT" -p status=scheduled
  expect "the lines still to call off" "$(field '[.items[].line_no] | join(" ")')" "2"
  passed
}
# step_revenue_schedule is T81c (ADR-0068 §8): Nora reads the recognition
# schedule of step 67's active contract. Nothing of it is stored: it is
# computed from the obligations as the contract fixed them and from the
# order's delivery confirmations. Before a delivery the brackets, a
# point-in-time obligation, are in no period; the support, over time, is
# spread on a straight line over the days of its service period, the
# fiscal year, each fiscal period taking its days' share and the shares
# adding up to the allocated amount to the cent. Sally confirms the
# brackets as delivered today, and the schedule has their allocated
# amount in today's fiscal period, recognized. The amounts follow from
# the year's days, so the step holds in any year and on any day.
step_revenue_schedule() {
  heading "Step 70 — Recognition schedule: an active revenue contract's revenue per obligation and fiscal period, computed on read; goods in the period of their delivery confirmation, a service on a straight line by days over its service period, the shares adding up to the allocated amount"
  local order=$REVENUE_ORDER contract=$REVENUE_CONTRACT day year yeardays show
  [ -n "$order" ] && [ -n "$contract" ] || fail "step 67's order and revenue contract are not there"
  day=$(company_day)
  year=${day:0:4}
  yeardays=$(jq -rn --arg y "$year" '($y | tonumber) as $n | if ($n % 4 == 0 and $n % 100 != 0) or $n % 400 == 0 then 366 else 365 end')
  show='"recognition schedule of sales order \(.sales_order_number), \(.customer_number) \(.customer_name), as of \(.as_of): transaction price \(.transaction_price.amount) = recognized \(.recognized_amount.amount) + planned \(.planned_amount.amount) + not yet scheduled \(.unscheduled_amount.amount) \(.currency)", (.obligations[] | "  obligation \(.obligation_no) \(.description), \(.timing): allocated \(.allocated_amount.amount) = recognized \(.recognized_amount.amount) + planned \(.planned_amount.amount) + not yet scheduled \(.unscheduled_amount.amount)", (.periods[] | "    \(.fiscal_year)/\(.fiscal_period) \(.period_start) to \(.period_end): \(.amount.amount)" + (if .days then " for \(.days) days" else "" end) + (if .delivered_quantity then " for \(.delivered_quantity.value) \(.delivered_quantity.unit) delivered" else "" end) + ", recognized \(.recognized_amount.amount), planned \(.planned_amount.amount)"))'
  as nora
  say "Step 67's contract is active: 2700.00 allocated as 1446.43 to the brackets, satisfied at a point in time, and"
  say "1253.57 to the year of support, satisfied over time. Nora reads its recognition schedule (ADR-0068 §8). It is"
  say "computed at the read from the obligations and the order's delivery confirmations; nothing of it is stored and"
  say "nothing is posted. Nothing is delivered yet, so the brackets' amount is in no period: not yet scheduled. The"
  say "support is spread on a straight line over the $yeardays days of $year: each fiscal period takes 1253.57 x its days /"
  say "$yeardays, rounded so that the periods add up to 1253.57 exactly. What falls on days up to today, $day, is"
  say "recognized by the schedule; what falls later is planned:"
  api 200 "$show" getRevenueSchedule -p "id=$contract"
  expect "the schedule's contract, order and day" "$(field '"\(.revenue_contract_id) \(.sales_order_id) \(.as_of) \(.currency)"')" "$contract $order $day CAD"
  expect "the brackets before a delivery: no period, all not yet scheduled" \
    "$(field '.obligations[0] | "\(.timing) \(.periods | length) \(.recognized_amount.amount) \(.planned_amount.amount) \(.unscheduled_amount.amount)"')" \
    "point_in_time 0 0.00 0.00 1446.43"
  expect "the support's periods, the fiscal year's" "$(field '.obligations[1] | "\(.timing) \(.service_from) \(.service_to) \([.periods[].fiscal_period] | join(" "))"')" \
    "over_time $year-01-01 $year-12-31 1 2 3 4 5 6 7 8 9 10 11 12"
  expect "the periods' days, the year's" "$(field '[.obligations[1].periods[].days] | add')" "$yeardays"
  expect "the periods' amounts in cents, the allocated amount's" "$(field '[.obligations[1].periods[].amount.amount | sub("\\."; "") | tonumber] | add')" 125357
  expect "each period within a cent of 1253.57 x its days / $yeardays" \
    "$(field "[.obligations[1].periods[] | ((.amount.amount | sub(\"\\\\.\"; \"\") | tonumber) - 125357 * .days / $yeardays) | fabs < 1] | all")" true
  expect "every period before today's recognized, every later one planned" \
    "$(field "[.obligations[1].periods[] | if .period_end < \"$day\" then .recognized_amount.amount == .amount.amount elif .period_start > \"$day\" then .planned_amount.amount == .amount.amount else true end] | all")" true
  expect "the support: recognized and planned add up to its allocated amount, in cents" \
    "$(field '.obligations[1] | [.recognized_amount.amount, .planned_amount.amount, .unscheduled_amount.amount] | map(sub("\\."; "") | tonumber) | add')" 125357
  expect "the contract: recognized, planned and not yet scheduled add up to the transaction price, in cents" \
    "$(field '[.recognized_amount.amount, .planned_amount.amount, .unscheduled_amount.amount] | map(sub("\\."; "") | tonumber) | add')" 270000
  as sally
  say "The brackets leave the warehouse today. Sally confirms the delivery of all 120 (T78c):"
  api 201 '"delivery \(.delivery_no) of sales order \(.sales_order_number) on \(.delivery_date): " + ([.items[] | "item \(.item_no) \(.quantity.value) \(.quantity.unit)"] | join(", "))' \
    confirmSalesDelivery -p "id=$order" -body "{\"delivery_date\": \"$day\", \"items\": [{\"item_no\": 1, \"quantity\": {\"value\": \"120\", \"unit\": \"EA\"}}]}"
  as nora
  say "The delivery confirmation is when a point-in-time obligation is satisfied, unless Nora names another day on the"
  say "obligation: the schedule now has the brackets' whole allocated amount, 1446.43, in today's fiscal period,"
  say "recognized. The support's line is as it was."
  api 200 "$show" getRevenueSchedule -p "id=$contract"
  expect "the brackets: their allocated amount in the delivery's period, recognized" \
    "$(field ".obligations[0] | \"\\(.periods | length) \\(.periods[0].period_start <= \"$day\" and \"$day\" <= .periods[0].period_end) \\(.periods[0].amount.amount) \\(.periods[0].recognized_amount.amount) \\(.periods[0].delivered_quantity.value) \\(.periods[0].delivered_quantity.unit) \\(.unscheduled_amount.amount)\"")" \
    "1 true 1446.43 1446.43 120 EA 0.00"
  expect "nothing left unscheduled; the sums still the transaction price's, in cents" \
    "$(field '"\(.unscheduled_amount.amount) \([.recognized_amount.amount, .planned_amount.amount] | map(sub("\\."; "") | tonumber) | add)"')" "0.00 270000"
  expect "the contract's periods, the obligations' added up: today's holds the brackets and the support's share" \
    "$(field "[.periods[] | select(.period_start <= \"$day\" and \"$day\" <= .period_end) | .amount.amount | sub(\"\\\\.\"; \"\") | tonumber] | add")" \
    "$(field "[.obligations[].periods[] | select(.period_start <= \"$day\" and \"$day\" <= .period_end) | .amount.amount | sub(\"\\\\.\"; \"\") | tonumber] | add")"
  say "A draft has no allocated amounts and so no schedule:"
  QUIET=1 api 200 '' listRevenueContracts -p "legal_entity_id=$LE" -p status=draft
  [ "$(field '.items | length')" -gt 0 ] || fail "no draft revenue contract to show"
  refused 409 revenue.invalid_status getRevenueSchedule -p "id=$(field '.items[0].id')"
  passed
}
# step_close is the financial close (ADR-0058), the story's last step: it
# stays last, and each slice of T64 adds its part before the sign-off
# (ADR-0058, Steps). T64a: Ada proposes the close template, a tier 2 change
# the process owner approves; Carla reads the year's close calendar, each
# period's planned first and last due dates on the company's business days.
# Then the close's checklist (T64b), its depreciation check (T64k) with a
# late asset in the previous period's close (T64l), its other checks
# (T64c), its accrual proposals (T64e), its variances (T64h) and their
# comments (T64i) and, last, its sign-off (T64d), which locks the period.
step_close() {
  heading "Final step — Financial close: the close template through a governed change, the year's close calendar, the checklist and its sign-off, and the group's consolidated figures finalized"
  as ada
  say "Ada proposes the close template (ADR-0058 §3): the tasks every company's period close is given, each due"
  say "a number of business days after the period's last day (negative: before it), some waiting for others:"
  say "manual tasks a person completes, and checks the system runs (no entry of the period left parked, the"
  say "period's depreciation posted in every ledger, its variances explained, the close agent's accrual proposals"
  say "decided). The"
  say "template decides what a sign-off requires, so it is configuration, approved as a tier 2 change."
  api 200 '"active configuration version \(.items[0].number)"' listConfigVersions -p is_active=true
  local base cr request
  base=$(field '.items[0].number')
  jq "{title: \"Close template\", rationale: \"The monthly close checklist (T64a)\", base_version: $base,
    proposed: (.items[0].document | .close.tasks = [
      {\"code\": \"CUTOFF_NOTICE\", \"name\": \"Cut-off reminder sent to the team\", \"kind\": \"manual\", \"due_day\": -2},
      {\"code\": \"AP_CUTOFF\", \"name\": \"Supplier invoices of the period entered\", \"kind\": \"manual\", \"due_day\": 1, \"depends_on\": [\"CUTOFF_NOTICE\"]},
      {\"code\": \"TB_REVIEW\", \"name\": \"Trial balance reviewed\", \"kind\": \"manual\", \"due_day\": 3, \"depends_on\": [\"AP_CUTOFF\"]},
      {\"code\": \"NO_PARKED\", \"name\": \"No entry of the period left parked\", \"kind\": \"check\", \"check\": \"no_parked_entries\", \"due_day\": 3, \"depends_on\": [\"AP_CUTOFF\"]},
      {\"code\": \"DEPRECIATED\", \"name\": \"Depreciation of the period posted in every ledger\", \"kind\": \"check\", \"check\": \"depreciation_posted\", \"due_day\": 2},
      {\"code\": \"VARIANCES\", \"name\": \"Variances explained\", \"kind\": \"check\", \"check\": \"variances_explained\", \"due_day\": 4, \"depends_on\": [\"TB_REVIEW\"]},
      {\"code\": \"ACCRUALS\", \"name\": \"Accrual proposals decided\", \"kind\": \"check\", \"check\": \"accruals_decided\", \"due_day\": 3}])}" \
    "$WORK/last.json" >"$WORK/close.json"
  api 201 '"change request \(.number), \(.status): \(.title)"' createChangeRequest -body @close.json
  cr=$(field .id)
  api 200 '"\(.status), tier \(.tier): \([.items[].path] | join(", "))"' submitChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "the change's tier" "$(field .tier)" 2
  expect "the tasks among the changes" "$(field '[.items[] | select(.path | startswith("close.tasks["))] | length')" 7
  expect "status after submit" "$(field .status)" pending_approval
  request=$(field .workflow_request_id)
  approve priya "$request" "The monthly close checklist, as agreed with the controller"
  as ada
  QUIET=1 api 200 '' getChangeRequest -p "id=$cr"
  api 200 '"\(.status): configuration version \(.activated_version)"' activateChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "status after activation" "$(field .status)" active
  QUIET=1 api 200 '' listConfigVersions -p is_active=true
  local holidays
  holidays=$(field "[.items[0].document.calendars[]? | select(.company == \"$CC\") | .holidays[]?.date]")
  as carla
  say "Carla reads the close calendar of $CC's fiscal year $YEAR: each regular period of the leading ledger, its"
  say "status, and the first and last due dates of its close's tasks, counted in the company's business days"
  say "(neither its weekend nor one of its holidays):"
  api 200 '"\(.company_code) \(.company_name), \(.ledger_code) \(.ledger_name), weekend \(.weekend | join(" and ")):",
    (.periods[] | "  period \(.period) \(.start_date) to \(.end_date), \(.status): tasks due \(.first_due_on) to \(.last_due_on)")' \
    getCloseCalendar -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR"
  expect "the periods" "$(field '[.periods[].period] | map(tostring) | join(" ")')" "1 2 3 4 5 6 7 8 9 10 11 12"
  expect "the weekend" "$(field '.weekend | join(" ")')" "saturday sunday"
  expect "every due date a business day" \
    "$(field "[.periods[] | .first_due_on, .last_due_on] | all(. as \$d | (\$d | strptime(\"%Y-%m-%d\") | mktime | gmtime | .[6]) as \$w
      | \$w != 0 and \$w != 6 and ($holidays | index(\$d) | not))")" true
  expect "the cut-off reminder before each period's end, the review after it" \
    "$(field '[.periods[] | .first_due_on < .end_date and .last_due_on > .end_date] | all')" true
  close_checklist
  close_late_depreciation
  close_depreciation
  close_checks
  close_accruals
  close_variances
  close_comments
  close_variance_agent
  close_sign_off
  close_consolidation
  passed
}

# close_checklist is T64b's part of step_close (ADR-0058 §6): Carla opens
# the close of today's period, the template copied into it, and works its
# manual checklist to the end, every task done for the sign-off to come.
close_checklist() {
  local period close tag carla olga
  period=$((10#${TODAY:5:2}))
  carla=$(awk -F'\t' '$2 == "Carla Controller" { print $1 }' "$WORK/names")
  say "Carla opens the close of $CC's period $period of $YEAR, the period of the company's today ($TODAY): its checklist"
  say "is the template's tasks for the company, copied with their due dates, so a later change of the template"
  say "leaves this close as it is; it covers every ledger the company keeps."
  api 201 '"close of \(.company_code) \(.fiscal_year)/\(.fiscal_period), ending \(.period_end): \(.status), template of configuration version \(.config_version)",
    (.ledgers[] | "  covers \(.ledger_code) \(.ledger_name), its period \(.period_status)"),
    (.tasks[] | "  \(.code), due \(.due_on): \(.name)\(if .blocked_by == [] then "" else " (after " + (.blocked_by | join(", ")) + ")" end)")' \
    createPeriodClose -body "{\"legal_entity_id\": \"$LE\", \"fiscal_year\": $YEAR, \"fiscal_period\": $period}"
  close=$(field .id)
  tag=$ETAG
  expect "the close's tasks" "$(field '[.tasks[].code] | join(" ")')" "CUTOFF_NOTICE AP_CUTOFF TB_REVIEW NO_PARKED DEPRECIATED VARIANCES ACCRUALS"
  expect "the leading ledger first" "$(field '.ledgers[0].ledger_code')" 0L
  say "A second close of the period is refused, and names the first:"
  refused 409 closing.close_exists createPeriodClose -body "{\"legal_entity_id\": \"$LE\", \"fiscal_year\": $YEAR, \"fiscal_period\": $period}"
  say "A task goes only to a person who may act in $CC (product owner 2026-11-04): Olga, a new colleague not yet given"
  say "any company, is refused as the cut-off reminder's assignee."
  as ada
  QUIET=1 api 201 '' createPrincipal -body '{"kind": "human", "external_subject": "olga", "display_name": "Olga Onboarding", "all_companies": false}'
  olga=$(field .id)
  as carla
  refused 422 closing.assignee_invalid updateCloseTask -p "id=$close" -p code=CUTOFF_NOTICE -if-match "$tag" -body "{\"assignee_id\": \"$olga\"}"
  say "Carla takes the cut-off reminder herself. The supplier invoices wait for it: completing them first is refused."
  api 200 '"  \(.code) assigned to \(.assignee_name); the close is now at version \(.close_version)"' \
    updateCloseTask -p "id=$close" -p code=CUTOFF_NOTICE -if-match "$tag" -body "{\"assignee_id\": \"$carla\"}"
  tag=$ETAG
  refused 409 closing.task_blocked completeCloseTask -p "id=$close" -p code=AP_CUTOFF -if-match "$tag" -body '{}'
  say "Her worklist, the tasks assigned to her across closes:"
  api 200 '(.items[] | "  \(.company_code) \(.fiscal_year)/\(.fiscal_period) \(.code), due \(.due_on): \(.status)\(if .overdue then ", overdue" else "" end)")' \
    listCloseTasks -p assignee=me
  expect "her tasks" "$(field '[.items[].code] | join(" ")')" CUTOFF_NOTICE
  local code
  for code in CUTOFF_NOTICE AP_CUTOFF TB_REVIEW; do
    api 200 '"  \(.code) \(.status) by \(.completed_by_name)\(if .note then ": " + .note else "" end)"' \
      completeCloseTask -p "id=$close" -p code=$code -if-match "$tag" -body '{"note": "Done for the period"}'
    tag=$ETAG
  done
  say "A late supplier invoice of the period turns up: Carla reopens the invoices task with the reason, which the"
  say "audit trail keeps, and completes it again once the invoice is entered. The review, which depends on it, stays done."
  api 200 '"  \(.code) \(.status) again"' reopenCloseTask -p "id=$close" -p code=AP_CUTOFF -if-match "$tag" \
    -body '{"reason": "A late supplier invoice of the period arrived"}'
  tag=$ETAG
  QUIET=1 api 200 '' getPeriodClose -p "id=$close"
  expect "the review after the reopening" "$(field '.tasks[] | select(.code == "TB_REVIEW") | .status')" done
  QUIET=1 api 200 '' completeCloseTask -p "id=$close" -p code=AP_CUTOFF -if-match "$tag" -body '{"note": "The late invoice entered"}'
  api 200 '"\(.company_code) \(.fiscal_year)/\(.fiscal_period): \(.tasks_done) of \(.tasks_total) tasks done, \(.tasks_overdue) overdue",
    (.tasks[] | "  \(.code) \(.status)\(if .completed_by_name then " by " + .completed_by_name else "" end)")' getPeriodClose -p "id=$close"
  expect "every manual task done" "$(field '[.tasks[] | select(.kind == "manual") | .status == "done"] | all')" true
  say "The calendar's row of the period now shows its close:"
  api 200 ".periods[] | select(.period == $period) | \"  period \\(.period): close \\(.close.status), \\(.close.tasks_done) of \\(.close.tasks_total) done\"" \
    getCloseCalendar -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR"
  expect "the calendar's close" "$(field ".periods[] | select(.period == $period) | .close.id")" "$close"
}

# close_late_depreciation is T64l's part of step_close (ADR-0058 §6 as
# built, PO 2026-11-04): a machine contributed in kind is capitalised after
# step 29's run, valued in the previous period. Carla opens that period's
# close: a run of it is refused in the leading ledger, whose later run
# posted, so the check warns of the machine there, naming the catch-up the
# preview of this period shows for it; 2L, without a later run, still
# fails until Carla runs the previous period's depreciation there. The
# check then passes, the warning kept. Skipped in January, whose previous
# period is another fiscal year's.
close_late_depreciation() {
  local period prev from cc late close tag l2 run catch
  period=$((10#${TODAY:5:2}))
  prev=$((period - 1))
  if [ "$prev" -lt 1 ]; then
    say "(The previous period is another fiscal year's in January; the late asset's warning is shown from February.)"
    return
  fi
  from=$(printf '%s-%02d-01' "$YEAR" "$prev")
  as carla
  say "A second shareholder contribution in kind, a packaging machine in use since $from, reaches Carla only now,"
  say "after step 29 posted this period's depreciation in the leading ledger. She capitalises it with that value date:"
  accounts
  QUIET=1 api 200 '' listCostCenters -p "legal_entity_id=$LE" -p code=CC-1100
  cc=$(field '.items[0].id')
  api 201 '"asset \(.company_code)/\(.asset_number) \(.description): \(.status)"' createAsset \
    -body "{\"legal_entity_id\": \"$LE\", \"class_code\": \"MACHINES\", \"description\": \"Packaging machine\", \"cost_center_id\": \"$cc\"}"
  late=$(field .id)
  api 201 '"acquisition onto asset \(.asset_number), valued \(.value_date), posted \(.posting_date): \(.amount.amount) \(.amount.currency)"' \
    createAssetAcquisition -body "{\"asset_id\": \"$late\", \"offset_account_id\": \"$(account 3000)\", \"amount\": {\"amount\": \"2400.00\", \"currency\": \"CAD\"}, \"value_date\": \"$from\", \"text\": \"Packaging machine contributed in kind\"}"
  say "The previous period's depreciation can no longer be run in the leading ledger, whose run of $YEAR/$period posted:"
  refused 409 assets.run_out_of_order startDepreciationRun -body "{\"legal_entity_id\": \"$LE\", \"fiscal_year\": $YEAR, \"period\": $prev}"
  say "What a run of $YEAR/$period would post for the machine now, its catch-up of both periods:"
  api 200 ".items[] | select(.asset_id == \"$late\") | \"  \\(.asset_number) \\(.description): planned \\(.planned.amount), booked \\(.booked.amount), difference \\(.difference.amount) \\(.difference.currency)\"" \
    previewDepreciationRun -p page_size=100 -body "{\"legal_entity_id\": \"$LE\", \"fiscal_year\": $YEAR, \"period\": $period}"
  catch=$(field ".items[] | select(.asset_id == \"$late\") | .difference.amount")
  say "Carla opens the close of $YEAR/$prev, still open, and runs its depreciation check:"
  api 201 '"close of \(.company_code) \(.fiscal_year)/\(.fiscal_period): \(.status)"' \
    createPeriodClose -body "{\"legal_entity_id\": \"$LE\", \"fiscal_year\": $YEAR, \"fiscal_period\": $prev}"
  close=$(field .id)
  tag=$ETAG
  local result='"  \(.code) \(.status): \(.check_result.summary)",
    (.check_result.ledgers[] | "    \(.ledger_code): \(if .posted then "posted run" else "no posted run, \(.assets_to_depreciate) assets a run would select" end)\(if .later_depreciation_run_id then ", the run of \(.later_depreciation_fiscal_year)/\(.later_depreciation_period) posted" else "" end)"),
    "    \(.check_result.warnings_count) warnings:", (.check_result.warnings[] | "    \(.code): \(.message)")'
  api 200 "$result" checkCloseTask -p "id=$close" -p code=DEPRECIATED -if-match "$tag"
  tag=$ETAG
  expect "the check without a later run in 2L" "$(field '.check_result.passed')" false
  expect "the ledgers failing" "$(field '[.check_result.ledgers[] | select((.posted | not) and .assets_to_depreciate > 0 and .later_depreciation_run_id == null) | .ledger_code] | join(" ")')" 2L
  expect "the machine's warning in 0L" "$(field ".check_result.warnings[] | select(.asset_id == \"$late\") | \"\\(.code) \\(.ledger_code) \\(.missing_from_year)/\\(.missing_from_period) \\(.missing_through_year)/\\(.missing_through_period)\"")" \
    "depreciation_catch_up 0L $YEAR/$prev $YEAR/$period"
  expect "the warning's catch-up, the preview's difference" "$(field ".check_result.warnings[] | select(.asset_id == \"$late\") | .catch_up.amount")" "$catch"
  expect "0L's later run's period" "$(field '.check_result.ledgers[] | select(.ledger_code == "0L") | "\(.later_depreciation_fiscal_year)/\(.later_depreciation_period)"')" "$YEAR/$period"
  l2=$(field '.check_result.ledgers[] | select(.ledger_code == "2L") | .ledger_id')
  say "Carla runs $YEAR/$prev's depreciation in 2L, where no later run has posted, and checks again: it passes, and"
  say "the warning stays, for the next run of the leading ledger to catch the machine up."
  api 202 '"\(.kind) \(.status): \(.resource)"' startDepreciationRun \
    -body "{\"legal_entity_id\": \"$LE\", \"ledger_id\": \"$l2\", \"fiscal_year\": $YEAR, \"period\": $prev}"
  run=$(field .resource)
  run=${run##*/}
  wait_op
  QUIET=1 api 200 '' getDepreciationRun -p "id=$run"
  expect "2L's run of $YEAR/$prev" "$(field .status)" posted
  api 200 "$result" checkCloseTask -p "id=$close" -p code=DEPRECIATED -if-match "$tag"
  expect "the check with the warning" "$(field '"\(.check_result.passed) \(.status)"')" "true done"
  expect "the warning kept" "$(field "[.check_result.warnings[] | select(.asset_id == \"$late\") | .ledger_code] | join(\" \")")" 0L
}

# close_depreciation is T64k's part of step_close (ADR-0058 §6): the
# depreciation check names each covered ledger without a posted run of the
# period. Step 29 ran the period's depreciation in the leading ledger
# alone, so the check fails naming 2L, where the lathe still waits for
# its depreciation; Carla runs the period's depreciation in 2L and the
# check passes, naming each ledger's run.
close_depreciation() {
  local period close tag l2 run
  period=$((10#${TODAY:5:2}))
  as carla
  QUIET=1 api 200 '' listPeriodCloses -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR"
  close=$(field ".items[] | select(.fiscal_period == $period) | .id")
  QUIET=1 api 200 '' getPeriodClose -p "id=$close"
  tag=$ETAG
  say "DEPRECIATED checks that each ledger the close covers has a posted depreciation run of the period, or no asset"
  say "a run would select. Step 29 posted the period's run in the leading ledger alone; Carla runs the check:"
  api 200 '"  \(.code) \(.status): \(.check_result.summary)",
    (.check_result.ledgers[] | "    \(.ledger_code) \(.ledger_name): \(if .posted then "posted run \(.depreciation_run_id[0:8])…" else "no posted run, \(.assets_to_depreciate) assets a run would select" end)")' \
    checkCloseTask -p "id=$close" -p code=DEPRECIATED -if-match "$tag"
  tag=$ETAG
  expect "the check's verdict before 2L's run" "$(field '.check_result.passed')" false
  expect "the ledgers without a posted run" "$(field '[.check_result.ledgers[] | select(.posted | not) | .ledger_code] | join(" ")')" 2L
  expect "2L's assets to depreciate" "$(field '.check_result.ledgers[] | select(.ledger_code == "2L") | .assets_to_depreciate > 0')" true
  expect "the task after a failing check" "$(field .status)" open
  l2=$(field '.check_result.ledgers[] | select(.ledger_code == "2L") | .ledger_id')
  say "Carla runs the period's depreciation in 2L:"
  api 202 '"\(.kind) \(.status): \(.resource)"' startDepreciationRun \
    -body "{\"legal_entity_id\": \"$LE\", \"ledger_id\": \"$l2\", \"fiscal_year\": $YEAR, \"period\": $period}"
  run=$(field .resource)
  run=${run##*/}
  wait_op
  api 200 '"  run \(.fiscal_year)/\(.period) in \(.ledger_code): \(.status), \(.assets_posted) assets posted, \(.amount.amount) \(.amount.currency)"' getDepreciationRun -p "id=$run"
  expect "2L's run" "$(field .status)" posted
  say "and runs the check again: it passes, naming each ledger's run, and the task is done."
  api 200 '"  \(.code) \(.status) by \(.completed_by_name): \(.check_result.summary)",
    (.check_result.ledgers[] | "    \(.ledger_code): posted run \(.depreciation_run_id[0:8])…")' \
    checkCloseTask -p "id=$close" -p code=DEPRECIATED -if-match "$tag"
  expect "the check's verdict after 2L's run" "$(field '.check_result.passed')" true
  expect "2L's run named" "$(field '.check_result.ledgers[] | select(.ledger_code == "2L") | .depreciation_run_id')" "$run"
  expect "the task after a passing check" "$(field .status)" done
}

# close_checks is T64c's part of step_close (ADR-0058 §6): the close's
# check task runs on demand and records what it found. Tom parks an entry
# dated in the period; Carla's check fails naming it, Tom cancels it, and
# the check passes, which completes the task: every task is done for the
# sign-off to come.
close_checks() {
  local period close tag entry
  period=$((10#${TODAY:5:2}))
  as carla
  QUIET=1 api 200 '' listPeriodCloses -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR"
  close=$(field ".items[] | select(.fiscal_period == $period) | .id")
  QUIET=1 api 200 '' getPeriodClose -p "id=$close"
  tag=$ETAG
  say "NO_PARKED is a check: Carla does not complete it by hand, the system runs it, and a manual task has no check."
  refused 409 closing.task_is_check completeCloseTask -p "id=$close" -p code=NO_PARKED -if-match "$tag" -body '{}'
  refused 409 closing.task_not_check checkCloseTask -p "id=$close" -p code=TB_REVIEW -if-match "$tag"
  as tom
  say "Tom parks an accrual dated today, in the period, meaning to post it once the invoice arrives:"
  api 201 '"parked entry \(.id[0:8])…: \(.status), \(.header_text)"' parkJournalEntry \
    -body "{\"legal_entity_id\": \"$LE\", \"document_date\": \"$TODAY\", \"posting_date\": \"$TODAY\", \"currency\": \"CAD\", \"header_text\": \"Accrual for the courier, invoice to come\", \"lines\": [{\"account_id\": \"$(account 6000)\", \"amount\": {\"amount\": \"120.00\", \"currency\": \"CAD\"}}, {\"account_id\": \"$(account 211000)\", \"amount\": {\"amount\": \"-120.00\", \"currency\": \"CAD\"}, \"partner_id\": \"$SUPPLIER\"}]}"
  entry=$(field .id)
  local entry_tag=$ETAG
  as carla
  say "Carla runs the check. It fails, naming what is left parked in the period and who parked it, and the task"
  say "stays open; the result is kept on the task with when and by whom it ran."
  api 200 '"  \(.code) \(.status): \(.check_result.summary)", (.check_result.parked_entries[] | "    \(.posting_date) \(.header_text), \(.status), parked by \(.created_by_name) (\(.created_by_kind))")' \
    checkCloseTask -p "id=$close" -p code=NO_PARKED -if-match "$tag"
  tag=$ETAG
  expect "the check's verdict" "$(field '.check_result.passed')" false
  expect "the entry it names" "$(field "[.check_result.parked_entries[].id] | index(\"$entry\") != null")" true
  expect "the task after a failing check" "$(field .status)" open
  as tom
  say "The courier's invoice will be entered as an invoice; Tom cancels his accrual:"
  api 200 '"  parked entry: \(.status)"' cancelParkedJournalEntry -p "id=$entry" -if-match "$entry_tag" -body '{"reason": "The invoice will be entered instead"}'
  as carla
  say "Carla runs the check again: it passes, and the task is done, by her, when it passed."
  api 200 '"  \(.code) \(.status) by \(.completed_by_name): \(.check_result.summary)"' checkCloseTask -p "id=$close" -p code=NO_PARKED -if-match "$tag"
  expect "the check's verdict" "$(field '.check_result.passed')" true
  expect "the task after a passing check" "$(field .status)" done
  QUIET=1 api 200 '' getPeriodClose -p "id=$close"
  expect "every task done but the checks of the variances and the accruals, which come with their comments and proposals" \
    "$(field '[.tasks[] | select(.status != "done") | .code] | join(" ")')" "VARIANCES ACCRUALS"
}

step_cash_snapshot() {
  heading "Step 26 — Snapshots of the liquidity forecast: one taken on demand and read again with its lines; the daily ones"
  as tom
  local snap forecast_total forecast_cents lines_total
  say "Before the board meeting Tom keeps Maple's forecast as it is now: a snapshot, every source and certainty, kept per"
  say "document and liquidity item for the next 31 days and summed per bucket after, in each flow's own currency (T53f)."
  QUIET=1 api 200 '' getLiquidityForecast -p "legal_entity_id=$LE"
  forecast_total=$(field '[.currencies[] | select(.currency == "CAD") | .total.amount][0]')
  forecast_cents=$(field 'def c: sub("\\."; "") | tonumber; [.currencies[] | select(.currency == "CAD") | .total.amount | c][0]')
  api 201 '"snapshot \(.kind) \"\(.name)\" of \(.snapshot_date) (\(.time_zone)), per document to \(.document_level_until), horizon \(.horizon_end), configuration v\(.config_version)",
      (.currencies[] | "  \(.currency): total \(.total.amount), blocked suppliers \(.blocked.amount), \(.line_count) lines")' \
    takeCashSnapshot -body "{\"legal_entity_id\": \"$LE\", \"name\": \"Board meeting\"}"
  snap=$(field .id)
  expect "the snapshot's CAD total, the forecast's" "$(field '[.currencies[] | select(.currency == "CAD") | .total.amount][0]')" "$forecast_total"
  say "Its CAD lines, the first ten: each document a flow pays under its liquidity item, then the buckets' sums:"
  api 200 '"  \(.count) lines, total \(.total.amount) \(.total.currency)", (.items[0:10][] | "  \(.value_date)\(if .bucket_end then "..\(.bucket_end)" else "" end) \(.source)/\(.certainty) \(.liquidity_item // "unclassified") \(.amount.amount)\(if .document then ": \(.document.kind) \(.document.number // "-")" else "" end)\(if .supplier then ", \(.supplier.name)" else "" end)")' \
    listCashSnapshotFlows -p "id=$snap" -p currency=CAD -p page_size=500
  lines_total=$(field 'def c: sub("\\."; "") | tonumber; [.items[] | select(.source != "payable_blocked") | .amount.amount | c] | add // 0')
  expect "the lines add up to the snapshot's total" "$lines_total" "$forecast_cents"
  expect "per document within 31 days, per bucket after" \
    "$(field '[.items[] | select(.bucket_end == null) | .flow_count] | all(. == 1)')" true
  say "The snapshots of Maple, newest first. The daily one is taken once a day after Maple's snapshot time (07:00 in"
  say "America/Toronto) by the TakeCashSnapshot workflow, here the server's in-process snapshotter; it shows up once"
  say "the walkthrough runs after that time:"
  api 200 '.items[] | "  \(.company_code) \(.snapshot_date) \(.kind) \(.name // "")"' listCashSnapshots -p "legal_entity_id=$LE"
  expect "the board meeting's snapshot listed" "$(field "[.items[].id] | index(\"$snap\") != null")" true
  as rita
  say "Rita holds the forecast's read alone: she reads the same lines without their documents or suppliers."
  api 200 '"  \(.count) lines; documents shown \([.items[] | select(.document)] | length) of \([.items[] | select(.has_document)] | length)"' \
    listCashSnapshotFlows -p "id=$snap" -p currency=CAD -p page_size=500
  expect "documents shown to Rita" "$(field '[.items[] | select(.document or .supplier)] | length')" 0
  as tom
  passed
}

# close_accruals is T64e's part of step_close (ADR-0058 §8, §12): Ada
# names the company's accrual account, a new liability, through a governed
# change, and sets up the close agent, a level-1 agent principal. Alex
# enters the courier's invoice of last month's deliveries; the agent, as
# it will (T64g), proposes this month's accrual citing it, parked as its
# entry dated the period's last day, and may not post it. Carla reads the
# proposal with its evidence and posts its parked entry; a person's
# proposal is refused.
close_accruals() {
  local period close end base cr request role agent session last invoice body proposal entry
  period=$((10#${TODAY:5:2}))
  as ada
  say "Ada names the account $CC's accruals are credited to (ADR-0058 §3): a new liability, 2195 Accrued"
  say "liabilities, posted by hand (group account G2000, as $CC is in group MAPLE). The accrual account decides what"
  say "the accrual proposals' entries post (tier 3), and a new account changes the chart, so the change is tier 4: the"
  say "invoices of the last year are replayed under it."
  QUIET=1 api 200 '' listConfigVersions -p is_active=true
  base=$(field '.items[0].number')
  jq "{title: \"Accrual account 2195\", rationale: \"The close agent's accrual proposals (T64e)\", base_version: $base,
    proposed: (.items[0].document | .companies |= map(if .code == \"$CC\" then .chart_of_accounts += [{number: \"2195\", name: \"Accrued liabilities\",
      type: \"liability\"} + if any(.chart_of_accounts[]; .group_account) then {group_account: \"G2000\"} else {} end] | .close.accrual_account = \"2195\" else . end))}" "$WORK/last.json" >"$WORK/accrual.json"
  api 201 '"change request \(.number), \(.status): \(.title)"' createChangeRequest -body @accrual.json
  cr=$(field .id)
  api 200 '"\(.status), tier \(.tier): \([.items[] | "\(.path) (tier \(.tier))"] | join(", "))"' submitChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "the change's tier" "$(field .tier)" 4
  expect "the accrual account's tier" "$(field "[.items[] | select(.path == \"companies[$CC].close.accrual_account\") | .tier] | join(\" \")")" 3
  QUIET=1 api 201 '' createRegressionSuite -body '{"name": "Before the accrual account", "window_days": 366}'
  QUIET=1 api 200 '' getChangeRequest -p "id=$cr"
  api 202 '"\(.kind) \(.status)"' simulateChangeRequest -p "id=$cr" -if-match "$ETAG"
  wait_op
  api 200 '.evidence[] | "evidence \(.kind): \(if .passed then "passed" else "FAILED" end), \(.summary)"' getChangeRequest -p "id=$cr"
  expect "status after the evidence" "$(field .status)" pending_approval
  request=$(field .workflow_request_id)
  approve sam "$request" "A new liability for accruals, unposted"
  approve carla "$request" "The account the close agent's accruals are credited to"
  as ada
  QUIET=1 api 200 '' getChangeRequest -p "id=$cr"
  api 200 '"\(.status): configuration version \(.activated_version)"' activateChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "status after activation" "$(field .status)" active
  say "Ada sets up the close agent (ADR-0058 §12): an agent principal at autonomy level 1, which proposes and decides"
  say "nothing; its role reads closes and the ledger and proposes accruals in its sessions, parking their entries."
  api 201 '"role \(.code), \(.name)"' createRole -body '{"code": "close_agent", "name": "Close agent"}'
  role=$(field .id)
  printf '%s' "$role" >"$WORK/close_agent_role"
  grant "$role" closing.close.read closing.accrual.propose ledger.entry.park ledger.entry.read agents.session.record \
    agents.session.read agents.model.invoke ledger.account.read payables.invoice.read procurement.order.read partner.supplier.read
  api 201 '"\(.kind) \(.display_name) of type \(.agent_profile.agent_type), level \(.agent_profile.autonomy_level) on \(.agent_profile.model)"' createPrincipal \
    -body '{"kind": "agent", "external_subject": "close-agent", "display_name": "Close agent", "all_companies": true, "agent_profile": {"autonomy_level": 1, "agent_type": "close", "model": "erp-close", "description": "Proposes accruals and variance comments for people to decide (ADR-0058)"}}'
  agent=$(field .id)
  QUIET=1 api 201 '' createRoleAssignment -body "{\"principal_id\": \"$agent\", \"role_id\": \"$role\"}"
  token close-agent "Close agent"
  printf '%s\tClose agent\n' "$agent" >>"$WORK/names"
  last=$(date_add "${TODAY:0:8}01" -1)
  as alex
  say "Tyrell invoices its courier deliveries monthly. Alex enters last month's invoice, dated $last, CAD 300.00"
  say "plus 15.00 GST, and posts it:"
  api 201 '"\(.kind) \(.document_number) \(.supplier_reference) of \(.invoice_date): \(.status), \(.gross_amount.amount) \(.gross_amount.currency)"' \
    createSupplierInvoice -body "{\"legal_entity_id\": \"$LE\", \"supplier_id\": \"$SUPPLIER\", \"supplier_reference\": \"COURIER-${last:0:7}\", \"invoice_date\": \"$last\", \"gross_amount\": {\"amount\": \"315.00\", \"currency\": \"CAD\"}, \"tax_amount\": {\"amount\": \"15.00\", \"currency\": \"CAD\"}, \"items\": [{\"account_id\": \"$(account 6000)\", \"description\": \"Courier deliveries of ${last:0:7}\", \"quantity\": {\"value\": \"1\", \"unit\": \"EA\"}, \"amount\": {\"amount\": \"300.00\", \"currency\": \"CAD\"}, \"tax_code\": \"V1\"}]}"
  invoice=$(field .id)
  api 200 '"\(.document_number): \(.status), journal entry \(.journal_entry_id)"' postSupplierInvoice -p "id=$invoice" -if-match '"1"' -body "{\"posting_date\": \"$TODAY\"}"
  as carla
  QUIET=1 api 200 '' listPeriodCloses -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR"
  close=$(field ".items[] | select(.fiscal_period == $period) | .id")
  end=$(field ".items[] | select(.fiscal_period == $period) | .period_end")
  as close-agent
  say "The agent starts a session. This month's deliveries are not invoiced yet, so it proposes their accrual at"
  say "last month's amount, citing that invoice: the closing context parks the entry as the agent, Dr 6000 and Cr"
  say "the accrual account 2195, dated the period's last day, for a person to post."
  api 201 '"session \(.id[0:8])…: \(.status)"' startAgentSession -body '{"purpose": "Propose the accruals of the close", "model": "erp-close"}'
  session=$(field .id)
  body="{\"session_id\": \"$session\", \"close_id\": \"$close\", \"account_id\": \"$(account 6000)\", \"supplier_id\": \"$SUPPLIER\", \"amount\": {\"amount\": \"300.00\", \"currency\": \"CAD\"}, \"description\": \"Courier deliveries of ${TODAY:0:7}, not invoiced yet\", \"reason\": \"Tyrell invoices its deliveries monthly; last month's invoice was 300.00 and this month's has not arrived.\", \"evidence\": [{\"kind\": \"supplier_invoice\", \"document_id\": \"$invoice\"}]}"
  api 201 '"  proposal \(.id[0:8])…: \(.status), Dr \(.account_number) \(.account_name) Cr \(.accrual_account_number) \(.accrual_account_name) \(.amount.amount) \(.amount.currency) on \(.posting_date), parked entry \(.parked_entry_id[0:8])…"' \
    proposeAccrual -body "$body"
  proposal=$(field .id)
  entry=$(field .parked_entry_id)
  expect "the proposal" "$(field .status)" proposed
  expect "the accrual account" "$(field .accrual_account_number)" 2195
  say "Retried, the same proposal answers as it is, and parks nothing more:"
  api 200 '"  proposal \(.id[0:8])…: \(.status)"' proposeAccrual -body "$body"
  expect "the retried proposal" "$(field .id)" "$proposal"
  say "Proposing is all it may do: posting its entry is a person's."
  QUIET=1 api 200 '' getParkedJournalEntry -p "id=$entry"
  expect "who parked the entry" "$(field .created_by_kind)" agent
  refused 403 identity.agent_not_allowed postParkedJournalEntry -p "id=$entry" -if-match "$ETAG" -body '{}'
  QUIET=1 api 200 '' endAgentSession -p "id=$session" -if-match '*' -body '{"status": "completed", "input_tokens": 0, "output_tokens": 0}'
  as carla
  say "Carla reads the proposal with its evidence, each document named, and the close's proposals:"
  api 200 '"  \(.company_code) \(.fiscal_year)/\(.fiscal_period) by \(.agent_name): \(.description), \(.amount.amount) \(.amount.currency) for \(.supplier_name // .supplier_id)", "  because: \(.reason)",
    (.evidence[] | "  evidence \(.kind) \(.document_number) of \(.supplier_name // .supplier_id)")' getAccrualProposal -p "id=$proposal"
  expect "the evidence, last month's invoice" "$(field '[.evidence[] | .kind] | join(" ")')" supplier_invoice
  expect "the cited invoice" "$(field '.evidence[0].document_id')" "$invoice"
  api 200 '(.items[] | "  \(.id[0:8])… \(.account_number) \(.amount.amount) \(.amount.currency): \(.status)")' listAccrualProposals -p "close_id=$close"
  expect "the close's proposals" "$(field '[.items[].id] | join(" ")')" "$proposal"
  say "Its parked entry, the agent's, waits for a person. Carla reads it and posts it (postParkedJournalEntry):"
  api 200 '"  parked entry by \(.created_by_kind) on \(.posting_date): \(.header_text)", (.lines[] | "    \(.amount.amount) \(.amount.currency) on \(.account_id[0:8])…: \(.line_text)")' \
    getParkedJournalEntry -p "id=$entry"
  expect "the parked entry's date, the period's last day" "$(field .posting_date)" "$end"
  QUIET=1 api 200 '' getParkedJournalEntry -p "id=$entry"
  api 200 '"  parked entry: \(.status), journal entry \(.journal_entry_id)"' postParkedJournalEntry -p "id=$entry" -if-match "$ETAG" -body '{}'
  expect "the accrual's entry" "$(field .status)" posted
  local journal
  journal=$(field .journal_entry_id)
  say "A person's proposal is refused: a person parks an entry herself (parkJournalEntry)."
  refused 403 agents.not_an_agent proposeAccrual -body "$body"
  close_accrual_agent "$close"
  close_accrual_decisions "$close" "$proposal" "$journal" "$invoice"
}

# close_accrual_agent is T64g's part of step_close (ADR-0058 §12): the
# close agent itself, erp agent close-accruals on the close, through the
# model stand-in answering from a script of turns (its suite's replay,
# evals/close). Rideau IT Services invoiced 6100 in each of the three
# months before the close's and not in it; Laurentian Paper invoiced 6000
# in those months and in this one. The agent proposes Rideau's accrual of
# its last invoice's net amount citing the three, and nothing for
# Laurentian; Carla posts the proposal's parked entry, so the close stays
# ready to sign off.
close_accrual_agent() {
  local close=$1 m1 m2 m3 rideau laurentian fees office ids=() inv n day net tax gross proposal entry got session
  m1=$(date_add "${TODAY:0:8}01" -1)
  m2=$(date_add "${m1:0:8}01" -1)
  m3=$(date_add "${m2:0:8}01" -1)
  as alex
  QUIET=1 api 200 '' listBusinessPartners -p "name_prefix=Rideau IT"
  rideau=$(field '.items[0].id')
  QUIET=1 api 200 '' listBusinessPartners -p "name_prefix=Laurentian"
  laurentian=$(field '.items[0].id')
  if [ -z "$rideau" ] || [ "$rideau" = null ] || [ -z "$laurentian" ] || [ "$laurentian" = null ]; then
    fail "the migrated suppliers Rideau IT Services and Laurentian Paper are not there"
  fi
  fees=$(account 6100)
  office=$(account 6000)
  say "Rideau IT Services invoices its support monthly on 6100 Professional fees; its invoices of ${m3:0:7}, ${m2:0:7} and"
  say "${m1:0:7} arrive together and Alex posts them today, dated in their months. Laurentian Paper invoices its paper"
  say "monthly on 6000, this month's included. (Each net amount plus 5 % GST, V1.)"
  for n in "$m3 400.00 20.00 420.00" "$m2 420.00 21.00 441.00" "$m1 450.00 22.50 472.50"; do
    read -r day net tax gross <<<"$n"
    QUIET=1 api 201 '' createSupplierInvoice -body "{\"legal_entity_id\": \"$LE\", \"supplier_id\": \"$rideau\", \"supplier_reference\": \"RIT-${day:0:7}\", \"invoice_date\": \"$day\", \"gross_amount\": {\"amount\": \"$gross\", \"currency\": \"CAD\"}, \"tax_amount\": {\"amount\": \"$tax\", \"currency\": \"CAD\"}, \"items\": [{\"account_id\": \"$fees\", \"description\": \"IT support of ${day:0:7}\", \"quantity\": {\"value\": \"1\", \"unit\": \"EA\"}, \"amount\": {\"amount\": \"$net\", \"currency\": \"CAD\"}, \"tax_code\": \"V1\"}]}"
    inv=$(field .id)
    api 200 '"  \(.document_number) \(.supplier_reference) of \(.invoice_date): \(.status), \(.gross_amount.amount) \(.gross_amount.currency)"' \
      postSupplierInvoice -p "id=$inv" -if-match '"1"' -body "{\"posting_date\": \"$TODAY\"}"
    ids+=("$inv")
  done
  for n in "$m3 180.00 9.00 189.00" "$m2 190.00 9.50 199.50" "$m1 200.00 10.00 210.00" "$TODAY 210.00 10.50 220.50"; do
    read -r day net tax gross <<<"$n"
    QUIET=1 api 201 '' createSupplierInvoice -body "{\"legal_entity_id\": \"$LE\", \"supplier_id\": \"$laurentian\", \"supplier_reference\": \"LP-${day:0:7}\", \"invoice_date\": \"$day\", \"gross_amount\": {\"amount\": \"$gross\", \"currency\": \"CAD\"}, \"tax_amount\": {\"amount\": \"$tax\", \"currency\": \"CAD\"}, \"items\": [{\"account_id\": \"$office\", \"description\": \"Copy paper of ${day:0:7}\", \"quantity\": {\"value\": \"1\", \"unit\": \"EA\"}, \"amount\": {\"amount\": \"$net\", \"currency\": \"CAD\"}, \"tax_code\": \"V1\"}]}"
    inv=$(field .id)
    api 200 '"  \(.document_number) \(.supplier_reference) of \(.invoice_date): \(.status), \(.gross_amount.amount) \(.gross_amount.currency)"' \
      postSupplierInvoice -p "id=$inv" -if-match '"1"' -body "{\"posting_date\": \"$TODAY\"}"
  done
  say "The close agent runs: erp agent close-accruals starts a session on erp-close, reads through its tools what"
  say "the close's company invoiced, proposed and parked, and proposes. The model is the stand-in, answering from a"
  say "script of turns as the agent's evaluation suite replays them (evals/close): it reads the close, the invoices"
  say "of the last three months and this one, the close's proposals and the parked entries, and proposes Rideau's"
  say "accrual of ${m1:0:7}'s 450.00 citing the three invoices; Laurentian has invoiced this month, so nothing."
  jq -n --arg close "$close" --arg le "$LE" --arg from "${m3:0:8}01" --arg to "$TODAY" --arg acct "$fees" --arg sup "$rideau" \
    --arg month "${TODAY:0:7}" --arg i1 "${ids[0]}" --arg i2 "${ids[1]}" --arg i3 "${ids[2]}" '{
    vars: {session: "session id is ([0-9a-f-]{36})"},
    turns: [
      {tool: "getPeriodClose", input: {id: $close}},
      {tool: "listSupplierInvoices", input: {legal_entity_id: $le, invoice_date_from: $from, invoice_date_to: $to}},
      {tool: "listAccrualProposals", input: {close_id: $close}},
      {tool: "listParkedJournalEntries", input: {legal_entity_id: $le, status: "parked"}},
      {tool: "proposeAccrual", input: {body: {session_id: "{{session}}", close_id: $close, account_id: $acct, supplier_id: $sup,
        amount: {amount: "450.00", currency: "CAD"}, description: "IT support of \($month), not invoiced yet",
        reason: "Rideau IT Services invoiced 400.00, 420.00 and 450.00 on 6100 in the last three months and not this one; the last, 450.00, is accrued.",
        evidence: [{kind: "supplier_invoice", document_id: $i1}, {kind: "supplier_invoice", document_id: $i2}, {kind: "supplier_invoice", document_id: $i3}]}}},
      {final: "Proposed Rideau IT Services accrual of 450.00. Laurentian Paper has invoiced this month, so nothing is accrued for it."}]}' \
    >"$WORK/model-script.json"
  show ERP_AGENT_TOKEN=… erp agent close-accruals -close "$close"
  got=$(ERP_AGENT_TOKEN=$(cat "$WORK/tokens/close-agent") "$ERP" agent close-accruals -close "$close" 2>"$WORK/stderr") ||
    fail "erp agent close-accruals failed ($(cat "$WORK/stderr"))"
  rm -f "$WORK/model-script.json"
  printf '%s\n' "$got" | sed 's/^/     /'
  session=$(printf '%s\n' "$got" | sed -nE '1s/^Session ([0-9a-f-]{36}): .*/\1/p')
  expect "what the agent made" "$(printf '%s\n' "$got" | sed -nE '1s/^Session [0-9a-f-]{36}: //p')" "proposals made 1, refused 0."
  proposal=$(printf '%s\n' "$got" | sed -nE 's/^  proposal ([0-9a-f-]{36}), parked entry ([0-9a-f-]{36})$/\1/p')
  entry=$(printf '%s\n' "$got" | sed -nE 's/^  proposal ([0-9a-f-]{36}), parked entry ([0-9a-f-]{36})$/\2/p')
  as carla
  say "Carla reads the close's proposals: the agent's, with its three invoices, and none for Laurentian:"
  api 200 '(.items[] | "  \(.id[0:8])… by \(.agent_name) for \(.supplier_name // .supplier_id): \(.account_number) \(.amount.amount) \(.amount.currency), evidence \([.evidence[].document_number] | join(", "))")' \
    listAccrualProposals -p "close_id=$close"
  expect "Rideau's proposals" "$(field "[.items[] | select(.supplier_id == \"$rideau\")] | length")" 1
  expect "Rideau's amount, its last invoice's net" "$(field ".items[] | select(.supplier_id == \"$rideau\") | .amount.amount")" 450.00
  expect "on 6100" "$(field ".items[] | select(.supplier_id == \"$rideau\") | .account_number")" 6100
  expect "citing the three invoices" "$(field ".items[] | select(.supplier_id == \"$rideau\") | [.evidence[].document_id] | sort | join(\" \")")" \
    "$(printf '%s\n' "${ids[@]}" | sort | paste -sd' ' -)"
  expect "nothing for Laurentian, already invoiced" "$(field "[.items[] | select(.supplier_id == \"$laurentian\")] | length")" 0
  expect "the proposal the agent printed, Rideau's" "$(field ".items[] | select(.supplier_id == \"$rideau\") | .id")" "$proposal"
  expect "its parked entry" "$(field ".items[] | select(.id == \"$proposal\") | .parked_entry_id")" "$entry"
  expect "made in the session it printed" "$(field ".items[] | select(.id == \"$proposal\") | .session_id")" "$session"
  as close-agent
  say "The agent's session, which the command named, records each tool call:"
  api 200 '"  session \(.id[0:8])… \(.status) on \(.model): \(.purpose)"' getAgentSession -p "id=$session"
  expect "the session" "$(field .status)" completed
  api 200 '"  its tool calls: \([.items[].tool] | join(", "))"' listAgentToolCalls -p "id=$session"
  expect "its tool calls" "$(field '[.items[].tool] | join(" ")')" "getPeriodClose listSupplierInvoices listAccrualProposals listParkedJournalEntries proposeAccrual"
  as carla
  say "Carla posts the parked entry of the agent's proposal ${proposal:0:8}…, as she posted the first:"
  QUIET=1 api 200 '' getParkedJournalEntry -p "id=$entry"
  api 200 '"  parked entry: \(.status), journal entry \(.journal_entry_id)"' postParkedJournalEntry -p "id=$entry" -if-match "$ETAG" -body '{}'
  expect "the agent's accrual" "$(field .status)" posted
}

# close_accrual_decisions is T64f's part of step_close (ADR-0058 §6, §8):
# the proposal Carla posted reads posted with its journal entry's number,
# derived from its parked entry; the agent proposes a second accrual, which
# Carla rejects with a reason, cancelling its parked entry; the close's
# proposals are summed per status in CAD; and the check that every
# proposal is decided passes.
close_accrual_decisions() {
  local close=$1 proposal=$2 journal=$3 invoice=$4 session second number tag
  as carla
  say "A proposal's status is derived from its parked entry (ADR-0058 §8): Carla posted it, so it reads posted, with"
  say "the journal entry's number, and as proposed:"
  QUIET=1 api 200 '' getJournalEntry -p "id=$journal"
  number=$(field .document_number)
  api 200 '"  proposal \(.id[0:8])…: \(.status), journal entry \(.journal_entry_number), \(.posted_amount.amount) \(.posted_amount.currency) posted\(if .modified then ", changed before posting" else " as proposed" end)"' \
    getAccrualProposal -p "id=$proposal"
  expect "the posted proposal" "$(field .status)" posted
  expect "its journal entry's number" "$(field .journal_entry_number)" "$number"
  expect "posted as proposed" "$(field .modified)" false
  as close-agent
  say "The agent proposes a second accrual, of Tyrell's implementation services on 6100, citing the same invoice:"
  api 201 '"session \(.id[0:8])…: \(.status)"' startAgentSession -body '{"purpose": "Propose the accruals of the close", "model": "erp-default"}'
  session=$(field .id)
  api 201 '"  proposal \(.id[0:8])…: \(.status), Dr \(.account_number) \(.account_name) \(.amount.amount) \(.amount.currency)"' \
    proposeAccrual -body "{\"session_id\": \"$session\", \"close_id\": \"$close\", \"account_id\": \"$(account 6100)\", \"supplier_id\": \"$SUPPLIER\", \"amount\": {\"amount\": \"45.00\", \"currency\": \"CAD\"}, \"description\": \"Implementation services of ${TODAY:0:7}\", \"reason\": \"Tyrell billed implementation services before; more may be owed for this month.\", \"evidence\": [{\"kind\": \"supplier_invoice\", \"document_id\": \"$invoice\"}]}"
  second=$(field .id)
  tag=$ETAG
  QUIET=1 api 200 '' endAgentSession -p "id=$session" -if-match '*' -body '{"status": "completed", "input_tokens": 0, "output_tokens": 0}'
  as carla
  say "The implementation was invoiced in full: Carla rejects the proposal with her reason, which cancels its parked entry."
  api 200 '"  proposal \(.id[0:8])…: \(.status) by \(.decided_by_name): \(.decision_reason)"' \
    rejectAccrualProposal -p "id=$second" -if-match "$tag" -body '{"reason": "The implementation was invoiced in full; nothing more is owed."}'
  expect "the rejected proposal" "$(field .status)" rejected
  expect "the reason kept" "$(field .decision_reason)" "The implementation was invoiced in full; nothing more is owed."
  QUIET=1 api 200 '' getAccrualProposal -p "id=$second"
  QUIET=1 api 200 '' getParkedJournalEntry -p "id=$(field .parked_entry_id)"
  expect "its parked entry" "$(field .status)" cancelled
  refused 409 closing.accrual_not_open rejectAccrualProposal -p "id=$proposal" -if-match '"1"' -body '{"reason": "Posted already"}'
  say "The close's proposals, summed per status in CAD: how much of what the agent proposed was posted, as proposed"
  say "or changed, and how much rejected."
  api 200 '(.totals[] | "  \(.status): \(.count), \(.amount.amount) \(.amount.currency) proposed\(if .posted_amount then ", \(.posted_amount.amount) posted, \(.modified_count) changed" else "" end)")' \
    listAccrualProposals -p "close_id=$close"
  expect "the totals" "$(field '[.totals[] | "\(.status) \(.currency) \(.count) \(.amount.amount)"] | join(", ")')" "posted CAD 2 750.00, rejected CAD 1 45.00"
  QUIET=1 api 200 '' listAccrualProposals -p "close_id=$close" -p status=rejected
  expect "the rejected ones" "$(field '[.items[].id] | join(" ")')" "$second"
  say "ACCRUALS checks that no proposal of the close waits for a person; each is posted or rejected, so it passes:"
  QUIET=1 api 200 '' getPeriodClose -p "id=$close"
  api 200 '"  \(.code) \(.status) by \(.completed_by_name): \(.check_result.summary)"' checkCloseTask -p "id=$close" -p code=ACCRUALS -if-match "$ETAG"
  expect "the check's verdict" "$(field '.check_result.passed')" true
  expect "the task after a passing check" "$(field .status)" done
}

# close_variances is T64h's part of step_close (ADR-0058 §9): Ada sets the
# company's variance thresholds through a tier 2 change; Carla reads the
# period's variances against the prior period, the rows past the
# thresholds flagged, and its totals, whose actuals are the period's
# profit and loss in the trial balance; the plan, not configured, is
# refused.
close_variances() {
  local period close base cr request threshold=500 percent=10
  period=$((10#${TODAY:5:2}))
  as ada
  say "Ada proposes $CC's variance settings (ADR-0058 §3): its close compares each profit and loss account with the"
  say "prior period and the same period of the prior year, and a variance of at least $threshold.00 CAD and $percent %"
  say "needs a comment before sign-off. They post nothing, so the change is tier 2."
  QUIET=1 api 200 '' listConfigVersions -p is_active=true
  base=$(field '.items[0].number')
  jq "{title: \"Variance thresholds\", rationale: \"The close's variance report (T64h)\", base_version: $base,
    proposed: (.items[0].document | .companies |= map(if .code == \"$CC\" then .close.variance = {bases: [\"prior_period\", \"prior_year\"],
      threshold_amount: \"$threshold.00\", threshold_percent: \"$percent\"} else . end))}" "$WORK/last.json" >"$WORK/variance.json"
  api 201 '"change request \(.number), \(.status): \(.title)"' createChangeRequest -body @variance.json
  cr=$(field .id)
  api 200 '"\(.status), tier \(.tier): \([.items[].path] | join(", "))"' submitChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "the change's tier" "$(field .tier)" 2
  request=$(field .workflow_request_id)
  approve priya "$request" "The variance thresholds, as agreed with the controller"
  as ada
  QUIET=1 api 200 '' getChangeRequest -p "id=$cr"
  api 200 '"\(.status): configuration version \(.activated_version)"' activateChangeRequest -p "id=$cr" -if-match "$ETAG"
  as carla
  accounts
  QUIET=1 api 200 '' listPeriodCloses -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR"
  close=$(field ".items[] | select(.fiscal_period == $period) | .id")
  say "Carla reads the close's variances against the prior period: per profit and loss account of the leading ledger,"
  say "the period's net movement in CAD (debit positive), the prior period's, the variance and its percent, flagged"
  say "where it reaches the thresholds:"
  api 200 '"\(.company_code) \(.fiscal_year)/\(.fiscal_period) against \(.comparison_fiscal_year)/\(.comparison_fiscal_period), \(.ledger_code), \(.currency):",
    (.items[] | "  \(.account_number) \(.account_name): \(.actual.amount) against \(.comparison.amount), \(.variance.amount)\(if .variance_percent then " (" + .variance_percent + " %)" else "" end)\(if .requires_comment then ", needs a comment" else "" end)"),
    "  totals: \(.totals.actual.amount) against \(.totals.comparison.amount), \(.totals.variance.amount); \(.totals.rows) rows, \(.totals.rows_requiring_comment) needing a comment"' \
    listPeriodCloseVariances -p "id=$close" -p basis=prior_period -p page_size=500
  expect "a report with rows" "$(field '.totals.rows > 0')" true
  expect "every row flagged exactly when it reaches the thresholds" \
    "$(field "[.items[] | ((.variance.amount | tonumber) as \$v | (if \$v < 0 then -\$v else \$v end) >= $threshold
      and (.variance_percent == null or ((.variance_percent | tonumber) as \$p | (if \$p < 0 then -\$p else \$p end) >= $percent))) == .requires_comment] | all")" true
  expect "the flagged rows counted" "$(field '[.items[] | select(.requires_comment)] | length')" "$(field .totals.rows_requiring_comment)"
  local actual comparison flagged
  actual=$(field '.totals.actual.amount | tonumber * 100 | round')
  comparison=$(field '.totals.comparison.amount | tonumber * 100 | round')
  flagged=$(field .totals.rows_requiring_comment)
  expect "the variance the actual less the comparison" "$(field '.totals.variance.amount | tonumber * 100 | round')" "$((actual - comparison))"
  local pl
  pl=$(jq -c '[.items[] | select(.is_balance_sheet | not) | .id]' "$WORK/accounts.json")
  api 200 "\"  the trial balance's profit and loss of period $period: \\([.items[] | select(.account_id as \$a | $pl | index(\$a)) | (.debit_lc.amount | tonumber) - (.credit_lc.amount | tonumber)] | add // 0 | . * 100 | round / 100) CAD\"" \
    listBalances -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR" -p "fiscal_period=$period" -p page_size=500
  expect "the totals' actuals the period's profit and loss in the trial balance" \
    "$(field "[.items[] | select(.account_id as \$a | $pl | index(\$a)) | (.debit_lc.amount | tonumber) - (.credit_lc.amount | tonumber)] | add // 0 | . * 100 | round")" "$actual"
  say "The rows that need a comment alone:"
  api 200 '(.items[] | "  \(.account_number) \(.account_name): \(.variance.amount)")' \
    listPeriodCloseVariances -p "id=$close" -p basis=prior_period -p requires_comment=true -p page_size=500
  expect "the flagged rows" "$(field '.items | length')" "$flagged"
  expect "the totals still the whole report's" "$(field '.totals.actual.amount | tonumber * 100 | round')" "$actual"
  say "The plan is no basis $CC configures:"
  refused 422 closing.basis_not_configured listPeriodCloseVariances -p "id=$close" -p basis=plan
}

# close_comments is T64i's part of step_close (ADR-0058 §10): the close
# agent (set up in close_accruals), its role now proposing variance
# comments too, proposes a comment on a flagged row
# citing the entry behind it, as the agent will (T64j); a citation of an
# entry with no line on the row's account is refused. Carla accepts the
# proposal and explains the other flagged rows herself; the report shows
# each row's accepted comment, the unexplained filter and the rows
# explained, and the variances check passes.
close_comments() {
  local period close session row account entry other comment tag basis n
  period=$((10#${TODAY:5:2}))
  as ada
  say "Ada lets the close agent propose variance comments in its sessions too."
  grant "$(cat "$WORK/close_agent_role")" closing.variance.propose
  as carla
  QUIET=1 api 200 '' listPeriodCloses -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR"
  close=$(field ".items[] | select(.fiscal_period == $period) | .id")
  QUIET=1 api 200 '' listPeriodCloseVariances -p "id=$close" -p basis=prior_period -p requires_comment=true -p page_size=500
  row=$(field '.items[0].account_number // empty')
  [ -n "$row" ] || fail "no row of the prior period requires a comment"
  account=$(field '.items[0].account_id')
  as close-agent
  say "The agent starts a session and finds the entries behind the flagged row $row against the prior period:"
  api 201 '"session \(.id[0:8])…: \(.status)"' startAgentSession -body '{"purpose": "Explain the variances of the close", "model": "erp-default"}'
  session=$(field .id)
  api 200 '(.items[] | "  entry \(.document_number) of \(.posting_date): \(.header_text // "")")' \
    listJournalEntries -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR" -p "fiscal_period=$period" -p "account_id=$account" -p page_size=5
  entry=$(field '.items[0].id')
  QUIET=1 api 200 '' listJournalEntries -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR" -p "fiscal_period=$period" -p page_size=200
  other=$(field "[.items[] | select([.lines[].account_id] | index(\"$account\") | not)][0].id // empty")
  [ -n "$other" ] || fail "every entry of the period has a line on account $row"
  say "A citation of an entry with no line on the row's account is refused: a comment cites what its figures are made of."
  refused 422 closing.citation_invalid proposeVarianceComment \
    -body "{\"session_id\": \"$session\", \"close_id\": \"$close\", \"basis\": \"prior_period\", \"account_id\": \"$account\", \"text\": \"Explained by another entry\", \"citations\": [\"$other\"]}"
  say "It proposes a comment citing the entry, for a person to decide:"
  api 201 '"  \(.account_number) \(.account_name) against \(.basis): \(.status) by \(.author_name), \(.actual.amount) against \(.comparison.amount) \(.actual.currency), citing \([.citations[].document_number] | join(", "))"' \
    proposeVarianceComment -body "{\"session_id\": \"$session\", \"close_id\": \"$close\", \"basis\": \"prior_period\", \"account_id\": \"$account\", \"text\": \"The period's movement on this account comes from the entry cited.\", \"citations\": [\"$entry\"]}"
  comment=$(field .id)
  tag=$ETAG
  expect "the proposal" "$(field .status)" proposed
  say "Proposing is all it may do: accepting a comment is a person's."
  refused 403 identity.agent_not_allowed acceptVarianceComment -p "id=$comment" -if-match "$tag" -body '{}'
  QUIET=1 api 200 '' endAgentSession -p "id=$session" -if-match '*' -body '{"status": "completed", "input_tokens": 0, "output_tokens": 0}'
  as carla
  say "Carla sees the proposal on the row, reads it and accepts it:"
  api 200 ".items[] | select(.account_number == \"$row\") | \"  \\(.account_number): \\(.proposed_comments) proposed, explained \\(.explained)\"" \
    listPeriodCloseVariances -p "id=$close" -p basis=prior_period -p requires_comment=true -p page_size=500
  api 200 '"  \(.status) by \(.decided_by_name), proposed by \(.author_name) (\(.author_kind))"' acceptVarianceComment -p "id=$comment" -if-match "$tag" -body '{}'
  say "She explains the other flagged rows herself, against each basis $CC compares, as the unexplained filter lists them:"
  for basis in prior_period prior_year; do
    QUIET=1 api 200 '' listPeriodCloseVariances -p "id=$close" -p "basis=$basis" -p requires_comment=true -p unexplained=true -p page_size=500
    for account in $(field '.items[].account_id'); do
      api 201 '"  \(.basis) \(.account_number) \(.account_name): \(.status), by \(.author_name)"' createVarianceComment \
        -body "{\"close_id\": \"$close\", \"basis\": \"$basis\", \"account_id\": \"$account\", \"text\": \"Reviewed with the department: the movement is expected.\"}"
    done
    api 200 '"  against \(.basis): \(.totals.rows_requiring_comment) rows need a comment, \(.totals.rows_explained) explained", (.items[] | select(.comment) | "    \(.account_number): \(.comment.text) (\(.comment.author_name), accepted by \(.comment.decided_by_name), stale \(.comment.stale))")' \
      listPeriodCloseVariances -p "id=$close" -p "basis=$basis" -p requires_comment=true -p page_size=500
    expect "every flagged row explained against $basis" "$(field '[.items[] | .explained] | all')" true
    n=$(field .totals.rows_requiring_comment)
    expect "the rows explained against $basis" "$(field .totals.rows_explained)" "$n"
    QUIET=1 api 200 '' listPeriodCloseVariances -p "id=$close" -p "basis=$basis" -p requires_comment=true -p unexplained=true
    expect "the unexplained flagged rows against $basis" "$(field '.items | length')" 0
  done
  QUIET=1 api 200 '' listPeriodCloseVariances -p "id=$close" -p basis=prior_period -p page_size=500
  expect "the agent's comment on row $row" "$(field ".items[] | select(.account_number == \"$row\") | \"\(.comment.author_name) \(.comment.stale)\"")" "Close agent false"
  say "The variances check passes: every flagged row has an accepted comment that is current."
  QUIET=1 api 200 '' getPeriodClose -p "id=$close"
  api 200 '"  \(.code) \(.status): \(.check_result.summary)"' checkCloseTask -p "id=$close" -p code=VARIANCES -if-match "$ETAG"
  expect "the check's verdict" "$(field '.check_result.passed')" true
  QUIET=1 api 200 '' getPeriodClose -p "id=$close"
  expect "every task done" "$(field '.tasks_done == .tasks_total')" true
}

# close_variance_agent is T64j's part of step_close (ADR-0058 §12): the
# close agent itself, erp agent close-variances on the close, through the
# model stand-in answering from a script of turns (its suite's replay,
# evals/close). Every flagged row is explained (close_comments) when Alex
# posts a large travel invoice of the period on 6300, an account with no
# figure before, so its row is flagged against both bases and has no
# comment. The agent proposes one comment per basis on that row, citing
# the invoice's entry, and none on the rows below the thresholds; the
# variances check fails on the row until Carla accepts both, and then
# passes, so the close stays ready to sign off.
close_variance_agent() {
  local period close travel inv entry got session basis comment
  period=$((10#${TODAY:5:2}))
  as ada
  say "Ada lets the close agent read the plan against actual by cost center too, the last of its variance tools."
  grant "$(cat "$WORK/close_agent_role")" controlling.plan.read
  as carla
  QUIET=1 api 200 '' listPeriodCloses -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR"
  close=$(field ".items[] | select(.fiscal_period == $period) | .id")
  travel=$(account 6300)
  for basis in prior_period prior_year; do
    QUIET=1 api 200 '' listPeriodCloseVariances -p "id=$close" -p "basis=$basis" -p page_size=500
    expect "no row of 6300 Travel against $basis yet" "$(field '[.items[] | select(.account_number == "6300")] | length')" 0
  done
  as alex
  say "Alex posts a travel invoice of the period: the team's trip to a customer's site, CAD 3,000.00 plus 150.00 GST,"
  say "on 6300 Travel, which had no figure in this period, the prior one or the prior year's."
  QUIET=1 api 201 '' createSupplierInvoice -body "{\"legal_entity_id\": \"$LE\", \"supplier_id\": \"$SUPPLIER\", \"supplier_reference\": \"TRAVEL-${TODAY:0:7}\", \"invoice_date\": \"$TODAY\", \"gross_amount\": {\"amount\": \"3150.00\", \"currency\": \"CAD\"}, \"tax_amount\": {\"amount\": \"150.00\", \"currency\": \"CAD\"}, \"items\": [{\"account_id\": \"$travel\", \"description\": \"Team travel to the customer's site\", \"quantity\": {\"value\": \"1\", \"unit\": \"EA\"}, \"amount\": {\"amount\": \"3000.00\", \"currency\": \"CAD\"}, \"tax_code\": \"V1\"}]}"
  inv=$(field .id)
  api 200 '"  \(.document_number) \(.supplier_reference) of \(.invoice_date): \(.status), journal entry \(.journal_entry_id[0:8])…"' \
    postSupplierInvoice -p "id=$inv" -if-match '"1"' -body "{\"posting_date\": \"$TODAY\"}"
  entry=$(field .journal_entry_id)
  as carla
  say "The row is flagged against both bases, and it alone needs a comment:"
  for basis in prior_period prior_year; do
    api 200 "\"  against $basis: \\([.items[] | \"\\(.account_number) \\(.account_name) \\(.variance.amount)\"] | join(\", \"))\"" \
      listPeriodCloseVariances -p "id=$close" -p "basis=$basis" -p requires_comment=true -p unexplained=true -p page_size=500
    expect "the unexplained flagged rows against $basis" "$(field '[.items[].account_number] | join(" ")')" 6300
  done
  say "The close agent runs: erp agent close-variances starts a session on erp-close, reads through its tools the"
  say "close's variance report against each basis $CC configures and the entries behind the rows that need a comment,"
  say "and proposes. The model is the stand-in, answering from a script of turns as the agent's evaluation suite"
  say "replays them (evals/close): it reads the close, the unexplained flagged rows against each basis and the"
  say "entries on 6300, and proposes a comment on the row against each basis citing the invoice's entry. The rows"
  say "below the thresholds, and those Carla has explained, get none."
  jq -n --arg close "$close" --arg le "$LE" --argjson year "$YEAR" --argjson period "$period" --arg acct "$travel" --arg entry "$entry" '{
    vars: {session: "session id is ([0-9a-f-]{36})"},
    turns: ([
      {tool: "getPeriodClose", input: {id: $close}},
      {tool: "listPeriodCloseVariances", input: {id: $close, basis: "prior_period", requires_comment: true, unexplained: true}},
      {tool: "listPeriodCloseVariances", input: {id: $close, basis: "prior_year", requires_comment: true, unexplained: true}},
      {tool: "listJournalEntries", input: {legal_entity_id: $le, fiscal_year: $year, fiscal_period: $period, account_id: $acct}}]
      + ([["prior_period", "the prior period"], ["prior_year", "the same period of the prior year"]] | map(
      {tool: "proposeVarianceComment", input: {body: {session_id: "{{session}}", close_id: $close, basis: .[0], account_id: $acct,
        text: "Travel is 3,000.00 this period against 0.00 in \(.[1]): the entry cited, the supplier invoice for the team travel to a customer site.",
        citations: [$entry]}}}))
      + [{final: "Proposed comments on 6300 Travel against the prior period and the prior year, citing its entry; every other row is explained or below the thresholds."}])}' \
    >"$WORK/model-script.json"
  show ERP_AGENT_TOKEN=… erp agent close-variances -close "$close"
  got=$(ERP_AGENT_TOKEN=$(cat "$WORK/tokens/close-agent") "$ERP" agent close-variances -close "$close" 2>"$WORK/stderr") ||
    fail "erp agent close-variances failed ($(cat "$WORK/stderr"))"
  rm -f "$WORK/model-script.json"
  printf '%s\n' "$got" | sed 's/^/     /'
  session=$(printf '%s\n' "$got" | sed -nE '1s/^Session ([0-9a-f-]{36}): .*/\1/p')
  expect "what the agent made" "$(printf '%s\n' "$got" | sed -nE '1s/^Session [0-9a-f-]{36}: //p')" "proposals made 2, refused 0."
  expect "the comments it printed" "$(printf '%s\n' "$got" | sed -nE 's/^  comment [0-9a-f-]{36} on (.*)$/\1/p' | paste -sd',' -)" \
    "6300 against prior_period,6300 against prior_year"
  as carla
  say "Carla reads the session's comments: on the flagged row alone, against each basis, each citing the invoice's entry:"
  api 200 '(.items[] | "  \(.basis) \(.account_number) \(.account_name): \(.status) by \(.author_name), citing \([.citations[].document_number] | join(", "))", "    \(.text)")' \
    listVarianceComments -p "session_id=$session"
  expect "the session's comments" "$(field '[.items[] | "\(.account_number) \(.basis)"] | join(",")')" "6300 prior_period,6300 prior_year"
  expect "each citing the invoice's entry" "$(field "[.items[] | [.citations[].journal_entry_id] == [\"$entry\"]] | all")" true
  expect "each waiting for a person" "$(field '[.items[].status == "proposed"] | all')" true
  say "None on the rows below the thresholds:"
  for basis in prior_period prior_year; do
    api 200 "\"  against $basis: \\(.items | length) rows below the thresholds, \\([.items[] | select(.proposed_comments > 0)] | length) with a proposal\"" \
      listPeriodCloseVariances -p "id=$close" -p "basis=$basis" -p requires_comment=false -p page_size=500
    expect "rows below the thresholds against $basis" "$(field '.items | length > 0')" true
    expect "proposals on them against $basis" "$(field '[.items[] | select(.proposed_comments > 0)] | length')" 0
  done
  as close-agent
  say "The agent's session, which the command named, records each tool call:"
  api 200 '"  session \(.id[0:8])… \(.status) on \(.model): \(.purpose)"' getAgentSession -p "id=$session"
  expect "the session" "$(field .status)" completed
  api 200 '"  its tool calls: \([.items[].tool] | join(", "))"' listAgentToolCalls -p "id=$session"
  expect "its tool calls" "$(field '[.items[].tool] | join(" ")')" \
    "getPeriodClose listPeriodCloseVariances listPeriodCloseVariances listJournalEntries proposeVarianceComment proposeVarianceComment"
  as carla
  say "Until a person accepts them, the variances check fails on the row:"
  QUIET=1 api 200 '' getPeriodClose -p "id=$close"
  api 200 '"  \(.code) \(.status): \(.check_result.summary)"' checkCloseTask -p "id=$close" -p code=VARIANCES -if-match "$ETAG"
  expect "the check's verdict before" "$(field '.check_result.passed')" false
  say "Carla accepts both comments:"
  QUIET=1 api 200 '' listVarianceComments -p "session_id=$session"
  for comment in $(field '.items[].id'); do
    QUIET=1 api 200 '' getVarianceComment -p "id=$comment"
    api 200 '"  \(.basis) \(.account_number): \(.status) by \(.decided_by_name), proposed by \(.author_name) (\(.author_kind))"' \
      acceptVarianceComment -p "id=$comment" -if-match "$ETAG" -body '{}'
  done
  QUIET=1 api 200 '' getPeriodClose -p "id=$close"
  api 200 '"  \(.code) \(.status): \(.check_result.summary)"' checkCloseTask -p "id=$close" -p code=VARIANCES -if-match "$ETAG"
  expect "the check's verdict after" "$(field '.check_result.passed')" true
  QUIET=1 api 200 '' getPeriodClose -p "id=$close"
  expect "every task done" "$(field '.tasks_done == .tasks_total')" true
}

# close_sign_off is T64d's part of step_close, its end (ADR-0058 §7): the
# sign-off is refused while the period is open; Carla closes it in each
# covered ledger and signs off, which runs the checks again and locks each
# period through the period lock (T32), the leading ledger first; a manual
# entry dated in the period is then refused.
close_sign_off() {
  local period close tag p
  period=$((10#${TODAY:5:2}))
  as carla
  QUIET=1 api 200 '' listPeriodCloses -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR"
  close=$(field ".items[] | select(.fiscal_period == $period) | .id")
  QUIET=1 api 200 '' getPeriodClose -p "id=$close"
  tag=$ETAG
  say "Every task is done. Carla signs off the close; it is refused while the period still takes postings: a close"
  say "is signed off after the period is closed, and a sign-off never closes a period itself."
  refused 409 closing.period_not_closed signOffPeriodClose -p "id=$close" -if-match "$tag" -body '{}'
  say "She closes the period in each ledger the close covers (closeFiscalPeriod):"
  QUIET=1 api 200 '' getPeriodClose -p "id=$close"
  for p in $(field '.ledgers[] | "\(.ledger_code)=\(.fiscal_period_id)"'); do
    QUIET=1 api 200 '' getFiscalPeriod -p "id=${p#*=}"
    api 200 '"  '"${p%%=*}"' period \(.fiscal_year)/\(.period): \(.status)"' closeFiscalPeriod -p "id=${p#*=}" -if-match "$ETAG"
  done
  say "and signs off. The checks run again (the parked-entries check passes and is recorded), and each covered period"
  say "is locked through the period lock, the leading ledger first: it no longer reopens with :open, only through an"
  say "approved unlock request."
  api 200 '"signed off by \(.signed_by_name) at \(.signed_at): \(.note)", (.ledgers[] | "  \(.ledger_code) \(.ledger_name): period \(if .locked then "locked by the sign-off" else "locked already" end)")' \
    signOffPeriodClose -p "id=$close" -if-match "$tag" -body '{"note": "Period closed; every task done and every check passing"}'
  expect "every covered period locked by the sign-off" "$(field '[.ledgers[].locked] | all')" true
  expect "the leading ledger first" "$(field '.ledgers[0].is_leading')" true
  api 200 '"\(.company_code) \(.fiscal_year)/\(.fiscal_period): \(.status), by \(.signed_off_by_name)",
    (.ledgers[] | "  \(.ledger_code): \(.period_status)")' getPeriodClose -p "id=$close"
  expect "the close" "$(field .status)" signed_off
  expect "the periods" "$(field '[.ledgers[].period_status] | unique | join(" ")')" locked
  expect "its sign-offs" "$(field '.sign_offs | length')" 1
  as tom
  say "A manual entry dated in the period is refused: the period is locked."
  refused 409 ledger.period_not_open postJournalEntry \
    -body "{\"legal_entity_id\": \"$LE\", \"document_date\": \"$TODAY\", \"posting_date\": \"$TODAY\", \"currency\": \"CAD\", \"header_text\": \"A late courier charge\", \"lines\": [{\"account_id\": \"$(account 6000)\", \"amount\": {\"amount\": \"40.00\", \"currency\": \"CAD\"}}, {\"account_id\": \"$(account 211000)\", \"amount\": {\"amount\": \"-40.00\", \"currency\": \"CAD\"}, \"partner_id\": \"$SUPPLIER\"}]}"
}

# consolidation_comprehensive_income is other comprehensive income on the
# profit and loss consolidation_statements just read (the accounting
# specialist's B3, po:2026-11-09; ADR-0059 §11, As built: other
# comprehensive income): below the profit, the fiscal year's translation
# differences (IAS 21.39(c), 21.41) with the translation reserve of each
# company (IAS 21.52(b)), and total comprehensive income attributed to
# non-controlling interests and to the owners of the parent (IAS 1.81B).
# MAPLE has no final run of an earlier fiscal year, so the reserve opens
# at zero and the year's difference is Acme Corp US's whole reserve, the
# one the balance sheet carries; Maple holds it whole, so all of it is the
# owners'. It reads $WORK/profit-and-loss.json and $WORK/balance-sheet.json.
consolidation_comprehensive_income() {
  local column cents='(.amount | tonumber * 100 | round)' pl="$WORK/profit-and-loss.json"
  say "Below the profit, the statement shows other comprehensive income (B3): the year's translation differences are not"
  say "part of the profit; they accumulate in the translation reserve, kept per company. The reserve opens at what the"
  say "group's final run of the previous fiscal year's last period showed; MAPLE has none, so it opens at zero:"
  jq -r '"  opening run: \(.opening_run_id // "none")",
    (.translation_reserve[] | "  \(.company_code) \(.company_name) (\(.role)\(if .ownership_percent then ", held at \(.ownership_percent)%" else "" end)): opening \(.opening.amount), difference \(.difference.amount), closing \(.closing.amount) \(.closing.currency); non-controlling interests \(.non_controlling_interests.amount), owners of the parent \(.owners_of_the_parent.amount)"),
    (.totals | to_entries[] | select(.key | test("comprehensive")) | "  total \(.key): reported \(.value.reported.amount), eliminations \(.value.eliminations.amount), consolidated \(.value.consolidated.amount)")' "$pl"
  expect "the opening run" "$(jq -r '.opening_run_id // "none"' "$pl")" none
  expect "the companies with a reserve row" "$(jq -r '[.translation_reserve[] | "\(.company_code) \(.role)"] | join(", ")' "$pl")" "1000 member, 3000 parent"
  expect "each reserve's opening and difference make its closing" \
    "$(jq -r "[.translation_reserve[] | (.opening | $cents) + (.difference | $cents) == (.closing | $cents)] | all" "$pl")" true
  expect "each difference's two shares make it" \
    "$(jq -r "[.translation_reserve[] | (.non_controlling_interests | $cents) + (.owners_of_the_parent | $cents) == (.difference | $cents)] | all" "$pl")" true
  expect "other comprehensive income is the companies' differences" "$(jq -r ".totals.other_comprehensive_income.reported | $cents" "$pl")" \
    "$(jq -r "[.translation_reserve[].difference | $cents] | add" "$pl")"
  expect "Acme Corp US has a translation difference" "$(jq -r '.translation_reserve[] | select(.company_code == "1000") | .difference.amount != "0.00"' "$pl")" true
  expect "the reserve on the balance sheet is its opening and other comprehensive income, consolidated" \
    "$(jq -r ".items[] | select(.role == \"cta\") | .consolidated | $cents" "$WORK/balance-sheet.json")" \
    "$(jq -r "([.translation_reserve[].opening | $cents] | add) + (.totals.other_comprehensive_income.consolidated | $cents)" "$pl")"
  for column in reported eliminations consolidated; do
    expect "total comprehensive income is the profit and other comprehensive income, $column" \
      "$(jq -r ".totals.total_comprehensive_income.$column | $cents" "$pl")" \
      "$(jq -r "(.totals.profit.$column | $cents) + (.totals.other_comprehensive_income.$column | $cents)" "$pl")"
  done
  say "Maple holds the whole of Acme Corp US, so total comprehensive income is the owners' of the parent too:"
  expect "total comprehensive income's attribution" \
    "$(jq -r '"\(.totals.comprehensive_income_non_controlling_interests.consolidated.amount) \(.totals.comprehensive_income_owners_of_the_parent.consolidated.amount == .totals.total_comprehensive_income.consolidated.amount)"' "$pl")" \
    "0.00 true"
}

# close_consolidation is finalizing the group's consolidation (T68g,
# ADR-0059 §10), the end of step_close: after Maple's sign-off locked its
# period, Carla closes and locks Acme Corp US's, runs the group MAPLE
# again on the locked periods, discards step_consolidation's draft,
# accepts the new run's open difference and finalizes it: the group's
# consolidated figures of the period, by her, not stale.
close_consolidation() {
  local period us p draft run difference tag
  period=$((10#${TODAY:5:2}))
  as carla
  QUIET=1 api 200 '' listLegalEntities -p page_size=100
  us=$(field '.items[] | select(.code == "1000") | .id')
  say "Maple's period is locked. Carla closes and locks Acme Corp US's period $YEAR-$period in its leading ledger, so that"
  say "the group's figures can be finalized (ADR-0059 §10):"
  QUIET=1 api 200 '' listFiscalPeriods -p "legal_entity_id=$us" -p "fiscal_year=$YEAR" -p "ledger_id=$LEADING" -p page_size=100
  p=$(field ".items[] | select(.period == $period) | \"\(.id)/\(.version)\"")
  api 200 '"  period \(.fiscal_year)-\(.period) of Acme Corp US: \(.status)"' closeFiscalPeriod -p "id=${p%/*}" -if-match "\"${p#*/}\""
  api 200 '"  period \(.fiscal_year)-\(.period) of Acme Corp US: \(.status)"' lockFiscalPeriod -p "id=${p%/*}" -if-match "$ETAG"
  say "Acme Corp US's acquisition is measured at $ACQ_PERIOD, the period before its first; finalizing freezes it for good,"
  say "so Carla closes and locks that period too (ADR-0059 §9):"
  QUIET=1 api 200 '' listFiscalPeriods -p "legal_entity_id=$us" -p "fiscal_year=${ACQ_PERIOD%-*}" -p "ledger_id=$LEADING" -p page_size=100
  p=$(field ".items[] | select(.period == $((10#${ACQ_PERIOD#*-}))) | \"\(.id)/\(.version)\"")
  api 200 '"  period \(.fiscal_year)-\(.period) of Acme Corp US: \(.status)"' closeFiscalPeriod -p "id=${p%/*}" -if-match "\"${p#*/}\""
  api 200 '"  period \(.fiscal_year)-\(.period) of Acme Corp US: \(.status)"' lockFiscalPeriod -p "id=${p%/*}" -if-match "$ETAG"
  QUIET=1 api 200 '' listConsolidationRuns -p group_code=MAPLE -p status=computed
  draft=$(field '.items[0].id')
  say "She runs the group again: the run reads both companies' figures from their locked periods."
  api 202 '"operation \(.kind) \(.status): \(.resource)"' createConsolidationRun -body "{\"group_code\": \"MAPLE\", \"fiscal_year\": $YEAR, \"period\": $period}"
  run=$(field '.resource | split("/") | last')
  wait_op
  api 200 '"run \(.status): \([.members[] | "\(.company_code) period \(.period_status) at version \(.period_version)"] | join(", ")); \([.difference_counts[] | "\(.count) \(.status)"] | join(", "))"' \
    getConsolidationRun -p "id=$run"
  expect "the new run" "$(field '"\(.status) \([.members[].period_status] | unique | join(" "))"')" "computed locked"
  say "The draft of step 40, read from open periods, is discarded:"
  QUIET=1 api 200 '' getConsolidationRun -p "id=$draft"
  api 200 '"  run \(.id): \(.status)"' discardConsolidationRun -p "id=$draft" -if-match "$ETAG"
  expect "the draft" "$(field .status)" discarded
  QUIET=1 api 200 '' listIntercompanyDifferences -p "id=$run" -p status=open
  difference=$(field '.items[0].id')
  tag="\"$(field '.items[0].version')\""
  say "The same rate difference is open in the new run; Carla accepts it again, and finalizes the run:"
  api 200 '"  \(.elimination.code) \(.difference.amount) \(.difference.currency): \(.status) by \(.accepted_by_name)"' acceptIntercompanyDifference \
    -p "id=$run" -p "difference_id=$difference" -if-match "$tag" -body '{"reason": "Rate difference, as in the draft"}'
  QUIET=1 api 200 '' getConsolidationRun -p "id=$run"
  api 200 '"run of \(.group_code) for \(.fiscal_year)-\(.period): \(.status), finalized by \(.finalized_by_name) at \(.finalized_at), stale \(.stale)"' \
    finalizeConsolidationRun -p "id=$run" -if-match "$ETAG"
  expect "the final run" "$(field '"\(.status) \(.finalized_by_name) \(.stale)"')" "final Carla Controller false"
  expect "the acquisition it froze" "$(field '[.members[] | select(.acquisition) | .acquisition.frozen_by_run_id] | join(" ")')" "$run"
  api 200 '"\(.statement), run \(.run_status), stale \(.stale): assets \(.totals.assets.consolidated.amount), profit \(.totals.profit.consolidated.amount) \(.group_currency)"' \
    getConsolidatedStatement -p "id=$run" -p statement=balance_sheet -p page_size=500
  expect "the final run's statement" "$(field '"\(.run_status) \(.stale)"')" "final false"
  api 200 '"final runs of MAPLE: \([.items[] | "\(.fiscal_year)-\(.period) by \(.finalized_by_name)"] | join(", "))"' listConsolidationRuns -p group_code=MAPLE -p status=final
  expect "the group's final runs" "$(field '.items | length')" 1
  say "The final run froze Acme Corp US's acquisition (ADR-0059 §9): a later run reads it rather than measuring it again."
  api 202 '"operation \(.kind) \(.status): \(.resource)"' createConsolidationRun -body "{\"group_code\": \"MAPLE\", \"fiscal_year\": $YEAR, \"period\": $period}"
  draft=$(field '.resource | split("/") | last')
  wait_op
  api 200 '"run \(.status): \([.members[] | select(.acquisition) | "\(.company_code)'"'"'s acquisition, investment \(.acquisition.investment.amount) \(.acquisition.investment.currency), frozen \(.acquisition.frozen)"] | join(", "))"' \
    getConsolidationRun -p "id=$draft"
  expect "the later run's acquisition" "$(field '"\(.status) \([.members[] | select(.acquisition) | "\(.acquisition.investment.amount) \(.acquisition.frozen)"] | join(" "))"')" \
    "computed $(amount "$CAPITAL") true"
  api 200 '"  run \(.id): \(.status)"' discardConsolidationRun -p "id=$draft" -if-match "$ETAG"
}

# step_cash_actuals is the statement lines of the week classified by
# their matches, by liquidity item and bucket, and the lines behind them
# as each reader may see them (T53f2, ADR-0051 §4, §11).
step_cash_actuals() {
  heading "Step 27 — Cash actuals: the week's statement lines by liquidity item, classified by what they were matched to"
  as tom
  local from moved payments
  from=$(date_add "$TODAY" -6)
  moved=$(amount $(($(cents "$CLOSING") - $(cents 500000.00))))
  payments=$(jq '.items | length' "$WORK/payments.json")
  say "Tom reads what actually moved on Maple's accounts from $from to $TODAY, in one bucket of a week (T53f2): step 8's"
  say "statement lines by value date, each classified as its match says; a line matched to payments takes the liquidity"
  say "items of the supplier items they settled, a line not matched yet its direction's default, counted unclassified."
  api 200 '"\(.from)..\(.to) by \(.grain), read on \(.read_date) in \(.time_zone)", (.currencies[] | .currency as $c
      | "  \($c): in \(.inflow.amount), out \(.outflow.amount), total \(.total.amount) of \(.count) lines, unclassified \(.unclassified.amount.amount) (\(.unclassified.count))",
      (.banks[] | "    bank \(.house_bank_code): \(.total.amount) of \(.count) lines"),
      (.items[] | "    \("  " * .level)\(.liquidity_item // "unclassified") \(.name // ""): \(.total.amount)\([.matches[] | ", \(.match) \(.total.amount)"] | join(""))"))' \
    getCashActuals -p "legal_entity_id=$LE" -p "from=$from" -p "to=$TODAY" -p grain=week
  expect "the CAD total, what step 8's statement moved" "$(field '.currencies[] | select(.currency == "CAD") | .total.amount')" "$moved"
  expect "the CAD lines, one per payment of the run" "$(field '.currencies[] | select(.currency == "CAD") | .count')" "$payments"
  expect "the unclassified lines once reconciled" "$(field '.currencies[] | select(.currency == "CAD") | .unclassified.count')" 0
  expect "RBC's total, the currency's" "$(field '.currencies[] | select(.currency == "CAD") | .banks[] | select(.house_bank_code == "RBC") | .total.amount')" "$moved"
  expect "the top rows add up to the total" \
    "$(field '.currencies[] | select(.currency == "CAD") | ([.items[] | select(.level == 0) | .total.amount | sub("\\."; "") | tonumber] | add) == (.total.amount | sub("\\."; "") | tonumber)')" true
  say "The lines behind them, each with its statement, the payment it was matched to and its supplier:"
  api 200 '"  \(.count) lines, total \(.total.amount) \(.total.currency)", (.items[0:5][] | "  \(.value_date) \(.house_bank_code) line \(.statement.line_no // "-") of \(.statement.statement_ref // "-") \(.status) by \(.match): \(.amount.amount)\([.shares[] | ", \(.liquidity_item // "unclassified") \(.amount.amount)"] | join(""));\([.payments[] | " run \(.payment_run_number) \(.amount.amount) to \(.supplier.name // "-")"] | join(""))")' \
    listCashActualLines -p "legal_entity_id=$LE" -p currency=CAD -p "from=$from" -p "to=$TODAY" -p page_size=500
  expect "every line matched to a payment" "$(field '[.items[] | select(.match == "payment" and .payment_count == 1)] | length')" "$payments"
  expect "each payment and its supplier shown to Tom" "$(field '[.items[] | select((.payments | length) == 1 and .payments[0].supplier and (.suppliers | length) == 1 and .statement.statement_ref)] | length')" "$payments"
  as rita
  say "Rita holds the forecast's read alone: she reads the same lines without their statements, payments or suppliers."
  api 200 '"  \(.count) lines; statements shown \([.items[] | select(.statement)] | length), payments \([.items[].payments[]] | length), suppliers \([.items[].suppliers[]] | length)"' \
    listCashActualLines -p "legal_entity_id=$LE" -p currency=CAD -p "from=$from" -p "to=$TODAY" -p page_size=500
  expect "what Rita is shown of other contexts" "$(field '[.items[] | select(.statement or (.payments | length) > 0 or (.suppliers | length) > 0)] | length')" 0
  as tom
  passed
}

# step_cash_variance is the forecast of the day's snapshot against the
# day's statement lines, attributed by document (T53f3, ADR-0051 §11).
step_cash_variance() {
  heading "Step 28 — Forecast variance: what the snapshot forecast for the day against what the statements show, attributed"
  as tom
  local day board snap kind adds actual forecast
  QUIET=1 api 200 '' listCashSnapshots -p "legal_entity_id=$LE" -p kind=on_demand
  board=$(field '[.items[] | select(.name == "Board meeting")][0].id')
  day=$(field '[.items[] | select(.name == "Board meeting")][0].snapshot_date')
  say "Tom compares the forecast of the snapshot taken on $day (lag 0: the day itself) with what the bank statements show"
  say "for that day (T53f3): per liquidity item and bucket the forecast, the actual and the variance (actual less forecast),"
  say "its size in percent of the forecast's; each document the snapshot forecast is matched to the statement lines that"
  say "paid it, and the variance is attributed to timing, amount, new (paid, not forecast), missing (forecast, not paid)"
  say "and unclassified (not matched yet). The daily snapshot is compared if one was taken, else the last taken on demand."
  api 200 '"snapshot \(.snapshot.kind) of \(.snapshot.snapshot_date), per document to \(.snapshot.document_level_until); \(.from)..\(.to) lag \(.lag) by \(.grain), read on \(.read_date) in \(.time_zone)",
      (.currencies[] | .currency as $c
      | "  \($c): forecast \(.total.forecast.amount), actual \(.total.actual.amount), variance \(.total.variance.amount) (\(.total.variance_percent // "-")%)",
        "    timing \(.total.attribution.timing.amount), amount \(.total.attribution.amount.amount), new \(.total.attribution.new.amount), missing \(.total.attribution.missing.amount), unclassified \(.total.attribution.unclassified.amount)",
        (.items[] | "    \("  " * .level)\(.liquidity_item // "unclassified"): \(.total.forecast.amount) -> \(.total.actual.amount), variance \(.total.variance.amount)"))' \
    getLiquidityForecastVariance -p "legal_entity_id=$LE" -p "from=$day" -p "to=$day" -p lag=0
  snap=$(field .snapshot.id)
  kind=$(field .snapshot.kind)
  adds=$(field 'def c: sub("\\."; "") | tonumber; [.currencies[] | (.buckets[], .total, .items[].total, .items[].buckets[])
    | ([.attribution[] | .amount | c] | add) == (.variance.amount | c)] | all')
  expect "every cell's attribution adds up to its variance" "$adds" true
  actual=$(field '[.currencies[] | select(.currency == "CAD") | .total.actual.amount][0] // "0.00"')
  forecast=$(field '[.currencies[] | select(.currency == "CAD") | .total.forecast.amount][0] // "0.00"')
  QUIET=1 api 200 '' getCashActuals -p "legal_entity_id=$LE" -p "from=$day" -p "to=$day"
  expect "the CAD actual, the day's statement lines (getCashActuals)" "$actual" "$(field '[.currencies[] | select(.currency == "CAD") | .total.amount][0] // "0.00"')"
  QUIET=1 api 200 '' listCashSnapshotFlows -p "id=$snap" -p currency=CAD -p "from=$day" -p "to=$day" -p page_size=500
  expect "the CAD forecast, the snapshot's lines of the day but blocked suppliers'" "$forecast" \
    "$(amount "$(field 'def c: sub("\\."; "") | tonumber; [.items[] | select(.source != "payable_blocked") | .amount.amount | c] | add // 0')")"
  if [ "$kind" = on_demand ]; then
    expect "the snapshot compared, step 26's" "$snap" "$board"
  fi
  as rita
  say "Rita holds the forecast's read alone: the variance names no document or supplier, so she reads the same figures."
  api 200 '"  \([.currencies[] | "\(.currency) variance \(.total.variance.amount)"] | join(", "))"' \
    getLiquidityForecastVariance -p "legal_entity_id=$LE" -p "from=$day" -p "to=$day" -p lag=0
  expect "Rita's CAD actual, Tom's" "$(field '[.currencies[] | select(.currency == "CAD") | .total.actual.amount][0] // "0.00"')" "$actual"
  as tom
  say "A week's lag needs the snapshot of a week before, which this new tenant never took; lags of 0, 7 and 30 days are offered:"
  refused 409 cash.snapshot_not_taken getLiquidityForecastVariance -p "legal_entity_id=$LE" -p "from=$day" -p "to=$day" -p lag=7
  refused 422 request.validation_failed getLiquidityForecastVariance -p "legal_entity_id=$LE" -p "from=$day" -p "to=$day" -p lag=5
  passed
}

# step_depreciation_run is this period's depreciation posted by a run in
# the leading ledger, the lathe of step 22 among the assets step 25
# previewed, and a repeat run posting nothing (T57f, ADR-0057 §7).
step_depreciation_run() {
  heading "Step 29 — The depreciation run: this period's depreciation posted in the leading ledger, one entry per batch; a repeat run posts nothing"
  as carla
  say "Carla runs this period's depreciation in the leading ledger (ADR-0057 §7, T57f). The run is a long-running"
  say "operation: it selects the assets the preview shows, in batches of 500, and posts each batch as one entry in"
  say "that ledger alone (document type AF, dated the period's last day), each asset's planned depreciation less what is"
  say "booked, on its class's accounts and its centers."
  local period lathe run expected entry number
  QUIET=1 api 200 '' listAssets -p "legal_entity_id=$LE" -p status=active
  lathe=$(field '[.items[] | select(.description == "CNC lathe") | .id][0]')
  QUIET=1 api 200 '' getAssetSchedule -p "id=$lathe" -p "fiscal_year=$YEAR"
  period=$(field ".periods[] | select(.start_date <= \"$TODAY\" and .end_date >= \"$TODAY\") | .period")
  api 200 '"the preview of \(.fiscal_year)/\(.period) in \(.ledger_code): \(.totals.assets) assets, difference \(.totals.difference.amount) \(.totals.difference.currency)"' \
    previewDepreciationRun -body "{\"legal_entity_id\": \"$LE\", \"fiscal_year\": $YEAR, \"period\": $period}"
  expected=$(field .totals.difference.amount)
  api 202 '"\(.kind) \(.status): \(.resource)"' startDepreciationRun -body "{\"legal_entity_id\": \"$LE\", \"fiscal_year\": $YEAR, \"period\": $period}"
  run=$(field .resource)
  run=${run##*/}
  wait_op
  api 200 '"run \(.fiscal_year)/\(.period) in \(.ledger_code) dated \(.posting_date): \(.status), \(.assets_planned) assets planned, \(.assets_posted) posted, \(.amount.amount) \(.amount.currency)",
    (.batches[] | "  batch \(.batch_no): \(.assets) assets, \(.amount.amount), entry \(.document_number)")' getDepreciationRun -p "id=$run"
  expect "the run's status" "$(field .status)" posted
  expect "what the run posted, the preview's difference" "$(field .amount.amount)" "$expected"
  entry=$(field '.batches[0].journal_entry_id')
  number=$(field '.batches[0].document_number')
  api 200 '"entry \(.document_number) on \(.posting_date), in the leading ledger: \(.leading_ledger); lines \([.lines[] | .amount.amount] | join(", "))"' \
    getJournalEntry -p "id=$entry"
  expect "the batch's entry's ledger" "$(field '"\(.leading_ledger) \(.document_number)"')" "true $number"
  say "The lathe's movements name the batch's entry, in the leading ledger alone; it is booked through the period there,"
  say "so its terms in that ledger are fixed now:"
  api 200 '.items[] | "  \(.seq) \(.kind) \(.ledger_code) \(.posting_date): accumulated \(.accumulated.amount) \(.accumulated.currency), entry \(.document_number)"' \
    listAssetMovements -p "id=$lathe" -p kind=depreciation
  expect "the lathe's depreciation in 0L" "$(field '[.items[] | "\(.ledger_code) \(.accumulated.amount) \(.document_number)"] | join(", ")')" "0L -41.67 $number"
  say "A second run of the period posts only what changed since: nothing."
  api 202 '"\(.kind) \(.status)"' startDepreciationRun -body "{\"legal_entity_id\": \"$LE\", \"fiscal_year\": $YEAR, \"period\": $period}"
  run=$(field .resource)
  run=${run##*/}
  wait_op
  api 200 '"repeat run: \(.status), \(.assets_planned) assets, \(.assets_posted) posted, \(.amount.amount) \(.amount.currency), entries \([.batches[] | .document_number // "none"] | join(", "))"' \
    getDepreciationRun -p "id=$run"
  expect "the repeat run" "$(field '"\(.status) \(.assets_posted) \(.amount.amount) \([.batches[].journal_entry_id] | map(select(. != null)) | length)"')" "posted 0 0.00 0"
  say "A run of an earlier period is refused while this one's is posted:"
  if [ "$period" -gt 1 ]; then
    refused 409 assets.run_out_of_order startDepreciationRun -body "{\"legal_entity_id\": \"$LE\", \"fiscal_year\": $YEAR, \"period\": $((period - 1))}"
  fi
  passed
}

# step_cash_variance_comment is a person's explanation of a row of the
# day's variance, read back by another reader, who may not write one
# (T53f4, ADR-0051 §11, §13, §15).
step_cash_variance_comment() {
  heading "Step 30 — Variance comments: Tom explains a row of the day's variance; Rita reads it and may not write one"
  as tom
  local day item row comment snap
  QUIET=1 api 200 '' listCashSnapshots -p "legal_entity_id=$LE" -p kind=on_demand
  day=$(field '[.items[] | select(.name == "Board meeting")][0].snapshot_date')
  QUIET=1 api 200 '' getLiquidityForecastVariance -p "legal_entity_id=$LE" -p "from=$day" -p "to=$day" -p lag=0
  snap=$(field .snapshot.id)
  item=$(field '[.currencies[] | select(.currency == "CAD") | .items[] | select(.liquidity_item != null and .level == 0)][0].liquidity_item // empty')
  if [ -n "$item" ]; then row="\"liquidity_item\": \"$item\""; else row='"unclassified": true'; fi
  say "Tom explains the variance of ${item:-what no rule assigns} on $day at lag 0 (T53f4). The comment names the row, the"
  say "period and the lag, and the snapshot that variance compares with, which is then kept with it past its 25 months;"
  say "a person's comment is accepted as written, audited and published as VarianceCommentDecided."
  api 201 '"comment on \(.liquidity_item // "unclassified") \(.liquidity_item_name // ""): \(.from)..\(.to) lag \(.lag), snapshot of \(.snapshot_date), \(.status)", "  \"\(.text)\""' \
    createForecastVarianceComment -body "{\"legal_entity_id\": \"$LE\", \"from\": \"$day\", \"to\": \"$day\", \"lag\": 0, $row, \"text\": \"The run of the day paid as forecast; the rest is the bank's timing.\"}"
  comment=$(field .id)
  expect "the comment accepted as written" "$(field .status)" accepted
  expect "the comment's row" "$(field '.liquidity_item // "unclassified"')" "${item:-unclassified}"
  QUIET=1 api 200 '' getForecastVarianceComment -p "id=$comment"
  expect "its snapshot, the variance's" "$(field .snapshot_id)" "$snap"
  as rita
  say "Rita holds the forecast's read alone: she reads the comments of the period, and may not write one."
  api 200 '.items[] | "  \(.company_code) \(.from)..\(.to) lag \(.lag) \(.liquidity_item // "unclassified"): \(.text) (\(.status))"' \
    listForecastVarianceComments -p "legal_entity_id=$LE" -p "from=$day" -p "to=$day"
  expect "Tom's comment listed for Rita" "$(field "[.items[].id] | index(\"$comment\") != null")" true
  refused 403 identity.permission_missing createForecastVarianceComment \
    -body "{\"legal_entity_id\": \"$LE\", \"from\": \"$day\", \"to\": \"$day\", \"lag\": 0, $row, \"text\": \"Not mine to explain.\"}"
  as tom
  passed
}

# step_asset_retirement is half the press of step 24 sold and the rest
# scrapped after step 29's run, retiring it (T57g, ADR-0057 §8).
step_asset_retirement() {
  heading "Step 31 — Asset retirements: half the press sold with a gain, the rest scrapped, the press retired; a retirement valued in the period before refused"
  as carla
  say "Maple sells half of the hydraulic press of step 24 for CAD 4,000.00 and scraps the rest (ADR-0057 §8, T57g). This"
  say "period's run (step 29) already posted the press's depreciation in the leading ledger. A retirement first catches"
  say "up each ledger's depreciation through the period before its own, which posts this period's back, so the half sold"
  say "depreciates for none of it; it then takes its share of the press's cost and accumulated depreciation and posts one"
  say "entry per ledger: Dr sale clearing for the proceeds, Cr machinery for the cost, the gain or loss for the difference."
  local press start prev values sale scrap
  accounts
  QUIET=1 api 200 '' listAssets -p "legal_entity_id=$LE" -p status=active
  press=$(field '[.items[] | select(.description == "Hydraulic press") | .id][0]')
  values='"  asset \(.asset_number) \(.description): \(.status); \([.values[] | "\(.ledger_code) cost \(.cost.amount), accumulated \(.accumulated.amount), net \(.net_book_value.amount) \(.net_book_value.currency)"] | join("; "))"'
  api 200 "$values" getAsset -p "id=$press"
  QUIET=1 api 200 '' getAssetSchedule -p "id=$press" -p "fiscal_year=$YEAR"
  start=$(field ".periods[] | select(.start_date <= \"$TODAY\" and .end_date >= \"$TODAY\") | .start_date")
  prev=$(date_add "$start" -1)
  say "A retirement is not backdated: one valued in the period before this one is refused."
  refused 422 assets.value_date_outside_period createAssetRetirement \
    -body "{\"asset_id\": \"$press\", \"kind\": \"scrapping\", \"value_date\": \"$prev\", \"posting_date\": \"$TODAY\"}"
  api 201 '"retirement \(.kind) of asset \(.asset_number), \(.share_percent) %, proceeds \(.proceeds.amount) \(.proceeds.currency) (group \(.proceeds_gc.amount) \(.proceeds_gc.currency)): \(.status)",
    (.ledgers[] | "  \(.ledger_code): catch-up \(.catch_up.amount), cost \(.cost.amount), accumulated \(.accumulated.amount), book value \(.net_book_value.amount), gain \(.gain.amount) \(.gain.currency) (group \(.gain_gc.amount) \(.gain_gc.currency))"),
    "  entries \([.entries[] | "\(.ledger_code) \(.document_number)"] | join(", "))"' \
    createAssetRetirement -body "{\"asset_id\": \"$press\", \"kind\": \"sale\", \"proceeds\": {\"amount\": \"4000.00\", \"currency\": \"CAD\"}, \"share\": {\"percent\": \"50\"}, \"text\": \"Half the press sold\"}"
  sale=$(field .id)
  expect "the gain in every ledger" "$(field '[.ledgers[].gain.amount] | unique | join(" ")')" "250.00"
  expect "the leading ledger's catch-up, this period's run posted back" "$(field '.ledgers[] | select(.ledger_code == "0L") | .catch_up.amount')" "-62.50"
  expect "an entry in every ledger" "$(field '.entries | length')" "$(field '.ledgers | length')"
  api 200 '"entry \(.document_number) in the leading ledger: \(.leading_ledger); \([.lines[] | "\(.account_id | acct) \(.amount.amount)"] | join(", "))"' \
    getJournalEntry -p "id=$(field '.entries[] | select(.ledger_code == "0L") | .journal_entry_id')"
  api 200 "$values" getAsset -p "id=$press"
  expect "the half left" "$(field '"\(.status) \([.values[].cost.amount] | unique | join(" "))"')" "active 3750.00"
  say "The rest is scrapped: a full retirement, no proceeds, the book value left a loss. It retires the press."
  api 201 '"retirement \(.kind) of asset \(.asset_number), full \(.full): \(.status)", (.ledgers[] | "  \(.ledger_code): cost \(.cost.amount), book value \(.net_book_value.amount), loss \(.gain.amount) \(.gain.currency)")' \
    createAssetRetirement -body "{\"asset_id\": \"$press\", \"kind\": \"scrapping\", \"text\": \"Rest of the press scrapped\"}"
  scrap=$(field .id)
  expect "the loss in every ledger" "$(field '[.ledgers[].gain.amount] | unique | join(" ")')" "-3750.00"
  api 200 "$values" getAsset -p "id=$press"
  expect "the press, retired with nothing left" "$(field '"\(.status) \([.values[] | .cost.amount, .cost_gc.amount, .accumulated.amount, .accumulated_gc.amount] | unique | join(" "))"')" "retired 0.00"
  say "A retired asset takes no more documents:"
  refused 422 assets.asset_retired createAssetRetirement -body "{\"asset_id\": \"$press\", \"kind\": \"scrapping\"}"
  api 200 '"the press'"'"'s retirements: \([.items[] | "\(.kind) \(.status)"] | join(", "))"' listAssetRetirements -p "asset_id=$press"
  expect "the press's retirements" "$(field '[.items[].id] | join(" ")')" "$sale $scrap"
  passed
}

step_asset_transfer() {
  heading "Step 32 — Asset transfers: the milling machine moved to a cost center of another profit center from this period, its balances reposted; a quarter of it moved to a tool of another class"
  as sam
  say "Maple opens a Toronto office. Sam sets up its profit center and cost center, which the controller approves."
  local pc cc mill tool q start values cost before value
  api 201 '"profit center \(.code) \(.name): \(.status)"' createProfitCenter \
    -body "{\"code\": \"PC-TOR\", \"name\": \"Toronto office\", \"responsible\": \"Carla Controller\", \"legal_entity_ids\": [\"$LE\"], \"valid_from\": \"$YEAR-01-01\", \"reason\": \"The Toronto office\"}"
  pc=$(field .id)
  center_approved sam
  api 201 '"cost center \(.code) \(.name): \(.status)"' createCostCenter \
    -body "{\"legal_entity_id\": \"$LE\", \"code\": \"CC-2100\", \"name\": \"Toronto workshop\", \"responsible\": \"Carla Controller\", \"category\": \"ADMIN\", \"profit_center_id\": \"$pc\", \"valid_from\": \"$YEAR-01-01\", \"reason\": \"The Toronto workshop\"}"
  cc=$(field .id)
  center_approved sam
  as carla
  say "The milling machine moves to the Toronto workshop (ADR-0057 §8, T57h). A center transfer takes effect from the"
  say "first day of its posting period: each ledger's depreciation is first caught up through the period before on the"
  say "old centers (what this period's run already charged on them is posted back; step 29's run charged the lathe and"
  say "the press, not the machine, so its catch-up is zero here), and since the profit center changes, the machine's"
  say "booked cost and accumulated depreciation move to the new profit center on the same accounts."
  QUIET=1 api 200 '' listAssets -p "legal_entity_id=$LE" -p status=active
  mill=$(field '[.items[] | select(.description == "CNC milling machine") | .id][0]')
  values='"  asset \(.asset_number) \(.description) (\(.class_code)): \(.status) on \(.cost_center_code // "no cost center")/\(.profit_center_code // "-"); \([.values[] | "\(.ledger_code) cost \(.cost.amount), accumulated \(.accumulated.amount)"] | join("; "))"'
  api 200 "$values" getAsset -p "id=$mill"
  cost=$(field '[.values[] | "\(.ledger_code) \(.cost.amount)"] | join(", ")')
  QUIET=1 api 200 '' getAssetSchedule -p "id=$mill" -p "fiscal_year=$YEAR"
  start=$(field ".periods[] | select(.start_date <= \"$TODAY\" and .end_date >= \"$TODAY\") | .start_date")
  if [ "$start" != "$TODAY" ]; then
    say "A center transfer valued on another day of the period is refused:"
    refused 422 assets.transfer_mid_period createAssetTransfer \
      -body "{\"asset_id\": \"$mill\", \"kind\": \"center\", \"cost_center_id\": \"$cc\", \"value_date\": \"$TODAY\"}"
  fi
  api 201 '"transfer \(.kind) of asset \(.asset_number) from \(.value_date): \(.from.cost_center_code)/\(.from.profit_center_code) -> \(.to.cost_center_code)/\(.to.profit_center_code), profit center change \(.profit_center_change)",
    (.ledgers[] | "  \(.ledger_code): catch-up \(.catch_up.amount) on \(.catch_up_posting_date), cost moved \(.cost.amount), accumulated moved \(.accumulated.amount) \(.cost.currency)"),
    "  entries \([.entries[] | "\(.ledger_code) \(.part) \(.document_number)"] | join(", "))"' \
    createAssetTransfer -body "{\"asset_id\": \"$mill\", \"kind\": \"center\", \"cost_center_id\": \"$cc\", \"text\": \"Milling machine to Toronto\"}"
  q=$(field .id)
  expect "the transfer's value date, the period's first day" "$(field .value_date)" "$start"
  expect "the new centers" "$(field '"\(.to.cost_center_code) \(.to.profit_center_code) \(.profit_center_change)"')" "CC-2100 PC-TOR true"
  expect "the cost moved, all of the machine's" "$(field '[.ledgers[] | "\(.ledger_code) \(.cost.amount)"] | join(", ")')" "$cost"
  api 200 '"entry \(.document_number): \([.lines[] | "\(.account_id | acct) \(.amount.amount)"] | join(", "))"' \
    getJournalEntry -p "id=$(field '.entries[] | select(.ledger_code == "0L" and .part == "main") | .journal_entry_id')"
  api 200 "$values" getAsset -p "id=$mill"
  expect "the machine's centers" "$(field '"\(.cost_center_code) \(.profit_center_code)"')" "CC-2100 PC-TOR"
  say "A quarter of the machine, its tooling, becomes an asset of its own in the tools class (a reclassification):"
  say "the share of its cost and accumulated depreciation, after the catch-up, moves to a new tool asset, which becomes"
  say "active from the transfer's value date and depreciates what it received over its own life."
  api 201 '"asset \(.company_code)/\(.asset_number) \(.description) (\(.class_code)): \(.status)"' createAsset \
    -body "{\"legal_entity_id\": \"$LE\", \"class_code\": \"TOOLS\", \"description\": \"Milling tool set\", \"cost_center_id\": \"$cc\"}"
  tool=$(field .id)
  before=$(company_day)
  api 201 '"transfer \(.kind) of asset \(.asset_number) to \(.to_asset_number) \(.to_asset_description) (\(.to_class_code)), \(.share_percent) % from \(.value_date)",
    (.ledgers[] | "  \(.ledger_code): catch-up \(.catch_up.amount), cost \(.cost.amount), accumulated \(.accumulated.amount) \(.cost.currency)")' \
    createAssetTransfer -body "{\"asset_id\": \"$mill\", \"kind\": \"asset\", \"to_asset_id\": \"$tool\", \"share\": {\"percent\": \"25\"}, \"text\": \"Tooling split off the milling machine\"}"
  expect "the receiver's class" "$(field .to_class_code)" TOOLS
  value=$(field .value_date)
  expect_today "the transfer's value date, Maple's today" "$value" "$before"
  api 200 "$values" getAsset -p "id=$tool"
  expect "the tool, active from the transfer's value date" "$(field '"\(.status) \(.capitalised_on)"')" "active $value"
  api 200 "$values" getAsset -p "id=$mill"
  api 200 '"the machine'"'"'s transfers: \([.items[] | "\(.kind) \(.status)"] | join(", "))"' listAssetTransfers -p "asset_id=$mill"
  expect "the machine's transfers" "$(field '[.items[].kind] | sort | join(" ")')" "asset center"
  say "The center transfer is no longer the machine's latest posting, so it is not reversed any more:"
  QUIET=1 api 200 '' getAssetTransfer -p "id=$q"
  refused 409 assets.later_movements reverseAssetTransfer -p "id=$q" -if-match "$ETAG" -body '{"reason": "Moved in error"}'
  passed
}

# step_cash_orders is a released purchase order in the liquidity forecast:
# its open value with its tax, expected by the order's terms from the
# item's delivery date, drilled down to the order, as each reader may see
# it (T53g, ADR-0051 §5, decision P7).
step_cash_orders() {
  heading "Step 33 — Purchase orders in the liquidity forecast: an open order expected from its delivery date and terms"
  as ben
  local deliver due item po flows
  deliver=$(date_add "$TODAY" 20)
  due=$(date_add "$deliver" 30)
  item="{\"description\": \"Standing desks\", \"quantity\": {\"value\": \"4\", \"unit\": \"EA\"}, \"unit_price\": {\"amount\": \"500.00\", \"currency\": \"CAD\"}, \"account_id\": \"$(account 6000)\", \"tax_code\": \"HON\", \"delivery_date\": \"$deliver\"}"
  say "Ben orders four standing desks from Tyrell at CAD 500.00 each with Ontario HST (HON), for delivery on $deliver,"
  say "net 30: the item carries its delivery date (SAP's EKET-EINDT, T53g)."
  api 201 '"purchase order \(.document_number // .id): \(.status), \(.total_amount.amount) \(.total_amount.currency), terms \(.payment_term_code)", (.items[] | "  item \(.item_no): \(.quantity.value) \(.quantity.unit) at \(.unit_price.amount), \(.tax_code), delivery \(.delivery_date)")' \
    createPurchaseOrder -body "{\"legal_entity_id\": \"$LE\", \"supplier_id\": \"$SUPPLIER\", \"currency\": \"CAD\", \"order_date\": \"$TODAY\", \"payment_term_code\": \"NT30\", \"items\": [$item]}"
  po=$(field .id)
  expect "the item's delivery date" "$(field '.items[0].delivery_date')" "$deliver"
  api 200 '"\(.status)\(if .release_request_id then ": release request \(.release_request_id)" else "" end)"' releasePurchaseOrder -p "id=$po" -if-match "$ETAG"
  if [ "$(field .status)" = pending_approval ]; then
    approve priya "$(field .release_request_id)" "Budgeted desks"
    as ben
    QUIET=1 api 200 '' getPurchaseOrder -p "id=$po"
  fi
  expect "order status" "$(field .status)" released
  as tom
  say "Tom's forecast now has the order's open value (planned): what is not invoiced yet, CAD 2,000.00, plus the HST"
  say "its code gives on the read date, CAD 260.00, expected invoiced on the delivery date and paid by NT30 on Maple's"
  say "last payment day (Tuesday or Thursday) on or before $due, as a run would pay the invoice. He drills the weeks"
  say "from the delivery date down to the order:"
  api 200 '"  \(.count) flows, total \(.total.amount) \(.total.currency)", (.items[] | select(.source == "purchase_order") | "  \(.date) \(.source) \(.certainty) \(.share.amount): \([.documents[] | "\(.kind) \(.number // "-") \(.amount.amount)"] | join(", ")), supplier \(.supplier.name // "not shown")")' \
    listLiquidityForecastFlows -p "legal_entity_id=$LE" -p currency=CAD -p "from=$deliver" -p "to=$due" -p page_size=500
  cp "$WORK/last.json" "$WORK/order-flows.json"
  flows=$(field "[.items[] | select(.source == \"purchase_order\" and any(.documents[]; .id == \"$po\"))]")
  expect "the order's flows" "$(jq 'length' <<<"$flows")" 1
  expect "its amount, the open value with its tax" "$(jq -r '.[0].amount.amount' <<<"$flows")" "-2260.00"
  expect "its certainty" "$(jq -r '.[0].certainty' <<<"$flows")" planned
  expect "expected on the last payment day before it is due" \
    "$(jq -r --arg due "$due" --arg week "$(date_add "$due" -7)" '.[0].date | . <= $due and . > $week' <<<"$flows")" true
  expect "the order named, to read with getPurchaseOrder" "$(jq -r --arg po "$po" '.[0].documents[] | select(.id == $po) | .kind' <<<"$flows")" purchase_order
  say "The forecast's rows by liquidity item count it under Supplier payments (its item's account 6000 has no rule):"
  api 200 '.currencies[] | select(.currency == "CAD") | .items[] | select(.liquidity_item == "OP_SUPPLIERS") | (.flows[] | select(.source == "purchase_order") | "  CAD OP_SUPPLIERS \(.source) \(.certainty): \(.total.amount)")' \
    getLiquidityForecast -p "legal_entity_id=$LE"
  expect "the order in Supplier payments" \
    "$(field '[.currencies[] | select(.currency == "CAD") | .items[] | select(.liquidity_item == "OP_SUPPLIERS") | .flows[] | select(.source == "purchase_order")] | length')" 1
  as rita
  say "Rita holds the forecast's read alone: she sees the order's flow, not the order (procurement.order.read) nor Tyrell."
  api 200 '.items[] | select(.source == "purchase_order") | "  \(.date) \(.source) \(.share.amount): \(.documents | length) of \(.document_count) documents shown, supplier \(.supplier.name // "not shown")"' \
    listLiquidityForecastFlows -p "legal_entity_id=$LE" -p currency=CAD -p "from=$deliver" -p "to=$due" -p page_size=500
  expect "the orders shown to Rita" "$(field '[.items[] | select(.source == "purchase_order") | .documents[]] | length')" 0
  expect "the orders counted for Rita" "$(field '[.items[] | select(.source == "purchase_order") | .document_count] | add')" \
    "$(jq '[.items[] | select(.source == "purchase_order") | .document_count] | add' "$WORK/order-flows.json")"
  as tom
  passed
}

# sheet_reconciles WHAT checks the asset history sheet just read (ADR-0057
# §9; po:2026-11-09, B14; IAS 16.73(d), (e)): every row and the totals
# reconcile from opening to closing for cost and for accumulated
# depreciation, and each net book value is its cost plus its accumulated
# depreciation.
sheet_reconciles() {
  expect "$1: rows and totals that do not reconcile from opening to closing" "$(field 'def c: .amount | sub("\\."; "") | tonumber;
    [(.items[], .totals) | select(
      (.opening_cost | c) + (.acquisitions | c) + (.retirements | c) + (.transfers | c) != (.closing_cost | c)
      or (.opening_accumulated | c) + (.depreciation | c) + (.retirements_accumulated | c) + (.transfers_accumulated | c) != (.closing_accumulated | c)
      or (.opening_cost | c) + (.opening_accumulated | c) != (.opening_net_book_value | c)
      or (.closing_cost | c) + (.closing_accumulated | c) != (.closing_net_book_value | c))] | length')" 0
}

# step_asset_history_sheet reads the year's asset history sheet by class and by
# asset and checks its closing values against the trial balance (T57i,
# ADR-0057 §9), and that every row reconciles from opening to closing
# (po:2026-11-09, B14).
step_asset_history_sheet() {
  heading "Step 34 — The asset history sheet: the year's acquisitions, depreciation, retirements and transfers by class; its closing values the trial balance's asset accounts"
  as carla
  say "Carla reads this year's asset history sheet in the leading ledger (ADR-0057 §9, T57i): per class the cost at"
  say "the year's opening, the acquisitions, retirements and asset transfers of steps 22 to 32 and the closing cost, the"
  say "same for accumulated depreciation (negative, the credit it is), and the net book values. The postings keep its"
  say "year totals, so it reads a few rows per class rather than every movement."
  local row cols c ledger
  row='"  \(.class_code) \(.class_name // ""): cost \(.opening_cost.amount) + acquired \(.acquisitions.amount) + retired \(.retirements.amount) + transferred \(.transfers.amount) = \(.closing_cost.amount); accumulated depreciation \(.opening_accumulated.amount) + \(.depreciation.amount) + retired \(.retirements_accumulated.amount) + transferred \(.transfers_accumulated.amount) = \(.closing_accumulated.amount); net book value \(.closing_net_book_value.amount) \(.closing_net_book_value.currency)"'
  api 200 '"sheet of \(.company_code) in \(.ledger_code) \(.ledger_name), \(.start_date) to \(.end_date), by \(.group_by):", (.items[] | '"$row"'),
    (.totals | "  total: cost \(.closing_cost.amount), accumulated depreciation \(.closing_accumulated.amount), net book value \(.closing_net_book_value.amount)")' \
    getAssetHistorySheet -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR"
  ledger=$(field .ledger_id)
  expect "the classes" "$(field '[.items[].class_code] | join(" ")')" "MACHINES TOOLS"
  expect "the press's retirement in the machines' cost" "$(field '.items[] | select(.class_code == "MACHINES") | .retirements.amount | startswith("-")')" true
  expect "the tool set's cost, transferred from the milling machine" \
    "$(field '[.items[] | .transfers.amount] | join(" ")')" "$(field '"-\(.items[1].transfers.amount) \(.items[1].transfers.amount)"')"
  expect "the year's depreciation" "$(field '.totals.depreciation.amount | startswith("-")')" true
  say "Each class reconciles from opening to closing, for cost and for accumulated depreciation, and so do the totals"
  say "(IAS 16.73(d), (e); po:2026-11-09, B14): closing = opening + the year's columns, net book value = cost + accumulated."
  sheet_reconciles "by class"
  cp "$WORK/last.json" "$WORK/sheet.json"
  say "Only assets post to the acquisition cost and accumulated depreciation accounts (step 19), so the sheet's closing"
  say "values are those accounts' balances in the trial balance: debits less credits of every period so far (the"
  say "accounts are this year's; a catch-up may fall in the period before, so last year's are added)."
  accounts
  QUIET=1 api 200 '' listAssetClasses -p "legal_entity_id=$LE"
  cp "$WORK/last.json" "$WORK/classes.json"
  for c in acquisition_cost:closing_cost accumulated_depreciation:closing_accumulated; do
    local kind=${c%%:*} column=${c#*:} number balance=0 b y
    for number in $(jq -r "[.items[].companies[] | select(.company_code == \"$CC\") | .accounts.$kind.number] | unique | .[]" "$WORK/classes.json"); do
      b=0
      for y in $((YEAR - 1)) "$YEAR"; do
        QUIET=1 api 200 '' listBalances -p "legal_entity_id=$LE" -p "ledger_id=$ledger" -p "fiscal_year=$y" -p "account_id=$(account "$number")" -p page_size=500
        expect "one page of balances" "$(field '.next_page_token // "none"')" none
        b=$((b + $(field '[.items[] | (.debit_lc.amount | sub("\\."; "") | tonumber) - (.credit_lc.amount | sub("\\."; "") | tonumber)] | add // 0')))
      done
      say "  account $number: $(amount "$b") in the trial balance"
      balance=$((balance + b))
    done
    expect "the sheet's $column, the accounts' balance" "$(jq -r ".totals.$column.amount" "$WORK/sheet.json")" "$(amount "$balance")"
  done
  say "By asset, the same year: one row per asset by number with its class; the rows add up to the totals."
  api 200 '.items[] | "  \(.asset_number) \(.description) (\(.class_code), \(.status)): cost \(.closing_cost.amount), accumulated \(.closing_accumulated.amount), net \(.closing_net_book_value.amount)"' \
    getAssetHistorySheet -p "legal_entity_id=$LE" -p "fiscal_year=$YEAR" -p group_by=asset -p page_size=500
  expect "one page of assets" "$(field '.next_page_token // "none"')" none
  say "Every asset reconciles too: the press of step 31, disposed of this year, shows the cost and the accumulated"
  say "depreciation its retirement removed, and closes with neither:"
  field '.items[] | select(.status == "retired") | "  \(.asset_number) \(.description): cost \(.opening_cost.amount) + acquired \(.acquisitions.amount) + retired \(.retirements.amount) + transferred \(.transfers.amount) = \(.closing_cost.amount); accumulated depreciation \(.opening_accumulated.amount) + \(.depreciation.amount) + retired \(.retirements_accumulated.amount) + transferred \(.transfers_accumulated.amount) = \(.closing_accumulated.amount)"'
  expect "the retired assets' closing values" "$(field '[.items[] | select(.status == "retired") | "\(.closing_cost.amount) \(.closing_accumulated.amount)"] | unique | join(", ")')" "0.00 0.00"
  expect "an asset acquired this year: no opening cost, its acquisition its closing cost less what left it" \
    "$(field '[.items[] | select(.opening_cost.amount == "0.00" and .acquisitions.amount != "0.00")] | length > 0')" true
  sheet_reconciles "by asset"
  for cols in closing_cost closing_accumulated; do
    expect "the assets' $cols, the total" "$(field "[.items[].$cols.amount | sub(\"\\\\.\"; \"\") | tonumber] | add")" \
      "$(field ".totals.$cols.amount | sub(\"\\\\.\"; \"\") | tonumber")"
  done
  passed
}

# sftp_stub starts the stand-in for the treasury system's SFTP server
# (step 35): the test double of the SFTP transport on SFTP_ADDR, its files
# in memory, signing in the walkthrough's own key; a copy of every file it
# receives is left in $WORK/sftp/received for the walkthrough to read.
sftp_stub() {
  local stub="$BIN/sftpstub" i
  if [ ! -x "$stub" ]; then
    stub="$WORK/bin/sftpstub"
    (cd "$ROOT" && go build -o "$stub" ./internal/platform/sftpclient/sftptest/sftpstub) || fail "the SFTP stand-in did not build"
  fi
  mkdir -p "$WORK/sftp/received"
  rm -f "$WORK/sftp/client" "$WORK/sftp/client.pub" "$WORK/sftp/host.pub"
  ssh-keygen -q -t ed25519 -N '' -C erp -f "$WORK/sftp/client" || fail "no client key for the SFTP stand-in"
  "$stub" -addr "$SFTP_ADDR" -client-key "$WORK/sftp/client.pub" -host-key "$WORK/sftp/host.pub" -mkdir /treasury \
    -out "$WORK/sftp/received" >"$WORK/sftp.log" 2>&1 &
  SFTP_PID=$!
  for i in $(seq 1 100); do
    if [ -s "$WORK/sftp/host.pub" ]; then return 0; fi
    kill -0 "$SFTP_PID" 2>/dev/null || fail "the SFTP stand-in exited at start ($(cat "$WORK/sftp.log")): is $SFTP_ADDR free? set WALKTHROUGH_SFTP_ADDR"
    sleep 0.1
  done
  fail "the SFTP stand-in is not listening after ten seconds ($i checks)"
}

# wait_export waits for the export in the last answer to be delivered and
# stops unless it was; the delivered export is left in last.json.
wait_export() {
  local id status i
  id=$(field .id)
  for i in $(seq 1 240); do
    QUIET=1 api 200 '' getCashExport -p "id=$id"
    status=$(field .status)
    case "$status" in
    delivered) return 0 ;;
    failed) fail "export $id failed: $(field .last_error_code)" ;;
    esac
    sleep 0.25
  done
  fail "export $id still $status after a minute (attempt $i)"
}

# step_cash_export is the treasury connector (T53h, ADR-0051 §14): a
# snapshot exported in the cash export schema v1 to a treasury system's
# SFTP directory, the same snapshot sent again recognised as a resend, and
# the treasury system told of the snapshot by a signed webhook
# (CashSnapshotTaken, ADR-0041) that its endpoint verifies.
step_cash_export() {
  heading "Step 35 — The treasury connector: a snapshot exported to the treasury system over SFTP, sent again, and announced by webhook"
  local host port snap export first sum hook
  sftp_stub
  host=${SFTP_ADDR%:*} port=${SFTP_ADDR##*:}
  as sam
  say "Sam connects Maple's treasury system: an integration connector of kind treasury_export on the SFTP transport the"
  say "banks use (ADR-0038), the server's key pinned, signing in with a private key named by secret_ref, never a password."
  say "It takes Maple's snapshots (company 3000) in the cash export schema v1 as CSV (api/schemas/cash-export.v1.json)."
  api 201 '"connector \(.name): \(.kind), \(.status), \(.settings.format) of \(.settings.companies | join(", ")) to \(.settings.directory)"' \
    createConnector -body "{\"kind\": \"treasury_export\", \"name\": \"Maple treasury\", \"secret_ref\": \"file:$WORK/sftp/client\", \"settings\": {\"host\": \"$host\", \"port\": $port, \"user\": \"erp\", \"host_key\": \"$(tr -d '\n' <"$WORK/sftp/host.pub")\", \"directory\": \"/treasury\", \"format\": \"csv\", \"companies\": [\"$CC\"]}}"
  local connector
  connector=$(field .id)
  say "The treasury system also wants to hear of each snapshot as it is taken: Sam subscribes its endpoint to the"
  say "cash context's webhook events (ADR-0041), of which CashSnapshotTaken is the one offered. The answer shows the"
  say "subscription's whsec_ secret once; Sam gives it to the treasury system, which verifies every delivery with it."
  api 201 '"subscription to \(.event_types | join(", ")) at \(.url): \(.status)"' createWebhookSubscription \
    -body "{\"url\": \"https://$HOOK_ADDR/treasury/events\", \"description\": \"Maple treasury\", \"event_types\": [\"cash.*\"]}"
  hook=$(field .id)
  field .secret >"$WORK/hook/secret"
  expect "a whsec_ secret, shown once" "$(cut -c1-6 "$WORK/hook/secret")" "whsec_"
  forecast_receipt
  say "Tom takes the month-end snapshot. Taking it queues its export to every active treasury connector of Maple; the"
  say "server's exporter sends it in the background, as the bank file transmitter sends bank files:"
  QUIET=1 api 201 '' takeCashSnapshot -body "{\"legal_entity_id\": \"$LE\", \"name\": \"Month-end treasury report\"}"
  snap=$(field .id)
  api 200 '.items[] | "  export to \(.connector_name): \(.status), \(.format)"' listCashExports -p "cash_snapshot_id=$snap"
  expect "exports queued with the snapshot" "$(field '[.items[] | select(.connector_id == "'"$connector"'")] | length')" 1
  export=$(field '.items[0].id')
  printf '{"id": "%s"}\n' "$export" >"$WORK/last.json"
  wait_export
  excerpt '"  \(.status) after \(.attempts) attempt(s): \(.file_name)", "  sha256 \(.sha256), resend \(.resend)"'
  first=$(field .file_name)
  sum=$(field .sha256)
  [ -s "$WORK/sftp/received/$first" ] || fail "the treasury system did not receive $first"
  expect "the file's hash, its name's" "$(shasum -a 256 "$WORK/sftp/received/$first" | cut -d' ' -f1)" "$sum"
  expect "named by company, snapshot date and hash" "$first" "cash-$CC-$(field .snapshot_date)-$sum.csv"
  say "The treasury system's file, its first lines (per day and bank within 31 days, per bucket beyond; no documents):"
  head -n 4 "$WORK/sftp/received/$first" | tr -d '\r' | sed 's/^/     /'
  expect "the CSV's header" "$(head -n 1 "$WORK/sftp/received/$first" | tr -d '\r' | cut -d, -f1-3)" "schema_version,company_code,snapshot_id"
  say "The treasury system lost the file; Tom sends the same snapshot again. A snapshot never changes, so it is the same"
  say "file under the same name: delivered, and marked a resend, which the treasury system recognises by its name."
  api 201 '"export \(.status) to \(.connector_name)"' createCashExport -p "id=$snap" -body "{\"connector_id\": \"$connector\"}"
  wait_export
  excerpt '"  \(.status): \(.file_name), resend \(.resend)"'
  expect "the same file name" "$(field .file_name)" "$first"
  expect "the same hash" "$(field .sha256)" "$sum"
  expect "recognised as a resend" "$(field .resend)" true
  cash_webhook "$hook" "$snap"
  passed
}

# forecast_receipt has Tom record a receipt before the month-end snapshot,
# on a day step 38's metrics read that snapshot for: a week from today,
# else a month (30 days), else today, the first of them that is one of
# Maple's working days, since the forecast moves a flow off any other day
# (ADR-0051 §5). Without it the snapshot has a flow on one of those days
# only by the weekday the walkthrough runs on and none on a holiday
# (Remembrance Day 2026, ci:batch64). FORECAST_RECEIPT_DAY is that day:
# empty when none of the three is a working day (25 December 2026: New
# Year's Day a week on, a Sunday a month on), and then nothing is
# recorded. Maple's calendar, as the active configuration holds it, is
# kept in calendar.json for step 38. The record names no house bank, so
# the cash position does not count it.
forecast_receipt() {
  local today
  as ada
  QUIET=1 api 200 '' listConfigVersions -p is_active=true
  field "[.items[0].document.calendars[]? | select(.company == \"$CC\")][0] // {}" >"$WORK/calendar.json"
  as tom
  today=$(company_day)
  FORECAST_RECEIPT_DAY=$(first_working_day "$WORK/calendar.json" "$today" 7 30 0) || fail "Maple's calendar could not be read for its working days"
  if [ -z "$FORECAST_RECEIPT_DAY" ]; then
    say "Neither today ($today), a week nor a month from it is one of Maple's working days: Tom expects no receipt then."
    return 0
  fi
  say "Tom expects a customer's CAD 1,750.00 on $FORECAST_RECEIPT_DAY, one of Maple's working days (a week from today, else a"
  say "month, else today), and records it before the month-end snapshot (a memo record without a house bank):"
  api 201 '"memo record \(.reference): \(.amount.amount) \(.amount.currency) on \(.value_date), \(.liquidity_item), \(.status)"' createMemoRecord \
    -body "{\"legal_entity_id\": \"$LE\", \"amount\": {\"amount\": \"1750.00\", \"currency\": \"CAD\"}, \"value_date\": \"$FORECAST_RECEIPT_DAY\", \"liquidity_item\": \"REC_CUSTOMERS\", \"description\": \"Lakefield Supply, invoice 1044\", \"reference\": \"WT-1044\"}"
  expect "the receipt's status" "$(field .status)" active
}

# cash_webhook HOOK SNAPSHOT shows the webhook announcing the snapshot:
# delivered as the subscription's deliveries show it, received by the
# treasury system's endpoint (webhookstub), its signature verified there
# and refused for a body changed by one byte.
cash_webhook() {
  local hook=$1 snap=$2 i file='' delivery event
  say "Taking the snapshot published CashSnapshotTaken through the outbox; the event bus carried it to the webhook"
  say "fan-out, which queued a delivery to the subscription, and the dispatcher sent it, signed with its secret:"
  for i in $(seq 1 240); do
    QUIET=1 api 200 '' listWebhookDeliveries -p "id=$hook"
    if [ "$(field '[.items[] | select(.subject == "'"$snap"'" and .status == "delivered")] | length')" = 1 ]; then break; fi
    sleep 0.25
  done
  api 200 '.items[] | select(.subject == "'"$snap"'") | "  \(.type): \(.status) after \(.attempts) attempt(s), the endpoint answered \(.last_status)"' \
    listWebhookDeliveries -p "id=$hook"
  expect "the snapshot's delivery, delivered (after $i checks)" "$(field '[.items[] | select(.subject == "'"$snap"'" and .status == "delivered")] | length')" 1
  delivery=$(field '.items[] | select(.subject == "'"$snap"'") | .id')
  event=$(field '.items[] | select(.subject == "'"$snap"'") | .event_id')
  for file in "$WORK"/hook/received/delivery-*.json; do
    [ -e "$file" ] || break # none received: the glob is left as written, and the check below says so
    if [ "$(jq -r .webhook_delivery_id "$file")" = "$delivery" ]; then break; fi
  done
  [ -s "$file" ] && [ "$(jq -r .webhook_delivery_id "$file")" = "$delivery" ] || fail "the treasury system's endpoint did not receive delivery $delivery"
  say "What the treasury system's endpoint received (a CloudEvent, application/cloudevents+json):"
  jq -r '"  webhook-id \(.webhook_id), webhook-timestamp \(.webhook_timestamp)",
    "  webhook-signature \(.webhook_signature | .[0:16])…",
    "  \(.event.type) for snapshot \(.event.data.cash_snapshot_id) of company \(.event.data.company_code) on \(.event.data.snapshot_date)"' "$file" | sed 's/^/   /'
  expect "the webhook-id, the event's id" "$(jq -r .webhook_id "$file")" "$event"
  expect "the event's type" "$(jq -r .event.type "$file")" "com.example.erp.cash.cash_snapshot_taken.v1"
  expect "the snapshot it names" "$(jq -r .event.data.cash_snapshot_id "$file")" "$snap"
  expect "its content type" "$(jq -r .content_type "$file")" "application/cloudevents+json"
  say "The endpoint verified the signature with the secret Sam gave it (Standard Webhooks, as api/webhooks-test-vectors.json"
  say "fixes it) and refused the same signature over the body with one byte changed:"
  jq -r '"  verified \(.verified), a changed body refused \(.tampered_body_refused)"' "$file" | sed 's/^/   /'
  expect "the signature verified" "$(jq -r .verified "$file")" true
  expect "a changed body refused" "$(jq -r .tampered_body_refused "$file")" true
}

step_cash_group() {
  heading "Step 36 — The group liquidity forecast: the companies a treasurer may act in, summed in group currency"
  as ada
  local us cal cory receipt weeks own usd maple_own maple_usd sum=0
  weeks=(-p grain=week -p "to=$(date_add "$TODAY" 55)")
  usd='[.currencies[] | select(.currency == "USD") | .total.amount, .beyond.amount.amount] | map(sub("\\."; "") | tonumber) | add // 0'
  own='.converted | [.opening.amount, .total.amount, .beyond.amount] | map(sub("\\."; "") | tonumber) | add'
  say "The group takes on its US sister, Acme Corp US (SAP company 1000, in USD): Ada creates its legal entity and"
  say "activates it."
  QUIET=1 api 200 '' getLegalEntity -p "id=$LE"
  cal=$(field .fiscal_calendar_id)
  api 201 '"legal entity \(.code) \(.name): \(.country_code), \(.local_currency), \(.status)"' createLegalEntity \
    -body "{\"code\": \"1000\", \"name\": \"Acme Corp US\", \"country_code\": \"US\", \"local_currency\": \"USD\", \"fiscal_calendar_id\": \"$cal\"}"
  us=$(field .id)
  api 200 '"legal entity \(.code): \(.status)"' activateLegalEntity -p "id=$us" -if-match "$ETAG"
  as tom
  receipt=$(date_add "$TODAY" 20)
  say "Tom expects a US customer's USD 3,000.00 at Acme Corp US on $receipt (a memo record without a house bank):"
  api 201 '"memo record \(.reference): \(.amount.amount) \(.amount.currency) on \(.value_date), \(.liquidity_item)"' \
    createMemoRecord -body "{\"legal_entity_id\": \"$us\", \"amount\": {\"amount\": \"3000.00\", \"currency\": \"USD\"}, \"value_date\": \"$receipt\", \"liquidity_item\": \"REC_CUSTOMERS\", \"description\": \"Acme Corp US, order 12\", \"reference\": \"WT-3001\"}"
  say "He reads the forecast across the companies he may act in (ADR-0051 §9, §10, §15): each company's own forecast"
  say "per currency, summed, and in the group currency, each company converted as its own forecast converts it:"
  api 200 '"  in \(.group_currency) from \(.read_date): \(.buckets | length) \(.buckets[0].grain)s",
      (.companies[] | "  company \(.company_code) \(.name): read \(.read_date) in \(.time_zone); rates \([.rates[] | "\(.currency) \(.rate_type) \(.rate)"] | join(", ")); without a rate: \(.incomplete_currencies | join(", "))"),
      (.currencies[] | "  \(.currency): opens at \(.opening.amount.amount), \(.total.amount) over the horizon, \(.beyond.amount.amount) after it"),
      "  converted: opens at \(.converted.opening.amount), closes at \(.converted.buckets[-1].closing.amount) \(.converted.currency)"' \
    getGroupLiquidityForecast "${weeks[@]}"
  cp "$WORK/last.json" "$WORK/group.json"
  say "Acme GmbH (2000) is among them: the legal entity step 3 recorded for the migration, which holds no cash."
  expect "the companies, by code" "$(field '[.companies[].company_code] | join(" ")')" "1000 2000 $CC"
  expect "the group currency" "$(field .group_currency)" CAD
  expect "the converted closing, the converted opening plus the converted totals" \
    "$(field 'def c: sub("\\."; "") | tonumber; .converted | ((.opening.amount | c) + ([.buckets[].total.amount | c] | add)) == (.buckets[-1].closing.amount | c)')" true
  say "The group's figures are the companies' own over the same weeks, summed: per currency, and converted into CAD:"
  api 200 '"  \(.legal_entity_id): converted opening \(.converted.opening.amount), total \(.converted.total.amount), beyond \(.converted.beyond.amount) \(.converted.currency)"' \
    getLiquidityForecast -p "legal_entity_id=$us" -p display_currency=CAD "${weeks[@]}"
  sum=$((sum + $(field "$own")))
  expect "Acme Corp US's USD, its receipt" "$(field "$usd")" 300000
  api 200 '"  \(.legal_entity_id): converted opening \(.converted.opening.amount), total \(.converted.total.amount), beyond \(.converted.beyond.amount) \(.converted.currency)"' \
    getLiquidityForecast -p "legal_entity_id=$LE" -p display_currency=CAD "${weeks[@]}"
  maple_own=$(field "$own")
  sum=$((sum + maple_own))
  maple_usd=$(field "$usd")
  expect "the group's USD, Maple's and Acme Corp US's" "$(jq "$usd" "$WORK/group.json")" "$((maple_usd + 300000))"
  expect "the group's converted opening, total and beyond, the companies' summed" "$(jq "$own" "$WORK/group.json")" "$sum"
  as ada
  say "Cory in reporting holds the forecast's read (cash_reporting) in Maple alone: Acme Corp US is not his, so it"
  say "reaches nothing he reads, neither a company, a currency nor a total."
  QUIET=1 api 201 '' createPrincipal -body '{"kind": "human", "external_subject": "cory", "display_name": "Cory Reporting", "all_companies": false}'
  cory=$(field .id)
  QUIET=1 api 201 '' createRoleAssignment -body "{\"principal_id\": \"$cory\", \"role_id\": \"$(role cash_reporting)\"}"
  QUIET=1 api 201 '' createCompanyAccess -body "{\"principal_id\": \"$cory\", \"legal_entity_id\": \"$LE\"}"
  token cory "Cory Reporting"
  printf '%s\tCory Reporting\n' "$cory" >>"$WORK/names"
  as cory
  api 200 '"  companies \([.companies[].company_code] | join(", ")); currencies \([.currencies[].currency] | join(", ")); converted total \(.converted.total.amount) \(.converted.currency)"' \
    getGroupLiquidityForecast "${weeks[@]}"
  expect "Cory's companies" "$(field '[.companies[].company_code] | join(" ")')" "$CC"
  expect "Cory's USD, Maple's alone" "$(field "$usd")" "$maple_usd"
  expect "Cory's converted opening, total and beyond, Maple's alone" "$(field "$own")" "$maple_own"
  passed
}

# step_cash_analytics is the cash actuals as a governed metric of the
# semantic layer (T53j1, ADR-0044 As built T53j1): the same statement lines
# as step 27, classified by the cash context, broken down by currency,
# liquidity item and what classified them; a reader of the forecast alone
# runs it without the suppliers' read and is refused supplier data.
step_cash_analytics() {
  heading "Step 37 — Cash actuals in analytics: the week's statement lines through the governed metric actual"
  as tom
  local from moved q sum
  from=$(date_add "$TODAY" -6)
  moved=$(amount $(($(cents "$CLOSING") - $(cents 500000.00))))
  q="{\"measures\": [\"actual\"], \"by\": [\"currency\", \"liquidity_item\", \"match\"], \"companies\": [\"$CC\"], \"where\": [{\"dim\": \"value_date\", \"op\": \"between\", \"values\": [\"$from\", \"$TODAY\"]}], \"order_by\": \"liquidity_item\"}"
  # The CAD cells of the measure (the last column), in cents, added up.
  sum='[.rows[] | select(.[0].value == "CAD") | .[-1].value | sub("\\."; "") | tonumber] | add'
  say "Tom asks the semantic layer for what actually moved on Maple's accounts from $from to $TODAY: the metric"
  say "actual, the statement lines' shares by liquidity item as step 27 classified them, by currency (Maple's house"
  say "banks keep CAD and USD), item and what classified each line:"
  api 200 '"answered from \(.source) as of \(.as_of), \(.columns | map(.name) | join(", "))", (.rows[] | "  " + (map(.value // "-") | join("  ")))' \
    runAnalyticsQuery -body "$q"
  expect "the CAD actuals, step 27's total" "$(amount "$(field "$sum")")" "$moved"
  expect "every line classified by its payment" "$(field '[.rows[] | select(.[0].value == "CAD") | .[2].value] | unique | join(" ")')" payment
  expect "the measure's definition" "$(field '.definitions[0].name')" actual
  as ada
  say "Ada lets Rita, who holds the forecast's read alone, run governed queries over the companies she reads"
  say "(analytics.query.run, tenancy.org.read); she still holds no partner.supplier.read:"
  grant "$(role cash_reporting)" analytics.query.run tenancy.org.read
  as rita
  say "Rita reads the same actuals: they name no supplier, so the suppliers' read is not needed (ADR-0044 As built T53j1)."
  api 200 '(.rows[] | "  " + (map(.value // "-") | join("  ")))' runAnalyticsQuery -body "$q"
  expect "Rita's CAD actuals, Tom's" "$(amount "$(field "$sum")")" "$moved"
  say "Supplier invoices by vendor are another context's data, read with its own permission: she is refused them."
  refused 403 identity.permission_missing runAnalyticsQuery -body "{\"measures\": [\"invoice_value\"], \"by\": [\"company\", \"vendor_name\"], \"companies\": [\"$CC\"]}"
  as tom
  passed
}

# step_cash_forecast_analytics is the forecast as governed metrics of the
# semantic layer (T53j2, ADR-0044 As built T53j2): each value date's flows
# as Maple's snapshot of the day the lag before kept them, here today's
# (the last taken on demand, step 35's, or the daily one when the
# walkthrough runs after Maple's snapshot time, which is then read instead),
# at lags 0, 7 and 30, each the snapshot's own lines of that day, blocked
# suppliers' flows left out; then its variance and accuracy (T53j3,
# forecast_accuracy_analytics); Rita, who holds the forecast's read alone,
# reads the same.
step_cash_forecast_analytics() {
  heading "Step 38 — The forecast in analytics: what today's snapshot expected for today, a week and a month ahead, and how accurate it was today"
  as tom
  local snap kind other day lag q kept got seen=0 total=0 preferred=0 on_demand=0 snapshot_day
  # Today is the day step 35's snapshot was taken on, the server's: a run
  # that crossed Toronto's midnight after the script fixed TODAY took it on
  # the next day (ci:batch56).
  QUIET=1 api 200 '' listCashSnapshots -p "legal_entity_id=$LE" -p kind=on_demand
  snapshot_day=$(field '[.items[] | select(.name == "Month-end treasury report")][0].snapshot_date')
  [ "$snapshot_day" != null ] || fail "step 35's snapshot, the Month-end treasury report, is not listed"
  say "The variance report compares one snapshot over its period. The governed forecast metrics read each value date"
  say "from the snapshot taken that many days before it (0, 7 or 30): the company's daily snapshot of that date, else the"
  say "last one taken on demand that day. Today's ($snapshot_day) is the one Tom's metrics read for today, in a week and in a month:"
  api 200 '.items[] | "  \(.snapshot_date) \(.kind) \(.name // "") taken \(.taken_at)"' listCashSnapshots -p "legal_entity_id=$LE" -p "from=$snapshot_day" -p "to=$snapshot_day"
  snap=$(field '([.items[] | select(.kind == "daily")] + .items)[0].id')
  kind=$(field '([.items[] | select(.kind == "daily")] + .items)[0].kind')
  other=$(field '[.items[] | select(.kind == "on_demand")][0].id')
  say "The metrics read today's $kind snapshot."
  QUIET=1 api 200 '' listCashSnapshotFlows -p "id=$snap" -p currency=CAD -p page_size=500
  cp "$WORK/last.json" "$WORK/forecast_lines.json"
  QUIET=1 api 200 '' listCashSnapshotFlows -p "id=$other" -p currency=CAD -p page_size=500
  cp "$WORK/last.json" "$WORK/forecast_other.json"
  for lag in 0 7 30; do
    day=$(date_add "$snapshot_day" "$lag")
    kept=$(kept_on "$WORK/forecast_lines.json" "$day")
    q="{\"measures\": [\"forecast_lag_$lag\"], \"by\": [\"currency\", \"value_date\", \"flow_source\", \"certainty\", \"liquidity_item\"], \"companies\": [\"$CC\"], \"order_by\": \"value_date\"}"
    say "forecast_lag_$lag, by currency, value date, flow source, certainty and liquidity item: $day is today's snapshot's day $lag."
    api 200 '"answered from \(.source) as of \(.as_of)", (.rows[] | "  " + (map(.value // "-") | join("  ")))' runAnalyticsQuery -body "$q"
    got=$(cad_on "$day")
    expect "the CAD forecast of $day at lag $lag, the snapshot's lines of that day" "$(amount "$got")" "$(amount "$kept")"
    expect "lag $lag: today's snapshot's other days left out" "$(field "[.rows[] | select(.[1].value != \"$day\")] | length")" 0
    if [ "$kept" != 0 ]; then seen=1; fi
    total=$((total + got)) preferred=$((preferred + kept)) on_demand=$((on_demand + $(kept_on "$WORK/forecast_other.json" "$day")))
  done
  expect "the measure's definition" "$(field '.definitions[0].name')" forecast_lag_30
  if [ "$kind" = daily ] && [ "$preferred" != "$on_demand" ]; then
    # Run after Maple's snapshot time, the daily snapshot was taken early in the story, before most of its flows.
    say "Today has a daily snapshot, taken before most of the story's flows: the metrics read it ($(amount "$preferred") over the three"
    say "days), not the one taken on demand later ($(amount "$on_demand")), as the variance would compare against it."
    expect "the three days' CAD forecast, the daily snapshot's" "$(amount "$total")" "$(amount "$preferred")"
  else
    forecast_seen "$snapshot_day" "$seen"
  fi
  forecast_accuracy_analytics "$snapshot_day"
  as rita
  say "Rita, who holds the forecast's read alone, reads the day-ahead forecast and its accuracy: they name no supplier"
  say "(ADR-0044 As built T53j2, T53j3)."
  q="{\"measures\": [\"forecast_lag_0\"], \"by\": [\"currency\", \"value_date\"], \"companies\": [\"$CC\"]}"
  api 200 '(.rows[] | "  " + (map(.value // "-") | join("  ")))' runAnalyticsQuery -body "$q"
  expect "Rita's CAD forecast of $snapshot_day, Tom's" "$(amount "$(cad_on "$snapshot_day")")" "$(amount "$(kept_on "$WORK/forecast_lines.json" "$snapshot_day")")"
  api 200 '(.rows[] | "  " + (map(.value // "-") | join("  ")))' runAnalyticsQuery -body "$ACCURACY_TODAY"
  expect "Rita's CAD accuracy of $snapshot_day, Tom's" "$(field '[.rows[] | select(.[0].value == "CAD") | .[4].value // "-"][0]')" "$ACCURACY_CAD"
  as tom
  passed
}

# forecast_seen DAY SEEN checks that the three reads of step 38 compared
# something: SEEN is 1 when the snapshot of DAY has a CAD line on DAY, a
# week or a month (30 days) after it. The forecast moves every flow to a
# working day of Maple's calendar (blocked suppliers', which the metrics
# leave out, apart), so the snapshot has a line there only on a working
# day: step 35's receipt WT-1044, recorded for the first of the three that
# is one, and none when none is. Only when Toronto's midnight passed
# between the receipt and its snapshot is the receipt on another day, and
# nothing told.
forecast_seen() {
  local day=$1 seen=$2 working lag on=0
  working=$(first_working_day "$WORK/calendar.json" "$day" 0 7 30) || fail "Maple's calendar could not be read for its working days"
  for lag in 0 7 30; do
    if [ "$(date_add "$day" "$lag")" = "$FORECAST_RECEIPT_DAY" ]; then on=1; fi
  done
  if [ -z "$working" ]; then
    say "Neither $day, a week nor a month from it is one of Maple's working days, and the forecast moves every flow to"
    say "one: today's snapshot has nothing on the three days."
    expect "today's snapshot forecasts none of those days, none a working day" "$seen" 0
  elif [ "$on" = 1 ]; then
    expect "today's snapshot forecasts one of those days, the receipt WT-1044's ($FORECAST_RECEIPT_DAY)" "$seen" 1
    expect "WT-1044's line in today's snapshot, on its day" \
      "$(jq -r --arg d "$FORECAST_RECEIPT_DAY" '[.items[] | select(.source == "memo" and .value_date == $d and .amount.amount == "1750.00")] | length' "$WORK/forecast_lines.json")" 1
  else
    say "Toronto's midnight passed between the receipt WT-1044 (step 35, for ${FORECAST_RECEIPT_DAY:-no day}) and its snapshot of $day: the"
    say "receipt is on none of the snapshot's three days, so whether one of them has a line is the weekday's doing."
  fi
}

# forecast_accuracy_analytics DAY is the forecast's variance and accuracy
# as governed metrics (T53j3, ADR-0044 As built T53j3; product owner
# 2026-11-04): DAY's (today's) actuals against its snapshot at lag 0, in cells
# of a currency and liquidity item, checked by hand against the variance
# report of the day (step 28's): each item's variance is the report's, its
# accuracy 100% less the report's variance_percent (none where nothing was
# forecast), and the day's accuracy 1 less WAPE, the items' absolute
# variances over their absolute forecasts rounded to two decimals of a
# percentage half away from zero. No snapshot was taken a week or a month
# ago, so no day is compared at lags 7 and 30.
forecast_accuracy_analytics() {
  local day=$1 report ours abs_v=0 abs_f=0 want v f item var pct line
  say "The variance report compares today's snapshot with today's statement lines, item by item (step 28):"
  api 200 '.currencies[] | select(.currency == "CAD") | .items[] | "  \("  " * .level)\(.liquidity_item // "unclassified"): \(.total.forecast.amount) -> \(.total.actual.amount), variance \(.total.variance.amount) (\(.total.variance_percent // "-")%)"' \
    getLiquidityForecastVariance -p "legal_entity_id=$LE" -p "from=$day" -p "to=$day" -p lag=0
  # The report's leaf rows of CAD (an item's row is followed by its
  # children's, one level deeper), each item's own cell, of something
  # forecast or paid.
  report=$(field '[.currencies[] | select(.currency == "CAD") | .items as $it | range(0; $it | length) as $i
    | select($i + 1 == ($it | length) or $it[$i + 1].level <= $it[$i].level) | $it[$i]
    | select(.total.forecast.amount != "0.00" or .total.variance.amount != "0.00")
    | "\(.liquidity_item // "")|\(.total.forecast.amount)|\(.total.variance.amount)|\(.total.variance_percent // "-")"] | .[]')
  local -a mine=()
  while IFS='|' read -r item f var pct; do
    [ -n "$item$f" ] || continue
    v=$(cents "$var") f=$(cents "$f")
    abs_v=$((abs_v + (v < 0 ? -v : v))) abs_f=$((abs_f + (f < 0 ? -f : f)))
    if [ "$pct" = - ]; then mine+=("$item $var -"); else mine+=("$item $var $(ratio4 $((10000 - $(cents "$pct"))))"); fi
  done <<<"$report"
  say "Tom asks the semantic layer for the variance and accuracy of today's forecast at lag 0 (variance_lag_0, accuracy_lag_0),"
  say "by currency and liquidity item: the actual less the forecast, and 100% less the variance's size in percent of the"
  say "forecast's, as the report's variance_percent; none where nothing was forecast:"
  api 200 '"answered from \(.source) as of \(.as_of), \(.columns | map(.name) | join(", "))", (.rows[] | "  " + (map(.value // "-") | join("  ")))' \
    runAnalyticsQuery -body "{\"measures\": [\"variance_lag_0\", \"accuracy_lag_0\"], \"by\": [\"currency\", \"liquidity_item\"], \"companies\": [\"$CC\"], \"where\": [{\"dim\": \"value_date\", \"op\": \"in\", \"values\": [\"$day\"]}]}"
  ours=$(field '.rows[] | select(.[0].value == "CAD") | select(.[2].value != "0.00" or .[3].value != null) | "\(.[1].value // "") \(.[2].value) \(.[3].value // "-")"' | sort)
  expect "each item's variance and accuracy, the report's" "$ours" "$(printf '%s\n' ${mine[@]+"${mine[@]}"} | sort)"
  if [ "$abs_f" = 0 ]; then
    want=-
  else
    want=$(ratio4 $((10000 - (2 * abs_v * 10000 + abs_f) / (2 * abs_f))))
  fi
  ACCURACY_TODAY="{\"measures\": [\"variance_lag_0\", \"absolute_variance_lag_0\", \"absolute_forecast_lag_0\", \"accuracy_lag_0\"], \"by\": [\"currency\"], \"companies\": [\"$CC\"], \"where\": [{\"dim\": \"value_date\", \"op\": \"in\", \"values\": [\"$day\"]}]}"
  say "The day's accuracy is 1 less WAPE: the items' absolute variances, $(amount "$abs_v") CAD, over their absolute forecasts,"
  say "$(amount "$abs_f") CAD, the quotient rounded to two decimals of a percentage first: $want."
  api 200 '(.rows[] | "  " + (map(.value // "-") | join("  ")))' runAnalyticsQuery -body "$ACCURACY_TODAY"
  line=$(field '[.rows[] | select(.[0].value == "CAD")][0]')
  expect "today's absolute variance, by hand" "$(jq -r '.[2].value' <<<"$line")" "$(amount "$abs_v")"
  expect "today's absolute forecast, by hand" "$(jq -r '.[3].value' <<<"$line")" "$(amount "$abs_f")"
  ACCURACY_CAD=$(jq -r '.[4].value // "-"' <<<"$line")
  expect "today's accuracy, 1 less WAPE by hand" "$ACCURACY_CAD" "$want"
  say "No snapshot was taken a week or a month before today, so no day is compared at those lags (the report refuses them):"
  api 200 '"  \(.rows | length) rows"' runAnalyticsQuery -body "{\"measures\": [\"accuracy_lag_7\", \"accuracy_lag_30\"], \"companies\": [\"$CC\"]}"
  expect "no day compared a week or a month ahead" "$(field '.rows | length')" 0
}

# ratio4 N is N ten-thousandths as a ratio with four decimals, "-0.5000".
ratio4() {
  local n=$1 sign=''
  if [ "$n" -lt 0 ]; then sign=- n=$((-n)); fi
  printf '%s%d.%04d' "$sign" "$((n / 10000))" "$((n % 10000))"
}

# kept_on FILE DAY is the CAD cents a snapshot's page of lines (FILE)
# forecast for DAY at document level, blocked suppliers' flows left out.
kept_on() {
  jq -r --arg d "$2" 'def c: sub("\\."; "") | tonumber;
    [.items[] | select(.bucket_end == null and .value_date == $d and .source != "payable_blocked") | .amount.amount | c] | add // 0' "$1"
}

# cad_on DAY is the CAD cents of the last query's rows of DAY (currency
# the first column, value date the second, the measure the last).
cad_on() {
  field "[.rows[] | select(.[0].value == \"CAD\" and .[1].value == \"$1\") | .[-1].value | sub(\"\\\\.\"; \"\") | tonumber] | add // 0"
}

# name_partners replaces the business partners' ids in what it reads with
# their names, as the API gives them.
name_partners() {
  local line id
  while IFS= read -r line; do
    for id in $(printf '%s' "$line" | grep -oE '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' || true); do
      line=${line//$id/$("$ERP" call getBusinessPartner -p "id=$id" 2>/dev/null | sed 1d | jq -r '.name // empty')}
    done
    printf '%s\n' "$line"
  done
}

# name_actors replaces principals' ids with their names in what it reads.
name_actors() {
  local line id name
  while IFS= read -r line; do
    while IFS=$'\t' read -r id name; do line=${line//$id/$name}; done <"$WORK/names"
    printf '%s\n' "$line"
  done
}

# step_cash_forecast_agent is the forecast agent (T53i, ADR-0051 §13): erp
# agent cash-variance on today's variance at lag 0, through the model
# stand-in answering from a script of turns, as its evaluation suite
# replays them (evals/cash). Northwind's receipt WT-1042, forecast for
# three days on (step 13), arrives today on a new RBC statement, which Tom
# realises it by; Lakeshore Rentals has paid USD 3,200.00 on the first of
# each of the last three months at MUSD, which no memo record forecasts,
# and an insurer refunded USD 900.00 once. The agent proposes a comment on
# the row the receipt falls in, citing the memo record and the statement
# the variance's documents list (a citation of a document not behind that
# row is refused), and a memo record for Lakeshore's next receipt citing
# its three lines; nothing for the one-off refund or for any other row.
# Tom accepts both: the agent cannot. Run after the cash steps, since a
# new statement line today changes the day's actuals that steps 27, 28 and
# 37 compare.
step_cash_forecast_agent() {
  heading "Step 39 — The forecast agent: today's variance explained citing its documents, a monthly receipt proposed as a memo record, nothing else"
  local role memo statement musd history p1 p2 p3 next docs row attribution got session comment record run_payment
  as ada
  say "Ada sets up the forecast agent (agent type cash_forecast, ADR-0051 §13): it reads the forecast, its variance and the"
  say "documents behind it, the statements, memo records and payment runs, and proposes comments and memo records for"
  say "people to decide. It holds no partner.supplier.read, so it names suppliers by their documents."
  api 201 '"role \(.code), \(.name)"' createRole -body '{"code": "forecast_agent", "name": "Forecast agent"}'
  role=$(field .id)
  grant "$role" tenancy.org.read cash.forecast.read cash.memo.read cash.memo.propose cash.variance.propose payments.statement.read \
    payments.run.read agents.session.read agents.session.record agents.model.invoke
  api 201 '"\(.kind) \(.display_name) of type \(.agent_profile.agent_type), level \(.agent_profile.autonomy_level) on \(.agent_profile.model), budget \(.agent_profile.monthly_budget.amount) \(.agent_profile.monthly_budget.currency) a month"' createPrincipal \
    -body '{"kind": "agent", "external_subject": "forecast-agent", "display_name": "Forecast agent", "all_companies": true, "agent_profile": {"autonomy_level": 1, "agent_type": "cash_forecast", "model": "erp-default", "description": "Explains the liquidity forecast variance for people to decide (ADR-0051 §13)", "monthly_budget": {"amount": "5.00", "currency": "USD"}}}'
  QUIET=1 api 201 '' createRoleAssignment -body "{\"principal_id\": \"$(field .id)\", \"role_id\": \"$role\"}"
  token forecast-agent "Forecast agent"
  as tom
  QUIET=1 api 200 '' listMemoRecords -p "legal_entity_id=$LE" -p page_size=200
  memo=$(field '[.items[] | select(.reference == "WT-1042")][0].id')
  [ -n "$memo" ] && [ "$memo" != null ] || fail "step 13's memo record WT-1042 is not there"
  say "Northwind's CAD 25,000.00 (WT-1042, step 13, expected on $(field '[.items[] | select(.reference == "WT-1042")][0].value_date')) arrives today. RBC's"
  say "second statement of the day shows it; Tom imports it and realises the memo record by its line:"
  rbc_receipt
  api 201 '.statements[] | "statement \(.statement_ref): \(.line_count) line, closing \(.closing_balance.amount) \(.closing_balance.currency)"' \
    importBankStatements -body "{\"house_bank_id\": \"$RBC\", \"format\": \"camt.053\"}" -file file=@camt053-receipt.xml
  statement=$(field '.statements[0].id')
  QUIET=1 api 200 '' getMemoRecord -p "id=$memo"
  api 200 '"memo record \(.reference): \(.status), by line \(.realised_by.line_no // "-")"' realiseMemoRecord -p "id=$memo" -if-match "$ETAG" \
    -body "{\"statement_id\": \"$statement\", \"line_no\": 1}"
  expect "WT-1042 realised" "$(field .status)" realised
  QUIET=1 api 200 '' listHouseBanks -p "legal_entity_id=$LE"
  musd=$(field '.items[] | select(.code == "MUSD") | .id')
  p1=$(date_add "${TODAY:0:8}01" -1)
  p1=${p1:0:8}01
  p2=$(date_add "$p1" -1)
  p2=${p2:0:8}01
  p3=$(date_add "$p2" -1)
  p3=${p3:0:8}01
  next=$(date_add "${TODAY:0:8}01" 32)
  next=${next:0:8}01
  say "MUSD's statement of the last three months: Lakeshore Rentals' sublease of USD 3,200.00 on $p3, $p2 and $p1, and"
  say "an insurer's refund of USD 900.00 once. No memo record forecasts Lakeshore's next receipt."
  musd_history "$p3" "$p2" "$p1"
  api 201 '.statements[] | "statement \(.statement_ref): \(.line_count) lines, closing \(.closing_balance.amount) \(.closing_balance.currency)"' \
    importBankStatements -body "{\"house_bank_id\": \"$musd\", \"format\": \"camt.053\"}" -file file=@camt053-musd.xml
  history=$(field '.statements[0].id')
  say "The documents behind today's attributed variance (listForecastVarianceDocuments, lag 0), as Tom may see them:"
  api 200 '.items[] | "  \(.side) \(.attribution) \(.currency) \(.liquidity_item // "unclassified") \(.value_date) \(.amount.amount): \([.documents[] | "\(.kind) \(.number // "-")"] | join(", "))"' \
    listForecastVarianceDocuments -p "legal_entity_id=$LE" -p "from=$TODAY" -p "to=$TODAY" -p lag=0 -p page_size=500
  expect "every document behind the day's variance on one page" "$(field '.count')" "$(field '.items | length')"
  docs="$WORK/forecast-documents.json"
  cp "$WORK/last.json" "$docs"
  row=$(jq -r --arg m "$memo" '[.items[] | select(.side == "actual" and any(.documents[]; .id == $m))][0].liquidity_item // ""' "$docs")
  attribution=$(jq -r --arg m "$memo" '[.items[] | select(.side == "actual" and any(.documents[]; .id == $m))][0].attribution // ""' "$docs")
  [ -n "$attribution" ] || fail "the receipt is not behind today's variance"
  run_payment=$(jq -r '[.items[] | .documents[] | select(.kind == "payment") | .id][0] // ""' "$docs")
  say "The receipt is $attribution in ${row:-the unclassified row}. The agent runs: erp agent cash-variance starts a session on"
  say "erp-default, reads through its tools and proposes. The model is the stand-in, answering from a script of turns as the"
  say "agent's evaluation suite replays them (evals/cash): it reads the variance, its documents, the memo records and"
  say "MUSD's statements; cites a payment of the supplier row under the receipt's row (refused: not behind it); then"
  say "proposes the comment citing WT-1042 and today's statement, and Lakeshore's next receipt on $next citing its lines."
  cash_agent_script "$memo" "$statement" "$history" "$musd" "$row" "$attribution" "$next" "$run_payment"
  show ERP_AGENT_TOKEN=… erp agent cash-variance -company "$CC" -period "$TODAY..$TODAY" -lag 0
  got=$(ERP_AGENT_TOKEN=$(cat "$WORK/tokens/forecast-agent") "$ERP" agent cash-variance -company "$CC" -period "$TODAY..$TODAY" -lag 0 2>"$WORK/stderr") ||
    fail "erp agent cash-variance failed ($(cat "$WORK/stderr"))"
  rm -f "$WORK/model-script.json"
  printf '%s\n' "$got" | sed 's/^/     /'
  session=$(printf '%s\n' "$got" | sed -nE '1s/^Session ([0-9a-f-]{36}): .*/\1/p')
  expect "what the agent made" "$(printf '%s\n' "$got" | sed -nE '1s/^Session [0-9a-f-]{36}: //p')" \
    "comments proposed 1, memo records proposed 1, refused $([ -n "$run_payment" ] && echo 1 || echo 0)."
  comment=$(printf '%s\n' "$got" | sed -nE 's/^  comment ([0-9a-f-]{36})$/\1/p')
  record=$(printf '%s\n' "$got" | sed -nE 's/^  memo record ([0-9a-f-]{36})$/\1/p')
  say "Tom reads the proposals: the comment, with its citations, and nothing on any other row of the day:"
  api 200 '.items[] | "  \(.liquidity_item // "unclassified") \(.from) lag \(.lag): \(.status), citing \([.citations[].kind] | join(", "))\n    \"\(.text)\""' \
    listForecastVarianceComments -p "legal_entity_id=$LE" -p status=proposed
  expect "the day's proposals, the one comment" "$(field '[.items[].id] | join(" ")')" "$comment"
  expect "on the receipt's row" "$(field '.items[0].liquidity_item // ""')" "$row"
  expect "citing WT-1042 and today's statement" "$(field '[.items[0].citations[] | "\(.kind) \(.id)"] | sort | join(", ")')" \
    "bank_statement $statement, memo_record $memo"
  expect "in the session the agent named" "$(field '.items[0].session_id')" "$session"
  api 200 '.items[] | "  memo record: \(.status), \(.amount.amount) \(.amount.currency) on \(.value_date) at \(.house_bank_code // "-"), \(.liquidity_item), evidence lines \([.evidence[].line_no] | join(", "))"' \
    listMemoRecords -p "legal_entity_id=$LE" -p status=proposed
  expect "the one memo record proposed, Lakeshore's next" "$(field '[.items[] | "\(.id) \(.amount.amount) \(.value_date) \([.evidence[].line_no] | join(","))"] | join(" ")')" \
    "$record 3200.00 $next 1,2,4"
  as forecast-agent
  say "The agent may not decide its own proposal (x-agent: false):"
  QUIET=1 api 200 '' getForecastVarianceComment -p "id=$comment"
  refused 403 identity.agent_not_allowed acceptForecastVarianceCommentProposal -p "id=$comment" -if-match "$ETAG"
  api 200 '"  session \(.id[0:8])… \(.status) on \(.model): \(.purpose)"' getAgentSession -p "id=$session"
  api 200 '"  its tool calls: \([.items[].tool] | join(", "))"' listAgentToolCalls -p "id=$session"
  as tom
  say "Tom accepts both as they stand: the comment explains the row, and Lakeshore's receipt now counts."
  QUIET=1 api 200 '' getForecastVarianceComment -p "id=$comment"
  api 200 '"  comment: \(.status), decided by Tom"' acceptForecastVarianceCommentProposal -p "id=$comment" -if-match "$ETAG"
  expect "the comment" "$(field .status)" accepted
  QUIET=1 api 200 '' getMemoRecord -p "id=$record"
  api 200 '"  memo record: \(.status)"' acceptMemoRecordProposal -p "id=$record" -if-match "$ETAG" -body '{}'
  expect "the memo record" "$(field .status)" active
  passed
}

# rbc_receipt writes RBC's second camt.053 statement of today: Northwind's
# CAD 25,000.00, from step 8's closing balance.
rbc_receipt() {
  local closing
  closing=$(amount $(($(cents "$CLOSING") + 2500000)))
  {
    printf '<?xml version="1.0" encoding="UTF-8"?>\n'
    printf '<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.02"><BkToCstmrStmt>\n'
    printf '<GrpHdr><MsgId>RBC-%s-2</MsgId><CreDtTm>%sT23:30:00</CreDtTm></GrpHdr>\n' "$TODAY" "$TODAY"
    printf '<Stmt><Id>RBC-1000001-%s-2</Id><ElctrncSeqNb>2</ElctrncSeqNb><Acct><Id><Othr><Id>1000001</Id></Othr></Id><Ccy>CAD</Ccy></Acct>\n' "$TODAY"
    printf '<Bal><Tp><CdOrPrtry><Cd>OPBD</Cd></CdOrPrtry></Tp><Amt Ccy="CAD">%s</Amt><CdtDbtInd>CRDT</CdtDbtInd><Dt><Dt>%s</Dt></Dt></Bal>\n' "$CLOSING" "$TODAY"
    printf '<Bal><Tp><CdOrPrtry><Cd>CLBD</Cd></CdOrPrtry></Tp><Amt Ccy="CAD">%s</Amt><CdtDbtInd>CRDT</CdtDbtInd><Dt><Dt>%s</Dt></Dt></Bal>\n' "$closing" "$TODAY"
    printf '<Ntry><Amt Ccy="CAD">25000.00</Amt><CdtDbtInd>CRDT</CdtDbtInd><Sts>BOOK</Sts><BookgDt><Dt>%s</Dt></BookgDt><ValDt><Dt>%s</Dt></ValDt><BkTxCd><Domn><Cd>PMNT</Cd><Fmly><Cd>RCDT</Cd><SubFmlyCd>ESCT</SubFmlyCd></Fmly></Domn></BkTxCd><NtryDtls><TxDtls><RltdPties><Dbtr><Nm>Northwind Traders</Nm></Dbtr></RltdPties><RmtInf><Ustrd>Invoice 1042</Ustrd></RmtInf></TxDtls></NtryDtls></Ntry>\n' "$TODAY" "$TODAY"
    printf '</Stmt></BkToCstmrStmt></Document>\n'
  } >"$WORK/camt053-receipt.xml"
}

# musd_history P3 P2 P1 writes MUSD's camt.053 statement of the three
# months: Lakeshore's sublease on each day, the insurer's refund on P2.
musd_history() {
  local d entry='<Ntry><Amt Ccy="USD">%s</Amt><CdtDbtInd>CRDT</CdtDbtInd><Sts>BOOK</Sts><BookgDt><Dt>%s</Dt></BookgDt><ValDt><Dt>%s</Dt></ValDt><BkTxCd><Domn><Cd>PMNT</Cd><Fmly><Cd>RCDT</Cd><SubFmlyCd>ESCT</SubFmlyCd></Fmly></Domn></BkTxCd><NtryDtls><TxDtls><RltdPties><Dbtr><Nm>%s</Nm></Dbtr></RltdPties><RmtInf><Ustrd>%s</Ustrd></RmtInf></TxDtls></NtryDtls></Ntry>\n'
  {
    printf '<?xml version="1.0" encoding="UTF-8"?>\n'
    printf '<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.02"><BkToCstmrStmt>\n'
    printf '<GrpHdr><MsgId>MUSD-%s</MsgId><CreDtTm>%sT23:00:00</CreDtTm></GrpHdr>\n' "$3" "$3"
    printf '<Stmt><Id>MUSD-5012346-%s</Id><ElctrncSeqNb>1</ElctrncSeqNb><Acct><Id><Othr><Id>5012346</Id></Othr></Id><Ccy>USD</Ccy></Acct>\n' "$3"
    printf '<Bal><Tp><CdOrPrtry><Cd>OPBD</Cd></CdOrPrtry></Tp><Amt Ccy="USD">0.00</Amt><CdtDbtInd>CRDT</CdtDbtInd><Dt><Dt>%s</Dt></Dt></Bal>\n' "$1"
    printf '<Bal><Tp><CdOrPrtry><Cd>CLBD</Cd></CdOrPrtry></Tp><Amt Ccy="USD">10500.00</Amt><CdtDbtInd>CRDT</CdtDbtInd><Dt><Dt>%s</Dt></Dt></Bal>\n' "$3"
    # shellcheck disable=SC2059 # the entry is the format
    printf "$entry" 3200.00 "$1" "$1" "Lakeshore Rentals" "Sublease $1"
    # shellcheck disable=SC2059
    printf "$entry" 3200.00 "$2" "$2" "Lakeshore Rentals" "Sublease $2"
    # shellcheck disable=SC2059
    printf "$entry" 900.00 "$2" "$2" "Ontario Mutual Insurance" "Claim 7781 refund"
    # shellcheck disable=SC2059
    printf "$entry" 3200.00 "$3" "$3" "Lakeshore Rentals" "Sublease $3"
    printf '</Stmt></BkToCstmrStmt></Document>\n'
  } >"$WORK/camt053-musd.xml"
}

# cash_agent_script MEMO STATEMENT HISTORY MUSD ROW ATTRIBUTION NEXT
# PAYMENT writes the stand-in's turns of the forecast agent's run: its
# reads, a citation of a payment under the receipt's row (when the day
# has one, refused: not behind that row), the comment and the memo
# record.
cash_agent_script() {
  local item
  item=$([ -n "$5" ] && printf '{"liquidity_item": "%s"}' "$5" || printf '{"unclassified": true}')
  jq -n --arg le "$LE" --arg day "$TODAY" --arg memo "$1" --arg st "$2" --arg hist "$3" --arg musd "$4" --arg att "$6" --arg next "$7" \
    --arg pay "$8" --argjson row "$item" '
    {legal_entity_id: $le, from: $day, to: $day, lag: 0} as $v |
    ({body: ($v + $row + {session_id: "{{session}}", citations: [{kind: "memo_record", id: $memo}, {kind: "bank_statement", id: $st}],
      text: "Northwind Traders paid its invoice 1042, CAD 25,000.00, today (line 1 of RBC'"'"'s second statement), three days before memo record WT-1042 expected it: \($att) in this row."})}) as $comment |
    {vars: {session: "session id is ([0-9a-f-]{36})"},
     turns: ([
      {tool: "getLiquidityForecastVariance", input: $v},
      {tool: "listForecastVarianceDocuments", input: $v},
      {tool: "listMemoRecords", input: {legal_entity_id: $le}},
      {tool: "listBankStatements", input: {house_bank_id: $musd}},
      {tool: "getBankStatement", input: {id: $hist}}]
      + (if $pay == "" then [] else [{tool: "proposeForecastVarianceComment",
          input: ($comment | .body.citations = [{kind: "payment", id: $pay}])}] end)
      + [{tool: "proposeForecastVarianceComment", input: $comment},
      {tool: "proposeMemoRecord", input: {body: {session_id: "{{session}}", legal_entity_id: $le, house_bank_id: $musd,
        amount: {amount: "3200.00", currency: "USD"}, value_date: $next, liquidity_item: "REC_CUSTOMERS", certainty: "expected",
        description: "Lakeshore Rentals, sublease", reference: "LAKESHORE-NEXT",
        reason: "Lakeshore Rentals paid USD 3,200.00 for its sublease on the first of each of the last three months; no memo record forecasts the next.",
        evidence: [{statement_id: $hist, line_no: 1}, {statement_id: $hist, line_no: 2}, {statement_id: $hist, line_no: 4}]}}},
      {final: "Proposed a comment on the receipt'"'"'s row and Lakeshore'"'"'s next sublease as a memo record. The insurer'"'"'s refund was once: nothing proposed. Every other row of the day is left to people."}])}' \
    >"$WORK/model-script.json"
}

STORY='step_tenant step_pack step_migrate step_procure step_invoices step_credit step_payment step_statement step_fx step_ledgers step_tax step_centers step_cash step_reports step_grir step_downpayment step_audit step_capture step_assets step_asset_master step_cash_items step_asset_acquisitions step_cash_forecast step_asset_reversal step_asset_depreciation step_cash_snapshot step_cash_actuals step_cash_variance step_depreciation_run step_cash_variance_comment step_asset_retirement step_asset_transfer step_cash_orders step_asset_history_sheet step_cash_export step_cash_group step_cash_analytics step_cash_forecast_analytics step_cash_forecast_agent step_consolidation step_uae step_year_end step_einvoice step_asset_estimate step_asset_held_for_sale step_asset_order_expenses step_customer step_customer_invoice step_internal_order step_order_budget step_order_settlement step_distribution step_assessment step_incoming_payment step_customer_credit step_ar_aging step_dunning step_collections step_credit_limit step_rfq step_catalog step_quotation step_sales_order step_billing step_price_list step_rfq_award step_revenue_contract step_credit_block step_scheduling_agreement step_revenue_schedule step_close'

main() {
  stop_unseen
  start
  if [ -e "$WORK/failed" ]; then exit 1; fi
  local s stopped='' before
  for s in $STORY; do
    CURRENT=$s before=${#STEPS[@]}
    "$s"
    ran "$s" "$before"
    if [ "${WALKTHROUGH_UNTIL:-}" = "$s" ]; then
      stopped=$s
      break
    fi
  done
  summary "$stopped"
  finished
  if [ "${WALKTHROUGH_KEEP:-0}" = 1 ]; then
    say ""
    say "The server keeps running for a look around (Ctrl-C stops it):"
    say "  export ERP_API_URL=$ERP_API_URL ERP_ADMIN_URL=$ERP_ADMIN_URL"
    say "  export ERP_TOKEN=\$(cat $WORK/tokens/carla)   # or ada, ben, alex, priya, sam, tom, tara"
    say "  $ERP call listSupplierInvoices"
    wait "$SERVER_PID" || true # the server killed: the run ends as its summary did; Ctrl-C ends it non-zero after cleanup
  fi
}

main "$@"
