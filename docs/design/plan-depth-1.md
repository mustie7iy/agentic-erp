<<<<<<< HEAD
# Depth plan 1: first variant slices (17 November 2026)

**Status:** plan (product owner, 16–17 November 2026: breadth is built;
the variants start, pilot-relevant first, ahead of the 1 December date
the "later: …" leaves name). Each lane splits two named variants of its
placeholder into slices in the form of `plan-breadth-week3.md` (Adds,
Acceptance, Later) and builds them. `docs/tracker.json` stays the only
plan (07 §7.5); each leaf's `r` names its section below. Each lane writes
its own `## <lane>` section.

## recv

Placeholder: "later: incoming payment variants"
(`plan-breadth-week1.md` §T75c, Later; ADR-0063 §6). Two variants are
split here; the rest stay in the placeholder.

### T75f Partial payment: residual items and payment differences within tolerance

**Capability.** An AR accountant applies a receipt that is less than the
invoices it pays: either a residual item keeps what is still owed open as
an item of its own, linked to the invoice and with a reason, or a small
difference within the company's tolerance is written off; an overpayment
within tolerance is kept as revenue.

- **Reuses:** T75c's application (`ledger.Postings.ApplyReceipt`, one DZ
  entry offsetting receipt and items at their booked amounts, the
  exchange difference lines), the items' payment terms
  (`ledger.open_item_terms`), the account determination.
- **Adds:** per item of `POST /v1/incoming-payments` an optional
  `residual` (amount, reason `short_payment` | `disputed` | `deduction` |
  `other`, free text); the residual item, a debit line of the clearing
  entry on the item's account with the customer, at the item's own rate,
  with the item's due date (its terms without cash discount tiers); the
  payment difference, what the items net of their residuals differ from
  the receipt, posted to `payment_difference_expense` (underpayment) or
  `payment_difference_revenue` (overpayment) when it is within the
  company's tolerance `companies[].receivables.payment_tolerance`
  (amount in the company's currency and percent of the items applied,
  the lower binding, each direction its own; SAP OBA3's customer
  tolerance group); the payment shows the difference and each item's
  residual with its open item. Migration: the residual and difference
  columns, the two account determination keys.
- **Walkthrough:** `step_partial_payment`: Northwind pays less than a new
  invoice; one receipt leaves a residual item with the reason
  `disputed`, the residual is Northwind's open item with the invoice's
  due date; a second, cents short of another invoice, writes the
  difference off within tolerance; a shortfall over the tolerance is
  refused.
- **Acceptance:** a residual is more than zero and less than its item, on
  a debit item only, in the receipt's currency (422); a difference
  outside the tolerance, or with no account for it, is refused (422) and
  nothing posts; a company without a tolerance keeps T75c's exact rule;
  the residual item is open, the original cleared; replays answer the
  same payment.

### T75g Payment on account and its later application

**Capability.** An AR accountant puts a receipt that names no invoice, or
the part of a receipt the invoices do not take, on the customer's
account as an unapplied credit, and later applies that credit to the
customer's open items, in full or in part.

- **Reuses:** T75f's application (residuals, tolerance), the customer's
  reconciliation account (partner context, T74a), `OpenItems.Closed` (new, a read without lock).
