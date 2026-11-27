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
- **`mrp`** (schema `mrp`, `internal/mrp`, from S2k; ADR-0084): materials'
  planning data at plants and planning runs. A leaf: it reads sales'
  demand, inventory's stock and prices and procurement's receipts and
  requisitions through ports and proposes requisitions through
  procurement's published API.
- The rest of planning (S2l–S2n) and the connectors (S2q–S2s) are decided
  by their own ADRs when they are built; the WMS connector (S2q) needed
  none: it is the integration context's connector pattern, recorded in
  ADR-0082 As built S2q, and adds no import between contexts.

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

#### S1a2 Descriptions by language

The first item of the leaf "later: material master variants" (lane
matdesc, 2026-11-24); what is left of it stays a leaf (Later, below).

**Capability.** A master data specialist describes a material in
further languages beside its default description (SAP MAKT, one text
per language, from memory), Arabic for the UAE pilot; a person, an
integration or an agent reading a material asks for a language and gets
its description in that language, or the default one when it has none.

- **Reuses:** S1a's material and its `description`, S1b's material
  changes (a change of the descriptions is governed as any other field),
  S2o's SAP load of MAKT, `golang.org/x/text/language` (as the web
  workspace's locales, `internal/web/i18n`).
- **Adds:** `materials.material.description_language` (the default
  description's language, `en` for every material created before) and
  `materials.material_description` (a further description per language,
  removed by its mark as a unit is); `description_language` and
  `descriptions` on `MaterialCreate`, `descriptions` on
  `MaterialUpdate` and `MaterialChangeRequested`; `default_language`,
  `description_language` and `descriptions` on `Material`; the query
  parameter `language` of `getMaterial` and `listMaterials`; the
  problem codes `materials.unknown_language` and
  `materials.duplicate_language`; MaterialChanged's
  `description_language` and `descriptions`; ADR-0082 As built S1a2.
- **Decided by default (ADR-0082 As built S1a2):** a language is a
  BCP 47 tag of a language with an optional script and region in their
  canonical form (`ar`, `ar-AE`, `zh-Hant`), its subtags known to the
  IANA registry as `golang.org/x/text/language` holds it (a partner's
  ISO 639-1 `language` is such a tag); a read names its language with
  the explicit parameter `language`, since the API has no
  `Accept-Language` convention (only the web workspace reads that
  header), and finds it by RFC 4647 lookup (`ar-AE`, then `ar`), else
  the default; `description` answers that description, so a read that
  names no language is as before; the default's language is
  `description_language` at creation (`en` when absent) and does not
  change in this slice; further descriptions are replaced as a whole by
  a change, each language once and none in the default's; the duplicate
  check stays on the default description; no contract step is needed,
  as `description` stays the default's text.
- **SAP:** the load maps MAKT-SPRAS to a tag (a table of SAP's
  one-character keys and the two-letter ISO codes, from memory); the
  text it picks today (English, else the first other) stays the default
  in its language, and the material's other MAKT texts become its
  further descriptions; a text in a key the table has no tag for is a
  warning and is left out (a default in such a key is `und`).
- **Walkthrough:** `step_material`: the bracket is created with an
  Arabic description, read in Arabic, in `ar-AE` and in French (the
  default English answered); the zinc-plated change carries the new
  Arabic text and a French one, approved with the rest; a further
  description in the default's language is refused.
- **Acceptance:** a description is stored and answered as given,
  Arabic included, no case or white space folded; a read in a language
  the material has answers it, any other the default, naming the
  language answered; a change of the descriptions waits for its
  approval as the material's other fields do and is audited with the
  texts before and after; materials created before keep their
  description as their default, in `en`.
- **As built** (2026-11-24): ADR-0082 As built S1a2, migrations 000484
  and 000485, walkthrough step 122.

#### S1a3 Dimensions, weights and GTINs

The second item of the leaf "later: material master variants" (lane
matdims, 2026-11-26); what is left of it stays a leaf (Later, below).

**Capability.** A master data specialist records, for each unit of
measure a material is kept in, its base unit and each alternative unit,
its gross and net weight, volume, length, width and height and its GTIN
(SAP MARM per unit, MARA's copy for the base unit, from memory); a person,
an integration or a scanner reading materials finds a material by a
unit's GTIN.

- **Reuses:** S1a's material and its units (`unit_conversion`), S1b's
  material changes (the measures are governed as any other field),
  `ref.unit_of_measure` and its dimensions, S2o's SAP load of MARA and
  MARM.
- **Adds:** `materials.unit_measure` (one row per unit with measures,
  removed by its mark as a unit is) and `materials.gtin_valid`;
  `internal/platform/gtin`; `base_unit_measures` on `MaterialCreate`,
  `MaterialUpdate`, `Material` and `MaterialChangeRequested`, `measures`
  on `MaterialUnitInput` and `MaterialUnit`; the query parameter `gtin`
  of `listMaterials`; the problem codes `materials.invalid_measures`,
  `materials.invalid_gtin`, `materials.duplicate_gtin` and
  `materials.gtin_taken`; MaterialChanged's `base_unit_measures` and
  `alternative_units[].measures`; ADR-0082 As built S1a3.
- **Decided by default (ADR-0082 As built S1a3):** measures per unit,
  base and alternative, as MARM keeps them, a net weight on any unit;
  decimal strings above zero of up to ten digits and three decimals in
  one unit of mass, one of volume and one of length, a unit given with a
  value and only then, the net no more than the gross; no new units of
  measure; a GTIN of GS1's four lengths with its check digit, compared in
  14 digits, naming one unit of one material in the tenant whatever its
  status, freed when a create change is rejected or withdrawn; a GTIN
  checked when a change is requested and again when it is applied.
- **SAP:** MARA's BRGEW, NTGEW, GEWEI, VOLUM, VOLEH and EAN11 and the
  base unit's MARM dimensions; each alternative unit's MARM BRGEW, GEWEI,
  VOLUM, VOLEH, LAENG, BREIT, HOEHE, MEABM and EAN11 (from memory); what
  the platform cannot hold is left out with a warning, a GTIN two units
  share is left out of both.
- **Walkthrough:** `step_material`: the bracket's and the box's GTINs,
  weights, volume and size are given by a change Priya approves; a list
  finds the bracket by its GTIN written in 13 digits; the box's GTIN on a
  new material, a net weight above the gross and a wrong check digit are
  refused.
- **Acceptance:** each value is kept and answered as given, never as a
  float, without trailing zeros; a unit of the wrong kind, a value without
  its unit and a net weight above the gross are refused naming the field;
  a GTIN is refused without its check digit, on two units of a request or
  on another material's unit, a GTIN-12 and its GTIN-13 being one; a
  change of the measures waits for its approval and is audited before and
  after; an alternative unit removed takes its measures with it.
- **As built** (2026-11-26): ADR-0082 As built S1a3, migration 000495,
  walkthrough step 122.

**Later for Material master:** changing a material's default language,
documents and outputs taking the material's description in their
partner's language, the duplicate check across languages, a later SAP
run's new or changed descriptions of a material an earlier run created
(the rest of descriptions by language, S1a2); the material's sales
organisation and purchasing views (MVKE, EINA/EINE belong to S1f and
sourcing); of dimensions, weights and GTINs (S1a3): a delivery's gross
weight and volume from its items' units, GTINs on documents and e-invoice
lines and items found by GTIN, GTIN types beyond GS1's four lengths and
several GTINs per unit (SAP's NUMTP, MEAN), a later SAP run's new or
changed measures of a material an earlier run created;
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

#### S2b3 Goods issue reversal of a delivery not billed (602)

The first item of the leaf "later: goods issue variants" (lane
gireverse, 2026-11-24); what is left of it stays a leaf: partial goods
issue, picking by wave or bin.

**Capability.** A warehouse clerk reverses the goods issue of an
outbound delivery whose quantities are not billed (SAP VL09, from
memory): its stock comes back to the storage location at the value the
goods issue took, the cost of goods sold is taken back, and the sales
order's items are no longer delivered by it, open again for another
delivery.

- **Adds:** `POST /v1/outbound-deliveries/{id}:reverseGoodsIssue`
  (`If-Match`, human-only); inventory's
  `Endpoints.ReverseForDelivery`, which posts the 601's reversal, a 602
  naming it, through the reversal inventory already has (its lines
  negated, each at what its 601 line took); sales'
  `Endpoints.ReverseIssue`, which records the reversal of the goods
  issue's sales delivery (`sales.delivery_reversal`) under the order's
  row lock and the availability check's locks of its materials at the
  plant (`database.LockTenant` `sales.availability`, the leaf
  src review:batch86), so a check never counts the stock given back
  against the demand before it; the delivery's status
  `goods_issue_reversed`, naming the 602; the event
  SalesOrderDeliveryReversed.
- **Decided by default (ADR-0083 As built S2b3):** the delivery ends
  reversed and changes no more, its quantities open again on the order
  for a new delivery (SAP takes the delivery back to before its goods
  issue, from memory; this version has no deletion of a delivery, so a
  delivery taken back could hold its quantities for good); the reversal
  posts on the company's date today; a delivery is billed when reversing
  it would leave an order item billed beyond what stays delivered (409
  `sales.delivery_billed`: the billing document is cancelled first);
  the reversal needs the goods issue's permission; trade compliance is
  not asked again. The reversal counts in the period of its date:
  cost of sales by profitability segment falls by its cost there, and a
  point-in-time revenue obligation scheduled by deliveries by its share.
- **Walkthrough:** `step_goods_issue`: step 143's billed delivery is
  refused; a further delivery of one bracket is issued and reversed: the
  stock and its value come back, the cost of goods sold is credited, the
  order's item is open again and the order is cancelled.
- **Acceptance:** the reversal posts once, at the issue's value; the
  stock, the valuation and the cost of goods sold are as before the
  issue; a billed delivery is refused; the order item's delivered
  quantity falls by what was issued.
- **As built** (2026-11-24): ADR-0083 As built S2b3, migrations 000471
  and 000472, walkthrough step 143 (`goods_issue_reversal`).

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
  recount history (first slice S2d2: recounts on the same document; MI11
  linking the documents stays Later).

### S2d2 Recounts with history (difference posting variants, first slice)

**Capability.** An inventory controller sends counted items of a count
document whose differences are not posted for a recount; the earlier
counts are kept with who counted and when, the items are counted again,
and only the latest count is posted.

- **Adds** (lane recount, 2026-11-25; the tracker's "later: difference
  posting variants" split, its first item; decided by default, ADR-0082
  As built S2d2): `POST /v1/physical-inventory-documents/{id}:recount`
  (SAP MI11, from memory; `If-Match`, permission `inventory.count.write`,
  human-only) on a `counting` or `counted` document, by item number: each
  counted item's count is kept in `inventory.count_item_recount`
  (insert-only, numbered from 1, with who counted it and when and who
  sent it for the recount and when), the item open to be counted again
  through `:enterCount`, the document `counting` until every item is
  counted. 409 `inventory.item_not_counted` for an item not counted, 409
  `inventory.recount_limit` past 99 counts, 409
  `inventory.count_invalid_status` once the differences are posted, wait
  for approval, or were rejected or withdrawn. Each item reads
  `count_no` and `earlier_counts`, in count order. Audited
  (`inventory.count_document.recount`) and published as
  PhysicalInventoryRecountRequested. On the count planning and demand
  forecasting agents' never lists.
- **Walkthrough:** extends `step_count`. **Acceptance:** an earlier
  count is kept and shown in order; only the latest count is posted; a
  posted item is not recounted (409).
- **Later:** a recount by a new document linked to the first (SAP MI11's),
  a recount while differences wait for approval, difference reasons,
  approval tiers by value.
- **Integration** (batch 96S, migration 000490): the names kept with
  counts and recounts are in the personal data inventory (ADR-0082 As
  built S2d2).

### S2e Count planning agent

**Capability.** A recommending agent (ADR-0072) proposes the materials to
count this period (cycle counting by ABC class, movement frequency and
last count) for an inventory controller to decide.

- **Adds:** an agent package, its type, suite and walkthrough step.
  **Acceptance:** it never creates a count document itself.
- **Detailed** (lane cntag, 2026-11-21; one slice, one leaf, nothing to
  split; decided by default, ADR-0082 As built S2e and ADR-0072 As built
  S2e): `erp agent count-plan -company CODE -plant CODE`, agent type
  `count_planning` (`internal/countagent`). Before its session the run
  reads the plant's stock, materials, the year's material documents and
  its count documents through inventory's and materials' published reads
  and computes what is due: an ABC class by the year's consumption value
  at the plant (A the first 80 percent, B the next 15, C the rest), the
  material's movements at the storage location and its last posted
  count there; due when never counted there or not within its class's
  interval (90, 180, 365 days); batch- and serial-managed materials and
  those an open count holds left out. The due ones, ranked by class,
  movements and last count, each with its line, are offered to the
  model, which proposes those it recommends, in that order, with its
  reasons, in one `count_proposal` on the plant; nothing due, no
  session. Never: every write of count documents and goods movements.
  Suite `evals/count_planning`; walkthrough `step_count_agent`.

### S2c2 Cycle counting indicators (count variants, first slice)

**Capability.** An inventory controller gives a plant its cycle counting
indicators (A, B, C, D or the tenant's own, each with a count interval in
days) and a material at the plant its indicator, reads the counts due and
creates count documents from them.

- **Adds** (lane cyclecount, 2026-11-24; the tracker's "later: count
  variants" split, its first item; decided by default, ADR-0082 As built
  S2c2): `inventory.cycle_count_indicator` (SAP T159C, a plant's
  indicators, from memory): a code of one to four capital letters or
  digits, unique at the plant, and an interval of 1 to 366 days (a
  material is counted at least once a year), changed at its version;
  `inventory.cycle_count_material` (SAP MARC-ABCIN, from memory): a
  material's indicator at a plant, one per material and plant, of that
  plant, changed to another of the plant at its version. The material is
  one a count document takes: the tenant's, not pending, kept in stock,
  extended to the plant, neither batch- nor serial-managed (counts refuse
  those, As built S2f, S2g). No indicator is created for a plant by
  itself: the controller creates the plant's. `POST`, `GET`, `GET /{id}`,
  `PATCH` of `/v1/cycle-count-indicators` and `/v1/cycle-count-materials`
  (permission `inventory.count_plan.write`; reads under
  `inventory.stock.read`), audited, no event; human-only.
- **The counts due** (`GET /v1/cycle-counts-due?plant_id=`, SAP MICN,
  from memory): each material with an indicator at the plant, at each
  storage location of the plant where it has stock now (as `listStock`
  lists it and the agent reads it), that no open count holds there and that was never counted there
  (a posted count; a rejected or withdrawn one is none) or whose last
  posted count there is its indicator's interval or more before the
  company's date today. Each names its material, storage location,
  indicator and interval, its last count (document, number, date) and
  the day it fell due; never counted first, then the longest due.
  Creating a count is the normal `POST /v1/physical-inventory-documents`
  with a storage location's materials as listed; they then leave the list
  until the count is posted, and fall due again an interval after it.
- **The count planning agent** (S2e) reads the indicators
  (`listCycleCountMaterials`): a material with one at the plant is due by
  its interval instead of its class's, and its line names it; the others
  keep the class computed per run.
- **Walkthrough:** extends `step_count`. **Acceptance:** a material
  counted within its interval is not due; one never counted is; a count
  created from the list takes it off the list.
- **Later:** an indicator taken off a material, indicators set from the
  ABC analysis (SAP MIBC), float time and a share of counts per period,
  counts due per storage location's own indicator, a calendar of planned
  counts.

### S2c3 Indicators taken off and set from the ABC analysis (count variants)

**Capability.** An inventory controller takes a material's cycle
counting indicator at a plant off, and sets the plant's materials'
indicators from an ABC analysis of their consumption value: a preview
first, then an apply of what the preview showed.

- **Adds** (lane countoff, 2026-11-27; the tracker's "later: count
  variants" split, its next two items; decided by default, ADR-0082 As
  built S2c3): migration 000496, `status` on
  `inventory.cycle_count_material` (`active`, `removed`) with one active
  indicator per material and plant; `POST
  /v1/cycle-count-materials/{id}:remove` (If-Match, once, audited);
  `status` filter on `listCycleCountMaterials`; `GET
  /v1/cycle-count-abc-analysis` (plant and window, computed in SQL from
  posted material documents, S2e's consumption, classes by the company's
  `cycle_count_abc_shares`, default 80 and 15, each line proposing the
  plant's indicator of its class's code) and `POST
  /v1/cycle-count-abc-applications` (the plant and window with the
  preview's `preview_token`: computed again under the plant's lock and
  applied only when unchanged, 409 `inventory.abc_preview_stale`
  otherwise; each change audited as the indicator's create or change;
  the application kept with its changes, read and listed by plant);
  both human-only, on the count planning and demand forecasting agents'
  never lists; the count planning agent reads active indicators only.
- **Walkthrough:** extends `step_count`. **Acceptance:** an indicator
  taken off is kept as removed, not due by it, taken off once; the
  analysis's lines and totals add up; applying it makes the preview's
  changes and a second apply of it is refused as stale.
- **Later:** float time and a share of counts per period, counts due per
  storage location's own indicator, a calendar of planned counts, the
  count planning agent's classes from the company's shares.

**Later for Physical inventory and cycle counting:** cycle counting
indicators on the plant data (first slice S2c2), sampling inventory, counts on handling units
or bins (EWM), freeze of the book quantity at a past date.

### S2f Batches

**Capability.** A material managed in batches carries a batch on every
movement and stock row; batches have a manufacture and expiry date.
**Adds:** `inventory.batch`, batch on movements and stock (stock key
extended, expand-migrate-contract). **Acceptance:** a batch-managed
material is refused without a batch; stock by batch adds up to the
storage location's.

- **Detailed** (lane batch, 2026-11-20; one slice, one leaf, nothing to
  split; decided by default, ADR-0082 As built S2f): a material is
  batch-managed from its creation (`batch_managed` on `POST
  /v1/materials`, SAP MARA's XCHPF, from memory; fixed once created).
  `POST /v1/batches` (`inventory.batch.write`) creates a batch of it: a
  number given by the request, unique per material (SAP's batch level
  material, from memory), a manufacture date and an optional expiry date
  not before it; `GET /v1/batches` and `/{id}`. Every goods movement of a
  batch-managed material names a batch of it (`batch_id` on the items of
  `POST /v1/material-documents`; a transfer keeps it, a reversal takes
  its original's), and another material's none. Stock by batch is
  `inventory.batch_stock` (SAP MCHB) beside the location's stock row
  rather than a wider stock key, so the location's stock and its readers
  stay as they are; the database adds a batch's movement to both and
  refuses one that would leave them apart, so the batches add up to the
  location's stock (`GET /v1/batch-stock`). The flows that carry no batch
  yet refuse a batch-managed material at their first step: a purchase
  order's stock item, a quotation's or sales order's item, a stock transport
  order, a count and the SAP stock take-over.
  **Later:** batches in those flows, a batch created at goods receipt,
  internal batch numbers, changing a batch's dates, batch status, stock
  by batch as at a date, transfer to another batch (309).
- **S2f1 Batches in the purchase order's goods receipt** (lane grbatch,
  2026-11-24; split from the batch variants, decided by default, ADR-0082
  As built S2f1). **Adds:** a purchase order's stock item of a
  batch-managed material (refused at ordering until now); `batch` on the
  items of `POST /v1/goods-receipts` (its number, and for a new batch its
  manufacture date and optional expiry date), the batch created by the
  receipt when the material has none of that number (SAP MIGO creating a
  batch at goods receipt, from memory), the goods receipt's items naming
  the batch by id and number, its reversal (102) taking the batch's stock
  back. **Acceptance:** a batch-managed stock item's receipt without a
  batch is refused; a receipt naming a new batch creates it with its
  dates, one naming an existing batch adds to its stock, and stock by
  batch still adds up to the storage location's. **Later:** the batch
  variants left (the delivery's goods issue, stock transport orders,
  counts, the SAP take-over, a WMS-reported receipt naming a batch,
  internal batch numbers, changing a batch's dates, batch status, stock
  by batch as at a date, 309, FEFO, classification, where-used, recall).
- **S2f2 Batches in the outbound delivery's goods issue** (lane gibatch,
  2026-11-25; split from the batch variants, decided by default, ADR-0083
  As built S2f2). **Adds:** an order item and a delivery of a
  batch-managed material (refused until now); an optional body on
  `POST /v1/outbound-deliveries/{id}:postGoodsIssue` naming, per item,
  the batches its picked quantity leaves from by number with the
  quantity of each (a batch split, SAP's batch split items, from
  memory), each a line of the 601 checked against the batch's stock at
  the storage location; the delivery's items naming what each batch
  issued (`issue_lines`); its reversal (602) giving each batch back what
  it issued. The person names the batch: batch determination is Later.
  **Acceptance:** a picked item of a batch-managed material whose goods
  issue names no batch is refused; the batches named add up to the
  picked quantity; a batch short of the quantity is refused; the 602
  gives the batch's stock back, so stock by batch still adds up to the
  storage location's. **Later:** the batch variants left (stock
  transport orders, an intercompany one's 643 included, counts, the SAP
  take-over, a WMS-reported goods receipt or goods issue naming a batch,
  batches named at picking, internal batch numbers, changing a batch's
  dates, batch status, an expired batch's check at issue, stock by batch
  as at a date, 309, FEFO, classification, where-used, recall).
  **Integration** (batch 96S, migration 000491): an item confirmed in part
  on the lead time (S2j2) is issued by batch and reversed under the
  availability lock (ADR-0083 As built S2f2).

### S2g Serial numbers

**Capability.** A material with a serial number profile records each
unit's serial number on receipt and issue. **Adds:** `inventory.serial`
and its history. **Acceptance:** a serial number is in stock at most once.

- **Detailed** (lane serial, 2026-11-21; one slice, one leaf, nothing to
  split; decided by default, ADR-0082 As built S2g): a material has a
  serial number profile from its creation (`serial_managed` on `POST
  /v1/materials`, fixed once created; SAP MARC's SERNP, from memory).
  Every goods movement of it (`POST /v1/material-documents`) names the
  serial number of each unit it moves (`serial_numbers`, a whole number
  of base units), unique per material; the first receipt naming one
  creates it. `inventory.serial` holds where each is in stock now and
  `inventory.movement_serial` its history; the database refuses a
  receipt of one in stock and an issue of one not at the line's location,
  so a serial number is in stock at most once. `GET /v1/serial-numbers`
  and `/{id}` with its history. The flows that carry no serial numbers
  yet refuse a serial-managed material where they refuse a batch-managed
  one (S2f). **Later:** serial numbers in those flows, profiles per plant
  and other serial number usages.
- **S2g1 Serial numbers in the purchase order's goods receipt** (lane
  grbatch, 2026-11-24; split from the serial number variants, decided by
  default, ADR-0082 As built S2f1). **Adds:** a purchase order's stock
  item of a serial-managed material; `serial_numbers` on the items of
  `POST /v1/goods-receipts`, one per unit of the base unit, each created
  by the receipt as a 501's are; the goods receipt's items naming them;
  its reversal (102) taking them out of stock again. **Acceptance:** a
  serial-managed stock item's receipt names one serial number per unit,
  none in stock already; its reversal leaves them in no stock. **Later:**
  the serial number variants left (the delivery's goods issue, stock
  transport orders, counts, the SAP take-over, a WMS-reported receipt
  naming serial numbers, profiles per plant, other usages, maintenance
  and service, serialization at delivery only).
- **S2g2 Serial numbers in the outbound delivery's goods issue** (lane
  gibatch, 2026-11-25; split from the serial number variants, decided by
  default, ADR-0083 As built S2f2). **Adds:** an order item and a
  delivery of a serial-managed material; `serial_numbers` on the goods
  issue's items, one per unit of the base unit, each in stock at the
  delivery's storage location, out of stock after the 601; the
  delivery's items naming them; the reversal (602) taking them into
  stock there again. **Acceptance:** a picked item of a serial-managed
  material whose goods issue names no serial numbers, or one not in
  stock there, is refused; the 602 refuses a serial number in stock
  again since (409 `inventory.serial_in_stock`). **Later:** the serial
  number variants left (stock transport orders, an intercompany one's
  643 included, counts, the SAP take-over, a WMS-reported goods receipt
  or goods issue naming serial numbers, serial numbers named at
  picking, profiles per plant, other usages, maintenance and service,
  serialization at delivery only).

**Later for Batch and serial number management:** batch classification
and determination (FEFO), where-used lists, recall, serial numbers in
maintenance (phase 6).

### S2h Stock transport order within a company

**Capability.** A planner orders stock from one plant to another of the
same company; the issue puts it in transit and the receipt at the other
plant takes it out (SAP 351/101, from memory). **Adds:** the order type
in procurement, stock in transit. **Acceptance:** stock in transit is
neither plant's unrestricted stock and adds up across the two movements.

- **Detailed** (lane sto, 2026-11-20; one slice, one leaf, nothing to
  split; decided by default, ADR-0082 As built S2h): `POST
  /v1/stock-transport-orders` (permission `procurement.stock_transport.write`)
  creates an order from a supplying plant to another plant of the same
  company, each item a material from a storage location of the one to a
  storage location of the other, checked by inventory as its goods issue
  will be, numbered from the company's `stock_transport_order` range;
  `GET` (company, plant, status) and `/{id}` under
  `procurement.stock_transport.read`. `:postGoodsIssue` (`If-Match`,
  `procurement.stock_transport.post`) posts one material document of
  `351`: each item leaves its supplying location, valued as an issue is,
  and is in transit to its receiving location at the receiving plant
  (`inventory.transit_stock`), valued in the receiving plant at what it
  took (its standard price there, the rest a price difference), posted as
  `WA` between the two plants' stock accounts (SAP's 351 within a company,
  from memory). `:postGoodsReceipt` posts one `101` taking it out of
  transit into the receiving location, within the receiving plant's
  valuation, so nothing is posted. The order moves whole: `open`,
  `in_transit`, `received`; an open order may be cancelled instead
  (`cancelled`, review:batch92, ADR-0082 As built S2h). Stock in transit is no location's stock
  (`GET /v1/stock`) but the receiving plant's valuation; the material
  documents' lines say which line is in transit (`in_transit`).
  **Later:** partial issues and receipts, reversals of either (352, 102;
  409 `inventory.reverse_stock_transport` until then), a delivery between the plants (SAP's replenishment delivery,
  shipping), one-step transfers (SAP's 301 is S1c's), the stock in
  transit as a list of its own, stock in transit valued at the supplying
  plant until receipt.

### S2i Intercompany stock transfer

**Capability.** The same between plants of two companies, with
intercompany billing and the receiving company's supplier invoice.
**Acceptance:** each company's ledger balances, the transfer price is the
billing's. **Later for Stock transfers and intercompany:** one-step
transfers between companies, transit stock valuation, cross-border
customs (S2s).

- **Detailed** (lane icsto, 2026-11-21; decided by default from SAP's
  cross-company stock transport order, from memory; ADR-0082 As built
  S2i): three slices, three leaves.
  - **The order is the receiving company's purchase order** naming the
    plant of the other company that supplies it (`supplying_plant_id`;
    SAP's purchasing document with a supplying plant, EKKO-RESWK, document
    type NB between companies). Unlike S2h's order within a company it
    has a supplier, prices, a release and GR/IR, so it is not S2h's
    table.
  - **Partners.** The receiving company's supplier is the business
    partner that is the supplying company (its group company, ADR-0059
    §4; SAP assigns the vendor to the supplying plant, LFA1-WERKS). The
    supplying company's customer for its billing (S2i2) is the business
    partner that is the receiving company, a customer in the supplying
    company (SAP: the receiving plant's internal customer in the stock
    transport shipping data).
  - **Transfer price.** The order item's price, in the order's currency,
    as on any purchase order (entered or from a contract); the billing
    (S2i2) bills that price, so the transfer price is the billing's and
    the receiving company's invoice (S2i3) matches its receipt. SAP
    prices the billing by the supplying side's own conditions (PI01,
    billing type IV), which may differ: a pricing of its own is Later.
  - **Receiving valuation.** The goods receipt is the order's 101 at the
    order price against GR/IR (S1e): a moving price takes it, a standard
    price puts the difference on price differences.
  - **S2i1 Intercompany stock transport order** (this lane): the order is
    created with its supplying plant (stock items only, no over-delivery
    tolerance) and released as any order; `:postIntercompanyGoodsIssue`
    (`procurement.stock_transport.post`, a person who acts in both
    companies) posts the whole order's goods issue once as a `643` of the
    supplying company, out of a storage location of the supplying plant
    per item, valued as an issue and posted WA Dr cost of goods sold, Cr
    stock (SAP's 643 posts the supplying company's cost of goods sold,
    from memory); the receiving company's goods receipt takes it in, at
    most what was issued and only after the issue. In between the goods
    are in neither company's stock or valuation. Cancelling or closing an
    order in transit is refused; a released one is not changed.
    **Acceptance:** each company's entry balances, the supplying
    company's at its stock's value, the receiving company's at the order
    price. Walkthrough step `step_intercompany_transport`.
  - **S2i2 Intercompany billing** (built, lane icbill, 2026-11-22; ADR-0082
    As built S2i2): the supplying company bills the posted goods issue to
    the receiving company's customer at the order's prices (a document of
    its own, `sales.intercompany_billing`, not a sales billing document),
    its tax by the supplying company's tax code, posted at once as a
    customer invoice (receivable and revenue); once per goods issue.
    `POST /v1/intercompany-billings`; walkthrough step 155.
  - **S2i3 The receiving company's supplier invoice from the billing**
    (built, lane ics3, 2026-11-22; ADR-0082 As built S2i3): the posted
    intercompany billing is entered as a supplier invoice of the receiving
    company for the supplier that is the supplying company, at the
    billing's amounts and tax, its items the order's at the billing's
    nets, its codes proposed from the billed rates, parked or held for a
    person to match against the order's receipts and post (clearing
    GR/IR, the matching of S1e); once per billing.
    `POST /v1/intercompany-billings/{id}/supplier-invoices`; walkthrough
    step 155.
  - **Later:** partial and repeated goods issues, a delivery between the
    companies (SAP's NLCC delivery), the 643's reversal (644; 409
    `inventory.reverse_stock_transport` until then), changing a released
    intercompany order, the supplying side's own pricing, one-step
    transfers (645), transit stock valuation and its list, customs (S2s).

### S2j Available-to-promise on the sales order

**Capability.** Confirming a sales order item checks the material's
available quantity at the plant (stock less confirmed open orders and
deliveries) and confirms what is available on the requested date.
**Acceptance:** two orders racing for the last quantity confirm it once.
**Later for Available-to-promise:** checking rules with replenishment lead
time (first slice S2j2), back-order processing, product allocation,
global ATP across plants.

- **Detailed** (lane atp, 2026-11-20; ADR-0082 As built S2j, decided by
  default): the order names its plant (`plant_id`, optional, kept);
  confirming checks each item of a material kept in stock against the
  plant's stock (inventory's `PlantStock`) less the other confirmed
  orders' confirmed and undelivered quantities, under a lock per
  material and plant, and confirms in full, in part or nothing
  (`confirmed_quantity`); deliveries take at most that, from the order's
  plant; a change rechecks the items it adds or changes. Per-item plants
  join the Later list.
