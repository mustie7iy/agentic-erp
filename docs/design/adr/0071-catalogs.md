# ADR-0071: Catalogs

**Status:** accepted (T84a, 2026-11-12; decided by default under ADR-0053 d;
`go-reviewer` had no blocking finding on T84a's last commit). It decides
catalog items and the requisition item taken from one, the slices T84a
and T84b (`docs/design/plan-breadth-week2.md`, Lane B). Each slice records
what it built under "As built". The defaults taken and the questions left
are listed at the end for the product owner's digest.

## Context

The product owner asked for a first usable version of every capability in
scope by 2026-12-01, the main path only (2026-11-10, breadth first). The
tracker's group Sourcing and procurement › Catalogs and guided buying has
two placeholders, Catalog connector (punch-out) and Requisitions from
catalogs. `plan-breadth-week2.md` splits them into T84a (catalog items
loaded and searched) and T84b (a requisition item from the catalog) and
asks T84a to write this ADR.

What the design already settles:

- Requisitions, contracts and purchase orders are the procurement
  context's (03 §3.8): a requisition names a suggested supplier and
  contract, its items a description, quantity, unit and estimated price;
  an order's supplier must be usable in the company
  (`procurement.supplier_not_usable`); a contract item prices an order
  item that names it (`applyContractPrice`).
- There is no material master (outside the required scope; the plan's
  "Boundaries"): what is bought is named by a description and a key.
- ADR-0003 (RLS), ADR-0005 (money in minor units), ADR-0007 (outbox),
  ADR-0008 (state guards), ADR-0011 (audit), ADR-0053 (slices, headless
  first). Events name a business partner by id and number, never by name
  (product owner, 2026-11-05).

Nothing settles what a catalog is here. The tracker's placeholder speaks
of a connector to a supplier's catalog (punch-out). The plan leaves that
for after 1 December and takes the simplest thing that makes the main
path work (default 18): the catalog is the tenant's own table.

## Decision

### 1. The catalog is the tenant's own table, per company

A catalog item is a row of `procurement.catalog_item`: a supplier's item
in one company (legal entity), with an item key, a description, a unit of
measure and one price per price unit in a currency. It belongs to the
procurement context, beside the requisitions that take it; no new context
and no new import.

Nothing is fetched from a supplier's system: there is no connector, no
URL, no credential and no file read in the first version. A catalog item
is ordinary request data, checked and bounded by the contract as any
other (a key of at most 40 characters, a description of at most 200, one
item per request). Punch-out (cXML PunchOut, OCI), a bulk or scheduled
load and their connector are the "later" leaf; when they are built they
read outside data and need their own decision on what is fetched, from
where and with which credentials (secrets by ADR-0017, connectors
as ADR-0038 and ADR-0061 build theirs).

### 2. Loading an item

`POST /v1/catalog-items` loads one item, `active` at once, by permission
`procurement.catalog.manage`. It is checked as an order is:

- the company is an active legal entity the caller may act in;
- the supplier can be ordered from in the company (`e.supplier`, the
  check of a purchase order);
- the unit is one of the reference data, the currency a known one, the
  price positive with the currency's decimals;
- the item key has no white space around it and is the supplier's once
  among the company's active items (a partial unique index; 409
  `procurement.catalog_item_exists`). The same key may be another
  supplier's, or the supplier's in another company.

An item may name the contract item that prices an order of it
(`contract_id` with `contract_item_no`, both or neither): an item of an
`active` contract of the same company and supplier, in the catalog
item's currency and unit, at the catalog item's price: the same amount
per the same price unit, compared as written (422
`procurement.catalog_item_price`; product owner, 2026-11-12, T84c). So
the estimate a requisition takes from the catalog is the price an order
that names the contract item takes.
The contract's state is checked when the item is loaded and not held
afterwards: a contract terminated or expired later leaves the catalog
item naming it, and the requisition and the order that use it check the
contract again, as they do for any contract.

What an item offers is not changed afterwards: the application may
update an item's status and nothing else (column privileges). A new
price is a new item after the old one is retired. Changing a price with
validity dates is the "later" leaf.

### 3. Searching and retiring