- **Adds:** `on_account` on `POST /v1/incoming-payments` (items may then
  be empty): what the receipt leaves after the items is a credit line on
  the customer's reconciliation account; `POST
  /v1/incoming-payments/{id}/applications` applies the payment's open
  credit on account to open items, what is left staying on account as a
  new credit line; the payment shows what was put on account, what is
  still open there and its applications; event
  `IncomingPaymentOnAccountApplied`. Migration: the on-account columns
  and `receivables.incoming_payment_application`.
- **Walkthrough:** `step_payment_on_account`: a receipt with no
  remittance goes on Northwind's account; part of the credit is applied
  to T75f's residual item, the rest stays on account; the payment shows
  both.
- **Acceptance:** on account with nothing left is refused (422); an
  application consumes the payment's current credit only while it is
  open (409 once it is cleared, by another application or elsewhere);
  two applications race and one wins; items over the credit follow
  T75f's residual and tolerance rules; replays answer the same
  application.

**Later (still in the placeholder):** one receipt for several customers;
a receipt in another currency than the items; undoing an application;
matching by rule and agent proposals from statement lines; receipts
without a statement; direct debits; refunds; cash discount granted (its
own leaf); reason codes from configuration; output tax corrected on a
payment difference.

## dunning

Collections, "later: dunning variants" (`plan-breadth-week1.md` §T76b,
Later; ADR-0065). SAP S/4HANA's dunning procedure (FBMP) is the reference:
grace days on the line items, charges per dunning level, interest per
level at a rate, a minimum amount below which a customer is not dunned,
and dunning blocks on the customer's company data and on a line item. The
company's dunning configuration (`companies[].dunning`) is its one
procedure; procedures per customer stay later.

### T76d Dunning charges, interest, grace days and minimum amounts

**Capability.** A credit controller's dunning run charges each notice its
level's dunning charge and interest on its items at the company's annual
rate, leaves out the items still within the company's grace days and the
notices below its minimum amount.

- **Reuses:** the proposal (`internal/collections/propose.go`), the
  configuration's dunning levels and their validation and tier
  (`internal/config/dunning.go`), `money` (rounding half away from zero,
  `money.Sum`), the run's events.
- **Adds:** configuration `companies[].dunning.grace_days`,
  `.minimum_amount`, `.interest_rate_percent`, `levels[].charge`,
  `levels[].interest` (tier 2, as the levels); the run keeps the settings
  it applied and its charge, interest and below-minimum totals; a notice
  its charge, interest and total; an item its interest; the events carry
  the charge and interest. No new endpoint or table.
- **Walkthrough:** `step_dunning_charges`: a new customer with a small
  invoice and a larger one on longer terms; Ada sets grace days, a minimum
  amount, an interest rate and the levels' charges through a tier 2
  change; a run within the grace days proposes nothing of the customer, a
  later one names its notice as below the minimum, a run once both
  invoices are overdue proposes it, is confirmed, and the next run
  charges the second level's charge and the interest of the days overdue.
- **Acceptance:** an item overdue by no more than the grace days is not
  proposed; a notice whose overdue amount is below the minimum amount is
  not proposed and its items are not recorded when the run is confirmed,
  the run counting it; the charge is the notice level's; interest is
  amount × rate × days overdue ÷ 365 per item of a level that charges
  interest, rounded half away from zero in the company's currency; the
  notice's total is its overdue amount, charge and interest; nothing is
  posted; a run without the settings proposes as before.
- **Depends on:** T76b (in `main`).

### T76e Dunning blocks

**Capability.** A credit controller blocks a customer, or one of its open
items, from dunning in a company with a reason and an end date, and
releases a block early; a dunning run honours the blocks.

- **Reuses:** the collections context; the open item check of dispute
  cases (T86a); the run's list of items left out (`disputed_items`, T86b);
  the confirmation's stale check (T76b); `etag`, audit.
- **Adds:** table `collections.dunning_block`; `POST /v1/dunning-blocks`,
  `GET /v1/dunning-blocks`, `GET /v1/dunning-blocks/{id}`,
  `POST /v1/dunning-blocks/{id}:release`; permission
  `collections.dunning.block`; the run's `blocked_items` with their count
  and amount.
- **Walkthrough:** `step_dunning_blocks`: Ruth blocks one invoice of
  `step_dunning_charges`' customer and a run names it as blocked; a block
  of the whole customer leaves both invoices out until its end date; a
  past end date and a second release are refused; a proposal confirmed
  after a block was placed is stale; the invoice's block released, the
  next run proposes it.
- **Acceptance:** a block is in force for a run while it is not released
  and the key date is on or before its end date; a blocked item is in no
  notice and in none of the run's totals, named in `blocked_items` with
  its block; an item block names an open item the customer owes; the end
  date is not before the company's today; releasing is once; a proposal
  with an item blocked since it was started is refused as stale (409).
- **Depends on:** T76d.

**Later for dunning (still after these):** letters and e-mails (output
management); procedures per customer; charges and minimum amounts per
currency and per level; posting dunning charges and interest to the
customer; blocks shown in the collection worklist; scheduled and large
runs as a workflow; cancelling a confirmed run.
||||||| 2dfb6488
=======
# Depth plan, first variant slices (from 17 November 2026)

**Status:** plan (product owner, 2026-11-17: breadth is built; the
variants start, pilot-relevant first). Each lane splits the two variants
its brief names out of a "later: …" leaf of `docs/tracker.json` into
slices, in the form of `plan-breadth-week3.md`, and narrows that leaf to
what remains. `docs/tracker.json` stays the only plan (07 §7.5); each new
leaf's `r` names its section below. What every slice reuses is
`plan-breadth-week3.md`'s list, unchanged.

## sales

Sales and billing, "later: sales order variants" (`plan-breadth-week2.md`
§T78b, Later) and "later: billing document variants" (§T79a, Later;
ADR-0067). SAP S/4HANA is the reference: an order is changed in place
(VA02) with change documents kept (CDHDR/CDPOS), and cancelled by a
reason for rejection only while nothing of it is delivered; a billing
document is cancelled (VF11) by a cancellation document of its own,
numbered in the billing range, whose accounting document reverses the
invoice's receivable, revenue and output tax, and the order's items
become billable again.

### T78d Sales order changed and cancelled

**Capability.** A sales representative changes an open sales order (an
item's quantity and price, the requested delivery date), each change
kept as a numbered change with what it changed, and cancels an order of
which nothing is delivered, with a reason.

- **Reuses:** T78b's order under its row lock, T78a's item check and
  its one pricing function (`itemCheck.item`, `price`), T77b's credit
  check, the `If-Match` version, audit, outbox, `SalesOrderChanged`.
- **Adds:** on `sales.sales_order` the status `cancelled` with its
  reason, who and when, and an optional `requested_delivery_date`;
  table `sales.sales_order_change` (insert-only: change number, the
  order's version after it, reason, what changed with its values before
  and after); `POST /v1/sales-orders/{id}:change`,
  `GET /v1/sales-orders/{id}/changes`, `POST /v1/sales-orders/{id}:cancel`
  (`sales.order.write`, `If-Match`, idempotent, human-only);
  `requested_delivery_date` on `createSalesOrder`.
- **Walkthrough:** `step_sales_order_change`: an order is created
  directly and confirmed; its quantity and price are changed and the
  change reads back with its values before and after; a second order is
  cancelled with a reason; the delivered order of `step_sales_order`
  refuses a cancellation.
- **Acceptance:** a draft or confirmed order changes, a cancelled one
  does not (409); a changed item is checked and priced again as an item
  given directly is (§3), on the order's date; its quantity is never
  below what is delivered and its quantity and price change only while
  nothing of it is billed (422 naming the field); the order's net value
  is the sum of its items' exactly; a change that raises a confirmed
  order's value is checked against the customer's credit limit, and an
  order over it is blocked again; each change is numbered and kept with
  its values before and after; cancelling needs a reason and an order of
  which nothing is delivered (409), once; a stale `If-Match` is 412;
  audited and published; nothing is posted.
- **Depends on:** T78b, T77b (in `main`).

### T79c Billing document cancelled

**Capability.** A billing clerk cancels a billing document: a created one
simply, a posted one by a cancellation document whose customer credit
memo reverses the invoice's receivable, revenue and output tax exactly;
either way the order's items are billable again.

- **Reuses:** T79b's posting port (`receivables.Endpoints.CreateInvoice`
  and `PostInvoice`) with T85d's credit memo naming the invoice and
  carrying the tax the invoice bore (`ItemInput.Tax`), the order's row
  lock, the billing number range, audit, outbox.
- **Adds:** the status `cancelled` on `sales.billing_document`; table
  `sales.billing_cancellation` (insert-only: one per document, numbered
  from the company's `billing_document` range, its date, reason and, for
  a posted document, the credit memo and its entry); the pricing of each
  billing item kept on the item when it is billed, so a later order
  change does not alter a billed document; `POST
  /v1/billing-documents/{id}:cancel` (`sales.billing.post`, `If-Match`,
  idempotent, human-only); event `BillingDocumentCancelled`; the
  cancellation in the document's representation and, as a negative
  billing, in `sales.Orders` and in net revenue by segment.
- **Walkthrough:** `step_billing_cancellation`: the order of
  `step_sales_order_change` is delivered, billed and posted; the
  document is cancelled; the credit memo names the cancellation and the
  invoice and reverses 1200, 4000 and 2300 by exactly the invoice's
  amounts; the order's items show nothing billed and a new billing
  document bills them again; a second cancellation is refused.
- **Acceptance:** a created or posted document is cancelled once (409
  otherwise); one named by a returns order or a credit memo request that
  is not rejected is refused (409); the cancellation's credit memo is the
  invoice's net, tax and gross to the minor unit; the order's billed
  quantities fall by the document's and its later billing documents'
  amounts follow the cumulative rule over the documents not cancelled; a
  refusal by receivables or the ledger leaves the document as it was and
  names its fields; audited and published.
- **Depends on:** T79b, T85d (in `main`).

**Decided by default (sales):** (1) a change is a custom method
(`:change`), not `PATCH`, since a confirmed order is no draft (04 §4.3);
(2) only an item's quantity and price and the order's requested delivery
date change in this slice; items are not added or removed; (3) an item
with something billed keeps its quantity and price (cancel the billing
first), so billed amounts never need recalculating; (4) a changed item
is priced again on the order's date (an entered price stays entered, a
price-list price is determined again); (5) a raised confirmed order is
checked for credit on the increase and blocked again over the limit,
whatever an earlier release; (6) `:cancel` serves both a created and a
posted billing document (SAP's VF11); the cancellation is numbered from
the `billing_document` range and is its credit memo's source; (7) the
credit memo is not cleared against the invoice: it stands as the
customer's open credit item and clears by the ledger's clearing, as
T85d's does; (8) a confirmed order with a revenue contract keeps its
items' quantities and prices and is not cancelled (409), since the
contract holds their values and does not follow a change.

**Later for these variants:** adding and removing items, changing the
payment term, the customer or the currency; schedule lines; reversing a
delivery; cancelling an order with deliveries (a reason for rejection per
item); collective cancellation; the revenue contract following an order
change, a cancellation or a cancelled billing; clearing the cancellation
against an open invoice automatically.
>>>>>>> sales

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
