# ADR-0059: Year-end close

**Status:** accepted (T64m design step, 2026-11-05; decided by default under
ADR-0053 d; `go-reviewer` approved in round N with no blocking finding). The
decisions taken by default and the questions for the product owner are
listed at the end; the slices T64m1-T64m6 are in the tracker under
Financial close.

## Context
ADR-0058 §1 left the year-end close out of T64 and asked the product owner
whether it should be built ("special periods 13-16, closing the profit and
loss into retained earnings and carrying balances forward ... the ledger
has no carry-forward today"). The product owner decided on 2026-11-04 that
the year-end close is its own design (tracker leaf T64m, "year-end close
designed"). This ADR is that design: what an ERP replacing SAP FI needs at
the end of a fiscal year, within the approved scope of ledger, close and
assets. Today the ERP has:

- **The fiscal calendar** (T3, `internal/ledger/setup.go`): each company's
  calendar has 12 normal and 4 special periods (`normalPeriods`,
  `specialPeriods`; `ledger.fiscal_calendar.special_periods`, 0-4 allowed by
  the table, 4 created by `ledger.Setup` for every calendar). Special periods
  13-16 share period 12's start and end dates (`fiscalYear`,
  `ledger.fiscal_period.is_special`). A fiscal year exists for a company
  once the configuration has a number range for it; its periods are created
  `closed` in every ledger the company keeps (ADR-0045).
- **Special periods take no posting.** A posting's period is found from its
  date among the regular periods only (`periodOn`, `internal/ledger/journal.go`:
  `NOT is_special`); clearings (000049), revaluation key dates
  (`revaluation.go`), period closes (ADR-0058 §6, `closing.special_period`)
  and tax returns (`returnperiods.go`) leave them out, each saying they "are
  the year-end close's". `:open`, `:close`, `:lock` and the unlock request
  (T32) work on them as on any period (`changePeriod` has no special-period
  rule).
- **Balances** (ADR-0006): `ledger.period_balance` per ledger, company,
  account, fiscal year, period (1-16) and transaction currency, with debit
  and credit in transaction, local and group currency, kept by the
  projector; the trial balance (`listBalances`) reads one company, ledger
  and year (projected balances plus the lines not yet projected,
  `balanceRows` in `internal/ledger/reads.go`), so it shows that year's
  movements only. Balances by account assignment
  (`ledger.assignment_balance`, ADR-0050 §7) are profit and loss only: a
  balance sheet by profit center or segment needs document splitting,
  decided not now (ADR-0050 P3).
- **No carry-forward.** "The ledger posts no year-end carry-forward
  entries, so every year's lines are summed" (`Accounts.Balances`,
  `internal/ledger/ports.go`); revaluation reads every year up to the key
  date's (`revaluation.go`, `l.fiscal_year <= $4`); open items are lines of
  any year, open until a clearing; the asset history sheet sums the years
  before into its opening values (`internal/assets/sheet.go`, ADR-0057 §9).
- **Accounts**: `account_type` asset, liability, equity, revenue or
  expense, `is_balance_sheet` generated from it (000005); a type never
  changes once activated (`ledger.Setup`). The configuration has no retained
  earnings account (`config.AccountDetermination`'s keys are AP
  reconciliation, input tax, bank, exchange gain and loss, GR/IR, price
  difference, cash discount and down payment).
- **Every entry balances** in transaction, local and group currency
  (`ledger.check_entry_balanced`, 000005), and has one transaction currency.
- **Parallel ledgers** (ADR-0045): periods per ledger, opened, closed and
  locked apart; open items are the leading ledger's; reports read one
  ledger.
- **Migration** (docs/migration.md): a company's opening balances from SAP
  are journal entries of the first loaded year ("Opening balances from SAP",
  `internal/integration/load.go`).
- **The period close** (ADR-0058, T64a-T64k): a close per company and
  regular period with a checklist from the configuration's template, checks
  run on demand and at sign-off, sign-off locking each covered period
  through T32's `LockPeriod`; `closing` is a leaf reaching the ledger,
  assets and others through ports; the depreciation check reads
  `assets.CloseChecks.Depreciation` per company, ledger and period (T64k).
- **Archiving** (ADR-0006): "old years move to cheaper storage"; nothing
  moves yet.

