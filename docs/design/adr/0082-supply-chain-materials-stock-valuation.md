# ADR-0082: Supply chain: material master, stock and valuation

**Status:** accepted (scm lane, S1a and S1b, 2026-11-17; decided by
default under ADR-0053 d after the product owner's decision of 2026-11-17
that supply chain joins the current plan; `go-reviewer` had no blocking
finding in round 3). It decides the contexts of the supply chain
foundation and the slices S1a-S1f of `docs/design/plan-supply-chain.md`.
Each slice records what it built under "As built". **The SAP behaviour
named here is stated from memory** (no primary source in the repository);
the plan's "Facts for the specialist" lists it for the SAP specialist.

## Context

Supply chain was outside the required scope of the breadth phase (product
owner, 2026-11-10). On 2026-11-17 the product owner put it into the
current plan. The sales and procurement chains built so far name what is
sold and bought by a free-text key and a description (`item_key`,
`material_key`; `plan-breadth-week2.md`, "Boundaries"), and 03 §3.16
foresaw "a shared `material` master in a future `product` context,
referenced by id from procurement, inventory, sales and manufacturing".

What the design already settles:

- Contexts own their schema; no foreign key leaves a schema (02 §2.1);
  a context reads another through a port the composition root wires, in
  the other's published types (ADR-0001, `internal/layout`).
- ADR-0003 (RLS, `tenant_id` leading every key), ADR-0004 (UUIDv7 ids,
  document numbers from the configuration's number ranges, per company,
  document type and fiscal year), ADR-0005 (no floats), ADR-0007
  (outbox), ADR-0008 (row locks, state guards), ADR-0011 (audit).
- Plants and storage locations exist: tenancy's org units of kind `plant`
  and `storage_location`, a storage location under its plant
  (`parent_id`), each in one legal entity (phase 1, 03 §3.3).
- Master data governance exists for business partners (03 §3.6,
  `internal/partner`): a create or change is a partner change applied
  when its approval request is approved, one pending change per kind, a
  duplicate list on the create change, blocking at once.
- Units of measure are `ref.unit_of_measure` (code, name, dimension).
- Financial effects go through the ledger's posting port alone
  (`ledger.Postings`).

## Decision

### 1. Two contexts: `materials` and `inventory`

