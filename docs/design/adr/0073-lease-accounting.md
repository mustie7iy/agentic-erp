# ADR-0073: Lease accounting

**Status:** accepted (T88a, 2026-11-13; decided by default under ADR-0053 d,
with the product owner's decisions of 2026-11-12; `go-reviewer` had no
blocking finding in round 1 on T88a and T88b, 2026-11-13). It decides the leases context and
the slices T88a-T88c (`docs/design/plan-breadth-week3.md`, Lane A). Each
slice records what it built under "As built"; T88a's, T88b's and T88c's
are there now. **The six statements about
IFRS 16 this ADR rests on (§2) are not yet confirmed by the accounting
specialist.**

## Context

The product owner asked for a first usable version of every capability in
scope by 2026-12-01, the main path only (2026-11-10, breadth first). The
tracker's group Finance › Lease accounting (IFRS 16) has two placeholders,
Lease contracts and Right-of-use asset and liability schedules.
`plan-breadth-week3.md` splits them into the slices T88a (the lease
contract with its payment schedule), T88b (the schedule read and the
contract activated) and T88c (the lease run posted), and asks T88a to
write this ADR.

What the design already settles:

- A leaf context reads others through ports and nothing imports it
  (ADR-0051 §2, as `cash`, `assets`, `collections`, `revenue` and `grc`
  are built). Financial effects go through `ledger.Postings` alone.
- ADR-0003 (RLS), ADR-0005 (money in minor units, exact arithmetic,
  `money.Allocate` for shares that add up), ADR-0008 (state guards),
  ADR-0011 (audit), ADR-0053 (slices, headless first). Events name a
  business partner by id and number, never by name (product owner,
  2026-11-05). Documents are numbered from the company's number ranges.
- The plan's defaults 6 and 7 (`plan-breadth-week3.md`, "Defaults
  chosen"): a period's rate is the annual rate divided by the payments a
  year; a payment on the commencement date is not part of the measured
  liability; a lease posts through the ledger with five accounts of the
  account determination, and the right-of-use asset is not an asset
  record.

The product owner decided on 2026-11-12 (the plan's question 10, "as
recommended"): leases are built now on the plan's six statements about
IFRS 16 as planning defaults, each marked not yet confirmed here; the
accounting specialist's answers may overrule them.

## Decision

### 1. A leaf context `leases`

A new bounded context `leases` (schema `leases`, package `internal/leases`)
with its own outbox and inbox, registered in `outbox.Contexts`. It is a
leaf (`internal/layout`): no context imports it. It reads

- tenancy's legal entities (`tenancy.Directory`),
- the partner context's suppliers and names (`partner.Suppliers`,
  `partner.Payees`),
- the ledger's cost centers and calendar (`ledger.AssignmentBalances`,
  `ledger.Calendar`) and, from T88b, its account determination and
  postings,
- the configuration's number ranges and company dates
  (`configuration.Numbers`, `configuration.Settings`)

through ports it declares, which the composition root wires
(`app.Leases`). It imports those contexts' published types
(`tenancy.Entity`, `partner.Supplier`, `partner.Named`,
`ledger.CostCenterRef`, `config.Config`). It posts, from T88b, through
`ledger.Postings` with `source_context` `leases`.

### 2. What the standard is taken to say, and who judges

The primary source is IFRS 16 *Leases* (IASB). **Every statement below is
from memory**: no lane has network access and the repository holds no
copy of the standard, so none was checked against its text. The paragraph
numbers are from memory too. The accounting specialist was asked to
confirm each (`plan-breadth-week3.md`, question 1 for the accounting
specialist).

| # | What the standard is taken to say | Paragraph (from memory) | Status |
|---|---|---|---|
| 1 | At the commencement date a lessee recognizes a right-of-use asset and a lease liability. | 22 | not yet confirmed by the accounting specialist |
| 2 | The lease liability is measured at the present value of the lease payments not paid at that date, discounted at the interest rate implicit in the lease if it can be readily determined, otherwise at the lessee's incremental borrowing rate. | 26 | not yet confirmed by the accounting specialist |
| 3 | The right-of-use asset is measured at cost: the initial amount of the lease liability, plus payments made at or before commencement less incentives received, initial direct costs and an estimate of restoration costs. | 23, 24 | not yet confirmed by the accounting specialist |
| 4 | After commencement the liability is increased by interest on it and reduced by the payments made. | 36 | not yet confirmed by the accounting specialist |
| 5 | The right-of-use asset is carried at cost less accumulated depreciation and impairment, and depreciated from commencement to the earlier of the end of its useful life and the end of the lease term, unless ownership transfers. | 29, 30, 31, 32 | not yet confirmed by the accounting specialist |
| 6 | A lessee may elect not to apply this to short-term leases and leases of low-value assets, expensing their payments on a straight line. | 5, 6 | not yet confirmed by the accounting specialist |

