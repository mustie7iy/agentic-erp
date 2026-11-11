# Breadth-first plan, week 1 (11–17 November 2026)

**Status:** plan (product owner, 2026-11-10: breadth first). It details the
tracker's week 1 placeholders into slices (ADR-0053) and is frozen once
merged: each placeholder is split once, here. `docs/tracker.json` stays the
only plan (07 §7.5); each leaf's `r` names its section below.

The decision: every capability in the required scope gets a first usable
version end to end by 2026-12-01: the main path only, one or two slices
each, each sized for one agent in about half a day and fully gated (07
§7.7). Variants and depth come after 1 December; each placeholder's
"later: …" leaf names what was left out on purpose and is not picked up
before then.

Thirteen placeholders are split: Customer master with company data (Sales
and service); Customer open items, Incoming payment posting and clearing,
Credit memos and AR aging report (Accounts receivable); Dunning and
Collection worklists (Collections); Credit limits (Credit management); Order
master, Order budget and Settlement (Internal orders); Assessment and
distribution cycles and Settlement rules (Overhead allocation). They give 17
slices and 13 "later" leaves.

## What every slice reuses

Each slice is built as payables and the ledger were, and adds none of these
again:

- the unit of work and `reqctx.Request` (`internal/platform/uow`,
  `reqctx`), RLS with `tenant_id` leading every key (ADR-0003), company
  access as the existing contexts check it;
- `money.Minor` and `money.Allocate` (ADR-0005); rows read `FOR UPDATE` or
  guarded by `version`/state (ADR-0008); `Idempotency-Key` on every write and
  `If-Match` on changes (04 §4.7);
- financial effects only through the ledger's posting port
  (`ledger.Postings`: `Post`, the open items, reservations and clearing of 03
  §3.7), with `source_context` and `source_document_id` (03 §3.16);
- the context's own outbox and inbox registered in `outbox.Contexts`
  (ADR-0007), an audit record per state change (ADR-0011), `problem.*` errors
  with their constraint mappings (04 §4.6);
- contract first (`api/openapi.yaml`, `api/asyncapi.yaml`, `make generate`),
  real PostgreSQL tests with `pgtest` and `pgtest.FailingTx`, a schema test
  per database rule, a budget for a new report read (07 §7.7 item 7), and one
  new or extended step of `scripts/walkthrough.sh`.

Headless first: no web workspace work (ADR-0053 g).

## Build order and lanes

Three lanes; a slice starts when the slices it depends on are in `main`, or
from their lane's tip as a stacked base.

- **Lane A, receivables** (new `internal/receivables`; in `internal/ledger`
  only the open-item files: `ports.go` terms, `settle.go`, `reads.go`,
  `aging.go`): T75a → T75b → T75c → T75d → T75e.
- **Lane B, customer, collections, credit** (`internal/partner`; new
  `internal/collections`; new `internal/credit`): T74a → T76a → T76b → T76c →
  T77a. T76a and T77a need T75b's reader of customer open items.
- **Lane C, controlling** (`internal/controlling`; in `internal/ledger` only
  the assignment files: `assignment.go`, the journal's derivation,
  `controlling.go`; `internal/config` for T60g): T60a → T60b → T60c → T60d →
  T60e → T60f → T60g. T60f and T60g need only T56's cost centers, so they may
  be pulled forward if T60a–T60e wait on anything.

By day: day 1 T75a ∥ T74a ∥ T60a, T60b; day 2 T75b, T75c ∥ T76a ∥ T60c,
T60d; day 3 T75d, T75e ∥ T76b, T76c ∥ T60e, T60f; day 4 T77a ∥ T60g.

Lanes A and C both change `internal/ledger`, in different files; lane A
merges first on a day both do. `api/openapi.yaml`, `api/gen`, migration
numbers, `internal/app` wiring, `scripts/walkthrough.sh` and the tracker are
shared by every slice and resolved at integration as always (`make
generate`, renumbering).

Step numbers: T60a–T60g detail the approved step T60 (07 §7.6). T74–T77 are
new; nothing on the base uses them. Integration renames them if another plan
took the same numbers.

## ADRs