- **As built** (2026-11-20): ADR-0082 As built S2j, migration 000430,
  walkthrough step 150.

#### S2j2 Checking with the replenishment lead time (ATP variants, first slice)

**Capability.** A planner gives a material at a plant its replenishment
lead time in days; a sales clerk's confirmation of an order asked for on
or after the company's date today plus that lead time confirms the item
in full, the stock first and the rest on the lead time, and the order
says which part rests on it.

- **Adds** (lane atplead, 2026-11-25; the tracker's "later: ATP
  variants" split, its first item; decided by default, ADR-0082 As built
  S2j2): `replenishment_lead_time_days` (1 to 999 calendar days,
  optional) on a material's planning data at the plant
  (`mrp.material_plan`, SAP MARC-WZEIT, from memory), set and removed by
  `createMaterialPlan` and `updateMaterialPlan`, whatever the MRP type;
  sales reads it through a port the composition root wires (`mrp.Plans`).
  An order item's `lead_time_quantity` and `lead_time_date`
  (`sales.sales_order_item`): the part of `confirmed_quantity` the stock
  did not cover at the check, and the day the lead time ends.
- **The check** (`confirmSalesOrder`, and `changeSalesOrder` for the
  items it rechecks): an item of a material with a lead time at the
  order's plant, of an order asked for on or after today plus the lead
  time (the company's day), is confirmed in full: what the stock covers,
  as before, and the rest on the lead time. Sooner, or with no requested
  date, or without a lead time: the stock alone, as S2j. What rests on
  the lead time counts as promised for later checks, under the same lock
  per material and plant (`database.LockTenant` `sales.availability`,
  which the goods issue reversal takes too). Audited per item with the
  lead time it was judged on.
- **Walkthrough:** extends `step_available_to_promise`. **Acceptance:**
  an order asked for beyond the lead time is confirmed in full with the
  part beyond the stock named; one asked for sooner gets only the stock;
  a material without a lead time behaves as before.
- **Later:** planned receipts (open purchase orders) in the check, a
  confirmation on a later date than asked for (schedule lines at the
  lead time's end), a recheck when only the requested date changes, the
  lead time from the planned delivery and goods receipt processing times
  (SAP PLIFZ and WEBAZ, from memory), working-day calendars.
- **As built** (2026-11-25): ADR-0082 As built S2j2, ADR-0084 As built
  (replenishment lead time), migrations 000492 and 000493, walkthrough
  step 150. Integrated with S2k2's lot sizing (batch 96S): a plan carries
  both, a merge patch naming one leaves the other, and a run sizes by the
  lot sizing alone.

### S2k MRP net requirements

**Capability.** A planning run per plant computes net requirements
(demand from open sales orders and a reorder point against stock and open
receipts) and proposes purchase requisitions. **Acceptance:** a rerun
replaces its own open proposals and never a person's requisition.

- **Detailed and as built** (lane mrp, 2026-11-21; ADR-0084, decided by
  default): a planner gives a material its planning data at a plant
  (`POST /v1/material-plans`: MRP type `reorder_point` or `no_planning`,
  a reorder point in the base unit, the account its proposals name); a
  run of a plant (`POST /v1/mrp-runs`, an Operation, one transaction)
  counts per planned material the plant's confirmed orders' ordered and
  undelivered quantities, its stock and its open receipts (purchase
  orders' stock items and the proposals a person took over) and proposes
  the shortage below the reorder point as a draft requisition at the
  valuation price; a rerun cancels the earlier runs' proposals still
  unchanged drafts and never touches one a person changed or raised.
  Migration 000436, walkthrough step 152 (`step_mrp`).
- **Intercompany demand** (lane mrpic, batch 91, review:batch90; ADR-0084
  As built): the run's demand also counts the intercompany orders (S2i)
  the plant supplies and has not issued, through procurement's
  `IntercompanyDue`; a stock transport order within the company was still
  counted on neither side (until batch 92, below). No migration;
  test-only acceptance (`TestPlanningRunIntercompany`): step_mrp runs
  before any intercompany order exists in the walkthrough.
- **Stock transport orders** (lane mrpsto, batch 92, review:batch91;
  ADR-0084 As built): a stock transport order within the company is the
  supplying plant's demand until issued (procurement's `TransportsDue`)
  and the receiving plant's receipt until received, open or in transit
  (`TransportsOnOrder`), as SAP's release order and receipt are; with
  sales and intercompany orders, each order counted once on each side
  (`TestPlanningRunTransports`). No migration; walkthrough step 152
  (`step_mrp`): an open order of G20 gaskets from Toronto to Montreal.

### S2k2 Lot sizing procedures (MRP variants, first slice)

**Capability.** A planner gives a material's planning data at a plant a
lot sizing procedure, so a run proposes what the plant orders in: the
shortage itself (lot for lot, as before), fixed lots, or up to a maximum
stock level, within a minimum and a maximum lot size and rounded to a
pack size.

- **Adds** (lane lotsize, 2026-11-24; the tracker's "later: MRP variants"
  split, its first item; decided by default, ADR-0084 As built S2k2):
  `lot_size` on `mrp.material_plan` (SAP MARC-DISLS, from memory):
  `lot_for_lot` (EX, the default), `fixed` (FX, with `fixed_lot_size`)
  or `replenish_to_maximum` (HB, with `maximum_stock_level`, not below
  the reorder point); `minimum_lot_size`, `maximum_lot_size` and
  `rounding_value` (BSTMI, BSTMA, BSTRF) with the other two, the bounds
  multiples of the rounding value; all in the base unit. A run sizes each
  shortage by it, an item per lot of the material's one requisition, at
  most 100 lots (409 `mrp.too_many_lots` fails the run); its line names
  the procedure, what it proposed and in how many lots. Migration 000486;
  `createMaterialPlan`, `updateMaterialPlan` (merge patch, `null` removes
  a quantity), `MrpRunLine` additive.
- **Walkthrough:** extends `step_mrp` (step 152): a washer planned at
  Montreal in fixed lots of 40, proposed in whole lots.
  **Acceptance:** a fixed lot covers the shortage in whole lots; a
  replenish-to-maximum plan proposes up to its level, bounded and
  rounded; the run's lines say what was proposed and in how many lots.
- **Later:** period lot sizes (daily, weekly, monthly), optimising
  procedures (part period, least unit cost, Groff), scrap, firming in a
  planning time fence; the replenishment agent recommending on a
  proposal of several lots.

### S2l Replenishment agent

**Capability.** A recommending agent reviews the run's proposals and
recommends what to order, from which supplier and when, for a buyer to
decide. **Later for MRP and replenishment:** lot sizing procedures (lot for
lot, fixed and replenish to maximum first, slice S2k2), safety stock calculation, MRP areas, planned orders for production (phase
5), scheduling with calendars.

- **Detailed and as built** (lane repl, 2026-11-21; ADR-0072 As built
  S2l, decided by default): `erp agent replenish -run ID` reads a
  completed run and recommends on each proposed requisition: `order`, the
  proposal's quantity in its unit, by the day it is needed, from a source
  of the material the run read (an active catalog item, an active
  contract item valid on that day, or a released or closed purchase
  order's item: the source data and purchasing history that exist) and
  its active supplier, or `source_needed` when there is none. The buyer
  acts through the requisition and order operations; the agent has none
  of them. `listPurchaseOrders` and `listContracts` filter by
  `material_id`. No migration; walkthrough step 153
  (`step_replenishment_agent`), suite `evals/replenishment`.
- **S2l1 Supplier choice by lead time and on-time share** (lane replsup,
  2026-11-24; the first item of the tracker's "later: replenishment
  agent variants", split; ADR-0082 As built S2l1 and ADR-0072 As built
  S2l1, decided by default).
  - **Adds:** `GET /v1/supplier-delivery-performance` (procurement,
    `procurement.order.read`): for a material at a plant over a stated
    window of goods receipt posting dates (default the 365 days ending on
    the plant's company's date, at most 1,096), per supplier, measured in
    SQL from posted documents: the receipts (101 items not reversed, on
    items of the material at the plant), the planned lead time (the
    order item's delivery date less the order date, on average, in
    days), the observed lead time (the receipt's posting date less the
    order date), the on-time share (receipts posted on or before the
    item's delivery date, of those whose item has one) and whether the
    history is `measured` (at least 3 receipts) or `thin`; the suppliers
    by number with their names, and the totals. The replenishment agent
    reads it for each proposal and names, in its order, the measures of
    the supplier it suggests (a line `Measures: …`, which its check holds
    to its own read of that supplier over the year ending on the run's
    day); it prefers the measured supplier with the higher on-time
    share, then the shorter observed lead time, over one with thin or no
    history, and on a tie keeps the source order it had. It still only
    proposes; its never list is unchanged. A migration indexes order
    items by material and goods movements by the one they reverse.
  - **Acceptance:** the measures equal a hand count over posted orders
    and receipts, with reversed receipts, returns, other plants, other
    materials and receipts outside the window left out; a window that
    ends before it starts or is longer than 1,096 days is 400
    `request.invalid_parameter`; an unknown plant, or one of a company
    the caller cannot act in, is 422 `request.reference_not_found`; an
    order without the measures line, or with another supplier's
    measures, is refused by the agent's check; the suite's cases (the
    faster, reliable supplier preferred; one with no history not
    preferred over one with history; a tie) pass replayed.
  - **Walkthrough:** `step_replenishment_agent` (step 153): Ben's orders
    of hinges from Tyrell (on time) and from a second supplier (late),
    received; the read shows both, and the agent suggests Tyrell naming
    its measures.
  - **Later** (the leaf's remainder): orders created on acceptance; a
    planned delivery time kept as master data (SAP's info record or
    material master, from memory) and the order dated back from the need
    by it; a weighted supplier score with price, quantity reliability and
    quality (SAP's vendor evaluation, from memory); source lists and
    quota arrangements.

### S2m Demand forecasting agent

**Capability.** A recommending agent forecasts a material's demand by
plant from its goods issues and open orders and states its accuracy
(the analytics context's forecast pattern, ADR-0044).

- **Detailed and as built** (lane fcast, 2026-11-22; one slice, one leaf,
  nothing to split; decided by default, ADR-0082 As built S2m and
  ADR-0072 As built S2m): `erp agent demand-forecast -company CODE
  -material NUMBER`, agent type `demand_forecast`
  (`internal/demandagent`). Before its session the run reads the
  material's documents and the company's confirmed orders of it and
  computes, for each plant of the company the material is extended to,
  weekly buckets ending on the company's date: the moving average of the
  last 13 weeks' goods issues (201, 601, 643 less 202, 602), the 8 weeks
  ahead each the greater of it and the confirmed orders' undelivered
  quantity due that week, and its accuracy, 100 less WAPE of the average
  over the last 4 weeks (the cash forecast's measure); one line per
  plant. The model leaves one `demand_forecast` on the material with
  every plant's line and its reasons; neither issues nor orders, no
  session. Never: every goods movement and count, every write of sales
  orders, deliveries, requisitions and purchase orders, planning runs
  and planning data. `listSalesOrders` takes `plant_id` and
  `material_id`. No migration; suite `evals/demand_forecast`; walkthrough
  step 158 (`step_demand_forecast_agent`, after `step_bad_debt_relief`).

### S2n S&OP review

**Capability.** A planner reviews the forecast against supply and
capacity by material group and month and fixes a consensus plan version.
**Later for Demand forecasting and S&OP:** statistical models beyond the
agent's, promotions, collaborative forecasts with customers, IBP's
supply optimisation, scenario comparison, capacity, approval tiers by a
plan's value or deviation (the consensus plan's approval is S2n2).

- **Detailed and as built** (lane sop, 2026-11-22; one slice, one leaf;
  decided by default, ADR-0082 As built S2n and ADR-0084 As built S2n):
  `createSopPlan` reviews a company on its day into a `draft` plan
  version numbered per company (`mrp.sop_plan`): per material group,
  base unit and calendar month of 1 to 12 (3 by default) from the day
  after, the forecast (per planned material and plant the greater of the
  moving average of 13 weeks' goods issues over the month's days and the
  orders due in it, confirmed sales orders and intercompany orders it
  supplies, S2m's rule in months), the orders, and in the first month the
  stock, purchase orders, stock in transit and MRP's proposals still to
  be ordered; the projected stock the opening stock
  and receipts less the consensus, which starts as the forecast.
  `updateSopPlan` sets the consensus by line key, `fixSopPlan` fixes the
  version, which then changes no more (database triggers); audited;
  people only. Capacity is not reviewed (no production context). Reads
  inventory's `PlantIssues`, sales' `PlantRequirementsDue` and
  procurement's `TransportReceipts` and `IntercompanyDue`, new in their published APIs.
  Migration 000447; walkthrough step 159 (`step_sop_review`).
- **S2n2 approval of the consensus plan** (lane sopapprove, 2026-11-26;
  the first item of "later: S&OP variants"; ADR-0084 As built S2n2).
  **Adds:** `companies[].sop_approval.steps` (off by default),
  `submitSopPlan`, status `submitted` and `approval_request_id`, object
  type `sop_plan` with the approval permission `mrp.sop.approve`, problems
  `mrp.sop_plan_submitted` and `mrp.sop_approval_required`; migration
  000494. **Acceptance:** where a company names steps, a version is
  submitted, not fixed; its consensus changes no more while it waits;
  approved by distinct people step by step it is fixed by the last
  approver; rejected or withdrawn it is a draft again; a company without
  steps fixes as before. A planning run does not read the consensus plan
  today, so nothing links runs to approved plans. **Walkthrough:** step
  159, `step_sop_review`.

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
MARA's views beyond the basic data. (Plant-scoped storage location codes,
listed here first, were built in batch 84.)

### S2p SAP migration of stock and values (pilot)

**Capability.** The cutover loads stock by storage location (MARD) and
valuation (MBEW) as initial entries (movement 561) valued at SAP's price,
posting against the migration clearing account, reconciled to SAP's
stock account balances. **Acceptance:** quantity and value per material
and plant equal SAP's at the cutover date.

- **Reuses:** the migration run (S2o's plants, storage locations and
  materials, its quality gate, external references and reconciliation),
  inventory's valuation and initial entry (S1c, S1d), the ledger's totals.
- **Adds:** MBEW and MARD's LABST in the extract; `inventory.TakeOver`
  and `TakenOver` (the published API, `integration → inventory`); the
  quality findings and checks `Stock`, `Stock: valuations with
  differences`, `Stock value total`, `Stock accounts: accounts with
  differences`; report counts `material_valuations`, `initial_entries`;
  ADR-0082 As built S2p.
- **Walkthrough:** `migrated_stock` in step 3, `step_migrate`.
- **Acceptance (as built):** each valuation's stock per storage location
  and value equal SAP's per material and plant (`Stock`), the stock
  accounts hold SAP's values (`Stock accounts`), the trial balance agrees
  without the take-over; what the platform cannot hold of a material that
  loads refuses the run.

**Later for the stock's migration:** split valuation and valuation at
company code level; SAP's other stock types and special stocks; batches
and serial numbers; the stock over OData; topping a taken-over stock up in
a later run.

### S2q WMS connector

**Capability.** An external warehouse system receives deliveries and
receipts to pick and put away and reports the movements back (the
integration context's connector pattern).

- **Split** (lane wms, 2026-11-22) into S2q1 (outbound deliveries and
  their goods issue) and S2q2 (inbound receipts and their goods receipt),
  each a leaf: both directions of both documents are more than one
  half-day slice. Decided by default (ADR-0082 As built S2q).

#### S2q1 Outbound deliveries to the WMS and their goods issue reported back

**Capability.** An integration administrator connects a warehouse
system for some plants; each outbound delivery created at such a plant
is sent to it to pick, and the goods issue it reports back is posted
once, as a warehouse clerk's would be.

- **Adds:** the connector kind `wms` (settings `adapter`, `base_url`,
  `plants` by company code; credentials by `secret_ref` in the tenant's
  vault namespace, PO 2026-11-16; one active connector per plant, 409
  `integration.plant_has_wms`); the provider-neutral port
  `integration.Warehouse` (send a message; list, read and acknowledge
  the confirmations) with the stand-in's adapter (`wmstest`, refused in
  production: `integration.adapter_not_certified`), called through the
  egress client (public addresses only; the walkthrough's stand-in by
  `ERP_WALKTHROUGH_WMS`); `integration.wms_message` (000449); the bus
  consumer `wms-outbound` reading shipping's OutboundDeliveryChanged
  (ADR-0007) and queueing the delivery's message once; the exchange
  worker sending the queue (retries 1, 5, 15 min, 1 h, 4 h; six attempts;
  the WMS's refusal final) and polling each active connector every 10 s for its
  confirmations; `GET /v1/wms-messages`.
- **Walkthrough:** step 160, `step_wms` (before close).
- **Acceptance:** a delivery at a WMS plant is sent once, whatever reads
  its event twice; the goods issue the WMS reports is posted once by its
  message id (a confirmation listed again is acknowledged, not posted);
  a confirmation for a delivery the connector was not sent, or one
  shipping refuses, is recorded refused and posts nothing.
- **As built** (2026-11-22): ADR-0082 As built S2q.

#### S2q2 Inbound receipts to the WMS and their goods receipt reported back

**Capability.** The warehouse system is sent each released purchase
order's stock items for a plant it manages, as receipts expected to put
away, and the goods receipt it reports back is posted once by its
message id through procurement's published API. Built on S2q1's
connector, table, port and exchange (a message kind each way).

- **Adds:** the kinds `inbound_receipt` (outbound) and `goods_receipt`
  (inbound) of `integration.wms_message` (000453, validated by 000454);
  the bus consumer `wms-outbound` reading procurement's
  PurchaseOrderReleased too and queueing the order's message once (its
  stock items at the plant of the first one a WMS manages); the port
  `integration.WMSOrders` (procurement's order read and goods receipt,
  `app.WMSOrders`); the goods receipt posted (101) by the poll;
  `GET /v1/wms-messages?purchase_order_id=` and the message's
  `purchase_order_id` and `purchase_order_number`; the connector's
  change lock lowered to `FOR NO KEY UPDATE` (the wms backlog).
- **Walkthrough:** `step_wms`, extended.
- **Acceptance:** a released order with a stock item at a WMS plant is
  sent once, whatever reads its event twice, naming only its stock items
  at that plant still to receive; the goods receipt the WMS reports is
  posted once by its message id as a 101 (listed again, acknowledged,
  not posted); one of an order the connector was not sent, naming an item
  it was not sent, or that procurement refuses, is recorded refused and
  posts nothing.
- **As built** (2026-11-22): ADR-0082 As built S2q2. Stock transport
  orders within a company and a message per plant are Later (below).

**Later for the WMS connector:** a vendor's adapter (when the pilot or
a customer names one; until then the stand-in alone, refused in
production); sending a delivery created before its plant had a WMS, or
again, by hand; a person's pick or goods issue refused on a delivery a
WMS holds (SAP's decentralised WMS locks the delivery, from memory);
changes and cancellations of a sent delivery; handling units, bins and
serial or batch numbers in the messages; a push endpoint for a WMS that
cannot be polled (security posture, the product owner's); for receipts
(S2q2), a message per plant of an order whose stock items are at
several, stock transport orders within a company, changes and
cancellations of a sent order, and returns reported by the WMS.

### S2r TMS connector

**Capability.** An external transport system receives outbound deliveries
to plan and returns the shipment and freight cost.

- **Adds:** the connector kind `tms` (a `wms` connector's settings; one
  active connector a plant, 409 `integration.plant_has_tms`); the message
  kinds `transport_request` (outbound) and `shipment` (inbound, status
  `recorded`) of `integration.wms_message` (000462, validated by 000463),
  on S2q's queue, exchange, port and stand-in; the delivery's `shipment`
  (`shipping.outbound_delivery`'s shipment number, carrier code, freight
  amount and currency, recorded at), set once by shipping's published
  `RecordShipment` (`integration.TMSDeliveries`, `app.tmsDeliveries`);
  `GET /v1/tms-messages`; OutboundDeliveryChanged's `shipment`.
- **Walkthrough:** step 161, `step_tms` (after `step_wms`, before close).
- **Acceptance:** a delivery at a TMS plant is sent once to plan,
  whatever reads its event twice, beside its WMS message; the shipment
  the TMS reports (number, carrier, freight cost with its currency) is
  recorded on the delivery once by its message id (listed again,
  acknowledged, not recorded twice), also after the goods issue, nothing
  posted; one of a delivery the connector was not sent, a second shipment
  of a delivery or one shipping cannot hold is recorded refused.
- **As built** (2026-11-23): ADR-0082 As built S2r.

**Later for the TMS connector:** a vendor's adapter; the ship-to address,
weights, volumes and dates in the request; a shipment of several
deliveries and stages; changes and cancellations of a sent delivery or a
reported shipment; a shipment entered by a person; freight settlement,
carrier selection and shipment costs on the stock value.

### S2s Trade compliance connector

**Capability.** An external trade compliance service screens deliveries'
parties and materials (sanctioned parties, export classification) and
blocks a delivery it refuses. **Later for the connectors:** an embedded
warehouse (SAP EWM), freight settlement, customs declarations.

- **Adds:** the connector kind `gts` (a `wms` connector's settings; one
  active connector a plant, 409 `integration.plant_has_gts`); the message
  kinds `screening_request` (outbound) and `screening_result` (inbound,
  status `recorded`) of `integration.wms_message` (000467, validated by
  000468), on S2q's queue, exchange, port and stand-in; the delivery's
  `trade_compliance` (`shipping.outbound_delivery`'s status `pending`,
  `released` or `blocked`, the service's reason code and answer time, and
  who released it by hand and when), `pending` from its creation at a
  screened plant (`shipping.Screening`, `integration.TradeScreening`), set
  by the service's answer through shipping's published `RecordScreening`
  (`integration.GTSDeliveries`, `app.gtsDeliveries`) or by a person's
  `releaseOutboundDeliveryTradeComplianceBlock` (permission
  `shipping.trade_compliance.release`); the goods issue refused while
  `pending` or `blocked` (409 `shipping.trade_compliance_block`);
  `GET /v1/gts-messages`; `listOutboundDeliveries?trade_compliance_status=`;
  OutboundDeliveryChanged's `trade_compliance`.
- **Walkthrough:** step 162, `step_gts` (after `step_tms`, before close).
- **Acceptance:** a delivery at a screened plant is created pending and
  sent once to screen, whatever reads its event twice; its goods issue,
  by a person or a WMS, is refused until it is released; the service's
  answer is recorded once by its message id (listed again, acknowledged,
  not recorded twice): one delivery released and goods-issued, one
  blocked and refused at goods issue; the service's answers counted in
  the order it screened, an older one read later refused; an answer of a
  delivery the
  connector was not sent, one shipping cannot hold, or one after the goods
  issue is recorded refused; a block is released by hand only with its
  own permission, audited.
- **As built** (2026-11-23): ADR-0082 As built S2s.

**Later for the trade compliance connector:** a vendor's adapter (SAP
GTS's or another's); the parties' names and addresses and the materials'
classifications in the request, for a service that holds no master data;
screening of sales orders and purchase orders, and of the business
partners themselves; a delivery created before its plant was screened,
or whose request could not be sent, sent again by hand; customs
declarations, preference calculation and licences (the tracker's "later:
trade variants").

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
- For S2p: MBEW's BWKEY (the valuation area, a plant's T001W-BWKEY when
  valuation is at plant level), BWTAR (split valuation), LBKUM and SALK3
  (the valuated stock and its value), STPRS and VERPR per PEINH; MARD's
  LABST (unrestricted stock) beside INSME, SPEME, EINME, UMLME and RETME
  (the stock types not taken over), and that LBKUM is the sum of those
  over a plant's storage locations; GBB BSA as the offset of a 561, set
  to a migration clearing account at a cutover, and SAP's G/L stock
  balance loaded against the same clearing account — all from memory.
- SAP's account determination for inventory (transaction keys BSX, WRX,
  PRD, GBB with its account modifiers, UMB) is from memory; ADR-0082 §8
  maps it onto this ERP's account determination keys and is confirmed by
  the SAP specialist before S1d.
- For S2s: SAP GTS's sanctioned party list screening and its legal
  control blocking a feeder system's delivery for goods issue until GTS
  releases it, the release of a blocked document done in GTS, and GTS
  holding the business partners and classified products replicated from
  the feeder system — all from memory, for the trade compliance
  specialist.
