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