### What SAP does (the parity target)
**From memory, for a specialist to check against the pilot's SAP release;
no source was read for this section.** SAP carries balances forward with
the balance carry-forward program (FAGLGVTR in the new G/L; in S/4HANA it
writes carry-forward records into the universal journal for period 0 of the
new year): balance sheet accounts keep their balances per currency and
assignment, and profit and loss accounts are carried to a retained earnings
account assigned per "P&L statement account type" in the chart of accounts
(OB53); the program is run per company code and ledger, may be repeated, and
once it has run, later postings in the old year are carried forward
automatically. Customer and vendor balances are carried forward for the
subledger totals (F.07), while their open items stay open. Special periods
13-16 are used by entering the posting period on postings dated in the last
regular period, opened per account type with the posting period variant
(OB52). Asset accounting has its own fiscal year change (AJRW) and year-end
closing (AJAB). The Financial Closing cockpit and Advanced Financial Closing
keep year-end task lists like the monthly ones.

## Decision

### 1. Scope
In: postings into special periods 13-16 (manual and parked entries and
their reversals); a retained earnings account per company in the
configuration; the balance carry-forward of a company, ledger and fiscal
year into the next year, stored as balances (never as journal entries),
re-run after late postings, with its reads; the year close in `closing`: a
company's close of a fiscal year with its own checklist from the
configuration's template, checks (parked entries of the year, revaluation at
year end, depreciation of every period, carry-forward current) and a
sign-off that needs every regular period locked and locks the special
periods, so that the whole year is locked.

Out (none built; gaps or questions listed at the end): carrying late
postings forward automatically (re-run instead, §6); several retained
earnings accounts per company or per ledger (question 1); supplier and
customer balances carried forward per partner (open items are the
subledger, §9); a balance sheet by profit center or segment (ADR-0050 P3);
opening balances shown by the trial balance (`listBalances` unchanged,
§5); financial statements and statutory reports (T65); consolidation and a
group year close (T68); archiving old years (ADR-0006); an automatic check
of open items against their reconciliation accounts (§9); year-close tasks
in the cross-close worklist `listCloseTasks`; agent proposals for year-end
adjustments (§12); screens in the web workspace (headless first,
ADR-0053).

### 2. Where it lives
- **The ledger** owns special-period postings, the retained earnings
  account's use and the carry-forward: balances are the ledger's (ADR-0006),
  and the ledger may not read `closing`.
- **`closing`** (the leaf of ADR-0058) owns the year close. Its ports gain:
  `closing.Ledger`, the periods of a company and year per ledger it keeps
  (beside `ledger.ClosePeriods.Of`, which reads one period, T64b), and the
  current carry-forward of a company, ledger and year with whether it is
  stale (§6); `closing.Depreciation`, unchanged, asked once per regular
  period of the year.
- No dependency direction changes, so no other ADR is needed; this ADR
  records the new port methods (CLAUDE.md).

### 3. Special periods take postings
- **Which postings.** `postJournalEntry`, `parkJournalEntry` (and
  `postParkedJournalEntry`, which posts what was parked) and
  `reverseJournalEntry` take an optional `fiscal_period`, 13 up to 12 plus
  the calendar's special periods. Without it a posting goes to the regular
  period of its date, as today. Every other posting (the contexts' ports,
  clearings, settlements, revaluation, depreciation, the migration) stays
  in regular periods: year-end adjustments are manual G/L postings, as SAP
  uses special periods (from memory, above).
- **Rules.** The posting date must be in the year's last regular period,
  whose dates the special periods share (422
  `ledger.special_period_date`, on `posting_date`); a period the calendar
  does not have is 422 `ledger.no_fiscal_period`; the special period must be
  open in every ledger posted to, read `FOR SHARE` in the postings' order
  (`postingPeriod`, unchanged but for the period number), else 409
  `ledger.period_not_open` as today. A parked entry keeps its special
  period and is held to these rules again when it is posted. A reversal
  names its own date and optional special period by the same rules; without
  one it goes to the regular period of its date, as today.
- **Tax.** A line with a tax code in a special period is refused (422
  `ledger.special_period_tax_code`) until the product owner answers
  question 2: tax returns select tax items by their tax period's dates, and
  whether a year-end adjustment may carry tax is a tax rule.
