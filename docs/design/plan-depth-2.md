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
