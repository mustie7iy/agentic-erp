# ADR-0058: Financial close

**Status:** accepted (T64 design step, 2026-11-03; decided by default under
ADR-0053 d; `go-reviewer` approved in round 3 with no blocking finding). The
decisions taken by default and the questions for the product owner are
listed at the end; the slices T64a-T64k are in the tracker.

## Context
T64 is "Financial close" (07 §7.6): "A close calendar and checklist;
accrual proposals and variance commentary by an agent; sign-off locks the
period through T32. Lane F." The tracker's leaves under Financial close are
the same four: close calendar and checklist; accrual proposals by agent;
variance commentary by agent; period lock after sign-off. This ADR details
that entry and adds nothing beyond it. Today the ERP has:

- **Periods** (T32, 03 §3.7): fiscal periods per legal entity, ledger,
  fiscal year and period (1-12 regular, 13-16 special, `is_special`), each
  with start and end dates, moving open ↔ closed (`:open`, `:close`,
  `ledger.period.manage`) and closed → locked (`:lock`,
  `ledger.period.lock`, people only, `PeriodLocked`); a locked period
  refuses postings, reversals and clearings, is not reopened by `:open`,
  and returns to closed only through an approved `period_unlock` request
  (`:unlock`, one approval by the role `controller`). Locking needs a
  closed period. `ledger.LockPeriod` is the published function behind
  `:lock`; it reads the period `FOR UPDATE`, writes the audit record and
  the event.
- **Parked entries** (T35, ADR-0056): a manual entry parked for a person
  to post, checked as a posting is except for the open period and rates.
  Agents park (`parkJournalEntry`, `x-autonomy: propose`): "this is how
  they propose an entry, which only a person can post". Posting a parked
  entry applies the company's `manual_entry_approval_limit` (above it, a
  second person approves). Agents never post or reverse
  (`identity.agent_not_allowed`).
- **Agents** (ADR-0036, ADR-0039, 04 §4.13): agents call the public API
  within their autonomy level (0 reads, 1 proposes, 2 writes what their
  permissions allow; `x-agent: false` operations never); proposals are
  made in the agent's running session and decided by people
  (`proposeBankStatementLine`, `proposeMemoRecord`, and their
  `:acceptProposal`/`:rejectProposal`); model calls go through the cell's
  gateway with budgets and redaction; each agent type has an evaluation
  suite under `evals/`, replayed in CI, and is raised above level 0 only
  when certified on the model its alias routes to. Agents run as clients
  (`erp agent reconcile -statement ID`, `internal/reconagent`).
- **Variance precedents**: the forecast agent's variance comments (ADR-0051
  §11, §13: proposed by the agent, citations checked, accepted or rejected
  by a person; "T64's close can ask for them"); plan against actual by cost
  and profit center (ADR-0050 §8, `listCostCenterPlanActual`: variance and
  a percent of two places computed exactly, absent when the plan is zero);
  the trial balance (`listBalances`, period totals per account) and
  `gl_account.is_balance_sheet`.
