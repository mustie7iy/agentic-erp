# ADR-0065: Dunning and collections

**Status:** accepted (T76a, 2026-11-12; decided by default under ADR-0053 d;
`go-reviewer` had no blocking finding on T76a's last commit). It decides
the collections context and the slices T76a-T76c
(`docs/design/plan-breadth-week1.md`, Lane B); the dispute cases of
T86a-T86b (`plan-breadth-week3.md`) record what they build under it. Each slice records what it
built under "As built"; all three are there now. The defaults taken and the
questions left are listed at the end for the product owner's digest.

## Context

The product owner asked for a first usable version of every capability in
scope by 2026-12-01, the main path only (2026-11-10, breadth first). The
tracker's group Finance › Collections has two placeholders, Dunning and
Collection worklists. `plan-breadth-week1.md` splits them into the slices
T76a (a dunning run proposed), T76b (confirmed) and T76c (the collection
worklist) and asks T76a to write this ADR.

What the design already settles:

- The customer's open items are the ledger's: lines on a customer
  reconciliation account, each naming its customer, with the payment terms
  they were posted with (ADR-0063 §5, T75b). `ledger.OpenItems.Receivable`
  reads them for a company, "for the aging, dunning, the collection
  worklist and the credit exposure" (ADR-0063 As built), so an item posted
  by a manual or migrated entry counts as an invoice's does.
- A leaf context reads others through ports and nothing imports it
  (ADR-0051 §2, as `cash`, `assets`, `closing`, `consolidation` and
  `einvoicing` are built).
- Settings that are the tenant's own are configuration, validated and
  changed through governed change requests by tier (03 §3.5 and the tiers
  of `internal/config`).
- ADR-0003 (RLS), ADR-0005 (money in minor units), ADR-0007 (outbox),
  ADR-0008 (state guards), ADR-0011 (audit), ADR-0053 (slices, headless
  first). Events name a business partner by id and number, never by name
  (product owner, 2026-11-05).

Nothing settles what dunning is here: its levels, which items a run
takes, what a run changes and when. This ADR decides them at the level
the three slices need.

## Decision

### 1. A leaf context `collections`

A new bounded context `collections` (schema `collections`, package
`internal/collections`) with its own outbox and inbox, registered in
`outbox.Contexts`. It is a leaf (`internal/layout`): no context imports
it. It posts nothing and changes nothing outside its schema. It reads

- the ledger's customer open items (`ledger.OpenItems.Receivable`),
- the partner context's numbers and names (`partner.Payees.Names`),
- tenancy's legal entities and
- the active configuration

through ports it declares, which the composition root wires
(`app.Collections`). Unlike the other leaves it imports those contexts'
published types (`ledger.ReceivableItem`, `partner.Named`,
`tenancy.Entity`, `config.Config`) rather than copying them: the open
item is the ledger's, and a copy of its type would be a second definition
of the amounts' sign and of when an item is due. The dependency points
one way (ledger, partner, tenancy, configuration → collections); the
layout test lists the four imports and keeps `collections` among the
leaves. No foreign key leaves the schema (02 §2.1).

### 2. Dunning levels are configuration

`companies[].dunning.levels` is a company's list of dunning levels, each
a `level` and the `days_overdue` an item must have reached for it:

```yaml
companies:
  - code: "3000"
    dunning:
      levels:
        - {level: 1, days_overdue: 1}
        - {level: 2, days_overdue: 15}
        - {level: 3, days_overdue: 30}
```

Levels are numbered from 1 without a gap, at most nine, each at least a
day and at most ten years overdue and more days than the level before
(`config.validation_failed` otherwise, the level's own field named). They
post nothing, so a change is tier 2, operational, as payment terms' days
are. No country pack ships levels (plan default 6): a company without
any has no dunning, and its run is refused, 422
`collections.no_dunning_levels`. There are no charges, no interest, no
texts, no grace days and no minimum amounts in the first version.

### 3. A dunning run and its notices

A credit controller starts a run for one company at a key date
(`POST /v1/dunning-runs`). The run is one synchronous request and one
transaction. It stores

- `collections.dunning_run`: the company, the key date, the status and
  the totals (notices, items, their overdue amount in the company's
  currency);
- `collections.dunning_notice`: one per customer with items to dun, with
  the customer's number when the run proposed, its level (the highest
  among its items), its items' count and their amount in the company's
  currency;
- `collections.dunning_notice_item`: each item, named by its journal line
  (fiscal year, journal entry, line: the ledger's open item), with what
  the run read of it (document number, reference, document date, net due
  date, days overdue, amount in its own currency and in the company's as
  booked) and the level it was last dunned at and the one it would reach.

A run is created `proposed`. Proposing changes nothing on the items and
sends nothing: the notices are a proposal, read with
`GET /v1/dunning-runs/{id}/notices`. What a run proposed is fixed
(notices and items are insert-only); confirming it (§8) changes the
run's status alone. A key date at which nothing is to dun gives a run
without notices, so the controller sees that it ran. Several proposals
of one company may exist side by side; §8 says which of them can still
be confirmed. The same request sent again with its `Idempotency-Key`
answers the run it started (04 §4.7). The key date is required: a run is
reproducible from its request, and a date is never taken from the clock.

### 4. Which items, and at which level

Of the company's open customer items, a run proposes those that

- the customer owes: a debit, an amount above zero. Credit items (a
  credit memo, a payment on account) are not dunned and reduce nothing
  (plan default 6): a notice's amount is the sum of its debit items;
- are overdue on the key date by at least the days of the first level:
  days are counted from the net due date to the key date, and an item due
  on the key date is not overdue (`ledger.ReceivableItem.DaysOverdue`).
  The net due date is that of the payment terms the item was posted
  with; an item posted without payment terms (a manual or migrated
  entry's) is due on its document date (product owner 2026-11-12; As
  built, Items without payment terms). Until then such an item had no
  due date and was never dunned.

The level an item would reach is the last level whose days it has
reached, but one above the level it was last dunned at, at most, and
never below it: an item rises one level per confirmed run and the
highest level stays (plan default 6). An item never dunned is at level
0, so it reaches level 1 however long it is overdue. Amounts in the
company's currency are the items' as booked; nothing is revalued.

A run reads the items open when it is started, not those that were open
on the key date: the key date only counts the days overdue. A run at a
past key date therefore leaves out an item cleared since, and takes one
posted since with an earlier due date. That is what a controller dunning
today as of a date wants: nobody is reminded of an item already paid.

### 5. The size of a run

A run reads every open customer item of the company and writes its
proposal in the request's transaction, so its size is bounded: more than
5,000 overdue items are refused, 422 `collections.run_too_large`, the
limit named (`collections.MaxRunItems`). Runs as a workflow, and
scheduled runs, come later (the plan's "Later for Dunning").

### 6. Events and audit

Each change of a run's status writes an audit record and publishes
`DunningRunChanged` through the context's outbox in the same transaction
(ADR-0007, ADR-0011): the run's company (id and code), key date, totals,
status and status before. It carries no notice and so names no customer.
T76b adds `DunningNoticeIssued`, one per customer of a confirmed run,
naming the customer by id and number, never by name.

### 7. Permissions and agents

`collections.dunning.read` reads runs and notices;
`collections.dunning.run` starts a run and, with T76b, confirms it;
T76c adds `collections.worklist.read`. Every read and write is held to
the companies the caller may act in: another company's run is not found
(404), and naming it in a request is answered as a missing reference
(422). Starting a run is people's (`x-agent: false`), as entering and
posting receivables documents is (ADR-0063 §8); the reads are agent
tools. The collections agent is a later leaf.

### 8. Confirming a run (T76b)

`POST /v1/dunning-runs/{id}:confirm` moves a run from `proposed` to
`confirmed`, guarded by its status (`WHERE status = 'proposed'`,
ADR-0008), and records each item's level and dunning date (the run's key
date) in `collections.dunned_item`, insert-only: item, level, run, date.
From then on an item's previous level is the highest level recorded for
it, which the next run reads, so the next run proposes the next level.
Before confirming, the proposal is compared with the ledger's open items
again: a proposal with an item cleared since, or with an item another
confirmed run of the company has dunned since (its recorded level is no
longer the proposal's previous level, or it was recorded again at that
level: As built, T76b), is stale and refused, 409, and a new run is
proposed. Confirming twice records and publishes once.

### 9. The collection worklist (T76c)

`GET /v1/collection-worklist` is computed on read from
`ledger.OpenItems.Receivable` and `collections.dunned_item` and stores
nothing: the customers with overdue items on a key date, the largest
overdue amount first (plan default 7), each with its open and overdue
totals, oldest due date, highest dunning level and last dunning date. It
is a report read and gets a budget test.

## Alternatives

- **Dunning inside `receivables`.** Rejected: dunning reads every
  customer item of the ledger, receivables' documents or not, and
  receivables posts; a leaf that only reads keeps the two apart and lets
  credit management (T77a) follow the same shape.
- **Levels kept on the ledger's open items** (SAP keeps the dunning level
  on the item). Rejected: the ledger's lines are immutable, and a level is
  collections' fact about an item, not the ledger's.
- **Levels by days overdue alone**, without the one-level step. Rejected
  by plan default 6: a customer never reminded would be sent the last
  level's notice first.
- **A proposal computed on read**, stored only when confirmed. Rejected:
  the controller confirms what they read, so the proposal has to be the
  same rows at both moments, and T76b can tell that it went stale.
- **A copy of the open item's type behind the port.** Rejected in §1.

## Consequences

- A company gets dunning by configuring levels; nothing is posted and no
  item changes, so a proposal is safe to start at any time.
- The levels' days mean what they say from the second run on; the first
  run of an old item is level 1 whatever its age.
- One request bounds a run at 5,000 items; a company with more waits for
  runs as a workflow.
- T76b and T76c add a table and their operations without changing
  T76a's.

## Defaults chosen (for the product owner's digest)

1. Dunning levels are the tenant's configuration, a list of days
   overdue; none is shipped; a change is tier 2 (plan default 6).
2. A run's key date is required and may be any date up to 2100-12-31
   (the limit is T76b's, As built).
3. An item is proposed only once it is overdue by the first level's
   days; an item without payment terms is due on its document date
   (product owner 2026-11-12, overruling the default that it is never
   dunned).
4. A run with nothing to dun is stored, without notices.
5. One run proposes at most 5,000 items; more is refused.
6. Starting a run is people's; agents read runs and notices.
7. `DunningRunChanged` carries the run's totals and no customer.
8. A notice's amount is in the company's currency as booked; each item
   also shows its own currency's amount.

## Questions

1. **Dunning charges, interest and minimum intervals** (product owner;
   plan question 2): not built. Are any wanted before the pilot, and by
   what rule?
2. **Items without payment terms** (product owner): a customer item
   posted by a manual or migrated entry without terms has no due date and
   is never dunned. Should such an item be due on its document date
   instead? Answered 2026-11-12: yes (As built, Items without payment
   terms).

## As built

### T76a Dunning run proposed (migration 000273)

- **Context.** `internal/collections` with the ports `LegalEntities`,
  `OpenItems`, `Partners` and `Settings`, wired by `app.Collections`;
  `collections` in `outbox.Contexts`; `internal/layout` allows its four
  imports and lists it among the leaves. Nothing in `internal/ledger`
  changed: the run reads `ledger.OpenItems.Receivable` as T75b built it.
- **Configuration.** `config.Dunning` and `config.DunningLevel`
  (`internal/config/dunning.go`), validated as §2 says; tier 2 by the
  rule `companies[*].dunning`; levels are compared by `level` in a
  change's diff. No pack ships levels.
- **Schema.** `collections.dunning_run` (update revoked from the
  application: nothing changes a proposal yet), `dunning_notice` and
  `dunning_notice_item` (insert-only), the two permissions; the checks
  are in 03 §3.27 and `db/collections_test.go`. `previous_level` is
  written 0 by every run of T76a, which records no levels; the check
  `dunning_notice_item_one_level` already holds a level to its previous
  level or one above, so T76b writes the recorded level there without a
  schema change to the proposal.
- **Operations.** `POST /v1/dunning-runs` (201 with `Location`; 422
  `collections.no_dunning_levels`, `collections.run_too_large`,
  `collections.amount_too_large`, `request.reference_not_found` naming
  `legal_entity_id`), `GET /v1/dunning-runs` (by key date, the latest
  first; filters company, status, key date from and to),
  `GET /v1/dunning-runs/{id}`, `GET /v1/dunning-runs/{id}/notices` (by
  customer number; filters customer and level; each notice with its
  items). Every amount carries its currency; a run names its company by
  code and name, a notice its customer by number and name, an item its
  journal entry by document number and reference.
- **Level.** `collections.reach`: the last level reached by days, one
  above the previous level at most and never below it; T76a passes the
  previous level 0.
- **Event and audit.** `DunningRunChanged` (`proposed`, status before
  null), audit action `collections.dunning_run.propose`.
- **No budget test.** The notices read is a keyset-paged list of stored
  rows over an index (`dunning_notice_by_customer_number`), each page's
  items in one query, not a computed report (07 §7.7 item 7); starting a
  run is bounded by §5. T76c's worklist, computed on read, gets the
  budget.
- **Walkthrough.** `step_dunning` (step 57): Ruth posts an invoice to
  Northwind on NT30; a run before Maple has dunning levels is refused;
  Ada gives Maple its levels through a tier 2 change; a run at a key date
  40 days after the invoice date, ten days past its due date, proposes
  one notice for Northwind at level 1, the invoice's item among its
  items; a run at the due date proposes none; the customer's open items are unchanged.
  The step duns an invoice of its own, so it holds whether or not the
  invoice of step 48 is still open when it runs.

### T76b Dunning run confirmed (migration 000277)

- **Schema.** `collections.dunned_item` (insert-only: the item's journal
  line, the confirmed run, the level, the dunning date; keyed by item
  and run, each row one of the items that run proposed); the run's
  status widened to `confirmed`; the application may update a run's
  status, version and `updated_by` alone, and the trigger
  `dunning_run_confirmed_once` lets a proposed run move to confirmed and
  nothing else. No new permission: `collections.dunning.run` confirms.
- **Operation.** `POST /v1/dunning-runs/{id}:confirm` with `If-Match`
  and `Idempotency-Key`, people's (`x-agent: false`), answers the
  confirmed run. In one transaction (`collections.ConfirmRun`): the
  company's advisory lock; the run updated `WHERE status = 'proposed'`;
  the proposal compared with the open items and the recorded levels;
  the levels recorded; the audit record
  `collections.dunning_run.confirm`; `DunningRunChanged` (`confirmed`,
  status before `proposed`) and one `DunningNoticeIssued` per notice, in
  the order of customer number.
- **Once.** A run that is not proposed is refused, 409
  `collections.invalid_status` (the guarded update finds no row), so a
  second confirmation records and publishes nothing; a retry with the
  same `Idempotency-Key` answers the first's result. Decided by default,
  as posting a customer invoice twice is answered (ADR-0063 §5).
- **Stale.** 409 `collections.proposal_stale`, the detail counting the
  items cleared and the items dunned by another run. "Dunned by another
  run" is judged by how often the item was dunned, not by its level
  (review round 1): a run stores with each item the number of its
  `dunned_item` rows when it proposed it
  (`dunning_notice_item.previous_dunnings`, not shown in the API), and a
  confirmation compares it with the number now. §8's wording, "whose
  recorded level is no longer the proposal's previous level", would
  miss an item at the highest level or short of the next level's days,
  which a confirmation records again at the level it has; two
  proposals of it would both be confirmed and the customer dunned
  twice for one key date. The transaction is
  rolled back, so the stale run stays `proposed` and nothing of it is
  recorded; the controller starts a new run. The system does not start
  one itself: a refusal leaves nothing behind (decided by default).
  Stale runs stay listed as `proposed`; cancelling a run is among the
  plan's "Later".
- **Serialized per company.** Two proposals of one company confirmed at
  once wait for each other on a transaction-scoped advisory lock (as
  payments' proposals of a company do), so the second reads what the
  first recorded and is stale where they share an item, whether the
  first raised the item's level or left it. Without the lock both would
  pass the comparison and an item would be dunned twice.
- **A run without notices** is confirmed too and records nothing
  (decided by default: the controller closes what she started).
- **Levels read.** `collections.recorded` gives, per item, the highest
  level, the latest dunning date (the worklist's, T76c) and the number
  of rows of `dunned_item`. A run reads it for
  the items overdue on its key date and writes it as `previous_level`;
  `reach` then gives the next level. An item at the highest level is
  proposed and recorded again at that level with the later date, so the
  last dunning date moves while the level stays. An item dunned before is
  proposed at its level at an earlier key date too, as long as it is
  overdue then: a level never falls; one not overdue on the key date is
  never proposed.
- **Event.** `DunningNoticeIssued` (`x-webhook: true`): the notice, its
  run, the company (id and code), the customer by id and number, never
  by name (product owner, 2026-11-05), the dunning date, the notice's
  level, its items' count and amount in the company's currency. Up to
  5,000 events are written one by one in the request (§5 bounds a run).
- **Key date limit** (src `review:T76a`).
  `ledger.ReceivableItem.DaysOverdue` counts through a `time.Duration`,
  which holds about 292 years, so a key date further than that past a
  due date would count wrongly. Collections refuses a key date after
  2100-12-31 (`collections.LastKeyDate`), 422
  `collections.invalid_key_date` naming `key_date`; the count is then
  right for every item due since 1809. `internal/ledger` is unchanged.
  This replaces default 2 above: a key date may be any date up to that
  one. No lower limit: an early key date only makes fewer items overdue.
- **Walkthrough.** `step_dunning` extended: Ruth confirms the run
  (`confirmed`, version 2); confirming it again is refused; a colleague's
  proposal of the same key date is refused as stale and stays
  `proposed`; a run twenty days later, the invoice 30 days overdue,
  proposes Northwind's notice at level 2 with the invoice's item from
  level 1 to 2.

### T76c Collection worklist (migration 000278)

- **Operation.** `GET /v1/collection-worklist?legal_entity_id=&key_date=`
  (both required; `x-slo-class: report`, `x-snapshot: true`; an agent
  tool), permission `collections.worklist.read`, which the migration
  seeds; nothing else is stored. A company the caller may not act in is
  404, as the cash position answers it.
- **Rows.** `collections.WorklistOf` reads every open customer item of
  the company (`ledger.OpenItems.Receivable`) and groups them by
  customer. A customer is listed when one of its items is overdue on the
  key date as §4 counts it (a debit past its net due date by a day at
  least, the document date for an item without payment terms; the first dunning level's days do not matter,
  and a company without dunning levels has a worklist too). Each row:
  the customer's id, number and name; `open_amount`, all its open items
  with credits; `overdue_amount`, its overdue items, which credits do
  not reduce (as a notice's amount); `oldest_due_date` among the overdue
  items; `dunning_level` and `last_dunning_date`, the highest level and
  latest date `dunned_item` holds for its open items (0 and absent when
  none was dunned). Amounts are in the company's currency as booked.
  Decided by default: the level and date are those of items still open,
  as they are recorded now, whatever the key date; a cleared item's
  dunning no longer counts.
- **Totals.** `customer_count`, `open_amount` and `overdue_amount` of
  every customer listed, on every page (REVIEW.md, a report without its
  totals).
- **Order and paging.** The largest overdue amount first (plan default
  7), customers with the same amount by id, so the order is total. The
  cursor is the last row's overdue amount and customer id, signed and
  bound to the company and key date; the worklist is computed again for
  each page and the page continues after that place. While nothing
  changes, nobody is listed twice or left out; a customer whose overdue
  amount rose between two pages, to before that place, is not read
  again, and one whose amount fell, to after it, is listed again with
  its new amount, so a consumer that pages while items are posted or
  cleared keys its rows by `customer_id` (review round 1). Names and dunning are read for the page's
  customers only.
- **Key date.** Required and at most 2100-12-31, as a run's (T76b).
  The items are those open at the read, not on the key date (§4).
- **Budget.** 01 §1.3 gets the row: p95 3 s, p99 6 s over 100,000 open
  customer items, the liquidity forecast's figures for a report computed
  on read at that volume (decided by default).
  `TestCollectionWorklistBudget` (2,000 customers with 50 items each,
  every item dunned once, a page of 100): p95 1.2-1.5 s at a load
  average of 35-47, 2026-11-12.
- **Walkthrough.** `step_collections` (step 58): at the key date of the
  run confirmed in `step_dunning`, Northwind leads Maple's worklist with
  its notice's overdue amount, its items' due date, level 1 and the key
  date as last dunning date, its open amount less than that by the
  credit memo of step 55, which reduces nothing overdue; at the due date
  it is not listed.

### Items without payment terms (po:2026-11-12, no migration)

- **Rule** (product owner 2026-11-12, answering question 2 and
  overruling default 3): a customer open item posted without payment
  terms is due on its document date. On that date it is not overdue; from
  the day after it is, by the days past it.
- **One place.** `ledger.ReceivableItem.NetDueDate()` returns the terms'
  net due date or, without terms, the document date, and
  `DaysOverdue(on)` counts from it; both lost their second result
  (`ok`), so no reader can branch on "no terms" any more. `overdue`
  (propose.go), which a run's proposal, a confirmation's check and the
  worklist share, reads them; a notice item's `net_due_date` and the
  worklist's `oldest_due_date` are that date. Nothing is stored
  differently: an item without terms still has no row in
  `ledger.open_item_terms`, and posting defaults no terms.
- **What a run now proposes.** A debit without terms once it is past its
  document date by the first level's days, at the levels and one-level
  steps of §4. Credit items stay undunned and reduce nothing, with or
  without terms. Customer special G/L items do not exist today (special
  G/L A is the supplier down payment account's, ADR-0037); one would
  follow the rule.
- **Not changed:** supplier items (a payment proposal takes an item
  without terms as due by its supplier's current terms), and
  `GET /v1/open-items`, whose `due_date_to` filter and aging show an item
  without terms, a customer's too, without a due date (`no_due_date`):
  that list shows the items as posted. The AR aging follows the rule
  (ADR-0063 As built).
- **Tests.** `TestItemsWithoutTerms` (worklist: not listed on the
  document date, listed the day after with that date as oldest due date;
  a run: nothing on the document date or a day short of the first level,
  proposed at its days with the document date as `net_due_date`);
  `TestNextLevel`, `TestConfirmationsSerialize`, `TestKeyDates`,
  `TestWorklist` and `TestWorklistPages` now carry the story's debit
  without terms, dated the key date, through the later runs.

### T86a Dispute case opened and closed (migration 000324)

Decided by default under this ADR (`plan-breadth-week3.md` §T86a and
default 4; no new ADR), each listed for the product owner's digest.

- **Schema.** `collections.dispute_case`: the company, the customer (id
  and number when opened), the open item by its journal line (fiscal
  year, journal entry, line) with its document number and reference,
  the disputed amount in the item's currency and its share in the
  company's, the reason, an assignee and a note, the status (`open`,
  `accepted`, `rejected`) and, once closed, the close's reason, time and
  person. The checks are in 03 §3.27 and `db/collections_test.go`
  (`TestDisputeCaseRules`, `TestDisputeCaseClosedForTheOwner`). The
  application may set the assignee, the note, the status with the
  close's columns, the version and `updated_by` alone; the trigger
  `dispute_case_closed_once` keeps a closed case as it is for every
  role. Permissions `collections.dispute.read` and `.write`; `.write`
  opens, assigns and closes (decided by default: the plan names two).
- **What a case is opened on.** An open item of the company in the
  leading ledger on a customer reconciliation account that the customer
  owes (a debit), read from `ledger.OpenItems.Receivable` when the case
  is opened, due or not (decided by default: a customer disputes an
  invoice before it is due too). Any other line is 422
  `collections.item_not_open` naming `journal_entry_id`. A case names
  the item by its journal line, as a dunning notice's item and the
  clearing do, not by a receivables invoice: collections reads the
  ledger's items whoever posted them (§1), so a migrated item can be
  disputed too. The read takes every open customer item of the company,
  as a run and the worklist do; a narrower ledger read is not added for
  one item (decided by default, rule 9: no new port method).
- **Amount.** In the item's currency (422 `collections.currency_mismatch`
  naming `disputed_amount.currency`), above zero and at most what the
  customer owes on the item (422 `collections.invalid_disputed_amount`
  naming `disputed_amount.amount`, the open amount in the message). The
  case also stores `disputed_amount_lc`, its share of the item's amount
  in the company's currency as booked: local amount × disputed ÷ amount,
  rounded half away from zero once (`collections.share`), so all of the
  item disputed is all of its local amount and nothing is revalued (as
  §4's amounts are as booked). T86b's worklist adds it up.
- **One open case per item.** The partial unique index
  `dispute_case_one_open` refuses a second, whether it comes after the
  first or races it (the second insert waits for the first's and fails),
  mapped to 409 `collections.dispute_exists`. A closed case leaves the
  item free for a new one. `TestProblemCodes` lists the code for the
  update and the close too, since they write the table; neither can
  reach it, and the contract says so beside the code.
- **Assignee.** An active person of the tenant who may act in the case's
  company, on opening and on a change (422 `collections.assignee_invalid`
  naming `assignee_id`), as a close task's assignee is (ADR-0058 §6).
  The rule moved from `internal/closing` to `authz.AssignableTo`, which
  both contexts call, rather than being copied; closing's refusals are
  unchanged. Names come through the port `collections.People`, wired to
  `app.LedgerPeople`; collections imports no further context.
- **Change and close.** `PATCH /v1/dispute-cases/{id}` (merge patch of
  `assignee_id` and `note`) and `POST /v1/dispute-cases/{id}:close`
  (`outcome` `accepted` or `rejected`, an open enumeration whose other
  values are 422 `collections.unknown_outcome`, and the reason) read the
  case `FOR UPDATE` and compare `If-Match`; a case that is not open is
  409 `collections.dispute_closed`; the close's update is also guarded
  by `status = 'open'` (ADR-0008). A patch that changes nothing is 422
  `collections.nothing_to_change`. Texts of white space alone are 422
  `request.validation_failed`. A case whose item was cleared meanwhile
  stays open and is closed as any other (plan acceptance). Closing posts
  nothing: an accepted case is credited by a person (T85c, T75d).
- **Lock order.** Opening takes no row lock: it reads the open items and
  inserts, and the unique index orders two openings of one item. A
  change or a close locks the one case row and writes nothing else, so
  no two writers take locks in opposite orders.
- **Event and audit.** `DisputeCaseChanged` (`opened`, `updated`,
  `closed`; not a webhook event, as the plan asks of week 3's events)
  names the customer by id and number and carries no free text, since a
  reason or a note may name people; audit actions
  `collections.dispute_case.open`, `.update`, `.close`, whose detail
  holds the texts.
- **List.** `GET /v1/dispute-cases`, the latest first (`created_at`,
  keyset), filtered by company, customer, status and assignee. No
  budget test: a keyset-paged list of stored rows over an index (07
  §7.7 item 7), as the dunning runs' list.
- **Walkthrough.** `step_dispute` (step 95): Ruth opens a case on the
  invoice `step_dunning` dunned for CAD 150.00 of it; a second case and
  more than the item's amount are refused; she assigns it to herself
  with a note and reads it back; Northwind's open items are unchanged.

### T86b Disputed invoices in dunning and the worklist (migration 000325)

Decided by default under this ADR (`plan-breadth-week3.md` §T86b and
default 4, confirmed by the product owner on 2026-11-12 as question 9),
each listed for the product owner's digest.

- **Left out of the proposal.** A run reads the open dispute cases of
  company (`collections.openCases`, by the index
  `dispute_case_open_by_company`; review round 1: one read of the
  company's open cases rather than one per item set, which the worklist
  shares) and leaves an item that would reach a
  level but has an open case out of every notice and every total,
  whatever part of it is disputed (plan default 4: dunning the
  undisputed part is among the plan's "Later"). An item disputed but not
  overdue enough for a level is neither proposed nor listed.
- **Listed on the run as disputed.** `dunning_run.disputed_items`, a JSON
  list written once with the run (the application's update grant leaves
  it out, so a confirmation does not change it), by customer and item:
  the open case, the customer's id and number, the item by journal line
  with its document number, reference, net due date and days overdue,
  and its amounts in its currency and the company's (the currency's
  digits read with the run from `ref.currency`, not stored). The plan
  asks for no new table, and a list read whole with its run is what a
  column holds; the API gives it as `disputed_items` on every run,
  empty for runs started before (decided by default).
- **Totals.** The run gives `disputed_item_count` and `disputed_amount`
  (company currency, the sum of the items' local amounts, checked for
  range when the run is proposed) beside its own totals (review round 1:
  a report without its totals).
- **Size.** The disputed items count with the proposed ones towards the
  5,000 a run takes (§5): they were read and are written with the run.
  The limit is checked once every item is counted, so a run whose items
  are all disputed is held to it too (review round 1).
  The refusal's detail now says "take more than N overdue items, the
  most one run proposes or names as disputed".
- **A run proposed before the case.** Confirming it does not compare
  cases (plan acceptance): the proposal is confirmed as it was and the
  item recorded as dunned at the level proposed. Only clearing and
  another run's dunning make a proposal stale (§8).
- **Worklist.** Each customer gets `disputed_amount`, the sum of
  `disputed_amount_lc` of the open cases of its open items, overdue or
  not (a dispute of an invoice not yet due is still the customer's), and
  `open_dispute_cases`, their number, with the worklist's totals of both
  over every customer listed (review round 1). The company's open cases
  are read once per request, beside its open items, and matched to the
  items; a case on a customer not listed counts in no total. A disputed item stays
  open and overdue, so `open_amount`, `overdue_amount`, the order and
  T76c's totals are unchanged; a case whose item was cleared counts no more,
  as a cleared item's dunning does not. Amounts that add up past an
  amount's range are refused as the worklist's others are (422
  `collections.amount_too_large`).
- **Budget.** `TestCollectionWorklistBudget` now seeds an open case on
  one item in ten (about 10,000): after review round 1 (the company's
  open cases read once), p95 2.2 s at scale 1 and 2.3 s at scale 2
  (budget 3 s and 6 s), median 1.4-1.5 s, at a load average of 32-47,
  2026-11-13; T76c measured 1.2-1.5 s at 35-47.
- **Walkthrough.** `step_dispute` extended: a run at `step_dunning`'s key
  date names the invoice as disputed with its case and has it in no
  notice; the worklist shows Northwind's CAD 150.00 disputed in one open
  case; Ruth closes the case as rejected and a new run at the key date
  proposes the invoice again (since T105b in `step_collections_agent`,
  step 98, once the collections agent has recommended on the worklist
  while the case is open; ADR-0072 As built).

### T76d Dunning charges, interest, grace days and minimum amounts (migration 000353)

Decided by default under this ADR (`plan-depth-1.md` §T76d; SAP
S/4HANA's dunning procedure, FBMP, as the reference; product owner
2026-11-17: the variants start), each listed for the product owner's
digest. It answers question 1 above for the first version: charges,
interest and a minimum amount are the tenant's configuration, and no
rule of law is built in.

- **Configuration.** `companies[].dunning` is the company's one dunning
  procedure (procedures per customer stay later): `grace_days` (0-365),
  `minimum_amount` (in the company's currency), `interest_rate_percent`
  (a year, above 0 and below 100, three places at most) and per level
  `charge` (in the company's currency) and `interest` (true: the level's
  items are charged interest; the rate is then required). Validated
  with each field named (`config.validation_failed`); tier 2 as the
  levels, since nothing is posted. None is shipped.
- **Grace days.** An item overdue by no more than the grace days is not
  proposed, whatever level its days reach (SAP's line item grace
  periods). The levels' days still count from the net due date, and the
  worklist, which proposes nothing, ignores them.
- **Minimum amount.** One per procedure, compared with a notice's
  overdue amount before charges and interest (SAP keeps one per level
  and currency: later). A notice below it is not proposed: its items are
  in no notice and are not recorded when the run is confirmed, so their
  level stays. The run counts such notices (`below_minimum_notice_count`,
  `below_minimum_amount`) and names no customer for them. The minimum
  applies after the disputed items are left out. A notice amounting to
  0.00 in the company's currency (a foreign-currency item booked at 0)
  is below any minimum and counted (review round 1).
- **Levels dropped.** An item keeps a level a confirmed run recorded
  even after the company drops that level (a level never falls, §4);
  its charge and interest are then the highest configured level's
  (`procedure.rule`, review round 1).
- **Charge.** The charge of the notice's level, a fixed amount in the
  company's currency, once per notice (SAP's charges per level; a
  charge by percentage or per currency is later).
- **Interest.** Per item at a level whose `interest` is true:
  `amount_lc × rate ÷ 100 × days overdue ÷ 365`, actual days over a
  year of 365, from the net due date to the key date, rounded half away
  from zero once per item in the company's currency
  (`collections.interestOn`). It is the interest to the key date, not
  the interest since the last notice: each notice states it in full. A
  statutory rate (a central bank's base rate plus points, as some
  countries set for late payment) is not built in: the tenant sets the
  rate (specialist question below).
- **Nothing is posted.** Charges and interest are what the notices ask
  for. Posting them to the customer's account is later, as SAP's
  dunning run does not post them either.
- **Stored.** The run keeps the procedure as it applied it (grace days,
  minimum amount, rate, null for none) and its charge, interest and
  below-minimum totals; a notice its charge and interest; an item its
  interest. Columns with defaults (06 §6.4 expand only): earlier runs
  read as without a procedure. The application's update grant on a run
  leaves them out, and notices and items stay insert-only. The checks
  are in 03 §3.27 and `db/collections_test.go` (`TestDunningCharges`).
- **API and events.** `DunningRun` gains `charge_amount`,
  `interest_amount`, `total_amount`, `grace_days`, `minimum_amount`,
  `interest_rate_percent` (absent for none),
  `below_minimum_notice_count` and `below_minimum_amount`;
  `DunningNotice` `charge_amount`, `interest_amount` and `total_amount`;
  `DunningNoticeItem` `interest_amount`. `DunningRunChanged` carries the
  run's charges and interest, `DunningNoticeIssued` the notice's and its
  total. Every amount carries its currency. Amounts that add up past an
  amount's range are refused, 422 `collections.amount_too_large`, as
  before.
- **Walkthrough.** `step_dunning_charges` (step 111): a new customer,
  Lakeshore Hardware, with a CAD 30.00 invoice on NT30 and a CAD 600.00
  one on NT60; Ada sets 7 grace days, a CAD 50.00 minimum, 9% interest
  and level 2's CAD 15.00 and level 3's CAD 25.00 with interest through a
  tier 2 change; a run 6 days past the small invoice's due date leaves
  it out, one 10 days past counts Lakeshore's notice as below the
  minimum; at 50 and 20 days the notice is proposed at level 1 for CAD
  630.00 without charges and confirmed; at 70 and 40 days it is level 2:
  CAD 15.00 and interest of 0.52 and 5.92, CAD 651.44 in all.
- **Specialist question.** Whether a pilot country fixes the rate or
  the day count of interest on late payment, or caps dunning charges,
  for business customers: not built in, the tenant's rate and charges
  apply as configured.

### T76e Dunning blocks (migration 000354)

Decided by default under this ADR (`plan-depth-1.md` §T76e; SAP
S/4HANA's dunning block on the customer's company code data and on the
line item, MANSP, as the reference), each listed for the product owner's
digest.

- **Schema.** `collections.dunning_block`: the company, the customer (id
  and number when placed), the open item by its journal line with its
  document number or none of them (a block of the whole customer), the
  reason (free text up to 500 characters: SAP's blocking reasons are
  codes; a reason list is later), `valid_to`, the last key date it holds
  for, and once released when and by whom. The checks are in 03 §3.27 and
  `db/collections_test.go` (`TestDunningBlockRules`,
  `TestDunningBlockReleasedForTheOwner`). The application may set the
  release's columns, the version and `updated_by` alone; the trigger
  `dunning_block_released_once` keeps a released block as it is for
  every role. Permission `collections.dunning.block` places and releases
  blocks; `collections.dunning.read` reads them (a block is part of
  dunning, decided by default).
- **What a block names.** A customer that is a business partner of the
  tenant (422 `request.reference_not_found` naming `customer_id`) and,
  for an item, an open item of the company the customer owes, read as a
  dispute case's is (422 `collections.item_not_open` naming
  `journal_entry_id`; the shared message now names both). A journal line
  named in part is 422 `request.validation_failed`. The end date is
  required (the brief: a reason and an end date), not before the
  company's today in its time zone, so a block holds for some run, and at
  most 2100-12-31, the last key date (422 `collections.invalid_block_end`
  naming `valid_to`, today and the zone in the detail). The clock is the
  endpoints' (`Endpoints.Now`, the composition root's).
- **In force.** A block holds for a run started while it is not released
  whose key date is on or before `valid_to`; the run reads the company's
  unreleased blocks ending on or after its key date
  (`collections.blocksInForce`, index `dunning_block_in_force`). Blocks
  are not exclusive: several of one customer or item may be active, each
  ending or released on its own, so no unique key refuses a second one
  (a run names the latest placed).
- **The run.** An item that would reach a level and is blocked, by its
  own block or its customer's, is in no notice and in none of the run's
  totals; the run lists it in `dunning_run.blocked_items` (a JSON list
  as `disputed_items`, written once, left out of the update grant), with
  the block, the customer's number and what the run read of the item,
  and gives `blocked_item_count` and `blocked_amount`. A blocked item is
  not listed as disputed too: the block is checked first. Blocked items
  count towards the 5,000 a run takes, as disputed ones do. The minimum
  amount (T76d) applies to what is left.
- **Stale on confirmation.** Unlike a dispute case opened after a
  proposal (T86b, plan acceptance), a block in force for the run's key
  date when the run is confirmed makes the proposal stale (409
  `collections.proposal_stale`, the detail counting the blocked items):
  a block is the explicit instruction not to dun (the safer default).
  The check reads the blocks after the company's advisory lock; a block
  placed while a confirmation is running takes no lock and may commit
  after the confirmation read the blocks, as if placed after it.
- **Release.** `POST /v1/dunning-blocks/{id}:release` with `If-Match`
  reads the block `FOR UPDATE`, refuses a released one (409
  `collections.block_released`) and updates it guarded by
  `released_at IS NULL` (ADR-0008). The end date is not changed: a new
  block is placed instead (decided by default, no patch).
- **Lock order.** Placing takes no row lock (it reads the open items and
  inserts); releasing locks the one block row. A confirmation takes the
  company's advisory lock and reads blocks without locking them, so no
  two writers take locks in opposite orders.
- **Audit, no event.** Audit actions `collections.dunning_block.place`
  and `.release`, whose detail holds the reason. No event: a block
  changes nothing outside the context, and the runs started afterwards
  name the items it holds back (decided by default).
- **Not in the worklist.** The collection worklist does not show blocks
  yet (later).
- **Walkthrough.** `step_dunning_blocks` (step 112): Ruth blocks
  Lakeshore's gasket invoice up to a date 120 days away and a run at
  step 111's last key date names it as blocked and duns the pump invoice
  alone; an end date in the past is refused; a colleague's proposal is
  refused as stale once Ruth blocks the whole customer for a payment
  plan; a run at the key date duns nothing of Lakeshore's, each invoice
  named with its block; after the customer's block has ended the pump
  is dunned again; the gasket's block released, once, the next run duns
  both.

### T76f Promises to pay (migration 000365)

Decided by default under this ADR (`plan-depth-2.md` §T76f; SAP S/4HANA
Collections Management's promise to pay as the reference; product owner
2026-11-17: the variants go on), each listed for the product owner's
digest.

- **Schema.** `collections.promise_to_pay` (the company, the customer by
  id and number, the promised amount in the company's currency, the
  promised date) and `collections.promise_to_pay_item` (each item by its
  journal line with its document number, reference, net due date and
  what the customer owed on it in its currency and the company's),
  both insert-only for every role: a promise is a record of what the
  customer said, not changed or withdrawn (later). The checks are in 03
  §3.27 and `db/collections_test.go` (`TestPromiseToPayRules`,
  `TestPromiseToPayFixedForTheOwner`). Permission
  `collections.worklist.write` records a worklist's work, promises here
  and contacts in T76g; they are read with `collections.worklist.read`
  (decided by default: one permission for a collector's work rather
  than one per kind).
- **What a promise names.** A customer that is a business partner of the
  tenant (422 `request.reference_not_found` naming `customer_id`) and 1
  to 100 open items of the company it owes, each read as a block's item
  is (422 `collections.item_not_open` naming `items[n].journal_entry_id`;
  `openItem` and `customersItem` now take the field), named once (422
  `request.validation_failed` naming `items[n]`). The amount is in the
  company's currency (422 `collections.currency_mismatch`), above zero
  and at most what the items amount to in it as booked (422
  `collections.invalid_promised_amount`): SAP's promised amount is in the
  company code's currency too. The date is from the company's today in
  its time zone to 2100-12-31, the last key date (422
  `collections.invalid_promised_date`, today and the zone in the detail),
  by the endpoints' clock (`Endpoints.today`, which the block's end date
  now shares).
- **One promise of an item at a time.** An item in another promise whose
  date is today or later is refused (409 `collections.promise_exists`,
  naming the promise), so the worklist does not count one item's
  promises twice. The check runs after a transaction-scoped advisory
  lock on the customer, so two promises of one item recorded at once
  wait for each other and the second sees the first (`TestPromisesRace`,
  `apitest.Race`); no row lock is taken.
- **Status, judged on read.** `open`, `kept` or `broken`, computed from
  the items as they are when read and a date, never stored:
  `paid_amount` is what the promised items no longer open
  (`ledger.OpenItems.Closed`, a read without lock, now on the
  `OpenItems` port) amount to in the company's currency as booked; a
  promise is kept once that reaches the promised amount, broken once the
  date is past the promised date and it is not kept, open otherwise. The
  promise's own read judges it on the company's today (`status_date`);
  the worklist judges it on its key date. Decided by default: a promise
  paid after its date reads kept from then on (the outcome is the
  items', not a history; SAP's check runs as a job and fixes it, which a
  later variant may do with the payment's date). An item counts in full
  once it is cleared, whether paid, credited or reversed: a partial
  payment that leaves a residual item (T75f) clears the original and
  counts it paid in full (the residual is receivables'; following it is
  later).
- **The run.** A run at a key date up to a promise's date leaves the
  promised items still open out of its notices and totals and lists them
  in `dunning_run.promised_items` (a JSON list as `blocked_items`,
  written once, out of the update grant) with the promise and its date,
  `promised_item_count` and `promised_amount`. A promise holds until its
  date whether it is kept or not (the brief: a block until the date). A
  blocked item is listed as blocked alone; a promised one is not listed
  as disputed (precedence block, promise, dispute: the explicit holds
  first). Promised items count towards the 5,000 a run takes. Of several
  promises of one item a past key date finds, the latest recorded is
  named.
- **Stale on confirmation.** A promise recorded after a proposal, by a
  date on or after the run's key date, makes it stale (409
  `collections.proposal_stale`, the detail counting the promised items),
  as a block does (T76e): the customer's word is an instruction not to
  dun. The check reads the promises after the company's advisory lock.
- **Worklist.** Each customer gets `promised_amount` (the sum of its
  promises open on the key date), `open_promises`, `next_promised_date`
  and `broken_promises`; the worklist `promised_amount` over every
  customer listed. The open promises are read once for the company (its
  promises dated on or after the key date, `promise_to_pay_by_date`);
  the broken ones, which reach back over every earlier promise, for a
  page's customers only (`promise_to_pay_by_customer`), as names and
  dunning are, so `broken_promises` has no worklist total.
  `open_amount`, `overdue_amount`, the order and the other totals are
  unchanged.
- **Audit, no event.** Audit action `collections.promise_to_pay.record`
  with the amount, date and items. No event: a promise changes nothing
  outside the context, and the runs name the items it holds (as a
  block). The collections agent never records a promise
  (`collectionsagent.Never`, ADR-0072).
- **Walkthrough.** `step_promise_to_pay` (step 118): Lakeshore promises
  the gasket's CAD 30.00 and the pump's CAD 600.00 by two dates; a
  second promise of the pump, CAD 600.01 and yesterday are refused; a
  run before both dates names both invoices as promised and duns
  neither; the worklist shows CAD 630.00 in two promises; Lakeshore pays
  the gasket and its promise reads kept; at the day after the pump's
  date the worklist counts its promise broken and a run duns the pump.

### T76g Contacts with customers and resubmission dates (migration 000366)

Decided by default under this ADR (`plan-depth-2.md` §T76g; SAP S/4HANA
Collections Management's customer contact and resubmission as the
reference), each listed for the product owner's digest.

- **Schema.** `collections.customer_contact`: the company, the customer
  by id and number, `contacted_at`, whom at the customer
  (`contact_name`, optional), the outcome, a note and an optional
  resubmission date; insert-only for every role (a contact is a record
  of what happened; a correction is a later contact). The checks are in
  03 §3.27 and `db/collections_test.go` (`TestCustomerContactRules`,
  `TestCustomerContactFixedForTheOwner`). Recorded with
  `collections.worklist.write` (T76f), read with
  `collections.worklist.read`. `contact_name` and `note` are in the
  personal data inventory (`records.Inventory`; `collections` left
  `records.SchemasWithout`).
- **Who, when, outcome.** The collector is who recorded it (`created_by`,
  shown by name as `contacted_by_name`). `contacted_at` is not after now
  by the endpoints' clock (422 `collections.invalid_contact_time`); a
  contact may be recorded afterwards, so an earlier time is accepted.
  The outcome is `reached`, `not_reached` or `left_message`, an open
  enumeration (422 `collections.unknown_outcome`, the code dispute
  closes use; SAP's contact results are configuration: later). Texts of
  white space are 422 `request.validation_failed`.
- **Resubmission.** Optional, after the company's today in its time zone
  (a date of today or earlier would hold the customer off no worklist
  from now on) and at most 2100-12-31 (422
  `collections.invalid_resubmission_date`). A customer's last contact is
  its latest by `contacted_at` (then by id); while that contact's
  resubmission date is after a worklist's key date the customer is not
  listed, so it reappears on that date, and a later contact without one
  brings it back at once. The worklist judges at its key date, as it
  judges promises (T76f).
- **Worklist.** Customers left out count in `resubmission_count` and
  `resubmission_overdue_amount` and in none of the other totals; each
  listed customer shows its `last_contacted_at`, `last_contact_outcome`
  and `resubmission_date`. The customers held are read once per request
  (those with a contact resubmitted after the key date,
  `customer_contact_resubmission`, whose last contact is then read over
  `customer_contact_latest`); the last contacts of a page's customers
  only. Dunning is unchanged: a resubmission date holds no item from a
  run (a dunning block or a promise does).
- **Budget.** `TestCollectionWorklistBudget` now also seeds a promise on
  one item in ten (T76f) and three contacts per customer, the last of one in twenty resubmitted after the
  key date: p95 1.38-1.63 s at scale 1 and 2 at a load average of 19-39,
  2026-11-17 (budget 3 s and 6 s; one scale 1 run at a load of 43 had a
  6.0 s outlier, its median 1.47 s).
- **Audit, no event.** Audit action `collections.customer_contact.record`
  with the ids, `contacted_at`, the outcome and the resubmission date,
  and `has_contact_name`/`has_note` in place of the texts, which are
  personal data a deletion run blanks on the contact and could not blank
  in the insert-only trail (ADR-0026, ADR-0079; batch 79 review). No
  event, as a block's. The collections agent never
  records a contact (`collectionsagent.Never`).
- **Walkthrough.** `step_collection_contacts` (step 119): Ruth records a
  call with Lakeshore's clerk with a resubmission date 135 days out; a
  contact dated tomorrow is refused; at step 118's last key date
  Lakeshore is off the worklist and counted aside; at the resubmission
  date it is back with its last contact; a later message without a
  resubmission date brings it back at once; its contacts read the latest
  first.

### T76h Dunning charges and interest posted (migration 000400)

Decided by default under this ADR (`plan-depth-3.md` §T76h; product
owner 2026-11-17: the variants start; SAP S/4HANA's interest on arrears
posted to the customer, FINT, as the reference, from memory), each
listed for the product owner's digest. It changes §1 and T76d's
"Nothing is posted": a confirmed run posts.

- **Where it posts.** Through receivables' published API, as sales posts
  its billing documents (ADR-0067 §9): per notice charged a charge or
  interest, one customer debit memo (T75j, document type `DD`) entered
  and posted in the confirmation's transaction, after its status guard,
  naming the notice as its source document (`source_document_id`, the
  notice's id also as its number: a notice has none), so receivables
  takes it once (`customer_invoice_source_once`). Its entry debits the
  customer's reconciliation account, the customer's open item, which a
  later run duns like any other once it is overdue, and credits the
  charge and the interest on their accounts. One memo for the two rather
  than one each: one document per notice for the customer, as the notice
  asks for both together.
- **Accounts.** `companies[].account_determination.dunning_charges` and
  `.dunning_interest`, revenue accounts of the company's chart; tier 3,
  posting-relevant, as the account determination is. A company naming
  neither posts nothing (T76d's behaviour, every company today); one
  naming either posts, and must name the account of each its levels
  charge (a charge above zero, interest), or the configuration is
  refused naming the missing one. The charges, rate and levels stay tier
  2: they change what a notice asks for, as a price does, not where it
  posts. Collections resolves the numbers through the ledger's account
  reads (`ledger.AssetReads.Accounts`), not the ledger's account
  determination rows, which keep a removed key's account.
- **The memo.** The notice's customer, the company's currency, the
  company's today in its time zone (`clock`, `CompanyDay`) as its date
  and posting date: the day the charge is raised, in whose fiscal year it
  is numbered and due by the customer's payment term in the company. Not
  the run's key date, which may lie in a fiscal year without a
  `customer_invoice` range yet (the walkthrough's future key dates found
  it). An item "Dunning charge, level n" on the charge account and one
  "Interest on overdue items to <key date>" on the interest account, each
  only when above zero, without a tax code.
- **Refusals.** What receivables or the ledger refuses refuses the whole
  confirmation, nothing recorded or posted, with their code and the
  customer's number leading the detail and without their field errors,
  which name the memo's fields (`problem.Led`, moved to the platform for
  any context that posts through another's): a customer that is not
  usable (blocked for posting, without data in the company), an account
  no longer postable, a missing `customer_invoice` or `journal_dd` range,
  a period not open. A charge or interest whose account the
  configuration no longer names since the run was proposed is 422
  `collections.no_charge_account`. The controller blocks such a customer
  from dunning (T76e) and starts a new run. Posting needs the credit
  controller's `collections.dunning.run` alone, as billing's posting
  needs no permission of receivables.
- **Stored.** `collections.dunning_charge`: per notice the memo's id and
  number, its entry and posting date; insert-only, one per notice and
  per memo (03 §3.27, `db/dunning_charge_test.go`). `DunningRun`
  `charge_document_count`, `DunningNotice` `charge_document`;
  `DunningNoticeIssued` names the memo (`customer_invoice_id`, `_number`,
  null for none) and `DunningRunChanged` the count. Receivables' API
  refuses to reverse such a memo, naming the dunning notice
  (`receivables.entered_from_source`).
- **Layout.** Collections imports receivables' published types for the
  port (`collections.Invoices`), as sales does; it stays a leaf, nothing
  imports it.
- **Size.** A run of 5,000 items has at most as many notices, each a
  memo entered and posted in the one transaction; runs as a workflow stay
  later.
- **Specialist question.** Whether a pilot country taxes dunning charges
  or interest on late payment (VAT/GST): posted without a tax code, as
  outside the scope of tax, stated from memory and not checked.
- **Walkthrough.** `step_dunning_postings` (step 138), first half: Ada
  adds 4810 Dunning charges and 4820 Interest on overdue receivables and
  names them through a governed change; Bayview Marine's CAD 400.00
  invoice is dunned at level 1 without a memo, then at level 2: a debit
  memo of CAD 15.00 and 2.96 interest, on the two accounts, Bayview's
  open item.

### T76i A confirmed dunning run cancelled (migrations 000401, 000402)

Decided by default under this ADR (`plan-depth-3.md` §T76i; SAP's reset
of a dunning run's dunning data, from memory, as the reference), each
listed for the product owner's digest. It changes §8's "a confirmed run
is not cancelled".

- **What a cancellation does.** `POST /v1/dunning-runs/{id}:cancel`
  (`If-Match`, a `reason`, permission `collections.dunning.run`,
  human-only and on `collectionsagent.Never`) moves a confirmed run to
  `cancelled` once, by an update guarded by its status under the
  company's dunning lock, which confirmations take too (`lockDunning`).
  The levels its confirmation recorded stay in `dunned_item`, insert-only,
  and no longer count: `recorded` reads the records of confirmed runs
  alone, so the next run, the worklist and a confirmation's stale check
  see each item at the level the runs before it recorded. A proposal
  started before the cancellation is stale (the times its items were
  dunned changed). The run's notices and amounts stay, for the record.
- **Latest first.** A run whose items a later confirmed run dunned from
  the levels this one recorded (that run's `previous_dunnings` above this
  one's), or whose debit memos a later run dunned, is refused, 409
  `collections.run_superseded`, naming the latest such run: its levels
  were reached through this one's, and cancelling it first keeps every
  item's recorded levels a chain one level at a time (the
  `dunning_notice_item_dunnings_check` invariant).
- **Memos reversed.** Each debit memo its confirmation posted (T76h) is
  reversed through receivables' new published `ReverseSourced`, for the
  context that entered it, with the cancellation's reason, on the memo's
  own posting date (T75k's reversal: the ledger reverses the entry and
  clears the memo's item against it). A memo whose item is cleared (409
  `receivables.document_cleared`, `ledger.entry_cleared`) or posted in a
  period no longer open (409 `ledger.period_not_open`) refuses the whole
  cancellation, its number and customer leading the detail; nothing is
  done. A reversal on a date of its own (into the next open period) is
  later. Receivables' API still refuses to reverse such a memo, naming
  the dunning notice and this operation (`receivables.entered_from_source`);
  `customer_invoice_reversed` admits a reversed debit memo with a source.
- **Stored.** `dunning_run.cancellation_reason` (1-200 characters, not
  white space alone, exactly when cancelled, `dunning_run_cancelled`) and
  status `cancelled`; the trigger `dunning_run_confirmed_once` admits
  confirmed → cancelled and no change after it. `dunning_charge` names the
  memo's reversal (entry year and id, date not before the memo posted,
  `dunning_charge_reversed`), set once (trigger
  `dunning_charge_reversed_once`, for every role). Checks added NOT VALID,
  validated by 000402 (03 §3.27, `db/dunning_cancel_test.go`,
  `db/dunning_charge_test.go`). The reverts are refused while a run is
  cancelled or a sourced memo reversed.
- **API and events.** `DunningRun.cancellation_reason`, status
  `cancelled`; `DunningChargeDocument.status` (`posted`, `reversed`) with
  the reversal. `DunningRunChanged` (`cancelled` from `confirmed`) and one
  `DunningNoticeCancelled` per notice, naming the customer by id and
  number and the reversed memo, so a subscriber that wrote to the customer
  withdraws the notice. Neither event nor the audit record carries the
  reason (ADR-0079); the run and the memos keep it.
- **Concurrency.** Cancellation and confirmation of a company's runs
  serialize on the lock; a cancellation racing a later proposal's
  confirmation either makes that proposal stale or is refused as
  superseded, and two cancellations of a run cancel once
  (`TestCancellationsSerialize`). Locks after the company's: the run's
  row, then each memo's row (receivables) and its entry (ledger), the
  order an incoming payment's application never reverses (it locks
  entries alone).
- **Walkthrough.** `step_dunning_postings` (step 138), second half: a
  level 3 run posts Bayview a second memo; cancelling the level 2 run
  first is refused as superseded; the level 3 run is cancelled, then the
  level 2 run, each memo reversed on its posting date; a second
  cancellation is refused; Bayview's open items are its invoice alone and
  a new run dunns it from level 1 again.
