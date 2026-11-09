# ADR-0060: Year-end close

**Status:** accepted (T64m design step, 2026-11-05; decided by default under
ADR-0053 d; `go-reviewer` approved in round 2 with no blocking finding). The
decisions taken by default and the questions for the product owner are
listed at the end; the slices T64m1-T64m6 are in the tracker under
Financial close. The design leaf T64m itself is added by the product
owner's items of 2026-11-04 on another branch; integration marks it done
with this ADR. Written as ADR-0059 on its branch; renumbered ADR-0060 at
batch58's integration, where group reporting and consolidation kept 0059.

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
  `NOT is_special`); clearings (000049) and revaluation key dates
  (`revaluation.go`) leave them out, and period closes (ADR-0058 §6,
  `closing.special_period`, `closeperiods.go`) and tax returns
  (`returnperiods.go`) leave them out saying they "are the year-end
  close's". `:open`, `:close`, `:lock` and the unlock request
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
  period (a new nullable `fiscal_period` on `ledger.parked_entry`, set,
  changed or removed by `PATCH` and shown on the `ParkedJournalEntry`
  representation) and is held to these rules again when it is posted.
  Entry templates (ADR-0056) take no special period: a schedule parks
  regular entries. A reversal
  names its own date and optional special period by the same rules; without
  one it goes to the regular period of its date, as today.
- **Tax.** A line with a tax code in a special period is refused (422
  `ledger.special_period_tax_code`) until the tax specialist confirms how
  year-end adjustments enter the returns (question 2, answered
  po:2026-11-05): tax returns select tax items by their tax period's dates, and
  whether a year-end adjustment may carry tax is a tax rule. The UAE tax
  specialist answered for the VAT 201 on 2026-11-09 (po:2026-11-09, C10,
  [M]; question 2): yes, a year-end adjustment can carry VAT, which follows
  the tax documents and statutory adjustments, not the accounting date;
  an adjustment carrying VAT belongs to the return of the period in which
  its tax invoice or tax credit note is issued (or its tax point falls),
  and the statutory annual adjustments (input tax apportionment, the
  capital assets scheme) to the first return after the end of the tax year
  or adjustment period (VAT Executive Regulation, apportionment and capital
  assets articles). Accruals, provisions and reclassifications carry no
  VAT and keep the refusal. The specialist suggests allowing a VAT code on
  a year-end adjustment only when it references a tax document, its return
  period derived from that document's date: a tracker leaf. Canada's GST34
  is not answered: its refusal stays.
- **The period close's parked-entry check.** ADR-0058's
  `no_parked_entries` selects parked entries by date within the period
  (`CloseChecks.Parked`), so an audit adjustment parked for period 13,
  dated in period 12, would block period 12's sign-off for months. From
  T64m1 a period close's check leaves out parked entries naming a special
  period; the year close's check covers them (§8).
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
- each **profit and loss account** carries nothing: year Y's profit or
  loss (the P&L accounts' balances of Y, per transaction currency) is
  carried to the retained earnings account as a `result` row beside that
  account's own `balance` row. The results of years before Y are already
  in that `balance` row: `result` is always year Y's alone, which the
  run's totals, its event and its reads report as the year's result.

Its rows sum to zero per transaction currency in transaction, local and
group currency, because every entry does (000005); a test asserts it on
every run. Carrying the result per transaction currency is what keeps that
invariant; how SAP presents a foreign-currency result in retained earnings
is for a specialist (below). An accounting specialist found it acceptable on
2026-11-09 (po:2026-11-09, B15; a technical review, not a signed opinion):
IFRS prescribes no closing-entry mechanics, and carrying the result per
transaction currency with its local and group amounts as booked is fine
provided the local total equals the year's profit or loss and the group
total the translated profit used in consolidation. The split by
transaction currency has no meaning for equity and is not to be presented
in reports.

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
  Y-1 (§6): that run's rows, its `result` rows added into the retained
  earnings account's `balance` rows, plus year Y's balances (periods 1-16),
  the trial balance's read (projected balances plus unprojected lines,
  `balanceRows`). A stale one is refused (409 `ledger.carry_forward_stale`,
  naming the year to re-run first), so a chain never builds on figures that
  changed.
