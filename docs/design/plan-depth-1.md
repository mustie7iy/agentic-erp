# Depth plan 1: first variant slices (from 17 November 2026)

**Status:** plan (product owner, 2026-11-16 and 2026-11-17: breadth is
built; the variants start, pilot-relevant first). Each lane splits the
"later: …" leaf it takes into slices in its own section below, in the form
of `plan-breadth-week3.md` (Adds, Acceptance, Later), and narrows the
leaf to what remains. `docs/tracker.json` stays the only plan (07 §7.5);
each new leaf's `r` names its section here.

## orders

Controlling, internal orders (ADR-0064). Two of the "later" leaves of
`plan-breadth-week1.md` are taken: the order budget variants (§T60c,
Later) and the order settlement variants (§T60e, Later). SAP S/4HANA's
budgeting of internal orders (budget supplement KO24 and return KO25,
availability control with tolerance limits in the budget profile) and its
settlement reversal (KO88 in reversal mode) are the reference, stated
from memory and marked so in ADR-0064's "As built".

### T60h Budget supplements and returns, and availability control

**Capability.** A controller supplements an order's budget or returns
part of it, each change kept as a version with its amounts before and
after, and gives the budget an availability control: a warning limit and
a refusal limit, each a percentage of the budget; a posting to the order
that takes its consumption above the refusal limit is refused, and one
above the warning limit is accepted and published as a warning.

- **Reuses:** T60c's budget (`controlling.order_budget`, its version and
  `If-Match`, the order held `FOR UPDATE`); the ledger's port
  `ledger.CostObjects`, which every posting naming an order already asks
  (T60b); `ledger.AssignmentBalances.OrderBalance` (the actual); audit;
  the controlling outbox.
- **Adds:** columns `warn_percent` and `refuse_percent` on
  `controlling.order_budget`; table `controlling.order_budget_change`
  (insert-only, one row per version of a budget from this slice on:
  `set`, `supplement`, `return`); `POST
  /v1/internal-orders/{id}/budget-changes` (a supplement or a return, with
  the budget's version), `GET /v1/internal-orders/{id}/budget-changes`;
  `availability_control` in the budget's `PATCH` (its `budget` becomes
  optional) and its read, with the consumption in percent and the state
  against the limits; a second method of `ledger.CostObjects`,
  `Consume`, asked by the ledger with the amount a posting adds to each
  order; the refusal reason `budget_exceeded` of 422
  `ledger.internal_order_invalid`; events `InternalOrderBudgetChanged`
  (now with the change's kind) and `InternalOrderBudgetWarned`.
- **Walkthrough:** `step_order_budget` extended: Carla supplements the
  trade fair's budget and returns part of it, gives it a warning limit
  and a refusal limit, reads the budget's versions; Alex's posting that
  would take the order above the refusal limit is refused.
- **Acceptance:** a supplement or return needs a budget set, a positive
  amount in the company's currency, and the budget's current version
  (412 otherwise); a return never takes the budget below zero or below
  the order's actual costs (409); every change of the budget (set,
  supplement, return, limits) is one version, one row of the history,
  audited with the amounts before and after and published; the limits
  are whole percentages from 1 to 999, the warning limit below the
  refusal limit, each optional; a posting that adds to an order's
  consumption (its debit on the order exceeds its credit) is refused when
  the consumption after it is above the refusal limit, and accepted with
  `InternalOrderBudgetWarned` when above the warning limit; a posting
  that lowers the consumption (a settlement), a reversal and an order
  without limits are never checked; two postings to one order are
  checked one after the other (the budget row held `FOR UPDATE` after the
  order's `FOR SHARE`), so together they never pass a limit that each
  would pass alone; schema tests for every new rule.
- **Depends on:** T60c.

**Later:** budgets per year; transfers between orders; commitments
(purchase requisitions and orders on an order) counted in the
consumption once orders on such items exist; tolerance limits by
activity group and kept in a budget profile per order type; approval of
budget changes; a warning returned on the posting's own answer.

### T60i Settlement reversed

**Capability.** A controller reverses an order's latest settlement: its
entry is reversed in the settled period, so the order's balance is
restored and the receiver's cost taken back, and the order can be
settled again.

- **Reuses:** `ledger.Postings.Reverse` (the ledger's reversal, which
  copies the lines' order and cost center and holds a closed order back,
  T60e); T60e's settlement, its lock and its period rule; the ledger's
  entry locks (`lockEntries`), through a new `ledger.Postings.LockEntries`.
- **Adds:** table `controlling.order_settlement_reversal` (insert-only,
  one per settlement); `POST
  /v1/internal-orders/{id}/settlements/{settlement_id}:reverse`; the
  reversal on `InternalOrderSettlement`; event
  `InternalOrderSettlementReversed`; the ledger's `:reverse` of a
  settlement's entry names this path instead.
- **Walkthrough:** `step_order_settlement` extended: Carla reverses the
  trade fair's settlement; the order holds its cost again and the cost
  center's report gives it back; she settles it again and closes it.
- **Acceptance:** only a released order's latest settlement that is not
  reversed is reversed (409 otherwise), at the order's version; the
  reversal entry is dated the settlement's own posting date, so the
  settled period must still be open (the ledger's 409); the order's
  balance per account is what it was before the settlement; the
  reversed settlement no longer bounds the period of the next
  settlement; a reversal is kept, audited and published, once; the locks
  are taken entry bucket first, then the order, as the ledger's own
  reversal of an entry naming the order takes them; schema tests.
- **Depends on:** T60e.

**Later:** settling many orders in one run (and reversing a run);
settlement to assets under construction and G/L accounts; settlement
through a settlement cost element; settlement as a close task; reversing
into a later open period when the settled one is closed.
