# Breadth-first plan, week 2 (18–24 November 2026)

**Status:** plan (product owner, 2026-11-10: breadth first). It details the
tracker's week 2 placeholders into slices (ADR-0053) and is frozen once
merged: each placeholder is split once, here. `docs/tracker.json` stays the
only plan (07 §7.5); each leaf's `r` names its section below. It follows
`plan-breadth-week1.md`, whose rules it does not repeat where they are
unchanged.

The decision: every capability in the required scope gets a first usable
version end to end by 2026-12-01: the main path only, one or two slices
each, each sized for one agent in about half a day and fully gated (07
§7.7). Variants and depth come after 1 December; each placeholder's
"later: …" leaf names what was left out on purpose and is not picked up
before then.

Week 2 is the sales chain from quotation to cash, revenue recognition, the
credit limit enforced on orders, RFQ to award, scheduling agreements and
catalogs. Seventeen placeholders are split: Quotations, Sales orders and
Order-to-delivery flow (Quotations and sales orders); Condition types and
pricing procedures in configuration and Price determination (Pricing);
Billing documents and Revenue and receivable postings (Billing); Order
credit blocks (Credit management); Revenue contracts and performance
obligations, Price allocation and Recognition schedules (Revenue
recognition); RFQ events, Bid comparison agent and Award to contract
(Sourcing, RFQ and bid analysis); Scheduling agreements (Contracts and
scheduling agreements); Catalog connector (punch-out) and Requisitions from
catalogs (Catalogs and guided buying). They give 20 slices and 17 "later"
leaves; the agent of the Bid comparison agent placeholder stays an open
leaf of its own for week 3, which takes the thin agents.

Pricing's two placeholders are split here because the chain needs them: a
quotation or an order has to be priced. Rebates, which settle after
billing, are not on the week 2 list and are left (see "Open leaves").

## What every slice reuses

What `plan-breadth-week1.md` lists under the same heading applies
unchanged: the unit of work and `reqctx.Request`, RLS, `money.Minor` and
`money.Allocate`, row locks and state guards, `Idempotency-Key` and
`If-Match`, financial effects only through `ledger.Postings`, the context's
outbox and inbox, an audit record per state change, `problem.*` errors,
contract first, real PostgreSQL tests with `pgtest` and `pgtest.FailingTx`,
a schema test per database rule, a budget for a new report read, and one
new or extended step of `scripts/walkthrough.sh`. Headless first: no web
workspace work (ADR-0053 g).

Week 2 adds three things every slice here holds to:

- an item's value is quantity × unit price ÷ price unit, computed with
  `money.Scale` and summed with `money.Sum`, and a quantity is checked with
  `money.PositiveQuantity`, as `internal/procurement/common.go` does
  (`quantity`, `itemsTotal`); nothing of it is copied;
- a document dated "today" takes the company's date in its time zone
  (`config.Config.CompanyDay`), never UTC's;
- events name a customer or a supplier by id and number, never by name
  (product owner, 2026-11-05).

## Boundaries with the areas outside the required scope

Supply chain, Manufacturing, Assets/service/EHS, Projects, Human capital
and the industry packs are outside the required scope. Where the sales
chain touches them, the plan stops at the boundary or uses the thinnest
stand-in inside the required scope, and no leaf of those areas is touched:

