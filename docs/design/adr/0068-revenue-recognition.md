# ADR-0068: Revenue recognition

**Status:** accepted (T81a, 2026-11-12; decided by default under ADR-0053 d,
with the product owner's decisions of 2026-11-12; `go-reviewer` had no
blocking finding on T81a's last commit). It decides the revenue context
and the slices T81a-T81d (`docs/design/plan-breadth-week2.md`, Lane B).
Each slice records what it built under "As built"; T81a's is there
now. **The six statements about IFRS 15 this ADR rests on (§2) are
not yet confirmed by the accounting specialist.**

## Context

The product owner asked for a first usable version of every capability in
scope by 2026-12-01, the main path only (2026-11-10, breadth first). The
tracker's group Finance › Revenue recognition (IFRS 15) has three
placeholders: Revenue contracts and performance obligations, Price
allocation and Recognition schedules. `plan-breadth-week2.md` splits them
into the slices T81a (the revenue contract with its performance
obligations), T81b (price allocation), T81c (the recognition schedule)
and T81d (the recognition run posted), and asks T81a to write this ADR.

What the design already settles:

- The sales context holds the sales order, its items with their net
  values, the confirmations of their deliveries and their billing
  (ADR-0067). `sales.Orders` is its published read of an order, "what
  revenue recognition reads" (ADR-0067 §5). Sales documents carry net
  amounts: tax is calculated at billing (ADR-0067 §3).
- A leaf context reads others through ports and nothing imports it
  (ADR-0051 §2, as `cash`, `assets`, `closing`, `consolidation`,
  `einvoicing` and `collections` are built).
- A context reacts to another's change through that context's events:
  written to its outbox in the change's transaction, relayed to the
  event bus, delivered at least once, and "every consumer is idempotent
  (it records processed event ids in its own `inbox` table)" (ADR-0007).
  The webhook fan-out is such a consumer (ADR-0041,
  `integration.Fanout`).
- ADR-0003 (RLS), ADR-0005 (money in minor units, `money.Allocate` for
  shares that add up), ADR-0008 (state guards), ADR-0011 (audit),
  ADR-0053 (slices, headless first). Events name a business partner by id
  and number, never by name (product owner, 2026-11-05).

The product owner decided on 2026-11-12:

- Revenue recognition is built now on the plan's six statements about
  IFRS 15 as they are written, although they are not confirmed against
  the standard's text. This ADR lists each with its paragraph and its
  status, and says that the person makes every judgement while the
  system only does the arithmetic. Nothing else about IFRS 15 is added.
- The plan's default 11 is replaced: **every confirmed sales order gets
  a revenue contract automatically**, one obligation per order item,
  satisfied at a point in time, with the item's net value as its
  stand-alone selling price, so that the recognition run (T81d) accrues
  what is delivered and not yet billed. A revenue accountant then edits
  the contract where the order is not that simple. The plan's
  `POST /v1/revenue-contracts` stays as the explicit way to create the
  contract of a confirmed order that has none yet.
- The delivery confirmation is the default evidence that control passed:
  a planning default until the accounting specialist answers the plan's
  question 2, not a confirmed reading of the standard.

## Decision

### 1. A leaf context `revenue`

A new bounded context `revenue` (schema `revenue`, package
`internal/revenue`) with its own outbox and inbox, registered in
`outbox.Contexts`. It is a leaf (`internal/layout`): no context imports
it, the sales context least of all. It reads

- the sales context's orders (`sales.Orders.Order`: the order, its items
  with their net values and, from T81c, their deliveries and billing),
- the partner context's numbers and names (`partner.Payees.Names`),
- tenancy's legal entities and, from T81c, the ledger's calendar

through ports it declares, which the composition root wires
(`app.Revenue`). It imports those contexts' published types
(`sales.Order`, `partner.Named`, `tenancy.Entity`), as collections does
(ADR-0065 §1). It posts nothing before T81d, which posts through
`ledger.Postings` with `source_context` `revenue`.

### 2. What the standard is taken to say, and who judges

The primary source is IFRS 15 *Revenue from Contracts with Customers*
(IASB). **Every statement below is from memory**: no lane has network
access and the repository holds no copy of the standard, so none was
checked against its text. The paragraph numbers are from memory too. The
accounting specialist was asked to confirm each
(`plan-breadth-week2.md`, question 1; asked on 2026-11-12).

