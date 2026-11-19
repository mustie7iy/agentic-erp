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
  ranges to company 1000 leaves it out. No country pack ships a
  `material` range: one prefix for every company would repeat across
  the tenant's companies (ADR-0048 As built, the pilot's ranges).
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
  have theirs (since batch 83 Canada 2026.11 and UAE 2026.8 bring it;
  ADR-0048 As built).
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
  5126, 5196 and 5197 and the `WA` and `PR` journal ranges (the ranges
  from Canada 2026.11 since batch 83; ADR-0048 As built).
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

### S2o (2026-11-19)

The SAP migration run (ADR-0012, ADR-0050 §10's pattern, docs/migration.md
"Plants, storage locations and materials") loads the material master. No
migration: the external references are `integration.external_ref`'s.

- **A new dependency direction, `integration → materials`**, through
  `materials.Migration`, the context's import entry point in its published
  API (and `tenancy.OrgUnitMigration`, integration already importing
  tenancy). Materials imports nothing new, so there is no cycle; the
  `internal/layout` row names this section.
- **The run is the approval** (decided by default, as for migrated centers
  and suppliers): a migrated material is created `active` with its create
  change `applied`, no approval request, its reason "Migration run <id>:
  SAP material <MATNR>"; a material an earlier run created is extended to
  the plants and storage locations a later run adds by an `update` change
  applied at once (`materials_extended`), never shrunk. Each is checked as
  a request is (type, units, active plants of companies the run may act
  in) and audited and published (`MaterialChanged`, `MaterialChangeChanged`)
  as the API's changes are. A migrated create lists no possible
  duplicates: the run holds its company's material range from its first
  material on, and the duplicate check's description lock taken after it
  would deadlock with a request taking the two the other way round. A
  material with a pending change is not extended (409
  `materials.change_pending`, a load refusal): applying that change would
  replace its plants and drop the extension.
- **Locks** (ADR-0008, review round 1): a request takes a material or a
  company's material range and then the tenant's material change numbers;
  the run takes the materials it extends, then every new material's number
  (`Migration.Number`, before it writes any change), and the change
  numbers last, so it never waits for a material or a range while holding
  them. It holds the change numbers until it commits, so a material
  request in the tenant waits for the load (a cutover window).
  `TestMaterialLoadAndRequestDoNotDeadlock` (batch 83's integration)
  forces a request holding the range to wait while the run starts: the
  run waits for the request, and both finish.
- **Numbers** come from the `material` range of the company of the
  material's first plant (by WERKS) in the scope; SAP's MATNR is kept as
  the external reference `MARA/<MATNR>` (external numbering stays Later,
  §S1a). Plants are `T001W/<WERKS>`, storage locations
  `T001L/<WERKS>/<LGORT>`.
- **Plants and storage locations** become org units coded as SAP's WERKS
  and LGORT, a plant in the company code T001W names (BUKRS flattened in
  from T001K by BWKEY), a storage location under its plant; an active one
  the company already has with that code (under that plant) is taken
  instead of created, one that is not active refuses its key. Since
  batch 84 a storage location's code is unique under its plant, as SAP
  keys LGORT within its WERKS (below, "Storage location codes"), so one
  LGORT at two plants of a company code loads as two storage locations;
  until then tenancy kept it unique in the company and the run refused it
  (coordinator decision, batch 83).
- **Held back, not refused:** a material whose base or alternative unit is
  none of `ref.unit_of_measure`'s, whose conversion (MARM UMREZ/UMREN) is
  not whole numbers from 1 to 1,000,000 (a value with decimals of zero
  reads), whose base unit's own row is not 1/1, which lists a unit twice,
  whose type is none of ROH, HALB, FERT, HAWA, NLAG and DIEN, whose group
  is no material group or which has no description is a quality warning
  "held back: <reasons>" and is not loaded; the rest loads. A material, or
  its plant or storage location data, flagged for deletion (LVORM) is not
  migrated, a warning. Errors refuse the run: a row twice, plant data of a
  material not in MARA, storage location data without plant data or of a
  storage location not in T001L, a code that is no org unit code, a plant
  or storage location without a name. The description is the English MAKT
  text, else the first other language's (a warning), as centers' texts are.
  SAP's internal unit keys (T006-MSEHI, such as ST) are not mapped onto
  the platform's: an extract must carry the commercial code, or the
  material is held back (specialist question).
- **Reconciliation** per company code: `Plants`, `Storage locations` and
  `Materials` with their differences by SAP key (`plant <WERKS>`, `storage
  location <WERKS>/<LGORT>`, `material <MATNR>`: type, group, base unit,
  status, description, the company's plants with their storage locations,
  alternative units); held-back and deletion-flagged materials are left
  out on SAP's side, as payables' duplicate invoices are.
- **File extract only:** the OData mapping (API_PRODUCT_SRV and the plant
  APIs) is not built; an OData extract carries no material master (tracker
  leaf split from S2o).
- Walkthrough: `migrated_materials` in step 3, `step_migrate`, with the six
  checks among the run's.

### Storage location codes (2026-11-19)

Tracker leaf "Storage location codes unique per plant" (review:batch83):
a storage location's code is unique under its plant (its parent org
unit), as SAP keys LGORT within its WERKS (T001L, from memory), not
within its company; every other org unit's code stays unique in its
legal entity and kind.