- **No material master** (Supply chain › Material master). A quotation
  or order item names what is sold by a description and an optional
  free-text `item_key` (T78a adds it; T80a's prices are found by it), with
  its unit of measure from `ref`. RFQ and catalog items name what is
  bought as procurement's items already do, by description and
  `material_key` (`internal/procurement/requisitions.go`, `Item`).
  `material_id` is not added to the new tables; 03 §3.16's shared
  `material` master joins them when it is built.
- **No stock, picking, goods issue or availability check** (Supply chain ›
  Shipping and delivery, Inventory management, Available-to-promise). T78c
  is a **delivery confirmation** kept in the sales context: a person
  records that quantities of an order's items were delivered, or a service
  made available, on a date. It moves no stock and posts nothing. Billing
  and revenue recognition read it.
- **No cost of goods sold.** Without stock and its valuation (Supply chain ›
  Inventory valuation) a delivery posts no cost. See question 3.

## Build order and lanes

Three lanes; a slice starts when the slices it depends on are in `main`, or
from their lane's tip as a stacked base.

- **Lane A, sales documents** (new `internal/sales`: quotations, orders,
  delivery confirmations, billing documents; in `internal/receivables` only
  the invoice's input and its source, for T79b; the port to credit and its
  wiring for T77b): T78a → T78b → T78c → T79a → T79b → T77b.
- **Lane B, catalogs, pricing, revenue** (`internal/procurement`: a new
  catalog file and the requisition item's reading of it; `internal/sales`:
  new pricing files and the one function of T78a and T78b that prices an
  item; `internal/config` for T80b and T81d; new `internal/revenue`):
  T84a → T84b → T80a → T80b → T81a → T81b → T81c → T81d. T80a and T81a
  need T78b; T81c needs T78c; T81d needs T79b.
- **Lane C, sourcing** (`internal/procurement`: new RFQ files, and for
  scheduling agreements `contracts.go` with a new schedule file):
  T82a → T82b → T82c → T82d → T83a → T83b. T83a and T83b need nothing from
  the RFQ slices, so they may be pulled forward.

By day: day 1 (18 November) T78a, T78b ∥ T84a, T84b ∥ T82a, T82b; day 2
T78c, T79a ∥ T80a, T80b ∥ T82c, T82d; day 3 T79b, T77b ∥ T81a, T81b ∥ T83a,
T83b; day 4 T81c, T81d. Days 5 to 7 are left for what the batches' gates
and the reviews send back.

What merges first: on day 1 lane C before lane B. Both add to
`internal/procurement`: each slice keeps its handlers in a file of its
own (`catalog_api.go`, `rfq_api.go`, `schedule_api.go`, as
`grir_clearing_api.go` does), so `api.go` changes only in T84b (the
requisition item's conversion) and in T83a (the contract's kind); they
share the `Endpoints` value and its wiring. From day 2 lane A before lane
B, which stacks on it:
T80a changes the function that prices an item in the quotation and order
files, which lane A does not change after T78b; T81a to T81d only read the
sales context through `sales.Orders`, to which lane A's own slices add
what they build (T78c the deliveries, T79a and T79b the billing). T77b
waits for T77a (`internal/credit`, ADR-0066,
batch 68) to be in `main`. `api/openapi.yaml`, `api/gen`, migration numbers,
`internal/app` wiring, `internal/layout`'s import table,
`scripts/walkthrough.sh` and the tracker are shared by every slice and
resolved at integration as always (`make generate`, renumbering).

Step numbers: T78–T84 are new; nothing on the base uses them (the highest
in use is T77a). T77b details week 1's step T77. Integration renames them if
another plan took the same numbers.

## New contexts

- **`sales`** (T78a): quotations, sales orders, delivery confirmations,
  billing documents and prices. It imports `config`, `ledger`, `partner`,
  `tenancy` and `receivables` and reads them through the ports it declares,
  as receivables reads its own (`internal/receivables/receivables.go`). It
  posts nothing itself: billing posts through receivables (ADR-0063 §1: "it
  is not a leaf: week 2's billing posts through it"). 02 §2.2 and 03 §3.16
  name `sales` among the later contexts.
- **`revenue`** (T81a): revenue contracts, their performance obligations
  and recognition runs. A leaf (ADR-0051 §2): it reads the sales context,
  the ledger, tenancy and the configuration through ports the composition
  root wires, posts through `ledger.Postings`, and nothing imports it.

RFQs, supplier quotations, schedule lines and catalog items are new tables
of the existing `procurement` context.

## ADRs

- Sales (quotation, order, delivery confirmation, billing, prices): **new
  ADR-0067, written by T78a**. T78b to T79b and T80a, T80b record what they
  build in its "As built".
- Order credit block: no new ADR. T77b records it in ADR-0066's "As built"
  (the check that blocked nothing now blocks an order) and in ADR-0067's.
- Revenue recognition: **new ADR-0068, written by T81a**, with the IFRS 15
  paragraphs of the section "Revenue recognition: what the standard is
  taken to say" below, each marked as confirmed by the accounting
  specialist or not.
- Sourcing, RFQ to award: **new ADR-0069, written by T82a**.
- Scheduling agreements: **new ADR-0070, written by T83a** (a kind of
  contract with schedule lines called off as purchase orders).
- Catalogs: **new ADR-0071, written by T84a**.

0062 is held by the parked SaaS ADR and 0066 is credit limits (T77a).
Integration renumbers on a clash.

---

## Lane A, sales documents

### T78a Quotation to a customer

**Capability.** A sales representative creates a quotation for a customer
(valid until a date; items by description and optional item key with
quantity, unit, unit price, revenue account and output tax code), reads it
back with its net value, and
records that the customer accepted or rejected it.

- **Reuses:** `partner.Postings.PostingBlock` with kind `customer` (the
  customer by role, as `receivables` checks one at entry),
  `partner.Payees.Names`, `ledger.Accounts.IsPostable` and
  `ledger.Accounts.TaxCode` (the item's account and output code), the
  configuration's number ranges, `money.Scale`, `money.Sum`,
  `money.PositiveQuantity`, `currency.ParseAmount`, `problem.Invalid`,
  `etag`.
- **Adds:** the context `sales` (schema, RLS, outbox and inbox, its row in
  `internal/layout`); tables `sales.quotation`, `sales.quotation_item`;
  `POST /v1/quotations`, `GET /v1/quotations`, `GET /v1/quotations/{id}`,
  `POST /v1/quotations/{id}:accept`, `POST /v1/quotations/{id}:reject`;
  event `QuotationChanged`; permissions `sales.quotation.read`, `.write`;
  number range `quotation`; ADR-0067.
- **Walkthrough:** `step_quotation`: a quotation to the customer of
  `step_customer` is created, read back with its net value and accepted.
- **Acceptance:** created idempotently and listed by customer and status;
  each item's value and the net total are exact in minor units; an unknown
  customer, account, unit or tax code is refused naming the request's own
  field; accepting moves `open` to `accepted` once (row lock, version; a
  replay answers the same); a quotation past its validity on the company's
  date is not accepted (409); nothing is posted.
- **Depends on:** T74a (in `main`).

**Later for Quotations (after 1 December):** changing a quotation and its
versions; the quotation as a document sent to the customer (output
management); the tax shown on the quotation; alternative items; follow-up
and win probability; quotations from the CRM and CPQ connectors; approval
of a quotation below a margin.

### T78b Sales order confirmed

**Capability.** A sales representative creates a sales order, from an
accepted quotation or directly, and confirms it; the order shows each
item's ordered, delivered and billed quantities.

- **Reuses:** T78a's item (one shape and one pricing function for both
  documents), `partner.Customers.Customer` and `Customer.Usable` (the
  customer's company data: reconciliation account and payment term),
  `partner.Suppliers.PaymentTerm` (a configured term), the number ranges.
- **Adds:** tables `sales.sales_order`, `sales.sales_order_item`;
  `POST /v1/sales-orders` (with `quotation_id` or items),
  `GET /v1/sales-orders`, `GET /v1/sales-orders/{id}`,
  `POST /v1/sales-orders/{id}:confirm`; event `SalesOrderChanged`;
  permissions `sales.order.read`, `.write`, `.confirm`; number range
  `sales_order`; a published read `sales.Orders` (an order with its items,
  as `procurement.Orders` is for payables) that T81a reads.
- **Walkthrough:** `step_sales_order`: an order is created from the
  quotation of `step_quotation` and confirmed.
- **Acceptance:** an order from a quotation copies its customer, currency
  and items, and a quotation gives one order (a second is refused, 409); an
  order needs the customer's company data in the company (422, as a
  receivables document does); the payment term is the order's, else the
  customer's; confirming moves `draft` to `confirmed` once; the order's net
  value is exact; audited and published; nothing is posted.
- **Depends on:** T78a.

**Later for Sales orders (after 1 December):** changing or cancelling an
order and its versions; sales organizations and sales areas; partner
functions (ship-to, bill-to, payer); order types; schedule lines and
requested delivery dates per item; incompletion checks; approval of an
order; the order confirmation as a document; custom fields; orders from
the capture agent and EDI (their own leaves); taking open orders over from
SAP.

### T78c Delivery confirmed against the order

**Capability.** A sales administrator confirms that quantities of a
confirmed order's items were delivered on a date (or, for a service, that
it was made available); the order shows what is delivered and what is
still open.

- **Reuses:** T78b's order under its row lock (`FOR UPDATE`), the quantity
  checks, audit, outbox. The pattern is a goods receipt's against a
  purchase order (`internal/procurement/receipts.go`: quantities against
  the open quantity under the order's lock), without its stock or posting.
- **Adds:** tables `sales.delivery`, `sales.delivery_item` (insert-only);
  `POST /v1/sales-orders/{id}/deliveries`,
  `GET /v1/sales-orders/{id}/deliveries`; event `SalesOrderDelivered`;
  permission `sales.delivery.confirm`; `delivered_quantity` per order item
  in the order's representation; in `sales.Orders` each item's deliveries
  (date and quantity), which T81c reads.
- **Walkthrough:** `step_sales_order` extended: the order's items are
  confirmed as delivered; the order reads as fully delivered.
- **Acceptance:** only a confirmed order takes a delivery; a quantity above
  the item's open quantity is refused naming the item (422); two
  deliveries racing for the last quantity deliver it once; the delivery
  date is not after the company's date today; a replay answers the same
  delivery; no stock moves and nothing is posted.
- **Depends on:** T78b.

**Later for Order-to-delivery flow (after 1 December):** the outbound
delivery document, picking, packing and goods issue with their stock
movement and cost of goods sold (Supply chain › Shipping and delivery,
outside the required scope); availability check; delivery blocks; partial
delivery rules and tolerances; reversing a confirmation; proof of
delivery; delivery notes (output management).

### T79a Billing document created

**Capability.** A billing clerk creates a billing document for an order:
its items are the order's delivered quantities not yet billed, at the
order's prices, and it reads back with its net value.

- **Reuses:** T78c's delivered quantities, the order's row lock, the number
  ranges, T78a's item value (the order item's net value, after T80b's
  discounts once they exist).
- **Adds:** tables `sales.billing_document`, `sales.billing_document_item`;
  `POST /v1/billing-documents` (order, billing date),
  `GET /v1/billing-documents`, `GET /v1/billing-documents/{id}`; event
  `BillingDocumentChanged`; permissions `sales.billing.read`, `.write`,
  `.post`; number range `billing_document`; `billed_quantity` per order
  item in the order's representation.
- **Walkthrough:** `step_billing`: a billing document is created for the
  delivered order of `step_sales_order`; its items and net value are the
  order's.
- **Acceptance:** each item bills the delivered quantity less what earlier
  billing documents of the order took, under the order's lock, so two
  requests never bill a quantity twice; an item's amount is the order
  item's net value × the quantity billed so far, this document included,
  ÷ the ordered quantity, less what earlier documents billed, so the
  documents of a fully billed item add up to its net value to the minor
  unit, the last taking the remainder; an order with nothing to bill is
  refused (422); a replay answers the same document; nothing is posted.
- **Depends on:** T78c.

**Later for Billing documents (after 1 December):** cancelling a billing
document; collective billing of several orders and invoice splits; billing
plans, milestones and down payment requests; pro forma invoices; billing
without delivery (order-related billing of services); intercompany
billing; the billing due list as a scheduled run; the invoice as a
document sent to the customer and as an e-invoice (output management,
T65h).

### T79b Billing posted through receivables

**Capability.** The billing clerk posts the billing document: a customer
invoice is entered and posted in receivables in the same transaction, and
it is the customer's open item, ready for dunning, the aging and an
incoming payment.

- **Reuses:** `receivables.Endpoints.CreateInvoice` and `PostInvoice`
  (`internal/receivables/invoices.go`, `post.go`: items by revenue account
  with net amount and output tax code, tax at the invoice date, the
  customer's reconciliation account and terms, `ledger.Postings.Post` with
  `source_context` `receivables`), called in process through a port the
  sales context declares; T75c's application of an incoming payment,
  unchanged.
- **Adds:** `POST /v1/billing-documents/{id}:post`; event
  `BillingDocumentPosted`; on `receivables.Input` and
  `receivables.customer_invoice` the document the invoice came from
  (`source_document_id`, `source_document_number`, both optional), shown on
  the customer invoice; `customer_invoice_id` and the invoice's number on
  the billing document; in `sales.Orders` each item's posted billing (net
  amount and posting date), which T81d reads. No new table.
