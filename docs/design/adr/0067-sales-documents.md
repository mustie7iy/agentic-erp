# ADR-0067: Sales documents

**Status:** accepted (T78a, 2026-11-12; decided by default under ADR-0053 d;
`go-reviewer` had no blocking finding on T78a's last commit). It decides
the sales context and the documents of T78a-T79b
(`docs/design/plan-breadth-week2.md`, Lane A): quotation, sales order,
delivery confirmation and billing document. Prices (T80a, T80b) and the
order credit block (T77b) record what they build here too. Each slice
records what it built under "As built"; T78a's to T79b's, T80a's and T80b's are there now, with T80c's change of §11 (an entered price is the final net price) and T80d's of default 12 (a price unit without a unit price is refused), both the product owner's of 2026-11-12. Returns orders and their receipts (T85a, T85b; `docs/design/plan-breadth-week3.md`, Lane A) have no section of the decision: the plan decides them by default and they are recorded under "As built" alone. The defaults
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
  determination (T80a, T80b; §10) changes that function and nothing else
  of the documents.
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
billed quantities. The quotation's items are copied as they were offered,
prices included; items given directly are checked and priced as a
quotation's (§3). The order needs the customer's data in the company
(§4). `sales.Orders` is the context's published read of an
order with its items, as `procurement.Orders` is for payables.

### 6. Events and audit
Each change of a document's status writes an audit record and publishes
an event from the context's outbox in the same transaction (ADR-0007,
ADR-0011): `QuotationChanged`, `SalesOrderChanged`, and the later
slices' own. Events carry the document's net value, not its items, and
name the customer by id and number, never by name.

### 7. Permissions and agents
`sales.quotation.read` and `.write` (creating, and recording the
customer's answer); `sales.order.read`, `.write` and `.confirm`;
`sales.price.read` and `.write` (§10); the later slices add theirs. None is an approval permission. Every write of the
context is human-only (`x-agent: false`) in the first version, as
receivables' are (ADR-0063 §8): no ADR gives an agent a sales action, and
the order capture agent is a leaf of its own.

### 8. The delivery confirmation (T78c)
A delivery is a confirmation by a person, kept in the sales context
(plan default 5): quantities of a confirmed order's items were delivered,
or a service made available, on a date. It moves no stock and posts
nothing; billing and revenue recognition read it.

Only a confirmed order takes one. The order is read under its row lock
(ADR-0008), as a goods receipt reads its purchase order's items, so two
confirmations of one order wait for one another; an item's quantity is
at most what is open of it, what was ordered less what earlier
deliveries confirmed, with no tolerance. The date is not after the
company's date today in its time zone. A delivery is written once,
numbered within its order, and is not changed or reversed in the first
version. The order item's delivered quantity is the sum of its
deliveries, kept by the database, and each delivery moves the order's
version, since the order's representation shows what is delivered.

### 9. Billing (T79a, T79b)
A billing document bills an order's delivered quantities not yet billed,
at the order's prices, one order per document (plan default 6). Posting
it enters and posts one customer invoice in receivables in the same
transaction, with the billing date as invoice and tax date; the customer
invoice is the accounting record, not the legal tax invoice (plan
default 7; ADR-0063 §2).

Billing is cumulative. Under the order's row lock (ADR-0008), each item
of a document bills what is delivered of an order item less what earlier
documents of the order billed; its amount is the order item's net value
× the quantity billed so far, this document included, ÷ the quantity
ordered, rounded half away from zero, less the amounts earlier documents
billed of it. The documents of an item billed in full therefore add up
to its net value to the minor unit, the last taking the remainder, and
no quantity is billed twice. A created billing document holds its
quantities and is not changed or cancelled in the first version.

**An order whose item has lapsed cannot be billed (a caveat for T79b).**
An order's items were checked when they were entered (§3; a quotation's
when it was made) and an order is not changed in the first version (§5).
If an item's account can no longer be posted in the company, or its tax
code is no output tax code of the company with a rate on the billing
date, the order cannot be billed on that date and cannot be corrected:
T79a refuses the billing document (422 `sales.order_item_not_billable`)
rather than create one that could never post. Another billing date may
help for a tax code; nothing does for an account until orders can be
changed (later). T79b still meets what T79a cannot see when the
document is created: a refusal by receivables at posting (a blocked
customer, a closed period, an account or code that lapsed after the
document was created) leaves the document unposted, and a created
document is not cancelled in the first version.

