# Depth plan 2: second variant slices (17 November 2026)

**Status:** plan (product owner, 17 November 2026: development focus,
variants continue after `plan-depth-1.md`). Each lane splits two named
variants of its placeholder into slices in the form of
`plan-breadth-week3.md` (Adds, Acceptance, Later) and builds them.
`docs/tracker.json` stays the only plan (07 §7.5); each leaf's `r` names
its section below. Each lane writes its own `## <lane>` section.

## recv2

Placeholder: "later: incoming payment variants"
(`plan-breadth-week1.md` §T75c, Later; `plan-depth-1.md` §recv, Later;
ADR-0063 §6). Two more variants are split here; the rest stay in the
placeholder.

### T75h One receipt for several customers

**Capability.** An AR accountant applies one bank receipt to the open
items of more than one customer, as when a parent company pays its
subsidiaries' invoices (SAP F-28's other accounts, from memory): each
customer's items are cleared, and the receipt balances to the amount.

- **Reuses:** T75c's application (`ledger.Postings.ApplyReceipt`, one DZ
  entry offsetting receipt and items at their booked amounts), T75f's
  residual items and tolerance, T75g's payment on account (to the paying
  customer).
- **Adds:** `other_customer_ids` on `POST /v1/incoming-payments`: the
  other customers whose items the receipt pays besides `customer_id`, the
  payer; each item is an open item of one of them; each other customer is
  a business partner with at least one item; a residual item stays with
  its item's customer; what is left on account goes to the payer. The
  payment shows each item's customer by id, number and name and each
  customer's part (its items net of their residuals); `customer_id` on
  `GET /v1/incoming-payments` also finds a payment that paid that
  customer's items; IncomingPaymentApplied carries the customers' parts.
  Migration: `incoming_payment_item.customer_id`.
- **Walkthrough:** `step_receipt_several_customers`: Northwind pays one
  receipt for its own invoice and an invoice of Northwind Logistics, a
  new customer of Maple; both invoices are cleared by one DZ entry, the
  payment shows the two customers' parts, and the list by Northwind
  Logistics finds it; an item of a customer not named is refused.
- **Acceptance:** an item of a customer that is neither the payer nor
  named is refused (422 `ledger.not_the_customers_item`); an other
  customer that is no business partner (422 `request.reference_not_found`),
  is the payer (422 `receivables.duplicate_customer`), or
  has no item (422 `receivables.customer_without_items`) is refused, each
  naming `other_customer_ids[i]`, and nothing posts; the parts, less the
  difference written off and plus what is left on account, are the
  receipt; a payment of one customer is as before; replays answer the
  same payment.

### T75i Undoing an application (resetting its clearing)

**Capability.** An AR accountant undoes an incoming payment applied to
the wrong items, or an application of its credit on account (SAP FBRA,
reset cleared items and reverse, from memory): the items are open again,
the receipt waits to be applied again or the credit on account is
restored as it was, and the clearing entry is reversed.

- **Reuses:** the ledger's reversal (`reverse`, in every ledger, on the
  original posting date), the entries' locks (`lockEntries`), T75g's
  credit on account, `etag`, audit, the outbox.
- **Adds:** `ledger.Postings.ResetClearing`: the clearing by an entry is
  reset (its cleared lines open again, kept in `ledger.clearing_reset_item`
  with the reset in `ledger.clearing_reset`; `ledger.clearing_item` rows
  may be deleted only of a reset clearing), and the entry reversed in the
  same transaction, on its own posting date, so the period must still be
  open; event `ClearingReset`. `POST /v1/incoming-payments/{id}:reset`
  (the payment's own application, once every application of its credit
  is undone) and `POST /v1/incoming-payments/{id}/applications/{application_no}:reset`
  (the latest application in force), both with `If-Match` and a reason;
  the payment shows its `status` (`applied`, `reset`), each reset with its
  reversal entry, and the credit on account open again; event
  `IncomingPaymentReset`. Migration: the two ledger tables and the
  clearing item rule, `receivables.incoming_payment_reset`, and the
  receipt, item and credit applied once while in force rather than once
  for ever.
- **Walkthrough:** `step_payment_reset`: Ruth applies a Northwind receipt
  to the wrong invoice, resets it (the invoice open again, the receipt
  among the unapplied receipts, the DZ entry reversed) and applies the
  receipt to the right one; she puts another receipt on account, applies
  part of the credit, finds the payment's reset refused while that
  application is in force, resets the application (the credit open again
  in full, the invoice open) and applies the credit again.
- **Acceptance:** a reset whose clearing entry's period is closed is
  refused with the ledger's problem (409 `ledger.period_not_open`) and
  nothing changes; a residual item or credit on account the application
  left that was cleared since is refused (409
  `ledger.clearing_lines_cleared`); the payment while an application is
  in force (409 `receivables.applications_in_force`), an application not
  the latest in force (409 `receivables.application_not_latest`), and a
  second reset (409 `receivables.already_reset`) are refused; a stale
  `If-Match` is 412; two resets racing end with one; the receipt and the
  items may be applied again, each once while in force; replays answer
  the same payment.