- **Walkthrough:** `step_billing` extended: the document is posted; the
  customer invoice names it; receivables, revenue and output tax moved by
  its amounts; a statement credit for its gross is applied to it as
  `step_incoming_payment` does, and the customer has no open item left
  from the order.
- **Acceptance:** posting moves `created` to `posted` once (row lock and
  state guard; a second post is refused, a replay answers the same); the
  invoice's items are the billing document's, one for one, with the
  order's payment term; a refusal by receivables (a blocked customer, a
  closed period, a tax code without a rate) leaves the billing document
  unposted and is returned naming the billing document's own fields; the
  invoice's date is the billing date, which is its tax date (ADR-0063 §4).
- **Depends on:** T79a, T75b (in `main`).

**Later for Revenue and receivable postings (after 1 December):** revenue
account determination by customer and item group in place of the account
on the item; reversal of a posted billing document; credit and debit
memos from billing (Returns and credit memos, not split here); posting to
a profit center or segment derived from the sales organization; cost of
goods sold; rebate accruals; a tax date other than the billing date.

### T77b Order credit block

**Capability.** When a sales order is confirmed, its value is checked
against the customer's credit limit: an order that takes the customer
over the limit is confirmed with a credit block, cannot be delivered, and
a credit manager releases it.

- **Reuses:** `credit.Endpoints.ExposureOf` (T77a, ADR-0066: the
  customer's open items against its limit, and whether an amount is
  within it) through a port the sales context declares and the composition
  root wires, since `credit` is a leaf; `ledger.Rates.ExactToCompany` for
  an order in another currency than the company's; T78b's confirmation and
  T78c's delivery under the order's lock.
- **Adds:** `credit_block` on `sales.sales_order` (none, blocked, released,
  with who released it and when);
  `POST /v1/sales-orders/{id}:release-credit-block`; events
  `SalesOrderCreditBlocked`, `SalesOrderCreditReleased` (customer id and
  number, no name); permission `sales.order.credit_release`; a filter of
  the order list by credit block.
- **Walkthrough:** `step_credit_block`: with the limit of
  `step_credit_limit`, an order within what is available confirms without
  a block; a larger one confirms blocked, its delivery is refused, the
  credit manager releases it and the delivery is confirmed.
- **Acceptance:** the order's net value, in the company's currency, is
  checked with the exposure read in the confirming transaction; a
  customer without a limit is never blocked; a blocked order refuses a
  delivery (409 naming the block) and so cannot be billed; release moves
  blocked to released once, needs the permission and is audited; no rule
  sets the releaser apart from the person who confirmed the order
  (question 11); an order already confirmed is not checked again.
- **Depends on:** T78c, T77a.