- **Expand, contract** (06 §6.4): migrations 000412 and 000413 build
  `org_unit_code_once` (legal entity, kind, code; every kind but storage
  locations) and `storage_location_code_once` (legal entity, parent,
  code; `NULLS NOT DISTINCT`, so a storage location without a plant stays
  unique in its legal entity) concurrently; 000414 then drops 000002's
  key. No row migrates: the company key held every storage location the
  plant key holds. Both map to 409 `tenancy.org_unit_code_taken`, a
  storage location's naming the plant; a move under a plant that has the
  code is refused the same way (`PATCH /v1/org-units/{id}`).
- **Reads by code.** `tenancy.OrgUnitMigration.OrgUnitByCode` takes the
  parent: a storage location is found under the plant SAP names it at.
  Inventory and materials read org units by id only, and every response
  naming a storage location's code names its plant's code beside it, so
  nothing else reads by code (decided by default: no change there).
- **The SAP migration** (S2o) no longer refuses one LGORT at two plants
  of a company code, and no longer refuses a storage location the company
  has under another plant: SAP's is created under its own plant. The
  mock's Laval plant 3200 has a raw materials store 0001, as Mississauga's
  3100 has (`migrated_materials`, step 3).

### S2p (2026-11-19)

The SAP migration run takes the stock over at the cutover (plan-supply-chain.md
§S2p; docs/migration.md "Stock and values"). No migration: the external
references are `integration.external_ref`'s.