**Later (still in the placeholder):** a receipt in another currency than
the items; matching by rule and agent proposals from statement lines;
receipts without a statement; direct debits; refunds; reason codes from
configuration; output tax corrected on a payment difference; resetting a
clearing without reversing its entry, and resetting clearings other than
incoming payments' (manual clearings, settlements).

## coll

Collections, "later: collection worklist variants"
(`plan-breadth-week1.md` §T76c, Later; ADR-0065). SAP S/4HANA's
Collections Management (FSCM-COL) is the reference: a collection
specialist works the worklist, records a customer's promise to pay for
invoices (promised amount and date; the promise is kept when paid,
broken when the date passes unpaid), and records each contact with the
customer (whom, when, the result, a note) with a resubmission date that
takes the customer off the worklist until then. Two variants are split
here; assigning customers to collectors and strategies and priorities
stay in the placeholder.

### T76f Promises to pay

**Capability.** A collections specialist records a customer's promise to
pay an amount by a date for some of its open items; the promise reads
open, kept or broken by the company's today; the worklist shows each
customer's open and broken promises, and a dunning run holds the
promised items back up to the promised date.

- **Reuses:** the collections context and its ports; the open item check
  of dispute cases and blocks (`customersItem`); the run's lists of items
  held back (`disputed_items`, `blocked_items`) and the confirmation's
  stale check (T76e); `config.DocumentDay` and `Endpoints.Now` (the
  company's today); `ledger.OpenItems.Closed` (whether a promised item is
  still open); cursor paging; audit.
- **Adds:** tables `collections.promise_to_pay` and
  `collections.promise_to_pay_item` (insert-only); `POST
  /v1/promises-to-pay`, `GET /v1/promises-to-pay`, `GET
  /v1/promises-to-pay/{id}`; permission `collections.worklist.write`
  (recording a worklist's work: promises here, contacts in T76g; read with
  `collections.worklist.read`); the run's `promised_items` with their
  count and amount; on the worklist per customer `promised_amount`,
  `open_promises`, `next_promised_date` and `broken_promises`, and the
  worklist's `promised_amount` total. No event (as a block: nothing
  outside the context changes).
- **Walkthrough:** `step_promise_to_pay`: Lakeshore Hardware (steps 111
  and 112) promises CAD 30.00 for the gasket invoice and CAD 600.00 for
  the pump invoice by two dates; a second promise of the pump, an amount
  above the items and a date in the past are refused; a run at a key
  date before the dates names both invoices as promised and duns neither;
  Lakeshore pays the gasket and that promise reads kept; the worklist
  after the pump's date shows its promise broken, and a run at that date
  duns the pump again.
- **Acceptance:** the amount is in the company's currency, above zero
  and at most what the items amount to in it (422 naming the field); each
  item is an open item of the company the customer owes, named once
  (422); an item in another promise whose date has not passed is refused
  (409), and two promises of one item racing let one win; the date is
  from the company's today to 2100-12-31 (422); kept is the promised
  items no longer open adding up to the promised amount at least, broken
  is not kept once the company's today is past the date, open otherwise;
  a run at a key date up to the date leaves the promised items out of its
  notices and totals and names them, a block taking precedence and a
  dispute coming after; a proposal with an item promised since is stale
  on confirmation; the worklist judges promises at its key date; schema
  tests for both tables.
- **Depends on:** T76e (in `main`).

### T76g Contact notes and resubmission dates

**Capability.** A collections specialist records a contact with a
customer of a company (when, whom they spoke to, the outcome, a note)
with an optional resubmission date; the worklist shows each customer's
last contact and leaves out a customer whose last contact's resubmission
date is after the key date, until that date.

- **Reuses:** the collections context; `config.DocumentDay` and
  `Endpoints.Now`; the people port (who recorded the contact, by name);
  cursor paging; audit; `collections.worklist.write` (T76f).
- **Adds:** table `collections.customer_contact` (insert-only: a contact
  is a record of what happened, corrected by a later one); `POST
  /v1/customer-contacts`, `GET /v1/customer-contacts`, `GET
  /v1/customer-contacts/{id}`; on the worklist per customer
  `last_contacted_at`, `last_contact_outcome` and `resubmission_date`,
  and the worklist's `resubmission_count` and
  `resubmission_overdue_amount` of the customers left out; the contact's
  person and note in the personal data inventory. No event.
- **Walkthrough:** `step_collection_contacts`: Ruth calls Lakeshore about
  the broken promise, reaches its accounts payable clerk and sets a
  resubmission date; a contact in the future is refused; the worklist
  before that date leaves Lakeshore out and counts it, at that date it is
  back with its last contact; a later contact without a resubmission
  date brings it back at once; the contacts read latest first.
- **Acceptance:** `contacted_at` is not after the clock (422 naming the
  field); the outcome is one of the open enumeration (422); the
  resubmission date is after the company's today and at most 2100-12-31
  (422); the customer is a business partner of the tenant (422); the last
  contact is the latest by `contacted_at`; texts of white space alone
  are refused; schema tests for the table and its insert-only rule.
- **Depends on:** T76f.

**Later (still in the placeholder):** assigning customers to collectors
and collection groups; strategies and priorities (the worklist's order by
rule); a promise in instalments, changed or withdrawn, and its tolerance
days; a promise kept by a payment's date rather than by the items being
open when read; contact outcomes from configuration; reminders to the
collector on the resubmission date.

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