- **Readers.** Nothing else changes: the trial balance already reads
  periods 1-16, revaluation and open items read by date (a special-period
  entry dated the year's last day is in the year-end valuation), and an
  entry's `fiscal_period` is already 1-16 in the contract and the database
  (`journal_line.fiscal_period BETWEEN 1 AND 16`).

### 4. The retained earnings account (configuration)
```yaml
companies:
  - code: "1000"
    year_end:
      retained_earnings_account: "330000"   # equity, postable in the company,
                                            # no reconciliation account, not open-item managed
```
- One account per company, for every ledger it keeps (the chart is the
  company's, ADR-0045 keeps one chart for every ledger). Validation (on the
  field): the account exists in the company's chart, is postable there, is
  of type `equity`, is no reconciliation account of any kind and is not
  open-item managed.
- **Tier 3** (posting-related): it decides where a year's result is
  carried. A change applies to the next carry-forward run only; each run
  records the account it used.
- It is not required at activation: a company without one is refused its
  first carry-forward (409 `ledger.retained_earnings_missing`), as a payment
  needing an exchange difference account is refused without it.

### 5. The balance carry-forward
**What it is.** The carry-forward of a company, ledger and fiscal year Y
is the opening balance of year Y+1: per account and transaction currency,
an amount in transaction, local and group currency (debit positive, as
`Accounts.Balances` returns them):
- each **balance sheet account** carries its balance at the end of Y;
- each **profit and loss account** carries nothing: its balance at the end
  of Y is carried, per transaction currency, to the retained earnings
  account as a `result` row beside that account's own `balance` row.

Its rows sum to zero per transaction currency in transaction, local and
group currency, because every entry does (000005); a test asserts it on
every run. Carrying the result per transaction currency is what keeps that
invariant; how SAP presents a foreign-currency result in retained earnings
is for a specialist (below).

**Never journal lines.** A carry-forward writes no journal entry. The
readers that sum every year's lines (`Accounts.Balances`, revaluation, open
items, the asset sheet) stay correct unchanged, the journal keeps only
postings (ADR-0006), and a re-run replaces balances without reversals. The
cost is that a reader wanting an opening balance reads the carry-forward
(§7); the trial balance does not add it (`listBalances` keeps its rows and
its `fiscal_period` 1-16: an opening row would be a period 0, outside the
contract's range).

**How it is computed** (a run: `POST /v1/balance-carry-forwards`, §7):
- **Chained** when the company and ledger have a current carry-forward from
  Y-1 (§6): that run's rows plus year Y's balances (periods 1-16), the
  trial balance's read (projected balances plus unprojected lines,
  `balanceRows`). A stale one is refused (409 `ledger.carry_forward_stale`,
  naming the year to re-run first), so a chain never builds on figures that
  changed.