`GET /v1/catalog-items` searches one company's catalog
(`legal_entity_id` is required: a catalog is a company's), by permission
`procurement.catalog.read`:

- `text` finds the items whose item key or description contains it,
  whatever the case. The table keeps the two in lower case
  (`search_text`, generated), and the search matches the text in it as
  plain text: no pattern characters, no ranking.
- `supplier_id` those of one supplier; `status` the active items when
  absent, the retired ones when asked.
- Keyset-paged by item key (default) or creation time, ending in the id,
  so pages are stable.

Each item is shown with its supplier's number and name beside the id
(`partner.Payees.Names`, as a payment shows its supplier's) and, when it
names one, its contract's document number.

`POST /v1/catalog-items/{id}:retire` moves an active item to `retired`
once, at the version read (`If-Match`; 409 `procurement.invalid_status`
for a retired one). A retired item is read by id and found with
`status=retired`; its key is free. Items are never deleted.

The search reads the company's items in key order and matches the text
in each. That is enough for a catalog loaded item by item; a larger
catalog's search (an index on the text, ranking) comes with the bulk
load (the "later" leaf).

### 4. Event and audit

`CatalogItemChanged` (procurement's outbox, `x-webhook`) at each change
of an item's status: loaded (`active`, status before null) and retired.
It carries the item's key, unit, price and contract item and names the
supplier by id and number, never by name; it does not carry the
description. Audit actions `procurement.catalog_item.create` and
`.retire`.

### 5. Permissions

`procurement.catalog.read` (search and read) and
`procurement.catalog.manage` (load and retire), neither an approval
permission. Loading needs no approval: a catalog item orders nothing and
posts nothing, and a requisition from it goes through approval as any
other.

### 6. A requisition item from the catalog (T84b)

A requisition item may name a catalog item (`catalog_item_id`), sent
with a quantity and the account to charge:

- It takes the catalog item's description, price and price unit and
  shows its item key. It sends none of the three, or the catalog item's
  own: another is refused (422 `procurement.catalog_item_override`;
  first built as the plan's "a price sent with it is refused", changed
  by the product owner on 2026-11-12 so that a requisition read back
  can be sent again, T84d), and its quantity is in the catalog item's
  unit: the shared `Quantity` keeps its unit, and another unit is
  refused by the same code rather than converted or silently replaced.
- The catalog item is an `active` one of the requisition's legal entity
  that the caller may act in, priced in the requisition's currency (422
  `procurement.invalid_catalog_item` on `items[i].catalog_item_id`, the
  detail naming the item by its key and saying why).
- The requisition's supplier is the catalog item's, and its contract the
  one the catalog item names, if it names one. What the requisition does
  not name it takes from the catalog item; another supplier or contract
  is refused naming the item (422 `procurement.catalog_item_supplier`,
  `procurement.catalog_item_contract`), so catalog items of two
  suppliers, or priced by two contracts, do not share a requisition (a
  cart split by supplier is the "later" leaf). A catalog item that names
  no contract leaves the contract the requester's. The contract is then
  checked as any requisition's (`checkRequisitionContract`) and the
  supplier as any (`e.supplier`).
- The rule holds while the requisition is a draft: a change that sends
  items resolves them again from the catalog (a catalog item retired
  since is refused); a change of the supplier or the contract that leaves
  the items is held to the stored items' catalog items, retired or not,
  and clearing one takes it from them again.
- The requester needs `procurement.catalog.read` besides the
  requisition's permission: taking an item reads the catalog (its
  description and price), and no permission is crossed (04 §4.4).
- The account, the account assignment and the cost center stay the
  requester's to give. An item that names no catalog item sends its
  description and price as before (422 `request.validation_failed`
  without).

The item stores `catalog_item_id` and a copy of what it took: a catalog
item retired later stays on the requisitions that took it, at the price
they took. The requisition is submitted, approved and ordered as any
other; the order's items are the buyer's, as today.

## Consequences

- A requester finds what to buy and its price in the company's own
  catalog; the price on a requisition from the catalog is the catalog's.
- No outside data is read, so the first version adds no connector, no
  secret and no network path.
- A catalog is kept by hand through the API until the bulk load is
  built; the search is sized for that.
- A price change is a retire and a load, which leaves the old item on
  the requisitions that took it.

## Defaults chosen (for the product owner's digest)

1. The catalog is the tenant's own table per company, loaded one item
   per request; no punch-out, no file load (plan default 18).
2. An item has one price and is not changed; it is retired and loaded
   again.
3. An item key is the supplier's once among a company's active items; a
   retired item frees it.
4. The search is by text contained in the key or the description,
   whatever the case, with no ranking; it requires the company and
   returns active items unless `status=retired` is asked.
5. A catalog item naming a contract item keeps its own price.
   Overruled by the product owner on 2026-11-12: its price is the
   contract item's, or the load is refused (T84c).
6. Loading and retiring are by permission, without approval; agents are
   not excluded, as from a contract's creation.
7. `CatalogItemChanged` names the supplier by id and number and carries
   no description.
8. An item from the catalog sends no description, price or price unit
   and is in the catalog item's unit; another unit is refused, not
   converted. Changed by the product owner on 2026-11-12: one equal to
   the catalog item's is accepted, another refused (T84d).
9. A requisition has catalog items of one supplier and at most one
   contract; what it does not name it takes from them.
10. Taking an item from the catalog needs `procurement.catalog.read`.
11. Submission does not check the catalog again: an item retired after
    the requisition took it stays at the price taken.

## Questions

1. **Catalog prices and contracts** (product owner; plan question 13): a
   catalog item's own price is used on the requisition; when it names a
   contract item, the order takes the contract's price as today. Should
   the catalog refuse a price that differs from its contract item's?
   **Answered 2026-11-12: yes** (T84c, below).

## As built

### T84a Catalog items (migration 000287)

- **Schema.** `procurement.catalog_item` with its checks, the partial
  unique index `catalog_item_active_key`, the search's index
  `catalog_item_by_key`, the generated `search_text`, the foreign key to
  the contract item, update granted to the application on `status`,
  `version`, `updated_by` and `updated_at` alone, and the two
  permissions; the rules are in 03 §3.8 and `db/catalog_test.go`.
- **Code.** `internal/procurement/catalog.go` (the item, its checks, its
  event) and `catalog_api.go` (the four handlers), reusing
  `e.legalEntity`, `e.supplier`, `exponent`, `amount`,
  `referencedContract` and `statusConflict`. `Endpoints` gains the port
  `SupplierNames` (the existing interface, wired to `partner.Payees`).
  No other procurement file changed.
- **Operations.** `POST /v1/catalog-items` (201 with `Location`),
  `GET /v1/catalog-items`, `GET /v1/catalog-items/{id}`,
  `POST /v1/catalog-items/{id}:retire`. A refusal names its field:
  `procurement.invalid_item_key` (`item_key`),
  `procurement.invalid_contract` (`contract_id`),
  `procurement.invalid_contract_item` (`contract_item_no`),
  `procurement.unknown_unit` (`unit`). The contract's problem codes list
  `procurement.currency_mismatch` for the load because the shared
  `amount` can return it; the load passes the price's own currency, so
  it does not occur.
- **Budget.** `TestCatalogSearchBudget`: one page of a company's 50,000
  active items among its tenant's 200,000 against the list budget (p95
  150 ms), the worst case being a text nothing contains.
