#!/usr/bin/env bash
# The pilot walkthrough (make walkthrough): a Canadian company's story told
# against the real binaries, for the product owner to watch. It builds
# erp-server and erp, starts the server with the development issuer on
# DATABASE_URL (migrated on start), provisions a tenant from the mock SAP
# extract and drives everything else through the API with `erp call`, as
# the people of the story. No test harness and no mocks beyond the mock SAP
# extract and a stand-in for the model provider (internal/llm/llmtest/
# modelstub), which answers a capture's extraction as the uploaded document
# prints it, without a live model or its key. A step that needs a
# capability not built yet says so and is skipped.
#
# Each step prints a heading, the commands it runs and an excerpt of the
# answers, and checks what it expects: any unexpected answer stops the run
# with the reason, the answer and the server's last log lines. The summary
# at the end lists the steps that passed and those skipped.
#
#   WALKTHROUGH_PAUSE=1   wait for Enter after each step
#   WALKTHROUGH_API_ADDR, WALKTHROUGH_ADMIN_ADDR   listeners (127.0.0.1:18080, 127.0.0.1:19090)
#   WALKTHROUGH_MODEL_ADDR   the model stand-in's listener (127.0.0.1:18081)
#   WALKTHROUGH_DIR       work directory (default: a temporary one, removed after)
#   WALKTHROUGH_BIN       directory with erp and erp-server already built (default: build them)
#   WALKTHROUGH_KEEP=1    keep the server running at the end, to look around with erp call
#   WALKTHROUGH_UNTIL     stop after this step (its function name, e.g. step_migrate)
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
PAUSE="${WALKTHROUGH_PAUSE:-0}"
# shellcheck source=walkthrough/dates.sh
. "$ROOT/scripts/walkthrough/dates.sh"
TODAY=$(company_day)
# A payment run is dated TODAY, the company's day. The migration (step 3)
# loads the suppliers' bank accounts valid from the day before UTC's
# (partner.Migration's one-day exemption from the backdating control,
# ADR-0051 As built; PO 2026-11-02), never later than the company's day in
# Toronto, so a Toronto evening's run finds them, on 31 December too.
YEAR=${TODAY:0:4}
PREV=$((YEAR - 1))
CC=3000 # the pilot's SAP company code, Maple Industries Canada in the mock extract

if [ -t 1 ]; then
  B=$'\033[1m' DIM=$'\033[2m' RED=$'\033[31m' GREEN=$'\033[32m' YELLOW=$'\033[33m' R=$'\033[0m'
else
  B='' DIM='' RED='' GREEN='' YELLOW='' R=''
fi

for tool in jq curl; do
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
SERVER_PID=''
MODEL_PID=''

cleanup() {
  if [ -n "$MODEL_PID" ]; then
    kill "$MODEL_PID" 2>/dev/null || true # it may have exited already: then there is nothing to stop
    wait "$MODEL_PID" 2>/dev/null || true # its exit status after a kill says nothing about the walkthrough
  fi
  if [ -n "$SERVER_PID" ]; then
    kill "$SERVER_PID" 2>/dev/null || true # it may have exited already: then there is nothing to stop
    wait "$SERVER_PID" 2>/dev/null || true # its exit status after a kill says nothing about the walkthrough
  fi
  if [ -z "${WALKTHROUGH_DIR:-}" ]; then rm -rf "$WORK"; fi
}
trap cleanup EXIT

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