- Customer master: no new ADR. 03 §3.6 settles it (one business partner
  plays both roles; company data per role and legal entity); T74a records it
  there.
- Accounts receivable (four placeholders): **new ADR, written by T75a**
  (the receivables context 02 §2.2 and 03 §3.16 name, its documents, posting,
  applying incoming payments, credit memos, the aging read). ADR-0047 already
  decides that incoming payments wait on the incoming clearing account for
  receivables; ADR-0049 §7 the output tax of entries.
- Dunning and collection worklists: **new ADR, written by T76a** (a leaf
  context `collections` reading the ledger and partner through ports).
- Credit limits: **new ADR, written by T77a** (a leaf context `credit`).
- Internal orders, budget, settlement, settlement rules and cycles: **new
  ADR, written by T60a**, which ADR-0050 §2 and its T60 boundary ask for
  (where the order master lives, the second real cost object, secondary cost
  elements, cycles posting through the ledger). T60b–T60g record what they
  build in its "As built".

A new ADR takes the next free number at integration; 0062 is held by the
parked SaaS ADR.

---

## Lane B first slice

### T74a Customer with company data

**Capability.** An AR accountant creates a customer (a business partner with
the role `customer`) with its data in a company: reconciliation account,
payment term, posting block. The change goes through the partner change and
its approval, as a supplier's does.

- **Reuses:** `internal/partner` (`changes.go`, the approval of partner
  changes, `supplier_company` as the pattern,
  `Accounts.IsReconciliationAccount` with kind `customer`,
  `Postings.PostingBlock`), the configuration's payment terms.
- **Adds:** table `partner.customer_company`; `customer_companies[]` on
  `createBusinessPartner`, `createPartnerChange` and the partner's
  representation; a published read `partner.Customers` (a customer's company
  data) for receivables. No new endpoint.
- **Walkthrough:** `step_customer`: a customer of Maple is created,
  approved and read back with its company data.
- **Acceptance:** a customer with company data is created and approved;
  the reconciliation account must be a customer reconciliation account
  postable in the company and the payment term an active one (422 naming the
  field); a posting to a customer blocked for posting in the company is
  refused; the change is audited and published; schema tests for
  `customer_company`.
- **Depends on:** nothing.

**Later (after 1 December):** sales-area data and partner functions
(ship-to, bill-to, payer); contact persons; customer payment methods and
direct debit mandates; planning group; custom fields on the company data;
taking customers over from SAP.

---

## Lane A, receivables

### T75a Customer invoice entered

**Capability.** An AR accountant enters a customer invoice without a sales
order (customer, dates, currency, items by revenue account with net amount
and output tax code) and reads it back with its calculated tax and totals.

- **Reuses:** the document shape of `internal/payables` (`invoices.go`,
  `tax.go`: header and items, a document number from the number ranges),
  `internal/tax` for the calculation, `ledger.Accounts` (account and
  assignment checks, tax codes at the tax date), `partner.Postings`. A helper
  both contexts need moves to `internal/platform`; none is copied.
- **Adds:** the context `receivables` (schema, RLS, outbox and inbox,
  permissions `receivables.invoice.read`, `.write`, `.post`); tables
  `receivables.customer_invoice`, `receivables.customer_invoice_item`;
  `POST /v1/customer-invoices`, `GET /v1/customer-invoices`,
  `GET /v1/customer-invoices/{id}`; event `CustomerInvoiceChanged`; the
  accounts receivable ADR.
- **Walkthrough:** `step_customer_invoice`: an invoice to the customer of
  `step_customer` is entered and read back, tax and gross as expected.
- **Acceptance:** a draft is created idempotently and listed by customer
  and status; tax per item is what its output code gives at the invoice date;
  net, tax and gross add up exactly in minor units; an unknown customer,
  account or tax code is refused naming the request's own field; nothing is
  posted.
- **Depends on:** nothing (the customer is checked by role, as today).

### T75b Customer invoice posted: the customer's open item

**Capability.** An AR accountant posts the invoice; it is the customer's
open item, with its payment terms and net due date, in `GET /v1/open-items`.

