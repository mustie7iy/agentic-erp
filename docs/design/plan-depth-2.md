# Depth plan 2: second variant slices (17 November 2026)

**Status:** plan (product owner, 16–17 November 2026: breadth is built;
the variants go on, pilot-relevant first). It follows
`plan-depth-1.md`, whose rules it does not repeat: each lane splits its
variants into slices in the form of `plan-breadth-week3.md` (Adds,
Acceptance, Later) and builds them. `docs/tracker.json` stays the only
plan (07 §7.5); each leaf's `r` names its section below. Each lane writes
its own `## <lane>` section.

## rev

Revenue and sales: the revenue contract following its sales order's
change (T81e, the leaf `plan-depth-1.md` §T78d left for later; ADR-0068)
and "later: sales order variants" (`plan-breadth-week2.md` §T78b, Later;
ADR-0067): adding and removing items of an open order and changing its
payment term. SAP S/4HANA is the reference: Revenue Accounting (RAR)
takes an order's changes from sales as revenue accounting items, after
the change and not in its transaction, and changes the contract's
obligations and their allocation; VA02 adds items to an open order,
deletes an item that has no follow-on document, and changes the header's
terms of payment, each kept in the change documents.

### T81e Revenue contract follows its sales order

**Capability.** A revenue accountant finds a confirmed order's contract
following the order's change by itself: changed quantities or prices
update the obligations' order values and the transaction price, which an
active contract reallocates; a cancelled order ends its contract's
obligations. A sales representative changes or cancels a confirmed order
whatever its contract (T78d's refusal is lifted).

- **Reuses:** T81a's consumer of `SalesOrderChanged` (`revenue.Consumer`,
  the inbox, `BuilderActor`), the order read under a share lock
  (`sales.Orders.Shared`), T81b's `allocate` with `money.Allocate`, the
  contract's row lock, audit, outbox, `RevenueContractChanged`.
- **Adds:** migration: on `revenue.contract` `changes` (the follows kept)
  and `ended_at`; on `revenue.performance_obligation` `ended_at`; table
  `revenue.contract_change` (insert-only: change number, cause, the
  order's version and change count, transaction price before and after,
  per obligation its order value and allocated amount before and after
  and whether it ended); the contract's rules admit a follow of an
  active contract only with its kept change, its obligations' judgements
  untouched and its shares still the transaction price. The consumer
  follows the order on every `SalesOrderChanged` of a confirmed or
  cancelled order that has a contract: the order as it is now, so a
  follow is idempotent and the events' order does not matter. The
  contract's `changes` and `ended_at`, the obligation's `ended_at` in its
  representation and event; an ended contract refuses a change and an
  activation (409 `revenue.contract_ended`). Sales drops
  `withoutContract` and its port `RevenueContracts`.
- **Walkthrough:** `step_revenue_follows_order`: an order is confirmed and
  its contract appears; its price is changed and the draft contract
  follows (order value, transaction price); the contract is activated;
  the quantity is changed and the active contract is reallocated; a
  second confirmed order is cancelled and its contract ends, its
  transaction price zero; the ended contract refuses an activation.
- **Acceptance:** a confirmed order's change of quantity or price is
  followed by its contract, draft or active: each obligation's order
  value is its item's net value, the transaction price the order's net
  value, and an active contract's shares are the new price allocated by
  the stand-alone selling prices exactly (they add up to it to the minor
  unit); the stand-alone selling prices, timings, service periods and
  days named stay the person's; a cancelled order ends every obligation,
  the transaction price and every share zero; an event read twice, out of
  order or for a change that leaves the order's values as they were
  changes nothing; each follow is numbered and kept with its values
  before and after, audited and published; what earlier recognition runs
  posted stands and the next run posts the difference in its own period
  (prospective: no period already run changes); T78d's 409
  `sales.revenue_contract_exists` is gone.
- **Depends on:** T81a–T81d, T78d (in `main`).

