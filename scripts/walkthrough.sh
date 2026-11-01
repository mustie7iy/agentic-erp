#!/usr/bin/env bash
# The pilot walkthrough (make walkthrough): a Canadian company's story told
# against the real binaries, for the product owner to watch. It builds
# erp-server and erp, starts the server with the development issuer on
# DATABASE_URL (migrated on start), provisions a tenant from the mock SAP
# extract and drives everything else through the API with `erp call`, as
# the people of the story. No test harness and no mocks beyond the mock SAP
# extract. A step that needs a capability not built yet says so and is
# skipped.
#
# Each step prints a heading, the commands it runs and an excerpt of the
# answers, and checks what it expects: any unexpected answer stops the run
# with the reason, the answer and the server's last log lines. The summary
# at the end lists the steps that passed and those skipped.
#
#   WALKTHROUGH_PAUSE=1   wait for Enter after each step
#   WALKTHROUGH_API_ADDR, WALKTHROUGH_ADMIN_ADDR   listeners (127.0.0.1:18080, 127.0.0.1:19090)
#   WALKTHROUGH_DIR       work directory (default: a temporary one, removed after)
#   WALKTHROUGH_BIN       directory with erp and erp-server already built (default: build them)
#   WALKTHROUGH_KEEP=1    keep the server running at the end, to look around with erp call
#   WALKTHROUGH_UNTIL     stop after this step (its function name, e.g. step_migrate)
#
# Needs jq, which reads the ids out of the answers, curl and PostgreSQL
# (make db-up). Every run provisions a tenant of its own; tenants are
# isolated, so runs never see each other's data. The mock SAP extract is
# the same on every run (fixed seed, January to September of this year);
# the pilot's documents are dated today (UTC, the server's day).
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
DATABASE_URL="${DATABASE_URL:-postgres://postgres:dev@localhost:55432/postgres?sslmode=disable}"
API_ADDR="${WALKTHROUGH_API_ADDR:-127.0.0.1:18080}"
ADMIN_ADDR="${WALKTHROUGH_ADMIN_ADDR:-127.0.0.1:19090}"
PAUSE="${WALKTHROUGH_PAUSE:-0}"
TODAY=$(date -u +%F)
YEAR=${TODAY:0:4}
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

cleanup() {
  if [ -n "$SERVER_PID" ]; then
    kill "$SERVER_PID" 2>/dev/null || true # it may have exited already: then there is nothing to stop
    wait "$SERVER_PID" 2>/dev/null || true # its exit status after a kill says nothing about the walkthrough
  fi
  if [ -z "${WALKTHROUGH_DIR:-}" ]; then rm -rf "$WORK"; fi
}
trap cleanup EXIT