- **Walkthrough.** `step_catalog` (step 61): Priya loads three items of
  Tyrell into Maple's catalog, a second item with a key in use is
  refused, she retires one; Ben finds the other two by a word of their
  description, one by text and supplier, and the retired one only with
  `status=retired`.

### T84b Requisition from the catalog (migrations 000288, 000289)

- **Schema.** `procurement.requisition_item.catalog_item_id` (nullable)
  with `requisition_item_catalog_item_fkey`, added `NOT VALID` by 000288
  and validated by 000289; `db/catalog_test.go`. During a rollout the
  previous release replaces a draft's items by an upsert that does not
  set the column, so the trigger `requisition_item_unlink_catalog`
  (000288) ends the link of an item whose description, unit, price or
  price unit an update changes while leaving `catalog_item_id` as it
  was (review round 1); this release never meets it, since a catalog
  item does not change. It can be dropped by a contract migration once
  no release without the column runs.
- **Contract.** The requisition's create and update requests take
  `RequisitionItemInput` (quantity and account required; description and
  price required unless `catalog_item_id` is given), a schema of its own
  so that the item read back, `RequisitionItem`, keeps its required
  fields: no breaking change. `RequisitionItem` gains the read-only
  `catalog_item_id` and `item_key`. No new operation.
- **Code.** `internal/procurement/catalog_requisition.go`: `fromCatalog`
  (the items resolved), `catalogParties` (the supplier and contract),
  `storedCatalogUse` (a change that leaves the items) and
  `requisitionParties`, which `createRequisition` and
  `updateRequisition` call in place of their contract and supplier
  checks. `requisitions.go`: `Item` and `ItemInput` carry
  `CatalogItemID`, `requisitionSelect` reads it with the catalog item's
  key, `writeRequisitionItems` writes it. `api.go`: `requisitionInputs`
  converts the new input and `requisitionToAPI` shows the two fields.
  Submission, approval and ordering are unchanged.