- **From every year** up to Y when there is no carry-forward from Y-1 (the
  company's first, or the first in a ledger assigned later, ADR-0045): the
  sum of every year's balances, the P&L of years before Y on the retained
  earnings account's `balance` row and year Y's on its `result` row. The
  two ways give the same rows, `balance` and `result` alike, which a test
  proves on three years. The chain keeps a run to one year's
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

**Locking** (ADR-0008, 03's entry locks): first a transaction-scoped
advisory lock per ledger run, keyed on `ledger.carry_forward`, the tenant,
company, ledger and year Y, taken for every ledger of the
request in one call in the ledgers' order (leading first, then by code), so
two runs of the same year serialise; then the periods of year Y and every
earlier year of every ledger of the request, `FOR SHARE`, in one statement
in the project's one order (03 §3.7's entry locks, ADR-0058 As built:
by fiscal year and period, then the leading ledger first, then by code,
sorted in the statement). A posting into those
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
  every period). A trigger sets it to the old count plus one on every
  update that changes the status to `open`, whoever writes it (`OpenPeriod`
  today, and the test fixtures that set `open` directly), and refuses any
  other change to it; a schema test proves both. A posting needs an open
  period, so a period whose count has not moved and that is not open now
  has taken no posting since it was read.
- **Stale.** A run records the count of every period of year Y and of
  every earlier year in its ledger (`ledger.carry_forward_period`, at most
  16 per year kept), whether it was chained or not. It is stale when one of
  them is open now or its count moved, or when its ledger has a period of
  year Y or earlier that the run did not record (a year set up later, by a
  number range added to load history) that is open now or has been opened
  (`opened_count` above 0); judged at each read of the run. A late posting
  into year Y (an unlock approved through T32, the period opened, the entry
  posted) makes the carry-forward from Y stale, and every later year's too,
  since each recorded Y's periods; no run's staleness depends on another
  run's. Locking or unlocking a period does not move the
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
  keeps): one run per ledger in one transaction (locks as §5: the ledgers'
  advisory locks, then every period in the one order);
  201 with the runs. People only (`x-agent: false`); `ledger.carry_forward.run`
  and, as `x-also-permissions`, `ledger.entry.read` (it returns balances).
- `GET /v1/balance-carry-forwards?legal_entity_id&fiscal_year&ledger_id&status`
  (cursor-paged) and `GET /v1/balance-carry-forwards/{id}`: the run with its
  company (code, name), ledger (code, name), years, basis (`chained` or
  `all_years`, with the run it chained from), the retained earnings account
  (number, name), status, who ran it (id and name) and when, the run that
  superseded it, `stale` with the periods
  opened since (year, period, ledger), the number of rows, and its totals
  per transaction currency: year Y's result carried to retained earnings and the
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
| `no_parked_entries` | no parked entry of the company dated in the fiscal year is `parked` or `pending_approval` (ADR-0058's check, over the year's dates, so entries parked for a special period are counted here) | `closing.Ledger`, T64m5 |
| `revaluation_posted` | each covered ledger has a posted revaluation run keyed on the year's last day (ADR-0058's check, on that date) | `closing.Ledger`, T64m5 |
| `depreciation_year_posted` | for each covered ledger and each regular period of the year, T64k's check passes (a posted run, or nothing a run would select); the result names each ledger and period that fails | `closing.Depreciation`, T64m5 |
| `carry_forward_current` | each covered ledger has a current carry-forward from the year that is not stale; the result names each ledger without one, or stale with the periods opened since | `closing.Ledger`, T64m5 |

**Sign-off locks the year.** `POST /v1/year-closes/{id}:sign-off`
(If-Match; people only; `closing.close.sign_off`, `ledger.period.lock` and
`ledger.entry.read`) does ADR-0058 §7's steps with the year's periods: it
refuses while a manual task is open or a check fails when re-run; then
every regular period of each covered ledger must be `locked` (409
`closing.year_periods_not_locked`, naming them: the period closes, or T32
directly, lock them) and every special period `closed` or `locked` (409
`closing.period_not_closed`); the year's periods of every covered ledger,
regular and special, are read `FOR UPDATE` in one statement, so no unlock
slips in between the check and the commit, in the project's one order (by
period, then the leading ledger first, then by code; 03 §3.7's entry
locks) and each closed one is locked through `LockPeriod` in that order;
the `year_sign_off` row records per ledger the periods it
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
to its balance at year end is not built: no ADR asks for it, and whether
the equality holds for every company depends on what the migration loaded
on reconciliation accounts (docs/migration.md keeps open vendor items as
lines in both modes; whether anything else of a reconciliation account's
SAP balance can reach it is for a migration specialist). Until then it is
a manual task of the template (a gap below). The SAP specialist answered on
2026-11-09 (po:2026-11-09, A15, [M]): yes, other things can, each told by
a field (docs/migration.md, "What to check on a real SAP system": special
G/L postings on alternative reconciliation accounts, noted items, a
changed reconciliation account, balances without vendor items, a period
000 carry-forward, valuation posted to the account, items cleared after
the key date). With ACDOCA the vendor line is the G/L line, so the equality
usually holds; the specialist recommends a check per account at the key
date over lines with a vendor (LIFNR), no special G/L indicator (UMSKZ)
and not noted (BSTAT not `S`), reported rather than refused: a tracker
leaf for the migration's reconciliation. The platform's own year-end
check stays a gap: no ADR asks for it.

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
- **Segregation of duties on the sign-off.** The product owner answered
  ADR-0058's question on 2026-11-04: no separation for the pilot, who
  signed is recorded, and the signer must differ from the preparer before
  go-live (tracker T64l). The year-close sign-off follows the same answer
  by default: its row records the signer (id and name), and T64l's rule,
  when built, applies to both kinds of close.
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
ledger.parked_entry          + fiscal_period smallint NULL (13-16 only; a special period named by the entry)
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
run per company, ledger and year; `opened_count` moves only by one on a
change to `open`, set by the trigger; a parked entry's `fiscal_period` is
null or 13-16; the year
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
year before the walkthrough company's today (`PREV`). The walkthrough's
configuration has no number range for it today (its ranges come from the
mock extract's journal year, and `PREV` serves only prices and tax-rate
dates), so T64m1's part sets `PREV` up through a change request (a number
range per company) and posts the business the later parts carry forward:
an expense and a revenue entry, a USD bank posting with the USD rates on
`PREV`'s last day, and `PREV`'s year-end revaluation run, before any
carry-forward (a run reopening period 12 later would make it stale). No other step
posts into `PREV`, so locking it touches nothing another step reads. Each
slice adds its part in order: special-period posting (T64m1),
carry-forward (T64m2, T64m3), the year close (T64m4), its checks (T64m5),
sign-off (T64m6).