# date_add DATE DAYS is the date DAYS days after DATE, with GNU or BSD date.
date_add() {
  if date -u -d "$1" +%F >/dev/null 2>&1; then
    date -u -d "$1 +$2 days" +%F
  else
    date -u -j -v"+$2"d -f %F "$1" +%F
  fi
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
  say "Today is $TODAY (UTC): the pilot's documents are dated today."
  say ""
  say "Not built yet, so not shown:"
  say "  - the web screens: only the workspace skeleton exists, sign-in comes with T73b, the screens with T73c-T73m"
  say "  - tax returns (T61g-T61l), and the Canada pack's tax codes (T62g, Canada pack 2026.3): step 2 configures one code, step 11 uses it"
  say "  - the liquidity forecast (T53e-T53k): step 13 shows the daily cash position with a memo record"
  say "  - cost center postings, reports and planning (T56d-T56j)"
  say "  - accounts receivable, fixed assets (T57), period-end close (T64), consolidation (T68)"
  say "  - natural-language questions run on a live model (erp ask); here the same question is asked as a governed query"

  export DATABASE_URL ERP_ENV=development ERP_DEV_ISSUER=true ERP_MIGRATE_ON_START=true
  export ERP_HTTP_ADDR="$API_ADDR" ERP_ADMIN_ADDR="$ADMIN_ADDR"
  export ERP_API_URL="http://$API_ADDR" ERP_ADMIN_URL="http://$ADMIN_ADDR" ERP_CONTROL_URL="http://$ADMIN_ADDR"
  export ERP_EXTRACT_DIR="$WORK/data" ERP_OBJECT_DIR="$WORK/data/objects"
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
  say ""
  say "erp-server is up (development, development issuer on): API $ERP_API_URL, admin $ERP_ADMIN_URL."
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
  for code in Z_PURCH_MGR process_owner controller solution_architect Z_TREASURY_MGR vendor_master_approver; do
    # shellcheck disable=SC2046 # permission codes are single words
    grant "$(role "$code")" workflow.request.decide $(grep '\.approve$' "$WORK/permissions")
  done
  grant "$(role Z_TREASURY_MGR)" payments.run.release payments.file.transmit
  say "  the approver roles Z_PURCH_MGR, process_owner, controller, solution_architect and vendor_master_approver"
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
  person fiona "Fiona Finance Director" finance_staff Z_TREASURY_MGR
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
  say "company $CC in CAD, with Canadian suppliers and a Canadian house bank (T72d)."
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
  "$ERP" config generate -source "file:$WORK/data/extract.json" -company "$CC" -year "$YEAR" -time-zone "$CC=America/Toronto" \
    -out "$WORK/config.yaml" >"$WORK/generate.out" 2>&1 || { sed 's/^/     /' "$WORK/generate.out"; fail "config generate failed"; }
  grep -q 'Validation: no findings' "$WORK/generate.out" || { sed 's/^/     /' "$WORK/generate.out"; fail "the generated configuration has findings"; }
  say "  the configuration generated from SAP's customizing validates with no findings."
  say "SAP's house bank (T012K) is Canadian: no IBAN, its account by institution, transit and account number:"
  jq -r --arg cc "$CC" '.T012K[] | select(.BUKRS == $cc) | "     T012K \(.HBKID): bank \(.BANKS) \(.BANKL), account \(.BANKN), IBAN \(if .IBAN == "" then "none" else .IBAN end)"' "$WORK/data/extract.json"
  grep -A 6 '^    house_bank:$' "$WORK/config.yaml" | sed 's/^/     /'
  grep -A 6 '^    house_bank:$' "$WORK/config.yaml" | grep -q 'institution_number: "003"' || fail "the house bank was not generated with its institution number"
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
    'Open down payments total' 'Open down payments: vendors with differences' 'Open GR/IR total' 'Open GR/IR: order items with differences'; do
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
  api 200 '.items[] | "bank account \(.purpose): \(.account_scheme), bank \(.bank_country_code) \(.institution_number)-\(.transit_number), account ending \(.account_last4), valid from \(.valid_from)"' \
    listBankAccounts -p "id=$laurentian"
  expect "the Canadian account's scheme and bank" "$(field '.items[0] | "\(.account_scheme) \(.institution_number) \(.transit_number)"')" "ca 001 00010"
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
  migrated_grir
  passed
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
  expect "open GR/IR positions after the migration, the shelving units' and order $CLOSED's" "$(field '.items | length')" 2
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
# categories, the liquidity items memo records name (T53d) and, until the
# Canada pack carries tax codes (T62g), one output tax code for the
# walkthrough's sales entry: GST at 5% (Excise Tax Act, Part IX, s. 165(1);
# CRA, "Charge and collect the tax - Which rate to charge",
# https://www.canada.ca/en/revenue-agency/services/tax/businesses/topics/gst-hst-businesses/charge-collect-which-rate.html).
# @YEAR@ is replaced with this year and @CC@ with the company code.
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
    | .tax_codes = ((.tax_codes // []) + [{"code": "GO5", "description": "GST 5% on sales (walkthrough, until Canada pack 2026.3)",
        "direction": "output", "kind": "standard", "category": "S", "components": [{"key": "gst", "jurisdiction": "CA",
        "account_key": "output", "account": "2300", "rates": [{"rate_percent": "5", "valid_from": "@YEAR@-01-01"}]}]}])
    | .house_banks = ((.house_banks // []) + [
      {"name": "RBC", "bic": "ROYCCAT2", "institution_number": "003", "transit_number": "00002", "account_number": "1000001",
       "gl_account": "1010", "main_gl_account": "1000", "incoming_gl_account": "1015"},
      {"name": "RBCUS", "bic": "ROYCCAT2", "institution_number": "003", "transit_number": "00002", "account_number": "4000001",
       "gl_account": "1020", "main_gl_account": "1030", "incoming_gl_account": "1040", "currency": "USD"}])
    | .release_dual_limit = "4000.00"
    | .service_posting_limit = "1000.00"
    | .fx_valuation = {"accounts": ["1020", "1030", "1040"] | map({"account": ., "unrealised_gain": "4900", "unrealised_loss": "6900"})}
    | .bank_statements = {"matching_rules": [{"kind": "end_to_end_id"}, {"kind": "amount", "date_window_days": 5, "counterparty": true}],
        "approval_tiers": [{"up_to": "10000.00", "steps": []}, {"steps": [{"role": "controller", "approvals": 1}]}]})'

