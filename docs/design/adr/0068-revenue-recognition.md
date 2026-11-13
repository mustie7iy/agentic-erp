# ADR-0068: Revenue recognition

**Status:** accepted (T81a, 2026-11-12; decided by default under ADR-0053 d,
with the product owner's decisions of 2026-11-12; `go-reviewer` had no
blocking finding on T81a's last commit). It decides the revenue context
and the slices T81a-T81d (`docs/design/plan-breadth-week2.md`, Lane B).
Each slice records what it built under "As built"; all four are there. **The six statements about IFRS 15 this ADR rests on (§2) are
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
  (`ledger.Calendar`: a company's regular fiscal periods) and the
  active configuration (a company's time zone, for its date today)

through ports it declares, which the composition root wires
(`app.Revenue`). It imports those contexts' published types
(`sales.Order`, `partner.Named`, `tenancy.Entity`,
`ledger.FiscalPeriod`, `config.Config`), as collections does
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

Computed on read from the active contract's obligations, as activation
fixed them, and from the delivery confirmations of its sales order as
they are at the read; nothing of it is stored. A draft has no allocated
amounts and no schedule.

- **A point-in-time obligation**'s allocated amount falls in the fiscal
  periods of its order item's delivery confirmations, in proportion to
  the quantity delivered: after each delivery, what is scheduled so far
  is the allocated amount × the quantity delivered so far ÷ the quantity
  ordered, rounded to the minor unit, and the delivery takes what that
  adds. The delivery that completes the item therefore takes the
  remainder, and the amounts add up to the allocated amount exactly;
  billing shares out an item's value the same way (ADR-0067 §9). What
  is not delivered is **not yet scheduled**, in no period. The delivery
  confirmation is the default evidence that control passed (statement
  1): a planning default of the product owner, 2026-11-12, until the
  accounting specialist answers question 2, not a confirmed reading of
  the standard.
- **The day a person names.** Where the terms of sale make another day
  the one the obligation is satisfied on, the revenue accountant sets
  that day on the obligation (`satisfied_on`) while the contract is a
  draft (product owner, 2026-11-12: "let the revenue accountant set
  another date on the obligation"). The schedule then has the
  obligation's whole allocated amount on that day, whatever was
  delivered. Which day it is is the person's judgement; the system
  checks nothing about it. An over-time obligation has no such day.
- **An over-time obligation**'s allocated amount is spread on a straight
  line by days over its service period, both days included (statement
  5): each fiscal period takes the amount × its days of the service
  period ÷ the service period's days, and `money.Allocate` rounds the
  shares so that they add up to the allocated amount to the minor unit,
  a unit left going to the largest remainder, the earlier period first.
- **Recognized and planned.** The schedule is told to a day: what it
  gives days up to and including that day is recognized, what it gives
  later days is planned. Of an over-time obligation's fiscal period
  under way on that day, the recognized part is the period's amount ×
  its service days so far ÷ its service days, rounded against the rest
  so that the two add up to the period's amount. The read's day is the
  company's date today in its time zone; the recognition run's (§9) is
  its period's last day, so the run recognizes whole fiscal periods.
  "Recognized" in the schedule is what the arithmetic gives to that
  day, not what is posted.
- **Fiscal periods** are the company's regular posting periods
  (`ledger.Calendar`). A day the schedule needs a period for, a
  delivery's, the day a person named or a day of a service period,
  that lies in no fiscal year set up for the company is refused naming
  the obligation (422 `revenue.no_fiscal_period`).

### 9. The recognition run (T81d)

A revenue accountant runs revenue recognition for a company and one of
its regular fiscal periods. The run is arithmetic on what the contracts
and the sales documents say; it judges nothing.

- **What is read.** Every active contract of the company. Per
  obligation: the revenue **recognized** to the period's last day, which
  is the schedule (§8) told to that day, so whole fiscal periods of an
  over-time obligation and the deliveries dated up to it; and the
  revenue **billed** to that day, the net amounts, without tax, of the
  order item in billing documents posted on or before it (ADR-0067 §9).
- **What billing already posted.** A billing document posts as a
  customer invoice that credits each item's revenue account with what
  was billed. So the ledger holds, per obligation, revenue as billed,
  and the run corrects it to revenue as recognized.
- **The entry, per contract.** Per obligation the order item's revenue
  account is moved by recognized less billed, less what earlier runs
  posted for the obligation: a credit when that is positive, a debit
  when negative. The contract's **position** is the sum of its
  obligations' recognized less billed. The entry takes the position the
  last run left off its account and puts the new one on the company's
  **contract asset** account when it is positive (a debit) or on its
  **contract liability** account when it is negative (a credit), so a
  contract shows one of the two and never both (statement 6). The entry
  balances: the position's change is the obligations' movements added
  up. The posted invoice stays the receivable. Output tax is not
  touched: it is reported at the invoice date (ADR-0049 §4).
- **Cumulative, so idempotent per contract and period.** A run keeps,
  per contract it posted, the cumulative amounts per obligation; the
  next run posts only the change. A contract whose recognized less
  billed did not move posts nothing. A delivery or a billing document
  dated into a period already run is caught up by the next run.
- **One run per company and period, in order.** A second run of a
  period is refused; a company's first run is for any open period, each
  later run for a later period than its last. The company's runs are
  held by one lock while a run reads what the runs before it posted, and
  the database holds both rules too. A run is not reversed in the first
  version.
- **Posted as other period-end runs are.** Through `ledger.Postings`
  with `source_context` `revenue`, document type `SA`, dated the
  period's last day, in the company's local currency, to every ledger
  the company keeps; the period must be open (the ledger's period
  lock), also when nothing is to post. The run is one request and one
  transaction: all of it is posted or none, and it reads at most 2,000
  active contracts (`revenue.MaxRunContracts`, as a collection run is
  bounded).
- **The accounts.** `companies[].account_determination.contract_asset`,
  an asset, and `.contract_liability`, a liability, neither a
  reconciliation account; a company without both is refused.
- **Event and audit.** One audit record and one `RevenueRecognized` per
  run, with its counts and totals; not offered to webhooks until a
  consumer asks (product owner, 2026-11-12).

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
7. The schedule's "recognized so far" is what it gives days up to the
   company's date today, and "planned" what it gives later days; a
   fiscal period under way is split by its service days (§8).
8. A point-in-time obligation's share per delivery is rounded on the
   cumulative quantity, as billing rounds an item's value, not with
   `money.Allocate`: the deliveries are not all known when the first is
   scheduled, and an amount already shown for a delivery does not move
   when a later-dated one is confirmed. A delivery confirmed afterwards
   with an earlier date does move the later ones' amounts, by the
   rounding and into the earlier period; the recognition run is
   cumulative and catches that up (§8, §9).
9. The day a person names for a point-in-time obligation is set while
   the contract is a draft, as its other judgements are, and takes the
   obligation's whole amount (§8). The plan listed "recognition at a
   point other than the delivery confirmation" under later; the product
   owner's decision of 2026-11-12 on question 2 brought this one form
   of it forward. Changing it on an active contract, and a day per
   delivery, stay later.
10. The schedule's periods across fiscal years need each year set up in
    the ledger's calendar; a service period reaching into a year that is
    not is refused at the read, not at the change of the obligation or
    at activation, since a year may be set up later (§8).
11. The run reads billing by posting date and recognition by delivery
    date or service day: both are compared at the period's last day (§9).
12. A run's entry is per contract, with a line per obligation that
    moved, and the position's change netted per account: one line on the
    contract asset account and one on the contract liability account at
    most (§9).
13. A contract's lines carry no customer, cost object or tax code: the
    two contract accounts are plain G/L accounts, and what the ledger
    asks of a revenue account (a cost center, say) refuses the run with
    the ledger's code. See "Gaps".
14. Reading runs takes `revenue.contract.read`; the plan names one new
    permission, `revenue.recognition.run` (§6).
15. A run is kept also when no contract moved, with `posting_count` 0:
    it says the period was run, and later runs follow it (§9).
16. The plan's two tables became three: per run and contract the entry
    and the contract's cumulative amounts, and per obligation the
    amounts the next run takes off, which a row per contract cannot
    hold.

## Questions

The plan's questions 1 to 8 stand (`plan-breadth-week2.md`), asked of
the accounting specialist on 2026-11-12; none blocks a slice. T81c and
T81d add, for the accounting specialist (each a planning default until
answered, none read from the standard):

- **Billed means invoiced, not paid.** The run takes a contract
  liability from billing ahead of recognition. Statement 6 speaks of the
  customer having paid or payment being due. Is an invoice posted and
  not yet due a contract liability, or only once it is due or paid?
- **Net per contract across obligations.** One obligation billed ahead
  and another delivered and not billed are netted into one position.
  Confirm that netting is per contract, not per obligation.
- **The asset as "contract asset".** What is delivered and not billed
  is shown as a contract asset. When the right to payment is already
  unconditional (only the invoice is missing), should it be a
  receivable (unbilled receivable) instead?
- **A day named on the obligation** takes the whole allocated amount,
  whatever was delivered. Is a split (part on delivery, part on
  acceptance) needed for the pilot's terms?
- **Rounding.** Shares by days and by quantities are rounded to the
  minor unit as §8 says. Any requirement on where a rounding unit goes?
- **Periods run in order, none reversed.** A correction dated into a
  period already run is recognized in the next run's period. Acceptable
  for the pilot's close, or must the earlier period be reopened?
Question 4 (should every confirmed order get a contract automatically)
is answered by the product owner: yes.

## Gaps (not built; for the product owner)

- A run's lines name no trading partner: the contract asset or
  liability towards an affiliated customer is not marked intercompany
  for consolidation.
- A run that the ledger refuses for one contract (an obligation's
  revenue account that asks for a cost center the line does not carry,
  or is no longer postable) is refused as a whole, with the ledger's
  code and the order's number in the log, not in the problem's detail.
- The year-end carry-forward, the foreign currency valuation and the
  liquidity forecast do not know the two contract accounts as anything
  but G/L accounts.
- A run cannot be reversed, and a company that ran a period cannot run
  an earlier one (the plan's "later").
- One active contract whose schedule reaches a fiscal year that is not
  set up refuses the company's whole run until the year is set up
  (default 10): the straight line's rounding needs every period of the
  service period. Requiring only the periods up to the run's end would
  place the rounding unit later.

- No read lists confirmed orders without a revenue contract (those
  confirmed before the consumer ran, or in a foreign currency).
- An event the consumer refuses is only logged; there is no dead-letter
  record of it.

## As built

### T81a Revenue contract with its performance obligations (migration 000298)

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
- **Walkthrough.** Step 67, `step_revenue_contract`: a second order of
  two items, goods and a support service, is confirmed; its contract
  appears by itself and reads back with two obligations and the order's
  net value, 2700.00 CAD, the entered prices being final (ADR-0067 §11,
  T80c), as its transaction price; a request for the
  same order is refused (409), as is one for a draft order (422).
- **Decided by default while building.** The explicit request cannot be
  shown succeeding in the walkthrough: there every confirmed order has
  its contract within a second, so the step shows the automatic contract
  and the request's refusal, and the tests show the request creating
  one. An obligation's number is its place among the order's items, and
  `order_item_no` names the item: they are equal today and kept apart
  for obligations that are not order items (the plan's "later").

### T81b Price allocation (migration 000299)

- **Schema.** `revenue.performance_obligation` gains `service_from`,
  `service_to` and `allocated_amount`; an over-time obligation has a
  service period, from not after to, and a point-in-time obligation none
  (`performance_obligation_service_period`); `revenue.contract` may be
  `active` (`contract_status_check`). A contract changes only while it
  is a draft, and becomes active only when every obligation holds its
  share and the shares are the transaction price
  (`contract_fixed_once_active`, `contract_allocated_in_full`, the
  trigger `revenue.check_contract_change`); an obligation is written and
  changed only while its contract is a draft
  (`performance_obligation_fixed_once_active`, which locks the
  contract's row for its check). The application may set an obligation's
  timing, service period, stand-alone selling price and allocated amount
  and a contract's status and version, nothing else. Schema tests in
  `db/revenue_test.go`.
- **Setting the obligations.** `PATCH /v1/revenue-contracts/{id}`
  (`application/merge-patch+json`, `If-Match`, `revenue.contract.write`,
  human-only): `obligations` names obligations by number, each once; a
  field left out stays; `service_from` and `service_to` are removed with
  null. Refusals name the request's own field: 422
  `revenue.unknown_obligation`, `revenue.obligation_repeated`,
  `revenue.unknown_timing`,
  `revenue.service_period_required`, `revenue.invalid_service_period`,
  `revenue.service_period_not_allowed`, `revenue.currency_mismatch`,
  `revenue.price_not_positive`; 409 `revenue.invalid_status` once the
  contract is active. The contract is read under its row lock; its
  version moves.
- **Activation.** `POST /v1/revenue-contracts/{id}:activate`
  (`If-Match`, `revenue.contract.activate`, human-only) under the
  contract's row lock: `allocate` computes each obligation's exact share,
  transaction price × stand-alone selling price ÷ the sum of the prices,
  in big integers and rationals, and `money.Allocate` rounds them to
  shares that add up to the transaction price, a unit left going to the
  largest remainder, the earlier obligation first. The shares, the
  status `active`, the audit record and the event are one transaction. A
  second activation and a later change are refused (409
  `revenue.invalid_status`).
- **Tested.** The shares add up to the transaction price to the minor
  unit for 500 fixed contracts and at the top of the amount range; equal
  remainders go to the earlier obligation; the starting prices allocate
  the order's item values (also for the contract an order's confirmation
  created); two activations at once, and an activation and a change at
  once, end with one done and one 412.
- **Event and audit.** `RevenueContractChanged` is published for a
  change of the obligations (`draft`, status_before `draft`) and for the
  activation (`active`, status_before `draft`); audit records
  `revenue.contract.update` and `revenue.contract.activate`.
- **Walkthrough.** Step 67 extended: the support service is set to over
  time for the fiscal year of Maple's date today with a stand-alone
  selling price of 1300.00, above its order value of 1200.00; an
  over-time obligation without a period is refused; the contract is
  activated and its obligations are allocated 1446.43 and 1253.57, which
  add up to 2700.00 and differ from the order's item values; a later
  change and a second activation are refused; the contract of step 63's
  order, activated with its starting prices, is allocated the order's
  item values.
- **Decided by default while building.** The patch's `obligations` is a
  list keyed by `obligation_no`, not a merge patch's replacing array: the
  obligations are the order's items and cannot be replaced. A
  point-in-time obligation with a service period is refused rather than
  the period being dropped silently. An allocated amount may be zero
  (a stand-alone selling price too small for a minor unit of the
  transaction price). The contract's `version` and
  `RevenueContractChanged` move with a change of the obligations, so a
  consumer following the contract sees it.

### T81c Recognition schedule (migration 000310)

- **Schema.** No table: the schedule is computed on read.
  `revenue.performance_obligation` gains `satisfied_on`, the day a
  person says a point-in-time obligation is satisfied on; an over-time
  obligation has none (`performance_obligation_satisfied_on`). The
  application may set it, while the contract is a draft as every
  judgement (`performance_obligation_fixed_once_active`). The revert is
  refused while an obligation names such a day. Schema tests in
  `db/revenue_test.go` (`TestRevenueSatisfiedOn`).
- **Ports.** The context reads the ledger's calendar
  (`revenue.Calendar`: `ledger.Calendar.Period` and `.Periods`) and the
  active configuration (`revenue.Settings`, for the company's date
  today, `config.DocumentDay`), wired by `app.Revenue`; `internal/layout`
  lists `config` and `ledger` among its imports. It stays a leaf.
- **The read.** `GET /v1/revenue-contracts/{id}/schedule`
  (`revenue.contract.read`): the contract's order, company and customer
  by id, number or code, and name; `as_of`, the company's date today;
  per obligation its timing, service period or `satisfied_on`, its
  allocated amount and its `recognized_amount`, `planned_amount` and
  `unscheduled_amount`, which add up to it, and its fiscal periods,
  earliest first, each with its dates, its amount split into recognized
  and planned, an over-time period's `days` and a delivery period's
  `delivered_quantity`; the contract's sums, which add up to the
  transaction price, and its `periods`, the obligations' added up. 409
  `revenue.invalid_status` for a draft; 422 `revenue.no_fiscal_period`
  naming the obligation, the day and the company; 404 for a contract of
  a company the caller may not act in.
- **Setting the day.** `PATCH /v1/revenue-contracts/{id}` takes
  `obligations[].satisfied_on` (null removes it); 422
  `revenue.satisfied_on_not_allowed` on an over-time obligation, naming
  the request's field.
- **The arithmetic** (`internal/revenue/schedule.go`). `straightLine`:
  exact shares amount × days ÷ the service period's days per fiscal
  period, rounded by `money.Allocate`; the period under way split the
  same way. `byDeliveries`: the cumulative share amount × delivered ÷
  ordered rounded half away from zero (`money.Round`), each delivery
  taking the difference. `onDay`: the whole amount on the day named.
  Fiscal years are read from the ledger once each per read.
- **Tested.** A contract of three obligations, goods delivered in
  parts across two periods, a service over the fiscal year and a kit
  delivered later: each obligation's periods and sums at a day in
  February and after the year, the contract's periods and sums; the
  straight line over the fiscal year (1200.00 over 365 days: six cents
  left go to the first six months of 31 days), one day, part of a
  period and from the middle of one period to the middle of another;
  for 300 fixed amounts and service periods of up to three years the
  periods add up to the amount and what is recognized never falls from
  one day to the next; for 200 fixed amounts delivered in up to six
  parts the scheduled and the unscheduled amounts add up after every
  delivery; the day a person names; the refusals; every database
  statement failing.
- **Budget.** None: the plan calls the schedule a read of one contract,
  not a report; it reads the contract, its order and one fiscal year
  per year its obligations touch.
- **Walkthrough.** Step 82, `step_revenue_schedule`: before a delivery
  the brackets of step 67's contract are not yet scheduled and the
  support's 1253.57 is spread over the twelve fiscal periods by their
  days, the days adding up to the year's and the amounts to 1253.57;
  Sally confirms the 120 brackets as delivered and the schedule has
  their 1446.43 in the delivery's period, recognized; a draft has no
  schedule.
- **Decided by default while building.** Defaults 7 to 10 above. The
  schedule shows a period's `days` and `delivered_quantity` so that a
  consumer can check each share without another call. A delivery dated
  after the read's day would be planned; the sales context refuses a
  delivery dated in the future, so today that arises only at a read
  told for an earlier day (the recognition run's).

### T81d Recognition run posted (migrations 000311, 000312)

- **Schema.** `revenue.recognition_run`, `revenue.recognition_posting`
  and `revenue.recognition_posting_obligation` under RLS, `tenant_id`
  first, written once (no update or delete for the application). One
  run per company and period (`recognition_run_once`); each for a later
  period than the company's last (`recognition_run_in_order`, a trigger
  that takes the company's advisory lock); a run's positions are what
  it recognized less what it billed
  (`recognition_run_positions_check`). It keeps what its entries moved
  revenue by, zero when it posted nothing
  (`recognition_run_revenue_check`). The account determination takes
  the keys `contract_asset` and `contract_liability` and the journal
  the source context `revenue`, both checks added NOT VALID in 000311
  and validated in 000312, the journal's partition by partition. The
  revert is refused while a run or one of the two keys exists. Schema
  tests in `db/revenue_test.go` (`TestRevenueRecognitionRules`).
- **Configuration.** `companies[].account_determination.contract_asset`
  (an asset) and `.contract_liability` (a liability), neither a
  reconciliation account (`config` validation), applied by
  `ledger.Setup` and known to country packs' account determination and
  to the automatic accounts' list. No pack ships them.
- **Ports.** `revenue.Postings` (`ledger.Postings.Post`, `.Postable`)
  and `revenue.Accounts` (`ledger.Accounts.Determination`,
  `.AccountNumbers`), wired by `app.Revenue`.
- **The run** (`internal/revenue/run.go`).
  `POST /v1/revenue-recognition-runs` (`legal_entity_id`, `fiscal_year`,
  `fiscal_period`; `revenue.recognition.run`; human-only; idempotent by
  key). In order: the company is one the caller may act in (422
  `request.reference_not_found`); the period is a regular one (422
  `ledger.no_fiscal_period`, `revenue.period_not_regular`); both
  accounts are determined (422 `revenue.no_contract_accounts`); the
  company's lock is taken and the run follows the last (409
  `revenue.recognition_run_exists`,
  `revenue.recognition_period_not_later`); the period is open (409
  `ledger.period_not_open`); at most 2,000 active contracts (422
  `revenue.recognition_run_too_large`); each contract's schedule to the
  period's last day (422 `revenue.no_fiscal_period` naming the order);
  totals within an amount (422 `revenue.amount_too_large`). Then the run
  is kept, each contract that moved posts its entry and keeps its
  figures, and the audit record and the event are written, in one
  transaction.
- **Reads.** `GET /v1/revenue-recognition-runs` (company, fiscal year,
  period; cursor paging by `posting_date`, latest first; without
  postings) and `GET /v1/revenue-recognition-runs/{id}` with the
  postings by sales order number (`revenue.contract.read`): the
  company's code and name, the run's totals and `revenue_amount`, what
  its entries moved revenue by on balance (review round 1: the run's
  own total, which its consumer would otherwise add up from the
  postings), per posting the order by id and number, the
  customer's number and name, the cumulative amounts, the position as
  contract asset or liability, the entry by id, year and number, and
  per obligation its description, its revenue account by id and number
  and its amounts; every amount with its currency.
- **Tested.** Delivered and not billed is accrued (Dr contract asset,
  Cr revenue), taken off once billed, and a third run posts nothing;
  one contract goes from a contract liability (a service billed ahead)
  to a contract asset in the next period and back, the entry taking the
  old position off and putting the new one on, never both; a billing
  document dated into a period already run is caught up; each entry is
  in every ledger the company keeps; the refusals, after which nothing
  is kept; a replay answers the run; two runs of a period at once end
  with one; a draft is not read, and a contract of an order blocked for
  credit, which takes no delivery, recognizes nothing; the database's
  rules answer as the run's; every database statement failing.
- **Budget.** None: the run is a posting, not a report, and the plan
  names no budget for it.
- **Walkthrough.** Step 82 extended: Maple gets accounts 1260 and 2460
  by a governed change; step 67's order is delivered and billed in
  full; the run for today's period posts two entries: for step 67's
  contract Sales is debited by 53.57 for the brackets (allocated
  1446.43, billed 1500.00) and by what the support is billed ahead, and
  the contract liability credited with the sum; for step 68's released
  order, delivered and not billed, Sales is credited and the contract
  asset debited with 5000.00. Step 63's contract posts nothing; a
  second run of the period is refused.
- **Decided by default while building.** Defaults 11 to 16 above.