Nothing else about IFRS 16 is assumed. A slice that finds it needs a rule
these six statements and the plan do not give stops that part and reports
it (BRIEF-RULES rule 8); it does not add one.

**The person makes every judgement; the system only does the
arithmetic.**

- The **person** decides that a contract is a lease, its term and its
  discount rate (the rate implicit in the lease or the incremental
  borrowing rate of statement 2), and enters the payments (T88a). The
  system judges none of it and applies no exemption of statement 6: a
  contract entered is capitalized when it is activated.
- The **system** does the arithmetic: the present value of statement 2
  (T88b); the right-of-use asset at the liability's initial amount plus
  a payment made on the commencement date, the other parts of statement
  3 (incentives, initial direct costs, restoration) being left to later;
  the interest of statement 4 and the straight-line depreciation over
  the lease term of statement 5 (T88b's schedule, T88c's run).
- The **postings** are the plan's reading of those statements, not a
  quotation: at activation the right-of-use asset against the lease
  liability (T88b); in each run interest expense against the liability
  and depreciation expense against accumulated depreciation (T88c); the
  lessor's invoice, posted by a person, against the liability.

### 3. The lease contract (T88a)

A lease contract (`leases.contract`) is entered by a person for a company
as **lessee**: the lessor, the commencement date, the term in whole
months, the payment amount, how often it is due (monthly, quarterly or
yearly), whether in advance or in arrears, the annual discount rate and
the cost center its expenses will be posted on.

- **Company**: one the caller may act in.
- **Lessor**: a business partner that is active, a supplier and set up
  and not blocked for posting in the company, the check procurement and
  payables make of a supplier (`partner.Supplier.Usable`): the lessor's
  invoices are supplier invoices.
- **Term**: 1 to 600 months (fifty years). The frequency divides it into
  whole intervals (`contract_frequency_divides_term`).
- **Payment**: more than zero, in the company's **local currency**;
  leases in a foreign currency are left for later, as the plan says.
  Every payment of the first version is this amount.
- **Discount rate**: a percentage above zero and at most 100 with up to
  six decimals, sent and kept as a **decimal string**, never a float
  (`numeric(9, 6)`, read back without trailing zeros).
- **Cost center**: an active cost center of the company in force on the
  commencement date.

The contract is numbered from the company's `lease_contract` number range
of the fiscal year of the company's date today (in its time zone, as
every document is dated), unique per company. It is created `draft` and
posts nothing. The first version does not change a contract.

### 4. The payment schedule (T88a)

The payments are written with the contract (`leases.payment`), one per
interval of the term, numbered from 1 in date order. The **n-th interval
starts n-1 intervals after the commencement date**, on its day of the
month, or the month's last day when the month has fewer days
(`clock.AddMonths`), each counted from the commencement date and never
from the interval before, so a lease from the 31st does not drift to the
28th; an interval ends the day before the next starts. A payment **in
advance** is due on its interval's first day, the first on the
commencement date; one **in arrears** on its last day. The term's last
day is the day before the date the term's months after the commencement
date. One payment a day per contract (`payment_date_once`).

### 5. Events and audit