- **A new dependency direction, `integration → inventory`**, through
  `inventory.Endpoints.TakeOver` and `TakenOver` (`internal/inventory/takeover.go`,
  the context's published API; integration declares the port `Stock`).
  Inventory imports nothing new, so there is no cycle; the `internal/layout`
  row names this section.
- **What is taken over.** SAP's valuation of a material that loads at one
  of its plants (MBEW: VPRSV S or V as `standard` or `moving_average`, the
  price STPRS or VERPR per price unit PEINH, the valuation class BKLAS,
  which must be one of the company's `companies[].account_determination.inventory`)
  becomes inventory's valuation, created as `POST /v1/material-valuations`
  creates one; its unrestricted stock at the plant's storage locations
  (MARD LABST) one initial entry (561) dated the company's date today, its
  header text "Migration run <id>: SAP stock of material <MATNR> at plant
  <WERKS>" (a 561 takes no reference, `material_document_reference`). At a
  moving price each storage location is worth its share of SALK3 by
  quantity (`money.Allocate`, so the shares add up to SAP's value); at a
  standard price the stock is worth its quantity at the price, as every
  standard-priced receipt is. The valuation is kept as `MBEW/<BWKEY>/<MATNR>`,
  the document as `MKPF/<BWKEY>/<MATNR>`; a later run keeps both as they
  are (the stock is taken over once, at the cutover). Report counts
  `material_valuations` and `initial_entries`, extract count `mbew`.
- **The migration clearing account.** The entry posts the stock on the
  class's stock account against its `initial_entry` account (GBB BSA, §8),
  which a tenant names its migration clearing account for the cutover
  (SAP's practice, from memory). SAP's journal brings SAP's G/L balance of
  its stock accounts with it, so the books are right when that balance is
  on the clearing account too: SAP's stock account (as the journal loads
  it) is the class's `initial_entry` account and the class's `stock`
  account another, which the take-over fills; the clearing then nets to
  zero. Decided by default, the safer of the two: the take-over never
  redirects SAP's journal, and a setup that holds the stock twice (the
  class's stock account also SAP's G/L stock account) fails the control
  "Stock accounts" below instead of loading silently.
- **Reconciliation** per company code: `Stock` and `Stock: valuations
  with differences` (each valuation by `stock <BWKEY>/<MATNR>`: class,
  price control, stock, value and stock per storage location), `Stock
  value total` and `Stock accounts: accounts with differences` (per stock
  account of the classes, SAP's values against the account's balance on
  the platform in the run's fiscal year, which is the take-over's: a
  take-over in another fiscal year is refused. The balance is read from
  the year's journal lines, so a reconciliation of a run of a later
  fiscal year, whose opening balance a carry-forward holds outside them
  (ADR-0060 §5), would show the taken-over value as a difference; the
  stock is taken over once, at the cutover, and reading the carried-forward
  balance into the check is Later). The trial balance and the total
  debits leave the initial entries out (`ledger.Reports.EntryTotals`
  taken off `Totals`), as SAP's journal has no document of them: the
  stock controls reconcile them.
- **Refused, an error** (a stock left out would pass a reconciliation that
  leaves it out too): a valuation twice, a valuation area that is not one
  plant's (valuation at company code level), split valuation (BWTAR), a
  price control other than S and V, a class the company lacks, a price
  unit outside 1 to 10,000, a price or value that is no amount, a standard
  price of zero, a stock (LBKUM) that is not the sum of the storage
  locations' LABST (stock in quality inspection, blocked or in transfer is
  not taken over: SAP's INSME, SPEME, UMLME are not extracted), stock at a
  storage location flagged for deletion, a value without stock, MARD stock
  without a valuation, and stock of a material type that keeps none. The
  stock of a material that does not load (held back, flagged for deletion
  or not extended to the plant) is a warning naming what is not taken
  over. The services' refusals (no initial entry account, a valuation the
  plant has already, no `material_document` range) are load refusals, and
  so is a take-over whose entry falls in another fiscal year than the
  run's.
- **Locks** (ADR-0008, review rounds 1 to 3; coordinator's decision of
  19 Nov after round 3). One global order, which every posting's order is a
  part of: order items, `goods_receipt`, `material_document`, an order's
  GR/IR lock, then the journal's ranges (`journal_we` first). It is a goods
  receipt into stock's order (`POST /v1/goods-receipts`: item,
  `goods_receipt`, valuations, `material_document`, GR/IR, `journal_we`); a
  GR/IR clearing takes GR/IR then `journal_we`, a goods movement or count
  `material_document` then the journal, an invoice items, GR/IR, the
  journal. The run follows it: it locks the order items it moves
  (`lockOrderItems`), loads the receipts (`goods_receipt`; a loaded receipt
  posts no entry, `PostLoadedReceipt`, and the invoiced take-over none),
  takes the `material_document` range of each company whose stock it posts
  without drawing a number (`lockStock`: `Stock.LockNumbers`, inventory's
  port over `configuration.Numbers.Lock`, in company code order), then the
  GR/IR locks of the orders it posts GR/IR lines on (`lockGRIR`, split out
  of `takeOverGRIR`), and only then posts (`stock`, `takeOverGRIR`, the
  invoices, the journal). Taking the range before the GR/IR locks, rather
  than locking every item of the GR/IR orders, also covers an item added on
  the platform and not in the extract. Tests in
  `internal/integration/stocklock_test.go` force each opposite posting to
  hold its first lock while the run starts (a goods receipt on a migrated
  order; a GR/IR clearing; a goods receipt into stock, holding
  `material_document` and then taking the GR/IR lock): the run waits, and
  both finish; each fails on the order it replaced. The valuations and
  stock rows the run writes are new; a valuation the plant has already is
  refused.
