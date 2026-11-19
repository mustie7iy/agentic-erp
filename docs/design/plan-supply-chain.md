# Supply chain plan (17 November 2026)

**Status:** plan (product owner, 2026-11-17: "remove the supply chain plan
and include it as part of the current implementation plan"). Supply chain
was outside the required scope of the breadth phase (product owner,
2026-11-10; `plan-breadth-week2.md` and `plan-breadth-week3.md`,
"Boundaries"); it joins the scope with this plan. It splits the tracker's
Supply chain placeholders into slices (ADR-0053) in the form of
`plan-breadth-week3.md` (Adds, Acceptance, Later). `docs/tracker.json`
stays the only plan (07 §7.5): each slice is a leaf under its
placeholder's group, named by its section here (`S1a` …), and each
placeholder is narrowed to what no slice here builds, the variants named
in the slices' "Later". ADR-0082 decides the contexts, their published
APIs, the data model and the SAP behaviour followed.

## What every slice reuses

What `plan-breadth-week1.md` to `-week3.md` list under the same heading
applies unchanged: the unit of work and `reqctx.Request`, RLS with
`tenant_id` leading every key, `money.Minor` and `money.Sum`, row locks and
state guards, `Idempotency-Key` and `If-Match`, financial effects only
through the ledger's posting port, the context's outbox and inbox, an
audit record per state change, `problem.*` errors, contract first, real
PostgreSQL tests with `pgtest` and `pgtest.FailingTx`, a schema test per
database rule, the company's date in its time zone (`config.DocumentDay`),
events naming a customer or supplier by id and number only, none offered
as a webhook event until a consumer asks, and one new or extended step of
`scripts/walkthrough.sh` through the public API. Headless first.

Supply chain adds:

- **Quantities are exact.** A quantity is a decimal string in the API and
  an integer count of a fixed number of decimal places in the database
  (ADR-0082 §5); a conversion between units is an integer numerator and
  denominator, never a float, and a converted quantity that does not come
  out exact in the target unit's decimals is refused, not rounded.
- **Plants and storage locations are tenancy's org units** (`plant`,
  `storage_location` under its plant; `internal/tenancy`, built in phase
  1). No new organisational table is added; SAP's T001W and T001L map onto
  them.
- **Stock postings go through the ledger's posting port** (from S1d): the
  inventory context never writes ledger tables. Account determination by
  valuation class is the configuration's (`companies[].account_determination`,
  ADR-0082 §8).
- **Procurement and sales read materials through materials' published
  API** (from S1e and S1f); the materials context imports neither.

## Pilot relevance