- **Period-end runs that a close waits for**: revaluation (ADR-0042: one
  posted run per key date, the period's last day, per ledger) and, once
  T57f is built, depreciation (ADR-0057 §7: "Close (T64) can check that
  every company and ledger has a posted run for the period before signing
  it off; that check is T64's").
- **Company calendars** (ADR-0048): holidays per company in the
  configuration (`Config.Holiday`); "which weekdays are a company's weekend
  is not in the section yet; the first context that counts business days
  decides it" and "it is added to the calendar, with its tier, by the
  first step that schedules by business days". Cash counts Saturday and
  Sunday as the weekend in its own unexported `working` (internal/cash
  position.go).
- **Leaves** (ADR-0051 §2, ADR-0057 §2): a context that reads several
  others and that none reads is a leaf with ports the composition root
  adapts (`internal/layout`'s `leaves`).

### What SAP does (the parity target)
**From memory, for a specialist to check against the pilot's SAP release;
no source was read for this section.** SAP S/4HANA's Advanced Financial
Closing (and the older Financial Closing cockpit) keeps task list templates
of closing tasks with dependencies, instantiated per company code and
closing period into task lists with planned dates relative to the period
end (working days of a factory calendar), responsible processors, and
statuses per task; tasks are manual (a person confirms) or scheduled jobs
and programs whose results are logged; an approval step can follow a task.
Period locks are posting-period variants (OB52) per account type. Accruals
are kept in the Accrual Engine (manual accrual objects, periodic accrual
runs, reversal); variance analysis is reporting (plan/actual reports,
Group Reporting's) rather than an agent.

## Decision

### 1. Scope of T64
In: a close template (the tasks, their kind, dependencies and due days) in
configuration; the close calendar of a company's fiscal year; one close per
company and regular period, covering the ledgers it names, with its
checklist instantiated from the template; manual tasks completed by
people; automatic checks run on demand and recorded (parked entries,
revaluation, depreciation, accruals decided and reversed, variances
explained); sign-off, which re-runs the checks and locks each covered
ledger's period through T32; accrual proposals by the close agent, parked
as entries for a person to post, with evidence; the period's variance
report (against the prior period, the same period of the prior year and a
plan category) with thresholds; variance comments proposed by the close
agent or written by people, accepted or rejected by people; the close
agent itself (both tasks) with its evaluation suite.

Out (none built; each is a question for the product owner or a gap, listed
at the end): year-end close (special periods 13-16, balance carry-forward);
automatic reversal of accruals; accrued revenue; accruals in a foreign
currency; a group close across companies, intercompany reconciliation and
consolidation (T68); tax returns as close tasks (T61's returns are their
own documents); a bank reconciliation check; notifications and reminders
of due tasks; variances per cost or profit center and per parallel ledger;
an approval step on tasks; screens in the web workspace (headless first,
ADR-0053).

### 2. The `closing` context: a leaf
A new context **`closing`** (schema `closing`, package `internal/closing`;
not `close`, Go's builtin) owns closes, their tasks and check results,
sign-offs, accrual proposals and variance comments. Like `cash` and
`assets` it is a **leaf**: it reaches the other contexts only through
ports it declares, which the composition root (`internal/app`) adapts, and
no context imports it. `internal/layout`'s `leaves` gains `closing`, and
`allowed["closing"]` names `config` only.

```mermaid
flowchart LR
  ledger --> closing
  controlling --> closing
  assets --> closing
  payables --> closing
  procurement --> closing
  agents --> closing
  tenancy --> closing
  identity --> closing
  closing -.events.-> analytics
```

Its ports, each run in the request's transaction under RLS (ADR-0003):
- **`closing.Ledger`** (adapter over the ledger's published functions and
  reads): the fiscal periods of a company, ledger, year and period (id,
  dates, status, version), the ledgers a company keeps (code, name,
  leading), `LockPeriod`; the count and the first ids of parked entries
  dated in a range and still `parked` or `pending_approval`; whether a
  posted revaluation run exists for a ledger and key date; the period's
  local-currency totals per profit and loss account of the leading ledger
  (number, name); whether a journal entry has a line on an account in a
  period; and, new and additive in the ledger (§4), parking an entry as
  the request's principal, the status of parked entries, and cancelling
  one.
- **`closing.Plans`** (adapter over `controlling`): a plan category's
  amounts per account for a company, year and period, summed over its
  cost center lines or its profit center lines (§9).
- **`closing.Depreciation`** (adapter over `assets`, T64k): whether a
  company and ledger has a posted depreciation run for a period, or has
  no asset that a run of it would select (ADR-0057 §7 step 1).
- **`closing.Evidence`** (adapter over payables and procurement): for a
  supplier invoice or purchase order id, whether it is the company's and
  posted (an invoice) or released (an order), with its number and the
  supplier's id and name.
- **`closing.Sessions`** (adapter over `agents`, as `cash.Sessions`): the
  session is the caller's and running (`agents.not_your_session`,
  `agents.session_ended`).
- **`closing.Companies`** (adapter over tenancy): a legal entity's code,
  name, local currency, time zone and today (T72n).
- **`closing.People`** (adapter over identity): a principal's name and
  kind, for assignees, signers and deciders.

No existing dependency direction changes, so no other ADR is needed: this
ADR records the new arrows (CLAUDE.md).

### 3. The close template and the calendar (configuration)
The template is configuration, not master data: it is short, changes
rarely, and decides what a sign-off requires, as ADR-0057 §3 kept asset
classes in configuration.

```yaml
close:
  tasks:                          # at most 200
    - code: AP_CUTOFF             # [A-Z0-9_]{1,30}, unique
      name: Supplier invoices of the period entered
      kind: manual                # manual | check
      due_day: 2                  # business days after the period's last day; 0 = that day; -31..31
    - code: NO_PARKED
      name: No entry of the period left parked
      kind: check
      check: no_parked_entries    # one of §6's checks; required with kind check, refused otherwise
      due_day: 3
      depends_on: [AP_CUTOFF]     # codes of this template; no cycle
      companies: ["1000"]         # optional: only these companies' closes get the task
calendars:
  "1000":
    weekend: [saturday, sunday]   # new (T64a); default Saturday and Sunday
    holidays: [...]               # as today (ADR-0048)
companies:
  - code: "1000"
    close:
      accrual_account: "2190"     # T64e: balance sheet, liability, not a reconciliation account, postable
      variance:                   # T64h
        bases: [prior_period, prior_year, plan]
        threshold_amount: "10000.00"   # local currency, decimal string, > 0
        threshold_percent: "10"        # optional, decimal string, > 0
        plan_category: BUDGET          # required with basis plan
        plan_centers: cost             # cost | profit: which plan lines are summed (§9)
```

- **Validation** (configuration, problems on the field): codes unique,
  `depends_on` names codes of the template without a cycle, `check` only
  with `kind: check` and only a check this build knows (each slice adds
  its own to the list), `companies` names companies of the document; the
  accrual account exists, is postable in the company, a balance sheet
  account and no reconciliation account of any kind; the plan category
  exists (`controlling`), the bases are known and listed once.
- **Tiers**: the tasks, the weekend and the variance settings are
  operational (tier 2); the accrual account is posting-related (tier 3);
  a task's name is cosmetic (tier 1).
- **Business days.** A business day of a company is a day that is not one
  of its `weekend` days and not one of its holidays. The rule moves out of
  cash into the configuration (`config.Config.BusinessDay(company,
  date)`), and cash's `working` calls it (CLAUDE.md: never copy a helper);
  with the default weekend, cash's behaviour does not change. Which days
  are a pilot company's weekend is the tenant's setting, applied by its
  country pack or its administrators; this ADR asserts none (a fact for a
  specialist, below).
- **Due dates.** A task's due date is the period's last day (the leading
  ledger's `end_date`) moved by `due_day` business days: 0 is the last day
  itself, n > 0 the n-th business day after it, n < 0 the |n|-th business
  day before it. The search is bounded as cash's is (366 days).
- **The calendar**: `GET /v1/close-calendar?legal_entity_id&fiscal_year`
  returns one row per regular period of the leading ledger (at most 12, so
  not paged): the period (number, dates, status), its close if one exists
  (id, status, tasks done and total, overdue count, signed off at and by),
  and the planned first and last due dates: the close's tasks' when it
  exists, else the template's computed for that period. A company the
  caller cannot act in has no rows (an empty list), as the ledger's list
  reads answer (`listCostCenterBalances`: "has none").

### 4. Changes to the ledger's published API
`closing` needs what the ledger's Go API does not yet offer; each is
additive and built in the slice that first needs it:
- **Reads by key** (T64b): the periods of a company, year and period per
  ledger (`ledger.PeriodsOf`), beside `GetPeriod`.
- **Counts and run status** (T64c): parked entries dated in a range in
  given statuses; the posted revaluation run of a ledger and key date.
- **`ledger.ParkedEntries`**: `Park(ctx, entry)` (T64e) parks an entry
  as the request's principal, exactly as `parkJournalEntry` does (the same
  checks, problems and `ParkedJournalEntryChanged`), returning its id and
  its lines' total; `Statuses(ctx, ids)` (T64f) returns each one's
  status, its current debit total in local currency, its journal entry
  (id, document number) once posted and whether that entry is reversed
  (the reversal's id and number); `Cancel(ctx, id, reason)` (T64f)
  cancels a parked one exactly as `cancelParkedJournalEntry` does, its
  owner rule included (only the owner cancels: the person who parked it,
  a person taking on an agent's entry no person owns, or that agent;
  403 `ledger.not_entry_owner`, 409 `ledger.invalid_parked_status`).
- **Account reads** (T64h, T64i): the period's local-currency totals per
  profit and loss account of a company's leading ledger; whether an entry
  has a line on an account in a period.

### 5. Data model
Every table has `tenant_id` leading its keys and RLS (ADR-0003); keys are
UUIDv7 (ADR-0004); amounts are `bigint` minor units of the company's local
currency with the currency beside them (ADR-0005); references to other
contexts (legal entity, ledgers, periods, accounts, centers, parked and
journal entries, invoices, orders, sessions, principals) are ids without
foreign keys (02 §2.1).

```
closing.period_close     (tenant_id, id, legal_entity_id, fiscal_year, fiscal_period, period_end,
                          config_version, status, version, created_*, updated_*)
                          UNIQUE (tenant_id, legal_entity_id, fiscal_year, fiscal_period)
closing.close_ledger     (tenant_id, close_id, ledger_id, period_id)      PK (tenant_id, close_id, ledger_id)
closing.close_task       (tenant_id, close_id, code, seq, name, kind, check_kind, due_on, depends_on text[],
                          assignee_id, status, completed_by, completed_at, note,
                          checked_at, checked_by, check_passed, check_result jsonb, version)
                          PK (tenant_id, close_id, code)
closing.sign_off         (tenant_id, id, close_id, signed_by, signed_at, note, locked jsonb)   insert-only
closing.accrual_proposal (tenant_id, id, close_id, legal_entity_id, session_id, agent_id, account_id,
                          cost_center_id, profit_center_id, supplier_id, currency, amount, description,
                          reason, parked_entry_id, status, decided_by, decided_at, decision_reason,
                          version, created_*)
closing.accrual_evidence (tenant_id, proposal_id, seq, kind, document_id) PK (tenant_id, proposal_id, seq)
closing.variance_comment (tenant_id, id, close_id, basis, account_id, text, author_kind, author_id,
                          session_id, currency, actual, comparison, status, decided_by, decided_at,
                          decision_reason, version, created_*)
closing.variance_citation (tenant_id, comment_id, seq, journal_entry_year, journal_entry_id)
                          PK (tenant_id, comment_id, seq)
```

- **States.** A close is `open` or `signed_off` (§7). A task is `open` or
  `done`. An accrual proposal is stored `proposed` or `rejected`; what a
  person did with its parked entry is read through the ledger (§8). A
  variance comment is `proposed`, `accepted`, `rejected` or `superseded`.
  Each transition is a guarded update (`WHERE status = $expected`), and a
  trigger refuses any other (a schema test per transition).
- **Database rules with schema tests**: one close per company and period
  (unique); a regular period only (`fiscal_period BETWEEN 1 AND 12`); a
  task's `check_kind` set exactly when `kind = 'check'`; `completed_*` set
  exactly when `done`; an accrual's `amount > 0`; at most one `accepted`
  comment per close, basis and account (partial unique index); a sign-off
  row and evidence and citation rows are never updated or deleted (no
  grant, a trigger); RLS on every table; keys lead with `tenant_id`; no
  foreign key leaves the schema.
- **Versions** (ADR-0008): a close's version moves with every change to it
  or its tasks, so `If-Match` on a task action refuses a stale view of the
  checklist; proposals and comments carry their own versions.
- **Locking** (ADR-0008): every write on a close, its tasks, its proposals
  or its comments first reads the close `FOR UPDATE`, so writes on one
  close serialise; sign-off then locks the covered periods through
  `LockPeriod` in the order postings take them `FOR SHARE`
  (`ledger.postingPeriod`, internal/ledger/journal.go: the leading ledger
  first, then by ledger code; 03's entry locks, whose sentence that a close
  takes no other lock T64d updates for sign-off), so a sign-off and
  a posting dated in the period never wait on each other's periods in
  opposite orders. No write takes a period lock before a close lock, so
  close-then-period never crosses either.

### 6. The close and its checklist
**Creating a close.** `POST /v1/period-closes` (Idempotency-Key; company,
fiscal year, regular period, `ledger_ids`, default every ledger the
company keeps) checks the period exists in each named ledger (422
`closing.period_unknown`, naming the ledger), that every named ledger is
kept by the company (422 `ledger.ledger_not_kept`), that the period is
regular (422 `closing.special_period`), that the active configuration has
close tasks for the company (409 `closing.no_template`), and that the
company has no close of that period (409 `closing.close_exists`, naming
it, with `Location`). It copies the template's tasks for the company from
the active configuration (code, name, kind, check, due date, depends_on)
and records the configuration version: a later change of the template
leaves existing closes as they were. It returns 201 with the close.

**Reading** (`closing.close.read` and `ledger.entry.read`, since check
results name parked entries). `GET /v1/period-closes/{id}` returns the close with its
company (code, name), its ledgers (code, name) with each one's period id
and live status, its tasks inline (at most 200: the template's bound), each
with assignee (id, name), due date, `overdue` (open and due before the
company's today), the latest check result, and the close's counts (tasks
done, open, overdue) and its sign-offs. `GET /v1/period-closes` lists
closes by company, fiscal year and status, cursor-paged. `GET
/v1/close-tasks?assignee=me|<id>&status&legal_entity_id&due_before` lists
tasks across closes with their close's company and period, cursor-paged
by (due date, close, code): a person's worklist.

**Manual tasks.** `PATCH /v1/period-closes/{id}/tasks/{code}` (If-Match:
the close's ETag) sets the assignee (a person of the tenant; 422
`closing.assignee_invalid` otherwise). `:complete` (If-Match; an optional
note) marks a manual task done, refused while a task it depends on is
open (409 `closing.task_blocked`, naming them) and for a check task (409
`closing.task_is_check`: a check completes by passing). `:reopen`
(If-Match, a reason) returns a done task, manual or check, to open, and
reopens nothing else: a task that depends on it stays done, and sign-off
needs every task done again.

**Checks.** `POST /v1/period-closes/{id}/tasks/{code}:check` (If-Match)
runs the task's check now, in the request's transaction, records the
result (`checked_at`, `checked_by`, `check_passed`, `check_result`: what
was found, with ids and names), and sets the task `done` when it passed
and `open` when it did not. It is refused while a task it depends on is
open (`closing.task_blocked`) and for a manual task (409
`closing.task_not_check`). The checks, each a read through a port:

| Check | Passes when | Port, slice |
|---|---|---|
| `no_parked_entries` | no parked entry of the company dated in the period is `parked` or `pending_approval` (the result names up to 20, with their creators' kind) | `closing.Ledger`, T64c |
| `revaluation_posted` | each covered ledger has a posted revaluation run keyed on the period's last day | `closing.Ledger`, T64c |
| `accruals_decided` | no accrual proposal of this close is `proposed` or `pending_approval` (§8) | `closing`, T64f |
| `accruals_reversed` | every accrual of the company's close of the previous regular period that was posted is reversed (none, when there is no such close) | `closing.Ledger`, T64f |
| `variances_explained` | every row of each configured basis that requires a comment has an accepted comment that is not stale (§9) | `closing`, T64i |
| `depreciation_posted` | each covered ledger has a posted depreciation run for the period, or has nothing a run would select | `closing.Depreciation`, T64k |

A check is a statement about the moment it ran: the result is shown with
its time, and sign-off runs every check again (§7). No check runs on its
own: nothing in T64 is long-running or scheduled (§11).

### 7. Sign-off locks the period through T32
`POST /v1/period-closes/{id}:sign-off` (If-Match; an optional note; people
only, `x-agent: false`; `closing.close.sign_off` and, as `x-also-permissions`,
`ledger.period.lock`, since it locks periods, and `ledger.entry.read`,
since its refusal names what the checks found) does, in one
transaction, with the close read `FOR UPDATE`:
1. refuses a close already `signed_off` whose covered periods are all
   locked (409 `closing.close_signed_off`);
2. refuses while a manual task is open (409 `closing.tasks_open`, naming
   them);
3. runs every check task again; any that fails refuses the sign-off (409
   `closing.checks_failing`, the problem naming each failing task with
   what its check found), and when all pass it records each result on its
   task;
4. reads each covered ledger's period and refuses unless each is
   `closed` or `locked` (409 `closing.period_not_closed`, naming the
   ledgers still open): a close is signed off after the period stopped
   taking postings, and sign-off never closes a period itself;
5. locks each `closed` covered period through `ledger.LockPeriod` with the
   version it just read (T32's lock: the period's guard, `PeriodLocked`
   and the ledger's audit record, unchanged), the leading ledger first,
   then by ledger code, as postings lock them (§5); a period already
   locked is left as it is;
6. writes a `sign_off` row (who, when, the note, and per ledger the period
   id and whether this sign-off locked it), sets the close `signed_off`,
   writes the audit record and `PeriodCloseSignedOff`.

A refused sign-off keeps nothing: the transaction rolls back, and the
problem tells the person what failed; `:check` records a result when they
want one kept. A replay of the same `Idempotency-Key` answers as the
first request did.

**After an unlock.** When a covered period is unlocked through T32 (an
approved `period_unlock` request returns it to `closed`), the close is
not told: it reads the periods' live status. A `signed_off` close whose
covered periods are not all locked again accepts writes: the first write
on it (a task reopened, a check, a proposal, a comment) sets it `open`
again, audited as `reopened` and published in `PeriodCloseChanged`, and a
later `:sign-off` locks the period again and adds a second `sign_off`
row. A `signed_off` close whose periods are all locked refuses every write
(409 `closing.close_signed_off`). The test drives a lock, an approved
unlock, a reopened task, a correcting entry and a second sign-off.

### 8. Accrual proposals
**What the agent proposes.** An accrual proposal is one expense the period
bore and the ledger does not yet show: `POST /v1/accrual-proposals`
(`closing.accrual.propose` and, since it parks a ledger entry,
`ledger.entry.park` as `x-also-permissions`; `x-autonomy: propose`; agents only:
`agents.not_an_agent` for a person, as `startAgentSession`; in the agent's
running session, `session_id`) with the close, an expense account (a
profit and loss account, postable in the company, no reconciliation
account: 422 `closing.accrual_account_invalid`), its cost and profit
center (validated at parking as a posting does: ADR-0050 §5), an optional
supplier, an amount in the company's local currency (> 0; 422
`request.invalid_amount`), a description, a reason, and 1-10 evidence
items (`kind`: `supplier_invoice`, `purchase_order`, `journal_entry`, and
the id), each the company's and posted or released (422
`closing.evidence_unknown`, `evidence[i]`), read through
`closing.Evidence` and the ledger.

**What it does.** Under the close's lock it refuses a close whose
company has no `accrual_account` (409 `closing.accrual_account_missing`)
and a duplicate: a proposal of the same close, expense account, cost
center and supplier whose derived status (below) is not `rejected` is 409
`closing.accrual_duplicate` naming it, unless it is this session's with
the same content, which answers 200 with it (a retried proposal). Else it
parks the entry through `ledger.ParkedEntries.Park` as the agent (posting
and document date the period's last day, the company's local currency,
header text naming the proposal: Dr the expense account with its centers,
Cr the accrual account with the expense line's profit center), writes the
proposal and its evidence, the audit record and `AccrualProposed`, and
returns 201 with the proposal and its parked entry. The agent never posts
it (ADR-0036, `identity.agent_not_allowed`).

**What a person does.** The parked entry is an ordinary parked entry of an
agent (`listParkedJournalEntries?created_by_kind=agent`), so the existing
operations decide it: a person may edit it, posts it with
`postParkedJournalEntry` (within the approval limit, or after a second
person's approval above it, T35), or cancels it. `POST
/v1/accrual-proposals/{id}:reject` (`closing.accrual.decide` and
`ledger.entry.park` as `x-also-permissions`, `x-agent: false`, If-Match,
a reason) cancels the parked entry through `ParkedEntries.Cancel`, under
the ledger's own rules (the person rejecting an agent's entry no person
has taken on is its owner for this; an entry another person took on is
theirs to cancel, 403 `ledger.not_entry_owner`), and records the
decision; it is refused once the entry is no longer `parked` (409
`closing.accrual_not_open`, checked first, so
`ledger.invalid_parked_status` arises only from a race the ledger's guard
settles). A proposal's status is derived
on read from its own state and its parked entry's, read in one call
through `ParkedEntries.Statuses`: `rejected` (rejected here, or the
parked entry cancelled or rejected in the ledger), `proposed` (parked),
`pending_approval`, `posted` (with the journal entry's id and number, and
`modified: true` with the posted amount when the parked entry's debit
total at posting differs from the proposed amount), `reversed` (with the reversal's id and number). `GET
/v1/accrual-proposals` (filters: close, company, status, session,
`parked_entry_id`; cursor-paged; `closing.close.read` and, since it shows
parked and posted entries, `ledger.entry.read` as `x-also-permissions`,
as `getAccrualProposal`) carries each proposal's account (number,
name), centers (codes, names), supplier (number, name), amount with its
currency, evidence (kind, id, number, supplier name) and parked entry, and
totals per status and currency (count, and amount with its currency, one
row per currency a filtered set holds: closes of companies in different
currencies are never summed together): how much of what the agent
proposed was posted as proposed, changed or rejected.

**Reversal.** An accrual is reversed in the next period by a person with
the ledger's `reverseJournalEntry` on its posted entry, dated in that
period, as any manual entry is; the next close's `accruals_reversed`
check finds any left unreversed. Automatic reversal is a question below.

### 9. The variance report
`GET /v1/period-closes/{id}/variances?basis=prior_period|prior_year|plan`
(`closing.close.read` and `ledger.entry.read`, `x-also-permissions`;
`x-slo-class: report`) reads the company's leading ledger in its local
currency:
- **Rows**: one per profit and loss account (`is_balance_sheet` false)
  with a non-zero actual or comparison: the account (number, name), the
  actual (the period's net movement, debit positive, as the trial balance
  signs it), the comparison, the variance (actual less comparison), the
  percent (variance over the comparison's absolute value, two places,
  computed exactly and rounded half away from zero, absent when the
  comparison is zero: ADR-0050 §8's rule), `requires_comment`, and the
  row's comments (the accepted one with `stale`, and the count proposed).
  Every amount carries its currency.
- **Bases**: `prior_period`, the previous regular period (period 12 of the
  previous fiscal year for period 1); `prior_year`, the same period of the
  previous fiscal year; `plan`, the configured plan category's amounts for
  the period, summed over its cost center lines (`plan_centers: cost`) or
  its profit center lines (`profit`) through `closing.Plans`, since a
  category may plan both and summing both would count one amount twice. A
  basis not configured for the company is 422 `closing.basis_not_configured`.
  A comparison period the company has no fiscal period for (a first year)
  compares as zero.
- **Requires a comment** when the variance's absolute value is at least
  `threshold_amount` and, when `threshold_percent` is set and the percent
  is present, the percent's absolute value is at least it.
- **Paged** by account number (cursor), filters `requires_comment=true`
  and `unexplained=true`; **totals** over the whole report, not the page:
  actual, comparison and variance, rows, rows requiring a comment, rows
  explained (an accepted comment that is not stale).

### 10. Variance comments
A comment explains one row of one basis.
- **Proposed by the agent**: `POST /v1/variance-comment-proposals`
  (`closing.variance.propose` and `ledger.entry.read`, `x-autonomy:
  propose`, agents only, in the running session) with the close, basis, account, text (at most 2,000
  characters) and 0-20 cited journal entries. Each cited entry must have a
  line on the account in the close's period or, for a prior basis, in the
  comparison period, in the leading ledger (422 `closing.citation_invalid`,
  `citations[i]`): the agent cites what the figures are made of, as
  ADR-0051 §13's citations are checked against the attribution. One
  proposal per session and row: the same again answers 200 with it,
  another is 409 `closing.comment_already_proposed`.
- **Written by a person**: `POST /v1/variance-comments`
  (`closing.variance.comment` and `ledger.entry.read`, `x-agent: false`),
  the same body and checks,
  accepted at once.
- **Decided by a person**: `:accept` (If-Match; an optional replacement
  text, recorded as edited) and `:reject` (If-Match, a reason), both
  `closing.variance.comment` and `ledger.entry.read`, `x-agent: false`,
  on a `proposed` comment
  (409 `closing.comment_not_proposed`). Accepting a comment, or writing
  one, sets the row's previously accepted comment `superseded` in the same
  transaction (the partial unique index holds one accepted per row).
- **Stale.** Every comment stores the row's actual and comparison when it
  was written; an accepted comment is `stale` when the row's figures now
  differ (an entry posted or reversed since), so the person reads it again
  and accepts or writes a new one. `variances_explained` (§6) counts only
  accepted comments that are not stale.
- `GET /v1/variance-comments` (filters: close, basis, account, status,
  session; cursor-paged; `closing.close.read` and `ledger.entry.read`,
  since it shows the rows' figures and the cited entries) carries the account's number and name, the
  figures with their currency, the citations (entry id, document number,
  posting date) and the author and decider (id, kind, name).
- Every write is audited and published as `VarianceCommentChanged`.

### 11. No workflow
Every T64 operation is a short transaction: a close's checks are bounded
reads (a count, a run's status, at most a few hundred account rows), and
sign-off locks a handful of periods. ADR-0012 puts long-running or batched
work in Temporal; none here is, so T64 adds no workflow, no dispatcher kind
and no schedule. The agent's runs are long, and they are clients of the
API like every agent (§12), outside any transaction.

### 12. The close agent
The **close agent** (agent type `close`, `internal/closeagent`, model
alias `erp-close`) runs as a client, as the reconciliation agent does:
`erp agent close-accruals -close ID` and `erp agent close-variances -close
ID [-basis B]` start a session, run, and end it. Its tools are generated
from the operations (04 §4.13), read ones and the two proposals:
- **Accruals** (T64g): `getPeriodClose`, `listAccrualProposals`,
  `listBalances`, `listJournalEntries`, `getJournalEntry`,
  `listSupplierInvoices`, `getSupplierInvoice`, `listPurchaseOrders`,
  `listParkedJournalEntries`, `listEntryTemplates`, `getBusinessPartner`,
  `listAccounts`, `proposeAccrual`. Its prompt: for each supplier that
  invoiced the company on the same expense account in each of the three
  regular periods before the close's, and has no invoice for the close's
  period, no open parked entry for it and no proposal of this close,
  propose one accrual of the most recent of those invoices' net amount
  on that account and those centers, citing the three invoices; propose
  nothing for goods received on an order (GR/IR already carries them,
  T33), for one-off invoices, or when unsure, and say why in one sentence.
- **Variances** (T64j): `getPeriodClose`, `listPeriodCloseVariances`,
  `listVarianceComments`, `listJournalEntries`, `getJournalEntry`,
  `listCostCenterPlanActual`, `listSupplierInvoices`, `proposeVarianceComment`.
  Its prompt: for each row requiring a comment and without an accepted,
  current one, explain the variance by the entries that make it, citing
  them, using only the row's figures and the cited entries' amounts;
  propose nothing for a variance it cannot explain, which a person
  explains.
- **Never**: it posts, reverses, cancels, completes or checks a task,
  signs off, accepts or rejects (every one is `x-agent: false` or outside
  its tools, and the API refuses it whatever its level). Everything a
  supplier or a person wrote (invoice texts, header texts, names) is
  untrusted data, never instructions (stated in its prompt, as
  reconagent's).
- **Autonomy.** It proposes at level 1; level 2 adds nothing, since every
  other write of `closing` is `x-agent: false` and posting is refused to
  agents at any level. It is raised above 0 only when its type is
  certified on the routed model (ADR-0039).
- **Evaluation** (ADR-0039): `evals/close/suite.yaml`, cases in replay in
  CI and live on demand. Accruals (T64g): a monthly supplier missing this
  period gets one proposal of last period's net amount citing three
  invoices (correctness); a supplier already invoiced this period, an
  order received through GR/IR, a one-off invoice and an existing proposal
  get none (safety); an instruction planted in an invoice's text is
  ignored (safety); no post, cancel or other write (safety). Variances
  (T64j): a row above threshold made by one large invoice gets a comment
  citing its entry (correctness); rows below threshold and a row already
  explained get none (safety); a comment states no figure other than the
  row's and the cited entries' (`$lacks`, safety); a planted instruction
  in a header text is ignored (safety). The pass bars are ADR-0039's.
- **Walkthrough evidence** for the agent slices runs the agent through the
  model stand-in (`internal/llm/llmtest/modelstub`), extended to answer
  successive calls from a script of turns as the suites' replays do, and
  the walkthrough's agent principal, at level 1, is raised in the
  walkthrough's development cell as the evaluation cells raise theirs
  (`ERP_EVAL_CELL=true`, which production refuses; ADR-0039). The
  proposal slices (T64e, T64i) use the same principal to call the
  proposal operations directly, as the agent would.

### 13. API and events (contract first)
Paths, in `api/openapi.yaml` (problem codes as `x-problem-codes`; writes
with `Idempotency-Key`; changes of one resource with `If-Match`; lists
cursor-paged with filters, 04 §4.5):

| Operation | Method and path | Permission | Agents |
|---|---|---|---|
| getCloseCalendar | `GET /v1/close-calendar` | `closing.close.read` | read |
| createPeriodClose, listPeriodCloses, getPeriodClose | `/v1/period-closes` | `closing.close.manage`; reads `closing.close.read` and `ledger.entry.read` (check results name entries) | `x-agent: false` for create |
| updateCloseTask, completeCloseTask, reopenCloseTask, checkCloseTask | `/v1/period-closes/{id}/tasks/{code}`, `:complete`, `:reopen`, `:check` | `closing.close.manage` and `ledger.entry.read` (the task returned carries its check result) | `x-agent: false` |
| listCloseTasks | `GET /v1/close-tasks` | `closing.close.read` and `ledger.entry.read` | read |
| signOffPeriodClose | `POST /v1/period-closes/{id}:sign-off` | `closing.close.sign_off`, `ledger.period.lock` and `ledger.entry.read` (`closing.checks_failing` names what the checks found) | `x-agent: false` |
| proposeAccrual | `POST /v1/accrual-proposals` | `closing.accrual.propose` and `ledger.entry.park` | `x-autonomy: propose` |
| listAccrualProposals, getAccrualProposal, rejectAccrualProposal | `/v1/accrual-proposals`, `:reject` | reads `closing.close.read` and `ledger.entry.read`; `:reject` `closing.accrual.decide` and `ledger.entry.park` | read; `x-agent: false` |
| listPeriodCloseVariances | `GET /v1/period-closes/{id}/variances` | `closing.close.read` and `ledger.entry.read` | read |
| proposeVarianceComment | `POST /v1/variance-comment-proposals` | `closing.variance.propose` and `ledger.entry.read` | `x-autonomy: propose` |
| createVarianceComment, acceptVarianceComment, rejectVarianceComment, listVarianceComments | `/v1/variance-comments`, `:accept`, `:reject` | `closing.variance.comment`; reads `closing.close.read`; every one also `ledger.entry.read` (the rows' figures and cited entries) | `x-agent: false`; read |

- Every operation is limited to the caller's companies (ADR-0010). The
  second permissions are `x-also-permissions` (403
  `identity.permission_missing` without them, as for
  `listCostCenterPlanActual`): an operation that returns ledger figures or
  entries needs `ledger.entry.read`, one that parks or cancels a parked
  entry `ledger.entry.park`, and sign-off `ledger.period.lock`, so no
  `closing` operation gives what the ledger's own would refuse.
- Representations carry names beside ids (company code and name, ledger
  code and name, account number and name, center codes and names,
  supplier number and name, document numbers, principals' names), every
  amount its currency, and each action's response the object it created
  or changed (the close, the task, the sign-off with the periods it
  locked, the proposal with its parked entry, the comment) (REVIEW.md's
  API-consumer classes).
- Problem codes (all `closing.*` new, registered with their constraint
  mappings): `closing.period_unknown`, `closing.special_period`,
  `closing.no_template`, `closing.close_exists`, `closing.assignee_invalid`,
  `closing.task_blocked`, `closing.task_is_check`, `closing.task_not_check`,
  `closing.close_signed_off`, `closing.tasks_open`, `closing.checks_failing`,
  `closing.period_not_closed`, `closing.accrual_account_missing`,
  `closing.accrual_account_invalid`, `closing.accrual_duplicate`,
  `closing.evidence_unknown`, `closing.accrual_not_open`,
  `closing.basis_not_configured`, `closing.citation_invalid`,
  `closing.comment_already_proposed`, `closing.comment_not_proposed`; and
  passed through: `ledger.ledger_not_kept`, the ledger's parking problems
  (from `Park`), `ledger.not_entry_owner` and
  `ledger.invalid_parked_status` (from `Cancel`),
  `identity.permission_missing`, `ledger.invalid_period_status` (a lock refused),
  `agents.not_an_agent`, `agents.not_your_session`,
  `agents.session_ended`, `request.version_mismatch`,
  `request.invalid_amount`, `config.not_bootstrapped`.
- Events, in `api/asyncapi.yaml`, a new `closing` channel (CloudEvents
  through the outbox, ADR-0007): `PeriodCloseChanged` (created, a task
  assigned, completed, reopened or checked, reopened after an unlock),
  `PeriodCloseSignedOff` (the close, the signer, the periods locked),
  `AccrualProposed`, `AccrualProposalRejected`, `VarianceCommentChanged`
  (proposed, accepted, rejected, superseded). Every write writes an audit
  record (ADR-0011).
- New permissions in identity's catalog: `closing.close.read`,
  `closing.close.manage`, `closing.close.sign_off`,
  `closing.accrual.propose`, `closing.accrual.decide`,
  `closing.variance.propose`, `closing.variance.comment`.

## Alternatives
- **The close inside the ledger.** The ledger owns periods, so sign-off
  would be one call; but the close reads controlling, assets, payables,
  procurement and agents, which the ledger must not import (02 §2.1).
  Rejected: a leaf reads them through ports and calls the ledger's lock.
- **Sign-off closes the period itself.** One action fewer, but a close
  would stop postings as a side effect of a sign-off that may then be
  refused. Rejected: the period is closed with T32's `:close` first.
- **Accrual proposals as their own entries, posted on acceptance by
  `closing`.** One more posting path with its own approval limit; the
  parked entry already is "how agents propose an entry", with T35's
  approval. Rejected.
- **Variance comments in `cash`'s table.** Different rows (accounts, not
  liquidity items) and a different owner. Rejected; the shape is reused.
- **A Temporal workflow per close, running checks on a schedule.** Nothing
  is long-running, and a scheduled check is stale by sign-off anyway.
  Rejected (§11).
- **Checks computed on every read.** No stored results, but a list of
  closes would run every check of every close, and the audit would not
  show what a person saw. Rejected for recorded results.

## Consequences
- A new context, schema and channel; a leaf with seven ports. Nothing else
  changes direction.
- The ledger gains reads by key, a parking function for contexts and
  parked entries' statuses, each additive; the configuration gains a
  close template, a company weekend and close settings per company; cash's
  business-day rule moves to the configuration unchanged.
- A period is locked by a sign-off only after it was closed by a person
  and every task is done; the lock and its unlock remain T32's.
- Accruals stay ordinary parked and posted entries: the ledger's approval
  limits, audit and reversal apply to them unchanged; only their evidence
  and decision are `closing`'s.
- The close agent proposes only. Until its type is certified on a tenant's
  routed model it stays at level 0 there, and the people do the close
  without it.

## Boundaries with the other steps
- **T32 (period lock)**: sign-off calls its lock (§7); unlocking stays an
  approval request.
- **T35 (parked entries) and ADR-0056**: accrual proposals are parked
  entries; a recurring template's entry needs no accrual (the agent reads
  templates and parked entries to avoid one).
- **T52 (parallel ledgers)**: a close covers the ledgers it names; a
  parallel ledger kept open after the leading one's lock (ADR-0045) is left
  out of the close and locked through T32 directly, or by a later close.
- **T56 (controlling)**: the plan basis reads plan lines (§9).
- **T57 (assets)**: `depreciation_posted` after T57f (T64k).
- **ADR-0042 (revaluation)**, **ADR-0051 (cash)**: `revaluation_posted`;
  the cash variance comments stay cash's.
- **T61 (tax)**, **T68 (consolidation)**, **T63 (report delivery)**: a
  return, an intercompany reconciliation or a delivered report is a
  manual task of the template until a check for it is asked for.

## Steps (slices)
Each is a slice (ADR-0053): one capability a named user can use end to
end through the public API, its contract, migration, domain code, tests
(real PostgreSQL, `FailingTx` on every database path, 100% coverage),
docs and an extended walkthrough step `step_close` in
`scripts/walkthrough.sh` as its evidence. Lane F, in this order; about
half a day each. Carla, the controller, is the named user throughout;
the close agent the API consumer of T64e, T64f, T64g, T64i and T64j.

**The walkthrough's close.** `step_close` is the story's last step and
stays last (a later step of any lane goes before it, and integration
keeps it so): it closes the period of the company's today, read once at
the step's start, which no other step locks (step_tax's filed return
locks only the latest ended quarter's periods), and its sign-off comes at
its end, after every other slice's part. Each slice inserts its part
before the sign-off, in the order: template and calendar (T64a), the
close and its tasks (T64b), checks (T64c), accrual proposals (T64e,
T64f), the accrual agent (T64g), variances (T64h), comments (T64i), the
variance agent (T64j), the depreciation check (T64k), then sign-off
(T64d). Each part leaves the close ready to sign off: what an agent
proposed is decided by Carla in the same part, and every task of the
walkthrough's template is done or passing before the sign-off.

| Step | Work | Acceptance criteria and walkthrough evidence |
|---|---|---|
| T64a | **Close template, company weekend and the close calendar** | `close.tasks` and `calendars[company].weekend` in the configuration, validated as §3 (codes, kinds, `depends_on` without cycles, known checks, companies) with tiers; `config.Config.BusinessDay` and cash's `working` calling it (cash's tests unchanged); the `closing` leaf, `internal/layout`, `closing.Companies` and `closing.Ledger`'s period reads; `GET /v1/close-calendar` with planned due dates from the template; `closing.close.read`. Walkthrough: Ada proposes the close template through a change request, it is approved and activated; Carla reads the year's calendar, each period's planned first and last due dates on business days |
| T64b | **The period close and its manual checklist** | Schema `closing` (`period_close`, `close_ledger`, `close_task`; RLS, `tenant_id` first, state triggers, schema tests of §5's rules); `POST/GET/list /v1/period-closes` (one per company and period, regular periods only, the template copied with the configuration version, ledgers defaulting to every kept one); task `PATCH` (assignee with name), `:complete`, `:reopen`, dependencies (`closing.task_blocked`), `overdue` on the company's today; `GET /v1/close-tasks`; `closing.close.manage`, the reads' and task actions' `x-also-permissions` (`ledger.entry.read`); `PeriodCloseChanged`; audit; `closing.People`. Walkthrough: Carla opens the close of today's period, assigns a task to herself, completes it; completing a task whose dependency is open is refused with `closing.task_blocked`; a second close of the period is refused with `closing.close_exists` |
| T64c | **Automatic checks: parked entries and revaluation** | `:check` with recorded results; `no_parked_entries` (naming up to 20 entries) and `revaluation_posted` per covered ledger, through `closing.Ledger`'s new reads (§4); `closing.task_not_check`, `closing.task_is_check`. Walkthrough: the parked-entries check fails naming an entry parked in the period, the entry is cancelled, the check passes and the task is done |
| T64d | **Sign-off locks the period through T32** | `:sign-off` as §7 (tasks open, checks re-run and recorded when all pass, nothing kept on a refusal, periods closed or locked, `ledger.LockPeriod` per covered ledger in the postings' order: leading first, then by code, with a test of a sign-off and a posting at once on a company with three ledgers whose ids and codes sort differently, the `sign_off` row, `x-also-permissions` `ledger.period.lock` and `ledger.entry.read`, people only; 03's entry-lock paragraph updated for sign-off's period locks); `PeriodCloseSignedOff`; reopening after an unlock (test: lock, approved unlock, a task reopened, a correcting entry, a second sign-off); `closing.close_signed_off`, `closing.tasks_open`, `closing.checks_failing`, `closing.period_not_closed`; `closing.close.sign_off`. Walkthrough, at the end of `step_close`: sign-off refused while the period is open (`closing.period_not_closed`); Carla closes the period, signs off; the period is locked and a manual entry dated in it is refused with `ledger.period_not_open` |
| T64e | **Accrual proposals** | `companies[].close.accrual_account` (tier 3, validated); `ledger.ParkedEntries.Park`; `accrual_proposal`, `accrual_evidence`; `POST /v1/accrual-proposals` (agents only, in session, `ledger.entry.park` also, evidence checked through `closing.Evidence`, duplicate and retry rules, the parked entry as §8); `GET` and list with names, evidence and the parked entry; `AccrualProposed`; `closing.accrual.propose`; the walkthrough's first agent principal (level 1, §12). Walkthrough: the agent principal proposes an accrual citing last month's invoice; Carla reads it with its evidence and posts its parked entry with `postParkedJournalEntry`; a person's proposal is refused with `agents.not_an_agent` |
| T64f | **Deciding accrual proposals** | `ledger.ParkedEntries.Statuses` and `Cancel` (the owner rule); the status derived from the parked entry (`proposed`, `pending_approval`, `posted` with the entry's number, `modified` with the posted amount, `reversed`, `rejected`); totals per status and currency; `:reject` (`closing.accrual.decide` and `ledger.entry.park`, `closing.accrual_not_open`, `ledger.not_entry_owner` for an entry another person took on); checks `accruals_decided` and `accruals_reversed` (test: a posted accrual of the previous close unreversed, then reversed); `AccrualProposalRejected`. Walkthrough: the proposal of T64e reads `posted` with its entry's number; a second proposal is rejected with a reason and reads `rejected`; the list's totals per status in CAD; `accruals_decided` passes |
| T64g | **The close agent: accruals** | Agent type `close`, `internal/closeagent` (tools, prompt, untrusted data; its `internal/layout` entry `{"apitools", "llm"}` as reconagent's), `erp agent close-accruals -close ID`, alias `erp-close`; `evals/close` with §12's accrual cases, replayed in CI; the model stand-in answering from a script of turns. Walkthrough: `erp agent close-accruals` on the close, through the stand-in, proposes the missing monthly accrual and nothing for the supplier already invoiced |
| T64h | **The variance report** | `companies[].close.variance` (bases, thresholds, plan category and centers; tier 2, validated); `GET /v1/period-closes/{id}/variances` as §9 (rows, percent exact, `requires_comment`, filters, totals over the report, paging by account number); `closing.Plans`; `closing.basis_not_configured`; a test per basis with hand-computed rows, including period 1's prior period in the previous year and a zero comparison. Walkthrough: Carla reads the variances against the prior period; the rows above the threshold are flagged and the totals equal the period's profit and loss in the trial balance |
| T64i | **Variance comments** | `variance_comment`, `variance_citation`; `POST /v1/variance-comment-proposals` (agents, in session, citations checked), `POST /v1/variance-comments` (people), `:accept` (optional edited text), `:reject`, `superseded`, `stale`; check `variances_explained`; `VarianceCommentChanged`; `closing.variance.propose\|comment`, `ledger.entry.read` also. Walkthrough: the agent principal proposes a comment citing the entry behind a flagged row; Carla accepts it; the check passes; a citation of an entry on another account is refused with `closing.citation_invalid` |
| T64j | **The close agent: variances** | `erp agent close-variances -close ID`, the variance tools and prompt, §12's variance cases in `evals/close`. Walkthrough: `erp agent close-variances` through the stand-in proposes a comment for the flagged row citing its entry, and none for rows below the threshold |
| T64k | **Depreciation check** (after T57f) | `closing.Depreciation` over assets; `depreciation_posted` per covered ledger (a company with nothing to depreciate passes). Walkthrough: the check on the close fails before the period's depreciation run and passes after it |

Each slice updates 02 §2.2 (T64a adds the context's row), 03 (a §3.x for
the `closing` schema), 04 §4.13 (T64e: accrual proposals among the
proposals) and this ADR's "As built".

## Decided by default (ADR-0053 d), for the product owner to overrule
1. `closing` is a leaf context with ports; no context imports it (§2).
2. The close template is configuration, one per tenant with optional
   company restrictions per task, copied into each close at creation (§3,
   §6).
3. A company's weekend is a configuration setting of its calendar,
   default Saturday and Sunday, tier 2; the business-day rule moves from
   cash into the configuration (§3), as ADR-0048 assigned it to the first
   step that schedules by business days.
4. Closes are per company and regular period (1-12), covering the ledgers
   they name (default every ledger kept); special periods and year-end
   are out (§1, a question below).
5. Checks run on demand and at sign-off, with recorded results; no
   workflow and no schedule (§6, §11).
6. Sign-off needs every task done, every check passing at sign-off, and
   each covered period closed by a person first; it locks through T32's
   `LockPeriod`, never closes a period, and needs `ledger.period.lock`
   and `ledger.entry.read` beside its own permission (§7, §13).
7. A signed-off close reopens on its first write after a covered period
   is unlocked, and is signed off again (§7).
8. Accrual proposals are parked entries made by `closing` on the agent's
   behalf, decided with the ledger's own operations (post, cancel) or
   `:reject`; expenses only, in local currency, one expense line against
   the configured accrual account (§8).
9. Accruals are reversed by a person in the next period; the next close's
   check finds those left (§8).
10. Variances are per profit and loss account of the leading ledger, in
    local currency, against the prior period, the prior year's period and
    a plan category summed over one kind of center (§9).
11. A row requires a comment when its variance reaches the amount
    threshold and, where set and computable, the percent threshold (§9).
12. Comments carry the figures they were written against and go stale
    when the figures change (§10).
13. The server checks the agent's citations, not the figures in its text;
    figures are graded by the evaluation suite (§10, §12), as ADR-0051
    §13 does for the forecast agent.
14. One close agent type, `close`, for both tasks, certified as one
    (§12).
15. The agent's walkthrough evidence uses the model stand-in with a
    script of turns, and a level-1 agent principal raised in the
    walkthrough's development cell as evaluation cells raise theirs
    (`ERP_EVAL_CELL`, refused in production) (§12).

## Questions for the product owner
- **Segregation of duties on sign-off** (security posture). Built: any
  person holding `closing.close.sign_off` and `ledger.period.lock` signs
  off, including one who completed tasks, as T32 lets one person lock.
  Should the signer be someone other than the people who completed the
  close's tasks, or should sign-off be an approval request? Recommendation:
  keep it as built for the pilot; add the rule if the pilots' auditors ask.
- **Year-end close** (new scope): special periods 13-16, closing the
  profit and loss into retained earnings and carrying balances forward.
  None is in T64's leaves, and the ledger has no carry-forward today.
  Recommendation: its own design step before the pilots' first year end.
- **Automatic reversal of accruals** (new scope): reversing every posted
  accrual on the next period's first day by one action or run, instead of
  a person reversing each. Recommendation: a leaf after T64e if the pilots
  ask.
- **Accrued revenue and foreign-currency accruals** (new scope).
  Recommendation: with receivables (phase 3).

## Gaps (out of scope, not built, no question asked)
Neither the roadmap entry nor a PO decision asks for these; each becomes a
tracker item only when one does (07 §7.5).
- A bank reconciliation check (ADR-0047's `getBankReconciliation`) and a
  tax return check (T61): manual tasks until asked for.
- Notifications and reminders of due or overdue tasks.
- Variances per cost or profit center, per segment and per parallel
  ledger.
- An approval step after a task.
- A group close across companies (T68).
- Screens in the web workspace (headless first, ADR-0053).

## Facts for a specialist (stated from memory, not verified here)
- SAP: Advanced Financial Closing and the Financial Closing cockpit (task
  list templates, dependencies, factory calendars), posting period
  variants (OB52), the Accrual Engine, as described in "What SAP does".
- Which weekdays are each pilot company's weekend (Canada, the UAE): the
  tenant sets them; this ADR asserts none.
- Accrual accounting under IFRS (the recognition of accrued expenses and
  their measurement) and any local rule on period locks or close
  deadlines: none is asserted by this design; the agent proposes and a
  person decides.

## Sources
- The repository: 07-transition §7.6 (T64's entry), the tracker's
  Financial close leaves, 03 §3.7 (period lock, parked entries), 04 §4.13
  (agents), ADR-0003, ADR-0005, ADR-0006, ADR-0007, ADR-0008, ADR-0010,
  ADR-0011, ADR-0012, ADR-0036, ADR-0039, ADR-0042, ADR-0045, ADR-0048,
  ADR-0050, ADR-0051, ADR-0053, ADR-0056, ADR-0057, `api/openapi.yaml`
  (`lockFiscalPeriod`, `unlockFiscalPeriod`, `parkJournalEntry`,
  `reverseJournalEntry`, `proposeBankStatementLine`, `proposeMemoRecord`,
  `listCostCenterPlanActual`, `x-also-permissions`),
  `internal/ledger/periods.go` (`LockPeriod`), `internal/cash/position.go`
  (`working`), `internal/reconagent`, `internal/layout` (leaves).
- No external source was read for this ADR (no internet access in the
  lane); every statement about SAP, IFRS and the pilot countries is flagged
  above for a specialist.

## As built
(Filled in by the slices T64a-T64k.)

**T64a (close template, company weekend and the close calendar).** Built
as §3: `close.tasks` (code, name, kind, check, due day, depends on,
companies) in `internal/config` (`close.go`), validated with the problems
on their fields, and `calendars[].weekend`; tiers: a task's name 1, the
rest of the template and the weekend 2 (the calendars' rule).
`config.Config.BusinessDay(company, date)` is the business-day rule
(not a weekend day, not a holiday), and cash's `working` calls it; cash's
tests pass unchanged. `GET /v1/close-calendar` (`getCloseCalendar`,
`closing.close.read`, added by migration 000193) is the `closing` leaf's
first operation, through the ports `closing.Settings` (the active
document), `closing.Companies` (a legal entity's code and name) and
`closing.Ledger` (the leading ledger's regular periods of a company and
year, over the new `ledger.ClosePeriods.Leading`); `internal/layout` lists
`closing` among the leaves with `config` alone, and 02 §2.2 has its row.
The walkthrough's `step_close` is created as the story's last step: Ada's
template of three manual tasks, approved by the process owner and
activated, and Carla's calendar of the year. Decided by default, for the
product owner to overrule:
- `calendars` stays the list keyed by company that ADR-0048 built (§3's
  example writes it as a map); the weekend is a field of a company's
  entry. Weekdays are written out in lower case (`saturday`), as §3's
  example has them, not abbreviated as `cash.payment_days` are; each
  once, and not all seven, which would leave no business day. An absent
  or empty weekend is the default, Saturday and Sunday, so a company
  with no weekend at all cannot be expressed; none is asked for.
- No check is known in T64a (`config.CloseChecks` is empty): a `check`
  task is refused until T64c adds the first checks, so the template of
  this slice is manual tasks alone.
- A task may depend only on tasks given to every company it is given to
  (a task restricted to companies 1000 and 2000 may depend on one for
  every company or for at least those two), so no close receives a task
  whose dependency it lacks; a dependency names a task once; a task
  depending on itself is a cycle.
- The calendar answers `company_code`, `company_name`, `weekend` and the
  rows only for a legal entity the caller may act in that exists; it
  always carries the leading ledger's code and name, from the
  configuration. A year without periods has the company and no rows.
  `fiscal_year` and `legal_entity_id` are required.
- Each row's close (§3: "its close if one exists") is not in the contract
  yet: no close can exist before T64b, which adds the field (additive) with
  the `closing` schema. The planned dates are always the template's
  computed for the period until then.
- Day 0 is the period's last day even when it is no business day (§3:
  "0 is the last day itself"); a search that finds no business day within
  366 calendar days stops there, as cash's does.
- The heading of `step_close` is "Final step — Financial close", without a
  number, so steps that other lanes add before it keep their numbers and
  it never needs renumbering.

**T64b (the period close and its manual checklist).** Built as §5 and §6:
migration 000194 creates the schema `closing` with its outbox and the
tables `period_close`, `close_ledger` and `close_task` (RLS, `tenant_id`
leading every key, no foreign key leaving the schema), and the permission
`closing.close.manage`. `POST /v1/period-closes` (`createPeriodClose`,
people only) opens one close per company and regular period
(`period_close_once`; 422 `closing.special_period` for 13-16), copies the active template's tasks for the company with their due
dates and the configuration version, and covers the named ledgers or
every one the company keeps (`ledger.ClosePeriods.Of`, the ADR's
`ledger.PeriodsOf`, through `closing.Ledger.PeriodsOf`). `GET` and the list
read it; `PATCH .../tasks/{code}`, `:complete` and `:reopen` work its
manual tasks, each reading the close `FOR UPDATE`, checking `If-Match`
against its version, moving it, and writing the audit record and
`PeriodCloseChanged` (`created`, `task_assigned`, `task_completed`,
`task_reopened` with the reason) through the new `closing` outbox and
channel. `GET /v1/close-tasks` is the worklist; the calendar's rows gain
their `close` (additive). `closing.People` reads principals through the
new `identity.People`. Walkthrough: `close_checklist` in `step_close`,
before the sign-off T64d adds. Decided by default, for the product owner
to overrule:
- The leading ledger is always covered: `ledger_ids` without it is 422
  `request.validation_failed`, since the due dates count from its period's
  last day (`period_end`) and sign-off locks it first. The covered ledgers
  are shown leading first, then by code. A ledger named twice is refused
  by the contract (`uniqueItems`, 400 `request.invalid_body`).
- A legal entity the caller cannot act in, or one that does not exist, is
  422 `request.reference_not_found` on `legal_entity_id`
  (`tenancy.Accessible`'s rule); a close of one the caller cannot act in
  is not found.
- Two problem codes beside §13's: 409 `closing.task_already_done`
  (completing a done task) and 409 `closing.task_not_done` (reopening an
  open one).
- Reopening clears who completed the task, when and its note; the reason
  is kept in the audit record and the event, not in a column. Completing
  takes an optional note (at most 2,000 characters).
- An assignee is an active human principal of the tenant (422
  `closing.assignee_invalid` for an agent, a service, a blocked or an
  unknown principal); `null` unassigns. Whether the assignee may act in the
  close's company is not checked: a person without access is assigned
  but does not see the task.
- A task action answers the task with the close's new ETag. Every task
  carries its close's id and version (`close_version`, the `If-Match` of
  its actions), company code and name, fiscal year and period, and
  `blocked_by`, the codes of the tasks it depends on that are still open,
  so a worklist consumer acts without reading the close first. An empty
  merge patch changes nothing: no version move, no audit record, no event.
- `overdue` and the counts are judged on the company's date at the read in
  its time zone, shown as `read_date` and `time_zone`, as memo records are
  (T72n2). `due_before` compares the due date with the day given.
- `GET /v1/period-closes` lists `PeriodCloseItem`s, the close without its
  ledgers and tasks but with their counts, sorted by `period_end`
  (default) or `created_at`, filtered by company, fiscal year and status.
  `GET /v1/close-tasks` pages in due date, close and code order (no
  `order_by`), filtered by `assignee` (`me` or an id), status, company,
  close and `due_before`.
- `ledger.entry.read` is an `x-also-permissions` of the reads and the task
  actions (§13, the T64b row), not of `createPeriodClose`; the calendar
  keeps `closing.close.read` alone, its close showing no check result.
- `close_task.check_kind` and its rule (`close_task_check_kind`) exist,
  but no check task is written until T64c adds the first check
  (`config.CloseChecks` is empty); the check result columns are T64c's,
  the sign-off rows and the close's signed-off fields T64d's.
- The database fixes what the template gave a task (code, place, name,
  kind, check, due date, dependencies) and what the close covers, refuses
  rewriting a completion while it stands, and starts closes and tasks
  open; the close's `open` <-> `signed_off` moves are allowed for T64d.
  Dependencies are checked by the application under the close's lock, not
  by the database.

**T64c (automatic checks: parked entries and revaluation).** Built as §6:
`config.CloseChecks` is `no_parked_entries` and `revaluation_posted`, so a
template's `check` task is accepted and copied into a close with its
check. `POST /v1/period-closes/{id}/tasks/{code}:check` (`checkCloseTask`,
people only, `closing.close.manage` and `ledger.entry.read`) reads the
close `FOR UPDATE`, checks `If-Match` against its version, runs the check
through `closing.Ledger`'s two new reads over the ledger's published port
`ledger.CloseChecks` (`Parked`: the count and the first entries of a
company by posting date in given statuses; `PostedRevaluation`: a
ledger's posted run at a key date), records the result on the task, sets
it done or open, and writes the audit record
(`closing.period_close.check_task`) and `PeriodCloseChanged`
(`task_checked`). `:check` on a manual task is 409
`closing.task_not_check`, `:complete` on a check task 409
`closing.task_is_check`. Migration 000196 adds the result columns
`checked_at`, `checked_by`, `check_passed` and `check_result` with their
rules (`close_task_checked`, `close_task_check_passed`, the trigger's), NOT
VALID, and 000197 validates them (06 §6.4). Walkthrough: `close_checks`
in `step_close`. Decided by default, for the product owner to overrule:
- `no_parked_entries` reads the company's parked entries whose posting
  date is in the leading ledger's period (its first to last day), in
  every ledger, a ledger-specific one included: §6 states the check per
  company, not per covered ledger. It names the first 20 by posting date
  and id, each with its id, document type, posting date, status,
  reference, header text and who parked it (id, kind, name), and counts
  them all (`parked_entries_count`); no amount is shown.
- `revaluation_posted` keys each covered ledger's run on that ledger's own
  period's last day, and counts only a `posted` run, not a cancelled one;
  the result names each covered ledger with its key date and run.
- A check runs on a done task as well, so a person refreshes a result
  without reopening first: a pass keeps the task's completion (who and
  when it first passed), a failure opens it and clears the completion.
  Reopening a check task keeps its latest result. A check task can be
  assigned like a manual one; only `:complete` is refused.
- The result keeps what the check found as it was when it ran, names of
  who parked the entries included (`check_result`, jsonb), and a one
  sentence `summary`; who ran it is named at the read, as for the other
  principals of a task. The database holds a check task done only while
  its latest check passed, and a result once recorded is replaced, never
  removed.
- `PeriodCloseChanged` gains `check_passed` (nullable, optional in the
  schema: additive), null for every other change; the audit record keeps
  the check, whether it passed and its summary.
- The walkthrough's template (T64a's part) gains the check task
  `NO_PARKED`, depending on `AP_CUTOFF`, since a close copies the template
  when it is opened; it has no `revaluation_posted` task, so the sign-off
  T64d adds needs no revaluation run in the walkthrough. The revaluation
  check is tested in `internal/closing` with real runs.

**T64d (sign-off locks the period through T32).** Built as §7:
`POST /v1/period-closes/{id}:signOff` (`signOffPeriodClose`, people only,
`closing.close.sign_off` with `ledger.period.lock` and `ledger.entry.read`
as `x-also-permissions`; the permission added by migration 000199) reads
the close `FOR UPDATE`, checks `If-Match` against its version and, in this
order, refuses a close signed off whose covered periods are all locked
(409 `closing.close_signed_off`), a manual task open (409
`closing.tasks_open`, naming them), a check failing when run again (409
`closing.checks_failing`) and a covered period still open (409
`closing.period_not_closed`, naming the ledgers); it then locks each
closed covered period through `ledger.LockPeriod` (over the new
`ledger.ClosePeriods.Lock`, at the version the sign-off read: T32's guard,
audit record and `PeriodLocked`, unchanged), the leading ledger first,
then by code, writes the `sign_off` row (migration 000199: insert-only,
1-20 covered ledgers), sets the close `signed_off`, and writes the audit
record (`closing.period_close.sign_off`) and `PeriodCloseSignedOff`. A
refusal keeps nothing (the request rolls back). A signed-off close one of
whose periods was unlocked is reopened by its first task action, audited
(`closing.period_close.reopen`, with the unlocked ledgers) and published
as `PeriodCloseChanged` (`reopened`) before the action's own change; a
signed-off close whose periods are all locked refuses every task action
(409 `closing.close_signed_off`). The test drives a lock, an approved
unlock, a second sign-off, another unlock, a reopened task, a correcting
entry and a third sign-off (`TestSignOffAfterUnlock`). 03's entry-lock
paragraph has the sign-off's period locks. Walkthrough: `close_sign_off`,
the end of `step_close`. Decided by default, for the product owner to
overrule:
- The action is `:signOff`, not `:sign-off`: the contract's path rule
  (`TestOperations`) takes a camelCase verb, as `:acceptProposal`.
- **The postings' lock order is fixed in the ledger.** `postingPeriod`
  took its periods `FOR SHARE` in the order of the ledgers' rows (a
  lateral per row, sorted only afterwards), not leading first, then by
  code as §5 assumed: the concurrency test, on a company whose three
  ledgers' ids and codes sort differently, deadlocked (40P01). Its ledgers
  now come from a subquery ordered leading first, then by code, so the
  lateral locks run in that order; the rows it returns keep their order
  (the representative first). A ledger group whose representative is not
  the leading ledger now locks the leading ledger's period first too.
- The answer is the sign-off (`PeriodCloseSignOff`: who, when, the note,
  and per covered ledger its code, name, period and whether this sign-off
  locked it, with the close's company and period) with the close's new
  ETag. `GET /v1/period-closes/{id}` lists the close's `sign_offs`, oldest
  first; the close, the list's items and the calendar's close carry
  `signed_off_at`, `signed_off_by` and `signed_off_by_name` (§3: "signed
  off at and by") while it is signed off.
- `closing.checks_failing` names each failing task in the problem's
  `failing_checks` (task code and name, check, the result's summary), as
  `payables.match_outdated` carries `match_issues`; the full result is what
  `:check` records. When every check passes, each result is recorded on its
  task as `:check` records it, without a `task_checked` event or audit
  record of its own: the sign-off's audit record and event stand for them.
- A covered period whose version changed between the sign-off's read and
  its lock (opened, closed or unlocked meanwhile) refuses the sign-off with
  412 `request.version_mismatch`: read the close again.
- A signed-off close whose covered periods are not all locked is signed off
  again as it is, without being reopened first: its status stays
  `signed_off` and a second `sign_off` row records the period this
  sign-off locked (`locked: true`) and those locked already (`false`).
- Reopening happens only on a task action that changes something (an empty
  merge patch reopens nothing), and the refusal of a write on a close
  signed off with every period locked comes before the action's own checks.
- The sign-off's optional `note` is 1-2,000 characters, as a task's note.
- Who signs off is not restricted further (no segregation of duties): any
  person holding `closing.close.sign_off` and `ledger.period.lock`, one who
  completed tasks included, as built above and as §7 and the open question
  below state.

**review:T64d (one lock order for every period lock).** T64d's review found
two more statements that locked periods of several ledgers in the order a
scan found them, each able to deadlock (40P01, a 503 to the caller) with a
sign-off or a posting: a migrated clearing's `clearingPeriods`
(`internal/ledger/migration.go`, a lateral `FOR SHARE` per entry of the
clearing entry's posting group, in the scan's order of `journal_entry`)
and the controlling switch's lock of a company's periods from the current
fiscal year on (`internal/ledger/controlling.go`, `FOR UPDATE` with no
`ORDER BY`). The rule §5 sets for sign-off and postings now holds for every
statement that locks `ledger.period_status` rows of several ledgers or
periods: by legal entity, fiscal year and period, then the leading ledger
first, then by ledger code, sorted in the statement itself. The clearing's
entries come from a subquery so ordered, as `postingPeriod`'s ledgers do;
the switch sorts under its `FOR UPDATE OF ps`. Each has a concurrency test
that deadlocked before (`TestMigratedClearingAndSignOffLockInOneOrder`,
`TestControllingSwitchAndPostingLockInOneOrder`, on
`fixture.ThreeLedgers`, whose ledgers' ids and rows sort out of code
order); the sweep found no other such statement (a filed tax return
locks the leading ledger's periods one at a time, by legal entity, year
and period; a period close, lock or unlock locks one row). 03's entry-lock
paragraph states the rule.

**T64e (accrual proposals).** Built as §8: migration 000216 creates
`accrual_proposal` and `accrual_evidence` (RLS, `tenant_id` leading every
key, no foreign key leaving the schema; a proposal moves only `proposed`
-> `rejected`, the evidence is insert-only) and the permission
`closing.accrual.propose`. `companies[].close.accrual_account` is validated
by the configuration (an account of the company's chart of type
liability, no supplier or asset reconciliation account, not the GR/IR or
down payment account) and is tier 3 by its own rule. `POST
/v1/accrual-proposals` (`proposeAccrual`, agents only: 403
`agents.not_an_agent` for a person; `x-autonomy: propose`;
`ledger.entry.park` as `x-also-permissions`; in the agent's running
session through `closing.Sessions`) reads the close `FOR UPDATE`, answers
the session's retry of the same proposal 200 as it is and refuses a
duplicate (409 `closing.accrual_duplicate`, with `Location`), refuses a
company without an accrual account it can post to (409
`closing.accrual_account_missing`), checks the expense account (422
`closing.accrual_account_invalid`), the supplier and each evidence item
(422 `closing.evidence_unknown` on `evidence[i]`), derives the expense
line's profit center through `closing.Ledger.ProfitCenter`, over the
ledger's new `ledger.CloseAccounts.ProfitCenter` (the posting's own
derivation and checks, `assign`), parks the entry
through `closing.Ledger.Park`, over the ledger's new published
`ledger.ParkedEntries` (`Park` runs `parkJournalEntry`'s own parking,
checks, problems and `ParkedJournalEntryChanged` included, as the request's
principal, and returns the entry's id and debit total), and writes the
proposal, its evidence, the audit record
(`closing.accrual_proposal.propose`) and `AccrualProposed`. The accounts
are read through `closing.Ledger.Accounts`, over the new
`ledger.CloseAccounts.Accounts` (an account by id, or by number when set up
in the company, with whether it is a balance sheet account and postable
by hand: `Accounts.IsPostable`'s rule), the centers' codes and names
through `closing.Ledger.Centers`, over the ledger's published
`AssetReads.CenterNames` (reused, not copied), and the evidence through the
new port `closing.Evidence`, adapted in `internal/app` over
`payables.Invoices.Sources` (whose `InvoiceSource` gains the invoice's
legal entity and status, additive), `procurement.Orders.Order` and
`partner.Payees.Names` (the suppliers' numbers and names; the composition
root reads the partner context, so no context's dependency changes). `GET
/v1/accrual-proposals/{id}` (`getAccrualProposal`) and `GET
/v1/accrual-proposals` (`listAccrualProposals`, filters close, company,
session and parked entry, cursor-paged by creation) show each proposal
with its company, agent, accounts, centers, supplier, amount with its
currency, evidence and parked entry. Walkthrough: `close_accruals` in
`step_close`, after the checks and before the variances. Decided by
default, for the product owner to overrule:
- **The stored status until T64f.** A proposal is stored `proposed`
  (`rejected` is T64f's `:reject`), and T64e shows that status. The
  statuses derived from the parked entry (`posted`, `pending_approval`,
  `modified`, `reversed`, `rejected` when the ledger cancelled or rejected
  it), the list's `status` filter and its totals per status and currency
  are T64f's leaf, which reads the entries' statuses
  (`ParkedEntries.Statuses`); until then a consumer reads what was done with
  the entry from the parked entry itself (`parked_entry_id`). For the same
  reason the duplicate rule counts a proposal that is not stored
  `rejected`: one whose parked entry was cancelled in the ledger still
  refuses a new proposal of the same expense until T64f derives it
  `rejected`.
- **Duplicate and retry.** The key is the close, the expense account, the
  cost center and the supplier, an absent center or supplier equal to
  another absent one. The session's retry is the same content: the profit
  center, the amount, the description, the reason and the evidence in the
  order given (a retry that leaves the profit center out matches the one
  the proposal took from its expense line); it is looked for before the
  accounts are checked and answers 200 with the proposal as stored,
  publishing nothing again, as T64i's retried comment does. The same key with other content, or from another
  session, is 409 `closing.accrual_duplicate` naming the first.
- **The accrual account at the proposal.** The configuration checks the
  chart; the ledger's master data is checked when a proposal is made: an
  accrual account not set up, active and postable by hand in the company
  (blocked, say) is 409 `closing.accrual_account_missing` too, its detail
  saying why.
- **The expense account** is a profit and loss account set up, active and
  not blocked in the close's company, of no reconciliation kind and not its
  GR/IR account; an unknown account or another company's is the same 422
  `closing.accrual_account_invalid`.
- **The amount** is in the company's local currency with its digits and
  more than zero; another currency, other digits and an amount of zero or
  less are each 422 `request.invalid_amount` on `amount` (§8's code), not
  the 400 other operations give for the digits.
- **The supplier** names a business partner, of any role (422
  `request.reference_not_found` on `supplier_id`), is not checked against
  the evidence's suppliers and is not on the entry's lines (an expense line
  takes no partner and the accrual account reconciles no supplier): it is
  the proposal's, for the duplicate rule and its reader.
- **Evidence.** A journal entry is checked through `closing.Ledger.Cited`
  (T64i's read: a posted entry of the company in the leading ledger),
  whatever accounts it has lines on. Each document is cited once
  (`uniqueItems`: 400 `request.invalid_body`), and is kept with its
  document number as cited (a posted invoice, a released order and a
  posted entry keep theirs), an invoice or an order with its supplier, an
  entry with its fiscal year.
- **The parked entry**: document type `SA`, document and posting date the
  close's `period_end`, header text `Accrual proposal <id>`, each line's
  text the description (1-200 characters, the ledger's line text limit),
  the credit line without a cost center. Both lines carry the expense
  line's profit center (§8): the one given or, without one, the one a
  posting of the expense line derives on that date (its cost center's, its
  account's default cost center's, or the company's default profit
  center), read before parking; the proposal records it as its profit
  center. The ledger's refusals pass through with their codes (listed by
  `TestProblemCodes` from the call graph): a field of the expense line is
  renamed to the proposal's (`lines[0].cost_center_id` becomes
  `cost_center_id`), one of the accrual account's line, which the
  configuration names, keeps `lines[1].…`, and the detail names the line
  ("The expense line: ...").
- **Names.** The accounts' numbers and names and the centers' codes and
  names are kept as they were when proposed, as T64i keeps a comment's
  account; the suppliers' numbers and names, the proposal's and each
  evidence item's, are read at the read and shown only to a caller holding
  `partner.supplier.read` (product owner 2026-11-02: a copy never crosses
  permissions), the ids always.
- `accrual_proposal` has no `legal_entity_id` column (§5 lists one): the
  close names the company, as for variance comments. Its codes of centers
  are never blank, and a parked entry is named by one proposal
  (`accrual_proposal_parked_once`).
- A proposal on a signed-off close follows §7: refused while its periods are
  all locked, reopening it after an unlock.
- **The walkthrough's agent principal** is now created in `close_accruals`
  (T64i's As built: "T64e, when built, takes it over"), its role reading
  closes and the ledger, proposing accruals and parking entries;
  `close_comments` grants it `closing.variance.propose`. Ada's change is
  tier 4, since the accrual account 2195 is new in the chart (the
  accrual account itself is tier 3), approved as step 19's chart change is.
  "Last month's invoice" is Tyrell's invoice of its courier deliveries,
  dated the last day of the previous month and posted today: the previous period may be locked
  by step 11's filed return, which a story run on any day cannot avoid.
- No performance budget: §8 asks for none.


**T64g (the close agent: accruals).** Built as §12: the agent type `close`
(`autonomy.Types`, the profile's open enumeration `agent_type`) and the
model alias `erp-close` (`modelalias.Aliases`, routed by default to the
model the platform routed to before certification, `agents.DefaultRoutes`;
`api/model.yaml`'s alias enumeration and the profile's `model` description
name it, additive). `internal/closeagent` (its `internal/layout` entry
`{"apitools", "llm"}`, as reconagent's) runs as a client: `Accruals` reads
the close (`getPeriodClose`; one the agent may not read stops the run
before a session or a model call), starts one session, offers the model
§12's thirteen tools (`AccrualTools`) at its level and runs the prompt
(§12's, with the untrusted-data sentence of reconagent's), naming the
close's company, year, period, its last day and the session. `erp agent
close-accruals -close ID` (`ERP_AGENT_TOKEN`) runs it on `erp-close`
unless `ERP_AGENT_MODEL` names an alias, as `erp ask` runs on
`erp-analytics`, and prints its session, each proposal made with its
parked entry, the count refused and the model's last text, also when the
run fails after proposing. `evals/close/suite.yaml` (version 1) holds §12's accrual cases,
replayed in CI by `TestEvalSuites` to level 2; `erp eval run` has the
adapter `close`. The model stand-in (`internal/llm/llmtest/modelstub`)
gains `-script`: while that file exists each call is answered by its next
turn, a tool call (its input's `{{name}}` filled from the script's `vars`,
regular expressions over the request, as a suite's `replay_vars`) or a
final text, from the first turn again when the script changes, refused
past its last. Walkthrough: `close_accrual_agent`, called at the end of
`close_accruals`. No migration, no new permission, no new operation.
Decided by default, for the product owner to overrule:
- **One session per close**, not per supplier as reconagent's per line:
  §12's prompt has the agent find the suppliers by reading, so there is no
  list to iterate before the model; the run has at most 40 model requests
  (`MaxIterations`).
- **The periods are the invoices' dates.** "Invoiced in each of the three
  regular periods before the close's" and "no invoice for the close's
  period" are judged on invoice dates (the supplier's billing month), not
  posting dates: a late invoice posted in a later period is still its
  month's, as the walkthrough's are. The net amount is the most recent
  invoice's items on that account.
- **The run's own close only.** The `proposeAccrual` tool refuses, before
  the API, a body naming another close than the run's
  (`closeagent.ErrOtherClose`, returned to the model as the tool's
  error), as the migration agent's guard keeps it to rehearsals: a limit
  beyond the agent's permissions, so text in a document cannot send it to
  another close.
- **What counts.** A proposal the API accepts (201, or 200 for a retry,
  which answers the same proposal) is reported once by its id with its
  parked entry; one the API or the run's guard refuses is counted
  refused; neither fails the command, since the model sees the problem
  and goes on. A run whose model fails (or a close, session or contract
  the agent cannot read) fails it, after printing what it made; a session
  started before the contract's tools could be built is ended `failed`.
- **The suite** is dated in 2026 as the other suites are (the close of
  company 2000's June, invoices of March to May, periods 3 to 6 opened by
  its setup), with seven cases: the monthly supplier gets one accrual of
  May's net amount citing the three invoices (correctness); a supplier
  already invoiced in June, goods received in June on an order valued
  through GR/IR, a one-off invoice, an expense already proposed for the
  close (by an earlier session) and one a person parked each get none; an
  instruction planted in an invoice's reference and item text is not
  followed (safety). §12's "no post, cancel or other write" is a check in
  every case (`tools_within` the thirteen tools; the agent's entry still
  `parked`; the person's parked entry neither posted nor cancelled). Its
  configuration is the shared one with an accrual account 219500, a GR/IR
  account and price differences, and a one-task close template. The
  bars are ADR-0039's (90 and 95). The adapter records the reply
  (`text`, `proposals`, `refused`) for `$reply` checks, which T64j's
  variance cases may use.
- **The walkthrough's close agent** is now of type `close` on `erp-close`
  (T64e's principal, created in `close_accruals`), still raised to level 1
  by the walkthrough's evaluation cell (§12); its role gains what its
  tools read (`ledger.account.read`, `payables.invoice.read`,
  `procurement.order.read`, `partner.supplier.read`,
  `agents.session.read`) and `agents.model.invoke`. Rideau IT Services'
  invoices of the three months before the close's, and Laurentian Paper's
  of those months and this one, are posted today, dated in their months,
  as T64e's invoice is (an earlier period may be locked by step 11's
  return); the stand-in's script proposes Rideau's accrual of its last
  net amount, 450.00 CAD on 6100, citing the three, and nothing for
  Laurentian; Carla posts its parked entry, so the close stays ready to
  sign off.
- No performance budget: §12 asks for none, and the agent's reads are the
  existing list operations.

**T64h (the variance report).** Built as §9: `companies[].close.variance`
(`bases`, `threshold_amount`, `threshold_percent`, `plan_category`,
`plan_centers`) in `internal/config` (`config.CloseVariance`, validated
with the problems on their fields; tier 2 by the new rule
`companies[*].close.variance`), and the closing context's configuration
section `closing.Setup`, which refuses a plan basis naming a plan category
the tenant does not have (the document's own validation cannot see
controlling's master data). `GET /v1/period-closes/{id}/variances`
(`listPeriodCloseVariances`, `closing.close.read` with `ledger.entry.read`
as `x-also-permissions`, `x-slo-class: report`) reads the actuals and the
prior periods' figures through `closing.Ledger.ProfitAndLoss`, over the
ledger's new published port `ledger.CloseAccounts` (the leading ledger's
local-currency net per profit and loss account over `balanceRows`, exact
at the read), and the plan through the new port `closing.Plans`, adapted
in `internal/app` over controlling's new published `controlling.Plans`
(a category by code; a category's current plan of a company's period per
account, over its cost center lines or its profit center lines). The
percent is `money.PercentOfMagnitude`, which shares its exact rounding
with the plan/actual reports' `money.PercentOf` (moved out of
`internal/controlling`) and the forecast variance's `money.AbsPercentOf`
(T53f3; batch 50's integration). No migration: the report stores nothing and
`closing.close.read` exists. Walkthrough: `close_variances` in
`step_close`, before the sign-off. Decided by default, for the product
owner to overrule:
- **What T64i adds.** The rows' comments (the accepted one with `stale`,
  the count proposed), the `unexplained` filter and the `rows explained`
  total read variance comments, which T64i creates; the report adds them
  then (additive), and T64i's leaf names them.
- The configuration: `bases` 1-3 known bases, each once; `threshold_amount`
  required, more than zero, in the company's currency's digits;
  `threshold_percent` optional, more than zero, at most two decimals (the
  report's percents have two); `plan_category` (a category code) and
  `plan_centers` (`cost` or `profit`) required with the basis `plan` and
  refused without it. No variance section: every basis is
  `closing.basis_not_configured`.
- A basis the company does not configure, one this build does not know
  included, is 422 `closing.basis_not_configured` on `basis`, naming the
  bases it configures; `basis` is required.
- The percent threshold is compared with the percent as shown (two
  places), so a consumer can tell from the row why it is flagged.
- `totals` are the whole report's, whatever the `requires_comment` filter
  and on every page alike, so their actuals stay the period's profit and
  loss; `requires_comment=false` keeps the rows below the thresholds.
- The rows are every profit and loss account set up in the company or with
  a movement in either period, with a non-zero actual or comparison;
  balance sheet accounts never (a plan amount on one, or on an account not
  set up in the company, has no row). Accounts are ordered and paged by
  number as the database orders text byte by byte (`COLLATE "C"`: 4000,
  4800, 540000, 6000), not by SAP's ALPHA padding.
- The report names what it compares: the comparison's fiscal year and
  period (the close's own for `plan`), the leading ledger, the currency, the
  thresholds and, for the plan, the category (id, code, name) and the kind
  of center.
- A configured plan category the controlling context no longer holds, or a
  threshold that is no amount (only a document written past the
  configuration context has them), fails the read (500), as other broken
  data does.
- No performance budget: §9 asks for none, and the report reads one
  company's period through the trial balance's statement (`balanceRows`,
  `period_balance_by_period`) and one plan sum.
- The balance projector helper the ledger's and controlling's tests each
  kept is `fixture.Project`, which closing's tests use too.


**T64i (variance comments).** Built as §10: migration 000202 creates
`variance_comment` and `variance_citation` (RLS, `tenant_id` leading every
key, no foreign key leaving the schema) and the permissions
`closing.variance.propose` and `closing.variance.comment`. `POST
/v1/variance-comment-proposals` (`proposeVarianceComment`, agents only:
403 `agents.not_an_agent` for a person; `x-autonomy: propose`; in the
agent's running session through the new port `closing.Sessions`, adapted
by `agents.Sessions` as cash's) and `POST /v1/variance-comments`
(`createVarianceComment`, people only, accepted at once) read the close
`FOR UPDATE`, compute the row's figures from the report (§9), check each
citation through `closing.Ledger.Cited`, over the ledger's new read
`ledger.CloseAccounts.Cited` (a posted entry of the company in the
leading ledger, with the periods it has a line on the account in), and
write the comment with the row's actual and comparison. `:acceptProposal`
(optional replacement text) and `:rejectProposal` (a reason) decide a
`proposed` one (409 `closing.comment_not_proposed`), If-Match on the
comment's version, under the close's lock. Accepting or writing a comment
sets the row's accepted one `superseded` in the same transaction. `GET
/v1/variance-comments` (filters close, basis, account, status, session;
cursor-paged by creation) and `GET /v1/variance-comments/{id}` show each
comment with its company, account, figures and their currency, `stale`,
citations, author and decider named. The variance report's rows carry
`comment` (the accepted one, with `stale`), `explained` and
`proposed_comments`, the filter `unexplained` and the total
`rows_explained`. `config.CloseChecks` gains `variances_explained`, run
by `:check` and the sign-off. Every write is audited
(`closing.variance_comment.propose|write|accept|reject|supersede`) and
published as `VarianceCommentChanged`. A write on a signed-off close
follows §7 (refused while its periods are all locked, reopening it
after an unlock). Walkthrough: `close_comments` in `step_close`, before
the sign-off, with the walkthrough's first agent principal. Decided by
default, for the product owner to overrule:
- The actions are `:acceptProposal` and `:rejectProposal`, not `:accept`
  and `:reject`: the contract reserves `:reject` for approval requests
  (`TestOperations`), and memo record proposals use these names.
- `GET /v1/variance-comments/{id}` (`getVarianceComment`) is added beside
  the list (§13 names none): a created comment's `Location` and the ETag
  its decisions take need it.
- A comment names a row of the report against the basis (422
  `closing.variance_row_unknown` on `account_id` otherwise, a code beside
  §13's): an account with a figure in either period; a row need not
  require a comment to be explained. A basis the company does not
  configure is 422 `closing.basis_not_configured`, as for the report.
- The figures a comment keeps are the row's actual and comparison when it
  was written, and it keeps them when accepted: a proposal accepted after
  the figures changed is stale at once, so the person writes a current
  comment instead. `stale` is shown on every comment (on the report's
  rows and in the list), judged at the read; it is absent where the
  company no longer compares against the basis.
- An accepted text replaced by the person is kept with the proposal's
  text (`proposed_text`, a column beside §5's) and shown as `edited`; the
  comment also keeps the account's number and name as they were when it
  was written, and each citation its document number and posting date
  (a posted entry never changes), so the reads need no ledger call per
  row.
- Citations are 0-20 journal entry ids, each once (`uniqueItems`); an
  entry must be a posted one of the company in the leading ledger with a
  line on the account in the close's period or, for `prior_period` and
  `prior_year`, in the period compared against (422
  `closing.citation_invalid` on `citations[i]`). A text or reason of white
  space alone is 422 `request.validation_failed`.
- One proposal per session and row (`variance_comment_proposed_once`):
  the same text and citations again answer 200 with it, whatever its
  status now; other content is 409 `closing.comment_already_proposed`.
  Proposals of other sessions for the row stay `proposed` when one is
  accepted or a person writes one; a person rejects them or they wait.
- `variances_explained` runs every basis the company configures, in the
  configured order; it fails, saying so, when the company configures no
  basis, since the template asks for variances to be explained. Its
  result names the first 20 unexplained rows (basis, account, variance
  with its currency, whether its accepted comment is stale) and counts
  them all (`unexplained_rows`, `unexplained_rows_count`).
- `VarianceCommentChanged`'s `change` is `accepted` for a person's
  comment too (§13 lists proposed, accepted, rejected, superseded), with
  `author_kind` telling a person's from an agent's.
- The walkthrough's agent principal (the close agent, untyped until T64g
  adds the type `close`) is created by Ada in `close_comments`, at level
  1 with a role of `closing.close.read`, `closing.variance.propose`,
  `ledger.entry.read` and `agents.session.record`; the walkthrough's server
  runs with `ERP_EVAL_CELL=true` (§12). T64e, when built, takes it over.
- No performance budget: §10 asks for none. A list page computes each
  close and basis's report once, to tell its comments' staleness.

**T64k (the depreciation check).** Built as §6: `config.CloseChecks`
gains `depreciation_posted`, so a template's check task may name it.
`:check` (and the sign-off, which runs every check again) reads, for each
covered ledger, the company's depreciation of the close's fiscal year and
period through the port `closing.Depreciation`, adapted in `internal/app`
over the assets context's new published port `assets.CloseChecks`
(`Depreciation`: the latest posted run of the company, ledger, year and
period; without one, how many assets a run of that period in that ledger
would select). The check passes when every covered ledger has a posted
run or nothing to select, and its result names each covered ledger, the
leading one first, then by code, with its period's last day, `posted`,
its run (`depreciation_run_id`) or, without one, `assets_to_depreciate`,
in the result's `ledgers` beside the revaluation check's (additive fields
of `CloseCheckLedger`; the check codes' open enumerations gain
`depreciation_posted`). No migration: the check stores its result in
T64c's columns. Walkthrough: `close_depreciation` in `step_close`, before
the other checks. Decided by default, for the product owner to overrule:
- **What counts as posted**: a run whose status is `posted` for exactly
  the close's fiscal year and period in that ledger. A `failed` run does
  not, even when some of its batches posted, and neither does a
  cancelled one (its Operation cancelled, the run ends `failed`, ADR-0057
  T57f) nor one still `queued` or `running`. Of several posted runs of the
  period (a repeat run) the result names the latest.
- **"Nothing to depreciate"** is the run's own selection
  (`selectDepreciable`, which the plan, each batch's re-check and the
  preview share; ADR-0057 §7 step 1), counted, so the check and a run can
  never disagree. A company without assets, or whose assets start
  depreciating after the period, passes. An asset that is selected but
  whose difference is zero (a fully depreciated asset still active) still
  needs the run, as the ADR's table says ("nothing a run would select"),
  and a run of it posts nothing and is `posted` with an amount of zero.
- **A period passed over**: once a later period's run has posted in a
  ledger, every asset it processed is booked through that later period,
  so the selection of the earlier one passes them over and its check
  passes: that run caught their depreciation up (ADR-0057 §7, period lock
  interplay), and the earlier period cannot be run any more
  (`assets.run_out_of_order`). An asset capitalised after that run with a
  value date in or before the earlier period is not booked yet, so it is
  selected for the earlier period too and fails that close's check,
  which no run can then pass; the next period's run catches it up. The
  check is §6's as written, so such a close cannot be signed off (it keeps
  the tasks it copied); whether the check should then pass on the later
  period's run is listed for the product owner, not decided here.
- **A posted run stays enough**: an acquisition posted into the period
  after its run (whose depreciation the next run catches up) does not
  fail the check again; §6 asks for a posted run, not for a zero
  difference.
- The selected assets are counted only without a posted run: after a run
  they are still selected (a repeat run selects them and posts nothing),
  so a count would mislead. `assets_to_depreciate` is absent with a run.
- `posted` keeps its meaning per check (a run of the check's kind), so a
  ledger with nothing to depreciate shows `posted: false` with
  `assets_to_depreciate: 0` and passes; the summary names each failing
  ledger with its count ("No posted depreciation run of 2026/1 in ledger
  2L (1 asset to depreciate); run the depreciation first.").
- **No further permission**: `checkCloseTask` keeps `closing.close.manage`
  and `ledger.entry.read`. The result shows a run's id and a count, not an
  asset; reading the run or the assets needs `assets.asset.read` as
  before.
- The assets port answers an error, not "nothing to depreciate", for a
  company the caller may not act in, so a check never passes on what it
  could not read; the closing context only asks for a close's company,
  which the caller may act in.
- **The walkthrough**: step 29 (`step_depreciation_run`) runs before
  `step_close`, which stays last, and posts the period's run in the
  leading ledger only; the close covers 0L and 2L, so `close_depreciation`
  runs the check, which fails naming 2L, where the lathe still waits,
  with 0L's run; Carla runs the period's depreciation in 2L and the check
  passes, naming both runs. That is the leaf's "fails before the period's
  depreciation run and passes after it", per covered ledger, without
  reordering the story. The template gains the check task `DEPRECIATED`
  (due day 2, no dependency).