- **`materials`** (schema `materials`, `internal/materials`; S1a, S1b): the
  material master and its governance. It imports `approvals` (to open
  requests, as `partner` does), `config` (the company's date) and
  `tenancy` (published types) and nothing else. It is not a leaf: from
  S1c on `inventory`, from S1e `procurement` and from S1f `sales` import
  its published API.
- **`inventory`** (schema `inventory`, `internal/inventory`; S1c-S1e and
  the physical inventory of S2): stock by location, goods movements and
  material documents, valuation and inventory postings. It imports
  `materials`, `ledger` (the posting port and calendar, as `leases`
  does), `config` and `tenancy`.

The name is `materials`, not 03 §3.16's `product`: SAP and the tracker
call the object a material (MARA), and a product master in a later sense
(sales catalogues, configurable products) is not decided here. 03 §3.16 is
updated.

### 2. Published APIs

- `materials.Materials` (S1c, S1e, S1f): a material by id with its
  number, description, base unit, type, status and plants, and whether it
  is usable at a plant (`active` and extended to it); a quantity in one of
  its units converted exactly to its base unit (`Convert`, refused when
  the result is not exact in six decimals).
- `inventory.Movements` (S1e, S2b, S2d): post a goods movement of a
  movement type in the caller's transaction and read its material
  document, for procurement's goods receipt, shipping's goods issue and
  physical inventory's differences.

Each is an interface the caller declares in its own package, satisfied by
the callee's type, as `leases.Lessors` is by `partner.Suppliers`.

### 3. Dependency directions

`materials → approvals, config, tenancy`; `inventory → materials, ledger,
config, tenancy`; `procurement → materials, inventory` (S1e) and `sales →
materials` (S1f). No cycle: neither `materials` nor `inventory` imports
procurement, sales or payables. Procurement's dependency on `inventory`
is the one new direction between existing chains; it is decided here, as
CLAUDE.md asks of a new cross-context import.

### 4. Data model (S1a, S1b)

Every table has `tenant_id` leading its primary key and RLS
(`platform.secure_table`); references to tenancy's org units and to
`ref.unit_of_measure` are by key, with no foreign key out of the schema
but `ref`'s (as every context references `ref`).

- `materials.material` (SAP MARA with MAKT's one description): `number`
  unique in the tenant; `description` (1 to 200 characters);
  `base_uom_code` (ref); `material_type`; `material_group`; `status`
  (`pending`, `active`, `blocked`; `pending` only while a create change
  waits, S1b); `version`; who created and changed it.
- `materials.plant_data` (MARC): the material at a plant, with the
  plant's legal entity kept for the companies a caller may act in.
- `materials.storage_location_data` (MARD without stock): the material
  at a storage location of one of its plants (foreign key to its
  `plant_data`).
- `materials.unit_conversion` (MARM): one row per alternative unit,
  `numerator` and `denominator` integers from 1 to 1,000,000: one
  alternative unit is `numerator / denominator` base units (SAP's UMREZ
  and UMREN, from memory). The base unit has no row.
- `materials.material_change` (S1b): the governed change, as
  `partner.partner_change` is: kind, status, payload, reason, possible
  duplicates, the approval request, numbered `MC` and eight digits per
  tenant by the context itself (as partner changes are, ADR-0004 As
  built review:T73g2).

The material types are an open enumeration of SAP's standard types (from
memory): `raw_material` (ROH), `semi_finished` (HALB), `finished_good`
(FERT), `trading_good` (HAWA), `non_stock` (NLAG), `service` (DIEN). The
material group is a code of 1 to 9 capitals, digits, `-` or `_` (SAP
MATKL's length, from memory), not yet a configured list (Later).

### 5. Quantities and units

Quantities are decimal strings in the API and `numeric(20,6)` in the
database, as procurement's are; never floats. A conversion multiplies by
the numerator and divides by the denominator exactly; a quantity whose
converted value has more than six decimals is refused, never rounded.

### 6. Number ranges

A material's number is drawn from the configuration's number range of
document type `material` of the company the request names
(`legal_entity_id`, one the caller may act in) for the fiscal year of
that company's date today, as every document is numbered (ADR-0004). SAP
numbers materials across the client, not per company code (from memory);
this ERP's ranges are per company and year, so a material is numbered by
the company that creates it and keeps that number wherever it is
extended. The number is unique in the tenant (`material_number_once`).
So that two companies' ranges cannot issue the same number, the
configuration refuses a `material` range that can issue a number another
company's or another year's `material` range issues
(`config.RangeRepeats` with `config.NumberedPerTenant`). No country pack
ships a `material` range: the signed-off packs are not changed here, as
for `lease_contract` and `expense_report` (ADR-0073, ADR-0074); a pack
version that brings one is left to the next pack change.

### 7. SAP behaviour followed, and where it is not

Followed (from memory): the material master's split into general data
(MARA), plant data (MARC), storage location data (MARD) and units
(MARM); the base unit and alternative units with numerator and
denominator; the material types' meanings; the material status blocking
use; governed creation and change as SAP MDG does it for materials, with
a duplicate check before approval; from S1c the movement types and their
reversals (101/102, 201/202, 301/302, 311/312, 501/502, 551/552,
561/562), material documents and stock per storage location; from S1d
price control `S` and `V`, the valuation area being the plant, and the
inventory account determination's split by valuation class.

Not followed:

- **Numbers per company and year** (§6), not across the client.
- **No external number assignment** and no number range per material
  type: one range per company.
- **One description**, in no language; MAKT's languages are Later.
- **Base unit and material type fixed once created** (SAP allows a type
  change through MMAM and refuses a base unit change once stock or
  documents exist; here both are refused from the start, the safer
  default, until a slice needs otherwise).
- **Statuses are three** (`pending`, `active`, `blocked`), not SAP's
  configurable cross-plant and plant-specific statuses with their own
  effects per business process.
- **The approver is the `process_owner` role**, fixed, as invoice
  exceptions' is (`approvals.DefaultExceptionRole`); a configurable role
  or policy is Later.

### 8. Inventory account determination (S1d)

The configuration's account determination (`companies[].account_determination`)
gains keys per valuation class for the stock account (SAP BSX), the
offsetting accounts of issues to consumption, scrapping and initial
entries (GBB with its modifiers), price differences (PRD) and
revaluation (UMB); GR/IR stays the existing key (WRX). The mapping is
stated from memory and built as the safer default (S1d As built); the
SAP specialist is to confirm it.

### 9. Governance (S1b)

As for business partners: creating a material makes it `pending` with a
`create` change; approval makes it `active`; rejection or withdrawal
leaves it `pending`, used nowhere, and no duplicate check finds it (as a
business partner whose create change was rejected, `partner.possibleDuplicates`). A change of an active material is an `update` change applied to
the material when approved, at its version then. Blocking applies at once
without approval; unblocking is a change. One pending change per
material. The create change lists the materials with the same
description, white space and case aside (the possible duplicates); a
create with such a match is refused (409) unless the request confirms it.

## Consequences

- Materials are tenant-wide master data with company-scoped plant data,
  as business partners are with their company data.
- A tenant creating materials needs a `material` range in each company
  that creates them, with prefixes or intervals that do not meet.
- Procurement gains a dependency on `inventory` in S1e.

## Alternatives

- **The material numbered by the context itself, as business partners
  are** (`BP` and eight digits). Rejected: the plan asks for numbers from
  the configuration's ranges, which a tenant governs and SAP migrations
  fill, and partner numbering is the exception ADR-0004's As built names.
- **A tenant-wide number range in the configuration.** Rejected for now:
  it changes every range's key (company, type, year) and the
  configuration context's tables, for one document type.
- **One context for materials and stock.** Rejected: the material master
  is read by procurement and sales, which must not depend on stock
  postings and the ledger they bring.

## As built

### Pilot assumption (product owner, 2026-11-18)

The plan's pilot assumption (`plan-supply-chain.md`, "Pilot relevance")
is confirmed and refined: materials are valued by material type, purchased
ones (`raw_material`, `trading_good`) at moving average and own-produced
ones (`semi_finished`, `finished_good`) at standard price, as SAP practice
is; one plant per physical stock site; production stays outside this ERP
(option A), finished goods received by a `501` at a standard price set by
hand (`:changePrice`) and components issued by a `201`; manufacturing comes
after completion. Nothing of S1a-S1d changes: price control stays the
inventory accountant's choice per valuation, which the pilot's settings
follow by type.

### S1a (2026-11-17)

- Migration 000373: schema `materials` with its outbox and inbox,
  `materials.material`, `.plant_data`, `.storage_location_data` and
  `.unit_conversion` with RLS, `tenant_id` first, the rules of 03
  §3.34 with schema tests (`db/materials_test.go`, the triggers also as
  the owner); the permissions `materials.material.read` and `.write`.
  `materials` is in `outbox.Contexts` and in `internal/layout`, importing
  `config` and `tenancy` only.
- **Nothing is deleted** (000011: no delete grant anywhere). A plant,
  storage location or unit a change takes off a material is marked
  `removed_at`/`removed_by`, the only columns of those rows the
  application updates (with a unit's factors); giving it back clears the
  mark. Decided by default from the platform's rule.
- `POST /v1/materials`, `GET /v1/materials` (number, plant, status, type,
  group; by number), `GET /v1/materials/{id}`, `PATCH /v1/materials/{id}`
  (merge patch, `If-Match`; description, group, status `active` or
  `blocked`, plants and units as whole lists). Writes are human-only
  (`x-agent: false`), the safer default until an agent needs them.
- **Plants by company access.** A material is the tenant's, readable by
  anyone with `materials.material.read`; its plants are shown, and
  replaced by a change, only in the companies the caller may act in, as
  a business partner's company data is (`partner.visible`). A plant of a
  company the caller may not act in stays as it is.
- **Number range.** `config.NumberedPerTenant` names `material`;
  `config.RangeRepeats` refuses a `material` range that can issue
  another company's or year's material numbers. The fixture gives each
  company a prefix of its own (`MA1000-`, `MA2000-`); the walkthrough's
  pilot settings add Maple's `MA` range, and step 40's copy of Maple's
  ranges to company 1000 leaves it out.
- **Base unit and type** are absent from the change's contract (400) and
  fixed by `material_fixed` as well. A change that would leave the
  material as it is is 422 `materials.nothing_to_change`.
- `MaterialChanged` (`created`, `changed`) names the numbering company and
  every plant by id and code, whatever companies the person who changed it
  may act in; the audit record of a change names the fields changed with
  their values before and after.
- tenancy's published API gains `Unit.ParentID` and `Directory.OrgUnits`
  (the org units by id in one read), so a page of materials reads its
  plants and storage locations once.
- Walkthrough step 122, `step_material`.

### S1b (2026-11-17)

- Migrations 000374 and 000375: `materials.material_change` (numbered
  `MC` and eight digits by the context, kind `create`, `update`, `block`
  or `unblock`, status `pending`, `applied`, `rejected` or `cancelled`,
  applied exactly with `applied_at`, its payload an object and its
  possible duplicates an array, a reason of at most 500 characters, one
  pending per material, `material_change_one_pending`, 409
  `materials.change_pending`); the material status `pending`, widened NOT
  VALID and validated by 000375; `platform.normalized_text`, partner's
  name rule for every context (`TestNormalizedText` holds them equal),
  with the index of the description match; the approval permission
  `materials.change.approve`.
- `POST /v1/materials` creates the material `pending` (`pending_change_id`,
  `pending_change_number`) with its `create` change and approval request
  (object type `material_change`, the fixed role `process_owner`,
  `approvals.DefaultMaterialChangeRole`); approved it is `active`.
  `confirm_duplicate` and `reason` on the request.
- **Duplicates.** The materials of the same description, white space and
  case aside, are listed on the create change, whatever their status, but
  a pending one whose create change was rejected or withdrawn; a match is
  refused (409 `materials.duplicate_description`) unless confirmed. The
  description's advisory lock is taken before the check, so two creations
  at once serialize (`TestDuplicatesAtOnce`). Description matches are
  refused, not only listed (partner lists a name match and refuses only a
  tax number's): a material has no other identifier to match by.
- **PATCH answers 200 with the material**, not 202: in this API a 202
  carries a long-running operation (`TestOperations`), so a pending
  change is named by the material's `pending_change_id`. A change that
  only blocks applies at once (an applied `block` change); one that only
  sets `active` is an `unblock`; any other an `update`. A pending material
  takes no change (409 `materials.invalid_status`).
- **Applying.** An update applies to the material as it is when approved;
  its plants replace those of the companies its requester could act in
  when it was requested (`plant_scope`, kept in the payload with each
  plant's company), whatever the approver's access, and are not checked
  again: they were when requested (`TestApprovalAfterPlantBlocked`). Approving, rejecting or withdrawing a
  decided change is 409 `materials.invalid_status`.
- **Locks.** A request locks the material, then writes the change; a
  decision reads the change, locks the material, then the change (the
  same order), so a request and a decision of one material never wait on
  each other in a cycle.
- **A block wins.** A block applies at once even while a change waits (it
  is an applied change, so `material_change_one_pending` does not refuse
  it), and a change keeps only the fields it changes (`Requested.only`),
  so an update approved after the block leaves it in place
  (review round 1 on S1b; `TestBlockWhileChangePending`).
- **Plants by company access on changes too.** A change's requested
  plants are shown only in the companies the caller may act in, each with
  its company's code and its storage locations by code and name; the
  list's `plant_id` filter matches only such plants (review round 1).
- **Only a creation is checked for duplicates**; a change of the
  description is not (its approver sees it). Planned with the fuzzy
  check of the governance variants.
- Numbering a material change moved, with partner's, to
  `internal/platform/numbering` (review round 1: a helper is not copied).
- `GET /v1/material-changes` (material, kind, status; newest first) and
  `/{id}`, naming the material's number and description and the plants'
  codes. `MaterialChangeChanged` at each step; `MaterialChanged` gains
  `activated`, `blocked` and `unblocked`.
- The web workspace names the object type (`object.material_change`) and
  summarises a material change on a request's page, as for every object
  type approvals decide.
- Walkthrough step 122 extended (each create and change approved by
  Priya) and step 123, `step_material_governance`.

### S1c (2026-11-18)

- Migration 000376: schema `inventory` with its outbox and inbox,
  `inventory.material_document` (SAP MKPF), `.movement` (MSEG: one row per
  stock effect, a transfer's issue and receipt apart) and `.stock` (MARD's
  quantity) with RLS, `tenant_id` first, the rules of 03 §3.35 with schema
  tests (`db/inventory_test.go`, the triggers also as the owner); the
  permissions `inventory.movement.post` and `inventory.stock.read` (which
  reads documents too). `inventory` is in `outbox.Contexts` and in
  `internal/layout`, importing `config`, `ledger`, `materials` and
  `tenancy`; it is not a leaf, as procurement will import it (S1e).
- `materials.Materials`, the published API (§2): materials by id with
  their plants, storage locations and units, whatever companies the
  caller may act in; `Material.Stocked` (types other than `non_stock` and
  `service`), `KeptAt` and `Convert` (exact, `ErrUnknownUnit`,
  `ErrInexact`).
- **One movement type per document**, as SAP's MIGO posts one
  transaction's items with one; items up to 100. The forward types are
  posted by `POST /v1/material-documents`, the reversal types only by
  `:reverse` (422 `inventory.unknown_movement_type` otherwise). Decided by
  default.
- **Cost centers.** A `201` and a `551` name an active cost center of the
  document's company in force on the posting date (SAP's scrapping takes
  its cost center as an issue does, from memory); other types name none.
- **Transfers stay in one company**: a `301` to a plant of another
  company is refused (422 `inventory.mixed_companies`); between companies
  is S2i's. Every plant of a document is of one company, whose range
  numbers it.
- **Posting date is the company's date today** (`config.DocumentDay`);
  the request gives none, so a document is never dated before those
  posted ahead of it (but around midnight, when one dated the day before
  may wait for one dated today). Posting into an earlier day waits for S1d's periods
  (Later). SAP allows the current and the previous period.
- **Stock never below zero.** A posting locks the stock rows it moves in
  one order (material, plant, storage location, as PostgreSQL orders
  uuids), writing an empty row for a first movement, then checks the
  quantity left (409 `inventory.insufficient_stock` naming the material,
  the location and both quantities) and the column's 14 integer digits
  (422 `inventory.quantity_too_large`); the database adds each movement to
  its row (`movement_stock`, a security definer, the only writer of the
  quantity) and refuses any line below zero (`stock_not_negative`,
  registered as 409 `inventory.insufficient_stock`), so the check walks
  the lines in their order as the database applies them, and a reversal
  takes the original's lines last first, back through the states the
  original went through (review round 1: a chain's reversal went below
  zero line by line; `TestCircularTransfers`). `TestIssuesAtOnce` holds the
  lock; postings of a company also queue on its number range.
- **The stock list has no totals**: it is a list of locations, each in
  its material's base unit; a total per material or plant is Later (SAP
  MMBE's), never across materials, whose units differ.
- **Reversal** of the whole document, once: a lock named after the
  document serializes reversals (documents are insert-only), as
  procurement's goods receipts do; `If-Match` is the document's
  representation (`etag.Of`), which moves when it is reversed. A
  reversal moves the stock back under the same checks; a material blocked
  since is still reversed (a correction, not a new use).
- `GET /v1/stock` (material, plant, storage location, `as_of`): now from
  `inventory.stock`, as at a day the sum of the movements of documents
  posted up to it; a location with none is left out; in the order its
  stock was first moved. `GET /v1/material-documents` (material, plant,
  movement type; the latest first) and `/{id}`, each line naming its
  material, plant, storage location and cost center by number or code.
- `MaterialDocumentPosted` names the company and every line's material,
  plant, storage location and cost center by id and number or code; the
  header text, a person's free text, is in neither the event nor the
  audit, which says whether there is one (ADR-0079). Writes are
  human-only (`x-agent: false`); no agent's never-list names an inventory
  operation, as no agent posts goods movements.
- Walkthrough step 125, `step_goods_movements` (numbered after S1d's
  step 124, which values its materials first); the pilot's settings add
  Maple's `MD` range (`material_document`), as the fixture's companies
  have theirs.
- **The ledger's refusal** (review of batch 80, batch 81): the ledger's
  `reverseJournalEntry` refuses an inventory entry (409
  `ledger.entry_owned_elsewhere`, `requireOwnEntry`) naming where it is
  reversed, as it names the other owners': a material document's entry
  (WE, WA) with `POST /v1/material-documents/{id}:reverse` and the
  document's id (a reversal document is not reversed again); a price
  change's (PR, S1d) names no reversal, since a price change is corrected
  by another price change (SAP's MR21, from memory): the entry does not
  name the valuation, so the detail names the material by its number (the
  entry's reference) and the reads that find its plant's valuation
  (`GET /v1/materials?number=`, `GET /v1/material-valuations?material_id=`)
  before `POST /v1/material-valuations/{id}:changePrice` (review round
  1). Before, the refusal said no reversal path existed.
  `TestReversalBelongsToTheOwner`.

### S1d (2026-11-18)

- Migrations 000377 and 000378: `inventory.valuation` (MBEW: price
  control `standard` or `moving_average`, the price per price unit in the
  company's currency, the valuation class, the stock's quantity and value
  in the plant, the valuation area) and `inventory.price_change` (MR21),
  with RLS and the rules of 03 §3.35 under schema tests; `movement.amount`
  and `.difference`; the material document's currency and journal entry.
  The account determination's key check takes
  `inventory_<kind>:<class>` and the journal's source contexts
  `inventory`, widened NOT VALID and validated by 000378. Permissions
  `inventory.valuation.read` and `.write`.
- **The database keeps the valuation.** `movement_stock` adds each line's
  quantity and value to its material's valuation in the plant, which
  must exist (`movement_valuation`), and sets a moving price to the value
  per price unit of the quantity, rounded half away from zero, while there
  is stock; `price_change_valuation` sets the price and adds the
  revaluation, of the price and stock as they are. The application may
  change a valuation's class alone; a standard-priced valuation is worth
  its quantity at its price (`valuation_standard_value`), so a wrong
  amount cannot be written.
- **Valuing a line** (`valueLines`, in line order as the database applies
  them): out of stock, a line takes what the value left leaves (at a
  standard price the rest is worth its quantity at the price; at a moving
  price the value in proportion, an issue of the whole stock its whole
  value), so the rounding of an issue is taken by the value left; into
  stock at a standard price it adds what the stock is then worth at the
  price, at a moving price what it is worth: the receipt's `amount` (501
  and 561 only), what a transfer's issue took, what a reversal's original
  was, or else its quantity at the moving price (422 `inventory.no_price`
  while that is zero). What a line is valued at beyond what its stock
  takes is its price difference.
- **Accounts by valuation class** (`companies[].account_determination.inventory`,
  each class's `stock`, `receipt` (501), `initial_entry` (561),
  `consumption` (201), `scrapping` (551), `price_difference` and
  `revaluation`; the configuration checks them, they are automatic
  accounts, and a company with classes needs `journal_we`, `journal_wa`
  and `journal_pr` ranges with its `material_document` ones). The ledger
  writes them as account determination keys (`config.InventoryKey`);
  inventory reads them through `ledger.Accounts.Determination`. A country
  pack ships none, and applying one keeps a document's classes.
- **One journal entry per document** through `ledger.Postings.Post`
  (source context `inventory`, the material document as source document),
  dated its posting date in the company's currency: each line's value on
  its class's stock account with its quantity, its price difference on
  the price difference account, the rest on its movement type's account
  with the item's cost center; document type `WE` for 501, 561 and their
  reversals, `WA` for the rest. A transfer between plants posts both
  stock lines (SAP's 301 posts BSX twice, from memory); a transfer within
  a plant (311, 312) posts none. A class without an account a line needs
  is refused (422 `inventory.no_inventory_account`) before anything is
  posted; the ledger's refusals (a closed period) refuse the movement.
- **Reversals** value each line at what its original line was, given
  back, its stock's share at the price now, the rest a price difference,
  against the original's accounts; a 312, as a transfer within a plant,
  receives what its issue took, so it leaves the plant's value as it was
  whatever moved since (review round 1, `TestTransferReversalAfterReceipt`),
  and a 302 gives each plant back what the 301 moved, its price
  difference reversed (`TestStandardTransfersAndReversals`).
- **Range.** A value beyond an amount's range is refused (422
  `inventory.amount_too_large`, `money.Sum` of the stock's value); the
  database computes a moving price by integer division of numerics,
  exactly as the application does (review round 1).
- **Valuation** of a stocked material extended to an active plant of a
  company the caller may act in, pending or blocked materials too (their
  accounting view is master data); refused when stock was moved there
  unvalued (409 `inventory.stock_without_valuation`, possible only for S1c
  documents). The class changes while there is no stock (409
  `inventory.valuation_class_with_stock`), as SAP refuses it with stock
  (from memory). Decided by default.
- **Price change** (`:changePrice`, If-Match): checked by `Postable` on the
  company's day (409 `ledger.period_not_open`), the stock revalued at
  quantity × new price / price unit against the class's revaluation
  account (document type `PR`), a moving price too; without stock the
  price is set and nothing posted. It answers the valuation with the
  change as its `last_price_change`, naming the revaluation's journal
  entry (review round 1: a consumer finds the entry it posted).
  `MaterialPriceChanged`. A list of a valuation's price changes is Later.
- **Locks.** A posting locks the valuations of its lines (material,
  plant order), then their stock rows, then draws numbers; a price change
  locks its valuation first. Valuations are read under the lock, so two
  postings value against what the other left.
- `MaterialDocumentPosted` gains the journal entry and each line's
  `amount` and `price_difference`, not required (ADR-0041).
- Walkthrough step 124, `step_valuation`, before step 125; the pilot's
  settings add valuation class 3100 with accounts 1416, 2196, 3996, 5116,
  5126, 5196 and 5197 and the `WA` and `PR` journal ranges.
- **Not built** (Later): the material ledger and actual costing, split
  valuation, future and previous period prices, revaluation into a past
  period, below-zero moving prices, small-difference tolerances. Totals
  of the stock list (ADR S1c) stay Later.

### S1e (2026-11-18)

- Migrations 000390 and 000391: `purchase_order_item.plant_id` and
  `.storage_location_id` (`purchase_order_item_stock`, kept by a released
  order's item, `purchase_order_item_identity`); `goods_movement.material_document_id`
  and `_number`; the material document's movement types `101` and `102`
  and its `reference`; `inventory.invoice_difference`. Rules of 03 §3.9 and
  §3.35 under schema tests (`db/stock_receipt_test.go`).
- **A stock item** is an order item naming a plant and a storage location
  with its material; the `material_id` an item could always carry without
  them (000006, unchecked) still makes no stock item, which S1f maps. It is
  checked as it is ordered and as an order change adds it: an active
  material kept in stock at that storage location of a plant of the
  company, no cost center, a unit of the material whose ordered quantity
  converts to the base unit exactly, ordered in the company's currency
  (422 `procurement.invalid_stock_item`). An item a change keeps keeps its
  material and location (`purchase_order_item_identity`) and is checked
  only for its quantity's exact conversion: a material blocked or a
  storage location removed since does not refuse the change, so its open
  quantity can still be brought down to what was received (review round 1).
  Order items and order changes answer the plant's and storage location's
  codes and names beside their ids. Its valuation is not checked
  until a receipt (the acceptance: a receipt without one is refused,
  nothing received, 422 `inventory.no_valuation`). Its `account_id` stays
  required, as the contract has it, and is kept, but is not posted: its
  receipts and invoices post the accounts of its valuation class, as SAP
  determines BSX and PRD for an item without account assignment (from
  memory). Decided by default.
- **One entry per goods movement.** Procurement's WE entry (source context
  `procurement`, which moves GR/IR's positions) carries the stock's value:
  inventory's `ReceiveForOrder` and `ReverseForOrder` lock the valuations
  and stock first, number the material document, have procurement post the
  entry through a callback (`inventory.PostEntry`) with each stock item's
  booking (`ledger.GRIRStock`), then write the document naming that entry,
  dated the goods receipt's posting date. So a receipt takes the locks in
  the order every inventory posting does (valuations, stock, ranges, then
  the ledger's GR/IR lock and periods), and a 501 and a 101 of one material
  never wait on each other in a cycle.
- **Values.** At a moving price a receipt is worth what its GR/IR line moves,
  the order price of its quantity as the ledger keeps a position exact; at a
  standard price its quantity at the price, the rest on the class's price
  difference account. A receipt's reversal (102) is valued as any reversal
  out of stock (S1d), against the original line's value, the rest of what
  its GR/IR line moves on the price difference account; it is refused once
  the stock is gone (409 `inventory.insufficient_stock`). A goods receipt's
  material document is reversed only by reversing the goods receipt (409
  `inventory.reverse_goods_receipt`), and `POST /v1/material-documents`
  posts neither type.
- **Not received into stock** (decided by default; Later): a return to the
  supplier (122) of a stock item (422 `procurement.stock_item_return`, the
  plan's Later), and a stock item of an order valued directly or in another
  currency than the company's, in which a valuation is kept (422
  `procurement.stock_item_not_valued`; foreign-currency stock items are
  Later). A material blocked since the order is not received
  (`inventory.material_not_usable`).
- **The invoice.** Its price difference (what the ledger puts beside GR/IR)
  goes to the stock as far as the valuation's stock in the plant still holds
  the quantity it is spread over, at a moving price; the rest, and all of it
  at a standard price, to the class's price difference account (SAP's
  stock coverage, from memory). Items of one valuation cover the stock in
  turn. The quantity is the invoiced one; a subsequent debit or credit,
  which moves none, spreads over what was received of the item (SAP spreads
  it over the invoiced quantity, from memory: for the specialist). A credit
  memo takes its difference off the stock the same way. What the stock took
  is `inventory.invoice_difference`, which adds it to the valuation's value
  and sets the moving price; a value below zero is refused (409
  `inventory.stock_value_below_zero`). Inventory's `InvoiceDifferences`
  locks the valuations before payables posts (the order items, then the
  valuations, then the ledger's locks, as a goods receipt).
- **An invoice that revalued stock is not reversed** (409
  `payables.invoice_revalued_stock`): the reversal would mirror the stock
  line onto a stock that may have been issued since; a credit memo corrects
  it. The safer default; reversing it as far as the stock holds is Later.
- **Ledger.** `GRIRItem.Stock` divides a stock item's counter line between
  the stock and price difference accounts (`ledger.GRIRStock`: a goods
  movement's amount, an invoice's share), in the company's currency only;
  a divided line is converted at the posting's rates (its group amounts'
  difference an exchange difference), an undivided one kept as the GR/IR
  line's mirror. `Entry.Stock` answers what each item put on either.
- **Dependencies** (§3): procurement imports `materials` and `inventory`;
  payables imports `inventory`'s published API, a new direction, decided
  here (no cycle: inventory imports neither). `materials.Material.Factor`
  is a unit's factor, which `Convert` and the invoice's coverage use.
- `MaterialDocumentPosted` gains `reference`, not required (ADR-0041); the
  goods receipt answers `material_document_id` and `_number`.
- **Not built** (Later, as planned): receipts into quality inspection,
  delivery tolerances beyond the over-delivery percentage, GR-based invoice
  verification by receipt, ERS, returns (122, 161), planned delivery costs.
  A GR/IR clearing (MR11) of a stock item's position still posts its
  difference to the company's price difference account, not to the stock
  (for the specialist).
- Walkthrough step 130, `step_stock_receipt`, after step 125.

### S1f (2026-11-19)

Split into S1f1 (sales items) and S1f2 (purchasing items and the
catalog's keys mapped), plan-supply-chain.md §S1f.

- **S1f1, migrations 000395 and 000396:** `sales.quotation_item` and
  `sales.sales_order_item` gain `material_id` (expand: nullable; a material
  item has an item key, `quotation_item_material_key` and
  `sales_order_item_material_key`, added `NOT VALID` and validated by
  000396; schema test `db/sales_material_items_test.go`). Sales imports
  `materials`' published API (§3, as decided).
- **The item key is the material's number** (SAP's item names its material
  by MATNR, from memory): absent, it is taken; another is refused (422
  `sales.item_key_not_material`). So the price records, discounts and
  scales (ADR-0067 §10-§12), the revenue obligations, the profitability
  segments and the sales analytics find a material item by key as they
  find any other: "sales prices found by material as well as by key" is
  met without a material on the price record (decided by default; the
  lane owns no pricing beyond the key lookup).
- **The material** is one of the tenant's (422 `request.reference_not_found`
  naming `items[i].material_id`), `active` on a new item (422
  `sales.material_not_usable` for `pending` or `blocked`), of any type: a
  sales view per sales organisation (MVKE) is Later, so a material needs no
  extension to a plant to be sold. Its description is the item's when the
  item sends none; one sent is the item's own (SAP's item text, from
  memory).
- **The quantity** is in the material's base unit or one of its
  alternative units and converts exactly in six decimals (422
  `sales.invalid_material_quantity` naming `items[i].quantity.unit`). The
  converted quantity is not stored, as S1e's order item does not store it:
  it is taken where stock moves (S2b's goods issue).
- **An item an order change keeps** keeps its material: the column is not
  among the application's grants on `sales.sales_order_item` (000355), and
  a quotation's items are immutable. Its new quantity must still convert
  exactly; a material blocked since does not refuse the change, as S1e's
  kept stock item. An added item is checked as a new one, its refusals
  naming `new_items[i]`.
- **Reads:** every sales item (`SalesItem`: quotation, order and billing
  items) answers `material_id`, its number being `item_key`; a billing
  document's item has its order item's. Delivery confirmations, returns
  and credit memo requests keep naming the item by its key.
- **One check for both contexts:** `materials.Material.ForItem` (the
  published API) checks an item naming a material (active when added, keyed
  by the number, in one of its units, exact) and gives its key and
  description; `UnitRefusal` says why a unit or quantity is refused. Sales,
  procurement and S1e's stock item use them, each with its own problem
  codes; `ByNumber` reads materials by number for the mapping.
- **S1f2, migration 000397:** `procurement.rfq_item.material_id` and
  `procurement.catalog_item.material_id` (expand: nullable); the
  application may update a catalog item's `material_id`, which
  `catalog_item_material_fixed` lets it set once and never change or clear,
  for the tables' owner too; `procurement.requisition_item.material_number`,
  the material's number kept beside its id as SAP's item keeps MATNR
  (`requisition_item_material_number`: only with a material, 1 to 40
  characters; schema tests `db/purchasing_material_items_test.go`). A
  requisition item's `material_id` (000006, unchecked until now) is checked
  as it is entered, and the item answers the number as its `item_key` when
  it has no catalog item's (review round 1: an id with the number a
  consumer needs).
- **Purchasing items** are checked as a sales item is (422
  `request.reference_not_found`, `procurement.material_not_usable`,
  `procurement.key_not_material` naming the RFQ item's `material_key` or the
  catalog item's `item_key`, `procurement.invalid_material_quantity` naming
  the item's unit; a catalog item, having no quantity, needs only its unit
  to be the material's). A requisition item has no key of its own; an RFQ
  item's `material_key` and a catalog item's `item_key` become optional in
  the contract, required without a material. Requisition items are checked
  as new on every write: a draft's material blocked since refuses its next
  change, as a draft's other references do.
- **The material is carried** from a catalog item to a requisition item
  taken from it (which may send that material or none, 422
  `procurement.catalog_item_override` otherwise), from a requisition item
  to the RFQ item created from it, and from an RFQ item to its award's
  contract item and to the purchase order an award creates. An RFQ's keys
  are unique: a requisition item is asked for by its key (its catalog
  item's, or its material's number), and items sharing a key (one material
  twice, or a material whose number an unmapped catalog item's key is)
  are each asked for by the requisition's number and their item number,
  without the material, rather than refused (decided by default; review
  round 1: such a requisition could otherwise never be sourced). A purchase order's item keeps
  answering `material_id` only for a stock item (S1e).
- **The mapping** (`POST /v1/catalog-material-mappings`,
  `procurement.catalog.manage`) maps a company's catalog, the master data
  new items are taken from: each active catalog item without a material
  whose key is the number of an active material, in one of its units,
  takes it, its version raised, audited and published
  (`CatalogItemChanged` gains `material_id` within v1). The items are locked
  in key and id order, so two runs of a company wait for each other; a
  second run maps nothing twice (a test holds the first run's transaction
  and sees the second wait). The report lists every key still without a
  material before the run, `mapped`, `no_material` or `not_mappable` with
  its reason (the status, or every unit that is none of the material's),
  and its items' count, with the items of each outcome totalled. It answers 200 with its report, a
  run as `/v1/accrual-proposals` is, not a collection custom method (those
  answer 202 with an Operation).
- **Documents are not re-keyed** (decided by default): a quotation, sales
  order, billing document, requisition, RFQ or contract already made keeps
  its items, so nothing billed or received changes, and SAP does not re-key
  a document's item either (from memory). Sales prices need no mapping:
  they are found by the material's number (S1f1). Mapping by a table of key
  and material pairs, open documents' items and contracts' items are Later.
- Walkthrough step 135, `step_material_items`, after step 134: the sales
  part (S1f1), then a requisition item by material and the catalog mapped
  (S1f2).


**S2b1** (ADR-0083, lane ship, 2026-11-19): movement types `601` (goods
issue for a delivery, posted by shipping through
`Endpoints.IssueForDelivery`, its reference the outbound delivery's
number) and `602` (its reversal, in the database's rules, posted by no one
yet); a valuation class's `cost_of_goods_sold` account (SAP GBB VAX, from
memory), an expense, which a 601's value posts against. `Endpoints.Held`
reads a location's stock without a lock for shipping's picking.