- **Mock:** company 3000's steel sheet (moving, 1,200 KG worth 3,000.00 at
  3100/0001), shelving (standard 85.00 at both plants) and hex bolts
  (moving 12.00 per 100, 6,500 PC worth 780.00 at 3200/0001 and 0003), all
  of class 3100; the held-back washer's 300 pieces are a warning.
  Walkthrough `migrated_stock` in step 3.
- **Not built** (Later, plan §S2p): split valuation, valuation at company
  code level, SAP's other stock types (quality inspection, blocked,
  returns, in transfer), special stocks (consignment, sales order and
  project stock), batches and serial numbers, the stock over OData, and
  topping a taken-over stock up to SAP's in a later run.

### S2c (2026-11-19)

- Migration 000406: `inventory.count_document` (SAP IKPF) and
  `inventory.count_item` (ISEG), the rules of 03 §3.35 under schema tests
  (`db/physical_inventory_test.go`, the triggers also as the owner); the
  permission `inventory.count.write` (reads under `inventory.stock.read`).
  Fixture companies number count documents `PI`; the walkthrough's pilot
  settings add Maple's `PI` range (`physical_inventory`; since batch 84
  Canada 2026.12 and UAE 2026.9 bring it, ADR-0048 As built).
- `POST /v1/physical-inventory-documents` (SAP MI01), `GET` (plant,
  storage location, material, status; the latest first) and `/{id}`,
  `:enterCount` (MI04, `If-Match`). Writes are human-only
  (`x-agent: false`); no agent's never-list names them, as no agent counts.
- **One storage location per document**, up to 100 items, one per
  material: a stocked material of the tenant, `active` or `blocked` (a
  blocked material's stock is still counted, as a blocked material's
  document is still reversed, S1c), never `pending`; extended to the
  plant, kept at the storage location and valued in the plant, so its
  difference can be posted (422 `inventory.no_valuation` at creation, not
  at posting). Decided by default.
- **The book quantity is frozen** as the document is created: the
  valuations and then the stock rows of its materials are locked in the
  order every posting takes them (S1d's Locks; a row with no quantity
  written for a material never moved there), and each item's book
  quantity is the stock row's quantity under that lock.
- **The posting block is always set** (SAP's MI01 indicator, optional
  there; here the safer default): while an item is `open` no goods
  movement of any kind moves its material at that storage location.
  `takeStock`, which every posting passes (a person's, a reversal, a goods
  receipt, the migration's initial entries, S2b's goods issue), refuses it
  under the stock rows' locks, naming the material, the location and the
  document (409 `inventory.posting_blocked`); the database's
  `count_posting_block` refuses what passes it. A count takes the same
  stock rows' locks before it writes its items, so a posting and a count
  of one location serialize: the posting that came first is in the book
  quantity, one that comes after is refused (`TestCountAndIssueAtOnce`).
  The book quantity frozen is therefore the book quantity when the
  differences are posted (S2d). Counting while goods move, and a book
  quantity frozen at a past date, are Later.
- **One open count per material and storage location** (409
  `inventory.count_open`, naming the open document, checked under the
  same locks; `count_item_one_open`): a recount is a new document once
  the first is closed (posted, rejected or withdrawn, S2d).
- **Counts** are entered by item number, in the material's base unit or
  an alternative unit, converted exactly (`inventory.inexact_quantity`),
  zero included: a zero count is entered as `0`, in any unit of the
  material. An item is counted once (409 `inventory.item_counted`;
  `count_item_once`); entries may come in several calls under the
  document's row lock and `If-Match`; the document is `counted` once every
  item is. A document no longer `counting` takes no count (409
  `inventory.count_invalid_status`).
- **Audited at each step** (`inventory.count_document.create`, `.count`),
  naming its items' materials and quantities; no event, as for credit
  limit changes: the stock moves only by the material documents of S2d,
  which publish `MaterialDocumentPosted`.