step_pack() {
  heading "Step 2 — The Canada country pack and the pilot's settings, before migration, through a governed change request"
  as ada
  api 200 '.items[] | "pack \(.country) \(.version): \(.contents.accounts) accounts, \(.contents.tax_codes) tax codes, \(.contents.holidays) holidays, \(.contents.number_ranges) number ranges; signed off: \(.signed_off)"' listCountryPacks
  local version
  version=$(field '[.items[] | select(.country == "CA") | .version] | sort_by(split(".") | map(tonumber)) | last')
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
  say "cost center categories, liquidity items, an output tax code for GST and, for the billing system's"
  say "entries, a service posting limit of CAD 1,000 (step 11). The company keeps SAP's"
  say "down payment account (T074), so the pack's own, 1400, is left out: a company has one (ADR-0037)."
  local settings=${PILOT_SETTINGS//@YEAR@/$YEAR}
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
  accounts
  passed
}

step_procure() {
  heading "Step 4 — Requisition, purchase order with its release strategy, its document for the supplier, goods receipt"
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
  before=$(TZ=America/Toronto date +%F)
  api 201 '"requisition \(.id) of \(.requisition_date): \(.status), \(.total_amount.amount) \(.total_amount.currency), needed by \(.needed_by)"' createRequisition \
    -body "{\"legal_entity_id\": \"$LE\", \"currency\": \"CAD\", \"supplier_id\": \"$SUPPLIER\", \"needed_by\": \"$NEEDED_BY\", \"items\": [$item]}"
  [ "$(field .requisition_date)" = "$before" ] || expect "the requisition's date, Toronto's" "$(field .requisition_date)" "$(TZ=America/Toronto date +%F)"
  req=$(field .id)
  api 200 '"\(.status): approval request \(.workflow_request_id)"' submitRequisition -p "id=$req" -if-match "$ETAG"
  request=$(field .workflow_request_id)
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
  say "The chairs arrive; Ben posts the goods receipt (debit expense, credit GR/IR)."
  api 201 '"goods receipt \(.id), journal entry \(.journal_entry_id)"' postGoodsReceipt \
    -body "{\"legal_entity_id\": \"$LE\", \"document_date\": \"$TODAY\", \"posting_date\": \"$TODAY\", \"items\": [{\"purchase_order_id\": \"$PO\", \"po_item_no\": 1, \"quantity\": {\"value\": \"10\", \"unit\": \"EA\"}}]}"
  api 200 '.items[] | "item \(.item_no): ordered \(.quantity.value), received \(.received_quantity.value)"' getPurchaseOrder -p "id=$PO"
  passed
}

# order_document has Tyrell's order desk address added, as SAP's extract
# holds no street, and shows the released order's document for Tyrell
# (T72g, ADR-0054).
order_document() {
  local number change
  number=$(field .document_number)
  say "SAP's extract has no street for Tyrell. Ben asks for the address Tyrell takes orders at; Carla, who"
  say "approves supplier master data, approves it."
  api 201 '"partner change \(.id): \(.status)"' createPartnerChange -body "{\"partner_id\": \"$SUPPLIER\", \"kind\": \"update\", \"reason\": \"Order desk address confirmed by Tyrell\", \"payload\": {\"addresses\": [{\"purpose\": \"order_from\", \"line1\": \"220 King Street West\", \"line2\": \"Order desk, floor 3\", \"city\": \"Toronto\", \"region\": \"ON\", \"postal_code\": \"M5H 1K4\", \"country_code\": \"CA\", \"valid_from\": \"$TODAY\"}]}}"
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
  heading "Step 6 — A credit memo"
  as alex
  say "One chair arrived damaged; Tyrell credits it. A credit memo debits the supplier when posted."
  invoice TY-CM-77 1 470.00 credit_memo
  CREDIT=$(field .id)
  api 200 "$MATCHED" matchSupplierInvoice -p "id=$CREDIT" -if-match '"1"'
  say "Alex posts it without a posting date: it is dated Maple's date in Toronto, not UTC's, so on a Toronto"
  say "evening it is still that day, in that day's period (product owner 2026-10-31)."
  local before
  before=$(TZ=America/Toronto date +%F)
  api 200 '"\(.kind) \(.document_number): \(.status) on \(.posting_date)"' postSupplierInvoice -p "id=$CREDIT" -if-match '"1"' -body '{}'
  expect "credit memo status" "$(field .status)" posted
  [ "$(field .posting_date)" = "$before" ] || expect "the credit memo's posting date, Toronto's" "$(field .posting_date)" "$(TZ=America/Toronto date +%F)"
  CREDIT_ENTRY=$(field .journal_entry_id)
  api 200 '.items[] | "open item \(.amount.amount) \(.amount.currency)"' listOpenItems -p "partner_id=$SUPPLIER"
  passed
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
    jq -r --arg d "$TODAY" '.items[] | "<Ntry><Amt Ccy=\"CAD\">\(.amount.amount)</Amt><CdtDbtInd>DBIT</CdtDbtInd><Sts>BOOK</Sts><BookgDt><Dt>\($d)</Dt></BookgDt><ValDt><Dt>\($d)</Dt></ValDt><BkTxCd><Domn><Cd>PMNT</Cd><Fmly><Cd>ICDT</Cd><SubFmlyCd>ESCT</SubFmlyCd></Fmly></Domn></BkTxCd><NtryDtls><TxDtls><Refs><EndToEndId>\(.end_to_end_id)</EndToEndId></Refs></TxDtls></NtryDtls></Ntry>"' "$WORK/payments.json"
    printf '</Stmt></BkToCstmrStmt></Document>\n'
  } >"$WORK/camt053.xml"
  CLOSING=$closing
}