The pilot's questionnaire answers are not known. **Assumption, confirmed
by the product owner on 2026-11-18:** the pilot company (Maple Industries
Canada, the walkthrough's) buys and sells physical goods it keeps in stock
and manufactures some of them; it has one plant per physical stock site.
Materials are valued by material type, as SAP practice is: purchased
materials (`raw_material`, `trading_good`) at moving average price,
own-produced ones (`semi_finished`, `finished_good`) at standard price.
Production stays outside this ERP (option A; manufacturing, production
orders and product costing come after completion): the pilot runs it in
its own system (SAP PP, for instance), receives finished goods into stock
by a goods movement (S1c's `501`) at a standard price maintained by hand
(S1d's price change), and issues components by goods issue (S1c's `201`).
It has no batches, serial numbers, intercompany stock transfers, MRP or
external warehouse, transport or trade systems at go-live. Under that
assumption the slices marked **pilot** below are pilot-relevant; the
others are not until the questionnaire says so. No tracker leaf carries
the `pilot` tag for this: the pilot scope changes only by the product
owner's decision. Which price control a valuation takes stays the
inventory accountant's choice when it is created (S1d); a default by
material type is not built.

## Build order and lanes

- **S1, the foundation, in sequence** (one lane, `scm`): S1a → S1b → S1c →
  S1d → S1e → S1f. Each depends on the one before; S1e and S1f are the
  first to change procurement and sales.
- **S2, the flows, in parallel once S1 is on main** (up to three lanes on
  code that does not overlap): shipping (S2a → S2b), physical inventory
  (S2c → S2d → S2e), batches and serials (S2f → S2g), transfers (S2h →
  S2i), ATP (S2j), planning (S2k → S2l → S2m → S2n), SAP migration (S2o →
  S2p), connectors (S2q, S2r, S2s).

## New contexts (ADR-0082)

- **`materials`** (schema `materials`, `internal/materials`): the material
  master and its governance. Imports `approvals`, `config` and `tenancy`
  only; procurement, sales and inventory import its published API.
- **`inventory`** (schema `inventory`, `internal/inventory`, from S1c):
  stock by location, goods movements, valuation and physical inventory.
  Reads materials through `materials`' published API and posts through
  the ledger's posting port.
- **`shipping`** (schema `shipping`, `internal/shipping`, from S2a;
  ADR-0083): outbound deliveries, their picking and goods issue. A leaf:
  it reads sales' orders and writes their deliveries through sales'
  published API and posts the goods issue through inventory's.
- Planning (S2k–S2n) and the connectors (S2q–S2s) are decided by their
  own ADRs when they are built.

## S1 Foundation

### S1a Material master (pilot)

**Capability.** A master data specialist creates a material with its
number from the company's `material` range, a description, a base unit of
measure, a material type, a material group and a status, extends it to
plants and their storage locations, gives it alternative units of measure
with exact conversions, reads and lists materials, and changes one at the
version read.

- **Reuses:** tenancy's plants and storage locations (org units),
  `ref.unit_of_measure`, the configuration's number ranges and company
  dates, the ledger's fiscal calendar, `etag`, audit, outbox, pagination.
- **Adds:** the context `materials` (schema, RLS, outbox and inbox, its
  row in `internal/layout`); tables `materials.material` (SAP MARA and
  MAKT's one description), `materials.plant_data` (MARC: the material in a
  plant), `materials.storage_location_data` (MARD without stock) and
  `materials.unit_conversion` (MARM); `POST /v1/materials`,
  `GET /v1/materials`, `GET /v1/materials/{id}`, `PATCH
  /v1/materials/{id}` (`If-Match`); event `MaterialChanged`; permissions
  `materials.material.read`, `.write`; number range `material`, unique in
  the tenant across companies and years (`config.RangeRepeats`); ADR-0082.
- **Walkthrough:** `step_material`: a bracket is created in the pilot
  company's plant with a box of 20 as an alternative unit, read back,
  changed at its version and blocked; a stale version, a conversion to the
  base unit itself and a storage location of another plant are refused.
- **Acceptance:** the number is the company range's next; the base unit,
  every alternative unit and every conversion factor are known and exact
  (integers above zero, no float); a storage location is one of a plant
  the material has; a change needs the current version (412 otherwise)
  and records its audit with the values before and after; the base unit
  and material type do not change once created.
- **Depends on:** nothing.

**Later for Material master:** further descriptions by language (MAKT);
the material's sales organisation and purchasing views (MVKE, EINA/EINE
belong to S1f and sourcing); dimensions, weights and EAN/GTIN codes;
material types and groups from configuration (T134, T023) instead of an
open enumeration and a code; plant-specific status and MRP data (MARC
fields beyond the assignment, S2k); external number assignment;
classification and characteristics; changing a material's base unit or
type once used (SAP's MM02 refusal and MMAM).

### S1b Material governance (pilot)

**Capability.** Creating or changing a material is a change request
approved through the existing approvals, as a business partner's is: the
material is `pending` until its create change is approved, a change of an
active material applies when its change is approved, possible duplicates
by description are listed for the approver, and blocking applies at once.

- **Reuses:** `internal/approvals` (`Open`, the handler map,
  `approvals.Paths`), the business partner governance's rules (one pending
  change of a kind per object, a rejected or cancelled create leaving no
  material behind, a duplicate list on the create change, blocking without
  approval; `internal/partner` changes.go and duplicates.go), S1a.
- **Adds:** `materials.material_change` (kind `create`, `update`, `block`,
  `unblock`; status `pending`, `applied`, `rejected`, `cancelled`;
  payload, reason, possible duplicates, the approval request);
  `GET /v1/material-changes`, `GET /v1/material-changes/{id}`; `POST
  /v1/materials` creates the material `pending` with its create change;
  `PATCH /v1/materials/{id}` opens an `update` change (the material
  answered with `pending_change_id`) unless it only blocks; approval
  object type `material_change` decided by the role
  `process_owner`; permission `materials.change.approve`; event
  `MaterialChangeChanged`.
- **Walkthrough:** `step_material_governance`: a new material waits for
  its approval and is listed with its possible duplicate; a second
  material with the same description is refused until confirmed; the
  process owner approves; a change waits and applies on approval; a
  rejected change leaves the material as it was.
- **Acceptance:** a material with the description of another, white space
  and case aside, is refused (409) unless the request confirms it, and its
  change lists the others; a second pending change of the same material
  is refused (409); approval applies the change to the material's then
  current version, rejection and withdrawal leave it unchanged, and a
  rejected create leaves the material pending, used nowhere and out of the
  duplicate checks (as a business partner's);
  the decisions are the approvals context's (the requester's own approval
  as approvals refuses it).

**Later for Governance for materials:** approval by plant or material
type, a configurable approver role and multi-step policies; a fuzzy
duplicate check (similar, not equal, descriptions; SAP MDG's match
profiles); field-level change tracking for downstream systems (change
pointers, ALE distribution); mass changes.

### S1c Stock by location and goods movements (pilot)

**Capability.** A warehouse clerk posts goods movements of a material at
a plant's storage location — a receipt without an order, an initial entry,
an issue to a cost center, a scrapping, a transfer between storage
locations or plants — each numbered as a material document, reverses one,
and reads the stock by location now or as at a date.

- **Reuses:** S1a's materials (`materials.Materials`, the published API),
  their units and conversions, tenancy's plants and storage locations,
  the ledger's cost centers (by id, read through a port), the
  configuration's number ranges (`material_document`).
- **Adds:** the context `inventory` (schema, RLS, outbox, `internal/layout`
  row); `inventory.material_document` and `.movement` (insert-only items,
  each with a movement type, quantity in the base unit and the entered
  unit, plant and storage location, sign), `inventory.stock` (the
  quantity per material, plant and storage location, updated under a row
  lock in the movement's transaction); movement types 501, 561, 201, 551,
  311, 301 and their reversals 502, 562, 202, 552, 312, 302 (SAP's,
  ADR-0082 §7, from memory); `POST /v1/material-documents`,
  `GET /v1/material-documents`, `GET /v1/material-documents/{id}`,
  `POST /v1/material-documents/{id}:reverse`, `GET /v1/stock`
  (`as_of`); event `MaterialDocumentPosted`; permissions
  `inventory.movement.post`, `inventory.stock.read`.
- **Walkthrough:** `step_goods_movements`: an initial entry, an issue to a
  cost center, a transfer to another storage location, a reversal; the
  stock read now and as at the day before.
- **Acceptance:** stock never goes below zero (409, two issues racing for
  the last quantity issue it once); a movement of a blocked or pending
  material, or at a plant the material is not extended to, is refused; a
  reversal reverses the whole document once; the stock as at a date is
  the sum of the movements posted up to it; nothing is posted to the
  ledger yet (S1d values the movements); a document is posted on its
  company's date today, one movement type and one company per document
  (a transfer between plants stays in the company).

**Later for Stock by location and Goods movements:** posting into an
earlier day or the previous period, stock types (quality
inspection, blocked, restricted; SAP's 321–344), special stocks
(consignment, customer, project), negative stock by plant, goods movement
reasons, reservations (RESB), the movement types of production (261, 101
for orders) which wait for Manufacturing, period-end closing of the
material period (MMPV), printing the goods receipt slip.

### S1d Valuation and inventory postings (pilot)

**Capability.** An inventory accountant gives a material its valuation in
a plant — moving average or standard price, a valuation class — and from
then every goods movement of it posts its value through the ledger by the
company's account determination; a price change revalues the stock.

- **Reuses:** S1c's movements, `ledger.Postings.Post` with
  `source_context` `inventory`, the account determination
  (`companies[].account_determination`), `money.Scale`, the ledger's
  calendar and open periods.
- **Adds:** `inventory.valuation` (SAP MBEW: price control `S` or `V`,
  standard price, moving price, price unit, total quantity and value per
  material and plant as the valuation area), the account keys by
  valuation class (stock, offsetting consumption, scrapping, initial
  entry, price difference, revaluation; ADR-0082 §8), the posting of each
  movement in its transaction (one journal entry per material document,
  document type `WA`/`WE` by direction), `POST /v1/material-valuations`,
  `PATCH` for the valuation class, `POST
  /v1/material-valuations/{id}:changePrice` (SAP MR21); event
  `MaterialPriceChanged`; permissions `inventory.valuation.read`,
  `.write`.
- **Walkthrough:** `step_valuation`: the bracket valued at moving average;
  a receipt at a price raises the moving price; an issue posts its value
  to consumption; a price change of a standard-priced material posts the
  revaluation.
- **Acceptance:** the stock value always equals quantity × price for a
  standard-priced material and the running value for a moving-average one,
  with the rounding difference of an issue taken by the value left
  (`money.Sum`); a movement without a valuation or without the accounts it
  needs is refused (422) and posts nothing; the ledger's stock account
  balance equals the valuation's total value; a price change is refused
  while the period is closed; a transfer within a plant posts nothing.

**Later for Moving average and standard price and Inventory postings:**
the material ledger and actual costing (SAP CKM3, multiple currencies and
valuations), split valuation (by batch or origin), future and previous
period prices, valuation of special stocks, the lowest value principle at
year end, revaluation with a price change in a past period, below-zero
moving prices, small-difference tolerances (SAP's OMR4-like checks).

### S1e Purchase order goods receipt into stock, GR/IR and the invoice (pilot)

**Capability.** A purchase order item names a material, a plant and a
storage location; its goods receipt is a movement 101 into stock valued
at the order price, posting stock against GR/IR; the supplier invoice
against the receipt clears GR/IR and puts a price difference on the stock
(moving average, as far as stock is left) or the price difference account
(standard price).

- **Reuses:** procurement's purchase orders and goods receipts (T-items
  of phase 1, `internal/procurement`), payables' invoice verification and
  GR/IR clearing, S1c's movements and S1d's valuation, materials'
  published API.
- **Adds:** optional `material_id`, `plant_id`, `storage_location_id` on a
  purchase order item (the item's unit converted to the base unit by the
  material's conversions); procurement's receipt of such an item calls
  inventory's movement port in the same transaction (movement 101, its
  reversal 102); the invoice's price difference posted by the valuation's
  rule; ADR-0082 As built.
- **Walkthrough:** `step_stock_receipt`: an order for 100 brackets
  received into stock in two parts; the stock and its value read; the
  invoice at a higher price; the moving price after it.
- **Acceptance:** a receipt of a stock item without a valuation is refused
  and nothing is received; GR/IR of a stock item balances once the
  invoice is posted; an item without a material behaves as before
  (expense or asset account assignment); the receipt's reversal reverses
  stock and GR/IR once.
- **As built** (2026-11-18): ADR-0082 As built S1e, walkthrough step 130.

**Later for the goods receipt into stock:** goods receipt into quality
inspection, delivery tolerances against the order (over- and
underdelivery), GR-based invoice verification by receipt versus by order,
evaluated receipt settlement (ERS), returns to the supplier (122, 161),
delivery costs (planned freight) on the stock value.

### S1f Item keys of sales and procurement mapped onto materials (pilot)

**Capability.** A quotation, sales order, billing, requisition, RFQ and
catalog item may name a material instead of a free-text key: its
description and unit come from the material, its quantity is converted to
the material's base unit, and the existing free-text keys of a tenant are
mapped to materials once.

- **Reuses:** materials' published API, the sales and procurement items
  (`item_key`, `material_key`), sales prices by item key (T80a).
- **Adds:** optional `material_id` on those items (expand, then a mapping
  run that sets it from a key equal to a material's number, reported per
  key that matches none); sales prices found by material as well as by key;
  ADR-0082 As built, and the as-built notes of ADR-0067 and ADR-0071.
- **Walkthrough:** `step_material_items`: a sales order item and a
  requisition item for the bracket by material, their descriptions and
  units taken from it.
- **Acceptance:** a blocked or pending material is refused on a new item;
  a key that names no material stays a free-text item; nothing already
  billed or received changes.
- **Split** (lane scm4, 2026-11-19) into two slices, S1f1 and S1f2, each
  a leaf; the decisions below are by default (ADR-0082 As built S1f).

#### S1f1 Sales items by material (pilot)

**Capability.** A sales clerk enters a quotation's or a sales order's item
(or adds one by an order change) by its material: its description is the
material's unless the clerk gives one, its item key is the material's
number, and its quantity is in a unit of the material that converts to the
base unit exactly; the order made from the quotation, and the order's
billing documents, name the material.

- **Adds:** optional `material_id` on `SalesItemCreate` (`description`
  then optional) and on every sales item read (`SalesItem`: quotation,
  order and billing items); `sales.quotation_item.material_id` and
  `sales.sales_order_item.material_id` (expand: nullable, a material item
  has an item key, checked `NOT VALID` and validated by the next
  migration); `sales → materials` (ADR-0082 §3).
- **Decided by default:** the item key is the material's number (SAP's
  item names its material by MATNR, from memory), so prices, discounts,
  scales, revenue obligations, segments and analytics find a material item
  by key as they find any other ("sales prices found by material as well
  as by key" needs no change of the price records); another key sent with
  a material is refused (422 `sales.item_key_not_material`). The material
  is one of the tenant's in status `active` (unknown: 422
  `request.reference_not_found`; `pending` or `blocked`: 422
  `sales.material_not_usable`), of any type (sales views per sales
  organisation, MVKE, are Later). The quantity's unit is the base unit or
  an alternative unit and converts exactly in six decimals (422
  `sales.invalid_material_quantity`); the converted quantity is not
  stored: it is taken where stock moves (S2b), as S1e's receipt takes it.
  An item an order change keeps keeps its material, and its new quantity
  must still convert exactly; a material blocked since does not refuse
  the change, as S1e's kept stock item.
- **Walkthrough:** `step_material_items` (sales part): a sales order item
  for the bracket by material, in boxes of 20, takes the material's
  description and number and its price recorded by the number; a blocked
  material and a key other than the number are refused; the billing
  document's item names the material.
- **Acceptance:** a blocked or pending material is refused on a new item;
  an item without a material behaves as before; a quotation's material is
  carried to its order and its order's billing.
- **As built** (2026-11-19): ADR-0082 As built S1f, walkthrough step 135.

#### S1f2 Purchasing items by material and the catalog's keys mapped (pilot)

**Capability.** A requester enters a requisition item, a buyer an RFQ
item and a catalog manager a catalog item by its material, with the same
rules as a sales item; and the catalog manager maps a company's catalog
items whose key is a material's number onto that material once, with a
report of the keys that match none.

- **Adds:** requisition items' `material_id` (000006, unchecked until now)
  checked; optional `material_id` on RFQ items and catalog items
  (`procurement.rfq_item.material_id`, `procurement.catalog_item.material_id`;
  expand), their `material_key`/`item_key` the material's number, an
  RFQ's `material_key` and a catalog item's `item_key` then optional; the
  material carried from a catalog item to a requisition item, from a
  requisition item to an RFQ item and from an RFQ item to its award's
  contract item; `POST /v1/catalog-material-mappings` (permission
  `procurement.catalog.manage`) and its report; a catalog item's material
  set once and never changed (a database rule).
- **Decided by default:** the mapping maps the catalog, the master data
  new items are taken from: an active catalog item without a material
  whose key equals an active material's number and whose unit is a unit of
  that material. Documents keep their keys: a quotation, order, billing
  document, requisition, RFQ or contract is a record of what was offered,
  ordered or billed (so nothing billed or received changes), and SAP does
  not re-key a document's item either (from memory). Sales prices need no
  mapping: they are found by the material's number (S1f1). The report
  names every distinct key of the company's active catalog items still
  without a material, as `mapped`, `no_material` (no material has that
  number) or `not_mappable` (the material is not active, or the item's unit
  is not one of its units), with its items' count; a second run maps
  nothing twice.
- **Walkthrough:** `step_material_items` (purchasing part): a
  requisition item for the bracket by material takes its description; a
  catalog item keyed by the bracket's number and one by a free-text key are
  mapped, the one onto the bracket and the other reported as matching no
  material; a second run maps nothing.
- **Acceptance:** a blocked or pending material is refused on a new item;
  a key that names no material stays a free-text item, reported; a mapped
  catalog item keeps its material; nothing already billed or received
  changes.
- **As built** (2026-11-19): ADR-0082 As built S1f, walkthrough step 135.

**Later for mapping item keys:** customer-material info records (SAP
KNMT), purchasing info records and source lists (EINA/EINE, EORD), the
material's sales and purchasing texts, substitution of materials, sales
views per sales organisation (MVKE), mapping by a table of key and
material pairs rather than the number alone, mapping open documents'
items and contracts' items, checking the material a purchase order's or a
contract's own item names (still unchecked unless it is a stock item, S1e).

## S2 Flows (parallel once S1 is on main)

### S2a Outbound delivery and picking (pilot)

**Capability.** A shipping clerk creates an outbound delivery for a
confirmed sales order's items due, from a plant and storage location, and
records the picked quantities.

- **Adds:** a shipping context (its ADR) with the delivery document
  (header and items, numbered from `outbound_delivery`), picking status,
  `POST /v1/outbound-deliveries`, `:pick`; the sales order's T78c
  delivery confirmation becomes the delivery's goods issue for a material
  item (S2b).
- **Walkthrough:** `step_outbound_delivery`. **Acceptance:** a delivery
  takes no more than the order's open quantity; picking is at most the
  delivery quantity and from stock that is there.
- **Detailed** (lane ship, 2026-11-19; ADR-0083, decided by default):
  the context `shipping` (schema `shipping`, `internal/shipping`), a leaf
  reading the sales order through sales' published API, materials and
  stock through materials' and inventory's, tenancy's plants and storage
  locations and the configuration's ranges and company dates. A delivery
  (SAP LIKP/LIPS, from memory) is of one confirmed order that is not
  blocked for credit, from one plant and storage location of the order's
  company; its items are the order's items of a material kept in stock,
  each named once with a quantity in the order item's unit, at most what
  is open: ordered less delivered less what other deliveries not yet
  issued hold. Without items it takes every such item's open quantity.
  Sales keeps what deliveries hold per order item
  (`sales_order_item.in_delivery_quantity`, the sum of
  `sales.delivery_assignment`), so an order change cannot take an item's
  quantity below what is delivered and in delivery, nor remove the item or
  cancel the order while a delivery holds it. Picking
  (`:pick`, `If-Match`) sets each item's picked quantity, at most its
  delivery quantity and at most what the storage location holds of the
  material in the base unit (stock is not reserved: ATP is S2j); the
  picking status is `not_picked`, `partly_picked` or `picked` (SAP KOSTK
  A, B, C, from memory). Reads `GET /v1/outbound-deliveries` (company,
  order, status) and `/{id}`; permissions `shipping.delivery.read`,
  `.write`; event `OutboundDeliveryChanged`; human-only writes.
- **As built** (2026-11-19): ADR-0083 As built S2a, walkthrough step 142.

### S2b Goods issue, cost of goods sold and billing from the delivery (pilot)

**Capability.** Posting the delivery's goods issue (movement 601) reduces
stock and posts the cost of goods sold at the valuation price; billing
bills the delivered quantities; profitability shows cost of sales.

- **Adds:** the goods issue through inventory's movement port and the
  valuation (S1d) with the account key for cost of goods sold; the billing
  document from the delivery; profitability's cost of sales from the goods
  issue (ADR-0075 As built). **Walkthrough:** `step_goods_issue`.
  **Acceptance:** the goods issue posts once; its cost equals the stock
  value taken; a delivery without goods issue is not billed.
- **Split** (lane ship, 2026-11-19) into S2b1 and S2b2, each a leaf;
  decided by default (ADR-0083).

#### S2b1 Goods issue and cost of goods sold, billed from the delivery (pilot)

**Capability.** A warehouse clerk posts a picked delivery's goods issue:
its picked quantities leave the storage location's stock as a material
document of movement 601, valued at the valuation's price, its value
posted from the stock account to the class's cost of goods sold account;
the sales order's items are delivered by it, and billing bills them.

- **Adds:** `POST /v1/outbound-deliveries/{id}:postGoodsIssue`
  (`If-Match`, permission `shipping.goods_issue.post`); movement types 601
  and its reversal 602 in inventory (601 posted only by shipping, through
  `inventory.Endpoints.IssueForDelivery`; a person neither posts nor
  reverses it); the account kind `cost_of_goods_sold` of a valuation
  class (SAP GBB VAX, from memory), an expense; the sales delivery
  (T78c's record) written by the goods issue through sales' published API,
  naming the outbound delivery and the material document, with each
  item's cost; T78c's confirmation refused for an item of a material kept
  in stock (422 `sales.delivery_by_goods_issue`), which a service or
  free-text item, or a material kept in no stock, keeps.
- **Decided by default:** the goods issue issues each item's picked
  quantity and gives the rest back to the order, the delivery keeping its
  delivery quantity (SAP would change it to the picked one before the
  goods issue, from memory); an item picked at nothing
  issues nothing; a delivery with nothing picked is refused. It is posted
  on the company's date today, once (the delivery's status and row lock),
  for an order not blocked for credit. Billing is unchanged: it bills
  what is delivered and not billed, so a delivery without goods issue is
  not billed. Reversal of a goods issue (602 with the billing cancelled)
  is Later.
- **Walkthrough:** `step_goods_issue`: the delivery of step
  `step_outbound_delivery` picked and issued; stock and the moving price's
  value fall by the cost; the WA entry debits cost of goods sold; the
  order is billed for the issued quantity; a second goods issue and a
  confirmation of the bracket by T78c are refused.
- **Acceptance:** the goods issue posts once; its cost equals the stock
  value taken; a delivery without goods issue is not billed.
- **As built** (2026-11-19): ADR-0083 As built S2b1, walkthrough steps 143
  and 135 (its stock item shipped).

#### S2b2 Cost of sales in net revenue by profitability segment (pilot)

**Capability.** A controller reads cost of sales and margin per customer
and item key beside net revenue: the cost of the goods issues of the
period.

- **Adds:** `cost_of_sales` and `margin` on each row and the totals of
  `GET /v1/profitability-report`, `includes_cost` true; sales'
  `Orders.Segments` adds the goods issues' cost by the order item's
  segment.
- **Decided by default:** a goods issue counts in the period of its
  posting date (the date its cost is posted), in the segment of the order
  item it delivered, for an order in the company's currency, as the
  report counts revenue; one in another currency is left out and counted
  in `foreign_currency_documents`. Margin is net revenue less cost of
  sales. The margin report already shows the posted cost of goods sold
  when its account is a named cost of sales account.
- **Walkthrough:** `step_goods_issue` (the report's part).
- **Acceptance:** the segment's cost of sales equals the goods issue's
  cost; rows and totals add up.
- **As built** (2026-11-19): ADR-0083 As built S2b2, walkthrough step 143.

**Later for Shipping and delivery:** packing and handling units, shipping
points and routes, delivery split and grouping, proof of delivery,
delivery notes, returns deliveries (stock posting of T85b's returns),
the order-to-delivery variants of `plan-breadth-week2.md` §T78c.

### S2c Physical inventory count documents (pilot)

**Capability.** An inventory controller creates a count document for a
plant and storage location's materials, freezing the book quantity, and
enters the counted quantities.

- **Adds:** `inventory.count_document` and items (SAP MI01, MI04),
  posting block during the count. **Walkthrough:** `step_count`.
  **Acceptance:** one open count per material and storage location; a
  count is entered once per item and recounted only as a new document.
- **Detailed** (lane count, 2026-11-19; one slice, one leaf, nothing to
  split; decided by default, ADR-0082 As built S2c): a count document is
  of one plant's storage location (SAP IKPF's plant and storage location)
  with up to 100 items, one per material (ISEG), numbered from the
  company's `physical_inventory` range of the fiscal year of its date (the
  company's date today), `POST /v1/physical-inventory-documents`, `GET`
  (by plant, storage location, material and status; newest first) and
  `/{id}`, `:enterCount` (`If-Match`). An item's material is a stocked
  material of the tenant kept at that storage location and valued in its
  plant (so its difference can be posted, S2d), `active` or `blocked` (a
  blocked material's stock is still counted), never `pending`. The book
  quantity is the stock's as the document is created, read under the
  stock row's lock (a row with no quantity written for a material never
  moved there), and the **posting block** is always set (SAP's MI01
  indicator, here not optional, the safer default): while the document is
  open no goods movement of any kind (a person's, a goods receipt, a goods
  issue) moves its materials at that storage location (409
  `inventory.posting_blocked`, the database's `count_posting_block` on
  every movement), so the book quantity frozen is the book quantity when
  the differences are posted. One open count per material and storage
  location (409 `inventory.count_open`, the database's
  `count_item_one_open`). Counts are entered by item number (SAP MI04), in
  any unit of the material converted exactly to its base unit, zero
  included (a zero count is entered as `0`); an item counted is not
  counted again (409 `inventory.item_counted`); a document is `counting`
  until every item is counted, then `counted`. Permission
  `inventory.count.write`; reads under `inventory.stock.read`. Audited at
  each step; no event (the stock moves only by S2d's material documents,
  which publish `MaterialDocumentPosted`). **Later:** counts without a
  posting block, book quantities frozen while movements go on, counts by
  bin, batch or special stock, count date per item, a deletion flag (a
  document is closed by posting its differences or by rejecting them,
  S2d).

### S2d Inventory differences posted (pilot)

**Capability.** The inventory controller posts a count's differences
(movements 701, 702; SAP MI07), valued at the material's price.

- **Adds:** the difference posting through S1c and S1d with the account
  key for inventory differences. **Walkthrough:** extends `step_count`.
  **Acceptance:** book quantity after posting equals the count; a
  difference above the company's tolerance needs approval.
- **Detailed** (lane count, 2026-11-19; one slice, one leaf, nothing to
  split; decided by default, ADR-0082 As built S2d): `:postDifferences`
  (`If-Match`, permission `inventory.count.post`) on a `counted`
  document posts each item's difference (counted less book): the gains as
  one material document of movement type `701`, the losses as one of
  `702` (one movement type per document, As built S1c), each valued as
  any receipt or issue is (S1d: a gain at the moving or standard price, a
  loss at the value it takes from the stock) and posted (`WA`) between the
  class's stock account and its new `difference` account
  (`companies[].account_determination.inventory[].difference`, SAP GBB
  with modifier INV), an expense holding gains and losses alike. The
  **tolerance** is the company's `inventory_difference_tolerance`, an
  amount in its currency (SAP's tolerance group's limit per document,
  OMJ2): when the differences' values, gains and losses alike, total
  more, the document waits for the controller's approval
  (`pending_approval`; object type `inventory_difference`, permission
  `inventory.difference.approve`, the requester never the approver), and
  approval posts them as valued then; a company without a tolerance has
  every difference of any value approved. Rejection (`rejected`) or the
  requester's withdrawal (`withdrawn`) posts nothing and closes the
  document, lifting its posting block: the materials are recounted by a
  new document. A posted document (`posted`) names each item's difference,
  value and material document; the book quantity is then the count. Its
  material documents are not reversed (409 `inventory.reverse_count`): a
  new count corrects them. **Later:** a tolerance per item and per user
  group, difference reasons, partial posting of a document's items,
  recount history (MI11 linking the documents).

### S2e Count planning agent

**Capability.** A recommending agent (ADR-0072) proposes the materials to
count this period (cycle counting by ABC class, movement frequency and
last count) for an inventory controller to decide.

- **Adds:** an agent package, its type, suite and walkthrough step.
  **Acceptance:** it never creates a count document itself.

**Later for Physical inventory and cycle counting:** cycle counting
indicators on the plant data, sampling inventory, counts on handling units
or bins (EWM), freeze of the book quantity at a past date.

### S2f Batches

**Capability.** A material managed in batches carries a batch on every
movement and stock row; batches have a manufacture and expiry date.
**Adds:** `inventory.batch`, batch on movements and stock (stock key
extended, expand-migrate-contract). **Acceptance:** a batch-managed
material is refused without a batch; stock by batch adds up to the
storage location's.

### S2g Serial numbers

**Capability.** A material with a serial number profile records each
unit's serial number on receipt and issue. **Adds:** `inventory.serial`
and its history. **Acceptance:** a serial number is in stock at most once.

**Later for Batch and serial number management:** batch classification
and determination (FEFO), where-used lists, recall, serial numbers in
maintenance (phase 6).

### S2h Stock transport order within a company

**Capability.** A planner orders stock from one plant to another of the
same company; the issue puts it in transit and the receipt at the other
plant takes it out (SAP 351/101, from memory). **Adds:** the order type
in procurement, stock in transit. **Acceptance:** stock in transit is
neither plant's unrestricted stock and adds up across the two movements.

### S2i Intercompany stock transfer

**Capability.** The same between plants of two companies, with
intercompany billing and the receiving company's supplier invoice.
**Acceptance:** each company's ledger balances, the transfer price is the
billing's. **Later for Stock transfers and intercompany:** one-step
transfers between companies, transit stock valuation, cross-border
customs (S2s).

### S2j Available-to-promise on the sales order

**Capability.** Confirming a sales order item checks the material's
available quantity at the plant (stock less confirmed open orders and
deliveries) and confirms what is available on the requested date.
**Acceptance:** two orders racing for the last quantity confirm it once.
**Later for Available-to-promise:** checking rules with replenishment lead
time, back-order processing, product allocation, global ATP across
plants.

### S2k MRP net requirements

**Capability.** A planning run per plant computes net requirements
(demand from open sales orders and a reorder point against stock and open
receipts) and proposes purchase requisitions. **Acceptance:** a rerun
replaces its own open proposals and never a person's requisition.

### S2l Replenishment agent

**Capability.** A recommending agent reviews the run's proposals and
recommends what to order, from which supplier and when, for a buyer to
decide. **Later for MRP and replenishment:** lot sizing procedures,
safety stock calculation, MRP areas, planned orders for production (phase
5), scheduling with calendars.

### S2m Demand forecasting agent

**Capability.** A recommending agent forecasts a material's demand by
plant from its goods issues and open orders and states its accuracy
(the analytics context's forecast pattern, ADR-0044).

### S2n S&OP review

**Capability.** A planner reviews the forecast against supply and
capacity by material group and month and fixes a consensus plan version.
**Later for Demand forecasting and S&OP:** statistical models beyond the
agent's, promotions, collaborative forecasts with customers, IBP's
supply optimisation.

### S2o SAP migration of the material master (pilot)

**Capability.** The migration run loads materials from SAP's MARA, MAKT
(one language), MARM, MARC, MARD's assignments, T001W and T001L into
materials and tenancy, with the quality gate and reconciliation of phase
0. **Acceptance:** every material number is kept as an external
reference; a material whose base unit or conversion is not exact is held
back with its reason. Which MARA fields carry which meaning is the SAP
specialist's to confirm (from memory here).

- **Reuses:** the migration run (`internal/integration`: extract, quality
  gate, load in savepoints, external references, reconciliation, report),
  the file extract and the mock (`internal/sap`), materials' checks and
  numbering, tenancy's org units.
- **Adds:** the tables MARA, MAKT, MARM, MARC, MARD, T001W (with T001K's
  company code) and T001L in the extract, kept for the scope's plants;
  `materials.Migration` (create a material active with its create change
  applied, extend one to more plants, both without an approval request:
  the run is their approval, as for centers) and
  `tenancy.OrgUnitMigration` (an org unit created active, one found by
  code); quality findings (errors refusing the run, warnings holding a
  material back with its reasons); reconciliation checks `Plants`,
  `Storage locations` and `Materials` with differences by SAP key; report
  counts `plants`, `storage_locations`, `materials`, `materials_extended`
  and the seven tables' rows; the mock's company 3000 with two plants,
  three storage locations, three materials that load, two held back and
  one flagged for deletion; ADR-0082 As built S2o.
- **Walkthrough:** `migrated_materials` in step 3, `step_migrate`: the
  plants and storage locations as org units, the materials active with
  their numbers from Maple's range and their units, the create change
  naming SAP's number without an approval request, the two held back with
  their reasons, the checks passing.
- **Acceptance (as built):** the SAP number is the external reference
  `MARA/<MATNR>`, named in the create change's reason; a unit that is none
  of the platform's or a conversion that is not whole numbers from 1 to
  1,000,000 holds the material back with its reason while the rest loads;
  a later run extends a loaded material to the plants it adds and creates
  nothing twice; the reconciliation agrees per company code.
- **Depends on:** S1a, S1b.

**Later for the material master's migration:** the OData mapping (the
product and plant APIs; a tracker leaf); descriptions in further
languages; SAP's internal unit keys mapped onto the platform's (T006);
MARA's views beyond the basic data; plant-scoped storage location codes
(tenancy keeps them unique per company).

### S2p SAP migration of stock and values (pilot)

**Capability.** The cutover loads stock by storage location (MARD) and
valuation (MBEW) as initial entries (movement 561) valued at SAP's price,
posting against the migration clearing account, reconciled to SAP's
stock account balances. **Acceptance:** quantity and value per material
and plant equal SAP's at the cutover date.

### S2q WMS connector

**Capability.** An external warehouse system receives deliveries and
receipts to pick and put away and reports the movements back (the
integration context's connector pattern).

### S2r TMS connector

**Capability.** An external transport system receives outbound deliveries
to plan and returns the shipment and freight cost.

### S2s Trade compliance connector

**Capability.** An external trade compliance service screens deliveries'
parties and materials (sanctioned parties, export classification) and
blocks a delivery it refuses. **Later for the connectors:** an embedded
warehouse (SAP EWM), freight settlement, customs declarations.

## Slices, counted

Twenty-five slices: six in S1 (S1a–S1f, built in sequence) and nineteen
in S2 (S2a–S2s). Pilot-relevant under the assumption above: S1a–S1f,
S2a–S2d, S2o and S2p (twelve).

## Facts for the specialist (from memory, to be confirmed)

- SAP's material tables and fields named here (MARA, MAKT, MARM with
  UMREZ/UMREN, MARC, MARD, MBEW with VPRSV, VERPR, STPRS, PEINH and BKLAS,
  T001W, T001L, T134, T023) and the movement types (101, 102, 201, 202,
  261, 301, 302, 311, 312, 351, 501, 502, 551, 552, 561, 562, 601, 602,
  701, 702) are stated from memory, without a primary source in the
  repository; so are the delivery tables LIKP and LIPS, the picking
  status KOSTK (A, B, C), the account modifier VAX of GBB for a goods
  issue's cost of goods sold, and SAP changing the delivery quantity to
  the picked one before goods issue, which ADR-0083 does not follow.
- SAP's account determination for inventory (transaction keys BSX, WRX,
  PRD, GBB with its account modifiers, UMB) is from memory; ADR-0082 §8
  maps it onto this ERP's account determination keys and is confirmed by
  the SAP specialist before S1d.