- Walkthrough step 141, `step_count`.

### S2d (2026-11-19)

- Migrations 000407 and 000408: the movement types `701` and `702`, the
  account determination key `inventory_difference:<class>` (widened NOT
  VALID, validated by 000408), the count document's approval, value and
  closing, the item's difference, value and material document, the rules
  of 03 §3.35 under schema tests (`TestDifferenceRules`); the permissions
  `inventory.count.post` and `inventory.difference.approve` (an approval
  permission, which agents never hold).
- `POST /v1/physical-inventory-documents/{id}:postDifferences` (SAP MI07,
  `If-Match`), human-only. A document is posted whole, once every item is
  counted (`counted`); posting some items of it is Later.
- **Gains and losses apart:** the gains as one material document of
  `701`, the losses as one of `702`, as one movement type per document
  holds (As built S1c; SAP's MI07 posts both in one, from memory). Their
  item numbers are the count document's; neither is posted by
  `POST /v1/material-documents` (422 `inventory.unknown_movement_type`)
  nor reversed (409 `inventory.reverse_count`): a count is the record of
  what was there, and a new count corrects it (decided by default; SAP
  allows no cancellation of an MI07 posting either, from memory, for the
  specialist).
- **Valued at the material's price** as any receipt or issue is (S1d's
  `valueLines`): a gain at the moving price or what the standard price
  makes of the stock (422 `inventory.no_price` while a moving price is
  zero), a loss at the value it takes from the stock; posted as `WA`
  between the class's stock account and its new `difference` account
  (`companies[].account_determination.inventory[].difference`, an expense
  holding gains and losses alike; SAP's GBB with modifier INV, from
  memory), 422 `inventory.no_inventory_account` without it.
- **Tolerance:** `companies[].inventory_difference_tolerance`, an amount
  in the company's currency, the limit of a document's differences
  valued gains and losses alike (SAP's tolerance group's limit per
  document, OMJ2, from memory; per item and per user group are Later).
  None is zero: every difference of any value is approved (the safer
  default, as a missing manual entry limit, T35). Above it the document
  waits (`pending_approval`) on an approval request of object type
  `inventory_difference` for the fixed role `controller`
  (`approvals.DefaultInventoryDifferenceRole`, as GR/IR clearings, whose
  differences reach profit and loss too), naming the value asked for; the
  requester never approves (the approvals context). Approval posts the
  differences as valued then, in the approver's transaction on the
  company's date then; rejection (`rejected`) or the requester's
  withdrawal (`withdrawn`) posts nothing and closes the document, lifting
  the posting block: the materials are recounted by a new document
  (MI11's link between the two is Later). The posting block holds while
  the differences wait, so the book quantity is still the frozen one.
- **Locks:** the count document's row, then the valuations and stock rows
  of every item with a difference in the order every posting takes them
  (`lockCount`, so the second material document locks nothing new), then
  numbers and the ledger. The document's items are closed before its
  material documents are written, so its own block does not refuse them;
  a posting of another document never locks a count document, so no
  cycle. Two postings of one document at once post once
  (`TestPostDifferencesAtOnce`).
- `inventory` imports `approvals` (its request type and object types), a
  new direction decided here, as `materials` and `credit` do (no cycle:
  approvals imports no context).
- Each material document is published as `MaterialDocumentPosted`, its
  audit naming the count document; the count document's steps are audited
  (`.request`, `.post`, `.reject`, `.withdraw`). The web workspace names
  the object type, headless otherwise.
- Walkthrough step 141 extended: the pilot's settings add account 5198,
  the class's `difference` account, and a tolerance of CAD 50.00.

**S2b1** (ADR-0083, lane ship, 2026-11-19): movement types `601` (goods
issue for a delivery, posted by shipping through
`Endpoints.IssueForDelivery`, its reference the outbound delivery's
number) and `602` (its reversal, in the database's rules, posted by no one
yet); a valuation class's `cost_of_goods_sold` account (SAP GBB VAX, from
memory), an expense, which a 601's value posts against. `Endpoints.Held`
reads a location's stock without a lock for shipping's picking.