### T78e Sales order items added and removed, payment term changed

**Capability.** A sales representative adds items to an open sales order,
removes an item of which nothing is delivered or billed, and changes the
order's payment term, each in a numbered change; a confirmed order's
contract follows (T81e).

- **Reuses:** T78d's `:change` (row lock, `If-Match`, idempotency, the
  kept change, the credit check of an increase), T78a's item check and
  pricing (`itemCheck.item`), the order's payment term check
  (`paymentTerm`), T81e's follow.
- **Adds:** on `:change` `new_items` (items as `createSalesOrder` takes
  them), `removed_items` (item numbers) and `payment_term_code`;
  migration: on `sales.sales_order_item` `removed_in_change` (the change
  that removed it: the item stays, as business data is never deleted,
  with its values, out of the order's net value, its delivery and
  billing, its open value in credit exposure and the sales lines); a new
  item numbered after the order's highest, inserted only with a kept
  change; on `sales.sales_order_change` the payment term before and
  after and each item's `change` (`changed`, `added`, `removed`); the
  item's `removed_in_change` in the order's representation. Revenue: a
  new item adds an obligation (point in time, its net value as its
  stand-alone selling price), a removed item ends its obligation.
- **Walkthrough:** `step_sales_order_items`: an order is confirmed; an
  item is added and another removed in one change, and the payment term
  is changed; the change reads back with each item's kind; the removed
  item is not delivered (422); the order's contract has an obligation for
  the new item and the removed item's ended.
- **Acceptance:** an item is added to a draft or confirmed order, checked
  and priced as on creation, numbered after the highest; an item is
  removed only with nothing delivered or billed (422
  `sales.order_item_delivered` naming `removed_items[i]`), once (422
  `sales.order_item_removed`), and not the order's last item (422
  `sales.order_needs_item`); a removed item is not changed, delivered or
  billed; the order's net value is its remaining items' exactly; a raised
  confirmed order is checked for credit as T78d's change is; the payment
  term is one the company knows (T78b's check) and changes only while
  nothing of the order is billed (422 `sales.order_billed`), since
  billing reads the order's term; each change is numbered with its
  values before and after; a stale `If-Match` is 412; audited and
  published; nothing is posted.
- **Depends on:** T81e.

**Decided by default (rev):** (1) the contract follows the order through
the event (ADR-0068 §4's mechanism, SAP RAR's after-the-fact processing),
not in the change's transaction, so sales still does not know of revenue
and loses its port; (2) a follow takes the order as it is when the event
is read, so it is idempotent per state; (3) a change of an item's
quantity or price is a change of the transaction price (IFRS 15 as the
plan takes it, not confirmed): the new price is allocated by the
stand-alone selling prices set at inception, which stay the person's; a
new obligation's starts as its item's net value, as at creation; (4)
"prospectively for what is already recognised": what recognition runs
posted is never reversed or restated; the schedule is computed from the
contract as it now is and the next run posts the change in its own
period, the cumulative catch-up the run already makes for a late
delivery (ADR-0068 §9); (5) a cancelled order, and a removed item, end
their obligations with nothing allocated: nothing of them is delivered
or billed, so what earlier runs recognised over time or on a named day is
taken off by the next run and no contract asset is left that no billing
can clear (the safer money rule; listed for the accounting specialist);
(6) an ended contract (its order cancelled) takes no change or
activation; a draft one is never activated; (7) a removed item stays on
the order, marked with its change, since the application deletes no
business data (000011); (8) the payment term changes only while nothing
of the order is billed, since a billing document reads its order's term.

**Later for these variants:** a contract modification treated as a
separate contract or as the termination of the old one and the creation
of a new one (IFRS 15 §18–21, as the specialist answers); a stand-alone
selling price estimated again on a change; a list of a contract's
follows by API; cancelling an order with deliveries (a reason for
rejection per item); changing the customer or the currency; schedule
lines; a payment term per billing document.