| Step | Work | Acceptance criteria and walkthrough evidence |
|---|---|---|
| T64m1 | **Postings into special periods** | `fiscal_period` (13 up to the calendar's last special period) on `postJournalEntry`, `parkJournalEntry` (stored on `ledger.parked_entry`, changed by `PATCH`, shown on the representation, checked again at `postParkedJournalEntry`) and `reverseJournalEntry`; `ledger.special_period_date`, `ledger.no_fiscal_period`, `ledger.period_not_open` per ledger, `ledger.special_period_tax_code`; the period read `FOR SHARE` in the postings' order; a period close's `no_parked_entries` leaving out entries parked for a special period; tests for each ledger closed in turn. Walkthrough: `PREV` set up through a change request with its business posted (above); Carla opens special period 13 of `PREV`, posts an audit adjustment into it dated the year's last day and reads it in the trial balance under period 13; a posting dated in period 11 naming period 13 is refused with `ledger.special_period_date` |
| T64m2 | **Retained earnings and the carry-forward run** | `companies[].year_end.retained_earnings_account` (tier 3, validated as §4); `opened_count` on `ledger.period_status` with its trigger and schema test; `ledger.carry_forward_run`, `carry_forward_balance`, `carry_forward_period`; `POST /v1/balance-carry-forwards` (per ledger, chained or from every year, §5's preconditions and problems, a stale predecessor refused with `ledger.carry_forward_stale`, the locks of §5, current and superseded); `GET` one run with year Y's result and totals per currency; `BalanceCarriedForward`; audit; `ledger.carry_forward.run`; tests: rows sum to zero per currency in every amount, chained equals from every year over three years (`balance` and `result` rows alike), `result` is year Y's alone in the second and third year, a parallel ledger's own figures; the chained budget. Walkthrough: Ada sets the retained earnings account through a change request; Carla closes `PREV`'s open periods and carries `PREV` forward in every ledger; the run's result equals `PREV`'s profit and loss in the trial balance |
| T64m3 | **Reading the carry-forward, staleness and re-runs** | `stale` with the periods opened since (recorded ones whose count moved or that are open, and unrecorded ones of year Y or earlier opened since) on every read of a run; `GET /v1/balance-carry-forwards` (list) and `/{id}/balances` (names, kinds, totals with every page); the from-every-year and page budgets. Walkthrough: Carla reads the balances (the retained earnings `result` row, the USD bank account in USD and CAD); she opens special period 13, posts a late adjustment, the run reads stale naming the period; she closes it, re-runs, the old run reads superseded and the new one current with the adjustment carried |
| T64m4 | **The year close and its manual checklist** | `close.year_tasks` (validated, year checks only); `closing.year_close`, `year_close_ledger`, `year_close_task` (RLS, rules, schema tests); `POST/GET/list /v1/year-closes` and the task operations (`PATCH`, `:complete`, `:reopen`) sharing the period close's code; `YearCloseChanged`; audit. Walkthrough: Ada adds year tasks through a change request; Carla opens the year close of `PREV`, assigns and completes a manual task; a second year close of `PREV` is refused with `closing.year_close_exists` |
| T64m5 | **Year-close checks** | `:check` on a year close; `no_parked_entries` over the year (special periods included), `revaluation_posted` on the year's last day, `depreciation_year_posted` (T64k's check per regular period and covered ledger) and `carry_forward_current` through `closing.Ledger`'s new read; results naming ledgers, periods and entries. Walkthrough: the revaluation check passes on `PREV`'s year-end run; `carry_forward_current` fails after a late posting (the run stale) and passes after the re-run; `depreciation_year_posted` passes, nothing in `PREV` to depreciate |
| T64m6 | **Year-close sign-off locks the year** | `:sign-off` as §8 (tasks, checks re-run, regular periods locked: `closing.year_periods_not_locked`, special periods closed: `closing.period_not_closed`, the special periods read `FOR UPDATE` in one statement by period, leading ledger, code and locked through `LockPeriod` in that order, `year_sign_off` with the signer's name, `YearCloseSignedOff`); reopening after an unlock and a second sign-off (test); a concurrency test of a sign-off and a posting into a special period on a company with three ledgers whose ids and codes sort differently. Walkthrough: sign-off refused while a regular period of `PREV` is unlocked; Carla locks it, closes period 13 and signs off; every period of `PREV` reads locked and a posting into period 13 is refused with `ledger.period_not_open` |

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
13. The year-close sign-off follows the product owner's 2026-11-04 answer
    on the period close's (no separation for the pilot, the signer
    recorded, T64l before go-live covering both closes) (§12).
14. A period close's parked-entry check leaves out entries parked for a
    special period; the year close's counts them (§3, §8).
15. No workflow; the budgets of §13.

## Questions for the product owner
Both answered by the product owner on 2026-11-05 (po:2026-11-05), as
recommended.

1. **Retained earnings per company** (accounting rule). **Answered
   po:2026-11-05: one retained earnings account per company for every
   ledger, as recommended.** Built: one retained
   earnings account per company for every ledger. SAP allows several per
   chart (by P&L statement account type), and a local GAAP or tax ledger
   might carry its result to another account. Recommendation: one per
   company for the pilot (Canada and the UAE report under IFRS alone,
   ADR-0045's rollout note); add one per ledger if a pilot keeps a local
   ledger with its own equity account.
2. **Tax codes on special-period postings** (tax rule). **Answered
   po:2026-11-05: the refusal stays until the tax specialist (Musto
   Ibrahim) confirms how year-end adjustments enter the GST34 and the VAT
   201, as recommended.** Built: refused
   (`ledger.special_period_tax_code`), so no year-end adjustment changes a
   tax return. Recommendation: keep the refusal until the tax specialist
   says whether a pilot country's returns take year-end adjustments, and
   how they are dated. **The UAE tax specialist's answer (po:2026-11-09,
   C10, [M]):** for the VAT 201, a year-end adjustment may carry VAT when it
   rests on a tax document, in the return of that document's period (§3);
   a tracker leaf. The GST34 is not answered and keeps the refusal.

## Gaps (out of scope, not built, no question asked)
Neither the PO decision nor an ADR asks for these; each becomes a tracker
item only when one does (07 §7.5).
- Opening balances in the trial balance (a period 0 row needs a contract
  change) and financial statements (T65).
- An automatic check of open items against their reconciliation accounts
  (§9) on the platform; the migration specialist said when it holds on
  2026-11-09 (A15), and the migration's check is a tracker leaf.
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
  and group amounts): answered on 2026-11-09 (B15, §5).
