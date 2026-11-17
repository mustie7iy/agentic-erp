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
