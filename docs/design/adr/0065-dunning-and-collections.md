# ADR-0065: Dunning and collections

**Status:** accepted (T76a, 2026-11-12; decided by default under ADR-0053 d;
`go-reviewer` had no blocking finding on T76a's last commit). It decides
the collections context and the slices T76a-T76c
(`docs/design/plan-breadth-week1.md`, Lane B). Each slice records what it
built under "As built"; T76a's and T76b's are there now. The defaults taken and the
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
- were posted with payment terms: an item without terms has no due date
  the ledger keeps (ADR-0063 §5), so it is never overdue. Special G/L
  items are posted without terms today and so are left out with them;
- are overdue on the key date by at least the days of the first level:
  days are counted from the net due date to the key date, and an item due
  on the key date is not overdue (`ledger.ReceivableItem.DaysOverdue`).

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
   days; an item without payment terms is never dunned.
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
   instead?

## As built

### T76a Dunning run proposed (migration 000268)

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
- **Walkthrough.** `step_dunning` (step 49): Ruth posts an invoice to
  Northwind on NT30; a run before Maple has dunning levels is refused;
  Ada gives Maple its levels through a tier 2 change; a run at a key date
  40 days after the invoice date, ten days past its due date, proposes
  one notice for Northwind with the invoice's item at level 1; a run at
  the due date proposes none; the customer's open items are unchanged.
  The step duns an invoice of its own, so it holds whether or not the
  invoice of step 48 is still open when it runs.

### T76b Dunning run confirmed (migration 000269)

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