**Posting (T79b).** `POST /v1/billing-documents/{id}:post` moves a
created document to posted once, under its row lock with a state guard
(ADR-0008). In the same transaction sales enters a customer invoice
through receivables' published API (`CreateInvoice`) and posts it
(`PostInvoice`), in process, through a port it declares: the document's
company, customer and currency, the billing date as invoice date, the
order's payment term, an item per item of the document with its
description, account, amount and tax code, and the document as the
invoice's source. Receivables calculates the tax and posts the entry
(ADR-0063 §4, §5); sales posts nothing itself. The invoice's net amount
is the document's to the minor unit and its gross is net and tax
exactly. The document then names the invoice and the invoice's entry.
Revenue is recognized as billed (plan default 11; revenue contracts are
ADR-0068's). What receivables or the ledger refuses fails the whole
transaction and is answered as a refusal of the document's post, with
the refusing context's code, the document's number and the document's
own fields. A posted document is not reversed or cancelled in the first
version.

### 10. Prices and their determination (T80a)
A price record is the net price of an item key in a company: for every
customer, or for one; per a price unit of a unit of measure, in a
currency; valid from a day and, when it has a last day, until it (plan
default 8). Records are insert-only: a price is never changed or
deleted, and a new price is a new record. A pricing analyst records
them (`POST /v1/sales-prices`, `sales.price.write`, human-only as every
write of the context, §7).

An item that gives a `unit_price` keeps it, whatever the records say:
the price sent with the item overrides the price list, and the item
reads `price_source` `manual`. An item without one takes the record of
its `item_key` in the document's company that is valid on the document's
date (the company's date, §2), in the item's unit of measure and the
document's currency; it reads `price_source` `price_list`. Among the
records that qualify:

1. the customer's price wins over the general one;
2. then the latest `valid_from`;
3. then, of two with the same `valid_from`, the one recorded last
   (`created_at`, then `id`), so that a price typed wrong is corrected by
   recording it again, and the choice never depends on the order rows
   are read in.

A record in another unit or another currency is not converted and does
not count: the unit and the currency must be the item's. An item with
neither a price nor such a record, or without an item key to look one
up by, is refused naming the price it lacks (422 `sales.no_price`,
`items[i].unit_price`). An item that sends a `price_unit` without a
`unit_price` is refused naming it (422 `request.validation_failed`,
`items[i].price_unit`; product owner 2026-11-12, T80d): the record's
price comes with its own price unit. The item key is compared exactly, as it is
stored.

The price is fixed on the item when it is created: the item stores the
unit price, the price unit and the net value, and a later record changes
no item. An order created from a quotation copies the quotation's items
with their prices and their sources (§5); it is not priced again. The
item's value is computed as before (§3): quantity × unit price ÷ price
unit, half away from zero at the minor unit, more than zero.

### 11. Condition types and the pricing procedure (T80b)
How an item is priced is the tenant's configuration (plan default 9):
the section `pricing` of the configuration document holds the
**condition types**, each a code, a name and a kind, `price` or
`discount_percent`, and one **procedure**, the condition types in the
order they apply. None is shipped in a country pack. The procedure names
each type once, a price first and no second price: one price is what the
discounts are taken from. A condition type is never removed once
activated and keeps its kind, as a payment term is never removed: price
records name it. Its name may change and it may leave the procedure. The
section posts nothing, so its changes are tier 2 (operational), a type's
name tier 1.

A price record (§10) may name a condition type. A record of a type of
kind `discount_percent` is a **discount**: for an item key in a company,
for every customer or for one, valid as a price is, a percentage more
than 0 and less than 100 of at most six decimals, with no unit, currency
or price. A record of a type of kind `price`, or without a type, is a
price. A record naming a type the active configuration does not have is
refused.

A tenant **without a procedure** prices as §10 says: the item has its
price, entered or recorded without a type, and no conditions. With a
procedure, an item is priced in its order:

1. **The price.** The price entered with the item or, without one, the
   record that applies (§10) among those without a type and those of the
   procedure's price type.
2. **Each discount type**, in the procedure's order: the record of that
   type for the item's key that applies, chosen as a price is (the
   customer's before the general one, then the latest `valid_from`, then
   the one recorded last). Its discount is its percentage of the value
   before it, the item's value less the discounts already taken, rounded
   half away from zero to the currency's minor unit (`money.Scale`). A
   type without such a record is passed over.

The item's net value is its value less its discounts, exact in minor
units, and stays more than zero (422 `sales.amount_not_positive`). The
item keeps the conditions applied, each with its type, the type's name
then, its rate (a price's unit price per price unit, a discount's
percent) and its amount, a discount's negative: the amounts add up to
the net value. They are fixed with the price when the item is created
and copied to the order a quotation gives. A price entered with the
item is its final net price (product owner 2026-11-12, T80c): no
discount is taken off it, its price is its only condition and its net
value is its value. As T80b built it, a discount applied to an entered
price as to a recorded one; that was default 15, which the product
owner overruled. An item without an item key has no discount either,
since discounts are recorded by key.

## Alternatives
- **Discounts in a table of their own, with their own endpoint**: the
  plan puts `condition_type` and `percent` on `sales.price` and adds no
  endpoint; a discount is found as a price is, by the same key and
  dates.
- **The conditions in a table per item table**: two more tables to hold
  what is written once with the item and only read with it. A JSON
  array on the item keeps the snapshot with what it explains.
- **An entered price as the final net value, with no discount**: it
  would make the entered price a way round the customer's discount in
  one direction and deny the customer its discount in the other. Not
  taken in T80b (default 15); the product owner chose it on 2026-11-12
  (T80c, §11).
- **A price record changed in place, with versions**: the items priced
  from it would point at a row that no longer says what they took. An
  insert-only record with a validity is what the plan names, and the
  later record wins.
- **One record per key, customer, unit, currency and `valid_from`
  (a unique rule)**: a wrong price could then not be corrected for its
  first day. Not taken: the record written last wins.
- **Converting a price from another unit or currency**: needs unit
  conversions per item and a rate date; the plan lists currency
  conversion of a price for later.
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
  cannot create the document (422 `config.no_number_range`). The country
  packs bring them from Canada 2026.9 and UAE 2026.6 (ADR-0048 As
  built); a company on an earlier version adds them by applying one of
  them or by a change request.
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

Taken in T80a beside plan default 8:

9. A price record names its unit and currency, and only an item in that
   unit on a document in that currency takes it; nothing is converted.
10. Of two records with the same `valid_from` the one recorded last
    wins; there is no uniqueness rule on a record's key and dates.
11. A record's customer is checked as a quotation's is (active, the
    customer role, not blocked for posting in the company); `valid_from`
    may be any day, past or to come.
12. `price_unit` sent without a `unit_price` is not read: a recorded
    price comes with its own. Overruled by the product owner on
    2026-11-12: it is refused naming the field (T80d).
13. Recording prices is human-only, as every sales write (§7).

Taken in T80b beside plan default 9:

14. A discount is recorded for an item key, general or for one customer,
    as a price is; there is no discount for a customer on every item
    (an access sequence, which the plan leaves for later).
15. With a procedure, the discounts found for an item's key are taken
    off an entered price too; an item without a key has none.
    Overruled by the product owner on 2026-11-12: an entered price is
    the final net price and has no discount (T80c).
16. A discount is more than 0 and less than 100 percent: one of 100
    would leave an item no value, which no item may have (default 4).
17. A price recorded without a condition type counts as the procedure's
    price, so the records made before a procedure was activated keep
    pricing; a record of a type applies only through a procedure that
    names its type.
18. The procedure has exactly one price, its first step; a condition
    type keeps its kind once activated.
19. Changes to the `pricing` section are tier 2, a type's name tier 1.
20. An item's conditions are kept as a JSON array on the item, with the
    condition type's name when the item was priced.

## Questions
- Plan questions 2 and 3 (accounting specialist, product owner): when
  control passes, and cost of goods sold without stock.
- **Discounts on an entered price** (product owner; a money rule):
  default 15 takes the customer's discount off a price the sales
  representative enters. Should an entered price be the final net value
  instead? Answered 2026-11-12: yes (T80c).
- **Billing at the order's prices** (product owner, T79a): once an item
  has discounts its net value is less than quantity × unit price ÷ price
  unit, so a billing document must take its share of the item's net
  value (net value × billed quantity ÷ ordered quantity), not the unit
  price times the billed quantity.
- **The emirate on a sales item** (product owner; scope): as on a customer
  invoice (ADR-0063, Questions), the UAE's one-code-for-every-emirate
  output codes are refused; selling under them needs a jurisdiction per
  item. Not built, no leaf added.

## As built

### T78a Quotation to a customer (migration 000290)
- `internal/sales` (`sales.go`, `items.go`, `pricing.go`, `quotations.go`,
  `quotation_api.go`), wired by `app.Sales`; `internal/layout` allows its
  imports of `config`, `ledger`, `partner` and `tenancy`; `sales` is in
  `outbox.Contexts` and among the tenant schemas `db/schema_test.go` holds
  to the design rules; the fixture's number ranges include `quotation`.
- Migration 000290: the schema with its outbox and inbox;
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
- Walkthrough `step_quotation` (step 62): Sally, the sales
  representative, makes a quotation to the customer of `step_customer`,
  reads it back with its net value, is refused an input tax code naming
  its field, and records that the customer accepted it; a second answer
  is refused and nothing is posted. The pilot's settings (step 2) add the
  `quotation` number range.
- **Decided by default:** the eight defaults above. Not built, by the
  plan: changing a quotation and its versions, the quotation as a
  document, the tax shown on it, alternative items, follow-up, CRM and CPQ
  connectors, approval below a margin.

### T78b Sales order confirmed (migration 000291)
- `internal/sales/orders.go`, `order_api.go`; `app.Sales` wires
  `partner.Customers` (the customer's company data) and
  `partner.Suppliers` (the tenant's payment terms, for customers as for
  suppliers) beside T78a's ports; the fixture's number ranges include
  `sales_order`.
- Migration 000291: `sales.sales_order` (`sales_order_number_once` per
  company; `sales_order_net_positive`; a payment term; statuses `draft`
  and `confirmed`; from a quotation only an accepted one of its company,
  customer and currency, `sales_order_quotation_accepted`, and one order
  per quotation, `sales_order_one_per_quotation`; confirming its one
  update, once, by `sales_order_confirmed_once` and a column grant of
  `status`, `version` and `updated_by`; no delete) and
  `sales.sales_order_item` (a quotation item's shape with
  `delivered_quantity` and `billed_quantity`, 0 when created and never
  above what was ordered and delivered, `sales_order_item_quantities`; no
  update or delete for the application until T78c and T79a are granted
  theirs); the permissions `sales.order.read`, `.write` and `.confirm`.
  Schema tests in `db/sales_test.go`.
- `POST /v1/sales-orders` (201 with `Location` and `ETag`, idempotent,
  human-only; `quotation_id` alone or `legal_entity_id`, `customer_id`,
  `currency` and `items`; `payment_term_code` with either),
  `GET /v1/sales-orders` (the companies the caller may act in; filters
  `legal_entity_id`, `customer_id`, `status`, `document_number`,
  `quotation_id`; sortable by `order_date` and `document_number`; cursor
  paging), `GET /v1/sales-orders/{id}` (404 for another company's),
  `POST /v1/sales-orders/{id}:confirm` (`If-Match`, idempotent,
  human-only). The order names its quotation by id and number and each
  item its ordered, delivered and billed quantities.
- From a quotation the order is created under the quotation's row lock:
  two requests wait for one another and the second is refused, 409
  `sales.quotation_ordered`; the unique index answers the same. The
  quotation's items are copied as they were offered, prices included:
  they are not priced or checked again (the offer stands; billing checks
  the account and the tax code when it posts). Items given directly go
  through T78a's checks and its one pricing function, on the order's
  date.
- Refusals: a quotation the caller cannot read 422
  `request.reference_not_found` (`quotation_id`); one not accepted 409
  `sales.quotation_not_accepted`; a mix of a quotation and the request's
  own company, customer, currency or items, or none of the two, 422
  `sales.quotation_or_items` naming the field given or missing; a
  customer without data in the company, not active, without the role or
  blocked for posting 422 `sales.customer_not_usable` naming
  `customer_id` or, for a quotation's customer, `quotation_id`; a payment
  term that is not configured 422 `sales.unknown_payment_term`; T78a's
  item refusals; confirming an order that is not a draft 409
  `sales.invalid_status`.
- `SalesOrderChanged` (`x-webhook`; created, confirmed; `quotation_id`
  null for an order created directly) and the audit records
  `sales.sales_order.create` and `.confirm`, in the change's transaction.
- `sales.Orders.Order` is the published read (an order with its items,
  company access left to the caller), for T81a.
- Walkthrough `step_sales_order` (step 63): Sally creates the order from
  the quotation of step 62, on Northwind's term NT30, with its items'
  ordered, delivered and billed quantities; a second order from the
  quotation is refused; she confirms it, once; nothing is posted. The
  pilot's settings (step 2) add the `sales_order` number range.
- **Decided by default:** the payment term is resolved and stored when
  the order is created (the customer's then, if none is given), so the
  order shows the term it will be billed with; a quotation's items are
  copied, not priced again; confirming checks nothing but the status (the
  credit check joins it with T77b); `delivered_quantity <= quantity` and
  `billed_quantity <= delivered_quantity` are database rules now, since
  T78c refuses a delivery above the open quantity and T79a bills
  delivered quantities only. Not built, by the plan: changing or
  cancelling an order, sales areas, partner functions, order types,
  schedule lines, incompletion checks, approval, the order confirmation
  as a document.

### T78c Delivery confirmed against the order (migration 000292)
- `internal/sales/deliveries.go`, `delivery_api.go`; nothing new is
  wired.
- Migration 000292: `sales.delivery` (numbered once within its order,
  `delivery_no_once`; only of a confirmed order,
  `delivery_order_confirmed`; insert-only) and `sales.delivery_item` (a
  quantity above zero of an item of the delivery's own order, an order
  item once per delivery; insert-only); the permission
  `sales.delivery.confirm`. Two triggers that run as the tables' owner,
  as procurement's `check_receipt` does, keep what the application may
  not change itself: each delivery item adds its quantity to the order
  item's `delivered_quantity`, which `sales_order_item_quantities`
  (000291) holds to what was ordered, and each delivery moves the
  order's `version` once and sets its `updated_by`. The order's change
  rule (`sales.check_sales_order_change`) admits a confirmed order's
  version moved from a trigger alone; for the application, confirming
  stays the order's one update. Schema tests in `db/sales_test.go`.
- `POST /v1/sales-orders/{id}/deliveries` (201, idempotent by
  `Idempotency-Key`, human-only; `delivery_date` and `items`, each the
  order item's number and a quantity in its unit) and
  `GET /v1/sales-orders/{id}/deliveries` (by `delivery_date`, cursor
  paging; `sales.order.read`). A delivery names its order by id and
  number and each item by the order item's number, key and description.
  It has no `Location`: there is no read of one delivery, the response
  is the delivery and the list gives it again.
- The order's row lock is taken by a statement of its own and the order
  with its items read by the next (`loadOrder`): at read committed, a
  statement that waited for the lock would still see the items as they
  were when it began, and a second delivery would be judged on what was
  open before the first (review round 1).
- The order's items show `delivered_quantity` (in the representation
  since T78b); what is open is `quantity` less it. The order's `version`
  and `ETag` move with each delivery.
- Refusals: an order that is not confirmed 409
  `sales.order_not_confirmed`; a date after the company's today 422
  `sales.delivery_date_in_future` (`delivery_date`, naming the company's
  date and zone); an item the order does not have
  `sales.unknown_order_item` (`items[i].item_no`); an order item named
  twice `sales.item_repeated`; a quantity that is not a plain decimal
  above zero `sales.invalid_quantity` (`items[i].quantity.value`);
  another unit than the order item's `sales.unit_mismatch`
  (`items[i].quantity.unit`); more than is open
  `sales.exceeds_open_quantity` (`items[i].quantity.value`, the message
  naming the item, what is open, ordered and delivered). An order of
  another company is not found (404). `sales.items_required` guards the
  port.
- `SalesOrderDelivered` (`x-webhook`; subject the order's id): the
  delivery's id, number and date, the items delivered with what is
  delivered of each order item after it, the customer by id and number,
  the order's version after it. The audit record
  `sales.delivery.confirm` is on the delivery (`sales.delivery`), in the
  change's transaction.
- `sales.Orders.Order` gives each item's deliveries (`ItemDelivery`: date
  and quantity, by date and then in the order they were confirmed), for
  T81c; the API's own reads of an order do not read them.
- Walkthrough `step_sales_order` (step 63) extended: Sally confirms 100
  of the 120 brackets as delivered and the order shows 100 delivered at a
  new version; 21 more and a date after today are refused naming their
  fields; a second delivery brings the last 20 and the installation's
  2.5 hours; the order reads as fully delivered and lists its two
  deliveries; a further delivery is refused; nothing is posted.
- **Decided by default:** a delivery has no document number and no
  number range (the plan names none): it is numbered within its order;
  it is read with the order's permission (`sales.order.read`), the plan
  adding `sales.delivery.confirm` alone; the delivery date is required
  and may be any day up to the company's today, also one before the
  order's date (nothing says an order is entered before its delivery);
  an order item is named once per delivery; the quantity's unit is the
  order item's and is checked, as a goods receipt's is; there is no
  over-delivery tolerance; the order's version moves with a delivery and
  no `SalesOrderChanged` is published for it (its status did not
  change; `SalesOrderDelivered` carries the version); the delivered
  quantity and the version are moved by the database's triggers rather
  than by an update granted to the application, so the quantity cannot
  differ from the sum of the deliveries. Not built, by the plan: the
  outbound delivery document, picking, packing and goods issue with
  stock and cost of goods sold, availability check, delivery blocks,
  tolerances, reversing a confirmation, proof of delivery, delivery
  notes.

### T79a Billing document created (migration 000293)
- `internal/sales/billing.go`, `billing_api.go`; nothing new is wired;
  the fixture's number ranges include `billing_document`.
- Migration 000293: `sales.billing_document` (`billing_document_number_once`
  per company; `billing_document_net_positive`; the status `created`; of
  a confirmed order of its company, customer and currency,
  `billing_document_order_confirmed`; no update or delete for the
  application until T79b is granted what posting sets) and
  `sales.billing_document_item` (a quantity and an amount above zero of
  an item of the document's own order, once per document; insert-only);
  the permissions `sales.billing.read`, `.write` and `.post`, the last
  used from T79b. As for deliveries (000292), two triggers running as
  the tables' owner add each item's quantity to the order item's
  `billed_quantity`, which `sales_order_item_quantities` (000291) holds
  to what is delivered, and move the order's `version` once per
  document; the item's trigger also holds the amounts billed of an order
  item to its net value, and to exactly that once it is billed in full
  (`billing_document_item_amounts`). Schema tests in `db/sales_test.go`.
- `POST /v1/billing-documents` (`sales_order_id`, optional
  `billing_date`; 201 with `Location` and `ETag`, idempotent by
  `Idempotency-Key`, human-only), `GET /v1/billing-documents` (the
  companies the caller may act in; filters `legal_entity_id`,
  `customer_id`, `sales_order_id`, `status`, `document_number`; sortable
  by `billing_date` and `document_number`; cursor paging),
  `GET /v1/billing-documents/{id}` (404 for another company's). The
  document names its company by code and name, its customer by number
  and name, its order by id and number with the order's payment term,
  and each item the order item it bills, with the quantity billed, the
  order's unit price, the amount, the account's number and name and the
  tax code.
- The amounts are calculated in exact rationals from the order item's
  net value (`billable`), so the function that prices an item
  (`pricing.go`) is not involved and a discount it gives from T80b is in
  the net value billed.
- The order's items show `billed_quantity` (in the representation since
  T78b); the order's `version` and `ETag` move with each billing
  document.
- Refusals: an order the caller cannot read 422
  `request.reference_not_found` (`sales_order_id`); nothing delivered
  that is not billed (a draft order included) 422
  `sales.nothing_to_bill` (`sales_order_id`); an item whose account can
  no longer be posted or whose tax code has no rate as an output code on
  the billing date 422 `sales.order_item_not_billable`
  (`sales_order_id`, the message naming the item, the date and which of
  the two); no fiscal year for the billing date
  `ledger.no_fiscal_period`; no number range `config.no_number_range`.
- `BillingDocumentChanged` (`x-webhook`; created) and the audit record
  `sales.billing_document.create`, in the change's transaction.
- Walkthrough `step_billing` (step 64): Ruth creates the billing
  document for the delivered order of step 63; its items and net value
  are the order's; the order shows everything billed at a new version; a
  second document is refused; nothing is posted. The pilot's settings
  (step 2) add the `billing_document` number range.
- **Decided by default:** the billing date is optional and defaults to
  the company's date today; no rule ties it to the deliveries' dates
  (posting checks the period, T79b); a quantity whose amount in a
  document would round to zero is left for a later document, so every
  item and every document has an amount above zero and posts one for one
  as a customer invoice item (T79b); for an item worth less than half a
  minor unit per quantity delivered this can leave a last quantity
  unbilled, at an amount of zero, the item's whole net value being
  billed by then; a draft order is refused as having nothing to bill,
  not by a status check of its own; the order's lapsed items are refused
  at creation (§9); a billing document copies nothing from the order's
  items but the quantity and the amount, since an order is not changed;
  the order's payment term is shown on the document, read from the
  order. Not built, by the plan: posting (T79b), cancelling a billing
  document, collective billing and invoice splits, billing plans and
  down payment requests, pro forma invoices, billing without delivery,
  intercompany billing, the billing due list, the invoice as a document
  or an e-invoice.

### T79b Billing posted through receivables (migration 000296)
- `internal/sales/billing_post.go` (`PostBilling`, `asBilling`),
  `billing_api.go` (`PostBillingDocument`); `app.Sales` wires
  `Invoices` to `app.Receivables` (receivables' `CreateInvoice` and
  `PostInvoice`, in process) and `Entries` to `ledger.OpenItems` (an
  entry's number); `internal/layout` allows sales to import
  `receivables`, which does not import sales.
- In `internal/receivables`, only the invoice's input and its source:
  `Input.Source` (`Source{ID, Number}`, nil for an invoice a person
  enters), written with the invoice in `CreateInvoice`
  (`invoices.go`); `Invoice.SourceDocumentID` and
  `SourceDocumentNumber`, read by `invoiceSelect` (`receivables.go`);
  `source_document_id` and `source_document_number` on the
  `CustomerInvoice` representation (`api.go`), absent for an invoice a
  person entered. `createCustomerInvoice`'s request takes no source,
  and the invoice's events are unchanged. Its tests are unchanged;
  `source_test.go` is new.
- Migration 000296: on `receivables.customer_invoice`
  `source_document_id` and `source_document_number`, both or neither
  (`customer_invoice_source`), a document invoiced once
  (`customer_invoice_source_once`); on `sales.billing_document` the
  status `posted` with `posting_date`, `customer_invoice_id`,
  `customer_invoice_number`, `journal_entry_year` and
  `journal_entry_id`, all of them when posted and none before
  (`billing_document_posted`), a customer invoice one document's
  (`billing_document_invoice_once`), the trigger
  `billing_document_posted_once` (only a created document changes, and
  only by being posted) and the update grant on the columns posting
  sets. No new table. Schema tests in `db/sales_test.go`
  (`TestBillingPostingRules`) and `db/receivables_test.go`
  (`TestCustomerInvoiceSource`).
- `POST /v1/billing-documents/{id}:post` (`sales.billing.post`,
  `If-Match`, idempotent by `Idempotency-Key`, human-only; optional
  `posting_date`): 200 with the document `posted` at version 2, naming
  `posting_date`, `customer_invoice_id`, `customer_invoice_number`,
  `journal_entry_id`, `journal_entry_year` and `journal_entry_number`.
  The entry is receivables' (`DR`, `source_context` `receivables`, the
  invoice as its source document): Dr the customer's reconciliation
  account with the gross, Cr each item's account with its amount and
  tax code, Cr output tax per code. The open item keeps the order's
  payment term, counted from the billing date.
- The document's lock is taken by a statement of its own and the
  document read by the next (`billing`), as the order's is. Of two
  posts of one document the second waits and then meets version 2: 412
  `request.version_mismatch`, or 409 `sales.invalid_status` at the new
  version. A replay with the same `Idempotency-Key` answers the posted
  document.
- **The refusals a post can still meet**, T79a having checked the
  account and the tax code when the document was created (§9): the
  customer not active, no longer a customer or blocked for posting
  since (422 `receivables.customer_not_usable`, `customer_id`; without
  a field when its data in the company is gone, which receivables
  finds only as it posts); an item's account
  that can no longer be posted (422 `receivables.account_not_postable`,
  `items[i].account_id`) or that asks for a center the item does not
  carry (the ledger's assignment refusals, `items[i].account_id`); a
  tax code that is no longer an output tax code of the company with a
  rate on the billing date (422 `receivables.unknown_tax_code`,
  `items[i].tax_code`); a posting date in a period that is not open
  (409 `ledger.period_not_open`); the customer's reconciliation account
  or an output tax account blocked (422 `ledger.account_not_postable`,
  `customer_id` or `items`); no `customer_invoice` or `journal_dr`
  number range (422 `config.no_number_range`). Receivables checks the
  customer, the accounts and the tax codes when the invoice is entered,
  before the ledger sees an entry, so a lapsed account or code is
  answered by receivables' refusal naming the item, not by a ledger
  error about a line. The invoice's fields are the document's, its
  items one for one, so its field errors stand; `asBilling` leads the
  detail with the document's number, turns an item's center into its
  account and the ledger's lines into what they are to the document
  (line 0 the customer, lines 1 to n the items with the field refused
  of them, the rest the output tax), and copies the problem rather than change it. Each refusal
  rolls the invoice entered back with everything else: the document
  stays `created` at version 1.
- `BillingDocumentPosted` (`x-webhook`): the document's net value, the
  invoice's tax and gross, the customer by id and number, the order,
  the invoice and its entry by id and number, the billing and posting
  dates. The audit record `sales.billing_document.post`. Receivables'
  own `CustomerInvoiceChanged` and `CustomerInvoicePosted` and its two
  audit records are written in the same transaction.
- `sales.Orders.Order` gives each item's posted billings
  (`ItemBilling`: the billing document by id and number, its posting
  date, the amount billed of the item, the customer invoice and its
  entry; by posting date, then document number), for T81d; a document
  not yet posted is not among them, and the API's own reads of an order
  do not read them. Posting does not move the order's version: nothing
  the order's representation shows changes.
- Walkthrough `step_billing` (step 64) extended: Ruth posts the
  document; the customer invoice names it, dated the billing date on
  NT30, net 1,737.53, tax 86.88, gross 1,824.41; the entry moves 1200,
  4000 and 2300 by exactly those amounts; Northwind's open item shows
  1,824.41; a second post (412, then 409) and the ledger's `:reverse`
  are refused; Tom imports RBC's next statement with Northwind's
  1,824.41, matches it as incoming and posts it, and Ruth applies it to
  the invoice. The step compares Northwind's open items and the
  unapplied receipts at its end with what it found at its start, and
  reads the statement's sequence and opening balance from RBC's latest
  statement, so it holds whatever earlier steps left open or imported.
- **Decided by default:** (1) the posting date is the request's, else
  the company's date today in its time zone, as for a customer invoice
  a person posts (ADR-0063 §5): the billing date is the invoice and tax
  date, not the posting date, so a document billed in a period closed
  since still posts; (2) `:post` takes `If-Match`, as confirming an
  order does; (3) a refusal keeps the refusing context's code, which
  the operation lists, rather than a sales code of its own, and sales
  does not check before receivables what receivables checks; (4)
  posting emits `BillingDocumentPosted` alone, not also
  `BillingDocumentChanged`, as a customer invoice's posting does; (5)
  the tax and the gross stay the customer invoice's: the billing
  document shows its net value and names the invoice, and the event
  carries all three; (6) the invoice's source is an id and a number
  without a type: billing documents are its only source today; it is
  set in process only, and is unique, so the database too refuses a
  second invoice of one document; (7) an invoice item's description is
  the order item's, and it carries no cost center or profit center
  (plan, "Later"): the account's default applies, and an account that
  needs one without a default cannot be billed; (8) `sales.billing.post`
  alone posts: the clerk needs none of receivables' permissions, the
  invoice being entered in process; (9) the order's version does not
  move. Not built, by the plan: revenue account determination, reversal
  or cancellation of a posted billing document, credit and debit memos
  from billing, posting to a profit center or segment, cost of goods
  sold, rebate accruals, a tax date other than the billing date.

### T80a Prices and their determination (migration 000294)
- `internal/sales/prices.go`, `price_api.go`; `pricing.go`'s `price`
  now takes the transaction and the document's customer
  (`document.customer`, set where a document is made in `quotations.go`
  and `orders.go`), and returns where the price came from. `items.go`
  stores and reads `price_source`; `ItemInput.UnitPrice` is optional.
  `quotations.go`'s `parties` is split into `company` and `customer`,
  which a price record's checks reuse. No new port and no new wiring.
- Migration 000294: `sales.price` (`price_item_key_check`,
  `price_positive`, `price_unit_positive`, `price_validity`; a unit and
  a currency of the reference data; insert-only by
  `platform.forbid_mutation`; `price_by_item_key`, `price_by_customer`);
  `price_source` on `sales.quotation_item` and `sales.sales_order_item`
  (`manual` by default, so the rows there and the previous version's
  writes are entered prices; `quotation_item_price_source_check`,
  `sales_order_item_price_source_check`); the permissions
  `sales.price.read` and `.write`. Schema tests in
  `db/sales_price_test.go`.
- `POST /v1/sales-prices` (201 with `Location` and `ETag`, idempotent,
  human-only), `GET /v1/sales-prices` (the companies the caller may act
  in; filters `legal_entity_id`, `item_key`, `customer_id`, `valid_on`;
  sortable by `item_key` and `valid_from`; cursor paging),
  `GET /v1/sales-prices/{id}` (404 for another company's). A record's
  `version` is 1: it is written once. It names its company by code and
  name and its customer by number and name.
- `unit_price` of `SalesItemCreate` is optional (a request that was
  valid stays valid); `SalesItem` and `SalesOrderItem` gain
  `price_source`.
- Refusals of a price record, each naming the request's own field: an
  item key of white space `request.validation_failed` (`item_key`); an
  unknown customer `request.reference_not_found`, one that cannot be
  sold to `sales.customer_not_usable` (`customer_id`); an unknown unit
  `sales.unknown_unit` (`unit`); an unknown currency
  `sales.unknown_currency` (`unit_price.currency`); an amount that is
  not the currency's `request.invalid_amount`, one of zero or less
  `sales.amount_not_positive` (`unit_price.amount`); `valid_to` before
  `valid_from` `sales.invalid_validity` (`valid_to`). Of an item: no
  price and no valid record, or no key, 422 `sales.no_price`
  (`items[i].unit_price`); a determined price whose value rounds to
  zero `sales.amount_not_positive` naming `items[i].quantity.value`,
  since the price was not the request's.
- `SalesPriceChanged` (`x-webhook`; the customer by id and number, both
  null for a general price) and the audit record `sales.price.create`,
  in the record's transaction.
- T78a's and T78b's tests pass as they were, but for what the contract's
  change forces: three literals in `quotations_test.go`,
  `orders_test.go` and `failures_test.go` give the now optional price as
  an optional value, and the tests' permission list names the two new
  permissions. Walkthrough steps 62 and 63 are unchanged.
- Walkthrough `step_price_list` (step 65): Paula, the pricing analyst,
  records the general price of BRACKET-M8 and Northwind's own per 100;
  Sally's quotation item without a price takes Northwind's (250 at
  1174.99 per 100: 2937.48, rounded half away from zero) beside an item
  at a price she enters; the same item for Contoso, a second customer,
  takes the general price; a key without a record and an item without a
  key are refused naming the price; a later general price changes no
  item priced before.
- **Decided by default:** defaults 9-13 above. Not built, by the plan:
  scales, prices per customer group, price list type or sales
  organization, surcharges and freight, minimum prices and approval,
  currency conversion of a price, repricing a document, mass changes
  and uploads, prices taken over from SAP.

### T80b Condition types and the pricing procedure (migration 000295)
- `internal/config/pricing.go`: the section `pricing` (`Pricing`,
  `ConditionType`, `ProcedureStep`), its validation (`validate.go` calls
  it), `PricingTransitions` (a type is never removed and keeps its kind;
  `internal/configuration` calls it beside the other transitions) and
  its tiers and list key in `diff.go`.
- `internal/sales/pricing.go`: `price` applies the procedure
  (`conditioned`), and one query shape (`validRecord`) picks the record
  that applies for prices and discounts alike; `document` carries the
  procedure, read from the active configuration where a document is
  made. `prices.go` records a discount or a price by the condition
  type's kind; `items.go` stores and reads `conditions`.
- Migration 000295: `condition_type` and `percent` on `sales.price`
  (`price_condition_type_check`, `price_percent_range`), a price's
  columns nullable, `price_or_percent`, `price_by_condition`;
  `conditions` on `sales.quotation_item` and `sales.sales_order_item`
  (`quotation_item_conditions_array`,
  `sales_order_item_conditions_array`, empty by default). Schema tests
  in `db/sales_condition_test.go`.
- No new endpoint. `SalesPriceCreate` gains `condition_type` and
  `percent`, and `unit` and `unit_price` are required of a price only;
  `SalesPrice` gains `kind`, `condition_type` and `percent`, and a
  discount has no `unit`, `unit_price` or `price_unit`;
  `GET /v1/sales-prices` filters by `condition_type`; `SalesItem` and
  `SalesOrderItem` gain `conditions[]` (`SalesCondition`: type, name,
  kind, the rate as `unit_price` per `price_unit` or `percent`, and
  `amount`). `SalesPriceChanged` gains `kind`, `condition_type` and
  `percent`; a discount's `unit`, `unit_price` and `price_unit` are
  null.
- Refusals of a record: a condition type the configuration does not
  have 422 `sales.unknown_condition_type` (`condition_type`); a field of
  the other kind given, or one of its own missing,
  `sales.price_or_percent` naming it; a percent that is not a plain
  decimal above 0 and below 100 of at most six decimals
  `sales.invalid_percent` (`percent`). Of an item: discounts that leave
  no value `sales.amount_not_positive`.
- Walkthrough `step_price_list` (step 65) extended: Ada's pricing
  procedure, PR00 and K007, is approved as a tier 2 change and
  activated; Paula records Northwind's discount of five percent on
  BRACKET-M8, and a record of an unknown type is refused; Sally's new
  quotation item shows the price, 2937.48, the discount, 146.87 (five
  percent of it, 146.874, rounded half away from zero), and the net
  value, 2790.61.
- **Decided by default:** defaults 14-20 above. Not built, by the plan:
  several procedures chosen by customer and document type, access
  sequences, fixed-amount and quantity-based conditions, header
  conditions, condition exclusion, statistical conditions, tax as a
  condition, rebates, pricing configuration generated from SAP's
  condition tables.

### T80c An entered price is the final net price (no migration)
- **Decision (product owner, 2026-11-12, row 2):** a price entered with
  a quotation or order item is the item's final net price. The pricing
  procedure takes no discount off it; a price determined from the price
  records has its discounts as before. This overrules default 15.
- `internal/sales/pricing.go`: `conditioned` stops after the price for
  an item whose source is `manual`, as it does for an item without a
  key. With a procedure the item's `conditions` are its price alone,
  whose amount is the net value; without one they are empty, as before.
- `price_source` keeps its two values: `manual` is the entered price,
  `price_list` the determined one. No new value, no schema change.
- Items priced before the change keep the conditions and net value they
  were stored with: an item is priced once, when it is created (§10),
  and an order from a quotation copies the quotation's items.
- Billing is unchanged: a billing document shares out the order item's
  net value (§9), which for an entered price is now quantity × unit
  price ÷ price unit, and shows the order item's conditions
  (`TestBillingAnEnteredPrice`, beside
  `TestBillingSharesOutTheDiscountedNet` for a determined price).
- Contract: descriptions only (`createQuotation`, `createSalesPrice`,
  `SalesItemCreate.unit_price`, `SalesItem.conditions`).
- Walkthrough `step_price_list` (step 65) extended: after Northwind's
  discount is recorded, Sally quotes the bracket at a price she enters,
  11.00; the item reads `manual`, lists the price alone and keeps
  2750.00, beside the item priced from the price list with its discount.

### T80d A price unit without a unit price is refused (no migration)
- **Decision (product owner, 2026-11-12, row 3):** `price_unit` sent
  without `unit_price` is refused with a field error; it was silently
  ignored (default 12), so an item could be priced per another quantity
  than its request said.
- `internal/sales/pricing.go`: `unitPrice` refuses the item, 422
  `request.validation_failed` naming `items[i].price_unit`, before it
  looks for a price record, whatever the price unit sent, 1 included.
  `itemInputs` passes a price unit the request did not give as 0, and an
  entered price without one is per 1, as before.
- It applies wherever the pair is accepted: `SalesItemCreate`, the item
  of `POST /v1/quotations` and of `POST /v1/sales-orders` with `items`.
  No operation changes an item afterwards, and an order from a
  quotation sends no items.
- Contract: `SalesItemCreate.price_unit` loses its `default: 1`. With
  it the generated decoder set the field on every request, so one sent
  could not be told from one absent; the description says 1 when absent.
  A request that sends no `price_unit` is read as before.
- **Decided by default:** the code is `request.validation_failed`, as
  procurement refuses a requisition item without the `unit_price` it
  must send, not a new `sales.*` code: the request's shape is wrong, not
  a rule of the sales context broken. Procurement's endpoints with the
  pair were left alone and need no change: a purchase order, contract,
  catalog, requisition and supplier quotation item require `unit_price`, and a
  requisition item from the catalog refuses a `price_unit` sent (422
  `procurement.catalog_item_override`).
- Walkthrough `step_price_list` (step 65) extended: Sally's item with a
  price unit of 100 and no price is refused naming
  `items[0].price_unit`.

### T77b Order credit block (migration 000300)

The credit check joins the confirmation, as T78b left it to
(`docs/design/plan-breadth-week2.md`, T77b; ADR-0066, As built T77b,
which records what the check compares, the exposure and the lock).

- `internal/sales/credit.go`: the ports `Credit` (the check, answered by
  the credit context through the composition root, since credit is a
  leaf) and `Rates` (`ledger.Rates`); the valuation of an order (net plus
  output tax on the order's date, in the company's currency);
  `Endpoints.OpenOrders`, the context's published read of a customer's
  confirmed orders not yet billed, for the credit exposure;
  `ReleaseCreditBlock`. `orders.go` checks at confirmation,
  `deliveries.go` refuses a blocked order, `order_api.go` shows the
  block, filters by it and releases it.
- Migration 000300: on `sales.sales_order`, `credit_block` (`none`,
  `blocked`, `released`; `sales_order_credit_block_check`),
  `credit_released_by`, `credit_released_by_name` (the releaser's name
  as it was then, as a close's sign-off keeps its signer's, with no
  rule of its own beside identity's for a display name: review round 2)
  and
  `credit_released_at`, set exactly when it is released
  (`sales_order_credit_release`); a draft has no block
  (`sales_order_credit_block_confirmed`). The order's change rule
  (`sales_order_confirmed_once`) admits a draft confirmed with or without
  a block, never already released, and a blocked order released, once;
  the application is granted the four columns. The delivery's trigger
  refuses a blocked order (`delivery_order_confirmed`). The permission
  `sales.order.credit_release`. Schema tests in `db/sales_test.go`.
- `POST /v1/sales-orders/{id}:confirm` answers the order with its
  `credit_block`: `none`, or `blocked` for an order over the customer's
  limit. It is confirmed either way (200). It refuses an order whose
  value cannot be calculated: 422 `sales.unknown_tax_code` (a tax code no
  longer an output code on the order's date), 422
  `ledger.no_exchange_rate` (another currency without a rate on the
  order's date; the refusal is worded here, naming the order, since the
  ledger's names a document field the confirmation does not have), 422 `sales.amount_too_large`.
- `POST /v1/sales-orders/{id}/deliveries` refuses a blocked order, 409
  `sales.order_credit_blocked` (the plan's status; a state the order is
  in, as 409 `sales.order_not_confirmed` is). Billing bills what is
  delivered, so a blocked order has nothing to bill (422
  `sales.nothing_to_bill`).
- `POST /v1/sales-orders/{id}:releaseCreditBlock`
  (`sales.order.credit_release`, `If-Match`, idempotent, human-only):
  `blocked` to `released`, once, under the order's row lock, with who
  released it and when; 409 `sales.order_not_credit_blocked` for an order
  without a block or already released. Nothing is checked again.
- `GET /v1/sales-orders?credit_block=` filters by the block; the order
  shows `credit_block`, `credit_released_by`, `credit_released_by_name`
  and `credit_released_at`. The releaser's name is read from the
  identity context through the port `People` (`app.LedgerPeople`);
  events carry the id alone, as a sign-off's do.
- Events and audit: `SalesOrderChanged` carries `credit_block`;
  `SalesOrderCreditBlocked` (`x-webhook`; the order's gross value, the
  amount checked, the limit and the exposure it was checked against) is
  published with it when the order is blocked; the confirmation's audit
  record holds the amount checked and, for a customer with a limit, the
  limit and the exposure. `SalesOrderCreditReleased` (`x-webhook`; who
  and when) and the audit record `sales.sales_order.release_credit_block`
  for the release. Events name the customer by id and number.
- Walkthrough `step_credit_block` (step 68): with Northwind's limit of
  step 59, Sally confirms an order within what is available, without a
  block, and a larger one, blocked; its delivery is refused; Sally may
  not release it; Ruth, who holds the credit manager's role, releases
  it, once, and the delivery is confirmed; the exposure shows the open
  orders beside the open items throughout.
- **Decided by default:** the action is `:releaseCreditBlock`, the
  contract's form of an action's name (`:signOff`, `:acceptProposal`;
  `api/spec_test.go`), where the plan wrote `:release-credit-block`; the
  refusal of a delivery is 409, the plan's
  status and the status of the order's other state refusals; `none` is
  stored for an unblocked order and a draft rather than a null; the
  release takes the caller's version (`If-Match`) as the confirmation
  does; `SalesOrderChanged` gains `credit_block` rather than the
  confirmation publishing a second kind of change event for an unblocked
  order. Not built, by the plan's "Later": see ADR-0066, As built T77b.

### T85a Returns order created and confirmed (migration 000306)

`docs/design/plan-breadth-week3.md`, T85a and its default 1: a returns
order is for a posted billing document's items, up to the billed
quantity; no stock, no posting.

- `internal/sales/returns.go` (`MakeReturns`, `ConfirmReturns`,
  `GetReturns`, `returnsOrder`), `returns_api.go`; nothing new is wired;
  the fixture's number ranges include `returns_order`.
  `quantities.go` holds the one check of the quantities a request takes
  of a document's items (`taking`: an item named once, a plain quantity
  above zero in the item's unit, at most what is open), which the
  delivery confirmation now uses too, and `sales.go` the one read of a
  document by id under its row lock for a caller's companies
  (`stored`), which billing documents use too.
- Migration 000306: `sales.returns_order` (`returns_order_number_once`
  per company; a reason of the four, `returns_order_reason_check`, and
  an optional text; statuses `draft` and `confirmed`; of a posted
  billing document of its company and customer with that document's
  sales order, `returns_order_document_posted` and
  `returns_order_billing_document_fkey`; confirming its one update,
  once, by `returns_order_confirmed_once` and a column grant of
  `status`, `version` and `updated_by`; no delete) and
  `sales.returns_order_item` (a quantity above zero of an item of the
  returns order's own billing document, a billing item once per returns
  order; no update or delete for the application);
  `returned_quantity` on `sales.sales_order_item`, at most what is
  billed (`sales_order_item_returned`); the permissions
  `sales.return.read`, `.write` and `.confirm`. As for deliveries
  (000292), two triggers running as the tables' owner add each item's
  quantity to the order item's `returned_quantity`, hold the returns
  orders of a billing document to what each of its items billed
  (`returns_order_item_quantities`) and move the sales order's
  `version` once per returns order. Schema tests in
  `db/sales_returns_test.go`.
- `POST /v1/returns-orders` (`billing_document_id`, `reason`, optional
  `reason_text`, `items` each a `billing_item_no` and a quantity; 201
  with `Location` and `ETag`, idempotent by `Idempotency-Key`,
  human-only), `GET /v1/returns-orders` (the companies the caller may
  act in; filters `legal_entity_id`, `customer_id`,
  `billing_document_id`, `sales_order_id`, `status`, `document_number`;
  sortable by `returns_date` and `document_number`; cursor paging),
  `GET /v1/returns-orders/{id}` (404 for another company's),
  `POST /v1/returns-orders/{id}:confirm` (`If-Match`, idempotent,
  human-only). The returns order names its company by code and name,
  its customer by number and name, its billing document and its sales
  order by id and number, and each item the billing item and the order
  item by number with the item key, the description, the quantity
  billed and the quantity returned. It carries no amounts.
- The billing document is read first, then its sales order's row is
  locked by a statement of its own and what the document's returns
  orders took is read by the next (as `loadOrder` reads an order's
  items after the lock): the second of two requests is judged on what
  the first took. Billing takes the same lock to count quantities, so
  returns orders and billing documents of one order wait for one
  another.
- The sales order's items show `returned_quantity` (required in the
  `SalesOrderItem` representation, 0 for an item nothing is returned
  of); the order's `version` and `ETag` move with each returns order
  created. Its delivered and billed quantities do not change.
- Refusals: a billing document the caller cannot read 422
  `request.reference_not_found` (`billing_document_id`); one that is
  not posted 409 `sales.billing_document_not_posted`; a reason that is
  not one of the four 422 `sales.unknown_return_reason` (`reason`); a
  reason text of white space `request.validation_failed`
  (`reason_text`); an item the billing document does not have
  `sales.unknown_billing_item` (`items[i].billing_item_no`); a billing
  item named twice `sales.item_repeated`; a quantity that is not a
  plain decimal above zero `sales.invalid_quantity`
  (`items[i].quantity.value`); another unit than the billed item's
  `sales.unit_mismatch` (`items[i].quantity.unit`); more than can still
  be returned `sales.exceeds_returnable_quantity`
  (`items[i].quantity.value`, the message naming the item, what was
  billed and what the document's returns orders took); no fiscal year
  for the date `ledger.no_fiscal_period`; no number range
  `config.no_number_range`. Confirming one that is not a draft 409
  `sales.invalid_status`. `sales.items_required` guards the port.
- `ReturnsOrderChanged` (created, confirmed; the reason, the customer
  by id and number, the billing document and the sales order by id and
  number; not `x-webhook`: product owner 2026-11-12, no webhook event
  until a consumer asks) and the audit records
  `sales.returns_order.create` and `.confirm`, the first with the
  quantities returned, in the change's transaction.
- **Credit exposure and revenue contract.** A returns order, confirmed
  or not, counts nowhere in the customer's credit exposure: the
  exposure is the customer's open items and its confirmed sales orders
  not yet billed (`Endpoints.OpenOrders`, which reads `sales_order`
  alone), and a return changes neither until its credit memo posts in
  receivables (T85d). It gets no revenue contract and changes none: the
  `revenue-contracts` consumer reads `SalesOrderChanged` alone, and a
  returns order publishes `ReturnsOrderChanged`; the sales order's
  version moves by the database's trigger without a
  `SalesOrderChanged`, as with a delivery. The return in the revenue
  contract (right of return, refund liability) is the plan's "Later".
  `TestReturnsOrderLeavesCreditAndRevenueAlone` (sales) and the
  consumer's `TestConsumerSkipsAndRefuses` case for the event (revenue)
  hold both.
- Walkthrough `step_returns` (step 74): Sally creates a returns order
  for 20 of the 120 brackets of step 64's billing document, as
  defective; the item shows 120 billed and 20 returned and the sales
  order 20 returned at a new version; 101 more and an unknown reason
  are refused naming their fields; she confirms it, once; Northwind's
  open items and credit exposure are as they were. The pilot's settings
  (step 2) add the `returns_order` number range.
- **Decided by default:** (1) every returns order of a billing
  document takes its quantity when it is created, a draft included,
  since a returns order is not changed or cancelled in the first
  version: "what the document's other returns orders took" (plan) is
  all of them; (2) `returned_quantity` on the sales order's item is
  that same sum over the order's billing documents, kept by the
  database's trigger on a column, as the delivered and billed
  quantities are, and the order's version moves with it; (3) a
  returns order is dated the company's date today, as a quotation and
  an order are; a person cannot set another date; (4) the reason's free
  text is optional with every reason, `other` included, at most 500
  characters; a reason outside the enumeration is refused, the
  enumeration being open for values added later; (5) the request names
  a billing document's item by `billing_item_no` and gives the
  quantity in the billed item's unit, which is checked, as a
  delivery's is; (6) confirming takes the caller's version
  (`If-Match`) and checks nothing but the status; no rule sets the
  person who confirms apart from the one who created it (product owner
  2026-11-12); (7) a returns order carries no amounts and no currency:
  what is credited is the credit memo request's, at the billed amount
  in full (product owner 2026-11-12), so nothing stored here says
  otherwise; (8) every write is human-only (§7); (9) a returns order
  counts neither in the credit exposure nor in a revenue contract
  (above). Not built, by the plan's "Later": a return before billing or
  without a billing document, the return delivery, goods receipt,
  inspection and stock posting, replacement deliveries, changing or
  cancelling a returns order, approval of a return, return reasons from
  configuration, the return in the revenue contract.

### T85b Returned quantities confirmed as received (migration 000307)

`docs/design/plan-breadth-week3.md`, T85b, its default 1 and its
boundary "A return moves no stock": a confirmation in the sales context
that returned quantities arrived, as T78c's is for a delivery.

- `internal/sales/receipts.go` (`ConfirmReceipt`), `receipt_api.go`;
  nothing new is wired. `returns.go` reads each item's
  `received_quantity` and `open_quantity`; the delivery's check of its
  date against the company's today is shared (`afterToday`).
- Migration 000307: `received_quantity` on `sales.returns_order_item`,
  0 by default and at most what is returned
  (`returns_order_item_received`); `sales.return_receipt` (numbered
  once within its returns order, `return_receipt_no_once`; only of a
  confirmed returns order, `return_receipt_order_confirmed`;
  insert-only) and `sales.return_receipt_item` (a quantity above zero
  of an item of the receipt's own returns order, a returns order item
  once per receipt; insert-only); the permission
  `sales.return.receive`. As for deliveries (000292), two triggers
  running as the tables' owner add each item's quantity to the returns
  order item's `received_quantity` and move the returns order's
  `version` once per receipt and set its `updated_by`; the returns
  order's change rule (`sales.check_returns_order_change`) admits a
  confirmed returns order changed from within a trigger alone, which
  the receipt's trigger is the only one to do, moving the version; for
  the application confirming stays its one update. Schema tests in
  `db/sales_returns_test.go` (`TestReturnReceiptRules`).
- `POST /v1/returns-orders/{id}/receipts` (201, idempotent by
  `Idempotency-Key`, human-only; `receipt_date` and `items`, each the
  returns order item's number and a quantity in its unit) and
  `GET /v1/returns-orders/{id}/receipts` (by `receipt_date`, cursor
  paging; `sales.return.read`). A receipt names its returns order by id
  and number and each item by the returns order item's number, the
  billing item's number, the item key and the description. It has no
  `Location`: there is no read of one receipt, the response is the
  receipt and the list gives it again, as for a delivery.
- The returns order's items show `received_quantity` and
  `open_quantity`, what is returned less what is received (both
  required in the `ReturnsOrderItem` representation); a returns order
  whose items all have an open quantity of 0 is fully received. Its
  `version` and `ETag` move with each receipt. The sales order is not
  touched: its delivered, billed and returned quantities and its
  version stay.
- Refusals: a returns order that is not confirmed 409
  `sales.returns_order_not_confirmed`; a date after the company's today
  422 `sales.receipt_date_in_future` (`receipt_date`, naming the
  company's date and zone); an item the returns order does not have
  `sales.unknown_returns_item` (`items[i].item_no`); an item named
  twice `sales.item_repeated`; a quantity that is not a plain decimal
  above zero `sales.invalid_quantity` (`items[i].quantity.value`);
  another unit than the item's `sales.unit_mismatch`
  (`items[i].quantity.unit`); more than is still expected
  `sales.exceeds_open_quantity` (`items[i].quantity.value`, the message
  naming the item, what is still expected, returned and received). A
  returns order of another company is not found (404).
  `sales.items_required` guards the port.
- `ReturnReceived` (subject the returns order's id; not `x-webhook`):
  the receipt's id, number and date, the items received with what is
  received of each returns order item after it, the customer by id and
  number, the billing document and the sales order by id and number,
  the returns order's version after it. The audit record
  `sales.return_receipt.confirm` is on the receipt
  (`sales.return_receipt`), in the change's transaction.
- **What T85c reads.** `returnsOrder` (`returns.go`), the read of a
  returns order for the caller's companies, under its row lock when
  asked: each `ReturnsItem` has the billing document's item
  (`BillingItemNo`), the sales order's item (`OrderItemNo`), the
  quantity that billing item billed (`Billed`), the quantity returned
  (`Quantity`), received (`Received`) and still expected (`Open`), as
  decimal strings in `Unit`. Through the API it is
  `GET /v1/returns-orders/{id}`. Nothing yet records what a credit memo
  request took of the received quantities: that is T85c's.
- Walkthrough `step_return_receipt` (step 75): Sally confirms 15 of the
  20 brackets of step 74's returns order as received and the returns
  order shows 15 received and 5 still expected at a new version; 6 more
  and a date after today are refused naming their fields; a second
  receipt brings the last 5 and the returns order reads as fully
  received; a further receipt is refused; the sales order and
  Northwind's open items are as they were.
- **Decided by default:** (1) a receipt has no document number and no
  number range (the plan names none): it is numbered within its returns
  order, as a delivery is within its order; (2) it is read with the
  returns order's permission (`sales.return.read`), the plan adding
  `sales.return.receive` alone; (3) the receipt date is required and
  may be any day up to the company's today, also one before the returns
  order's date (nothing says a returns order is entered before the
  goods arrive); (4) a returns order item is named once per receipt, by
  its number in the returns order, and the quantity's unit is the
  item's and is checked; there is no over-receipt tolerance; (5) the
  returns order's version moves with a receipt and no
  `ReturnsOrderChanged` is published for it (its status did not change;
  `ReturnReceived` carries the version); (6) the received quantity and
  the version are moved by the database's triggers, so the quantity
  cannot differ from the sum of the receipts; (7) `open_quantity` is
  shown per item rather than a status of the returns order: "fully
  received" is every item's open quantity at 0, and the returns order
  keeps its two statuses; (8) a receipt was built as a second
  walkthrough step (75) rather than as an extension of step 74, each
  slice having its own step; (9) a receipt, like the returns order,
  counts neither in the credit exposure nor in a revenue contract. Not
  built, by the plan's "Later": the return delivery, goods receipt,
  inspection and stock posting (Supply chain, outside the required
  scope), reversing a receipt, replacement deliveries.

### T85c Credit memo request created and decided (migration 000313)

`docs/design/plan-breadth-week3.md`, T85c, its default 2 and the
product owner's decisions of 2026-11-12: returned goods are credited at
the billed amount of the quantity received, in full, with no restocking
charge; the request is approved by one permission with no amount tier;
nothing sets the approver apart from the creator; an entered price is
final, so what is credited is what was billed, shared out with
`money.Allocate` when a part is credited.

- `internal/sales/credit_memo.go` (`RequestCredit`, `ApproveCredit`,
  `RejectCredit`, `GetCreditMemoRequest`, `creditRequest`, `share`),
  `credit_memo_api.go`; nothing new is wired; the fixture's number
  ranges include `credit_memo_request`.
- Migration 000313: `sales.credit_memo_request` and
  `sales.credit_memo_request_item` (03 §3.29 lists every rule), the
  permissions `sales.credit_memo_request.read`, `.write` and
  `.approve`. The two triggers that hold what a request may ask for only
  read, so they run as the caller; the decision's rule
  (`credit_memo_request_decided_once`) holds for the tables' owner too.
  Schema tests in `db/sales_credit_test.go`
  (`TestCreditMemoRequestRules`), with a case run as the tables' owner
  for each trigger.
- `POST /v1/credit-memo-requests` (`billing_document_id` and either
  `returns_order_id` or `items` each a `billing_item_no` and a
  `net_amount`, with `reason`; 201 with `Location` and `ETag`,
  idempotent by `Idempotency-Key`, human-only), `GET
  /v1/credit-memo-requests` (the companies the caller may act in;
  filters `legal_entity_id`, `customer_id`, `billing_document_id`,
  `sales_order_id`, `returns_order_id`, `status`, `document_number`;
  sortable by `request_date` and `document_number`; cursor paging), `GET
  /v1/credit-memo-requests/{id}` (404 for another company's), `POST
  …/{id}:release` (approve) and `…/{id}:decline` (reject, with
  `reason`), each with
  `If-Match`, idempotent, human-only, both by
  `sales.credit_memo_request.approve`. A request names its company by
  code and name, its customer by number and name, its billing document,
  sales order and returns order by id and number; it carries its
  currency (the billing document's) and net value, and each item the
  billing and order items by number, the item key and description, the
  quantity credited (a request for a returns order), the billed
  quantity and amount, the amount credited and the account (id, number,
  name) and tax code the credit memo will post with.
- **The amount of a quantity.** For each item of the returns order, the
  request takes what was received less what that returns order's
  requests not rejected took. Its amount is the share of the billing
  item's amount that the quantity requested of the item so far by
  requests for quantities (all returns orders, this request included)
  is of the billed quantity, shared out with `money.Allocate` against
  the rest (`share`), less what those requests took: the requests for
  an item's whole billed quantity add up to its billed amount to the
  minor unit, the last taking the remainder (T79a's rule, which the plan
  names). A quantity whose amount comes to zero is left for a later
  request, as billing leaves one; a returns order with nothing left to
  credit is 422 `sales.nothing_to_credit`.
- **The limit.** Under the sales order's row lock (`loadOrder(..., true)`,
  the lock returns orders and billing take), the requests of a billing
  document that are not rejected never ask for more of an item than it
  billed (422 `sales.exceeds_billed_amount` naming
  `items[i].net_amount.amount`, or `returns_order_id`, with the item,
  what it billed and what the other requests ask for); the database
  holds the same and that no returns order item is credited above what
  was received. A rejection frees what its request asked for.
- Refusals beside those: a billing document the caller cannot read 422
  `request.reference_not_found`, one not posted 409
  `sales.billing_document_not_posted`; both or neither basis 422
  `sales.credit_memo_request_basis` (`returns_order_id`); `items`
  without `reason`, a reason of white space 422
  `request.validation_failed` (`reason`); a returns order the caller
  cannot read 422 `request.reference_not_found`, another billing
  document's 422 `sales.returns_order_of_other_document`; an unknown
  billing item `sales.unknown_billing_item`, one named twice
  `sales.item_repeated`, another currency `sales.currency_mismatch`, an
  amount of zero `sales.amount_not_positive`, the wrong decimals 400
  `request.invalid_amount`. Deciding one that is not open 409
  `sales.invalid_status`.
- `CreditMemoRequestChanged` (created, approved, rejected; the net value,
  the customer by id and number, the billing document, sales order and
  returns order by id and number; not `x-webhook`) and the audit records
  `sales.credit_memo_request.create`, `.approve` and `.reject`, the
  first with the items' amounts and quantities, in the change's
  transaction.
- **Credit exposure and revenue contract.** A request, whatever its
  status, posts nothing, so the customer's open items and credit
  exposure do not change until T85d posts it. It touches no revenue
  contract: the `revenue-contracts` consumer reads `SalesOrderChanged`
  alone (`TestConsumerSkipsAndRefuses`' case for the event). How a
  return enters a revenue contract is the accounting specialist's open
  question 4 (plan); revenue is not touched by this slice or by T85d.
- Walkthrough `step_credit_memo_request` (step 85): Sally requests the
  credit for step 74's returns order, all 20 brackets received in step
  75: 20 of 120 at the billing item's amount in full; a second request
  finds nothing to credit and one by amount for the whole billed amount
  is refused naming the item; Carla approves it, once; Northwind's open
  items are as they were. The pilot's settings (step 2) add the
  `credit_memo_request` number range.
- **Decided by default:** (1) a request for a returns order takes
  everything received and not yet requested of every item, without a
  choice of items or quantities (the plan: "takes its received
  quantities not yet requested"); (2) the returns order may be a draft
  or confirmed, but only a confirmed one has receipts, so only it has
  anything to credit; (3) the reason is required with `items`, optional
  with a returns order, which has its own; it is at most 500 characters,
  as a return's; (4) a request is dated the company's date today and
  numbered from `credit_memo_request` (prefix `CR` in the pilot); (5)
  amounts are net: the tax is the customer credit memo's, at posting
  (T85d); (6) requests by amount and for quantities share one limit per
  billing item, its billed amount; the amount of a quantity is computed
  from what earlier requests for quantities took, so a request by amount
  does not shift what a later quantity is worth; (7) rejecting takes the
  approving permission and a reason; (8) approving and rejecting take
  the caller's version (`If-Match`), as confirming a returns order does;
  (9) every write is human-only; (10) the plan's `:approve` and
  `:reject` are named `:release` and `:decline`: the contract keeps
  `:approve` and `:reject` for approval requests (04 §4.9,
  `TestOperations`), and no approval request is made here, approval
  being one permission (product owner 2026-11-12); the statuses stay
  `approved` and `rejected`, the permission
  `sales.credit_memo_request.approve`. Not built, by the plan's "Later": debit
  memo requests, a request without a billing document, approval tiers
  and a separate approver, restocking and handling charges, the credited
  amounts in the revenue contract and in rebates.

### T85d Credit memo posted through receivables (migration 000314)

`docs/design/plan-breadth-week3.md`, T85d, its default 3 and the
product owner's decision of 2026-11-12 that what was billed is what is
credited; T79b's posting is the pattern.

- `internal/sales/credit_post.go` (`PostCredit`, `creditMemo`,
  `creditTax`, `ItemCredit`), `credit_memo_api.go`
  (`PostCreditMemoRequest`); `billing_post.go`'s refusal mapping is
  shared (`asPosting`, with `asBilling` as the billing document's).
  Nothing new is wired: the request posts through the `Invoices` port
  T79b gave sales, which gained `Invoice` (receivables'
  `Endpoints.Invoice`, its `GetInvoice`) to read the tax the billed
  invoice's items bore.
- Migration 000314: on `sales.credit_memo_request` the status `posted`
  with `posting_date`, `customer_invoice_id`, `customer_invoice_number`,
  `journal_entry_year` and `journal_entry_id`, all of them once posted
  and none before (`credit_memo_request_posted`), a credit memo one
  request's (`credit_memo_request_invoice_once`), the change rule
  admitting an approved request posted once (it holds for the owner too)
  and the update grant on the columns posting sets. No new table. Schema
  cases in `TestCreditMemoRequestRules`.
- `POST /v1/credit-memo-requests/{id}:post` (`sales.billing.post`,
  `If-Match`, idempotent, human-only; optional `posting_date`): 200 with
  the request `posted` at version 3, naming `posting_date`,
  `customer_invoice_id`, `customer_invoice_number`, `journal_entry_id`,
  `journal_entry_year` and `journal_entry_number`. The credit memo is
  receivables' (ADR-0063 §7, As built T85d): kind `credit_memo`,
  `reference_invoice_id` the billing document's invoice, the request as
  its source, the request's items one for one with the billing item's
  description, revenue account and tax code; its DG entry credits the
  customer's reconciliation account with the gross and debits each
  item's account and output tax. Its date (the tax date) and posting
  date are the request's `posting_date`, else the company's today; it
  has no payment term of its own, so it keeps the invoice's from the
  invoice's date, as T75d built.
- **The tax.** Of each billing item, the posted credits so far, this
  one included, bear the share of the tax the invoice's item bore that
  their net amounts are of the item's (`share`, `money.Allocate`), less
  what the earlier posted credits bore: the credits of an item's whole
  billed amount give back its net and its tax to the minor unit, in one
  request or in parts (`TestCreditsGiveBackWhatWasBilled`). Receivables
  posts the tax it is given (ADR-0063, As built T85d); the ledger holds
  it to the code's rate on the credit memo's date within a minor unit
  per line, so a rate changed between billing and crediting refuses the
  post (422 `ledger.output_tax_mismatch`).
- The request's lock is taken first, then the sales order's (the lock
  creation, returns and billing take), so the credits of one order's
  billing items are counted one post after the other
  (`TestCreditMemoRequestPostsRacing`); no path takes them the other way
  round. A refusal by receivables or the ledger leaves the request
  `approved`, leads the detail with the request's number and names its
  own fields (`items[i]`, `customer_id`; the invoice limit's
  `gross_amount` as the request's `net_amount`).
- `CreditMemoRequestPosted` (not `x-webhook`): the request's net value,
  the credit memo's tax and gross (positive), the customer by id and
  number, the billing document, sales order, returns order, credit memo,
  credited invoice and entry by id and number. The audit record
  `sales.credit_memo_request.post`. Receivables' own events and audit
  records are written in the same transaction.
- The billing document's items show `credited_amount` (required in
  `BillingDocumentItem`), what the posted requests credited of them; the
  billing document's version does not move. `sales.Orders.Order` gives
  each order item its posted credits (`ItemCredit`: the request, its
  billing document, posting date, net amount, credit memo and entry; by
  posting date, then number), for net revenue by segment (T92a).
- **Credit exposure and revenue contract.** The credit memo's credit
  line is an open item of the customer, so the exposure
  (`getCreditExposure`, open items plus open orders) falls by its gross
  as receivables already computes it; nothing in credit changed. Revenue
  is not touched: the `revenue-contracts` consumer does not read the
  event, and how a return enters a revenue contract stays the
  accounting specialist's question 4.
- Walkthrough `step_credit_memo_posting` (step 86): Ruth posts step 85's
  request; the credit memo names the request and the invoice, net the
  request's, tax the item's share of the GST billed; the DG entry moves
  1200, 4000 and 2300 back by exactly its amounts; Northwind's open
  amount falls by its gross; the billing item shows the amount credited;
  a second post is refused.
- **Decided by default:** (1) `:post` takes `sales.billing.post`, the
  billing clerk's permission (the plan adds none); (2) the credit memo's
  date is its posting date, the plan's "the company's date today unless
  given"; (3) the tax is what was billed, shared out, not recalculated
  (product owner 2026-11-12), which needed `ItemInput.Tax` in
  receivables; (4) the credit memo is not cleared against the invoice
  by the post: it stands as the customer's open credit item and clears
  by the ledger's clearing or nets with a payment (ADR-0063 §7 as
  built); (5) the request's version moves to 3 with the post; neither
  the billing document's nor the order's moves; (6) the request is the
  credit memo's source document (id and number), as the billing
  document is its invoice's. Not built, by the plan's "Later": a credit
  paid out to the customer, cancelling a posted request, the credit memo
  as a document or an e-invoice, the credited amounts in the revenue
  contract and in rebates.