- **From every year** up to Y when there is no carry-forward from Y-1 (the
  company's first, or the first in a ledger assigned later, ADR-0045): the
  sum of every year's balances. The two ways give the same rows, which a
  test proves on three years. The chain keeps a run to one year's
  balances, and is what lets old years move to cheaper storage later
  (ADR-0006).

**Preconditions.** In one transaction:
1. the company keeps the ledger (422 `ledger.ledger_not_kept`) and has a
   retained earnings account (409 `ledger.retained_earnings_missing`);
2. year Y+1 is set up in the ledger (its periods exist; 409
   `ledger.next_year_not_set_up`): the carry-forward is the opening of a
   year the company keeps;
3. every period of year Y and of every earlier year is `closed` or
   `locked` in the ledger (409 `ledger.carry_forward_periods_open`, naming
   them): nothing is being posted into what is carried, whichever way it
   is computed (earlier years are normally locked by then). A person closes the special periods for the
   run and opens them again for further adjustments, which makes the run
   stale (§6).

**Locking** (ADR-0008, 03's entry locks): a transaction-scoped advisory
lock on `'ledger.carry_forward:' || tenant || ':' || company || ':' ||
ledger || ':' || Y` first, so two runs of the same year serialise, then the
periods of year Y and every earlier year, `FOR SHARE`, by year and period (a run reads one
ledger, so the postings' ledger order does not arise). A posting into those
periods needs one open, which needs `OpenPeriod`'s `FOR UPDATE`; it waits
for the run, and the run sees every committed posting. The run takes no
entry bucket and no number range, so it adds no cycle to 03's lock order.

**Runs are kept.** Each run is a row (`ledger.carry_forward_run`) with its
balances (`ledger.carry_forward_balance`, insert-only) and the opened count
of each period it read (§6). One run per company, ledger and year is
`current` (a partial unique index); a new run sets the previous one
`superseded` (`WHERE status = 'current'`, naming its successor) in the same
transaction, so the history of what was carried, when and by whom stays.

### 6. Late postings and re-runs
- **Opened count.** `ledger.period_status` gains `opened_count` (0 for
  every period; `OpenPeriod` adds one; nothing else changes it, which a
  trigger holds and a schema test proves). A posting needs an open period,
  and a period is open only after an `OpenPeriod`, so a period whose count
  has not moved and that is not open now has taken no posting since it was
  read.
- **Stale.** A run records the count of every period of year Y and of
  every earlier year in its ledger (`ledger.carry_forward_period`, at most
  16 per year kept), whether it was chained or not. It is stale when one of
  them is open now or its count moved, judged at each read of the run: a
  late posting into year Y (an unlock approved through T32, the period
  opened, the entry posted) makes the carry-forward from Y stale, and every
  later year's too, since each recorded Y's periods; no run's staleness
  depends on another run's. Locking or unlocking a period does not move the
  count: only an opening allows a posting.
- **Re-run.** A person re-runs after closing the periods again; the new run
  supersedes the old one. Nothing is carried automatically: a posting into
  an earlier year does not write balances of a later year, so posting stays
  as it is (ADR-0006: no hot balance row), and the year close shows the
  carry-forward stale until it is re-run (§8). SAP's automatic carry-forward
  of later postings is the alternative rejected below.

### 7. Reading the carry-forward (API)
- `POST /v1/balance-carry-forwards` (`Idempotency-Key`; `legal_entity_id`,
  `fiscal_year` Y, `ledger_ids` defaulting to every ledger the company
  keeps): one run per ledger in one transaction (each ledger's preconditions
  and lock in the postings' ledger order: leading first, then by code);
  201 with the runs. People only (`x-agent: false`); `ledger.carry_forward.run`
  and, as `x-also-permissions`, `ledger.entry.read` (it returns balances).
- `GET /v1/balance-carry-forwards?legal_entity_id&fiscal_year&ledger_id&status`
  (cursor-paged) and `GET /v1/balance-carry-forwards/{id}`: the run with its
  company (code, name), ledger (code, name), years, basis (`chained` or
  `all_years`, with the run it chained from), the retained earnings account
  (number, name), status, who ran it and when, `stale` with the periods
  opened since (year, period, ledger), the number of rows, and its totals
  per transaction currency: the result carried to retained earnings and the
  debit and credit totals in transaction, local and group currency (which
  are equal, §5).
- `GET /v1/balance-carry-forwards/{id}/balances` (cursor-paged by account
  number and currency; filter `account_id`): each row's account (number,
  name, type), kind (`balance` or `result`), currency and amounts in
  transaction, local and group currency with their currencies, and the
  totals of the whole run with every page (REVIEW.md: a report without its
  totals).
- Reads take `ledger.entry.read`, at every agent autonomy level.

### 8. The year close (`closing`)
**The template.** The configuration's `close` gains `year_tasks`, the same
shape as `tasks` (ADR-0058 §3: code, name, kind, check, `due_day`,
`depends_on`, `companies`; at most 200) with its own codes; `due_day` counts
business days from the fiscal year's last day. Its checks are those a year
close knows (below); `accruals_decided`, `accruals_reversed` and
`variances_explained` are period checks and refused there. Tiers as
`tasks` (operational; names cosmetic).

**Its own tables.** A year close is stored apart from period closes
(`closing.year_close`, `year_close_ledger`, `year_close_task`,
`year_sign_off`), with the same columns and database rules as their
ADR-0058 §5 counterparts but for the period: a close names its company and
fiscal year (unique per company and year), the year's last day, the
configuration version and the ledgers it covers. The Go code of tasks,
checks and sign-off is shared between both kinds of close (CLAUDE.md: no
copied helper); only the tables and the covered periods differ. Storing
year closes in `period_close` with an empty period was rejected: a replica
of the release before reads every `period_close` row and would fail on one
without a period during a rolling deploy.

**API.** `POST /v1/year-closes` (company, fiscal year, `ledger_ids`
defaulting to every kept ledger; 409 `closing.year_close_exists` naming it,
409 `closing.no_template` without year tasks for the company, 422
`closing.period_unknown` when a ledger has no periods of the year), `GET`
and list (`legal_entity_id`, `fiscal_year`, `status`), and the task
operations under `/v1/year-closes/{id}/tasks/{code}` (`PATCH`, `:complete`,
`:reopen`, `:check`) and `:sign-off`, each as its ADR-0058 counterpart
(problems, `If-Match` on the close's ETag, permissions and
`x-also-permissions`, people only for writes). A year close's
representation names its company, fiscal year, and per covered ledger its
special periods and its regular periods' statuses (number, status), its
tasks with their latest check results, counts and sign-offs.

**Checks of a year close:**

| Check | Passes when | Port, slice |
|---|---|---|
| `no_parked_entries` | no parked entry of the company dated in the fiscal year is `parked` or `pending_approval` (ADR-0058's check, over the year's dates) | `closing.Ledger`, T64m4 |
| `revaluation_posted` | each covered ledger has a posted revaluation run keyed on the year's last day (ADR-0058's check, on that date) | `closing.Ledger`, T64m4 |
| `depreciation_year_posted` | for each covered ledger and each regular period of the year, T64k's check passes (a posted run, or nothing a run would select); the result names each ledger and period that fails | `closing.Depreciation`, T64m5 |
| `carry_forward_current` | each covered ledger has a current carry-forward from the year that is not stale; the result names each ledger without one, or stale with the periods opened since | `closing.Ledger`, T64m5 |

**Sign-off locks the year.** `POST /v1/year-closes/{id}:sign-off`
(If-Match; people only; `closing.close.sign_off`, `ledger.period.lock` and
`ledger.entry.read`) does ADR-0058 §7's steps with the year's periods: it
refuses while a manual task is open or a check fails when re-run; then
every regular period of each covered ledger must be `locked` (409
`closing.year_periods_not_locked`, naming them: the period closes, or T32
directly, lock them) and every special period `closed` or `locked` (409
`closing.period_not_closed`); each closed special period is locked through
`LockPeriod`, the leading ledger first, then by code, and within a ledger
by period; the `year_sign_off` row records per ledger the periods it
locked; `YearCloseSignedOff`. After it, every period of the year is locked
in the covered ledgers. Reopening follows ADR-0058 §7: an unlocked period
reopens the signed-off close at its first write, and a second sign-off
locks it again.

### 9. Open items
Open items are not carried forward: an item is a journal line, open until a
clearing of any later year clears it (clearings read lines of every year),
and the reconciliation account's balance is carried with every other
balance sheet account (§5). Nothing is copied into the new year, so payment
runs, aging, revaluation and the open item lists keep reading the lines as
they do. A check that the open items of each reconciliation account add up
to its balance at year end is not built: migrated opening balances may sit
on a reconciliation account without items (docs/migration.md's `balances`
mode), so the equality is not guaranteed; until a migration specialist
says when it holds, it is a manual task of the template (a gap below).

### 10. Parallel ledgers and foreign currency
- **Per ledger** (ADR-0045): a carry-forward is run per ledger, from that
  ledger's own balances (its copies and its ledger-specific postings), to
  the company's one retained earnings account; a ledger assigned later
  starts from its own first year. A year close covers the ledgers it names;
  a parallel ledger still open for adjustments after the leading ledger's
  year is locked is left out and closed by a later year close, or locked
  through T32.
- **Foreign currency**: balances are carried per transaction currency with
  their local and group amounts as booked, revaluation included (a
  year-end run's valuation entries are dated the year's last day and carried;
  their reset entries are dated the next year's first day, ADR-0042). The
  carry-forward revalues and translates nothing.

### 11. Assets
Assets need no carry-forward: their year totals are kept per year and the
history sheet sums the years before (ADR-0057 §5, §9), and the asset
reconciliation accounts' balances are carried like every balance sheet
account. The year close checks that every regular period's depreciation is
posted (`depreciation_year_posted`); SAP's asset fiscal year change has no
counterpart to build.

### 12. Audit, events, permissions and agents
- **Audit** (ADR-0011): a carry-forward run (with the run it superseded),
  each year-close write and the special periods' locks (T32's records,
  unchanged).
- **Events** (CloudEvents through the outbox, ADR-0007): `BalanceCarriedForward`
  on the ledger channel (run, company, ledger, years, basis, the run it
  superseded, the result per currency); `YearCloseChanged` and
  `YearCloseSignedOff` on the closing channel, shaped as their period-close
  counterparts with the fiscal year in place of the period.
- **Permissions**: one new, `ledger.carry_forward.run`, granted with
  `ledger.period.lock` in the role catalog; the year close uses ADR-0058's
  `closing.close.*`.
- **Agents** (ADR-0039): they read runs, balances and year closes at level
  0, and may park a year-end adjustment in a special period with
  `parkJournalEntry` (proposing, as for any entry, which only a person
  posts). Running a carry-forward and every year-close write are
  `x-agent: false`. No agent proposes year-end adjustments of its own: no
  leaf asks for it (a gap below).

### 13. No workflow; budgets
A run is one transaction: an aggregate over one year's balances (or every
year's, once) and an insert of its rows. ADR-0012 puts long-running work in
Temporal; this is not, so no workflow, dispatcher kind or schedule. New
rows for 01 §1.3, measured by budget tests at a company with 5,000 accounts
in 3 currencies with balances in every period:

| Operation | p95 | p99 |
|---|---|---|
| Balance carry-forward of one company and ledger, chained (one year of balances) | 3 s | 6 s |
| Balance carry-forward of one company and ledger from every year (ten years of balances) | 10 s | 20 s |
| Carry-forward balances, one page | 150 ms | 400 ms |

The year close's reads and writes are ADR-0058's (a single resource, a
list page).

### 14. Data model (planned, not built)
```
ledger.period_status         + opened_count integer NOT NULL DEFAULT 0 (only OpenPeriod adds one)
ledger.carry_forward_run     (tenant_id, id, legal_entity_id, ledger_id, from_fiscal_year, to_fiscal_year,
                              basis chained|all_years, chained_from_id, retained_earnings_account_id,
                              status current|superseded, superseded_by_id, superseded_at, rows,
                              created_at, created_by)
                              PK (tenant_id, id); UNIQUE (tenant_id, legal_entity_id, ledger_id, from_fiscal_year)
                              WHERE status = 'current'
ledger.carry_forward_balance (tenant_id, run_id, account_id, kind balance|result, currency_tc,
                              amount_tc, amount_lc, amount_gc)
                              PK (tenant_id, run_id, account_id, kind, currency_tc); insert-only
ledger.carry_forward_period  (tenant_id, run_id, period_id, opened_count)  PK (tenant_id, run_id, period_id); insert-only
closing.year_close           (tenant_id, id, legal_entity_id, fiscal_year, year_end, config_version, status,
                              version, created_*, updated_*)  UNIQUE (tenant_id, legal_entity_id, fiscal_year)
closing.year_close_ledger    (tenant_id, close_id, ledger_id)
closing.year_close_task      (as closing.close_task)
closing.year_sign_off        (as closing.sign_off; locked: per ledger the special periods and whether it locked each)
```
Database rules with schema tests: `tenant_id` leads every key and RLS on
every table (ADR-0003); a run's years are consecutive; `result` rows only
on the run's retained earnings account; a run moves only current →
superseded, naming its successor, and nothing else of it changes; balance
and period rows and sign-offs are never updated or deleted; one current
run per company, ledger and year; `opened_count` never goes down; the year
close's rules as ADR-0058 §5's. Migrations are expand-only (new tables and
a column with a default).

## Alternatives
- **Carry-forward as journal entries** (opening entries in period 1, or
  closing entries of the result into retained earnings in a special
  period). Familiar from US practice and visible in the trial balance, but
  every reader that sums all years (`Accounts.Balances`, revaluation, the
  asset sheet's reconciliation) would count the balances twice, a re-run
  would need reversals, and the P&L accounts' year would show zero.
  Rejected.
- **No stored carry-forward: opening balances computed on read** from every
  earlier year. Always current, but each read grows with the years kept
  (ten online, 01 §1.3), and old years could never move to cheaper storage.
  Rejected for stored runs with staleness.
- **Late postings carried automatically** (SAP, from memory): the
  projector, or the posting, would update the later years' carry-forward.
  Posting would write a hot row per account and year (ADR-0006 rejected
  that for balances), and the carried figures would change under a signed
  off year without a person seeing it. Rejected: staleness and a re-run.
- **Year closes in `period_close` with an empty period.** One set of
  tables, but a rolling deploy's old replica fails reading them (§8).
  Rejected.
- **Year-end tasks on period 12's close.** No new resource, but period 12
  is locked within days of the year end, while audit adjustments go to the
  special periods for months after. Rejected.

## Consequences
- The ledger gains special-period postings, a carry-forward with three
  tables and a column on `period_status`; `closing` gains a year close with
  four tables and the same code paths as the period close.
- A fiscal year is fully locked only after its year close; until the
  carry-forward is current, the next year's opening balances are marked
  stale wherever they are read.
- Readers of balances are unchanged; a reader wanting a year's opening
  balance reads the carry-forward, not the trial balance.
- A late posting into a closed year needs T32's unlock, an opened period,
  a re-run of the carry-forward and a second sign-off: each step audited.

## Boundaries with the other steps
- **T32 (period lock)**: the year close locks special periods through it;
  unlocks stay approval requests.
- **ADR-0058 (T64)**: the period closes lock the regular periods the year
  close requires locked; the template, checks, sign-off and reopening are
  shared.
- **ADR-0045 (T52)**: per ledger throughout (§10).
- **ADR-0042 (T48)**: year-end revaluation is the last period's run, checked
  again by the year close.
- **ADR-0057 (T57)**: §11.
- **T61 (tax)**: question 2. **T65 (statutory reports)** and **T68
  (consolidation)** read the carry-forward when they are designed.

## Steps (slices)
Each is a slice (ADR-0053): one capability Carla, the controller, uses end
to end through the public API, with its contract, migration, domain code,
tests (real PostgreSQL, `FailingTx` on every database path, 100% coverage),
docs and a walkthrough step as its evidence. Lane F, in this order; about
half a day each.

**The walkthrough's year end.** A new step `step_year_end`, before
`step_close` (which stays the story's last, ADR-0058), works on the fiscal
year before the walkthrough company's today, which the walkthrough's
configuration sets up with a number range and on which no other step
posts, so locking it touches nothing another step reads. Each slice adds its
part in order: special-period posting (T64m1), carry-forward (T64m2,
T64m3), the year close (T64m4), its checks (T64m5), sign-off (T64m6).

| Step | Work | Acceptance criteria and walkthrough evidence |
|---|---|---|
| T64m1 | **Postings into special periods** | `fiscal_period` (13 up to the calendar's last special period) on `postJournalEntry`, `parkJournalEntry` (kept and checked again at `postParkedJournalEntry`) and `reverseJournalEntry`; `ledger.special_period_date`, `ledger.no_fiscal_period`, `ledger.period_not_open` per ledger, `ledger.special_period_tax_code`; the period read `FOR SHARE` in the postings' order; tests for each ledger closed in turn. Walkthrough: Carla opens special period 13 of the year, posts an audit adjustment into it dated the year's last day and reads it in the trial balance under period 13; a posting dated in period 11 naming period 13 is refused with `ledger.special_period_date` |
| T64m2 | **Retained earnings and the carry-forward run** | `companies[].year_end.retained_earnings_account` (tier 3, validated as §4); `ledger.carry_forward_run`, `carry_forward_balance`, `carry_forward_period`; `POST /v1/balance-carry-forwards` (per ledger, chained or from every year, §5's preconditions, problems, advisory lock, current and superseded); `GET` one run with its totals per currency; `BalanceCarriedForward`; audit; `ledger.carry_forward.run`; tests: rows sum to zero per currency in every amount, chained equals from-every-year over three years, P&L carried per currency to retained earnings, a parallel ledger's own figures; the chained budget. Walkthrough: Ada sets the retained earnings account through a change request; Carla closes the year's open periods and carries the year forward in 0L and 2L; the run's result equals the year's profit and loss in the trial balance |
| T64m3 | **Reading the carry-forward, staleness and re-runs** | `opened_count` on `ledger.period_status` (`OpenPeriod` only; trigger and schema test); `stale` with the periods opened since on every read of a run; `GET /v1/balance-carry-forwards` (list) and `/{id}/balances` (names, kinds, totals with every page); a chained run refusing a stale predecessor (`ledger.carry_forward_stale`); the from-every-year and page budgets. Walkthrough: Carla reads the balances (the retained earnings result row, a USD bank account in USD and CAD); she opens special period 13, posts a late adjustment, the run reads stale naming the period; she closes it, re-runs, the old run reads superseded and the new one current with the adjustment carried |
| T64m4 | **The year close and its checklist** | `close.year_tasks` (validated, year checks only); `closing.year_close`, `year_close_ledger`, `year_close_task` (RLS, rules, schema tests); `POST/GET/list /v1/year-closes`, the task operations (`PATCH`, `:complete`, `:reopen`, `:check`) sharing the period close's code; checks `no_parked_entries` over the year and `revaluation_posted` on the year's last day; `YearCloseChanged`; audit. Walkthrough: Ada adds year tasks through a change request; Carla opens the year close, completes a manual task, the revaluation check passes on the year-end run |
| T64m5 | **Year-close checks: depreciation and carry-forward** | `depreciation_year_posted` (T64k's check per regular period and covered ledger) and `carry_forward_current` through `closing.Ledger`'s new read; results naming ledgers and periods. Walkthrough: `carry_forward_current` fails after a late posting (the run stale) and passes after the re-run; `depreciation_year_posted` passes with every period's run posted, or nothing to select |
| T64m6 | **Year-close sign-off locks the year** | `:sign-off` as §8 (tasks, checks re-run, regular periods locked: `closing.year_periods_not_locked`, special periods closed: `closing.period_not_closed`, `LockPeriod` on each closed special period in the postings' order, `year_sign_off`, `YearCloseSignedOff`); reopening after an unlock and a second sign-off (test); a concurrency test of a sign-off and a posting into a special period. Walkthrough: sign-off refused while a regular period is unlocked; Carla locks it, closes period 13 and signs off; every period of the year reads locked and a posting into period 13 is refused with `ledger.period_not_open` |

Each slice updates 03 (§3.7 for the ledger's tables, §3.22 for the year
close), 04 where a convention changes, the walkthrough and this ADR's "As
built".

## Decided by default (ADR-0053 d), for the product owner to overrule
1. The carry-forward is stored as balances per run, never as journal
   entries, and the trial balance does not show it (§5).
2. Balance sheet accounts carry their balances per account and transaction
   currency, with local and group amounts as booked; no account assignment
   is carried (period balances keep none; ADR-0050 P3) (§5, §10).
3. The result is carried to the retained earnings account per transaction
   currency, so the opening sums to zero in every currency (§5).
4. A run is chained from the previous year's current carry-forward, or
   computed from every year when there is none; a stale predecessor is
   refused (§5).
5. A run needs the year (and every year it reads) closed or locked, the
   next year set up and a retained earnings account (§5).
6. Late postings make a carry-forward stale, judged by the periods opened
   since; a person re-runs it; nothing is carried automatically (§6).
7. Special periods take manual and parked entries and their reversals only,
   dated in the year's last regular period (§3).
8. The year close is a separate resource and tables in `closing`, sharing
   the period close's code; its template is `close.year_tasks` (§8).
9. Year-close sign-off needs every regular period locked and locks the
   special periods; with that, the year is locked (§8).
10. Open items are not carried; no automatic open-item check (§9).
11. Assets need no carry-forward; the year close checks every period's
    depreciation (§11).
12. One new permission, `ledger.carry_forward.run`, granted with
    `ledger.period.lock`; agents read and may park only (§12).
13. No workflow; the budgets of §13.

## Questions for the product owner
1. **Retained earnings per company** (accounting rule). Built: one retained
   earnings account per company for every ledger. SAP allows several per
   chart (by P&L statement account type), and a local GAAP or tax ledger
   might carry its result to another account. Recommendation: one per
   company for the pilot (Canada and the UAE report under IFRS alone,
   ADR-0045's rollout note); add one per ledger if a pilot keeps a local
   ledger with its own equity account.
2. **Tax codes on special-period postings** (tax rule). Built: refused
   (`ledger.special_period_tax_code`), so no year-end adjustment changes a
   tax return. Recommendation: keep the refusal until the tax specialist
   says whether a pilot country's returns take year-end adjustments, and
   how they are dated.
3. **Segregation of duties on the year-close sign-off** (security
   posture). Built: as ADR-0058's sign-off, any person holding
   `closing.close.sign_off` and `ledger.period.lock`. Recommendation: the
   answer to ADR-0058's open question applies to both closes; keep it as
   built for the pilot.

## Gaps (out of scope, not built, no question asked)
Neither the PO decision nor an ADR asks for these; each becomes a tracker
item only when one does (07 §7.5).
- Opening balances in the trial balance (a period 0 row needs a contract
  change) and financial statements (T65).
- An automatic check of open items against their reconciliation accounts
  (§9), until a migration specialist says when it holds.
- Supplier and customer balances carried per partner (SAP F.07).
- Year-close tasks in the cross-close worklist (`listCloseTasks`), whose
  rows carry a period.
- Agent proposals of year-end adjustments.
- Archiving old years on the chained carry-forward (ADR-0006).
- Screens in the web workspace (headless first, ADR-0053).

## Facts for a specialist (stated from memory, not verified here)
- SAP: FAGLGVTR and its period 0 records in S/4HANA, OB53's retained
  earnings account per P&L statement account type, the automatic carry-forward
  of later postings, F.07, special periods with OB52, AJRW and AJAB, as in
  "What SAP does".
- How a profit or loss in a foreign transaction currency is presented in
  retained earnings (§5 carries it per transaction currency, with its local
  and group amounts).
- Whether year-end adjustments in special periods count in a period's tax
  return in Canada and the UAE (question 2), and any statutory deadline for
  a year's close: none is asserted.

## Sources
- The repository: ADR-0058 (§1's question, the close), ADR-0045, ADR-0006,
  ADR-0042, ADR-0050 (P3), ADR-0057 (§5, §9), ADR-0003, ADR-0005, ADR-0007,
  ADR-0008, ADR-0011, ADR-0012, ADR-0039, ADR-0053; 07-transition §7.5-§7.7;
  03 §3.7 (entry locks, periods) and §3.22; `internal/ledger/setup.go`
  (`fiscalYear`, `calendar`), `journal.go` (`postingPeriod`, `periodOn`),
  `periods.go`, `reads.go` (`balanceRows`), `ports.go`
  (`Accounts.Balances`), `revaluation.go`, `closeperiods.go`,
  `returnperiods.go`; `internal/assets/sheet.go`, `closechecks.go`;
  migrations 000005, 000049, 000163, 000194; docs/migration.md;
  `api/openapi.yaml` (`listBalances`, `PeriodClose`).
- The product owner's decision of 2026-11-04 (year-end close is its own
  design).
- No external source was read (no internet access in the lane); every
  statement about SAP, IFRS and the pilot countries is flagged above for a
  specialist.

## As built
(Filled in by the slices T64m1-T64m6.)