| # | What the standard is taken to say | Paragraph (from memory) | Status |
|---|---|---|---|
| 1 | Revenue is recognized when, or as, a performance obligation is satisfied by transferring a promised good or service to the customer, which happens when the customer obtains control of it. | 31 | not yet confirmed by the accounting specialist |
| 2 | At contract inception the entity determines, for each performance obligation, whether it is satisfied over time or at a point in time. | 32 (the criteria for over time: 35; the indicators for a point in time: 38) | not yet confirmed by the accounting specialist |
| 3 | The transaction price is the consideration the entity expects to be entitled to, excluding amounts collected on behalf of third parties. The plan takes sales tax to be such an amount. | 47 | not yet confirmed by the accounting specialist |
| 4 | The transaction price is allocated to the performance obligations in proportion to their stand-alone selling prices at contract inception. | 73, 74, 76 | not yet confirmed by the accounting specialist |
| 5 | For an obligation satisfied over time, revenue follows a measure of progress; time elapsed is an input measure, and recognizing on a straight-line basis may be appropriate when the entity's efforts are expended evenly over the period. | 39 to 41, B18 | not yet confirmed by the accounting specialist |
| 6 | A contract is presented as a contract liability when the customer has paid, or payment is due, before the entity transfers the good or service, and as a contract asset when the entity has transferred it before payment is due, apart from amounts presented as a receivable. | 105 to 107 | not yet confirmed by the accounting specialist |

Nothing else about IFRS 15 is assumed. A slice that finds it needs a rule
these six statements and the plan do not give stops that part and
reports it (BRIEF-RULES rule 8); it does not add one.

**The person makes every judgement; the system only does the
arithmetic.**

- The **person** identifies the contract and its performance
  obligations, decides for each whether it is satisfied at a point in
  time or over time, with its service period, and enters its
  stand-alone selling price. The system does not judge whether an item
  is distinct, whether the criteria for over time are met, or what a
  stand-alone selling price is. What a contract starts with (§3) is a
  starting point taken from the order, not a judgement: it stays the
  person's to change before the contract is activated.
- The **system** does the arithmetic and no more: the allocation of
  statement 4 (T81b, §7); for a point-in-time obligation, revenue in
  proportion to the quantity confirmed as delivered, and for an
  over-time obligation the straight line of statement 5 by days (T81c,
  §8); the contract's one position, revenue recognized less revenue
  billed, as a contract asset or a contract liability (statement 6,
  T81d, §9).

### 3. The revenue contract and its performance obligations (T81a)

A revenue contract (`revenue.contract`) stands for **one confirmed sales
order** in its company's **local currency**, and an order has at most one
(`contract_one_per_sales_order`). It names its company, its customer and
its order, by id and by the order's document number. Its **transaction
price is the order's net value**: the order carries net amounts, so the
price is without sales tax (statement 3). It is created `draft`.

It has one performance obligation (`revenue.performance_obligation`) per
order item, numbered in the items' order, each naming its order item and
carrying the item's key, description and net value. An item's net value
is read from the order and never computed from its quantity and price
(prices and discounts are the sales context's). Each obligation starts
as satisfied at a **point in time** with the **order item's net value as
its stand-alone selling price**.

No contract is made for an order that is not confirmed or is in another
currency than its company's local currency (contracts in a foreign
currency are left for later, as the plan says); such an order is
recognized as it is billed (ADR-0067 §9).

### 4. Every confirmed order gets its contract: how

The revenue context consumes the sales context's `SalesOrderChanged`
event (ADR-0007):

1. Confirming an order writes `SalesOrderChanged` (`status` `confirmed`)
   to `sales.outbox` in the confirmation's transaction, as it did before
   this ADR. The sales context is not changed and does not know of
   revenue.
2. The outbox relay publishes it to the event bus; a durable consumer,
   `revenue-contracts`, on the sales context's stream hands it to
   `revenue.Consumer`, at least once.
3. The consumer, in one transaction under the event's tenant, records
   the event's id in `revenue.inbox` and creates the order's contract as
   §3 describes, as the system's principal `revenue.BuilderActor`
   (`created_by`; the audit record says `source`
   `sales_order_confirmed`). The person who confirmed the order did not
   ask for a revenue contract and needs no revenue permission.

**Idempotent per order.** An event read twice finds its id in the inbox
and does nothing. An event published again under another id, and a
person's request for the same order, meet the unique rule: the insert
leaves an existing contract as it is (`ON CONFLICT … DO NOTHING`, which
also waits for a contract another transaction is creating), so an order
has exactly one contract and one `RevenueContractChanged` for its
creation, whichever path created it.

**What is acknowledged without a contract.** An event that is malformed,
or whose subject names another tenant than its `tenantid`, is refused and
logged, as the webhook fan-out does. An order no contract is made for
(§3: a foreign currency) is logged and its event recorded as read. A
database failure is an error: the event is read again.

**The explicit request.** `POST /v1/revenue-contracts` (sales order)
creates the same contract on a person's request, for a confirmed order
that has none yet: an order confirmed before the consumer first ran (a
new durable consumer starts at the events published after it), or whose
event has not been read yet. It is idempotent by its `Idempotency-Key`;
an order that has its contract is refused (409
`revenue.contract_exists`).