- **Reuses:** `ledger.Postings.Post` (debit the customer's reconciliation
  account with the partner, credit revenue per item, credit output tax from
  the tax items; ADR-0049), `Posting.Terms` and `ledger.open_item_terms`
  (T36b), `listOpenItems` and its `partner_id` filter, the period checks.
- **Adds:** `POST /v1/customer-invoices/{id}:post`; event
  `CustomerInvoicePosted`; in the ledger, terms kept for customer open items
  as for supplier ones, and the reader `ledger.OpenItems.Receivable` (customer
  open items of a company with partner, terms and due date) that T75e, T76a,
  T76c and T77a read. No new table.
- **Walkthrough:** `step_customer_invoice` extended: the invoice is posted;
  the open item shows with its due date; receivables, revenue and output tax
  moved by the invoice's amounts.
- **Acceptance:** posting moves draft to posted once (row lock and state
  guard; a second post is refused, a replay answers the same); the entry
  balances and carries `source_context` `receivables`; terms come from the
  invoice, else the customer's company data; a customer without company data
  in the company is refused (422); a closed period is refused; the ledger's
  `:reverse` refuses the entry as 04 §4.9 says; audit and event written in the
  transaction.
- **Depends on:** T74a, T75a.

**Later for Customer open items (after 1 December):** reversal through the
document; changing or cancelling a draft; parked invoices and approval above
an amount; invoices created by billing (week 2's Billing leaves post through
receivables); the customer account statement; down payments received;
attachments and custom fields; sending e-invoices (T65h); taking customer
open items over from SAP.

### T75c Incoming payment applied to a customer's invoices

**Capability.** An AR accountant applies an incoming payment that sits on a
house bank's incoming payments clearing account (T49h, ADR-0047) to a
customer's open items in full; payment and items are cleared by one entry.

- **Reuses:** the ledger's clearing building blocks (`accountItems`,
  `offsetItems`, `clearWithEntry`, the exchange difference lines of
  `settle.go`: each side offset at its original amounts, what is left in
  local or group currency a realized exchange difference, as a settlement
  does), item reservations, `incoming_gl_account` of the house bank.
- **Adds:** tables `receivables.incoming_payment`,
  `receivables.incoming_payment_item`; `POST /v1/incoming-payments`,
  `GET /v1/incoming-payments`, `GET /v1/incoming-payments/{id}`; a read of
  the receipts not yet applied (`GET /v1/unapplied-receipts`) unless an
  existing statement read already lists them; a ledger port method applying a
  receipt to customer items; event `IncomingPaymentApplied`; permission
  `receivables.payment.apply`.
- **Walkthrough:** `step_incoming_payment`: a statement credit matched as
  incoming (as `step_statement` does) is applied to the invoice of
  `step_customer_invoice`; the customer has no open item left and the
  incoming account's item is cleared.
- **Acceptance:** the items applied, credit memos netted, equal the receipt
  exactly and in its currency, else 422; items of another customer or
  company, cleared, reversed or reserved items are refused; applying the same
  receipt twice changes nothing (idempotency key, and the receipt is no longer
  open); the period must be open.
- **Depends on:** T75b.

**Later (after 1 December):** partial payments and residual items; payments
on account; payment differences and tolerances; cash discount granted (its
own leaf); one receipt for several customers; a receipt in another currency
than the items; undoing an application; matching by rule and agent proposals
from statement lines (ADR-0047); receipts entered without a statement;
direct debits; refunds.

### T75d Customer credit memo

**Capability.** An AR accountant enters and posts a credit memo to a
customer, naming the invoice it credits; it is an open credit item that
clears against the invoice or nets when a payment is applied.

- **Reuses:** T75a's document with a kind, as `payables/kinds.go` does for
  supplier credit memos (T34); T75b's posting with the signs turned;
  `createClearing` for clearing invoice and memo.
- **Adds:** kind `credit_memo` and `reference_invoice_id` on
  `receivables.customer_invoice`; no new endpoint or table.
- **Walkthrough:** `step_customer_credit`: a second invoice is partly
  credited; the memo is posted, and invoice and memo show as the customer's
  open items, net what the customer owes.
- **Acceptance:** the memo posts debit revenue and output tax, credit the
  customer; credits naming an invoice may not exceed its gross, checked at
  posting under the invoice's row lock; a memo naming no invoice is
  unlimited; the memo's tax is at its own date; T75c nets it.
