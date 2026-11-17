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