fail() {
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

summary() {
  local i n=0 s=0
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
  out=$("$ERP" call "$@" 2>"$WORK/stderr") || true # 4xx answers exit 1: the status is checked below
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
    BIN=$(cd "$WALKTHROUGH_BIN" && pwd) || fail "no directory $WALKTHROUGH_BIN"
  else
    BIN="$WORK/bin"
    say "Building erp-server and the erp command line from this checkout..."
    (cd "$ROOT" && go build -o "$BIN/erp" ./cmd/erp && go build -o "$BIN/erp-server" ./cmd/erp-server) || fail "the build failed"
  fi
  ERP="$BIN/erp"
  say "Version: $(cd "$ROOT" && git describe --tags --always --dirty 2>/dev/null || echo unknown) (git describe)"
  # Files the commands name are the work directory's, as they are shown.
  cd "$WORK"
  say "Today is $TODAY, Maple's date in $ZONE: the pilot's documents are dated today."
  say ""
  say "Not built yet, so not shown:"
  say "  - the web screens: only the workspace skeleton exists, sign-in comes with T73b, the screens with T73c-T73m"
  say "  - the GST34's lines (T61j-T61l): step 2 configures one"
  say "    sales code and the GST/HST registration; step 11 shows a pack tax code and prepares, approves and files returns in order"
  say "    whose boxes are illustrative, not the GST34's"
  say "  - the liquidity forecast's purchase orders and variance comments (T53f4-T53k): step 13 shows the daily cash"
  say "    position with a memo record and the forecast of the supplier open items, payment runs, parked invoices, down"
  say "    payment requests and blocked suppliers; step 21 its rows by liquidity item; step 23 its opening, memo records"
  say "    and payments in transit, conversion and drill-down; step 26 its snapshots; step 27 the cash actuals;"
  say "    step 28 the forecast's variance"
  say "  - cost center postings, reports and planning (T56d-T56j)"
  say "  - asset retirements, transfers and the rest of asset accounting (T57g-T57i): steps 19 and 20 show the classes and"
  say "    the master, steps 22 and 24 acquisitions and their corrections, step 25 the depreciation schedule and preview, step 29"
  say "    a depreciation run"
  say "  - accounts receivable, the period close beyond its checklist, automatic checks, variances and sign-off (T64e-T64g, T64i-T64k), consolidation (T68)"
  say "  - natural-language questions run on a live model (erp ask); here the same question is asked as a governed query"

  export DATABASE_URL ERP_ENV=development ERP_DEV_ISSUER=true ERP_MIGRATE_ON_START=true
  export ERP_HTTP_ADDR="$API_ADDR" ERP_ADMIN_ADDR="$ADMIN_ADDR"
  export ERP_API_URL="http://$API_ADDR" ERP_ADMIN_URL="http://$ADMIN_ADDR" ERP_CONTROL_URL="http://$ADMIN_ADDR"
  export ERP_EXTRACT_DIR="$WORK/data" ERP_OBJECT_DIR="$WORK/data/objects"
  model_stub
  illustrative_pack
  export ERP_PACKS_DIR="$WORK/packs"
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
}

# model_stub starts the stand-in for the model provider (step 18's capture)
# and points erp-server's model gateway at it. It answers every call with
# the text of model-answer.json, which step 18 writes.
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
  "$stub" -addr "$MODEL_ADDR" -answer "$WORK/model-answer.json" >"$WORK/model.log" 2>&1 &
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

# illustrative_pack copies the latest released Canada pack to the next
# version with the illustrative return of scripts/walkthrough/returns.yaml
# (T61h), for erp-server to serve unreleased (ERP_PACKS_DIR, development
# only): the released packs define no return until T61j. CA_PACK is its
# version. The copy drops the released pack's sign_off (T62h): the tax
# specialist signed off the released pack, not the illustrative return.
illustrative_pack() {
  local latest
  latest=$(cd "$ROOT/packs/ca" && ls -d 20*.* | sort -t. -k1,1n -k2,2n | tail -1)
  CA_PACK="${latest%%.*}.$((${latest#*.} + 1))"
  mkdir -p "$WORK/packs/ca"
  cp -R "$ROOT/packs/ca/$latest" "$WORK/packs/ca/$CA_PACK"
  sed -e "s/^version: \"$latest\"$/version: \"$CA_PACK\"/" \
    -e "s/^name: Canada$/name: \"Canada $latest with the walkthrough's illustrative return (not released)\"/" \
    -e "/^sign_off:/d" \
    "$ROOT/packs/ca/$latest/pack.yaml" >"$WORK/packs/ca/$CA_PACK/pack.yaml"
  grep -q "^version: \"$CA_PACK\"$" "$WORK/packs/ca/$CA_PACK/pack.yaml" || fail "the illustrative pack's version was not set"
  ! grep -q "sign_off\|tax_specialist" "$WORK/packs/ca/$CA_PACK/pack.yaml" || fail "the illustrative pack kept the released pack's sign-off"
  cp "$ROOT/scripts/walkthrough/returns.yaml" "$WORK/packs/ca/$CA_PACK/returns.yaml"
}

# ---------------------------------------------------------------- the people

# The permissions of everyone's day-to-day work: all but administering
# access and the organisation, activating configuration, the AI policy,
# releasing payments and deciding approvals, which the approver roles hold.
STAFF_DENIED=' identity.access.manage tenancy.org.manage config.change.activate agents.ai_policy.manage agents.model.invoke payments.run.release payments.file.transmit workflow.request.decide '

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
  show erp config generate -source file:data/extract.json -company "$CC" -year "$YEAR" -time-zone "$CC=America/Toronto" -out config.yaml
  if "$ERP" config generate -source "file:$WORK/data/extract.json" -company "$CC" -year "$YEAR" -time-zone "$CC=America/Toronto" \
    -out "$WORK/config.yaml" >"$WORK/generate.out" 2>&1; then
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
  parallel_run_extract
  small_item_refused "$connector"
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
    'Open GR/IR total' 'Open GR/IR in USD total' 'Open GR/IR: order items with differences'; do
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
  migrated_down_payment
  migrated_foreign_item
  migrated_grir
  migrated_foreign_grir
  passed
}

