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