**Later for Order credit blocks (after 1 December):** open orders,
deliveries and unposted billing in the exposure (T77a's later leaf);
checking again at delivery and when an order changes; automatic release
when a payment brings the customer within the limit; rejecting a blocked
order; the order's tax in the amount checked; a worklist of blocked
orders with their ages; approval of a release above an amount.

---

## Lane B, catalogs, pricing, revenue

### T84a Catalog items

**Capability.** A catalog manager loads a supplier's catalog items in a
company (item key, description, unit, price, optionally the contract item
that prices it), and a requester searches the company's catalog by text
and supplier.

- **Reuses:** `internal/procurement` (`e.legalEntity`, `e.supplier` for a
  supplier usable in the company, `amount`, `exponent`, the `ref` unit of
  measure foreign key and its `unknownUnit` problem, `referencedContract`
  for a named contract), cursor paging, `etag`.
- **Adds:** table `procurement.catalog_item`; `POST /v1/catalog-items`,
  `GET /v1/catalog-items` (text, supplier, status),
  `GET /v1/catalog-items/{id}`, `POST /v1/catalog-items/{id}:retire`;
  event `CatalogItemChanged`; permissions `procurement.catalog.read`,
  `.manage`; ADR-0071.
- **Walkthrough:** `step_catalog`: two items of the walkthrough's supplier
  are loaded and found by a search on a word of their description.
- **Acceptance:** an item key is unique per company and supplier among
  active items; the supplier must be usable in the company, as an order's
  must be; a named contract item must be of an active contract of that
  company and supplier, in the item's unit and currency; retiring moves
  active to retired once; the search is paged in a stable order and
  returns no retired item unless asked; a budget test for the search.
- **Depends on:** nothing.

**Later for Catalog connector (punch-out) (after 1 December):** punch-out
to a supplier's catalog (cXML PunchOut, OCI) and its connector; loading a
catalog file in bulk and by schedule; changing an item's price with
validity dates; categories and their default accounts; images and
attachments; a catalog shared by several companies or kept per
purchasing organization; search ranking.

### T84b Requisition from the catalog

**Capability.** A requester adds a catalog item to a requisition with a
quantity: description, unit, price and supplier come from the catalog, and
the requisition goes through approval to a purchase order as any other.

- **Reuses:** `requisitionItems` and `ItemInput`
  (`internal/procurement/requisitions.go`), `checkRequisitionContract`,
  the requisition's submission, approval and ordering as they are
  (`submitRequisition`, `orderedRequisition`).
- **Adds:** `catalog_item_id` on the requisition item's input and on
  `procurement.requisition_item` (nullable, with its foreign key); no new
  endpoint.
- **Walkthrough:** `step_catalog` extended: a requisition is created from
  one catalog item, read back with the catalog's price and supplier, and
  submitted.
- **Acceptance:** the item takes the catalog item's description, item key,
  unit and price, and a price sent with it is refused (422); the
  requisition's supplier, and its contract when the catalog item names
  one, are the catalog item's, and another is refused; a retired item, or
  one in another currency than the requisition's, is refused naming the
  item; the account and cost center are the requester's to give, as today.
- **Depends on:** T84a.

**Later for Requisitions from catalogs (after 1 December):** a shopping
cart across suppliers split into one requisition per supplier; the
account proposed from the catalog category; preferred suppliers and
guided buying rules; a requisition approved straight to a purchase order
for catalog items under a contract; favourites and order templates.

### T80a Prices and their determination

**Capability.** A pricing analyst records prices for item keys, for every
customer or for one, with validity dates; a quotation or order item that
names an item key without a price takes the price valid on the document's
date.

- **Reuses:** T78a's item and its pricing function (the one place both
  documents price an item), `partner.Postings.PostingBlock` for a
  customer-specific price's customer, `currency.ParseAmount`, the `ref`
  unit of measure.
- **Adds:** table `sales.price` (company, item key, optional customer,
  unit, unit price, price unit, currency, valid from and to; insert-only);
  `POST /v1/sales-prices`, `GET /v1/sales-prices`,
  `GET /v1/sales-prices/{id}`; event `SalesPriceChanged`; permissions
  `sales.price.read`, `.write`; `price_source` (`manual` or `price_list`)
  on quotation and order items.
- **Walkthrough:** `step_price_list`: a general price and a price for the
  customer of `step_customer` are recorded for one item key; a quotation
  item naming the key without a price takes the customer's price, and
  another customer's takes the general one.
- **Acceptance:** the customer's price valid on the document date wins
  over the general one, and among several the latest `valid_from`; unit
  and currency must be the item's; an item with neither a price nor a
  valid price record is refused naming the item (422); a price sent with
  the item stays and reads as `manual`; the price is fixed on the item
  when it is created and a later price record does not change it.
- **Depends on:** T78b.

**Later for Price determination (after 1 December):** scales by quantity
or value; prices per customer group, price list type or sales
organization; surcharges and freight; minimum prices and manual price
approval; currency conversion of a price; repricing a document; mass
changes and price uploads; prices taken over from SAP.

### T80b Condition types and the pricing procedure in configuration

**Capability.** A configuration administrator defines condition types (a
price, a percentage discount) and the order they apply in; a pricing
analyst records a customer's discount, and quotation and order items show
each condition applied and the net value after it.

- **Reuses:** the configuration document, its validation and tiers
  (`internal/config`: `validate.go`, `diff.go`), T80a's price records and
  determination, `money.Scale` (a decimal percentage of an amount, as
  `money.Percent` is for whole ones).
- **Adds:** the configuration section `pricing` (`condition_types[]` with a
  code, a name and a kind `price` or `discount_percent`; `procedure[]`,
  their order); `condition_type` and, for a discount, `percent` on
  `sales.price`; `conditions[]` (type, rate, amount) on quotation and
  order items. No new endpoint.
- **Walkthrough:** `step_price_list` extended: a pricing procedure with a
  price and a customer discount is activated; a discount of five percent
  is recorded for the customer; a new quotation item shows the price, the
  discount and the net value.
- **Acceptance:** the procedure names each condition type once, a price
  first; a discount is a percentage between 0 and 100 of the value before
  it, rounded half away from zero; the item's
  net value is its value less its discounts, exact in minor units; a
  record naming an unknown condition type is refused; a tenant without a
  pricing procedure prices as T80a does; a condition type is never removed
  once activated, as a payment term is not (`partner.PaymentTerms.Check`).
- **Depends on:** T80a.

**Later for Condition types and pricing procedures (after 1 December):**
several procedures chosen by customer and document type; access
sequences; fixed-amount and quantity-based conditions; header conditions;
condition exclusion; statistical conditions; tax as a condition; rebates
(their own leaves); pricing configuration generated from SAP's condition
tables.

### T81a Revenue contract with its performance obligations

**Capability.** A revenue accountant creates a revenue contract for a
confirmed sales order, with one performance obligation per order item, and
reads it back: the customer, the transaction price and each obligation.