- **Depends on:** T75b (T75c for the netting test).

**Later (after 1 December):** debit memos; credit memos from returns and
credit memo requests (week 2's sales leaves); paying a credit out; reversal.

### T75e AR aging

**Capability.** A credit controller reads the aging of a company's customer
open items on a key date: per customer and in total, not yet due and in
buckets of days overdue.

- **Reuses:** `ledger.OpenItems.Receivable` (T75b) and the bucket logic of
  `ledger/aging.go` (T73j), exported, not copied; cursor paging.
- **Adds:** `GET /v1/receivables-aging` (company, key date, bucket days,
  customer); no table; a budget test.
- **Walkthrough:** `step_ar_aging`: the open invoice and credit memo of
  `step_customer_credit` appear in the customer's row and the totals.
- **Acceptance:** each item falls in exactly one bucket by its net due
  date; credit items reduce their customer's total; the totals equal the
  customer reconciliation accounts' open items; amounts in local currency;
  items without terms and key dates are treated as `listOpenItems`' aging
  treats them.
- **Depends on:** T75b (T75d for the walkthrough's credit).

**Later (after 1 December):** aging as at a past date; by document date; by
profit center or segment and in group currency; export; analytics metrics;
an allowance for expected credit losses (IFRS 9; see Questions).

---

## Lane B, collections and credit

### T76a Dunning run proposed

**Capability.** A credit controller starts a dunning run for a company at a
key date and reads its proposal: per customer, the overdue items and the
dunning level each would reach.

- **Reuses:** `ledger.OpenItems.Receivable`, partner's names through its
  port, configuration tiers and validation (`internal/config`), the ledger's
  service limit pattern for the size of one synchronous request.
- **Adds:** the leaf context `collections` (schema, RLS, outbox,
  permissions `collections.dunning.read`, `.run`); configuration
  `companies[].dunning` (levels with their days overdue); tables
  `collections.dunning_run`, `collections.dunning_notice`,
  `collections.dunning_notice_item`; `POST /v1/dunning-runs`,
  `GET /v1/dunning-runs`, `GET /v1/dunning-runs/{id}`,
  `GET /v1/dunning-runs/{id}/notices`; event `DunningRunChanged`; the
  collections ADR.
- **Walkthrough:** `step_dunning`: at a key date after the open invoice's
  due date, the run proposes one notice for the customer at level 1.
- **Acceptance:** only customer items overdue on the key date are proposed;
  a company without dunning levels is refused (422); nothing changes on the
  items; the same request again answers the same run.
- **Depends on:** T75b.

### T76b Dunning run confirmed

**Capability.** The credit controller confirms the proposal: each item's
level and dunning date are recorded and one event per customer is published;
the next run proposes the next level.

- **Reuses:** T76a; state guard (`WHERE status = 'proposed'`); outbox;
  audit.
- **Adds:** table `collections.dunned_item` (insert-only: item, level, run,
  date); `POST /v1/dunning-runs/{id}:confirm`; event `DunningNoticeIssued`
  (customer id and number, no name).
- **Walkthrough:** `step_dunning` extended: the run is confirmed; a second
  run at a later key date proposes level 2.
- **Acceptance:** confirming twice records and publishes once; a proposal
  with an item cleared since is refused as stale (409) and a new run is
  proposed; an item rises at most one level per confirmed run; the highest
  level stays.
- **Depends on:** T76a.

**Later for Dunning (after 1 December):** dunning charges and interest;
letters and e-mails (output management); grace days and minimum amounts;
dunning blocks on a customer or item; procedures per customer; scheduled
runs and large runs as a workflow; cancelling a confirmed run.

### T76c Collection worklist

**Capability.** A collections specialist reads a company's worklist: the
customers with overdue items, the largest overdue amount first, each with
its open and overdue totals, oldest due date, highest dunning level and last
dunning date.

- **Reuses:** `ledger.OpenItems.Receivable`, `collections.dunned_item`,
  cursor paging; stores nothing.
- **Adds:** `GET /v1/collection-worklist`; permission
  `collections.worklist.read`; a budget test.
- **Walkthrough:** `step_collections`: the dunned customer of
  `step_dunning` leads the list with its level and amounts.
- **Acceptance:** a customer with nothing overdue is not listed; the
  amounts equal the customer's open items on the key date; the order is
  stable across pages.
- **Depends on:** T76b.

**Later (after 1 December):** assigning customers to collectors;
strategies and priorities; promises to pay; contact notes and resubmission
dates; dispute cases and the collections agent (their own leaves).

### T77a Credit limit and its check

**Capability.** A credit manager sets a customer's credit limit in a
company, and any API consumer reads the customer's exposure and whether an
amount is within the limit.

- **Reuses:** `ledger.OpenItems.Receivable`, `partner.Customers`, `etag` and
  `If-Match`, audit, outbox.
- **Adds:** the leaf context `credit` (schema, RLS, outbox, permissions
  `credit.limit.read`, `.write`); table `credit.credit_limit`;
  `POST /v1/credit-limits`, `PATCH /v1/credit-limits/{id}`,
  `GET /v1/credit-limits`, `GET /v1/credit-limits/{id}`,
  `GET /v1/credit-exposure`; event `CreditLimitChanged`; the credit
  management ADR.
- **Walkthrough:** `step_credit_limit`: a limit is set for the customer;
  the exposure equals its open items; one amount is within the limit and a
  larger one is not.
- **Acceptance:** one limit per customer and company, changed with the
  caller's version; exposure is the customer's open items, credits netted, in
  local currency; `available` is limit less exposure; a customer without a
  limit reads as within the limit, saying no limit is set; the check changes
  nothing and blocks nothing.
- **Depends on:** T74a, T75b.

**Later (after 1 December):** order credit blocks (their own leaf, with
week 2's sales orders); open orders, deliveries and billing in the
exposure; limits across the group; validity dates and reviews; approval of
limit changes; risk classes and the scoring agent (its own leaf).

---

## Lane C, controlling

### T60a Internal order created and released

**Capability.** A controller creates an internal order (code, name, company,
responsible cost center) and releases it for posting.

- **Reuses:** `internal/controlling` (its schema, outbox and wiring from
  T56g), `ledger.AssignmentBalances` to check the cost center, `etag`.
- **Adds:** table `controlling.internal_order`; `POST /v1/internal-orders`,
  `GET /v1/internal-orders`, `GET /v1/internal-orders/{id}`,
  `POST /v1/internal-orders/{id}:release`; event `InternalOrderChanged`;
  permissions `controlling.order.read`, `.write`, `.release`; the controlling
  II ADR.
- **Walkthrough:** `step_internal_order`: an order of Maple is created and
  released.
- **Acceptance:** the code is unique per company; the responsible cost
  center is an active one of the company; release moves created to released
  once (row lock, version); audited and published.
- **Depends on:** nothing.

### T60b Costs posted to an internal order

**Capability.** A GL accountant posts a cost to a released order on a
journal entry line, and a controller reads the order's actual costs by
account and period.

- **Reuses:** the journal's assignment derivation and validation
  (ADR-0050 §5, written so a second cost object slots in), the manual journal
  entry API, the pattern of `ledger.Partners` for a port the ledger asks
  another context through.
- **Adds:** `internal_order_id` on `ledger.journal_line` (nullable, no
  cross-schema foreign key, partial index per partition); the port
  `ledger.CostObjects`, implemented by controlling; `internal_order_id` on
  journal entry lines in the API; `GET /v1/internal-order-balances`.
- **Walkthrough:** `step_internal_order` extended: an expense is posted to
  the order and shows in its actuals.
- **Acceptance:** a primary cost line carries exactly one real cost object,
  a cost center or an order; the order must be released and of the line's
  company (422 naming the line); the line's profit center and segment come
  from the order's responsible cost center; a reversal copies the order; the
  actuals equal the lines posted; schema tests and a budget test.
- **Depends on:** T60a.

**Later for Order master (after 1 December):** order types and number
ranges; statistical orders; orders on requisition, purchase order and
supplier invoice items and their commitments; order groups; plan values on
orders; changes by approval; reopening; taking orders over from SAP.

### T60c Order budget against actual

**Capability.** A controller sets an order's budget, one overall amount in
the company's currency, and reads budget, actual and what remains.

- **Reuses:** T60b's actuals; `If-Match`; audit; outbox.
- **Adds:** table `controlling.order_budget`;
  `PUT /v1/internal-orders/{id}/budget`,
  `GET /v1/internal-orders/{id}/budget`; event `InternalOrderBudgetChanged`;
  permission `controlling.order.budget`.
- **Walkthrough:** `step_order_budget`: a budget is set; after the posting
  of `step_internal_order` the remaining amount is budget less actual.
- **Acceptance:** the budget is changed with the caller's version and
  audited with the amounts before and after; the read says when the actual
  exceeds the budget; no posting is refused for it.
- **Depends on:** T60b.

**Later (after 1 December):** budgets per year; supplements, returns and
transfers; availability control (warning or refusal, with tolerances);
commitments counted against the budget; approval of budget changes.

### T60d Settlement rule of an order

**Capability.** A controller gives an order its settlement rule: one
receiver, a cost center of the order's company, taking the whole.

- **Reuses:** `ledger.AssignmentBalances` to check the receiver; T60a's
  order lock and version.
- **Adds:** table `controlling.settlement_rule`;
  `PUT /v1/internal-orders/{id}/settlement-rule`,
  `GET /v1/internal-orders/{id}/settlement-rule`.
- **Walkthrough:** `step_order_settlement`: the order names its receiving
  cost center.
- **Acceptance:** the receiver is an active cost center of the order's
  company, valid today; the rule is replaced with the order's version and
  audited.
- **Depends on:** T60a.

**Later for Settlement rules (after 1 December):** several receivers by
percentage, equivalence number or amount; receivers other than cost centers
(a G/L account, an asset under construction, another order, a profit
center); validity periods; settlement cost elements; source structures.

### T60e Order settled and closed

**Capability.** A controller settles an order's actual costs up to a period
to its receiver and closes the settled order.

- **Reuses:** `ledger.Postings.Post` with `source_context` `controlling`
  (per account: credit the order, debit the receiving cost center, so each
  account's balance is unchanged, as the manual reposting between centers of
  ADR-0050); T60b's actuals; period checks.
- **Adds:** table `controlling.order_settlement`;
  `POST /v1/internal-orders/{id}:settle`,
  `GET /v1/internal-orders/{id}/settlements`,
  `POST /v1/internal-orders/{id}:close`; event `InternalOrderSettled`;
  permission `controlling.order.settle`.
- **Walkthrough:** `step_order_settlement` extended: the order is settled;
  its balance is zero, the cost center report shows the cost, and the order
  is closed.
- **Acceptance:** the entry balances and moves exactly the order's
  unsettled balance per account; settling again with nothing new posts
  nothing; an order without a rule is refused; a closed order takes no
  posting; closing needs a zero balance.
- **Depends on:** T60b, T60d.

**Later for Settlement (after 1 December):** settling many orders in one
run; reversing a settlement; settlement to assets under construction and to
G/L accounts; settlement through a settlement cost element; settlement as a
close task.

### T60f Distribution cycle run

**Capability.** A controller defines a distribution cycle (one sender cost
center, receiver cost centers with fixed percentages) and runs it for a
period: the sender's primary costs of the period move to the receivers on
their original accounts.

- **Reuses:** `ledger.AssignmentBalances.CostCenterActuals` (the sender's
  costs by account), `money.Allocate` (shares that add up exactly),
  `ledger.Postings.Post`.
- **Adds:** tables `controlling.allocation_cycle`,
  `controlling.allocation_receiver`, `controlling.allocation_run`;
  `POST /v1/allocation-cycles`, `GET /v1/allocation-cycles`,
  `GET /v1/allocation-cycles/{id}`, `POST /v1/allocation-cycles/{id}:run`,
  `GET /v1/allocation-runs`, `GET /v1/allocation-runs/{id}`; events
  `AllocationCycleChanged`, `AllocationRunPosted`; permissions
  `controlling.allocation.read`, `.write`, `.run`.
- **Walkthrough:** `step_distribution`: a cycle from one of Maple's cost
  centers to two others at 60 and 40 percent is run for the period; the
  centers' reports show the shares.
- **Acceptance:** percentages add up to 100; sender and receivers are
  active cost centers of one company; the run's entry balances per account
  and the shares add up to the sender's amount to the minor unit; one run per
  cycle and period (a second is refused, a replay answers the same); a sender
  without costs posts nothing.
- **Depends on:** nothing new (T56).

### T60g Assessment cycle run

**Capability.** A controller runs an assessment cycle: the sender's costs of
the period are charged to the receivers in one amount on the cycle's
assessment account, a secondary cost element.

- **Reuses:** T60f's cycle, receivers and run; `cost_element: secondary`,
  which ADR-0050 reserves and `internal/config` refuses until allocations.
- **Adds:** kind `assessment` and the assessment account on
  `controlling.allocation_cycle`; the configuration accepts
  `cost_element: secondary`; a ledger rule that a secondary account takes
  lines only from controlling's entries, with its schema test.
- **Walkthrough:** `step_assessment`: an assessment cycle is run; sender and
  receivers show the assessment account's lines.
- **Acceptance:** the entry debits the receivers and credits the sender on
  the assessment account, in balance; a manual entry on a secondary account
  is refused; the sender's primary costs stay on their accounts.
- **Depends on:** T60f.

**Later for Assessment and distribution cycles (after 1 December):**
shares from statistical key figures or from actual or plan values; fixed
amounts; several senders and segments in a cycle; iterative cycles;
simulating a run; reversing a run; plan allocations; overhead rates and
activity allocation; receivers other than cost centers.

---

## Defaults chosen (for the product owner's digest)

No design document or ADR settles these; each is the simplest behaviour
that makes the main path work, and the slice records it in its ADR.

1. A customer invoice entered in receivables is the accounting record of an
   invoice the company issued; it is not the legal tax invoice and sends no
   e-invoice (ADR-0049 §7: the pilot bills in its own system; T65h).
2. A customer without company data stays postable by manual journal entries
   as today; receivables documents require the company data.
3. A posted customer invoice is not reversed in the first version; it is
   corrected by a credit memo.
4. An incoming payment is applied only in full and exactly: no partial
   payment, no difference, no discount.
5. A customer credit memo naming an invoice may not exceed its gross, as the
   product owner decided for supplier credit memos (2026-11-10).
6. Dunning levels are the tenant's configuration, a list of days overdue;
   none is shipped. No charges, no interest, no letters. An item rises at
   most one level per confirmed run. Credit items are not dunned and reduce
   nothing.
7. The collection worklist is ordered by overdue amount.
8. A credit limit is one amount per customer and company in the company's
   currency, set at once by a person holding the permission; exposure is the
   open items only; no limit means no restriction; nothing is blocked.
9. An internal order is created and released directly by permission, with
   no approval request; its profit center comes from its responsible cost
   center.
10. An order's budget is one overall amount; exceeding it is shown, never
    refused.
11. An order settles to one cost center, on the original accounts, for its
    whole unsettled balance.
12. A cycle has one sender and fixed percentages; a distribution keeps the
    original accounts and an assessment uses one secondary account; one run
    per cycle and period, synchronous, not reversible in the first version.

## Questions (money, tax or accounting rules; none blocks a slice)

1. **Tax point of customer invoices and credit memos** (tax specialist):
   the first version reports output tax by the document's date, as supplier
   invoices do (ADR-0049 §4). Is that right for Canada and the UAE?
2. **Dunning charges, interest and minimum intervals** (product owner):
   not built. Are any wanted before the pilot, and by what rule?
3. **Expected credit losses, IFRS 9** (accounting specialist): no allowance
   is calculated. Which method and rates, if it is to be built?
4. **Credit limit scope and exposure** (product owner): per company or
   across the group, and whether open orders count once sales orders exist
   (week 2).
5. **Order budget** (product owner): should a posting that exceeds the
   budget be refused or warned about?
6. **Settlement and assessment accounts** (accounting specialist): is
   settlement on the original accounts acceptable, and a secondary cost
   element kept as an account in the chart?
7. **Credit memo limit for customers** (product owner): default 5 mirrors
   the supplier rule; confirm.