# migrated_foreign_item shows SAP's open invoice in USD of the CAD company
# (the mock's foreignInvoice) as it loaded, in its document currency
# (ADR-0005 As built, product owner 2026-10-31): an open item of USD
# 1,000.00 worth CAD 1,350.00 as SAP booked it, its entry in USD at the
# rate SAP's amounts imply, and the reconciliation's USD controls. Step 9
# revalues it. It keeps the item's entry in FX_ENTRY.
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

# closed_order_extract makes an order of the pilot's that SAP received and
# invoiced in full one valued through GR/IR (review:batch23), in both
# extracts: the first single-item order received in one receipt and
# invoiced in one posted invoice (CLOSED, its item's price CLOSED_PRICE)
# gets a receipt document debiting the item's account and crediting the
# GR/IR account (GR_IR account $1), and its invoice debits the GR/IR
# account instead of the item's, both on the order item; so the first run
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
    def we(belnr; date): (.BELNR = belnr | .BLART = "WE" | .XBLNR = "" | .BUDAT = date)
      | (.DOCLN = "000001"), (.DOCLN = "000002" | .RACCT = $a | .HSL |= neg | .WSL |= neg);
    ([.EKPO[] | select(.EBELN == $o)] | first) as $i
    | ([.MSEG[] | select(.EBELN == $o)] | first) as $m
    | ([.RSEG[] | select(.EBELN == $o)] | first) as $s
    | ([.RBKP[] | select(.BELNR == $s.BELNR and .GJAHR == $s.GJAHR)] | first) as $r
    | ([.ACDOCA[] | select(.RBUKRS == $cc and .BLART == "RE" and .XBLNR == $r.XBLNR and .RACCT == $i.SAKTO)] | first) as $x
    | ($x | .EBELN = $o | .EBELP = $i.EBELP) as $line
    | .EKPO = [.EKPO[] | if .EBELN == $o then .MENGE = ((.MENGE | tonumber) + 1 | tostring) else . end]
    | .MSEG += [$m + {MBLNR: "5099999998", MENGE: "1", BUDAT: $d}]
    | .ACDOCA = [.ACDOCA[] | if . == $x then $line | .RACCT = $a else . end]
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
# https://www.canada.ca/en/revenue-agency/services/tax/businesses/topics/gst-hst-businesses/charge-collect-which-rate.html).
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
    .chart_of_accounts |= map(select(.number != "1400"))
    | .chart_of_accounts += [
      {"number": "1010", "name": "Bank clearing - RBC CAD", "type": "asset", "open_items": true},
      {"number": "1015", "name": "Bank incoming - RBC CAD", "type": "asset", "open_items": true},
      {"number": "1020", "name": "Bank clearing - RBC USD", "type": "asset", "open_items": true, "revalue_foreign_currency": true},
      {"number": "1030", "name": "Bank - RBC USD", "type": "asset", "revalue_foreign_currency": true},
      {"number": "1040", "name": "Bank incoming - RBC USD", "type": "asset", "open_items": true, "revalue_foreign_currency": true}]
    | .tax_codes = ((.tax_codes // []) + [{"code": "GO5", "description": "GST 5% on sales (walkthrough; the Canada pack codes purchases)",
        "direction": "output", "kind": "standard", "category": "S", "components": [{"key": "gst", "jurisdiction": "CA",
        "account_key": "output", "account": "2300", "rates": [{"rate_percent": "5", "valid_from": "@PREV@-01-01"}]}]}])
    | .house_banks = ((.house_banks // []) + [
      {"name": "RBC", "bic": "ROYCCAT2", "institution_number": "003", "transit_number": "00002", "account_number": "1000001",
       "gl_account": "1010", "main_gl_account": "1000", "incoming_gl_account": "1015"},
      {"name": "RBCUS", "bic": "ROYCCAT2", "institution_number": "003", "transit_number": "00002", "account_number": "4000001",
       "gl_account": "1020", "main_gl_account": "1030", "incoming_gl_account": "1040", "currency": "USD"}])
    | .tax_registrations = [{"code": "GSTHST", "authority": "CRA", "number": "123456789 RT 0001", "return_type": "GST34",
        "frequency": "quarterly", "first_period_start": "@TAXFIRST@"}]
    | .cash = {"payment_days": ["tue", "thu"]}
    | .release_dual_limit = "4000.00"
    | .service_posting_limit = "1000.00"
    | .fx_valuation.accounts += (["1020", "1030", "1040"] | map({"account": ., "unrealised_gain": "4900", "unrealised_loss": "6900"}))
    | .bank_statements = {"matching_rules": [{"kind": "end_to_end_id"}, {"kind": "amount", "date_window_days": 5, "counterparty": true}],
        "approval_tiers": [{"up_to": "10000.00", "steps": []}, {"steps": [{"role": "controller", "approvals": 1}]}]})'