**Why the event, and not the alternatives.** The sales context cannot
call revenue: revenue is a leaf, and a call would turn the direction
around. Creating the contract on first read, or at the recognition run,
would make a read write, leave a confirmed order without a contract for
a revenue accountant to edit until something happens to read it, and
make "which orders have no contract" a question every list has to
answer. The event is the mechanism ADR-0007 gives for exactly this, the
relay and the consumer exist (ADR-0041 uses them), and the delay is the
relay's and the consumer's polling, a second or two.

### 5. Events and audit

Every change of a contract's status writes an audit record (ADR-0011)
and publishes `RevenueContractChanged` in the same transaction
(ADR-0007): the contract, its order by id and number, its company by id
and code, its customer by id and number, never by name, its transaction
price, the number of its obligations, its status and the status before.
It is offered to webhooks. T81d adds `RevenueRecognized`.

### 6. Permissions and agents

`revenue.contract.read` and `revenue.contract.write` (create a contract;
from T81b, set its obligations' timing and stand-alone selling prices),
`revenue.contract.activate` (T81b) and `revenue.recognition.run` (T81d).
Reads follow the caller's company access; a contract of another company
is not found. Every write is human-only (`x-agent: false`): they record
a person's judgement (§2).

### 7. Price allocation (T81b)

While a contract is a draft, the revenue accountant sets per obligation
its timing, `point_in_time` or `over_time`, an over-time obligation's
service period (from not after to) and its stand-alone selling price,
more than zero, in the contract's currency. Activating the contract
allocates the transaction price to the obligations **in proportion to
their stand-alone selling prices** (statement 4) with `money.Allocate`:
each obligation's exact share is the transaction price × its price ÷ the
sum of the prices, the shares are rounded so that they add up to the
transaction price to the minor unit, and a remaining minor unit goes to
the obligations with the largest remainders, the earlier one first, so
the same contract always allocates the same way. Activation moves
`draft` to `active` once and fixes the obligations and their amounts.
With the starting prices, the items' net values, the allocated amounts
are the order's item values.

### 8. The recognition schedule (T81c)

Computed on read from the active contract's obligations: a
point-in-time obligation's allocated amount falls in the fiscal periods
of its order item's delivery confirmations, in proportion to the
quantity delivered, the last delivery taking the remainder (the delivery
confirmation is the default evidence that control passed, a planning
default of the product owner, 2026-11-12, until the accounting
specialist answers question 2; statement 1); an over-time obligation's amount is spread on
a straight line by days over its service period, each fiscal period
taking its days' share (statement 5). T81c records the detail when it
builds it.

### 9. The recognition run (T81d)

Per company and period, each active contract is brought to its
cumulative position: revenue recognized to the period's end less revenue
billed, as one contract asset when it is positive or one contract
liability when it is negative, never both (statement 6), posted through
`ledger.Postings`. T81d records the detail when it builds it.

## Alternatives

- **The sales context creates the contract when it confirms an order.**
  One transaction and no delay, but sales would import revenue, or need a
  port to it: a leaf would gain a caller, against ADR-0051 §2.
- **The contract created on first read or by the recognition run.** No
  consumer, but a read that writes, and no contract to edit before then
  (§4).
- **Contracts only on a person's request** (the plan's default 11). An
  order without one is recognized as billed, so a delivery not yet
  billed at a period end is not accrued; replaced by the product owner
  on 2026-11-12.
- **The contract acts as the person who confirmed the order.** Their
  name on a document they did not ask for, under a permission they do
  not hold; the system's principal says what happened.
- **The schedule stored at activation.** It would go stale with each
  delivery; computed on read, it is always the deliveries' (T81c).

## Consequences

- A confirmed order's contract appears a moment after the confirmation,
  not in its transaction. Without an event bus (`ERP_NATS_URL` unset)
  events stay in the outbox and no contract is created; the explicit
  request works meanwhile. `erp-server` creates the durable consumer
  before it starts the relay, so the events still in the outbox when
  the consumer first exists are delivered to it. Events relayed before
  the consumer first existed (by a server without this slice) are not:
  a new durable consumer starts at the events published after it.
- Orders confirmed before this slice was deployed have no contract until
  a person asks for one; nothing lists them yet (see "Gaps").
- The revenue context depends on the shape of `SalesOrderChanged`
  (`sales_order_id`, `status`), which the event contract fixes.
- The arithmetic is only as right as the six statements: each is
  unconfirmed, and a correction by the accounting specialist changes §7
  to §9, not the judgements, which are the person's.

## Defaults chosen (for the product owner's digest)

1. The automatic contract is created by the revenue context's consumer of
   `SalesOrderChanged`, as the system's principal, once per order (§4).