step_statement() {
  heading "Step 8 — The bank statement: imported, matched by rules, posted with approval, reconciled"
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
  say "RBC's end-of-day statement (camt.053) shows the run's payments leaving the account."
  statement 500000.00
  say "  $(grep -c '<Ntry>' "$WORK/camt053.xml") entries, opening 500000.00, closing $CLOSING CAD"
  say "Tom imports it, the file as a part of the upload's form:"
  api 201 '' importBankStatements -body "{\"house_bank_id\": \"$RBC\", \"format\": \"camt.053\"}" -file file=@camt053.xml
  local st
  st=$(field '.statements[0].id')
  if [ -z "$st" ] || [ "$st" = null ]; then fail "the statement was not imported"; fi
  excerpt '.statements[] | "statement \(.statement_ref): \(.status), \(.line_count) lines"'
  say "The matching rules found each payment by its end-to-end id. Lines up to CAD 10,000.00 post at once;"
  say "the others wait for the controller:"
  api 200 '.items[] | "line \(.line_no): \(.status) by rule \(.rule)\(if .approval_request_id then ", approval request \(.approval_request_id[0:8])…" else "" end)"' listBankStatementMatches -p "id=$st"
  local r
  for r in $(field '.items[] | select(.approval_request_id != null and .status == "matched") | .approval_request_id'); do
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
  api 200 '.items[] | "account \(.account_id | acct): 0L \(.balance.amount), 2L \(.compare_balance.amount), difference \(.difference.amount) \(.currency)"' listLedgerReconciliation \
    -p "legal_entity_id=$LE" -p "compare_ledger_id=$l2" -p "fiscal_year=$YEAR"
  expect "accounts that differ" "$(field '.items | length')" 2
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
  heading "Step 11 — Tax: the billing system's sales entries, their tax checked by the ledger, large ones parked for a person"
  as carla
  api 200 '.items[] | select(.country == "CA") | "pack CA \(.version): \(.contents.tax_codes) tax codes"' listCountryPacks
  say "The released Canada packs carry no tax codes yet: GST/HST, QST and PST come with Canada pack 2026.3 (T62g)."
  say "Step 2 configured one output code for the pilot instead: GO5, GST at 5% from 1 January, to the pack's 2300 GST/HST payable."
  say "The ERP issues no sales invoices before phase 3, so the pilot's billing system posts its sales as journal"
  say "entries (T61f), as a service principal of its own that holds only ledger.entry.post:"
  billing
  accounts
  as billing
  local cash sales gst
  cash=$(account 100000)
  sales=$(account 4000)
  gst=$(account 2300)
  # sale LINES is a sales entry of today in CAD with the lines given as
  # "account amount [code]" triples' JSON.
  sale() {
    printf '{"legal_entity_id": "%s", "document_date": "%s", "posting_date": "%s", "currency": "CAD", "header_text": "%s", "lines": [%s]}' \
      "$LE" "$TODAY" "$TODAY" "$1" "$2"
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
  passed
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
  before=$(TZ=America/Toronto date +%F)
  api 200 '"read on \(.read_date) in \(.time_zone), from \(.from)"' getCashPosition -p "legal_entity_id=$LE" -p days=1
  after=$(TZ=America/Toronto date +%F)
  expect "the zone the position is read in" "$(field .time_zone)" America/Toronto
  [ "$(field .read_date)" = "$before" ] || expect "the read date, Toronto's" "$(field .read_date)" "$after"
  expect "the first day, the read date" "$(field .from)" "$(field .read_date)"
  local toronto judged status listed counted
  toronto=$(TZ=America/Toronto date +%F)
  say "A receipt promised for today ($toronto in Toronto), its last day today, is active until Toronto's day ends:"
  say "a memo record's status is judged on Maple's date in its zone, as the position is, not on UTC's (T72n2)."
  api 201 '"memo record \(.reference): \(.status) on \(.read_date) in \(.time_zone), last day \(.expires_on)"' createMemoRecord \
    -body "{\"legal_entity_id\": \"$LE\", \"house_bank_id\": \"$RBC\", \"amount\": {\"amount\": \"1000.00\", \"currency\": \"CAD\"}, \"value_date\": \"$toronto\", \"expires_on\": \"$toronto\", \"liquidity_item\": \"REC_CUSTOMERS\", \"reference\": \"WT-1043\"}"
  expect "the zone the memo record's status is judged in" "$(field .time_zone)" America/Toronto
  judged=$(field .read_date)
  [ "$judged" = "$toronto" ] || expect "the memo record's read date, Toronto's" "$judged" "$(TZ=America/Toronto date +%F)"
  status=active
  [ "$judged" = "$toronto" ] || status=expired # Toronto's midnight passed between the two reads
  expect "the memo record's status on Maple's date" "$(field .status)" "$status"
  api 200 '.items[] | "  \(.reference // "-"): \(.status) on \(.read_date) in \(.time_zone)"' listMemoRecords -p "legal_entity_id=$LE" -p status=active
  listed=$(field '[.items[] | select(.reference == "WT-1043")] | length')
  api 200 '"position read on \(.read_date): memo flows at RBC \([.banks[] | select(.house_bank_code == "RBC") | .days[].flows[]? | select(.source == "memo") | .amount.amount] | join(" "))"' \
    getCashPosition -p "legal_entity_id=$LE" -p days=7
  counted=$(field '[.banks[] | select(.house_bank_code == "RBC") | .days[].flows[]? | select(.source == "memo" and .amount.amount == "1000.00")] | length')
  [ "$(field .read_date)" != "$toronto" ] || expect "the record listed active and counted in the position on Toronto's date" "$listed $counted" "1 1"
  passed
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
  say "$DP_NAME's final invoice arrives: CAD 5,000.00 plus 250.00 GST on V1. Alex enters and posts it."
  local item invoice fy line
  item="{\"account_id\": \"$(account 6000)\", \"description\": \"Machine, final invoice\", \"quantity\": {\"value\": \"1\", \"unit\": \"EA\"}, \"amount\": {\"amount\": \"5000.00\", \"currency\": \"CAD\"}, \"tax_code\": \"V1\"}"
  api 201 '"\(.kind) \(.document_number) \(.supplier_reference): \(.status), \(.gross_amount.amount) \(.gross_amount.currency)"' createSupplierInvoice \
    -body "{\"legal_entity_id\": \"$LE\", \"supplier_id\": \"$DP_SUPPLIER\", \"supplier_reference\": \"FINAL-DP\", \"invoice_date\": \"$TODAY\", \"gross_amount\": {\"amount\": \"5250.00\", \"currency\": \"CAD\"}, \"tax_amount\": {\"amount\": \"250.00\", \"currency\": \"CAD\"}, \"items\": [$item]}"
  invoice=$(field .id)
  api 200 '"\(.document_number): \(.status), journal entry \(.journal_entry_id)"' postSupplierInvoice -p "id=$invoice" -if-match '"1"' -body "{\"posting_date\": \"$TODAY\"}"
  expect "the final invoice's status" "$(field .status)" posted
  say "She clears the migrated down payment against it, as she would one the platform paid: the transfer credits"
  say "the down payment account, reverses the 100.00 input tax SAP claimed on it (the invoice claims the whole tax)"
  say "and debits the supplier's payables with the down payment, netted against the invoice in the next run."
  read -r fy line <<<"$DP_LINE"
  api 200 '"\(.document_number): \(.status)"' clearSupplierInvoiceDownPayments -p "id=$invoice" -if-match "$ETAG" \
    -body "{\"posting_date\": \"$TODAY\", \"items\": [{\"fiscal_year\": $fy, \"journal_entry_id\": \"$DP_ENTRY\", \"line_no\": $line}]}"
  api 200 '"down payment request \(.document_number): \(.status), cleared against \(.cleared_invoice_id) by entry \(.clearing_entry_id)"' \
    getDownPaymentRequest -p "id=$DP_REQUEST"
  expect "the migrated down payment request" "$(field .status)" cleared
  api 200 '.lines[] | "\(.account_id | acct): \(.amount.amount) \(.amount.currency)\(if .tax_code then ", tax code \(.tax_code)" else "" end)\(if .special_gl then ", special G/L \(.special_gl)" else "" end)"' \
    getJournalEntry -p "id=$(field .clearing_entry_id)"
  expect "the input tax reversed" "$(field '[.lines[] | select(.tax_code == "V1" and .special_gl == null) | .amount.amount] | join(" ")')" -100.00
  api 200 '"open down payments of the supplier: \([.items[] | select(.special_gl == "A")] | length)"' listOpenItems -p "partner_id=$DP_SUPPLIER" -p page_size=100
  expect "open down payments left" "$(field '[.items[] | select(.special_gl == "A")] | length')" 0
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

STORY='step_tenant step_pack step_migrate step_procure step_invoices step_credit step_payment step_statement step_fx step_ledgers step_tax step_centers step_cash step_reports step_grir step_downpayment step_audit'

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