step_pack() {
  heading "Step 2 — The Canada country pack and the pilot's settings, before migration, through a governed change request"
  as ada
  api 200 '.items[] | "pack \(.country) \(.version): \(.contents.accounts) accounts, \(.contents.tax_codes) tax codes, \(.contents.holidays) holidays, \(.contents.number_ranges) number ranges; signed off: \(.signed_off)"' listCountryPacks
  local version
  version=$(field '[.items[] | select(.country == "CA") | .version] | sort_by(split(".") | map(tonumber)) | last')
  expect "the latest Canada pack" "$version" "$CA_PACK"
  say "CA $version is this walkthrough's own, not released: the latest released Canada pack with an illustrative"
  say "tax return definition (returns.yaml) for step 11, as no released pack defines a return until T61j. The copy"
  say "drops the released pack's sign-off, as the tax specialist did not review its return."
  say "Ada applies the latest Canada pack, $version, to company $CC. The merge keeps what the company"
  say "already has (SAP's chart and number ranges) and adds the rest; it becomes a draft change request."
  api 201 '"change request \(.number), \(.status): \(.title)"' applyCountryPack \
    -body "{\"country\": \"CA\", \"version\": \"$version\", \"company\": \"$CC\", \"applied_on\": \"$TODAY\", \"fiscal_year\": $YEAR}"
  local cr
  cr=$(field .id)
  say "The merge's notes for the reviewers include, outside production, that the pack is not signed off:"
  excerpt '.notes[] | select(.path | startswith("packs")) | "note: \(.message)"'
  field '.notes[] | .message' | grep -q 'signed' || fail "no note says the pack is not signed off"
  say "  ($(field '[.notes[] | select(.message | startswith("conflict"))] | length') more notes list where SAP's settings were kept over the pack's)"
  say "Ada adds the pilot's own settings to the proposal: RBC house banks in CAD and USD, USD accounts revalued"
  say "at month end, bank statement rules (match by end-to-end id, approval above CAD 10,000), payment runs"
  say "above CAD 4,000 released only after a second person approves, a 2/10 net 30 cash discount term,"
  say "cost center categories, liquidity items, an output tax code for GST, the GST/HST registration and, for the"
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
  [ "$(field .requisition_date)" = "$before" ] || expect "the requisition's date, Toronto's" "$(field .requisition_date)" "$(company_day)"
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
  api 201 '"purchase order \(.document_number // .id): \(.status), \(.total_amount.amount) \(.total_amount.currency), terms \(.payment_term_code)"' createPurchaseOrder \
    -body "{\"legal_entity_id\": \"$LE\", \"supplier_id\": \"$SUPPLIER\", \"currency\": \"CAD\", \"order_date\": \"$TODAY\", \"payment_term_code\": \"2/10N30\", \"requisition_id\": \"$req\", \"items\": [${item/450.00/470.00}]}"
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

# order_document has Tyrell's order desk address added, as SAP's extract
# holds no street, and shows the released order's document for Tyrell
# (T72g, ADR-0054).
order_document() {
  local number change utc_today
  number=$(field .document_number)
  # The address starts on the database's day, UTC's: a business partner
  # belongs to no company and so has no zone, and its address may start no
  # earlier than that day (partner.backdated_address, database.Today). It
  # is read as the change is asked for, not when the run began, so a run
  # that crosses 00:00 UTC (19:00 or 20:00 in Toronto) asks for that day.
  utc_today=$(utc_day)
  say "SAP's extract has no street for Tyrell. Ben asks for the address Tyrell takes orders at; Carla, who"
  say "approves supplier master data, approves it."
  api 201 '"partner change \(.id): \(.status)"' createPartnerChange -body "{\"partner_id\": \"$SUPPLIER\", \"kind\": \"update\", \"reason\": \"Order desk address confirmed by Tyrell\", \"payload\": {\"addresses\": [{\"purpose\": \"order_from\", \"line1\": \"220 King Street West\", \"line2\": \"Order desk, floor 3\", \"city\": \"Toronto\", \"region\": \"ON\", \"postal_code\": \"M5H 1K4\", \"country_code\": \"CA\", \"valid_from\": \"$utc_today\"}]}}"
  change=$(field .id)
  approve carla "$(field .workflow_request_id)" "Address confirmed"
  as ben
  QUIET=1 api 200 '' getPartnerChange -p "id=$change"
  expect "partner change status" "$(field .status)" applied
  say "Ben fetches the order's document to send to Tyrell (by hand: email and EDI are post-pilot)."
  api 200 '' getPurchaseOrderDocument -p "id=$PO"
  sed 's/^/     /' "$WORK/last.json"
  local want
  for want in "PURCHASE ORDER $number" "220 King Street West" "Payment terms: 2/10N30" "Needed by: $NEEDED_BY" "10 EA at 470.00 CAD per 1 EA" "Total 4700.00 CAD"; do
    grep -qF "$want" "$WORK/last.json" || fail "the order's document does not show '$want'"
  done
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
  heading "Step 6 — A credit memo; the receipt it leaves invoiced cannot be reversed"
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
  [ "$(field .posting_date)" = "$before" ] || expect "the credit memo's posting date, Toronto's" "$(field .posting_date)" "$(company_day)"
  CREDIT_ENTRY=$(field .journal_entry_id)
  api 200 '.items[] | "open item \(.amount.amount) \(.amount.currency)"' listOpenItems -p "partner_id=$SUPPLIER"
  receipt_held
  passed
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

step_payment() {
  heading "Step 7 — A payment run: proposed with the cash discount, its release approved by a second person, released by a third, the bank file"
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
  api 200 '.items[] | "excluded: \(.reason_code), \(.amount.amount) \(.amount.currency), supplier \(.supplier_name)"' listPaymentExclusions -p "id=$RUN" -p page_size=100
  expect "the blocked supplier left out, named by the API" "$(field '[.items[] | select(.reason_code == "supplier_blocked") | .supplier_name] | join(", ")')" "Massive Dynamic"
  expect "suppliers left out for want of a bank account" "$(field '[.items[] | select(.reason_code == "no_bank_account")] | length')" 0
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
  heading "Step 11 — Tax: the billing system's sales entries, their tax checked by the ledger, large ones parked for a person, and the return that files them"
  as carla
  api 200 '.items[] | select(.country == "CA") | "pack CA \(.version): \(.contents.tax_codes) tax codes; signed off: \(.sign_off // {} | if .tax_specialist then "\(.tax_specialist), \(.date)" else "no" end)"' listCountryPacks
  expect "Canada 2026.4's tax codes" "$(field '.items[] | select(.country == "CA" and .version == "2026.4") | .contents.tax_codes')" 10
  expect "Canada 2026.5's sign-off" "$(field '.items[] | select(.country == "CA" and .version == "2026.5") | "\(.signed_off) \(.sign_off.tax_specialist) \(.sign_off.date)"')" \
    "true Musto Ibrahim 2026-11-02"
  say "Canada pack 2026.4 brought the purchase codes for GST/HST, QST and PST (T62g), each with all its dated rates,"
  say "backed by the pack's sources; 2026.5 is the same pack signed off by the tax specialist (T62h), so production"
  say "applies it. Step 2 applied the codes with $CA_PACK, the walkthrough's unsigned copy of the latest released pack"
  say "with the illustrative return. Nova Scotia's HST:"
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
  file_return "$registration" "$start" "$end"
  passed
}

# tax_return CASH SALES GST REGISTRATION START END BEFORE RUNNING is step
# 11's draft return (T61h) of the latest ended period, START to END: two
# late sales, one dated END and one BEFORE, the first period's end; the
# draft computed twice; the output tax box drilled down to its items;
# SAP's down payment left out; the period under way, RUNNING, refused;
# the return of START refused until the first period's is filed, which
# reports the late sale of BEFORE.
tax_return() {
  local cash=$1 sales=$2 gst=$3 registration=$4 start=$5 end=$6 before=$7 running=$8
  say "Two of the billing system's invoices arrive late, and Carla posts them today with their own tax date (P6):"
  say "1039 of $end, the last day of the latest ended period, and 1031 of $before, in the period before it."
  api 201 "$entry" postJournalEntry -body "$(sale "Billing: invoice 1039, sent late" "$(line "$cash" 1050.00), $(line "$sales" -1000.00 GO5), $(line "$gst" -50.00 GO5)" "$end")"
  api 201 "$entry" postJournalEntry -body "$(sale "Billing: invoice 1031, sent late" "$(line "$cash" 210.00), $(line "$sales" -200.00 GO5), $(line "$gst" -10.00 GO5)" "$before")"
  say "Carla computes the draft return of $start to $end (T61h): the boxes sum the tax items no return has reported,"
  say "dated by the period's end; those of earlier periods are shown apart (P7). The pack defines the boxes; these are"
  say "illustrative, not the GST34's lines (T61j). Nothing is stored or claimed: computed again, the draft is the same."
  api 200 '"draft \(.return_type) of \(.registration_code), \(.period_start) to \(.period_end), \(.currency), pack \(.pack.country) \(.pack.version): \(.item_count) items, \(.prior_period.item_count) of earlier periods", (.boxes[] | "  \(.code) \(.amount.amount)\(if .prior_period_amount.amount != "0.00" then ", of which earlier periods \(.prior_period_amount.amount)" else "" end): \(.label)")' \
    getTaxReturnDraft -p "id=$registration" -p "period_start=$start"
  expect "the output tax, and its earlier period's part" "$(field '.boxes[] | select(.code == "output_tax") | "\(.amount.amount) \(.prior_period_amount.amount)"')" "60.00 10.00"
  local draft
  draft=$(jq -cS . "$WORK/last.json")
  QUIET=1 api 200 '' getTaxReturnDraft -p "id=$registration" -p "period_start=$start"
  expect "the draft computed again" "$(jq -cS . "$WORK/last.json")" "$draft"
  say "The output tax box drills down to the items it sums and the entries that wrote them:"
  api 200 '.items[] | "  \(.tax_date) \(.tax_code)/\(.component): tax \(.tax.amount), adds \(.box_amount.amount)\(if .prior_period then " (an earlier period'"'"'s)" else "" end), entry \(.document_number) of \(.posting_date): \(.header_text // "")"' \
    listTaxReturnDraftItems -p "id=$registration" -p "period_start=$start" -p box=output_tax
  expect "the output tax box's items" "$(field '[.items[].box_amount.amount] | join(" ")')" "10.00 50.00"
  expect "the items' invoices" "$(field '[.items[].header_text] | join(" / ")')" "Billing: invoice 1031, sent late / Billing: invoice 1039, sent late"
  QUIET=1 api 200 '' listTaxReturnDraftItems -p "id=$registration" -p "period_start=$start" -p box=input_tax -p page_size=500
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

# close_tax_periods START END BODY REGISTRATION closes the leading
# ledger's open fiscal periods of the tax period START to END, which
# preparing the return BODY asks for needs (T61i), once preparing has
# been refused for them; LEADING is the leading ledger.
close_tax_periods() {
  local start=$1 end=$2 body=$3 registration=$4 open p
  QUIET=1 api 200 '' listLedgers
  LEADING=$(field '.items[] | select(.is_leading) | .id')
  QUIET=1 api 200 '' listFiscalPeriods -p "legal_entity_id=$LE" -p "fiscal_year=${start%%-*}" -p "ledger_id=$LEADING" -p page_size=100
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
  expect "the first period's output tax, invoice 1031's" "$(field '.boxes[] | select(.code == "output_tax") | .amount.amount')" "10.00"
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
  expect "the return's output tax, invoice 1039's" "$(field '.boxes[] | select(.code == "output_tax") | .amount.amount')" "50.00"
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
  api 200 '"draft of \(.period_start) to \(.period_end): \(.item_count) items, output tax \((.boxes[] | select(.code == "output_tax")).amount.amount)"' \
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
  api 201 '"hierarchy \(.kind) \(.code), standard: \(.is_standard)"' createHierarchy \
    -body '{"kind": "profit_center", "code": "PC-STD", "name": "Profit centers", "is_standard": true}'
  pch=$(field .id)
  add_node "$pch" "$(field .root_node_id)" EAST "Eastern Canada"
  api 201 '"profit center \(.code) \(.name): \(.status)"' createProfitCenter \
    -body "{\"code\": \"PC-MTL\", \"name\": \"Montreal office\", \"responsible\": \"Carla Controller\", \"segment_id\": \"$segment\", \"legal_entity_ids\": [\"$LE\"], \"valid_from\": \"$YEAR-01-01\", \"standard_node_id\": \"$NODE\", \"reason\": \"The pilot's office\"}"
  pc=$(field .id)
  center_approved sam
  api 200 '"profit center \(.code): \(.status) from \(.periods[0].valid_from)"' getProfitCenter -p "id=$pc"
  expect "profit center status" "$(field .status)" active
  api 201 '"hierarchy \(.kind) \(.code), standard: \(.is_standard)"' createHierarchy \
    -body '{"kind": "cost_center", "code": "CC-STD", "name": "Cost centers", "is_standard": true}'
  cch=$(field .id)
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
  local before after
  before=$(company_day)
  api 200 '"read on \(.read_date) in \(.time_zone), from \(.from)"' getCashPosition -p "legal_entity_id=$LE" -p days=1
  after=$(company_day)
  expect "the zone the position is read in" "$(field .time_zone)" America/Toronto
  [ "$(field .read_date)" = "$before" ] || expect "the read date, Toronto's" "$(field .read_date)" "$after"
  expect "the first day, the read date" "$(field .from)" "$(field .read_date)"
  say "A client dates its forms on the same day: Maple's date today, in its zone (getLegalEntityToday), which the web"
  say "workspace pre-fills a manual entry's dates, a payment run's and the open items' key date with, not UTC's day:"
  before=$(company_day)
  api 200 '"today is \(.date) in \(.time_zone)"' getLegalEntityToday -p "id=$LE"
  after=$(company_day)
  expect "the zone Maple's today is in" "$(field .time_zone)" America/Toronto
  [ "$(field .date)" = "$before" ] || expect "Maple's today, Toronto's" "$(field .date)" "$after"
  local toronto judged status listed counted
  toronto=$(company_day)
  say "A receipt promised for today ($toronto in Toronto), its last day today, is active until Toronto's day ends:"
  say "a memo record's status is judged on Maple's date in its zone, as the position is, not on UTC's (T72n2)."
  api 201 '"memo record \(.reference): \(.status) on \(.read_date) in \(.time_zone), last day \(.expires_on)"' createMemoRecord \
    -body "{\"legal_entity_id\": \"$LE\", \"house_bank_id\": \"$RBC\", \"amount\": {\"amount\": \"1000.00\", \"currency\": \"CAD\"}, \"value_date\": \"$toronto\", \"expires_on\": \"$toronto\", \"liquidity_item\": \"REC_CUSTOMERS\", \"reference\": \"WT-1043\"}"
  expect "the zone the memo record's status is judged in" "$(field .time_zone)" America/Toronto
  judged=$(field .read_date)
  [ "$judged" = "$toronto" ] || expect "the memo record's read date, Toronto's" "$judged" "$(company_day)"
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
  local before after
  before=$(company_day)
  api 200 '"read on \(.read_date) in \(.time_zone), \(.buckets | length) buckets from \(.buckets[0].start) to \(.buckets[-1].end)",
    (.currencies[] | .currency as $c | "  \($c): total \(.total.amount), beyond the last bucket \(.beyond.amount.amount) (\(.beyond.count))",
      (.buckets[] | select(.flows | length > 0) | "    from \(.start): \([.flows[] | "\(.source) \(.certainty) \(.amount.amount) x\(.count)"] | join(", "))"))' \
    getLiquidityForecast -p "legal_entity_id=$LE"
  after=$(company_day)
  expect "the zone the forecast is read in" "$(field .time_zone)" America/Toronto
  [ "$(field .read_date)" = "$before" ] || expect "the forecast's read date, Toronto's" "$(field .read_date)" "$after"
  expect "the first bucket, the read date" "$(field '.buckets[0].start')" "$(field .read_date)"
  # A day's bucket starts on the day expected: the first Tuesday or Thursday
  # from today, or, when that is one of Maple's holidays, the next working
  # day (at most four days later: Christmas on a Thursday, Boxing Day on
  # the Friday); a week's or a month's bucket may start on another day.
  expect "SAP's USD invoice (step 3), on the next payment day" \
    "$(field 'def epoch: strptime("%Y-%m-%d") | mktime;
      . as $f | ([$f.buckets[] | select(.grain == "day") | .start | select(epoch | strftime("%a") | IN("Tue", "Thu"))][0] | epoch) as $pay
      | [.currencies[] | select(.currency == "USD") | .buckets | to_entries[] | select(any(.value.flows[]; .source == "payable"))
      | "\(.value.total.amount) \(if $f.buckets[.key].grain == "day" then (.value.start | epoch) - $pay | (. >= 0 and . <= 4 * 86400) else true end)"]
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
      else {"ledger": ., "method": "declining_balance", "useful_life_months": 120, "declining_factor": "2"} end]}]
| (.companies[] | select(.code == "@CC@")) |= (
    .chart_of_accounts += [
      {"number": "1600", "name": "Machinery", "type": "asset", "asset_reconciliation": true},
      {"number": "1609", "name": "Machinery, accumulated depreciation", "type": "asset", "asset_reconciliation": true},
      {"number": "1690", "name": "Asset clearing", "type": "asset"},
      {"number": "1695", "name": "Asset sale clearing", "type": "asset"},
      {"number": "4950", "name": "Gain on asset retirements", "type": "revenue"},
      {"number": "6250", "name": "Depreciation of machinery", "type": "expense"},
      {"number": "6950", "name": "Loss on asset retirements", "type": "expense"}]
    | .asset_accounts = [{"class": "MACHINES", "acquisition_cost": "1600", "accumulated_depreciation": "1609", "depreciation": "6250",
        "clearing": "1690", "gain": "4950", "loss": "6950", "sale_clearing": "1695"}])
| .number_ranges += (["asset", "journal_aa", "journal_af"] | map({"company": "@CC@", "document_type": ., "fiscal_year": @YEAR@, "first": 1, "last": 999999}))'

step_assets() {
  heading "Step 19 — Asset accounting: an asset class and its accounts through a governed change; the cost account takes only assets' postings"
  as ada
  say "Ada proposes the pilot's first asset class (ADR-0057 §3): machinery, depreciated straight-line over ten"
  say "years in the leading ledger and at twice the declining rate in 2L, with its accounts. The acquisition cost"
  say "and accumulated depreciation accounts are asset reconciliation accounts, which only the assets context"
  say "posts to, so their balances are the assets' values. The company numbers its assets and their entries"
  say "(AA, AF) in this fiscal year's ranges."
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
  expect "the classes of $CC" "$(field '[.items[] | .code] | join(" ")')" MACHINES
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
  local next to memo memo_etag cells
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
  to=$(date_add "$TODAY" 55)
  say "One grain over a range instead of Maple's buckets: weeks up to $to."
  api 200 '"\(.buckets | length) \(.buckets[0].grain)s from \(.buckets[0].start) to \(.buckets[-1].end)"' getLiquidityForecast -p "legal_entity_id=$LE" -p grain=week -p "to=$to"
  expect "the weeks up to the one holding $to" "$(field '.buckets | length')" 8
  say "Tom drills the first two weeks of Customer receipts down to their flows and the documents behind them, each"
  say "to read in its own context (getMemoRecord here), and the drill-down totals the row's cells:"
  api 200 '"  \(.count) flows, total \(.total.amount) \(.total.currency)", (.items[] | "  \(.date) \(.source) \(.certainty) \(.share.amount): \([.documents[] | "\(.kind) \(.number // "-") \(.amount.amount)"] | join(", ")) (\(.document_count))")' \
    listLiquidityForecastFlows -p "legal_entity_id=$LE" -p currency=CAD -p "to=$(date_add "$TODAY" 13)" -p liquidity_item=REC_CUSTOMERS
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
    listLiquidityForecastFlows -p "legal_entity_id=$LE" -p currency=CAD -p "to=$(date_add "$TODAY" 13)" -p liquidity_item=REC_CUSTOMERS
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

# step_close is the financial close (ADR-0058), the story's last step: it
# stays last, and each slice of T64 adds its part before the sign-off
# (ADR-0058, Steps). T64a: Ada proposes the close template, a tier 2 change
# the process owner approves; Carla reads the year's close calendar, each
# period's planned first and last due dates on the company's business days.
# Then the close's checklist (T64b), its depreciation check (T64k), its
# other checks (T64c), its variances (T64h) and, last, its sign-off
# (T64d), which locks the period.
step_close() {
  heading "Final step — Financial close: the close template through a governed change, the year's close calendar, the checklist and its sign-off"
  as ada
  say "Ada proposes the close template (ADR-0058 §3): the tasks every company's period close is given, each due"
  say "a number of business days after the period's last day (negative: before it), some waiting for others:"
  say "manual tasks a person completes, and checks the system runs (no entry of the period left parked, and the"
  say "period's depreciation posted in every ledger). The"
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
      {\"code\": \"DEPRECIATED\", \"name\": \"Depreciation of the period posted in every ledger\", \"kind\": \"check\", \"check\": \"depreciation_posted\", \"due_day\": 2}])}" \
    "$WORK/last.json" >"$WORK/close.json"
  api 201 '"change request \(.number), \(.status): \(.title)"' createChangeRequest -body @close.json
  cr=$(field .id)
  api 200 '"\(.status), tier \(.tier): \([.items[].path] | join(", "))"' submitChangeRequest -p "id=$cr" -if-match "$ETAG"
  expect "the change's tier" "$(field .tier)" 2
  expect "the tasks among the changes" "$(field '[.items[] | select(.path | startswith("close.tasks["))] | length')" 5
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
  close_depreciation
  close_checks
  close_variances
  close_sign_off
  passed
}

# close_checklist is T64b's part of step_close (ADR-0058 §6): Carla opens
# the close of today's period, the template copied into it, and works its
# manual checklist to the end, every task done for the sign-off to come.
close_checklist() {
  local period close tag carla
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
  expect "the close's tasks" "$(field '[.tasks[].code] | join(" ")')" "CUTOFF_NOTICE AP_CUTOFF TB_REVIEW NO_PARKED DEPRECIATED"
  expect "the leading ledger first" "$(field '.ledgers[0].ledger_code')" 0L
  say "A second close of the period is refused, and names the first:"
  refused 409 closing.close_exists createPeriodClose -body "{\"legal_entity_id\": \"$LE\", \"fiscal_year\": $YEAR, \"fiscal_period\": $period}"
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
  expect "every task done" "$(field '.tasks_done == .tasks_total')" true
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

STORY='step_tenant step_pack step_migrate step_procure step_invoices step_credit step_payment step_statement step_fx step_ledgers step_tax step_centers step_cash step_reports step_grir step_downpayment step_audit step_capture step_assets step_asset_master step_cash_items step_asset_acquisitions step_cash_forecast step_asset_reversal step_asset_depreciation step_cash_snapshot step_cash_actuals step_cash_variance step_depreciation_run step_close'

main() {
  start
  local s
  for s in $STORY; do
    "$s"
    if [ "${WALKTHROUGH_UNTIL:-}" = "$s" ]; then break; fi
  done
  summary
  if [ "${WALKTHROUGH_KEEP:-0}" = 1 ]; then
    say ""
    say "The server keeps running for a look around (Ctrl-C stops it):"
    say "  export ERP_API_URL=$ERP_API_URL ERP_ADMIN_URL=$ERP_ADMIN_URL"
    say "  export ERP_TOKEN=\$(cat $WORK/tokens/carla)   # or ada, ben, alex, priya, sam, tom, tara"
    say "  $ERP call listSupplierInvoices"
    wait "$SERVER_PID" || true # stopped by Ctrl-C or a kill: the walkthrough itself has passed
  fi
}

main "$@"
