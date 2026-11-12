# ADR-0067: Sales documents

**Status:** accepted (T78a, 2026-11-12; decided by default under ADR-0053 d;
`go-reviewer` had no blocking finding on T78a's last commit). It decides
the sales context and the documents of T78a-T79b
(`docs/design/plan-breadth-week2.md`, Lane A): quotation, sales order,
delivery confirmation and billing document. Prices (T80a, T80b) and the
order credit block (T77b) record what they build here too. Each slice
records what it built under "As built"; T78a's is there now. The defaults
taken and the questions left are listed at the end for the product owner's
digest.

## Context
The product owner asked for a first usable version of every capability in
scope by 2026-12-01, the main path only (2026-11-10, breadth first). The
tracker's group Sales and service › Quotations and sales orders has three
placeholders (Quotations, Sales orders, Order-to-delivery flow) and
Billing two (Billing documents, Revenue and receivable postings).
`plan-breadth-week2.md` splits them into the slices T78a-T79b and asks
T78a to write this ADR.

What the design already settles:

- 02 §2.2 names `sales` among the later contexts, "with the same rules"
  as the built ones. 03 §3.16 gives the patterns: documents as a header
  and items with a `document_number` from the number ranges; financial
  effects only through ledger postings; partners through partner roles
  (`customer` joins `supplier`); a shared `material` master in a future
  `product` context.
- ADR-0063 built `receivables` and says of itself "it is not a leaf: week
  2's billing posts through it" (§1). A customer invoice is items by
  revenue account with a net amount and an output tax code, taxed at the
  invoice date (§4), posted as the customer's open item with its payment
  terms (§5).
- T74a gave a customer its data in a company: the reconciliation account
  and the payment term (`partner.Customers`).
- Purchasing documents (`internal/procurement`) hold an item as a quantity
  (a decimal string, `numeric(20,6)`) in a unit of measure at a unit price
  per price unit, extended with `money.Scale` and summed with `money.Sum`.
- ADR-0005 (money in minor units), ADR-0003 (RLS), ADR-0007 (outbox),
  ADR-0008 (row locks and state guards), ADR-0011 (audit), ADR-0053
  (slices, headless first); the company's date in its time zone for a
  document dated today (product owner, 2026-10-31); events name a customer
  by id and number, never by name (product owner, 2026-11-05).

What nothing settles, and the plan decides by default (its "Defaults
chosen" 1-7): net amounts, the item without a material master, the
quotation's and the order's statuses, the delivery as a confirmation, and
billing of delivered quantities through receivables.

## Decision

### 1. A context `sales`, built as receivables is
A new bounded context `sales` (schema `sales`, package `internal/sales`)
with its own outbox and inbox. It imports `config`, `ledger`, `partner`
and `tenancy`, and from T79b `receivables`, and reads them through ports
it declares, as receivables reads its own. It is not a leaf: `revenue`
(ADR-0068) reads it through `sales.Orders`. It posts nothing itself:
billing posts through receivables (§9). No foreign key leaves the schema
(02 §2.1).

### 2. The quotation (T78a)
A quotation is what the company offers a customer: a header (company,
customer, the day it was made, the last day it is valid, currency, net
value, status) and items (§3). It is numbered from the company's
`quotation` number range of its date's fiscal year and dated the
company's date today in its time zone; a person cannot set another date
in the first version. `valid_until` is not before that date.

Its statuses are `open`, `accepted` and `rejected`. The customer's answer
is recorded once, by a person, at the version they read (`If-Match`),
under the quotation's row lock: `POST /v1/quotations/{id}:accept` and
`:decline`. The plan names the second `:reject`; the contract keeps
`:approve` and `:reject` for approval decisions, made on approval requests
alone (04 §4.9, `api` `TestOperations`), and a quotation has no approval,
so the action that records the customer's rejection is `:decline` and the
status it gives stays `rejected`. A quotation whose validity ended before the company's date
today is not accepted (409 `sales.quotation_expired`): it is made again.
It may still be rejected, which only records the customer's answer. There
is no `expired` status and no job that sets one: whether a quotation can
still be accepted is read from `valid_until`. A quotation is not changed
and not deleted; the database allows the answer as its one update.

No document is produced for the customer (output management, later).

### 3. Items, net amounts and the price
One item shape serves the quotation and the sales order: what is sold by a
description and an optional free-text `item_key` (there is no material
master: plan, "Boundaries"; T80a finds prices by the key), a quantity
more than zero in a unit of measure of the reference data, a unit price
per price unit, the revenue account its billing credits and, optionally,
an output tax code.

- **Net amounts.** Sales documents carry net amounts (plan default 1). An
  item keeps its output tax code; the tax is calculated when the order is
  billed, at the invoice date, which is the tax date (ADR-0049 §4,
  ADR-0063 §4). The code is checked when the document is created: an
  output tax code of the company with a rate in force on the document's
  date, and not one that levies the subdivision its lines name, which a
  customer invoice refuses (ADR-0063 §4), so that an item never waits for
  a billing that cannot post.
- **An item's value** is quantity × unit price ÷ price unit, rounded half
  away from zero to the currency's minor unit (`money.Scale`), more than
  zero; the document's net value is the sum of its items' (`money.Sum`),
  exactly. Quantities are decimal strings held as `numeric(20,6)`, never
  floats.
- **One pricing function.** The item's price and value are set in one
  function for both documents (`internal/sales/pricing.go`, `price`). In
  T78a and T78b the price is the one entered with the item; price
  determination (T80a, T80b) changes that function and nothing else of
  the documents.
- **The account.** Any account that can be posted in the company may
  carry an item, as on a customer invoice (ADR-0063, default 2). Revenue
  account determination comes later.

### 4. The customer
The customer is a business partner with the customer role. A quotation
checks it as receivables checks a customer at entry
(`partner.Postings.PostingBlock` with kind `customer`: active, the role,
not blocked for posting in the company). A sales order needs the
customer's data in the company (`partner.Customers.Customer`, `Usable`),
as a receivables document does when it posts: the order is what billing
posts from, and its payment term defaults from that data (§5).

### 5. The sales order (T78b)
A sales order is created from an accepted quotation, whose customer,
currency and items it copies, or directly from items. One accepted
quotation gives one order. Its payment term is the one given with it,
else the customer's in the company. Its statuses are `draft` and
`confirmed`; confirming is by permission, without approval, once, under
the order's row lock. An order is not changed or cancelled in the first
version (plan default 4). Each item shows its ordered, delivered and
billed quantities. `sales.Orders` is the context's published read of an
order with its items, as `procurement.Orders` is for payables.

### 6. Events and audit
Each change of a document's status writes an audit record and publishes
an event from the context's outbox in the same transaction (ADR-0007,
ADR-0011): `QuotationChanged`, `SalesOrderChanged`, and the later
slices' own. Events carry the document's net value, not its items, and
name the customer by id and number, never by name.

### 7. Permissions and agents
`sales.quotation.read` and `.write` (creating, and recording the
customer's answer); `sales.order.read`, `.write` and `.confirm`; the later
slices add theirs. None is an approval permission. Every write of the
context is human-only (`x-agent: false`) in the first version, as
receivables' are (ADR-0063 §8): no ADR gives an agent a sales action, and
the order capture agent is a leaf of its own.

### 8. The delivery confirmation (T78c)
A delivery is a confirmation by a person, kept in the sales context
(plan default 5): quantities of a confirmed order's items were delivered,
or a service made available, on a date. It moves no stock and posts
nothing; billing and revenue recognition read it.

### 9. Billing (T79a, T79b)
A billing document bills an order's delivered quantities not yet billed,
at the order's prices, one order per document (plan default 6). Posting
it enters and posts one customer invoice in receivables in the same
transaction, with the billing date as invoice and tax date; the customer
invoice is the accounting record, not the legal tax invoice (plan
default 7; ADR-0063 §2).

## Alternatives
- **Quotations and orders in `receivables`**: not taken. 02 §2.2 and 03
  §3.16 name `sales` as a context of its own, and receivables' documents
  post while sales' do not.
- **A leaf context** (as assets, cash, closing): not taken. Billing calls
  receivables in process and revenue recognition reads sales, which a
  leaf allows in neither direction.
- **Gross amounts with tax on the quotation**: the tax would be
  calculated at the quotation's date and again, perhaps differently, at
  the invoice date, which is the tax date. Net amounts leave one
  calculation, where receivables already makes it. Showing the tax on a
  quotation is listed for later.
- **An `expired` status set by a job**: a state that only repeats what
  `valid_until` and the date say, with a job to keep it true. Not taken.
- **One table for quotations and orders with a kind**: their statuses,
  their changes and what later slices hang on the order (deliveries,
  billing) differ; only the item shape is shared, and it is, in code.

## Consequences
- A new schema, its permissions and the document types `quotation` and,
  from T78b, `sales_order` for number ranges; a company without the range
  cannot create the document (422 `config.no_number_range`). The released
  country packs bring none; a tenant adds it by a change request.
- No cost of goods sold is posted by the sales chain (plan question 3).
- An item's account and tax code are checked when the document is
  created; billing checks again when it posts, through receivables.

## Defaults chosen (for the product owner's digest)
Plan defaults 1-7 above. Taken in T78a beside them:

1. A quotation is dated the company's date today; a person cannot set
   another date.
2. A quotation past its validity can still be rejected, not accepted.
3. An item's tax code is optional (an item without one bears no tax, as
   on a customer invoice) and is checked at the document's date.
4. An item's value is more than zero, so a price of zero is refused: a
   customer invoice item is more than zero (ADR-0063, default 3), and a
   free item could not be billed.
5. Every write is human-only (§7).
6. A code that levies its lines' subdivision is refused (§3).
7. The response names what it references: the company by code and name,
   the customer by number and name, each item's account by number and
   name (REVIEW.md, API-consumer classes).
8. The customer's rejection is recorded by `:decline`, not the plan's
   `:reject` (§2): the contract's rule on approval decisions decides it.

## Questions
- Plan questions 2 and 3 (accounting specialist, product owner): when
  control passes, and cost of goods sold without stock.
- **The emirate on a sales item** (product owner; scope): as on a customer
  invoice (ADR-0063, Questions), the UAE's one-code-for-every-emirate
  output codes are refused; selling under them needs a jurisdiction per
  item. Not built, no leaf added.

## As built

### T78a Quotation to a customer (migration 000276)
- `internal/sales` (`sales.go`, `items.go`, `pricing.go`, `quotations.go`,
  `quotation_api.go`), wired by `app.Sales`; `internal/layout` allows its
  imports of `config`, `ledger`, `partner` and `tenancy`; `sales` is in
  `outbox.Contexts` and among the tenant schemas `db/schema_test.go` holds
  to the design rules; the fixture's number ranges include `quotation`.
- Migration 000276: the schema with its outbox and inbox;
  `sales.quotation` (`quotation_number_once` per company;
  `quotation_validity`, valid until a day not before its date;
  `quotation_net_positive`; statuses `open`, `accepted`, `rejected`; the
  answer its one update, once, by `quotation_answered_once` and a column
  grant of `status`, `version` and `updated_by`; no delete) and
  `sales.quotation_item` (a positive quantity in a unit of the reference
  data, a positive price per a positive price unit, a positive net value;
  insert-only by `platform.forbid_mutation`); the permissions
  `sales.quotation.read` and `.write`. Schema tests in `db/sales_test.go`.
- `POST /v1/quotations` (201 with `Location` and `ETag`, idempotent by
  `Idempotency-Key`, human-only), `GET /v1/quotations` (the companies the
  caller may act in; filters `legal_entity_id`, `customer_id`, `status`,
  `document_number`; sortable by `quotation_date` and `document_number`;
  cursor paging), `GET /v1/quotations/{id}` (404 for another company's),
  `POST /v1/quotations/{id}:accept` and `:decline` (`If-Match`, idempotent,
  human-only; the two answer with the same representation).
- Refusals, each naming the request's own field where one caused it: an
  unknown customer 422 `request.reference_not_found` (`customer_id`); one
  that is not active, has no customer role or is blocked for posting in
  the company `sales.customer_not_usable`; a description or an item key
  of white space alone `request.validation_failed`
  (`items[i].description`, `items[i].item_key`; the columns check the
  trimmed text too); `valid_until` before the
  company's date `sales.invalid_validity`; a quantity that is not a plain
  decimal above zero `sales.invalid_quantity`
  (`items[i].quantity.value`); an unknown unit `sales.unknown_unit`
  (`items[i].quantity.unit`); a price in another currency
  `sales.currency_mismatch`; a value of zero or less
  `sales.amount_not_positive` (`items[i].unit_price.amount`); an account
  that cannot be posted `sales.account_not_postable`
  (`items[i].account_id`); a tax code that is no output code of the
  company with a rate on the quotation's date `sales.unknown_tax_code`
  (`items[i].tax_code`); one levying its lines' subdivision
  `sales.tax_jurisdiction_not_supported`; an unknown currency
  `sales.unknown_currency`; values out of range `sales.amount_too_large`
  (`items`); no fiscal year for the date `ledger.no_fiscal_period`; no
  number range `config.no_number_range`. Answering: 409
  `sales.invalid_status` for a quotation that is not open, 409
  `sales.quotation_expired` for accepting one past its validity.
  `sales.items_required` guards the port against a quotation without
  items, which the contract refuses first.
- The unit of measure is checked against `ref.unit_of_measure` before the
  insert, so the refusal names the item's field; the foreign key stays as
  the database's rule.
- `QuotationChanged` (`x-webhook`; created, accepted, rejected) and the
  audit records `sales.quotation.create`, `.accept` and `.decline`, in the
  change's transaction.
- Walkthrough `step_quotation` (step 54): Sally, the sales
  representative, makes a quotation to the customer of `step_customer`,
  reads it back with its net value, is refused an input tax code naming
  its field, and records that the customer accepted it; a second answer
  is refused and nothing is posted. The pilot's settings (step 2) add the
  `quotation` number range.
- **Decided by default:** the eight defaults above. Not built, by the
  plan: changing a quotation and its versions, the quotation as a
  document, the tax shown on it, alternative items, follow-up, CRM and CPQ
  connectors, approval below a margin.