- **Reuses:** `sales.Orders` (T78b) through a port, tenancy's legal
  entities, `partner.Payees.Names`, `etag`, audit, outbox.
- **Adds:** the leaf context `revenue` (schema, RLS, outbox, its row in
  `internal/layout`); tables `revenue.contract`,
  `revenue.performance_obligation`; `POST /v1/revenue-contracts` (sales
  order), `GET /v1/revenue-contracts`, `GET /v1/revenue-contracts/{id}`;
  event `RevenueContractChanged`; permissions `revenue.contract.read`,
  `.write`; ADR-0068.
- **Walkthrough:** `step_revenue_contract`: a second order with two items
  (goods, and a support service for the current fiscal year) is confirmed
  and a
  revenue contract is created for it; it reads back with two obligations
  and the order's net value as its transaction price.
- **Acceptance:** one contract per sales order (a second is refused, 409;
  a replay answers the same); only a confirmed order in the company's
  local currency (422 otherwise); the contract is a draft; each
  obligation starts as satisfied at a point in time with the order item's
  net value as its stand-alone selling price; the transaction price is
  the order's net value, without tax; nothing is posted.
- **Depends on:** T78b.

**Later for Revenue contracts and performance obligations (after 1
December):** combining several orders into one contract and one order
into several; obligations that are not order items (bundles, implied
promises, options and material rights, warranties); contract
modifications; contracts in a foreign currency; contracts created without
a sales order or automatically when an order is confirmed; principal or
agent; costs to obtain and fulfil a contract; disclosures; taking
contracts over from SAP RAR.

### T81b Price allocation

**Capability.** The revenue accountant sets, per obligation, whether it is
satisfied at a point in time or over time (with its service period) and
its stand-alone selling price, and activates the contract: the transaction
price is allocated to the obligations in proportion to those prices.

- **Reuses:** `money.Allocate` (shares that add up exactly), `etag` and
  `If-Match`, T81a's contract under its row lock.
- **Adds:** `PATCH /v1/revenue-contracts/{id}`,
  `POST /v1/revenue-contracts/{id}:activate`; `timing`, `service_from`,
  `service_to`, `standalone_selling_price` and `allocated_amount` on the
  obligation; permission `revenue.contract.activate`. No new table.