Every change of a contract writes an audit record (ADR-0011) and publishes
`LeaseContractChanged` in the same transaction (ADR-0007): the contract
by id and number, its company by id and code, its lessor by id and
number, never by name, its commencement date, term, payment amount,
frequency, timing and number of payments, its status and the status
before. It is not offered to webhooks until a consumer asks (the plan's
rule for week 3's events).

### 6. Permissions and agents

`leases.contract.read` and `leases.contract.write` (T88a),
`leases.contract.activate` (T88b) and `leases.run.post` (T88c). Reads
follow the caller's company access; a contract of another company is not
found. Every write is human-only (`x-agent: false`): it records a
person's judgement (§2) or posts it.

### 7. Activation and the lease accounts (T88b)

Activating a `draft` contract (`:activate`, with the caller's version)
moves it to `active` once, under its row lock, and posts **one entry on
the commencement date** through `ledger.Postings` (document type `SA`,
`source_context` `leases`, the contract as its source document): the
right-of-use amount debited to the company's `right_of_use_asset`
account and credited to its `lease_liability` account (the plan's
default 7). The right-of-use amount is the lease liability at
commencement and a payment due on the commencement date (statement 3, as
§2 reads it): the liability account holds that payment until a person
posts the lessor's invoice for it against the liability that day.

The two accounts are new keys of a company's account determination
(`companies[].account_determination.right_of_use_asset`, an asset, and
`.lease_liability`, a liability; neither a reconciliation account),
optional in the configuration and among the accounts the system posts to
(`config.Company.AutomaticAccounts`); a company without both activates
no lease (422 `leases.no_lease_accounts`). The commencement date must be
in an open period of a set-up fiscal year (the ledger's own refusals). An
active contract names the liability, the right-of-use amount and the
entry, and is fixed (`lease_contract_fixed_once_active`). The
right-of-use asset is **not an asset of asset accounting** (the plan's
boundary; the accounting specialist's question 3): it lives on its own
account and schedule.

### 8. The schedule (T88b)

One line per payment interval, numbered as its payment, from the
contract's stored payments (§4) and its rate (default 6 and the plan's
acceptance, statements 2, 4 and 5):

- The **period's rate** r is the annual rate divided by the payments a
  year (12, 4 or 1). Whether it should be the effective rate instead is
  the accounting specialist's question 2.
- From the last interval back, the **exact present value** at each
  interval's end of the payments still to come is a rational
  (`math/big.Rat`): (the next one + its payment) / (1 + r) in arrears,
  its payment + the next one / (1 + r) in advance, a payment due at an
  interval's start being still to come at the end of the interval
  before. Each is **rounded once**, half away from zero to the minor unit
  (`money.Round`, as `money.Scale` rounds): these are the closing
  liabilities, the last zero by construction.
- The present value at commencement, rounded, is the first **opening
  liability**. In advance it holds the payment due on the commencement
  date, which is **not part of the lease liability** (statement 2) and
  bears no interest; the lease liability is the opening liability less
  that payment. The **right-of-use asset** is the opening liability.
- A line's **interest** is its closing liability less its opening plus
  its payment: the period's rate × the balance outstanding during it
  (the opening balance in arrears, the opening balance less the payment
  in advance), to within the rounding of the two balances, which is never
  carried forward, so the liability ends at exactly zero with no
  remainder absorbed by a last line.
- The **depreciation** is the right-of-use asset on a straight line over
  the intervals, shared out with `money.Allocate` so the shares add up to
  it exactly (a minor unit left by the rounding going to the earliest
  lines); each line shows the asset's carrying amount at its end.

A draft's schedule is **computed on read**; activation stores it
(`leases.schedule_line`) and an active contract's is the stored one, so
what T88c's runs post is what was shown. The totals are the payments,
the interest (the payments less the right-of-use asset) and the
depreciation (the right-of-use asset).

### 9. The lease run (T88c)

Per company and regular fiscal period (`POST /v1/lease-runs`), each
active contract posts its stored lines whose interval ends on or before
the period's last day and that no earlier run posted: in the usual course
the one line whose interval ends in the period, none in a period that a
quarterly or yearly interval does not end in. One entry per contract,
dated the period's last day (document type `SA`, `source_context`
`leases`, the contract's number as its reference): the lines' interest
debited to `lease_interest` and credited to `lease_liability`, their
depreciation debited to `right_of_use_depreciation` and credited to
`right_of_use_accumulated_depreciation`, the expenses on the contract's
cost center. The run never posts the lease payment: a person posts the
lessor's invoice against the lease liability (the plan's default 7).

One run per company and period, each for a later period than the
company's last, under the company's advisory lock, as a revenue
recognition run (ADR-0068 §9); a line is posted once; a run is not
reversed. A run reads at most 2,000 active contracts, as a recognition
run does. The three accounts are new keys of the account determination:
two expenses and an asset (contra) that reconcile nothing; a company
without them, or without the lease liability, runs no lease accounting
(422 `leases.no_lease_accounts`).

## Consequences

- A lease is a document of its own beside the ledger, as a revenue
  contract is; the ledger sees its postings only (T88b, T88c).
- A contract is capitalized whatever its term or value (statement 6 is
  not applied), until the accounting specialist answers question 5.
- What the plan leaves for after 1 December stays out: changing a
  contract, modifications and reassessments, options, variable and
  index-linked payments, initial direct costs, incentives and
  restoration costs, the exemptions, non-lease components, leases in a
  foreign currency, lessor accounting and taking contracts over from SAP
  RE-FX.

## Alternatives

- **Payments derived on read instead of stored.** The plan writes them at
  creation, so a later change of how dates are counted cannot move the
  dates of a contract already entered.
- **The rate as a binary float or a JSON number.** Refused by ADR-0005's
  rule for amounts, which the plan extends to the rate: a decimal string
  and an exact `numeric`.
- **A contract numbered by the commencement date's fiscal year.** A
  commencement date can lie in a year that is not set up yet; the date
  today always is, as for every other document.

## As built

### T88a (2026-11-13)

- Migration 000326: schema `leases` with its outbox and inbox,
  `leases.contract` and `leases.payment` with RLS, `tenant_id` first, the
  rules of §3 and §4 as checks (`contract_term_months_check`,
  `contract_frequency_divides_term`, `contract_annual_rate_check`,
  `contract_number_once`, `payment_date_once`, …), the contract not
  updated or deleted by the application and the payments insert-only; the
  permissions `leases.contract.read` and `.write`.
- `POST /v1/lease-contracts` (human-only, idempotent by its key),
  `GET /v1/lease-contracts` (company, lessor, status; cursor paging by
  number; without the payments, with their number and total) and
  `GET /v1/lease-contracts/{id}` with the payments. Every response names
  the company by code and name, the lessor by number and name and the cost
  center by code, and gives the payments' number and total.
- Refusals, each 422 naming the request's field: `leases.unknown_frequency`,
  `leases.unknown_timing`, `leases.frequency_not_dividing_term`,
  `leases.invalid_discount_rate`, `leases.foreign_currency`,
  `leases.payment_not_positive`, `leases.amount_too_large` (the payments'
  sum beyond an amount's range), `leases.lessor_not_usable`,
  `leases.cost_center_invalid`; `request.reference_not_found` for a
  company the caller cannot act in; `config.no_number_range` without a
  `lease_contract` range.
- `addMonths` moved from the cash context to `clock.AddMonths`, which both
  use (CLAUDE.md: a helper is moved, never copied).
- Walkthrough step 96, `step_lease`: a three-year lease of office space,
  5,000.00 CAD a month in advance at 6 percent, read back with its 36
  payments; a foreign currency and a term the frequency does not divide
  refused; nothing posted. The pilot's settings add the company's
  `lease_contract` range, which no country pack ships (since batch 83
  Canada 2026.11 and UAE 2026.8 bring it; ADR-0048 As built).

### T88b (2026-11-13)

- Migrations 000327 and 000328: `leases.contract` gains `active` and what
  activation measured and posted (`lease_liability`, `right_of_use_amount`,
  the entry's id, year and number; all present exactly when active,
  `contract_activation_check`; the asset not below the liability,
  `contract_right_of_use_covers_liability`), fixed once active
  (`lease_contract_fixed_once_active`, a trigger, so the table's owner is
  held too); `leases.schedule_line`, insert-only, its liability moving by
  its interest and payment alone (`schedule_line_balance_check`); the
  account determination keys `right_of_use_asset` and `lease_liability`
  and the journal's source context `leases`, both checks widened NOT VALID
  and validated by 000328, the journal's partition by partition; the
  permission `leases.contract.activate`. The down refuses while an active
  contract exists.
- `GET /v1/lease-contracts/{id}/schedule` (computed for a draft, stored
  for an active contract; the company by code and name, every amount with
  its currency, the totals) and `POST /v1/lease-contracts/{id}:activate`
  (human-only, `If-Match`, idempotent by its key; 409
  `leases.invalid_status`, 422 `leases.no_lease_accounts` and the
  ledger's refusals). The contract and LeaseContractChanged carry the
  liability, the right-of-use amount and the entry once active.
- Decided by default: the entry is dated the **commencement date**,
  whatever the date of the request (statement 1), and an activation
  before that date is not refused: it posts into the commencement
  date's period if that period is open. The contract's row is locked FOR
  NO KEY UPDATE: its schedule lines' foreign key takes FOR KEY SHARE,
  which does not wait for it, and no other writer locks a contract, so
  two writers cannot meet in opposite order; of two activations at once,
  one posts and the other meets the version it moved (412).
- Three intervals of 100.00 a year at 10 percent are worked by hand in
  `TestScheduleByHand`; thirty years of monthly payments in
  `TestScheduleOf360Periods` (the liability in arrears 166,791.61, the
  annuity's present value); `TestScheduleBudget` reads the schedule of a
  contract of 600 payments, computed and stored, within 3 s at p95.
- Walkthrough step 96 extended: the schedule read (the liability below
  the payments' sum, the first interval's interest half a percent of the
  liability alone, the last closing liability zero), the contract
  activated and its entry's two lines read back, a second activation
  refused. The pilot's settings add the accounts 1660 and 2650 and name
  them in the account determination.

### T88c (2026-11-15)

- Migrations 000332 and 000333: `leases.run`, once per company and period
  (`lease_run_once`), each for a later posting date than the company's
  last (`lease_run_in_order`, a trigger under the company's advisory
  lock), insert-only; `leases.run_posting`, per run and contract the
  stored lines it posted (`first_line_no` to `last_line_no`, both
  foreign keys to the contract's schedule lines), each line posted once
  (`lease_line_posted_once`, a trigger), the interest and depreciation and
  the entry, insert-only; the account determination keys
  `lease_interest`, `right_of_use_depreciation` and
  `right_of_use_accumulated_depreciation`, widened NOT VALID and
  validated by 000333; the index `contract_by_company`; the permission
  `leases.run.post`. The down refuses while a run exists. Schema tests for
  each rule, the two triggers at owner level and the guard.
- `POST /v1/lease-runs` (human-only, idempotent by its key: a replay
  answers the run), `GET /v1/lease-runs` (company, fiscal year and period;
  cursor paging, latest first; without the postings) and
  `GET /v1/lease-runs/{id}`. The run names its company by code and name and
  gives its counts, the interest and depreciation it posted (its totals)
  and per posting the contract, its lessor by number and name, its cost
  center by code, the lines and the entry. Refusals: 409
  `leases.run_exists`, 409 `leases.run_period_not_later`, 422
  `leases.no_lease_accounts`, 422 `leases.run_too_large`, 422
  `leases.amount_too_large`, 422 `request.reference_not_found`, the
  ledger's 422 `ledger.no_fiscal_period` and the new 422
  `ledger.period_not_regular` (`ledger.Calendar.RegularPeriod`, which the
  run reads its period through), 409 `ledger.period_not_open` and what
  the ledger refuses of an entry. Audited, and published as
  `LeaseRunPosted` (not offered to webhooks).
- Config: the three keys (two expenses and an asset that reconcile
  nothing), among the automatic accounts; `ledger.Setup` applies them.
  `leaseAccounts` reads any of the context's keys, the activation's two
  and the run's four, and names the missing ones.
- Decided by default: a run posts the lines whose interval ends **on or
  before** the period's last day and that no run posted yet, not only
  those ending in the period: a contract activated after the period of
  its first lines was run, or a period a company skipped, is caught up by
  the next run instead of never being posted (established ERP practice,
  as SAP RE-FX's periodic posting posts every due item not yet posted, and
  as this repository's revenue recognition run catches up; the entry is
  dated the run's period). In the usual course this is the plan's "a
  contract whose schedule has no line in the period posts nothing". The
  entry is dated the period's last day, not the interval's end. Both
  expense lines carry the contract's cost center; the ledger refuses a
  cost center that is no longer valid on that day.
- The active contracts are counted and their lines due read in one
  statement, so the count and the postings are of one state at read
  committed (review round 1).
- Locks: the run holds the company's advisory lock (`leases.run:` tenant
  and company), reads active contracts and their stored lines unlocked
  (both fixed once active), and its postings take FOR KEY SHARE on the
  contract, its two lines and the run through their foreign keys. An
  activation locks only a draft (FOR NO KEY UPDATE), which no run reads
  or references, and takes no advisory lock, so a run and an activation
  cannot wait on each other in opposite order; two runs of one company
  meet at the advisory lock (`TestRunTwiceAtOnce`).
- Walkthrough step 96 extended (`lease_run`): the run of the commencement
  period posts the contract's first line, interest to 6720 against 2650
  and depreciation to 6730 against 1669 on CC-1100, read back and listed;
  a second run of the period refused. The pilot's settings add the three
  accounts.
