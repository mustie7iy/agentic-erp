# ADR-0006: Append-only ledger, derived balances, partitioning by fiscal year

**Status:** proposed

## Context
Financial records must never change after posting (SOX, audit). The ledger is
the highest-volume table (up to 10 billion lines per region per year).

## Decision
- `ledger.journal_entry` and `ledger.journal_line` are insert-only; the
  application role has no UPDATE/DELETE grant on them. Corrections are
  reversal entries referencing the original. An entry is never stored
  before it is posted: a manual entry that waits (for a person to post it,
  or for approval above the company's limit, T35) is parked in its own
  table, `ledger.parked_entry`, and the journal entry it becomes names it as
  its source document.
- Clearing (which open items are settled) is recorded in a separate insert-only
  table `ledger.clearing` instead of updating lines.
- Balances are derived: `ledger.period_balance` (tenant, ledger, legal entity,
  account, period, currency, dimensions) is maintained by an idempotent
  projector consuming `JournalEntryPosted` events, so posting never contends
  on a hot balance row. Reports read balances plus unprojected entries for
  exact, current figures.
- `journal_line` is partitioned by fiscal year (range), sub-partitioned by
  hash of `tenant_id` when a cell needs it; old years move to cheaper storage.
- Every entry is balanced per currency, enforced by a deferred constraint
  trigger at commit.

## Alternatives
- **Updating a balance row in the posting transaction:** simple reads, but a
  hot row per account serializes postings at volume.
- **Updating lines to mark clearing:** breaks immutability.

## Consequences
- Trial balance is a read over aggregates plus a small delta.
- Reversals double the lines for corrections; that is the audit requirement.

## Amendment (T33f, 2026-10-16)
GR/IR positions (`ledger.gr_ir_position`) are the exception. They are
updated in the posting transaction under the order's GR/IR advisory lock,
because a posting must read its order item's position to value the next
movement. Contention is per order item and already serialized by that
lock. Period balances stay with the asynchronous projector.

## As built

### Clearing dates (review:T75c, po:2026-11-12, no migration)

- **Rule** (product owner 2026-11-12, as recommended): a clearing is not
  dated before the items it clears. The clearing date, which is the
  clearing entry's posting date, is on or after the latest posting date
  of the items; the day itself is allowed. A request that would break it
  is refused, 422 `ledger.clearing_before_item`, with a field error
  (`before_item`) naming the request's own date field and giving the
  latest posting date.
- **One check.** `notBeforeItems` (`internal/ledger/clearing.go`) on the
  items `loadItems` read under the entries' locks, which now carry each
  line's posting date. Each path that posts a clearing entry calls it
  after its own item checks, so a refusal never states the posting date
  of another company's line, and before it posts:
  - manual clearing, `POST /v1/clearings` (`createClearing`):
    `clearing_date`;
  - settlement (`Postings.Settle`, a payment run's payments; there is no
    other settlement): `posting_date`, the run date;
  - a receipt applied (`Postings.ApplyReceipt`, `POST
    /v1/incoming-payments`): `posting_date`; the receipt's own line
    counts as an item;
  - a bank statement line that clears payments (`Postings.PostBank`,
    `POST /v1/bank-statements/{id}/lines/{line_no}:post`):
    `posting_date`;
  - a down payment clearing (`Postings.ClearDownPayments`, `POST
    /v1/supplier-invoices/{id}:clearDownPayments`): `posting_date`.
- **Payment runs** (decided by default). A run's proposal is an
  operation, so no request could be refused for an item the proposal
  would pick; and a payment refused at the release would stop a run
  whose items are already reserved. The proposal therefore leaves out an
  open item posted after the run date (`ledger.PayableItem.PostingDate`,
  `payments` `candidates`), as it leaves out an item not yet due; a later
  run pays it. The ledger's check in `Settle` stays as the rule's
  backstop.
- **Bank statement lines** (decided by default). A line booked before
  the payment it settles was posted is refused on its booking date and
  is posted with a `posting_date` from the payment's posting date on and
  a reason, the way a line is taken out of a locked period. A matched
  line the rules post on import stays `matched` when the ledger refuses
  it, as for any other refusal. A line waiting for approval is checked
  when the approval posts it, like the ledger's other posting checks
  (ADR-0047).
- **Exempt.** A migrated clearing (`Postings.Clear`, `internal/integration`
  documents mode) posts no entry and records the clearing with the
  source system's clearing document and its posting date; historical
  clearings keep their dates, whatever the items' (whether SAP itself
  always holds a clearing's date at or after its items' posting dates is
  not established here; nothing depends on it). A reversal clears its
  pair on the reversal's posting date (`clearReversal`), which the caller
  may set before the original's: that is a posting rule of reversals,
  not decided here.
- **No database rule** (decided by default). A check on
  `ledger.clearing_item` against the line's posting date would refuse
  those two cases, which exist in loaded and posted data, and telling
  them apart in a trigger would copy the application's knowledge of
  which path wrote the row. The rule is the ledger's, on the one way
  into each posting path.
- **Tests.** `TestClearingDate` (each path a day before the latest item
  refused naming its field, the day itself accepted; the migrated
  clearing and the reversal unchanged), `TestClearDownPayments`,
  `TestRunLeavesOutItemsPostedAfterItsDate` (payments), and the incoming
  payment's refusal through the API (receivables).