2. An order in a foreign currency gets no contract, automatically or on
   request (the plan leaves foreign-currency contracts for later); it is
   recognized as billed.
3. A request for an order that has its contract is refused (409), not
   answered with the existing one: the plan's "a second is refused".
4. A contract has no document number of its own; it is found by its
   sales order's number.
5. An obligation keeps the order item's key, description and net value as
   they were when the contract was created (orders do not change in the
   first version).
6. A remaining minor unit of the allocation goes to the largest
   remainder, the earlier obligation first (`money.Allocate`'s rule, §7).

## Questions

The plan's questions 1 to 8 stand (`plan-breadth-week2.md`), asked of
the accounting specialist on 2026-11-12; none blocks T81a or T81b.
Question 4 (should every confirmed order get a contract automatically)
is answered by the product owner: yes.

## Gaps (not built; for the product owner)

- No read lists confirmed orders without a revenue contract (those
  confirmed before the consumer ran, or in a foreign currency).
- An event the consumer refuses is only logged; there is no dead-letter
  record of it.

## As built

### T81a Revenue contract with its performance obligations (migration 000280)

- **Schema.** `revenue` with its outbox and inbox (`outbox.Contexts`),
  `revenue.contract` and `revenue.performance_obligation` under RLS,
  `tenant_id` first. One contract per sales order
  (`contract_one_per_sales_order`); status `draft` alone
  (`contract_status_check`, which T81b widens); a transaction price above
  zero; an obligation per order item
  (`performance_obligation_one_per_order_item`) with the item's key,
  description and net value, a timing of `point_in_time` or `over_time`
  and a stand-alone selling price above zero. The application may not
  update or delete either table (T81b grants what it sets). Schema tests
  in `db/revenue_test.go`.
- **The context.** `internal/revenue`, a leaf in `internal/layout`
  (imports `partner`, `sales`, `tenancy`; nothing imports it), wired by
  `app.Revenue`: `sales.Orders`, `tenancy.Directory`, `partner.Payees`.
  The sales context is untouched.
- **The automatic contract** (§4). `revenue.Consumer.Handle` on the
  durable consumer `revenue-contracts` over the sales stream, started by
  `erp-server` beside the webhook fan-out, both before the relay
  (review round 1: a new durable consumer is given only what is
  published after it, so the relay must not publish the outbox's backlog
  first; the ordering has no test of its own, since the test database's
  outboxes are relayed by other packages' tests too). It acts on
  `com.example.erp.sales.sales_order_changed.v1` with `status`
  `confirmed`; it records the event in `revenue.inbox` (consumer
  `revenue-contracts`) and creates the contract in one transaction
  (`uow.Job.As`) as `revenue.BuilderActor`, a system principal. The
  event's tenant is the subject's, which must be its `tenantid`
  (`outbox.Message.Tenant`, which the webhook fan-out now shares).
  Tested: the event read twice and published again under another id
  leave one contract and one `RevenueContractChanged`; a request racing
  the event ends with one contract (201 or 409 for the request); the
  begin, each statement and the commit failing leave nothing and the
  event read again creates the contract.
- **The request.** `POST /v1/revenue-contracts` (`sales_order_id`;
  `revenue.contract.write`; human-only; idempotent by key): 422
  `request.reference_not_found` naming `sales_order_id` for an order that
  is not there or of a company the caller may not act in, 422
  `revenue.order_not_confirmed`, 422 `revenue.foreign_currency`, 409
  `revenue.contract_exists` for an order that has its contract.
- **Reads.** `GET /v1/revenue-contracts` (company, customer, sales order,
  status; cursor paging by `sales_order_number`) and
  `GET /v1/revenue-contracts/{id}` (`revenue.contract.read`): the
  company's code and name, the customer's number and name, the order by
  id and number, every amount with its currency; a contract of a company
  the caller may not act in is not found.
- **Event and audit.** `RevenueContractChanged` (`x-webhook`) and the
  audit record `revenue.contract.create`, whose `source` is `request` or
  `sales_order_confirmed`, in the creation's transaction. Nothing is
  posted.
- **Walkthrough.** Step 57, `step_revenue_contract`: a second order of
  two items, goods and a support service, is confirmed; its contract
  appears by itself and reads back with two obligations and the order's
  net value, 2700.00 CAD, as its transaction price; a request for the
  same order is refused (409), as is one for a draft order (422).
- **Decided by default while building.** The explicit request cannot be
  shown succeeding in the walkthrough: there every confirmed order has
  its contract within a second, so the step shows the automatic contract
  and the request's refusal, and the tests show the request creating
  one. An obligation's number is its place among the order's items, and
  `order_item_no` names the item: they are equal today and kept apart
  for obligations that are not order items (the plan's "later").