- Whether year-end adjustments in special periods count in a period's tax
  return in Canada and the UAE (question 2): answered for the UAE on
  2026-11-09 (C10, §3), open for Canada; and any statutory deadline for a
  year's close: none is asserted.

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

**T64m1, postings into special periods** (migrations 000225 and 000226).
Built as §3 says; the choices taken by default (ADR-0053 d), for the
product owner to overrule:
1. *Where the rule is checked.* `fiscal_period` is checked
   (`ledger.specialPeriod`: the calendar has the special period in the
   posting date's fiscal year, else 422 `ledger.no_fiscal_period` on
   `fiscal_period`; the posting date in its dates, else 422
   `ledger.special_period_date`; no line with a tax code, else 422
   `ledger.special_period_tax_code` on `lines[i].tax_code`) only on the
   API's manual paths: `postJournalEntry` (before the approval or service
   limit, which then read the special period's status), `parkJournalEntry`,
   `updateParkedJournalEntry`, `postParkedJournalEntry` and its approval
   (`ManualEntries.Approved`), and `reverseJournalEntry`. A parked entry is
   checked when it is parked and changed as well as when it is posted, so a
   date in no fiscal year naming a special period is refused when parked
   (a regular one still is refused only when posted). Whether the period is
   open is `postingPeriod`'s, unchanged but for the period number, `FOR
   SHARE` in the postings' order.
2. *Reversals.* The API's reversal (`reverseManually`) may name a special
   period; the contexts' reversals (`Postings.Reverse`) and revaluation
   cancellations go through the unchanged `reverseJournalEntry` and never
   do. Any entry the API may reverse (manual, migrated, payments') may be
   reversed into a special period: "their reversals" (decision 7) is read
   as reversals a person posts, since a reversal is a manual correction.
3. *Database backstop.* `ledger.check_entry_period` refuses an entry in a
   special period that is neither `source_context` `ledger` nor a reversal;
   the date rule needed nothing new (a special period's dates are its
   year's last regular period's). The tax-code refusal is the
   application's only: a line trigger would read the period per line.
   `parked_entry.fiscal_period` is null or 13-16
   (`parked_entry_fiscal_period_special`, added NOT VALID and validated by
   000226); the contract bounds it too, so no problem is registered for it.
4. *Events.* `JournalEntryPosted` already carries the entry's
   `fiscal_period`; `ParkedJournalEntryChanged` is unchanged (the period is
   on the representation).
5. *The period close's check* (decision 14): `ledger.CloseChecks.Parked`
   leaves out entries naming a special period (`fiscal_period IS NULL`);
   T64m5 adds the year's read.
6. *Walkthrough* (`step_year_end`, before `step_close`): Ada adds a
   `journal_sa` number range of the company for `PREV` (a tier 1 change,
   approved on submission), which creates `PREV`'s periods, closed, in 0L
   and 2L; Carla opens period 12; Tom posts a sale, the rent and USD
   2,000.00 into the RBC USD account at the 1 December rate (1.36), with
   1.38 on 31 December; Carla revalues at `PREV`'s last day in each ledger
   (the year close checks every covered ledger, §8), a gain of CAD 40.00.
   The reset is dated this year's 1 January, whose period a return filed
   in step 11 has locked in the leading ledger from April to September: the
   step then has it unlocked through an approved request (T32), opened,
   and closed and locked again after the run. Carla opens special period
   13 in both ledgers, posts the auditors' adjustment into it dated `PREV`'s
   last day, reads it in the trial balance under period 13 and its 2L copy,
   and a posting dated in November naming period 13 is refused with
   `ledger.special_period_date`. Every date is `PREV`'s or this year's 1
   January, absolute, so a run crossing midnight, or 31 December, reads
   the same.
7. *A gate defect fixed on the way.* `api` `TestProblemCodes` looked
   operations up by lower-cased method name on the API's handler, which
   also has the ledger's unexported `postJournalEntry` and
   `reverseJournalEntry` (the Postings paths): met after the operations'
   methods, they took their place, so those two operations' codes were
   derived from the wrong function (`ledger.approval_required` was missing
   from `postJournalEntry`). The lookup now takes exported methods only;
   the rewritten lists add codes to `postJournalEntry`,
   `reverseJournalEntry`, `cancelAsset`, `cancelRequisition`,
   `submitRequisition`, `updateRequisition`, `renewContract` and
   `updateContract`, which other unexported twins had hidden.

**T64m2, retained earnings and the carry-forward run** (migrations 000227
and 000228). Built as §4-§6 say; the choices taken by default (ADR-0053 d),
for the product owner to overrule:
1. *Where the account is kept.* `companies[].year_end.retained_earnings_account`
   (config `YearEnd`, validated on the field: in the chart, `equity`, no
   supplier or asset reconciliation account, not open-item managed; every
   account of the chart is postable, the configuration has no posting
   block) is applied as the account determination key `retained_earnings`
   (000227 adds the key, NOT VALID, validated by 000228). Tier 3 by an
   explicit rule. The application has no DELETE grant, so `ledger.Setup.Check`
   lets the account be changed but not removed once activated; each run
   records the account it used.
2. *The opened count.* A trigger (`period_status_opened_count`) adds one on
   every change of the status to `open`, so the previous release's
   `OpenPeriod` and the tests that set `open` directly count too; an insert
   starts at 0 (1 when inserted `open`, as test fixtures do) and any other
   change of the count is refused. An import (ADR-0029) restores the count
   as exported: the importer sets `app.importing` to the tenant for its
   transaction, as the erasure sets `app.erasing`, and an insert in that
   tenant keeps the count it carries (000229; a count reset on import would
   let a run read before a later opening look current again); an export
   written before 000227 carries none and gets 0, or 1 when open.
3. *The rows.* Computed in one statement from the predecessor's rows (when
   chained) and the ledger's balances of the year (chained) or of every
   year up to it, read as the trial balance reads them: projected
   `period_balance` plus the lines of entries the projector has not reached.
   A row whose three amounts are all zero is left out. A predecessor's
   `result` rows fold into the retained earnings account's `balance`, as
   that account is a balance sheet account; a predecessor that carried to
   another retained earnings account (the configuration changed) keeps that
   account's balance as a balance sheet account.
4. *Refusals and their order.* An unknown company or ledger is 422
   `request.reference_not_found` (on `legal_entity_id`, `ledger_ids`), a
   ledger not kept 422 `ledger.ledger_not_kept`; then 409
   `ledger.retained_earnings_missing`; after the locks, a ledger without
   periods of the year is 422 `ledger.no_fiscal_period` on `fiscal_year`
   (not listed in §5: without it the run would carry nothing), then 409
   `ledger.next_year_not_set_up`, then 409
   `ledger.carry_forward_periods_open` naming every open period as
   `year/period in ledger`; per ledger, a stale predecessor is 409
   `ledger.carry_forward_stale` naming its year and the periods opened
   since. Staleness is `openedSince`, which T64m3's reads reuse.
5. *The response.* 201 with the runs in one body (`items`, the leading
   ledger first, then by code) and no `Location`, since one request makes
   several runs; `GET /v1/balance-carry-forwards/{id}` carries the totals
   per transaction currency (result, debits and credits in transaction,
   local and group currency) and, over every currency, the year's result
   and the debits and credits in local and group currency (review round 1:
   a company with results in several currencies reads its result in its
   own currency without adding them up);
   `stale`, the list and the balances are T64m3's.
6. *Who ran it.* The ledger names the person through a new port,
   `ledger.PersonNames` (`app.LedgerPeople` over `identity.People.Named`,
   resolved with `idref`, so an unknown person fails the read rather than
   showing no name); no dependency direction changes.
7. *The permission.* `ledger.carry_forward.run` is in `ref.permission`.
   There is no role catalog in the repository: roles are each tenant's,
   granted by its administrator (the walkthrough grants every permission
   to finance staff), and no migration has ever granted a permission to
   existing roles, so "granted with `ledger.period.lock`" is the advice to
   administrators, not a migration.
8. *Event and audit.* `BalanceCarriedForward` (ledger channel) carries the
   run, company, ledger, years, basis, the run it chained from and the one
   it superseded, the retained earnings account, the row count, the result
   in local and group currency over every currency and the result per
   currency with a result row (signed, debit positive); the
   audit record `ledger.carry_forward_run.run` names the run superseded.
9. *Budget.* `TestCarryForwardBudget`: a chained run over 5,000 accounts
   with balances in 3 currencies in every period 1-16 of two years: median
   312 ms, p95 432 ms (scale 1) and 472 ms (scale 2) against 3 s, load
   7-15 on 10 cores; 01 §1.3 has the row.
10. *Walkthrough* (`step_year_end`): Ada adds account 330000 Retained
   earnings to the chart and names it in `year_end`, a tier 4 change with
   its regression evidence and two approvals; Carla closes `PREV`'s open
   periods (12 and 13) in both ledgers and carries `PREV` forward: one
   `all_years` run per ledger, each run's debits equal its credits per
   currency in every amount, and 0L's result (CAD -1,790.00: the sale, the
   rent, the audit adjustment and the revaluation gain) is `PREV`'s profit
   and loss in the trial balance.

**T64m3, reading the carry-forward, staleness and re-runs** (no
migration). Built as §6-§7 say; the choices taken by default (ADR-0053 d),
for the product owner to overrule:
1. *Staleness on every read.* `stale` and `opened_since` (each period's
   ledger id and code, fiscal year and period, by year and period) are on
   every representation of a run, `POST`'s, `GET`'s and the list's, judged
   at the read by T64m2's rule (`openedSinceOf`, one statement for all the
   runs of a page): a recorded period open now or whose opened count moved,
   or an unrecorded one of the run's years open or opened. A superseded run
   is judged the same way, so it may read stale too; its `status` says it
   was replaced. A later year's period opening leaves a run current.
2. *The ETag of a run* is its representation's (`apitypes.ETagOf`), no
   longer its version: `stale` changes without the run changing, and no
   write takes `If-Match` on a run.
3. *The list* (`listBalanceCarryForwards`) filters by company, fiscal year
   carried forward, ledger and status, over the companies the caller may
   act in, cursor-paged by `created_at` (the default) or `fiscal_year`,
   ties by run id; each item is the full run as `GET` shows it, read for
   the whole page in a fixed number of statements (companies, names,
   totals and staleness by run), not per run.
4. *The balances* (`listBalanceCarryForwardBalances`) page by account
   number byte by byte (`COLLATE "C"`), then currency, kind and account id
   (the row's key in its run, unique even if two accounts shared a number,
   which only the configuration rules out), filter `account_id`; each row names
   its account (number, name, type). Beside the rows, every page carries the
   whole run's totals whatever the filter, the run's local and group
   currencies and its result and debits and credits in them, the figures
   `GET` shows. A run's rows are insert-only, so the pages and totals agree
   without a snapshot.
5. *Budgets* (01 §1.3; scale 1, then 2, load 12-22 on 10 cores):
   `TestCarryForwardAllYearsBudget`, a run from every year over ten years
   of the 5,000 accounts in 3 currencies (2.4 million balance rows):
   median 2.68 s, p95 2.84 s (scale 1) and 2.47 s (scale 2) against 10 s;
   `TestCarryForwardBudget` adds a page of 500 of the chained run's 7,506
   rows, the first and one deep in the run alternately, after one warm
   read: median 19 ms, p95 20 ms (scale 1) and 40 ms (scale 2) against
   150 ms; with the account id ending the page order (review round 1),
   p95 26 ms and 22 ms. The chained run itself measured 436 ms median, p95 508 ms and
   651 ms. A page reads the run's totals with every page, an aggregate
   over its rows by the primary key's leading columns.
6. *Walkthrough* (`step_year_end`): Carla lists `PREV`'s runs, current and
   not stale; reads 0L's opening balances: the retained earnings `result`
   row equal to the run's result per currency and the RBC USD account's
   rows in USD (2,000.00, CAD 2,720.00) and CAD (the revaluation's gain,
   40.00), the page's totals the run's; she opens special period 13 again
   in both ledgers and posts a late adjustment of CAD 100.00: both runs
   read stale naming `PREV/13`, and a re-run while it is open is refused
   with `ledger.carry_forward_periods_open`; she closes it (the runs stay
   stale), carries `PREV` forward again: the new runs are current and not
   stale, 0L's result is CAD 100.00 more, the first runs read superseded
   by them, and the list of current runs is the new ones.

**T64m4, the year close and its manual checklist** (migration 000237).
Built as §8 says; the choices taken by default (ADR-0053 d), for the
product owner to overrule:
1. *The template.* `close.year_tasks` has `close.tasks`' shape and rules,
   checked by the same validator on its own path and codes (a year task may
   repeat a period task's code, and depends only on year tasks); a name is
   tier 1, the rest tier 2, as `close.tasks`. No check is known on a year
   close yet (`config.YearCloseChecks` is empty), so a year task is manual
   until T64m5 builds the year checks, as the period's were before T64c
   (ADR-0058 As built, T64a); a period check is refused there.
2. *The year's last day* (`year_end`, from which the tasks' due dates
   count, business days of the company as ADR-0058 §3) is the leading
   ledger's last regular period's end. As for a period close, the leading
   ledger is always covered (`request.validation_failed` without it); a
   ledger with no periods of the year is 422 `closing.period_unknown` on
   `fiscal_year`, or on `ledger_ids[i]` when named.
3. *The covered ledgers carry no period* (`year_close_ledger`): a year
   close reads each one's periods of the year live, through the new port
   method `closing.Ledger.YearPeriods` over `ledger.ClosePeriods.OfYear`
   (§2), and shows them per ledger, regular and special, each with its id,
   number, `is_special` and status.
4. *Representations of their own* (`YearClose`, `YearCloseItem`,
   `YearCloseTask`, `YearCloseLedger`, `YearClosePeriod`): `CloseTask`
   requires a `fiscal_period`, so it cannot carry a year close's task
   without a breaking change. The request bodies are the period close's
   (`CloseTaskUpdate`, `CloseTaskComplete`, `ReasonInput`). A year close's
   signed-off fields and sign-offs are T64m6's and a task's
   `check_result` T64m5's, each additive then. Year-close tasks are not in
   `GET /v1/close-tasks`, whose rows carry a period (a gap above).
5. *Shared code* (CLAUDE.md: no copied helper). `closing.closeKind` names
   each kind's tables, records and events; the statements both kinds run
   name their tables through it (`closeKind.q`), so the task actions
   (`taskAction` with the shared `assign`, `complete` and `reopenTask`),
   the reads, the list (`listCloses`), the audit record and event
   (`record`: `closing.year_close.<action>`, `YearCloseChanged` with the
   fiscal year and no period) and the reopening after an unlock are one
   code path. A signed-off year close refuses a task action while every
   period of the year of its covered ledgers is locked, and is reopened by
   the first one after an unlock (ADR-0058 §7); its sign-off is T64m6's,
   and `TestYearCloseAfterUnlock` sets the status as that will. The
   triggers of 000194 and 000196, as 000230 left them, guard the year
   close's tables too, naming the table they guard (`TG_TABLE_NAME`) in
   their messages and constraint names, which for the period close's
   tables are unchanged; an import (`app.importing`, ADR-0029) restores a
   signed-off year close and its done tasks as it does a period close's
   (review round 1: 000237 first replaced them from their 000194/000196
   text, dropping 000230's exemption).
6. *Problem codes.* Each create operation gives the shared `openClose` its
   own checklist and insert (`opening`), so `api` `TestProblemCodes`
   attributes `closing.special_period` and `closing.close_exists` to
   `createPeriodClose` alone and `closing.year_close_exists` (409, with
   `Location`, mapped from `year_close_once` too) to `createYearClose`.
   The statements written through `closeKind.q` are not read by the test's
   written-table rule; none of them can break a registered constraint (a
   close's keys never change, `closing.check_period_close`), so the
   derived codes drop `closing.close_exists` from the period close's task,
   accrual and comment operations, where it could never occur.
7. *Walkthrough* (`year_end_close`, the end of `step_year_end`): Ada adds
   two year tasks (the auditors' adjustments, then the equity review
   depending on it), a tier 2 change the process owner approves; Carla
   opens `PREV`'s year close, covering 0L and 2L with their 16 periods
   each; completing the review first is refused with
   `closing.task_blocked`; she assigns the adjustments task to herself and
   completes it, which unblocks the review; a second year close of `PREV`
   is refused with `closing.year_close_exists`, and the list shows the one.

**T64m5, year-close checks** (no migration). Built as §8 and §11 say; the
choices taken by default (ADR-0053 d), for the product owner to overrule:
1. *The checks a year task may name.* `config.YearCloseChecks` is the
   table's four: `no_parked_entries` and `revaluation_posted` (the period
   checks' names, read over the year), `depreciation_year_posted` and
   `carry_forward_current`. Every other period check, `depreciation_posted`
   included, is refused on a year task, and the two year-only checks on a
   period task (`close.tasks`).
2. *The operation.* `POST /v1/year-closes/{id}/tasks/{code}:check`
   (`checkYearCloseTask`) is `checkCloseTask`'s: the same act (`check`), its
   refusals, If-Match, permissions and recording (`recordCheck` on the
   kind's task table), published as YearCloseChanged `task_checked` with
   `check_passed`. `run` sends a year close's task to `runYear`, which reads
   the covered ledgers' periods of the year live (`yearCoveredOf`, the read
   the representation uses). `YearCloseTask` gains `check_result`
   (`CloseCheckResult`, additive); the sign-off's `FailingCheck` is
   T64m6's.
3. *`no_parked_entries` over the year* reads the parked entries dated from
   the leading ledger's first regular period's first day to its last
   regular period's last day, those parked for a special period included:
   `CloseChecks.Parked` and `closing.Ledger.Parked` take a `special` flag,
   false for a period close (T64m1's rule unchanged), true for a year close.
4. *`revaluation_posted`* gives each covered ledger its last regular
   period, so the key date is that ledger's year end, as the period check
   gives its period's; the summary says "the year's last day".
5. *`depreciation_year_posted`* asks T64k's port once per covered ledger and
   regular period (`ledgerDepreciation`, now shared with
   `depreciation_posted`), in the ledgers' order, then by period; a period
   passes as T64k's check passes it (a posted run, nothing a run would
   select, or a posted run of a later period, its assets then
   `depreciation_catch_up` warnings). An asset late for a period is late
   for every period after it up to the later run's (its depreciation is
   missing from that period through the run's), so the periods one later
   run catches up warn of each asset once, as the last of them reads it,
   not once per period (review round 1). The result
   names only the periods that fail, each a `ledgers` row with
   `fiscal_period` and the assets a run would select, the first 20 with
   `ledger_periods_count` counting them all: every period of two ledgers
   would pass `ledgers`' bound of 20, and a passing period adds nothing to
   act on. The summary names each failing ledger with its periods.
6. *`carry_forward_current`* reads each covered ledger's current run from
   the year through the new port method `closing.Ledger.CarryForward`
   (`ledger.CloseChecks.CarryForward`, under the caller's companies), with
   T64m3's staleness (`openedSinceOf`), so the check and `GET
   /v1/balance-carry-forwards/{id}` never disagree. Each covered ledger is a
   `ledgers` row keyed on the year's last day, `posted` when it has a
   current run, with `carry_forward_run_id`, `stale` and `opened_since`
   (`CarryForwardOpenedPeriod`, as the run's representation names them).
   It fails naming the ledgers without a run and those whose run is stale
   with the periods opened since; a stale run stays stale when the period
   is closed again, until the year is carried forward again.
7. *Walkthrough* (`year_end_checks`, after `year_end_close`): Ada's change
   request of the year tasks adds the four checks (`NO_PARKED`,
   `REVALUED`, `DEPRECIATED`, `CARRIED_FORWARD`, the last after the
   auditors' adjustments); Carla runs them on `PREV`: nothing parked, both
   ledgers revalued on `PREV`'s last day, nothing to depreciate in any
   period, the carry-forward current; she posts one more adjustment into
   period 13 and closes it, the carry-forward check fails naming `PREV/13`
   in 0L and 2L, and passes, naming the new runs, once she carries `PREV`
   forward again.
