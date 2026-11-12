# ADR-0069: Sourcing, RFQ to award

**Status:** accepted (T82a, 2026-11-12; decided by default under ADR-0053 d;
`go-reviewer` had no blocking finding on T82a's last commit). It decides
the slices T82a-T82d (`docs/design/plan-breadth-week2.md`, Lane C). Each
slice records what it built under "As built"; T82a's, T82b's and T82c's
are there now. The defaults taken are listed at the end for the product
owner's digest.

## Context

The product owner asked for a first usable version of every capability in
scope by 2026-12-01, the main path only (2026-11-10, breadth first). The
tracker's group Sourcing and procurement › Sourcing, RFQ and bid analysis
has three placeholders: RFQ events, Bid comparison agent and Award to
contract. `plan-breadth-week2.md` splits them into the slices T82a (an RFQ
created and published), T82b (the suppliers' quotations recorded), T82c
(the quotations compared) and T82d (the RFQ awarded to a contract), and
asks T82a to write this ADR. The agent that reads the comparison and
recommends an award stays an open leaf for week 3.

What the design already settles:

- The procurement context (03 §3.8) holds requisitions, contracts and
  purchase orders; a contract is a draft with items priced per material
  key, released by a person (`internal/procurement/contracts.go`). An item
  names what is bought by a description and a `material_key`; there is no
  material master (plan, Boundaries).
- A supplier an order can be placed with is an active business partner in
  the supplier role, set up in the legal entity and not blocked for
  posting (`partner.Supplier.Usable`, `procurement.supplier_not_usable`).
- A document dated "today" takes the company's date in its time zone
  (`config.Config.CompanyDay`, T72n5), and a document number is drawn from
  the company's range for that date's fiscal year.
- ADR-0003 (RLS), ADR-0005 (money in minor units), ADR-0007 (outbox),
  ADR-0008 (row locks and state guards), ADR-0011 (audit), ADR-0053
  (slices, headless first). Events name a business partner by id and
  number, never by name (product owner, 2026-11-05).

Nothing settles what an RFQ is here: what it holds, who answers it, how
quotations are compared and what an award creates. This ADR decides them
at the level the four slices need.

## Decision

### 1. An RFQ is a document of the procurement context

Requests for quotation, supplier quotations and their items are new
tables of the existing `procurement` context (schema `procurement`,
package `internal/procurement`, each slice's handlers in a file of its
own): no new context, and no new import between contexts. An RFQ is
**free-standing**: it does not come from a requisition.

An RFQ belongs to a legal entity and has a document number from the
company's `rfq` number range, the **currency** quotations are given in, a
**due date** quotations are due by, **items** and the **suppliers
invited**. It is `draft` when created and `published` once; T82d adds
`awarded`. Nothing else changes an RFQ in the first version: a draft is
published as created.

### 2. Items carry a quantity and no price

An RFQ item names what is bought as a contract item does: a description
and a `material_key`, unique in the RFQ, with the quantity asked for and
its unit of measure (as a requisition's quantity: a positive plain
decimal, `numeric(20,6)`), and the general ledger account it is bought
on, one the legal entity can post to. The material key and the account
are there because an award makes the RFQ's items a contract's (§7), whose
items need both. An item has no price: prices are the quotations'.

### 3. Suppliers invited

An RFQ invites at least one supplier, each once. Each must be a supplier
an order could be placed with in the legal entity (the check a purchase
order's supplier passes), when the RFQ is created and again when it is
published. The RFQ keeps the order the suppliers were named in. The API
shows each by id, number and name (read from the partner context); events
and audit records carry the id and the number only.

### 4. The due date and publication

The due date is not before the legal entity's date today in its company's
time zone, when the RFQ is created and again when it is published; the
refusal names that date and the zone. Publishing reads the RFQ under its
row lock at the version the caller read (`If-Match`) and moves it from
`draft` to `published` once; a second publication is refused (409).
Publishing is by permission, with no approval request. The due date is
informative after publication in the first version: recording a quotation
after it is not refused (sealed bids opened at the due date are left for
later).

### 5. Events and audit

Every change is audited (`procurement.rfq.create`, `.publish`, and the
later slices' actions) and published in the same transaction through the
procurement outbox: `RfqChanged` with the RFQ as the change left it
(`change`: `created`, `published`), and `RfqPublished` for a publication,
which names the suppliers invited by id and number. T82b adds
`SupplierQuotationRecorded` and T82d `RfqAwarded`, each naming the
supplier by id and number. None is offered to webhooks in the first
version.

### 6. Permissions

`procurement.rfq.read` reads RFQs, their quotations and the comparison;
`procurement.rfq.manage` creates and publishes an RFQ and records
quotations; `procurement.rfq.award` (T82d) awards one. A caller sees the
RFQs of the legal entities it may act in; another company's is answered
as missing (04 §4.9). None is an approval permission in T82a and T82b.

### 7. Quotations, comparison and award (T82b-T82d)

- **T82b.** The buyer records each invited supplier's quotation against a
  published RFQ: a price per RFQ item in the RFQ's currency, positive, per
  the RFQ item's unit, with a price unit; the supplier's reference and the
  date the quotation is valid until. A supplier has one quotation per
  RFQ. A quotation may leave items out and then reads as incomplete. Its
  total is the sum of quantity × unit price ÷ price unit over the items it
  prices, exact in minor units (`money.Scale`, `money.Sum`).
- **T82c.** The comparison is computed on read and stores nothing:
  complete quotations valid on the company's date today, ranked by total,
  the lowest price per item marked; incomplete or expired quotations
  unranked; invited suppliers without a quotation listed as not having
  answered.
- **T82d.** An award goes to one quotation as a whole and creates a draft
  contract with that supplier (`draftContract`): the RFQ's items at the
  quoted prices, the target value the quotation's total. The RFQ, its
  quotations and the contract change in one transaction; the contract is
  released afterwards as any contract.

## Alternatives

- **A new `sourcing` context.** Rejected for the first version: an award
  writes a contract, so a separate context would need a port into
  procurement and a cross-context transaction for three tables that share
  procurement's suppliers, accounts, numbering and items.
- **An RFQ changed after it is created (PATCH).** Left out: the plan's
  first version has create and publish only; a draft that is wrong is
  left unpublished and created again.
- **Suppliers answering themselves.** That is the supplier portal or the
  supplier network, outside the first version; the buyer records what the
  suppliers sent.

## Consequences

- An RFQ cannot be withdrawn, closed without an award or changed; these
  are the "later" leaf's.
- An RFQ's items need a material key and an account although the RFQ
  posts nothing: the price of awarding to a contract without asking for
  them again.
- A tenant needs an `rfq` number range per company and fiscal year; a
  company without one is refused with `config.no_number_range`, as for
  every document type.

## Defaults chosen (for the product owner's digest)

1. An RFQ is free-standing, in one currency, with a due date; it is
   published by permission, without approval.
2. The suppliers invited must each be one an order could be placed with,
   checked when the RFQ is created and when it is published.
3. An RFQ item carries a material key (once per RFQ) and an account, as a
   contract item does, because an award makes it one.
4. The due date is checked when the RFQ is created and published, not when
   a quotation is recorded.
5. `RfqChanged` is published for every change of an RFQ and `RfqPublished`
   beside it for a publication; neither is offered to webhooks yet.
6. The API shows what an RFQ names with what its consumer needs: a
   supplier invited by id, number and name, the company by id, code and
   name, an item's account by id, number and name (REVIEW.md; the older
   procurement documents show a company and an account by id alone).
7. A quotation's validity date is recorded as given: one already past is
   not refused when it is recorded (the comparison, T82c, leaves an
   expired quotation unranked).
8. A quotation is not changed or withdrawn: a wrong one stays, as revised
   quotations are the "later" leaf's.
9. A quotation's total is computed from its prices and the RFQ's
   quantities on each read, never stored.
10. The comparison (T82c) is by price alone and compares amounts, exact
    in minor units. Equal totals share a rank (1, 1, 3) and stand in the
    order their suppliers were invited, as every other group of rows
    does.
11. The lowest price of an item is the lowest amount (the RFQ's quantity
    at the quoted price) among the quotations not expired, an incomplete
    one included; every price at that amount is marked. An expired
    quotation's price is never marked: it no longer holds.
12. A quotation valid until the company's date today is still valid that
    day; it is expired from the next.
13. The comparison is readable for an RFQ in any status and is not paged:
    an RFQ invites at most 100 suppliers and holds at most 1,000 items,
    so at most 100 rows of 1,000 prices each, the bounds of a page of
    the RFQ's quotations (up to 500 quotations of 1,000 prices).

## As built

### T82a RFQ created and published (migration 000276)

- **Schema.** `procurement.rfq` (company, number unique per company,
  currency, due date, status `draft` or `published`, version),
  `procurement.rfq_item` (item number, material key once per RFQ,
  description, quantity above zero, unit, account) and
  `procurement.rfq_supplier` (the supplier once per RFQ, `invite_no` its
  place in the invitation), each with RLS and `tenant_id` first. Trigger
  `rfq_lifecycle`: an RFQ starts as a draft, keeps its company, number,
  currency and due date, and stays published. Items and suppliers are
  written only while the RFQ is a draft (`check_rfq_is_draft`, which reads
  the RFQ `FOR SHARE`) and are never changed or deleted. Permissions
  `procurement.rfq.read` and `procurement.rfq.manage`. Schema tests in
  `db/rfq_test.go`.
- **API.** `POST /v1/rfqs` (idempotent by its key), `GET /v1/rfqs`
  (filters `legal_entity_id`, `supplier_id` for the RFQs a supplier is
  invited to, `status`, `document_number`; cursor paging by `due_date` or
  `document_number`), `GET /v1/rfqs/{id}` and `POST /v1/rfqs/{id}:publish`
  (`If-Match`). Refusals name the request's own field: `supplier_ids[i]`
  for a supplier that cannot be ordered from
  (`procurement.supplier_not_usable`) or is named twice
  (`procurement.rfq_supplier_repeated`), `due_date` for a date before the
  company's today (`procurement.rfq_due_date_past`, with the date and the
  zone), `items[i]…` for an item. At publication a supplier blocked since
  is refused as `suppliers[i].supplier_id`, its place in the RFQ.
- **Code.** `internal/procurement/rfq.go` and `rfq_api.go`.
  `Endpoints.supplier`'s check is `supplierAt`, which takes the field it
  refuses by. An RFQ is shown named (`Endpoints.named`): its suppliers
  through `Endpoints.Partners` (`SupplierNames`, wired to
  `partner.Payees`), its items' accounts through `Endpoints.AccountNames`
  (wired to `ledger.AssetReads`), each in one call for a page, and its
  company's code and name from the directory.
- **Events.** `RfqChanged` (`created`, `published`) and `RfqPublished`
  with the suppliers invited by id and number, each naming the company
  by `legal_entity_code` beside its id, in `api/asyncapi.yaml`.
- **Number range.** `rfq`: the walkthrough's tenant and the test fixture
  have one (`RQ`). The configuration generated from an SAP extract
  (`internal/delivery/sapconfig`) does not add one yet, as it adds none
  for customer invoices.
- **Walkthrough.** `step_rfq` (step 54): an RFQ for two items with two
  suppliers invited, a supplier named twice and a past due date refused,
  published once.

### T82b Supplier quotations recorded (migration 000277)

- **Schema.** `procurement.supplier_quotation` (its RFQ and supplier, the
  supplier's reference, `valid_until`, status `recorded`, version): one
  per RFQ and supplier (`supplier_quotation_once`), only of a supplier the
  RFQ invites (`supplier_quotation_invited`, a foreign key to
  `rfq_supplier`) and only for a published RFQ (trigger
  `supplier_quotation_of_published`, which reads the RFQ `FOR SHARE`).
  `procurement.supplier_quotation_item`: a price above zero and a price
  unit for one item of the quotation's RFQ (foreign keys to the quotation
  within its RFQ and to `rfq_item`), once per quotation. Neither is
  changed or deleted (T82d grants what an award sets). The total is not
  stored. Schema tests in `db/supplier_quotation_test.go`.
- **API.** `POST /v1/rfqs/{id}/quotations` (`procurement.rfq.manage`,
  idempotent by its key) reads the RFQ under its row lock, so the
  quotations of one RFQ are recorded one after another and T82d's award
  waits for them; a supplier's second quotation is refused (409
  `procurement.quotation_exists`, from `supplier_quotation_once`), a draft
  RFQ refuses every quotation (409 `procurement.invalid_status`) and a
  supplier the RFQ does not invite is refused by `supplier_id` (422
  `procurement.supplier_not_invited`). A price is refused by its own
  place: `items[i].rfq_item_no` for an item the RFQ lacks
  (`procurement.unknown_rfq_item`) or prices twice
  (`procurement.quotation_item_repeated`), `items[i].unit_price` for
  another currency or a price not above zero. `GET
  /v1/rfqs/{id}/quotations` (`procurement.rfq.read`; filter `supplier_id`;
  cursor paging by `created_at`) lists them. A quotation has no address of
  its own: the `Location` of a recorded one is the list filtered to its
  supplier, which holds it alone.
- **Representation.** Each quotation carries its RFQ's id and number, its
  supplier by id, number and name, each price beside the RFQ item it
  prices (material key, description, quantity and unit) with the item's
  value, `total` (a `Money`, in the RFQ's currency), `complete` and
  `unquoted_item_nos`.
- **Total.** Each item's value is the RFQ item's quantity × unit price ÷
  price unit, rounded half away from zero to the minor unit
  (`money.Scale`); the total is their sum (`money.Sum`), through
  `itemsTotal`, as a purchase order's. A quotation whose values or total
  are out of an amount's range is refused (`procurement.amount_too_large`).
- **Code.** `internal/procurement/rfq_quotations.go`; handlers in
  `rfq_api.go`. `RFQ` carries its currency's exponent.
- **Event.** `SupplierQuotationRecorded` (the supplier by id and number,
  the company by id and code, the total, `complete`), in `api/asyncapi.yaml`.
- **Walkthrough.** `step_rfq` extended: a quotation recorded for each of
  the two suppliers, one of them cheaper in total, a second quotation of a
  supplier and a supplier not invited refused.

### T82c Quotations compared (no migration)

- **API.** `GET /v1/rfqs/{id}/comparison` (`procurement.rfq.read`; agents
  may read it: the week 3 agent does) answers the comparison of one RFQ,
  computed on the read and stored nowhere: the RFQ by id, number, company
  (id, code, name), currency, status and due date; `as_of`, the legal
  entity's date today in its company's time zone, the day validity is
  judged on; `invited_count`, `answered_count` and `ranked_count`; the
  RFQ's `items`, each with the lowest amount quoted for it
  (`lowest_amount`, absent when no quotation that is not expired prices
  it); and `suppliers`, one row per supplier invited (id, number, name).
- **Rows.** A row carries `answered`, and for a quotation its id,
  reference, `valid_until`, status, `complete`, `expired`,
  `unquoted_item_nos`, `total` and `prices` (RFQ item number, unit price,
  price unit, amount, `lowest`). The quotations that are complete and
  not expired (`valid_until` not before `as_of`) are ranked by total, the
  lowest first (`rank`); equal totals share a rank and the next is
  skipped (1, 1, 3). After them come the quotations not ranked
  (incomplete or expired, without a `rank`), then the suppliers without
  a quotation (`answered` false, every item in `unquoted_item_nos`).
  Within each group, and between equal totals, the rows stand in the
  order the suppliers were invited, so the same data reads the same way
  each time.
- **Lowest.** Amounts are compared, not unit prices: quantity × unit
  price ÷ price unit in minor units, so prices with different price
  units compare exactly. The lowest amount of an RFQ item is taken over
  the quotations not expired, an incomplete one included, and every
  price at it is marked.
- **Code.** `internal/procurement/rfq_comparison.go` (`compare`, pure;
  `Compared.CanWin`, the rule T82d awards by); the handler in
  `rfq_api.go`. One query reads the RFQ's quotations. `itemsTotal`'s loop
  is `itemAmounts` (`common.go`), which the comparison shares. An amount
  out of range, which recording refuses, fails the read rather than
  ranking a wrong total.
- **Budget.** `TestRFQComparisonBudget`: an RFQ of 50 items and 20
  quotations (1,000 prices) within the budget of a list page (p95
  150 ms; 01 §1.3), measured at a median of 14 ms. That is far above an
  RFQ a buyer types quotations into, and far below the contract's bound
  (100 suppliers of 1,000 items), which no requirement asks a budget
  for.
- **Walkthrough.** `step_rfq` extended: the comparison ranks the cheaper
  supplier first and marks the lowest amount of each item.