- **Walkthrough:** `step_revenue_contract` extended: the service is set to
  over time, the fiscal year of the company's date today as its service
  period (so the step needs no other year's calendar), with a stand-alone
  selling price above
  its order price; the contract is activated; the two allocated amounts
  add up to the transaction price and differ from the order's item
  values as the proportion gives.
- **Acceptance:** an over-time obligation needs a service period, from not
  after to; a stand-alone selling price is positive; the allocated amounts
  add up to the transaction price to the minor unit; activation moves
  draft to active once and fixes obligations and amounts (a later patch is
  refused, 409); with the default prices the allocation equals the order's
  item values.
- **Depends on:** T81a.

**Later for Price allocation (after 1 December):** variable consideration
and its constraint; a discount or variable amount allocated to some
obligations only; a significant financing component; non-cash
consideration and consideration payable to a customer; estimating
stand-alone selling prices (adjusted market, cost plus margin, residual)
and keeping them as master data; reallocation after a modification or a
change of the transaction price.

### T81c Recognition schedule

**Capability.** The revenue accountant reads an active contract's
recognition schedule: per obligation and fiscal period, the revenue
recognized so far and planned.

- **Reuses:** `ledger.Calendar.Periods` (the company's fiscal periods),
  `sales.Orders` with the deliveries T78c put there (date and quantity
  per item), `money.Allocate`.
- **Adds:** `GET /v1/revenue-contracts/{id}/schedule`; no table: the
  schedule is computed on read from the obligations and the deliveries.
- **Walkthrough:** `step_revenue_schedule`: after the goods of
  `step_revenue_contract`'s order are confirmed as delivered, the
  schedule shows the goods' allocated amount in the delivery's period and
  the service's spread over the fiscal year's periods by their days.
- **Acceptance:** a point-in-time obligation's amount falls in the periods
  of its deliveries, in proportion to the quantity delivered, the last
  delivery taking the remainder; an undelivered quantity is shown as not
  yet scheduled; an over-time obligation's amount is spread over the days
  of its service period, each fiscal period taking its days' share, the
  shares adding up to the allocated amount exactly; a period outside the
  company's calendar is refused naming the obligation.
- **Depends on:** T81b, T78c.

### T81d Recognition run posted

**Capability.** The revenue accountant runs revenue recognition for a
company and a period: for each active contract, revenue is brought to
what is recognized to the period's end, and the contract's position, the
revenue recognized less the revenue billed to that date, stands as one
contract asset when it is positive or one contract liability when it is
negative.

- **Reuses:** T81c's schedule, `sales.Orders` with the posted billing
  T79b put there (net amount and posting date per item),
  `ledger.Postings.Post` with `source_context` `revenue`,
  `ledger.Accounts.Determination` and `companies[].account_determination`
  (`internal/config/config.go`, `AccountDetermination`),
  `ledger.Calendar.Period`, a bound on one synchronous run as
  `collections.MaxRunItems` is.
- **Adds:** tables `revenue.recognition_run`, `revenue.recognition_posting`
  (per run and contract: the cumulative amounts and the entry);
  `POST /v1/revenue-recognition-runs` (company, fiscal year and period),
  `GET /v1/revenue-recognition-runs`,
  `GET /v1/revenue-recognition-runs/{id}`; event `RevenueRecognized`;
  permission `revenue.recognition.run`; account determination keys
  `contract_liability` and `contract_asset`.
- **Walkthrough:** `step_revenue_schedule` extended: the service of
  `step_revenue_contract`'s order is confirmed as made available (T78c),
  so the whole order is delivered; it is billed in full and posted; the
  run for the current period posts one entry; the service's revenue
  account shows the share of the year's days up to the period's end, the
  goods' account the goods' allocated amount, and the contract liability
  the rest of what was billed.
- **Acceptance:** per obligation the entry moves the item's revenue
  account by the cumulative revenue recognized less the cumulative
  revenue billed, less what earlier runs posted for it; the contract's
  position is the sum of those cumulative differences, and the entry
  takes the position the last run left off its account and puts the new
  one on the contract asset account when it is a debit or the contract
  liability account when it is a credit, so a contract never shows both
  (a test takes one contract from a liability in one period to an asset
  in the next); the entry balances; a contract whose figures did not move
  posts nothing; one run per company and period (a second is refused, a
  replay answers the same); a company's first run is for any open period
  and each later run for a later period than its last; a document dated
  into a period already run is caught up by the next run, since the
  amounts are cumulative; a company without the two accounts is refused
  (422); the period must be open.
- **Depends on:** T81c, T79b.

**Later for Recognition schedules (after 1 December):** measures of
progress other than time elapsed (output measures, costs incurred,
milestones); recognition at a point other than the delivery confirmation;
reversing a run; the run as a close task and as a workflow for large
volumes; reclassifying between contract asset and receivable; remaining
performance obligations and the other IFRS 15 disclosures; contract
liabilities and assets in the cash forecast and in analytics; a parallel
ledger under another accounting principle.

---

## Lane C, sourcing

### T82a RFQ created and published

**Capability.** A buyer creates a request for quotation (company,
currency, a date quotations are due, items with quantity and unit, the
suppliers invited) and publishes it.

- **Reuses:** `internal/procurement` (`ItemInput` without a price, the
  `quantity` check, the check behind `e.supplier` for each invited
  supplier, its refusal naming that supplier's own field, `e.account`,
  `e.number`, `e.todayOf`, the status transition pattern of
  `changeContract`), `partner.Payees.Names` through the `SupplierNames`
  port.
- **Adds:** tables `procurement.rfq`, `procurement.rfq_item`,
  `procurement.rfq_supplier`; `POST /v1/rfqs`, `GET /v1/rfqs`,
  `GET /v1/rfqs/{id}`, `POST /v1/rfqs/{id}:publish`; events `RfqChanged`,
  `RfqPublished` (the invited suppliers by id and number); permissions
  `procurement.rfq.read`, `.manage`; number range `rfq`; ADR-0069.
- **Walkthrough:** `step_rfq`: an RFQ for two items is created with two
  invited suppliers and published.
- **Acceptance:** at least one item and one supplier; each supplier is
  usable in the company, as an order's must be, and named once; the due
  date is not before the company's date today; publishing moves draft to
  published once (row lock, version); a published RFQ is not changed;
  audited and published.
- **Depends on:** nothing.

### T82b Supplier quotations recorded

**Capability.** The buyer records each invited supplier's quotation
against the RFQ: a price per RFQ item, the supplier's reference and how
long the quotation is valid.

- **Reuses:** T82a's RFQ under its row lock, `amount`, `exponent`,
  `money.Scale` and `money.Sum` for the quotation's total.
- **Adds:** tables `procurement.supplier_quotation`,
  `procurement.supplier_quotation_item`; `POST /v1/rfqs/{id}/quotations`,
  `GET /v1/rfqs/{id}/quotations`; event `SupplierQuotationRecorded`
  (supplier id and number).
- **Walkthrough:** `step_rfq` extended: a quotation is recorded for each of
  the two suppliers, one of them cheaper in total.
- **Acceptance:** only a published RFQ takes quotations; the supplier must
  be one invited, and has one quotation (a second is refused, 409; a
  replay answers the same); prices are in the RFQ's currency, positive,
  per the RFQ item's unit; a quotation may leave items out and reads as
  incomplete; its total is exact in minor units.
- **Depends on:** T82a.

**Later for RFQ events (after 1 December):** an RFQ from a requisition;
suppliers answering themselves (a supplier portal or the supplier
network); several rounds and revised quotations; sealed bids opened at
the due date; auctions; attachments and questions; sending the RFQ as a
document (output management); cancelling or closing an RFQ without an
award; quotations in another currency.

### T82c Quotations compared

**Capability.** The buyer reads the comparison of an RFQ's quotations: per
supplier the total and each item's price, ranked from the lowest total,
with the lowest price per item marked.

- **Reuses:** T82b's quotations and totals; `partner.Payees.Names`;
  stores nothing.
- **Adds:** `GET /v1/rfqs/{id}/comparison`; a budget test.
- **Walkthrough:** `step_rfq` extended: the comparison ranks the cheaper
  supplier first and marks the lowest price of each item.
- **Acceptance:** complete quotations are ranked by total, equal totals in
  a stable order; an incomplete quotation is listed after them, unranked,
  with the items it lacks; an invited supplier without a quotation is
  listed as not having answered; a quotation whose validity ended before
  the company's date today is marked as expired and unranked.
- **Depends on:** T82b.

**The agent** that reads this comparison and recommends an award with
its reasons (it never awards) is not a variant: it stays an open leaf
under this placeholder for week 3, which takes the thin agents.

**Later for Bid comparison (after 1 December):** criteria other than
price (delivery time, payment terms,
supplier evaluation scores) and their weights; comparison against the
last purchase price and the contract price; total cost of ownership;
export of the comparison.

### T82d RFQ awarded to a contract

**Capability.** The buyer awards the RFQ to one quotation: a draft
contract with that supplier is created with the quoted prices, and the
RFQ and its quotations show the outcome.

- **Reuses:** `draftContract` and `NewContract`
  (`internal/procurement/contracts.go`: a draft contract with its items,
  validity and target value, numbered and audited), the contract's own
  release afterwards (`changeContract` with `releaseContract`), T82c's
  rules for which quotation can win.
- **Adds:** `POST /v1/rfqs/{id}:award` (quotation, the contract's validity
  dates); `contract_id` on the RFQ and `rfq_id` on the contract's
  representation; quotation statuses `awarded` and `not_awarded`; event
  `RfqAwarded` (the winning supplier by id and number); permission
  `procurement.rfq.award`.
- **Walkthrough:** `step_rfq_award`: the RFQ of `step_rfq` is awarded to
  the cheaper quotation; the draft contract carries its prices and
  supplier and is released.
- **Acceptance:** only a published RFQ is awarded, once (row lock and
  state guard; a replay answers the same contract); the quotation must be
  complete and valid on the company's date today; the contract's items
  are the RFQ's items at the quoted prices and its target value is the
  quotation's total; the other quotations become `not_awarded`; RFQ,
  quotations and contract change in one transaction.
- **Depends on:** T82b (T82c for the rule on which quotation can win).

**Later for Award to contract (after 1 December):** award to a purchase
order in place of a contract; splitting an award between suppliers or by
item; approval of an award above an amount; a recorded reason when the
lowest quotation does not win; notifying the suppliers not awarded;
withdrawing an award.

### T83a Scheduling agreement with its delivery schedule

**Capability.** A buyer creates a scheduling agreement with a supplier (a
contract of the kind `scheduling_agreement`, its items priced) and, once it
is released, enters its delivery schedule: per item, dates and quantities.

- **Reuses:** the contract as it is (`createContract`, its items,
  validity, target value, release, expiry and renewal;
  `internal/procurement/contracts.go`), `quantity`, the contract's row
  lock (`loadContract`).
- **Adds:** `kind` on `procurement.contract` (`contract`, the default, or
  `scheduling_agreement`) and on `createContract`; table
  `procurement.schedule_line`; `POST /v1/contracts/{id}/schedule-lines`,
  `GET /v1/contracts/{id}/schedule-lines`; a filter of the contract list
  by kind; event `ScheduleLineChanged`; ADR-0070. The permissions are the
  contract's (`procurement.contract.manage`, `.read`).
- **Walkthrough:** `step_scheduling_agreement`: a scheduling agreement
  with one item is created and released, and two schedule lines are
  entered for the coming two months.
- **Acceptance:** only an active scheduling agreement takes schedule
  lines, and a contract of the kind `contract` takes none (409); a line
  names an item of the agreement, a positive quantity and a delivery date
  within the agreement's validity; lines are numbered per agreement and
  listed by date; existing contracts read as kind `contract` and behave
  as before.
- **Depends on:** nothing.

### T83b Schedule line called off

**Capability.** The buyer calls off a schedule line: a purchase order for
its quantity is created against the agreement at the agreement's price,
due on the line's date, and then released, received and invoiced as any
order.

- **Reuses:** `createOrder` with the agreement as the order's contract and
  the item's `ContractItemNo` and `DeliveryDate` (`usableContract`,
  `applyContractPrice`), and, at the order's release, `coveringContract`
  and `consumeContract` (the agreement's target value);
  `internal/procurement/purchase_orders.go`.
- **Adds:** `POST /v1/contracts/{id}/schedule-lines/{line_no}:call-off`;
  `purchase_order_id` and the order's number on the schedule line; event
  `ScheduleLineCalledOff` (supplier id and number). The caller needs
  `procurement.order.create`.
- **Walkthrough:** `step_scheduling_agreement` extended: the first line is
  called off; the draft order has the agreement's price and the line's
  date and quantity; it is released and the agreement's consumed value
  rises by the order's value.
- **Acceptance:** a line is called off once (state guard on the line; a
  second call is refused, a replay answers the same order); the order is
  a draft of the agreement's company, supplier and currency, its item in
  the agreement item's unit and account; an agreement that is not active
  or not valid on the company's date today refuses the call-off; an order
  above the agreement's remaining target value is refused at its release,
  as for any contract.
- **Depends on:** T83a.

**Later for Scheduling agreements (after 1 December):** goods received
against the schedule line itself, without an order; forecast and
just-in-time delivery schedules sent to the supplier (EDI DELFOR,
DELJIT); releases created by MRP; firm and trade-off zones; cumulative
quantities and their reconciliation with the supplier; changing or
deleting a schedule line; target quantities per item; calling off several
lines into one order.

---

## Revenue recognition: what the standard is taken to say

The primary source is IFRS 15 *Revenue from Contracts with Customers*
(IASB). **Every statement below is from memory**: the planning lane has no
network and the repository holds no copy of the standard, so none was
checked against its text. Each names the paragraph for the accounting
specialist to confirm (question 1), and T81a's ADR-0068 marks each as
confirmed or not. Nothing else about IFRS 15 is assumed by the slices.

1. Revenue is recognized when, or as, a performance obligation is
   satisfied by transferring a promised good or service to the customer,
   which happens when the customer obtains control of it (paragraph 31).
2. At contract inception the entity determines, for each performance
   obligation, whether it is satisfied over time or at a point in time
   (paragraph 32; the criteria for over time are in paragraph 35, the
   indicators for a point in time in paragraph 38).
3. The transaction price is the consideration the entity expects to be
   entitled to, excluding amounts collected on behalf of third parties
   (paragraph 47); the plan takes sales tax to be such an amount.
4. The transaction price is allocated to the performance obligations in
   proportion to their stand-alone selling prices at contract inception
   (paragraphs 73, 74 and 76).
5. For an obligation satisfied over time, revenue follows a measure of
   progress (paragraphs 39 to 41); time elapsed is an input measure, and
   recognizing on a straight-line basis may be appropriate when the
   entity's efforts are expended evenly over the period (paragraph B18).
6. A contract is presented as a contract liability when the customer has
   paid, or payment is due, before the entity transfers the good or
   service, and as a contract asset when the entity has transferred it
   before payment is due, apart from amounts presented as a receivable
   (paragraphs 105 to 107).

How the slices use them, and no further:

- The **person** identifies the contract and the obligations (T81a starts
  from one obligation per order item and lets nothing else in), decides
  point in time or over time and enters the stand-alone selling prices
  (T81b). The system does not judge whether an item is distinct, whether
  paragraph 35's criteria are met or what a stand-alone selling price is.
- The **system** does the arithmetic: the allocation of statement 4
  (T81b); for a point-in-time obligation, revenue in proportion to the
  quantity confirmed as delivered (T78c is the first version's evidence
  that control passed; question 2); for an over-time obligation, the
  straight line of statement 5 by days (T81c); the contract liability or
  asset of statement 6 as the contract's one position, revenue recognized
  less revenue billed (T81d).
- An order **without** a revenue contract is recognized as it is billed:
  T79b's invoice credits revenue, and billing bills only delivered
  quantities, so revenue is never recognized before the delivery
  confirmation. It may be recognized later than the delivery when billing
  lags it past a period end; the run of T81d corrects that only for
  orders under a revenue contract (question 4).

---

## Open leaves of these areas left for week 3 or after

Not split here; each stays as it is in the tracker.

- **Sales and service › Customer master: Duplicate checks, Governed
  changes, Governance for customers.** Customer master was week 1's
  (T74a, whose changes already go through the partner change and its
  approval); these are master data governance, not the sales chain.
- **Sales and service › Order intake: Order capture agent.** An agent;
  week 3 takes the thin agents.
- **Sales and service › Order intake: EDI 850 inbound.** A second way to
  enter an order; a variant, after the connector work.
- **Sales and service › Rebates and settlement management (two leaves).**
  Not on the roadmap's week 2 list; rebates build on pricing and billing.
- **Sales and service › Returns and credit memos (two leaves).** The
  reverse chain; not on the week 2 list. T75d's credit memo in receivables
  (week 1) corrects an invoice meanwhile.
- **Sales and service › CRM, CPQ and commerce (two leaves).** Connectors
  to other systems; not on the week 2 list.
- **Sales and service › Customer service and field service; Subscription
  and usage billing (four leaves).** Tracker phases 6 and 7; not on the
  week 2 list.
- **Finance › Credit management: Credit scoring agent.** An agent; week 3.
- **Sourcing and procurement › Sourcing, RFQ and bid analysis: the bid
  comparison agent** (a new leaf under the split placeholder). An agent;
  week 3. T82c gives it the comparison to read.
- **Sourcing and procurement › Contracts and scheduling agreements:
  procurement's other events naming the supplier** (review:batch28). A
  review finding on built code, not a placeholder; not this plan's.
- **Sourcing and procurement › Supplier network (cXML, EDI); Services and
  contingent workforce; Spend analytics.** Not on the week 2 list; spend
  analytics belongs with week 3's analytics.
- **Controlling › Margin and profitability analysis.** Needs billing's
  revenue by customer and item first; with week 3's analytics.

The roadmap gives week 3 to analytics, GRC, retention, the thin agents and
the release candidate. The Sales and service leaves above that are neither
agents nor analytics (rebates, returns, EDI, CRM, service, subscriptions:
eleven leaves) have no week on the roadmap; whether they get a first
version before 1 December is the product owner's to say (question 9).

---

## Defaults chosen (for the product owner's digest)

No design document or ADR settles these; each is the simplest behaviour
that makes the main path work, and the slice records it in its ADR.

1. Quotations and sales orders carry **net** amounts. An item keeps its
   output tax code; the tax is calculated when the order is billed, at the
   invoice date, which is the tax date (ADR-0049 §4, ADR-0063 §4).
2. An item names what is sold by a free-text item key, a description and
   a revenue account, as a purchase order item names what is bought; there
   is no material master (outside the required scope).
3. A quotation is open, accepted or rejected; it is not changed. One
   accepted quotation gives one order.
4. A sales order is created and confirmed directly by permission, without
   approval, and is not changed or cancelled in the first version.
5. A delivery is a confirmation by a person, in the sales context: no
   stock, no goods issue, no cost of goods sold.
6. Billing bills delivered quantities only, one order per billing
   document, at the order's prices, the last document of an item taking
   the rounding remainder of its net value; a created billing document
   holds its quantities and is not cancelled in the first version.
7. A billing document posts as one customer invoice in receivables with
   the billing date as invoice and tax date; the customer invoice is the
   accounting record, not the legal tax invoice (week 1's default 1).
8. Prices are per company and item key, general or for one customer; the
   customer's price wins, then the latest valid one. A price sent with
   the item overrides the price list.
9. Pricing configuration has two kinds of condition, a price and a
   percentage discount, in one procedure per tenant; none is shipped.
10. The credit check runs once, when the order is confirmed, on the
    order's net value in the company's currency against the limit less
    the open items (T77a's exposure). An order over the limit is
    confirmed with a block and cannot be delivered; anyone holding
    `sales.order.credit_release` releases it. Open orders do not count in
    the exposure (week 1's question 4 is still open).
11. A revenue contract is created by a person for one sales order in the
    company's local currency; its obligations are the order's items. An
    order without one is recognized as billed.
12. A point-in-time obligation is recognized in proportion to the
    quantity confirmed as delivered; an over-time obligation on a
    straight line by days over its service period.
13. The recognition run brings each contract, per period, to its
    cumulative position, revenue recognized less revenue billed: one
    contract asset or one contract liability, never both, on one account
    of each kind per company (account determination); one run per
    company and period, synchronous, not reversible in the first version.
14. An RFQ is free-standing (no requisition), its quotations are recorded
    by the buyer, one per invited supplier, in the RFQ's currency.
15. The comparison ranks complete, valid quotations by total price alone.
16. An award goes to one quotation as a whole and creates a draft
    contract, released as any contract; no approval of the award.
17. A scheduling agreement is a contract of its own kind with schedule
    lines; a line is called off as a purchase order against the
    agreement, so receipts, GR/IR and invoices work unchanged.
18. The catalog is the tenant's own table, per company, loaded through
    the API, one price per item; a requisition item from it takes the
    catalog's price and supplier and cannot override them.

## Questions (money, tax or accounting rules; none blocks a slice)

1. **IFRS 15 paragraphs** (accounting specialist): confirm the six
   statements of "Revenue recognition: what the standard is taken to
   say" against the standard's text, paragraph by paragraph; they are
   from memory.
2. **When control passes** (accounting specialist): the first version
   takes the delivery confirmation as the point a point-in-time
   obligation is satisfied. Is that acceptable for the pilot's terms of
   sale (incoterms, acceptance clauses, bill-and-hold)?
3. **Cost of goods sold** (accounting specialist, product owner): no cost
   is posted at delivery, since stock and its valuation are outside the
   required scope. How does a company using the sales chain post its cost
   of sales meanwhile: by manual entries at period end, or is a stand-in
   wanted?
4. **Orders without a revenue contract** (accounting specialist):
   recognized as billed, so a delivery not yet billed at a period end is
   not accrued. Should every confirmed order get a revenue contract
   automatically, so that the run accrues it?
5. **Performance obligations and timing** (accounting specialist): one
   obligation per order item, the person choosing point in time or over
   time. Which of the pilot's products need another rule (bundles,
   installation, warranties beyond assurance, licences)?
6. **Measure of progress** (accounting specialist): straight line by days
   is the only over-time method. Is by days right, or by months, and
   which other measures are needed first?
7. **Contract modifications** (accounting specialist): none in the first
   version, since orders are not changed. Which treatment is wanted when
   they are (a separate contract, prospective, or cumulative catch-up)?
8. **Contract asset, receivable and tax** (accounting and tax
   specialists): the run nets a contract to one asset or liability; the
   output tax is reported at the invoice date whatever revenue is
   recognized (week 1's question 1 on the tax point stands). Confirm, for
   Canada and the UAE, that advance billing of a service is taxed at
   billing.
9. **Sales and service leaves with no week** (product owner): rebates,
   returns and credit memo requests, EDI order intake, the CRM connector,
   service orders and subscriptions are not on the roadmap's week 2 or
   week 3 lists. First versions before 1 December, or after?
10. **Credit check amount** (product owner): the order is checked at its
    net value, the open items at their gross. Add the order's tax to the
    amount checked?
11. **Who releases a credit block** (product owner, security posture): a
    permission of its own, with no rule that the releaser differs from
    the person who confirmed the order. Is a segregation of duties rule
    wanted?
12. **Award without approval** (product owner): an award creates a draft
    contract with no approval step; the contract's release is by
    permission, as today. Is an approval above an amount wanted before
    the pilot?
13. **Catalog prices and contracts** (product owner): a catalog item's own
    price is used on the requisition; when it names a contract item, the
    order takes the contract's price as today. Should the catalog refuse
    a price that differs from its contract item's?