- **Budget.** No budget test reads a requisition (`scripts/budgets.sh`
  finds none behind its endpoints), so none was measured; the read adds
  one lookup by primary key per item taken from the catalog.
- **Walkthrough.** `step_catalog` extended: Ben raises a requisition for
  two chairs from the catalog item; a price of his own and the retired
  arm are refused; read back it has the catalog's price, CAD 450.00, and
  Tyrell as its supplier; he submits it and Priya approves it.

### T84c A catalog price is its contract item's (no migration)

Product owner, 2026-11-12 (row 8 of that day's decisions): a catalog item
that names a contract item and whose price differs from that contract
item's is refused when it is loaded. Question 1 is answered and default 5
overruled.

- **Rule.** `catalogContractPrice` (`catalog.go`), called by
  `catalogContractItem` once the contract item is found: the catalog
  item's amount and price unit (1 when absent) are the contract item's,
  compared as written and not as a ratio, so 10.00 per 2 is refused
  against 5.00 per 1. The currency was already the contract's (422
  `procurement.invalid_contract` on `contract_id`, unchanged). The refusal
  is 422 `procurement.catalog_item_price` on `unit_price.amount`, or on
  `price_unit` when only that differs, and its detail names the catalog
  item by its key, the contract item and both prices.
- **Every path.** `POST /v1/catalog-items` is the only path that creates
  a catalog item or sets its price: an item's price is not changed
  afterwards (the application may update its status alone, §2), and
  retiring changes no price. No other code writes the table.
- **Items already stored when the contract item's price changes later
  (decided by default).** It cannot: a contract's items are changed only
  while it is a draft (`updateContract`), a catalog item may name only an
  `active` contract, and neither an active contract's items nor a catalog
  item's price change afterwards. So the prices agree for as long as both
  exist and nothing is checked again. A contract terminated or expired
  later leaves the catalog item naming it, as before (§2). When changing
  a contract's or a catalog item's price is built (both "later" leaves),
  that change must keep the rule.
- **Items loaded before this rule.** Not rechecked and not migrated: no
  catalog exists outside tests and the walkthrough before the pilot, and
  the rule is the load's. No schema change: the rule compares two tables'
  rows at load and is kept by the two being immutable, not by a
  constraint.
- **Walkthrough.** Step 61 loads no contract-priced item (the story has
  no contract), so it is unchanged and keeps passing; the rule's evidence
  is `TestCatalogItemContract`.

### T84d A requisition sent back with the catalog's price (no migration)

Product owner, 2026-11-12 (row 9): a requisition created from a catalog
item and sent back with a price equal to the catalog's is accepted; one
with a different price is refused. Before, any price sent with a catalog
item was refused, so the items of a requisition read back could not be
sent again as they were read.

- **Rule.** `noOverride` (`catalog_requisition.go`), which `fromCatalog`
  calls for every item that names a catalog item, on both paths that
  send items (`POST /v1/requisitions`, and `PATCH /v1/requisitions/{id}`
  with `items`): a `unit_price` sent is the catalog item's, the amount
  as the API shows it (the plain amount with the currency's decimals) in
  the same currency; anything else is 422
  `procurement.catalog_item_override` on `items[i].unit_price`, the
  detail naming the catalog item by its key with its price. The stored
  item is still filled from the catalog item, never from what was sent.
- **Price unit and description too (decided by default).** A requisition
  read back carries the item's `price_unit` and `description` beside its
  price, and the price is an amount per price unit. So the same rule
  holds for both: the catalog item's own is accepted, another refused
  on its field with the same code. Without it a requisition sent back
  as read would still be refused. The read-only `item_key` is not part
  of the request and is ignored, as before.
- **Compared with the catalog item, not the stored copy (decided by
  default).** The two are equal, since a catalog item's price does not
  change and the stored item is a copy of it. A change that sends items
  still resolves them from the catalog, so the items of a draft whose
  catalog item was retired since are refused when sent back
  (`procurement.invalid_catalog_item`, §6, unchanged); a change that
  leaves the items is not.
- **Contract.** No schema change: `RequisitionItemInput` already had the
  three fields; their descriptions and the two operations' say the rule.
  No new problem code.
- **Walkthrough.** `step_catalog` (step 61) extended: before submitting,
  Ben sends the requisition's item back as he read it, accepted at the
  catalog's price; the same item at a price of his own is refused, as
  it is on creation.
