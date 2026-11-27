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
  plan's Later; built in S1e2 below), and a stock item of an order valued directly or in another
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

### S1e2: returns to the supplier out of stock (2026-11-27)

The first slice of the plan's "later: inventory posting variants" (plan
§S1e2; decided by default where nothing above settles it, each listed for
the product owner).

- Migrations 000494 and 000495 (03 §3.35): the material document's
  movement types `122` (return delivery to the supplier) and `123` (its
  reversal), each naming the goods movement as its `reference`, a 123
  reversing a 122 (`material_document_reversal`, as every type the next);
  under schema tests (`db/supplier_return_test.go`, the down's refusal
  too). The contract's open enumerations of movement types gain both.
- **A return (122) of a stock item takes the goods out of stock**: its
  order item's material out of the storage location the item names, in the
  goods movement's transaction, through inventory's new
  `ReturnForOrder` (one function in its published API), which checks each
  item as a goods issue's (an active material kept there; a blocked
  material is refused, `inventory.material_not_usable`, as a delivery's
  goods issue refuses it), locks the valuations and stock as a 101 does and
  refuses a return beyond the stock there (409
  `inventory.insufficient_stock`) as well as beyond what was received
  (409 `procurement.return_exceeds_receipts`, as before). The stock is
  unrestricted stock: there are no stock types yet.
- **Values: as the receipt's reversal (102), reused, not a second
  valuation.** The line leaves the stock at a standard price at the price,
  at a moving price at the value in proportion (S1d's issue); the WE entry
  debits GR/IR at the order item's valuation price, as a return of any
  item does, and the rest goes to the valuation class's price difference
  account (`ledger.GRIRStock`). So a return of a moving-priced material
  whose price has moved since the receipt leaves its price unchanged and
  books the difference (SAP credits the stock at the receipt's value and
  moves the price, from memory: for the specialist). The returned quantity
  is no longer received: the order item can be received again, by a 101
  at the order price.
- **Its reversal (123) puts back what the return took**: inventory's
  `ReverseForOrder` now reverses a 122 as well as a 101, its lines given
  back at the value the 122's lines took out (at a moving price that value,
  at a standard price the price), the GR/IR line credited at the order
  price, the rest a price difference; reversed once (409
  `procurement.already_reversed`). A 123's lines are given back what the
  122's lines took from the stock alone (`reversedLines`), the price
  difference beside them being the GR/IR line's to take back; a 102 and a
  person's reversal are valued as before.
- **A return after the invoice is refused for a stock item** (409
  `procurement.return_below_invoiced`, decided by default, the simpler and
  safe one; SAP allows it and expects a credit memo, from memory: for the
  specialist): a return may not leave the order item received below what
  posted documents invoiced, net of credit memos, as a receipt's reversal
  may not; the supplier's credit memo comes first. A return of an item that
  is no stock item is not held to it, as before (ADR-0034).
- **Batches and serial numbers**: a batch-managed item's return names the
  batch it gives back by number alone (dates, 422
  `procurement.return_batch_dates`; a number of no batch of the material,
  422 `inventory.batch_invalid`; none, 422 `inventory.batch_required`), and
  a serial-managed item's the serial number of each unit, in stock at the
  location (409 `inventory.serial_not_in_stock`), as a delivery's goods
  issue names them (S2f2, S2g2). Movement types 122 and 123 carry both
  (`tracked`); the reversal gives them back. Inventory's field
  `batch_number` is the request's `batch` in the goods receipt's refusal.
- **An intercompany stock transport order's stock items are still not
  returned** (422 `procurement.stock_item_return`, reworded): the goods
  would leave the receiving company with nothing bringing them back to the
  supplying one (As built S2i); the receipt's reversal (102) puts them back
  in transit.
- **Locks**: a return takes the order items (`FOR UPDATE`, `lockItems`),
  then the valuations, stock, batches and serial numbers, the ranges and
  the ledger's locks, as a receipt does; an invoice takes the order items,
  then the valuations, so a return, a receipt, an issue and an invoice of
  the material post at once without a cycle (`TestSupplierReturnsAtOnce`).
- People only, as the goods receipt: a return is `postGoodsReceipt` with
  movement type 122, already on the never lists of the agents that list
  it; nothing new to list.
- Walkthrough step 130, `step_stock_receipt`, extended: four brackets
  returned, stock and GR/IR down to 96 received, received again; a return
  after the invoice refused.
- **Not built** (plan §S1e2, Later): a return naming the receipt (material
  document) it returns rather than the order item, at that receipt's value;
  returns from quality inspection or blocked stock (161, 122 from stock
  types); a return delivery with its own outbound delivery and shipping
  papers; returns of an intercompany order's stock items.

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
  (MARD LABST) one initial entry (561) dated the company's date today as
  the run read it when it locked the company's range (below), its
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
  port over `configuration.Numbers.Lock`, in company code order; it
  answers the company's date today it locked the fiscal year's range for,
  and the run dates that company's take-overs that day, so a run crossing
  midnight into a new fiscal year still draws from the range it holds,
  batch 84 integration), then the
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
  (MI11's link between the two is Later; an item recounted on its own
  document before posting is S2d2's). The posting block holds while the
  differences wait, so the book quantity is still the frozen one.
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

### S2d2 (2026-11-25)

Recounts with history, the first item of the tracker's "later:
difference posting variants" leaf (plan-supply-chain.md §S2d2), decided
by default from SAP's recount (MI11, ISEG's recount counter ZAEHL and the
recount indicator, all from memory, for the specialist):

- **The same document, not a new one.** SAP's MI11 marks the item and
  creates a new document for the recount, linked to the old (from
  memory); here the recount stays on its item, as the brief and S2c's
  one open count per material and location make simpler: the item keeps
  its book quantity, its posting block and its number, and a second
  document never holds the material while the first waits. Recounting by
  a new document linked to the first is in the leaf's remainder.
- `POST /v1/physical-inventory-documents/{id}:recount`
  (`recountPhysicalInventoryItems`, `If-Match`, permission
  `inventory.count.write`, as entering a count: SAP's MI11 is the
  counting clerk's transaction too, from memory), human-only
  (`x-agent: false`), by item number (422 `inventory.unknown_item`,
  `inventory.duplicate_item`). Each item's count is kept as an **earlier
  count** (`inventory.count_item_recount`, migration 000490), numbered
  from 1 among the item's counts, with what it counted in the base unit
  and as entered, when and by whom, and when and by whom it was sent for
  the recount; the item is then open to be counted again through
  `:enterCount`, its `count_no` one more, and the document is `counting`
  until every item is counted (from `counted`, the one way back the
  status flow allows, and only while an item is uncounted). Only the
  latest count is posted: `:postDifferences` reads the item's count as
  before, and an earlier count is never posted.
- **When.** An item is recounted while its document is `counting` or
  `counted` and the item is counted (409 `inventory.item_not_counted`
  otherwise). A document `pending_approval` is not recounted (409
  `inventory.count_invalid_status`, as `posted`, `rejected`,
  `withdrawn`): its approval request names the value of the counts it was
  asked for, so the approver rejects and a new document counts again, as
  S2d has it; a recount asked while differences wait for approval
  (withdrawing the request) is in the remainder. A posted item is not
  recounted (409).
- **At most 99 counts** of an item (98 recounts, 409
  `inventory.recount_limit`; `count_item_recounts_check`): SAP sets no
  limit that we know of (from memory); a bound keeps a document's history
  finite.
- **The database holds the history** (03 §3.35): an earlier count is
  insert-only and is the open, unposted item's count as it is, numbered
  after the item's earlier ones (`count_item_recount_check`); a count is
  taken off an item only with its recounts one more and that earlier
  count written (`count_item_once`, widened); a counted document goes back
  to `counting` only while an item of it is uncounted
  (`count_document_status_flow`, widened); a count's name goes with the
  count (`count_item_counted_by_name_check`, `count_item_once`); the down
  migration refuses while an earlier count or a count's name exists. Schema tests `TestRecountRules`,
  `TestRecountTriggersAsOwner` (as the owner), `TestRecountDownRefused`
  (as a migrating role held to RLS).
- **Locks.** The count document's row (`FOR UPDATE`), which `:enterCount`
  and `:postDifferences` take first, then its items; the earlier count's
  foreign key takes the item's key share, which the item's own update
  (no key column) does not conflict with. A recount and a posting at once
  are ordered by the document's lock and the second refused at the
  version it read (`TestRecountAndPostAtOnce`).
- **Read.** Each item gives `count_no` and `earlier_counts` (in count
  order, empty when never recounted), so the document shows each item's
  counts in order: its earlier counts, then its latest
  (`counted_quantity`). **Who** counted and who sent an item back are
  given by id and by name as named then (`counted_by_name`,
  `recounted_by_name`, read through the new `People` port, the app's
  `LedgerPeople`, when the count or recount is written, as sales' credit
  release and shipping's trade compliance release keep theirs): a count
  entered from this version on names its counter, an earlier one has no
  name. The audit record and the event carry the ids alone (ADR-0079, PO
  2026-11-05).
- **Audited and published.** `inventory.count_document.recount` names
  the document, each item's count number and the counts set aside
  (`recounted`). Unlike the count's other steps, a recount publishes
  **PhysicalInventoryRecountRequested** (asyncapi, inventory channel):
  whoever counts again (a counting device or a warehouse system) learns
  that an item is open again; it names the document, its company, plant
  and storage location, its status and version, and each item, in the
  request's order, with its material and the count set aside, by person
  id, never a name. Not
  offered to webhooks until a consumer asks.
- **Agents.** `recountPhysicalInventoryItems` is on the never lists of
  the count planning and demand forecasting agents (ADR-0072 As built
  S2e, S2m). The counts due (S2c2) and the count planning agent's last
  count read posted documents only, so a recount changes neither.
- **Personal data** (batch 96S integration): the three names as named
  then, `count_item.counted_by_name`, `count_item_recount.counted_by_name`
  and `recounted_by_name`, are entries of the personal data inventory
  (ADR-0079 §9, `internal/records`), counted for the principal they name
  (`counted_by`, `recounted_by`), as sales' and shipping's names as
  released are; records' `TestInventoryCoversTheDatabase` refused the
  lane's tip without them. No deletion run blanks a principal's name
  (T100c blanks business partners'), so they stay with the count as the
  others stay with theirs.
- Walkthrough step 141, `step_count`, extended: Wade's first count of the
  brackets is sent for a recount by Ines, recounted, both counts shown in
  order, the latest posted; a posted document's item is not recounted.

**Later** (the leaf's remainder): a recount by a new document linked to
the first (SAP MI11's), a recount while differences wait for approval,
difference reasons, approval tiers by value, a tolerance per item and
per user group, partial posting of a document's items.

**S2b1** (ADR-0083, lane ship, 2026-11-19): movement types `601` (goods
issue for a delivery, posted by shipping through
`Endpoints.IssueForDelivery`, its reference the outbound delivery's
number) and `602` (its reversal, in the database's rules, posted by no one
yet); a valuation class's `cost_of_goods_sold` account (SAP GBB VAX, from
memory), an expense, which a 601's value posts against. `Endpoints.Held`
reads a location's stock without a lock for shipping's picking.

### S2h (2026-11-20)

- Migrations 000418 and 000419: the movement type `351` and its
  `reference` (widened NOT VALID, validated by 000419);
  `inventory.movement.in_transit` and `inventory.transit_stock`, which
  `movement_stock` adds a line in transit to instead of the location's
  stock; `count_posting_block` only on lines not in transit;
  `procurement.stock_transport_order` and `_item`; the permissions
  `procurement.stock_transport.read`, `.write` and `.post`. Rules of 03
  §3.8 and §3.35 under schema tests (`db/stock_transport_test.go`, the
  order's trigger also as the owner, the down's refusal).
- **The order is procurement's** (the plan's "order type in procurement";
  SAP's purchasing document type UB, from memory), a table of its own
  rather than a purchase order: it has no supplier, prices, approval or
  GR/IR. It is numbered from the company's own `stock_transport_order`
  range (fixture `TO`; no country pack ships one yet, the walkthrough's
  pilot settings add Maple's), as each new document type is. Decided by
  default.
- **Checked by inventory as its goods issue will be** (`Endpoints.
  TransportQuantities`, published beside `IssueForTransport` and
  `ReceiveForTransport`): both plants active plants of one company the
  caller may act in, the material active, stocked, extended to both and
  kept at both storage locations, its quantity exact in its base unit; a
  material once per order. The stock and valuations are checked only at
  the goods issue.
- **Goods issue (351)**, through `IssueForTransport` in the order's
  transaction, the order's row locked first (nothing locks valuations or
  stock before an order, so no cycle; `TestStockTransportsInOppositeOrder`):
  each item is a transfer between plants (S1c's 301) whose receiving line
  is in transit to the receiving storage location: valued at the
  receiving plant at what the issue took (at a standard price its
  quantity at the price, the rest a price difference), posted as `WA`
  between the plants' stock accounts, so the receiving plant's valuation
  holds the stock in transit (SAP's 351 within a company values it in the
  receiving plant, from memory: for the specialist). A count of the
  supplying location blocks the issue; one of the receiving location
  does not (no count blocks stock in transit).
- **Goods receipt (101)**, through `ReceiveForTransport`: each item's
  quantity leaves the stock in transit (409 `inventory.insufficient_stock`
  when less is in transit, `transit_stock_not_negative` beneath) and
  enters the receiving storage location, a transfer within the receiving
  plant's valuation (as a 311), so nothing is posted. Its movement type is
  SAP's 101 (from memory), named "Goods receipt for a stock transport
  order" on the document, which lines in transit tell from a purchase
  order's 101. A material blocked since the issue is still received (its
  stock is the company's already); a count of the receiving location
  blocks it.
- **The base quantity is fixed at creation** (review round 1): the order
  keeps each item's quantity in the base unit as converted then, and its
  goods issue and receipt move exactly that (`TransportItem.BaseQuantity`),
  the lines showing the unit ordered: an alternative unit's factor a
  material change sets or removes since moves nothing else, so the
  receipt takes out of transit what the issue put in
  (`TestStockTransportUnitChanged`).
- **The order moves whole** (`open`, `in_transit`, `received`), one goods
  issue of every item and then one goods receipt, each step under the
  order's row lock and `If-Match`, guarded by the status and version read
  (409 `procurement.stock_transport_issued`,
  `procurement.stock_transport_not_in_transit`). Neither document is
  reversed (409 `inventory.reverse_stock_transport`); neither is posted by
  `POST /v1/material-documents` (422 `inventory.unknown_movement_type`).
  Audited at each step (`procurement.stock_transport_order.create`,
  `.goods_issue`, `.goods_receipt`); no event of the order, as for count
  documents: its stock moves by the material documents, which publish
  `MaterialDocumentPosted` with each line's `in_transit` (not required,
  ADR-0041). Writes are human-only (`x-agent: false`); no agent's
  never-list names them, as no agent posts goods movements.
- **Stock in transit is no location's stock**: `GET /v1/stock` (now and
  as at a day) leaves its lines out; the order's items answer what is in
  transit (`in_transit_quantity`), and a material document's lines which
  are in transit. A document's `reference` finds its order through
  `GET /v1/stock-transport-orders?number=` (review round 1).
- Walkthrough step 147, `step_stock_transport`.
- **Not built** (Later, plan §S2h): partial issues and receipts,
  reversals (352, 102), a delivery between the
  plants, a list of stock in transit, intercompany transfers (S2i).
- **Cancellation** (batch 93, 2026-11-23; review:batch92): an order
  entered by mistake was open for ever, a planning run counting it as the
  supplying plant's demand and the receiving plant's receipt (ADR-0084 As
  built, stock transport orders). Decided by default, from SAP's deletion
  indicator on a stock transport order's items before their goods issue
  (from memory, for the SAP specialist), and from how a purchase order is
  cancelled (`cancelPurchaseOrder`):
  - `POST /v1/stock-transport-orders/{id}:cancel` cancels an `open`
    order, the whole order (it moves whole, so items are not cancelled
    one by one), at the version read (`If-Match`), under its row lock;
    the update is guarded by the status and version read. An order whose
    goods issue is posted, `in_transit` or `received`, is refused (409
    `procurement.stock_transport_issued`, its stock having moved; undoing
    it is the reversal that is Later), and one already cancelled too (409
    `procurement.stock_transport_cancelled`), which also refuses its goods
    issue; its receipt is refused as for any order not in transit.
  - `cancelled` is final: migrations 000465 (the status and issue checks
    widened NOT VALID, the order's trigger allowing open to cancelled
    alone) and 000466 (validated), under schema tests
    (`db/stock_transport_test.go`, the trigger also as the owner, the
    down's refusal of cancelled orders with row security off). No reason
    or date columns: the audit record and `updated_at`/`updated_by` say
    who cancelled it and when, as for a purchase order.
  - It moves no stock and posts nothing. Planning stops counting it with
    no change of its own: `Orders.TransportsDue`, `TransportsOnOrder` and
    `TransportReceipts` read the orders by status (`TestStockTransportCancel`,
    `TestPlanningRunTransports`, `TestSopSupply`).
  - The permission is `procurement.stock_transport.write`, the one that
    creates the order (its description says so now), as
    `cancelPurchaseOrder` takes the purchase order's create permission.
    Human-only (`x-agent: false`), as the order's other writes; the
    demand forecasting and replenishment agents' never lists name it
    (the replenishment agent's names `createStockTransportOrder` too:
    an order to another plant is supply a run counts, so its `TestNever`
    holds every write named `StockTransportOrder`).
  - Audited (`procurement.stock_transport_order.cancel`) and published as
    `StockTransportOrderCancelled` (procurement, offered to webhooks: no
    personal data), naming the order by id and number, its company,
    plants and items' materials by id and by code or number, the quantity
    ordered and in the base unit. The goods issue and receipt still
    publish no event of the order: their material documents do.
  - Walkthrough: `step_stock_transport` (step 147) refuses cancelling the
    issued order; `step_mrp` (step 152) cancels an order of 30 gaskets
    entered by mistake, refuses cancelling it again, and the runs count
    only the open order of 80.

### S2i (2026-11-21)

Decided by default from SAP's cross-company stock transport order (from
memory, for the specialist) and plan §S2i, which splits it into S2i1
(built), S2i2 and S2i3 (tracker leaves).

- **The order is the receiving company's purchase order** naming the
  plant that supplies it (`supplying_plant_id`; SAP's purchasing
  document with a supplying plant, EKKO-RESWK, type NB between
  companies). S2h kept its order out of purchase orders because it has no
  supplier, prices, approval or GR/IR; between companies the order has
  all four, so it is a purchase order: numbered, released (and approved
  by the release strategy) and received as any, its GR/IR positions the
  ledger's per order item. Migration 000443 adds the supplying plant, set
  at creation, and the goods issue's material document, number and day,
  under `purchase_order_issue` and the trigger `purchase_order_supply_fixed`
  (schema tests in `db/intercompany_transport_test.go`, the trigger also
  as the owner, the down's refusal); 000444 validates the widened checks.
- **Partners.** The order's supplier is the business partner whose group
  company (ADR-0059 §4) is the supplying plant's company (422
  `procurement.intercompany_supplier`; `partner.Supplier.GroupCompanyID`
  added to the published read). SAP assigns that vendor to the supplying
  plant (LFA1-WERKS, from memory); here the group company already says
  which partner a company is. The supplying plant is an active plant of
  another company (422 `procurement.supplying_plant_invalid`,
  `procurement.supplying_plant_own_company`, which points to S2h's order).
  The supplying company bills the business partner that is the receiving
  company (S2i2).
- **Transfer price** is the order item's price in the order's currency,
  entered or from a contract as on any order. S2i2's billing bills it,
  so the billing's price is the order's and S2i3's invoice matches the
  receipt. SAP prices the billing by the supplying side's own conditions
  (PI01, billing type IV, from memory), which can differ: Later.
- **Items are stock items** (422 `procurement.intercompany_item_not_stock`)
  **without an over-delivery tolerance** (422
  `procurement.intercompany_over_delivery`; the company's default is not
  applied): the receipt takes what the issue issued.
- **Goods issue (643)**, `POST /v1/purchase-orders/{id}:postIntercompanyGoodsIssue`
  (`procurement.stock_transport.post`, human-only): a released order's
  every item, its ordered quantity, from the storage location of the
  supplying plant the request names per item (422
  `procurement.issue_items`), once (409 `procurement.intercompany_issued`;
  409 `procurement.not_intercompany` for another order), under the
  order's row lock at the version read, through inventory's
  `IssueForIntercompany` in the request's transaction: checked, valued
  and posted as an outbound delivery's 601 is (one helper,
  `issueAgainstCost`), in the supplying company, WA Dr cost of goods sold,
  Cr stock (SAP's 643, from memory). The caller acts in both companies:
  the order's and, as inventory checks, the supplying plant's. Audited
  (`procurement.purchase_order.goods_issue`); no event of the order, as
  for S2h: the stock moves are MaterialDocumentPosted. Not reversed (409
  `inventory.reverse_stock_transport`), nor posted by
  `POST /v1/material-documents` (422 `inventory.unknown_movement_type`).
- **Goods receipt** is the order's 101 (S1e), at the order price against
  GR/IR, refused before the issue (409 `procurement.intercompany_not_issued`)
  and beyond it (409 `procurement.over_delivery`).
- **In transit** the goods are in neither company's stock or valuation:
  the issue took them out of the supplying company's books and the
  receipt puts them into the receiving company's (SAP's two-step
  transfer between companies without stock in transit valuation, from
  memory). S2h's `inventory.transit_stock` is not used: it is valued in
  the receiving plant, which between companies would book the receiving
  company's stock before its receipt. Cancelling or closing an order
  whose issue is posted and not all received is refused (409
  `procurement.order_in_transit`); a released intercompany order is not
  changed (409 `procurement.intercompany_order_change`), as its issue
  issues the ordered quantities.
- **A return (122) is refused** as for every stock item (422
  `procurement.stock_item_return`, S1e): it would take the goods out of
  the receiving company with nothing bringing them back to the supplying
  one. The receipt's own reversal (102) puts them back in transit.
- **The receipt's posting date is the receiving company's**, as any
  receipt's: it is not held to the issue's day, which is the supplying
  company's in its own time zone (decided by default: each company books
  its own document on its own day).
- **The supplying company's work list**: `GET /v1/purchase-orders`
  filters by `supplying_plant_id` and by `issued` (review round 1); the
  material document's `reference` names the order's number, in the API
  and in MaterialDocumentPosted, whose movement type lists `643`.
- Walkthrough step 155, `step_intercompany_transport` (after
  `step_count_agent`, before the close).
- **Not built** (Later, plan §S2i): partial and repeated issues, a
  delivery between the companies, the 643's reversal (644), changing a
  released intercompany order, the supplying side's own pricing,
  one-step transfers (645), stock in transit valuation and its list,
  customs (S2s).

### S2i2 (2026-11-22)

Intercompany billing, decided by default from SAP's intercompany billing
of a cross-company stock transport order (billing type IV, from memory,
for the specialist) and S2i's decisions above.

- **A document of its own**, `sales.intercompany_billing` with its items
  (migration 000448, 03 §3.29; schema tests in
  `db/intercompany_billing_test.go`, the insert-only triggers also as the
  owner, the down's refusal). A sales billing document bills a sales
  order's delivered items, which its keys, triggers and readers
  (returns, credit memo requests, revenue) rest on; a goods issue of a
  purchase order has no sales order. Its number is of the supplying
  company's `billing_document` range, as SAP numbers both kinds alike.
- **Billed and posted at once**: `POST /v1/intercompany-billings`
  (`sales.billing.post`, human-only) enters and posts the customer
  invoice through receivables in the request's transaction and writes the
  billing with it, or nothing (SAP releases an IV billing to accounting
  as it is created, from memory). There is no `created` state and the
  billing never changes; cancelling it, a credit memo of it and the
  receiving side's reversal are Later.
- **Once per goods issue**: an order whose goods issue is not posted is
  refused (409 `sales.intercompany_not_issued`), one billed already too
  (409 `sales.intercompany_billed`, read first and, for two racing, the
  unique key `intercompany_billing_once`). An order naming no supplying
  plant is no intercompany one (422 `sales.not_intercompany`).
- **Who bills**: the caller acts in both companies, as for the goods
  issue: the order is read through procurement's published
  `Orders.Issued` (wired by the composition root, as sales does not
  import procurement) for a caller who may act in its company, and the
  supplying plant's company is one the caller acts in too (422
  `request.reference_not_found` naming `purchase_order_id` otherwise).
  Either company's people read the billing (`GET`, the list filtered by
  either company and by order): it is the supplying company's bill and
  the receiving company's invoice to enter (S2i3).
- **Locks**: `Orders.Issued` reads the order `FOR SHARE`, so no change of
  the order (each takes its row `FOR UPDATE`) runs beside the billing;
  the goods issue is fixed already (`purchase_order_supply_fixed`).
  Beyond it the billing takes what a billing document's post takes in
  the supplying company (its number ranges, the invoice, the ledger's
  posting); the receiving company's goods receipt of the order takes the
  order and the receiving company's, so the two meet only on the order's
  row, which the billing never waits for while holding the other's.
- **Customer**: named by the request and checked as S2i1 checks the
  order's supplier: the business partner whose group company is the
  order's company (422 `sales.intercompany_customer`;
  `partner.Customer.GroupCompanyID` added to the published read), set up
  as a customer in the supplying company (receivables' check, 422
  `receivables.customer_not_usable`). Its payment term is the
  customer's in the supplying company.
- **Prices and amounts**: every order item once (422
  `sales.intercompany_items`), its ordered quantity, which the goods
  issue issued, at the order's price; its amount is the order item's
  value as procurement computes it (quantity × price ÷ price unit,
  rounded half away from zero; a stored value out of range fails
  closed), so the billing's net is the order's, the transfer price. An
  intercompany order's item is valued above zero (422
  `procurement.intercompany_item_value` when the order is created or
  changed, review round 1): receivables refuses a customer invoice item
  of zero, and every item of the goods issue is billed, so an order with
  such an item could otherwise never be billed. The
  billing's currency is the order's; the supplying company books it at
  its average rate as any foreign-currency invoice.
- **Account and tax**: each item names the supplying company's revenue
  account and, optionally, its output tax code and jurisdiction, as a
  sales item names them (SAP determines them by account determination,
  key ERL, and the IV conditions, from memory: Later with the supplying
  side's own pricing). An item whose code levies the UAE's emirates and
  that names none takes the supplying plant's region (the seller's side,
  ADR-0049 §11, the rule a sales item's default follows, one helper
  `defaultedTo`). Receivables calculates the tax at the billing date's
  rates and refuses what it refuses for any invoice, named by the
  request's own fields (`asPosting`). Each item keeps the tax its
  invoice item bore, for S2i3.
- **Dates**: the billing date is the one given, else the supplying
  company's date today, never before the goods issue (422
  `sales.billing_before_issue`); it is the invoice and tax date. The
  posting date is the one given, else that company's date today.
- **Records**: audited (`sales.intercompany_billing.create`); no event of
  its own, the invoice publishing CustomerInvoiceChanged and
  CustomerInvoicePosted.
- **The invoice's source**: a customer invoice's `source_document_id` may
  now name an intercompany billing; the contract says so beside the
  billing document it named alone (review round 1). Both are numbered
  from the `billing_document` range, so the id tells them apart.
- Walkthrough step 155, `step_intercompany_transport` (its
  `intercompany_billing` part): Maple Gulf Trading bills Maple the
  lamps it issued, CAD 320.00 zero-rated (Z0), Dr 1200 Cr 4000, once.
- **Not built** (Later): cancelling or crediting a billing, billing a
  partial or repeated issue (with them, plan §S2i), the supplying side's
  own pricing and account determination, an outgoing e-invoice of a UAE
  company's intercompany billing as a step of its own.

### S2i3 (2026-11-22)

The receiving company's supplier invoice from the intercompany billing,
decided by default from SAP's invoice receipt of an intercompany billing
(the IV billing's invoice entered in the receiving company, by EDI or
MIRO against the stock transport order's receipt, from memory, for the
specialist) and S2i's and S2i2's decisions above.

- **Entered from the billing**:
  `POST /v1/intercompany-billings/{id}/supplier-invoices`
  (`enterIntercompanySupplierInvoice`, `payables.invoice.capture`,
  human-only as the goods issue and the billing are): the billing's
  sub-collection, a plain create answering 201 with the invoice (a
  collection custom method would start a job and answer 202, 04 §4.8);
  payables reads the billing through its port `IntercompanyBillings`,
  which the composition root wires to sales' `GetIntercompany` (payables
  does not import sales). A billing billed to a company the caller may
  not act in, or none, is 404, so ids cannot be probed: the supplying
  company's people do not enter the receiving company's invoice.
- **As capture reads a document**: the billing states what a document
  prints, so it goes through capture's code (`capturedInput`,
  `capturedTax`, `capturedExtensions`) rather than a second mapping: the
  order (by its number in the receiving company) gives the supplier, the
  business partner that is the supplying company (S2i1), and the items,
  each order item once at its quantity and the billing item's net; the
  supplier reference is the billing's number, the invoice date the billing
  date, the currency, gross and tax the billing's. Source `edi`, as the
  brief's design asked (an electronic document from the other company);
  the invoice names the billing (`intercompany_billing_id`).
- **Tax**: each item keeps its order item's input code where it charges
  the rate the billing item's output code charges in the supplying
  company on the billing date; else the one input code of the receiving
  company charging it is proposed, and with none or several the invoice
  is held naming the line, as for a captured document (T61d). An item
  billed without a code, or at 0%, whose order item has none stays
  without one: nothing is charged and nothing is recovered (decided by
  default: a 0% line would otherwise hold every zero-rated or exempt
  billing of a company without a 0% input code, as Maple has none); one
  billed without a code whose order item has one states 0%, so that code
  stays only where it charges nothing (review round 1). A
  code the supplying company no longer has on that date holds it, naming
  the line. A stated tax the codes do not bear out holds it with
  `payables.tax_mismatch`'s figures. Whether the receiving company owes
  or recovers tax of its own on the supply under its country's rules (a
  cross-border one, say) is not decided here: the person applies it while
  the invoice is parked; a rule of it is Later, for the specialist.
- **Parked or held, for a person to post**: matched against the order's
  receipts as any order invoice (`:match`, three-way), and posted
  (`:post`) it debits GR/IR at the order price, which is the billing's,
  so the order's GR/IR clears and the receiving company's entry is the
  billing's gross; nothing posts automatically.
- **Once per billing** (409 `payables.intercompany_billing_entered`
  naming the invoice, read first; for two racing the key
  `supplier_invoice_billing_once`): a rejected, cancelled or reversed
  invoice frees the billing, as `supplier_invoice_no_duplicates` frees a
  reference. The key holds whatever the parked invoice's supplier
  reference is changed to, which the duplicate reference check alone
  would not. Migrations 000455 (the column and its check, `edi` alone,
  added NOT VALID), 000456 (validated), 000457 (the key, built
  concurrently); schema tests in `db/supplier_invoice_billing_test.go`,
  the down's refusal too.
- **Locks**: the billing is insert-only and read without a lock; the
  invoice takes what any invoice entry takes (its number range, the
  order's read); the key's wait orders two entries of one billing.
- **Records**: audited (`payables.supplier_invoice.create`) and
  published (SupplierInvoiceCaptured, SupplierInvoiceHeld when held) as
  any entry. No agent's scope covers supplier invoices, so no never list
  changes; the operation is `x-agent: false`.
- Walkthrough step 155, `step_intercompany_transport` (its
  `intercompany_invoice` part): Alex enters Maple Gulf Trading's billing
  as Maple's invoice, CAD 320.00 without tax, once; matched and posted,
  Dr GR/IR, Cr the supplier, the order's GR/IR 0.00.
- **Not built** (Later): entering it automatically when the billing
  posts, a rule of the receiving company's own tax on the supply, a credit memo
  from a cancelled or credited billing (S2i2's Later).

### S2f (2026-11-20)

- Migrations 000428 and 000429 (03 §3.35, S2f): `materials.material.batch_managed`,
  `inventory.batch`, `inventory.batch_stock`, `inventory.movement.batch_id`
  and the stock rows' `batch_managed`, under schema tests
  (`db/batch_test.go`, the batch's trigger also as the owner, the down's
  refusal); the permission `inventory.batch.write` (reads under
  `inventory.stock.read`).
- **Batch management is the material's, fixed once created**
  (`batch_managed` on `POST /v1/materials`, SAP MARA's XCHPF, from
  memory), as its base unit is (§7): SAP refuses changing it once stock
  exists; here it never changes, the safer default. Only a material kept
  in stock is batch-managed (422 `materials.batch_managed_not_stocked`).
  It is on the material's create change for the approver and in
  `MaterialChanged` (not required, ADR-0041). Plant-level batch
  management (MARC's XCHAR) is Later. Decided by default.
- **A batch is unique per material** (SAP's batch level "material", from
  memory, rather than its default "plant"), so a transfer between plants
  keeps its batch. `POST /v1/batches` (SAP MSC1N, human-only): its number
  given by the request (a supplier's lot or the plant's; internal
  numbering from a range is Later), 1 to 10 characters (CHARG, from
  memory), its manufacture date, its expiry date optional (a lot that
  does not expire) and never before it (422 `inventory.batch_dates`); the
  material not pending (422 `inventory.material_not_usable`) and
  batch-managed (422 `inventory.material_not_batch_managed`); a number
  again 409 `inventory.batch_exists`. A batch is the tenant's, as its
  material is, and does not change (dates and batch status Later).
  Audited (`inventory.batch.create`); no event, as a batch moves no stock.
  `GET /v1/batches` (material, number) and `/{id}`.
- **Every movement of a batch-managed material names a batch of it**:
  `POST /v1/material-documents` items take `batch_id` (422
  `inventory.batch_required`, `inventory.batch_invalid`, and
  `inventory.batch_not_allowed` for another material); a transfer (311,
  301) receives the same batch (a transfer to another batch, SAP's 309,
  is Later); a reversal moves its original's batches. The lines and
  `MaterialDocumentPosted` (not required) name the batch by id and number.
  **The flows that carry no batch yet refuse a batch-managed material**
  at the flow's first step, so no document is created that cannot
  complete (review round 1): a purchase order's stock item (422
  `procurement.invalid_stock_item`, as it is created or changed), a new
  quotation or sales order item (422 `sales.material_not_usable`; review
  round 2) and, beneath it, an outbound delivery's item (422
  `shipping.material_not_usable`), a stock
  transport order (at its creation, `TransportQuantities`) and a count
  document (at its creation; both 422 `inventory.batch_not_supported`,
  which the goods receipt and goods issue also answer beneath), and the
  SAP stock take-over (561 without a batch, `inventory.batch_required`),
  each Later; a batch-managed material is created once no pre-S2f
  version serves (a stock row an earlier version writes is not
  batch-managed); the plan's slice names the
  batch on movements and stock, which the person's goods movements
  carry. Decided by default. S2f1 lifted the purchase order's refusal,
  and S2f2 the quotation's, the sales order's and the delivery's: a
  delivery's goods issue (601) and its reversal (602) carry the batch
  (ADR-0083 As built S2f2).
- **Stock by batch beside the location's**, not a wider stock key:
  `inventory.batch_stock` (SAP MCHB beside MARD), rather than the plan's
  "stock key extended, expand-migrate-contract", so `inventory.stock` and
  every reader of it (`GET /v1/stock`, `Held`, the counts, ATP's read)
  stay as they are. The database adds a batch's movement to both rows
  and refuses a movement whose batch does not match its stock row's
  `batch_managed`, so the batches at a location add up to its stock
  (`movement_batch`). The stock row carries the flag, written from the
  material by the application as it writes the row, so the database
  needs no read of the materials schema (02 §2.1).
- **Locks:** a posting locks the valuations, then the locations' stock
  rows, then the batch stock rows it moves (material, plant, storage
  location, batch, as PostgreSQL orders uuids), writing an empty row for
  a batch's first movement there, then checks each batch never goes below
  zero in line order (409 `inventory.insufficient_stock` naming the
  batch; `batch_stock_not_negative` beneath). The location's lock comes
  first, so two postings of one location serialize as before
  (`TestBatchIssuesAtOnce`).
- `GET /v1/batch-stock` (material, plant, storage location, batch): each
  batch's stock at a location now, with its dates; as at a date is Later.
- Walkthrough step 149, `step_batches`.
- **Not built** (Later, plan §S2f): batches in the purchase order's
  goods receipt, the delivery's goods issue, stock transport orders,
  counts and the SAP take-over; a batch created at goods receipt; internal
  batch numbers; changing a batch's dates; batch status (restricted
  stock); stock by batch as at a date; transfer to another batch (309);
  expiry checks and batch determination (FEFO), classification,
  where-used and recall.

### S2g (2026-11-21)

- Migration 000435 (03 §3.35, S2g): `materials.material.serial_managed`,
  the stock rows' `serial_managed`, `inventory.serial` and
  `inventory.movement_serial` with their triggers, under schema tests
  (`db/serial_test.go`, the insert-only triggers also as the owner, the
  down's refusal). No permission: serial numbers are moved by the goods
  movements that name them (`inventory.movement.post`) and read with the
  stock (`inventory.stock.read`).
- **A serial number profile is the material's, fixed once created**
  (`serial_managed` on `POST /v1/materials`; SAP keeps the profile per
  plant, MARC's SERNP, from memory), as batch management is (As built
  S2f): one profile, "a serial number for every unit on every goods
  movement" (SAP's serializing procedure MMSL with serial number usage
  obligatory, from memory); other usages and per-plant profiles are
  Later. Only a material kept in stock has one (422
  `materials.serial_managed_not_stocked`). On the create change and in
  `MaterialChanged` (not required, ADR-0041). A material may be both
  batch-managed and serial-managed; each rule applies on its own.
  Decided by default.
- **A serial number is unique per material** (SAP's serial number per
  material, from memory): 1 to 18 capitals, digits, `-` or `_` (SERNR's
  length, from memory). **The first receipt that names one creates it**
  (SAP MIGO's automatic creation, from memory), so there is no serial
  number master endpoint to keep apart from stock; it is never deleted.
  Decided by default.
- **Every goods movement of a serial-managed material names the serial
  number of each unit it moves**: `serial_numbers` on the items of `POST
  /v1/material-documents`, the item's quantity a whole number of base
  units (422 `inventory.serial_quantity`) and its serial numbers as many
  (422 `inventory.serial_required`), each once in the document (422
  `inventory.serial_repeated`), another material's none (422
  `inventory.serial_not_allowed`); a transfer (311, 301) receives the same
  serial numbers, a reversal moves its original's back. The lines, the
  audit and `MaterialDocumentPosted` (not required) name them, sorted by
  number.
- **A serial number is in stock at most once, database-enforced**:
  `inventory.serial` holds where it is now (a plant's storage location, or
  none); only `inventory.movement_serial`, its history, moves it
  (`movement_serial_moves`: a receipt of one in stock is refused,
  `serial_in_stock_once`; an issue of one not at the line's location,
  `serial_issued_from_stock`); at commit a line of a serial-managed stock
  row names one per unit and any other line none (`movement_serials` on
  the line, `movement_serial_counted` on each serial number added to a
  line written before, so one added later to a committed line is refused
  too; each line is counted once at commit, review rounds 1 and 2;
  `TestSerialManyUnits` posts an item of 1000).
  The posting checks both first and answers 409
  `inventory.serial_in_stock` (naming the serial number and where to read
  it) and `inventory.serial_not_in_stock` (naming the location).
- **Locks:** after the locations' and the batches' stock rows, a posting
  writes the serial numbers its receipts name for the first time (`ON
  CONFLICT DO NOTHING`) and then locks every serial number it moves, both
  in (material, number) order, bytewise as PostgreSQL's C collation orders
  them; every write precedes every lock, and `FOR UPDATE` does not wait on
  a row not yet committed, so two postings never wait on each other in a
  cycle (`TestSerialMovementsAtOnce`: receipts at two locations, so under
  no common stock row, of the same serial numbers named in opposite
  order, issued ones and new ones, post one each time; review round 1).
- **The flows that carry no serial numbers yet refuse a serial-managed
  material** where they refuse a batch-managed one: a purchase order's
  stock item (422 `procurement.invalid_stock_item`), an outbound
  delivery's item (422 `shipping.material_not_usable`), a stock transport
  order and a count document (422 `inventory.serial_not_supported`, which
  the goods receipt and goods issue also answer beneath) and the SAP stock
  take-over (561 without serial numbers, `inventory.serial_required`).
  A new quotation or sales order item of a serial-managed material is
  refused as a batch-managed one is (422 `sales.material_not_usable`
  naming `items[i].material_id`, `TestMaterialItemRefusals`), added
  beside S2f's refusal at batch 87's integration (the sales order's code
  was another lane's); the delivery's own refusal stays as a second
  guard. S2g2 lifted both, as S2f2 lifted a batch-managed material's: a
  delivery's goods issue names its units' serial numbers (ADR-0083 As
  built S2f2).
- `GET /v1/serial-numbers` (material, number, plant, storage location,
  in stock) and `/{id}` with its history (each movement's material
  document, movement type, location and whether it received or issued the
  unit), filtered by the company of the serial number's last movement
  and its movements' companies (SAP IQ09 and IQ03, from memory).
- Walkthrough step 151, `step_serial_numbers`.
- **Not built** (Later, plan §S2g): serial numbers in the purchase
  order's goods receipt, the delivery's goods issue, stock transport
  orders, counts and the SAP take-over; profiles per plant and other
  serial number usages (optional, automatic); equipment master data and
  serial numbers in maintenance and service (phase 6); a serial number's
  history as at a date.

### S2f1 and S2g1: batches and serial numbers in the purchase order's goods receipt (2026-11-24)

- Migrations 000473 and 000474 (03 §3.9): `procurement.goods_movement_item`'s
  `batch_id`, `batch_number` and `serial_numbers`, what the item's material
  document line moved (a reversal's item its original's), under schema
  tests (`db/goods_receipt_tracking_test.go`: a batch by id and number
  together, one to 1000 serial numbers, the down's refusal). The batch is
  inventory's, by id and number: no foreign key leaves a schema (02 §2.1).
- **A purchase order's stock item may be of a batch-managed or a
  serial-managed material** (S2f and S2g refused one at ordering); an
  intercompany stock transport order's still refuses it (422
  `procurement.invalid_stock_item`), as its goods issue (643) carries
  neither yet.
- **A batch-managed stock item's goods receipt names its batch by number**
  (`batch` on the items of `POST /v1/goods-receipts`; 422
  `inventory.batch_required`; another item's, 422
  `inventory.batch_not_allowed`, and on an item that is no stock item, 422
  `procurement.not_stock_item`). **A new number creates the batch at the
  receipt** (SAP MIGO creating a batch, from memory) with the manufacture
  date the item gives (422 `inventory.batch_manufacture_date_required`
  without one: the batch's date is the supplier's, never defaulted to the
  receipt's) and its optional expiry date (422 `inventory.batch_dates`
  before it), audited as `createBatch` is (`inventory.batch.create`), with
  no permission of its own: the receipt's (`procurement.receipt.post`)
  creates it, as a 501 creates a serial number. **Dates given for an
  existing batch are its own** (422 `inventory.batch_dates_differ`): a
  batch's dates do not change (Later, plan §S2f), so a receipt never
  changes them quietly; giving none names it by number alone. Decided by
  default.
- **A serial-managed stock item's goods receipt names the serial number of
  each unit** (`serial_numbers`), checked and created as a 501's are (As
  built S2g: 422 `inventory.serial_required`, `inventory.serial_quantity`,
  `inventory.serial_repeated`, `inventory.serial_not_allowed`; 409
  `inventory.serial_in_stock`).
- Movement types 101 and 102 carry batches and serial numbers (`tracked`);
  a stock transport order's receipt, its own kind, still does not. The
  goods receipt's items, `GoodsReceived` and `GoodsReceiptReversed` (not
  required) name the batch by id and number and the serial numbers, sorted;
  the material document's lines and `MaterialDocumentPosted` do as before.
  **The reversal (102) takes the batch's stock and the serial numbers back**
  through its original's lines (409 `inventory.insufficient_stock` naming
  the batch once it is issued, `inventory.serial_not_in_stock` once a unit
  is). A refusal of inventory's names the goods receipt's own item
  (`items[i]`), not the material document's, and `inventory.serial_repeated`'s
  detail names no item number (review round 1).
- **Locks:** a receipt reads the batches it names, then creates the new ones
  in one statement in (material, number) order, bytewise, `ON CONFLICT DO
  NOTHING`, so receipts creating the same batches named either way round
  never wait on each other in a cycle; a batch another receipt creates
  meanwhile is read once that one commits and its dates checked as an
  existing batch's (`TestReceiptBatchesAtOnce`); the re-read sees it because
  a goods receipt runs at READ COMMITTED (it is no snapshot read). A batch row is never
  updated or locked otherwise; the valuations, stock, batch stock and
  serial numbers are locked after it as every posting locks them.
- Walkthrough step 130, `step_stock_receipt`, extended.
- **Not built** (Later, plan §S2f, §S2g): a WMS-reported goods receipt
  naming a batch or serial numbers (it is refused with
  `inventory.batch_required` or `inventory.serial_required` and recorded
  so); batches and serial numbers in the delivery's goods issue, stock
  transport orders, counts and the SAP take-over; the other variants the
  two leaves name.

### S2j (2026-11-20)

- Migration 000430: `sales.sales_order.plant_id`,
  `sales.sales_order_item.confirmed_quantity` with their rules and
  `sales.delivery_assignment.plant_id` (the existing assignments take
  their delivery's) (03
  §3.16; schema tests in `db/sales_availability_test.go`, the trigger
  also as the owner).
- **The plant is the order's, optional** (decided by default): a sales
  order names the plant it delivers from (`plant_id` of
  `createSalesOrder`, an active plant of its company, 422
  `sales.plant_invalid`), with or without a quotation, kept as entered.
  SAP determines a delivering plant per item (VBAP-WERKS, from the
  customer-material info, the customer or the material, from memory);
  one plant per order is enough for the pilot's one plant per stock site,
  and per-item plants and their determination are Later. An order
  without a plant is not checked and is delivered as before, so
  existing orders, quotations and order capture are unchanged. What its
  outbound deliveries hold at a plant counts in that plant's
  availability (review round 1): each delivery assignment names the
  plant its delivery ships from (`ShipInput.PlantID`,
  `GoodsIssue.PlantID`, required; the column stays nullable until a
  contract step), and the check counts the held quantities of orders
  without a plant there, read from the items something is in delivery of
  (`sales_order_item_in_delivery_held`). Their confirmed but undelivered
  quantities are not counted, having no plant (for the product owner:
  whether a plant should be required for orders of stock materials;
  answered 2026-11-21, "S2j: the plant required" below).
- **What is available** at confirmation (`confirmSalesOrder`), for each
  item of a material kept in stock: the plant's stock at its storage
  locations in the base unit (`inventory.Endpoints.PlantStock`, a read
  published for this; stock in transit is not counted) less what the
  plant's other confirmed orders confirmed and have not delivered
  (`confirmed_quantity − delivered_quantity` of their standing items, at
  the material's factor now) and what deliveries from the plant hold of
  orders without a plant, read in one statement. What an order with a
  plant has in delivery is inside its confirmed quantity. Cancelled orders
  and removed items promise nothing. The items are confirmed in item
  order, each as far as is left, rounded down to six decimals in the
  item's unit: in full, in part or not at all; a shortage refuses
  nothing (SAP's one-time delivery confirms what is there, from memory).
  The check is of stock on hand, so it confirms the same for the
  requested delivery date as for today; nothing stores a confirmed date
  (checking rules with replenishment lead time are Later). An order
  blocked for credit keeps its confirmation, which counts.
- **Two orders racing for the last quantity confirm it once**: the check
  takes a transaction advisory lock per material and plant
  (`sales.availability:<tenant>:<plant>:<material>`, through
  `database.LockTenant`, the platform's helper since batch 86's
  integration), in the materials'
  uuid order, after the order's row lock and the credit check's customer
  lock, which every caller takes in that order; a goods issue takes none
  of them, so no cycle (`TestChangeAndConfirmationRace` proves a change
  takes the same lock). It reads the demand before the stock: a goods
  issue committed between the two reads counts out of the stock but
  still in the demand, so it promises less, never more
  (`TestTwoOrdersRaceForTheLastQuantity`, through
  `apitest.Serialized`). The reasoning holds while delivered quantities
  only rise: a goods issue reversal (602, Later) that lowers one must
  take the check's lock or count its stock first.
- **Deliveries take at most what is confirmed** (`OrderItem.open`): an
  item with a confirmed quantity has that, not its ordered quantity,
  open for delivery, less what is delivered and in delivery (422
  `sales.exceeds_open_quantity` names what is confirmed); the database
  keeps delivered and in delivery within it. A delivery of an order with
  a plant ships from that plant (422 `shipping.plant_mismatch`), so its
  goods issue takes the stock the check counted.
- **A change of a confirmed order checks again** the items it adds and
  those whose quantity it changes, in the same way: an item keeps what it
  had confirmed, up to its new quantity, and has more confirmed as far as
  the plant has it available, so a lowered quantity frees stock and a
  raised one is confirmed as stock arrives (decided by default; SAP
  rechecks a changed item, from memory). Re-confirming items no change
  touched is back-order processing, Later: until then an item confirmed
  short is confirmed again only by a change of its quantity (a change to
  the same quantity is `sales.nothing_changed`), for the product owner.
  A change that touches no stock item's quantity takes no lock and reads
  nothing. Confirmed quantities are rounded down to six decimals in the
  item's unit (`money.FloorQuantity`), so a part of a pack can be
  confirmed.
- **Not valued, nothing more promised**: a promise whose unit the
  material no longer has (a unit removed since) cannot be put in the
  base unit; the material then has nothing more available at the plant,
  and an item in such a unit keeps what it had (`unvalued` in its audit
  detail).
- Audited in the confirmation's and the change's records (`availability`:
  the plant and each item checked, with its material, quantity, unit and
  confirmed quantity before and after); no new event or permission:
  confirming stays `sales.order.confirm`, human-only.
- Walkthrough step 150, `step_available_to_promise`.
- **Not built** (Later, plan §S2j): checking rules with replenishment
  lead time (first slice S2j2, below) and planned receipts, back-order processing, product
  allocation, global ATP across plants, per-item plants.
- **Contract step** (lane salesfix, batch 92; leaf src review:batch86):
  migrations 000458 and 000459 make `sales.delivery_assignment.plant_id`
  NOT NULL, as 06 §6.4 adds a NOT NULL column: every server writes it
  since S2j (sales' `assign` refuses a delivery without a plant), so 000458
  gives an assignment that lacks it (one a server of the release before
  S2j wrote during the rolling upgrade) its delivery's plant, as 000430's
  backfill did, and adds `delivery_assignment_plant_required` NOT VALID;
  000459 validates it, sets the column NOT NULL (the validated check
  spares the scan) and drops the check. Decided by default: a backfill
  rather than a guard that stops the migration, since the delivery always
  names the plant and stopping the release would need a person to run the
  same update. `TestDeliveryAssignmentPlantRequired`,
  `TestDeliveryAssignmentRules` (an assignment without a plant) (db).
  `delivery_assignment_by_plant` keeps its `plant_id IS NOT NULL`
  predicate, which every row now meets.

### S2j: the plant required, and the emirate from it (2026-11-21)

The product owner's answer of 2026-11-21 to S2j's question above
(src `po:2026-11-21`): an order of stock materials names its plant.
- **The only plant by default**: an order created without `plant_id`
  delivers from its company's only active plant when the company has
  exactly one (`tenancy.Directory.OnlyPlant`; `plant_defaulted` in its
  audit record), whatever its items, so an item a change adds later has
  its plant too; with none or several it has none. Decided by default:
  the default does not wait for a stock item, since the plant of a
  one-plant company is not in doubt and an order of services alone is
  checked for nothing; an order from a quotation takes it as one entered
  directly does.
- **Confirming needs it**: `confirmSalesOrder` refuses an order without a
  plant that has a standing item of a material kept in stock (422
  `sales.plant_required` naming `plant_id`, the item and its material in
  the message), before the credit check and without a lock: its
  availability could not be checked. Service and non-stock items need
  none. Decided by default: the plant stays as entered (the column has no
  update grant, 000430), so such a draft is cancelled and created again
  with its plant; a plant set on a draft by a change is Later, with
  per-item plants and their determination from customer and material
  data (SAP's VBAP-WERKS, from memory; product owner: Later). A
  confirmed order without a plant (of services alone, or one confirmed
  before this step) takes no stock item unchecked: `changeSalesOrder`
  refuses one that adds a stock item or changes a stock item's quantity
  (422 `sales.plant_required`, review round 1).
- **A plant's region** (migration 000431, `tenancy.org_unit.region`;
  `org_unit_region`, `TestOrgUnitRegion`): the country subdivision of the
  plant's address (ISO 3166-2, a UAE plant's emirate), on
  `createOrgUnit`/`updateOrgUnit` for a plant only and of its company's
  country (422 `tenancy.invalid_region`), as SAP keeps a plant's region
  on its address (from memory). Nothing else had one: a plant had no
  address. A UAE sales item of a material kept in stock defaults its
  emirate to its order's plant's (ADR-0063 As built, the emirate's
  default); a change of the region changes no item entered before it.
- Walkthrough: step 148 (`step_einvoice_billing`) creates the UAE
  company's one plant, Sharjah's, and its stock item defaults its emirate
  there; step 150 (`step_available_to_promise`) has an order of the hinge
  without a plant refused on confirm in the Canadian company, which has
  two, and a UAE order without one delivering from its only plant.

### S2j2: checking with the replenishment lead time (2026-11-25)

The first item of the tracker's "later: ATP variants" (plan-supply-chain
§S2j2; migrations 000492, 000493). Decided by default, for the product
owner's review (SAP's check with the replenishment lead time, from
memory, for the SAP specialist):

- **The lead time** is the material's at the plant, on its planning data
  (`replenishment_lead_time_days` of `createMaterialPlan` and
  `updateMaterialPlan`, 1 to 999 calendar days, optional; why there:
  ADR-0084 As built, replenishment lead time). None, the default: the
  check counts the stock alone, as S2j. Calendar days, as the company's
  dates are; working-day calendars are Later.
- **When it counts**: for an order asked for on or after the company's
  date today (`config.DocumentDay`, the confirmation's or the change's
  day) plus the lead time. An order asked for sooner, or for no date
  (read as today, as SAP defaults the requested date, from memory), is
  checked against the stock alone: the plant could not replenish in time.
- **What it confirms**: such an item in full, the stock first, as far as
  S2j's check reaches, and the rest on the lead time (the alternative,
  an order beyond the lead time taking no stock so that nearer orders
  keep it, was not taken: the stock is then left idle while more is
  ordered, and SAP's cumulative ATP gives the stock to the first check,
  from memory). The item names that rest (`lead_time_quantity`, in its
  unit) and the day the lead time ends (`lead_time_date`). All it
  confirms counts as promised for later checks, so the stock is never
  promised twice: a later order asked for sooner gets what is left.
- **Checked again** (a change that adds an item or changes its quantity,
  S2j): an order asked for in time, as changed, has the item confirmed in
  full again, the rest on the lead time from the change's day; one asked
  for sooner keeps what it had confirmed (a promise is never taken back)
  with as much of it on the lead time as rested on it before, less the
  stock that arrived since, until the day it did; a check of stock alone
  never moves a promise onto the lead time. A change of the requested
  date alone checks nothing again (S2j); a recheck on it is Later, with
  confirmation on a later date than asked for (schedule lines at the lead
  time's end) and planned receipts in the check.
- **Locks**: unchanged. The lead times are read after the materials'
  locks (`database.LockTenant` `sales.availability`), under which the
  demand and stock are read; the goods issue reversal takes the same
  lock (S2b3). The planning data is read without a lock (ADR-0084 As
  built).
- **What a delivery takes**: at most what is confirmed (S2j); its goods
  issue still needs the stock (inventory refuses a shortage), so a part on
  the lead time is shipped once the stock has arrived.
- Audited in the confirmation's and the change's `availability` detail:
  each item of a material with a lead time names `lead_time_days`,
  `lead_time` (what rests on it now and until when) and
  `lead_time_before`. No event, permission or never-list change: no new
  operation.
- `TestConfirmOnTheLeadTime`, `TestJudged` (sales),
  `TestConfirmationOnTheLeadTime`, `TestLeadTimeOnAChange`,
  `TestLeadTimeDatabaseFailures` (shipping, on an injected clock),
  `TestLeadTimeRules` (db). Walkthrough step 150
  (`step_available_to_promise`, `lead_time_check`): a door latch with 20
  in stock and 10 days' lead time; an order of 50 asked for in two weeks
  is confirmed 50, 30 on the lead time; one of 5 asked for in three days
  is confirmed none.

### Planning (2026-11-21)

Material requirements planning (S2k onward) is the leaf context `mrp`,
decided by ADR-0084; it reads inventory's `PlantStock` and a new
`PlantPrices` and adds no direction to this ADR's §3.

### S2e (2026-11-21)

The count planning agent (ADR-0072 As built S2e, `internal/countagent`)
proposes the materials of a plant to count this period, for an inventory
controller to decide; it reads stock, materials, material documents and
count documents through this ADR's published reads only, adds no
direction to §3 and never creates a count document (x-agent: false).
The rule of a count due, decided by default from SAP's cycle counting
(the ABC analysis MIBC and the cycle counting indicator's intervals,
OMCO, from memory) and computed by the agent's run, not by the model:

- **The year** is the 365 days to the run's day, the company's date.
- **ABC class** by consumption value at the plant over the year (SAP's
  ABC indicator is a plant's, MARC-ABCIN): a material's consumption is
  the value of its issues to a cost center (201), scrappings (551) and
  deliveries' goods issues (601), less their reversals (202, 552, 602),
  as their lines' amounts give it, in the company's currency (an amount
  in another is refused, never added). Transfers, receipts and count
  differences are no consumption. In the order of consumption value,
  the greatest first, a material is A while the materials before it make
  up less than 80 percent of the plant's total, B while less than 95, C
  otherwise; one with no consumption is C.
- **Intervals**: A every 90 days, B every 180, C every 365 (four, two and
  one counts a year). A material is due at a storage location when it
  was never counted there (a rejected or withdrawn count is none) or its
  last posted count there is the interval or more before the day.
- **Candidates** are the materials in stock at a storage location of the
  plant (`listStock`) that a count document takes: `active` or `blocked`,
  neither batch- nor serial-managed (which counts refuse, As built S2f,
  S2g), and not held by an open count there (`counting`, `counted`,
  `pending_approval`, or a status this version does not know).
- **Movements** are the lines of the year's material documents (by
  posting date) moving the material at that storage location; lines in
  transit are not counted;
  they rank a class's materials, the most first, then the last count,
  never first, then the oldest.

**Later** (the plan's): materials with no stock counted, counts on a
calendar, counting by risk learned from differences, the tenant's own
shares of the ABC analysis. Cycle counting indicators on the plant data
are S2c2's, which the agent reads.

### S2c2 (2026-11-24)

Cycle counting indicators, the first item of the tracker's "later: count
variants" leaf (plan-supply-chain.md §S2c2), decided by default from
SAP's cycle counting (T159C, MARC-ABCIN and MICN, all from memory, for
the specialist):

- **Indicators are a plant's** (`inventory.cycle_count_indicator`, as
  SAP's T159C is keyed by plant): a code of one to four capitals or
  digits (A, B, C, D or the tenant's own; SAP's is one character) once at
  the plant, and an interval of 1 to 366 days: a material that carries
  one is counted at least once a year, the safer bound for an annual
  inventory by cycle counting. None is created for a plant by itself;
  the controller creates the plant's (SAP delivers A to D per plant in
  customizing, from memory). The interval changes at the indicator's
  version (`PATCH`, If-Match); the code and plant do not (column grants).
- **A material's indicator at a plant** (`inventory.cycle_count_material`,
  SAP MARC-ABCIN): one per material and plant, of that plant's
  indicators (a foreign key naming the plant with the indicator), changed
  to another of the plant at its version. The material is one a count
  document takes there (the checks are the count's, `refs.countable`):
  the tenant's, not pending, kept in stock, extended to the plant,
  neither batch- nor serial-managed. Taking an indicator off a material
  is Later, as removing a material's MRP data is (ADR-0084 §4).
- **The counts due** (`GET /v1/cycle-counts-due`, computed, not stored):
  on the plant's company's date today, each material with an indicator
  at each storage location of the plant where it has stock now (as
  `listStock` and the count planning agent read it), not held by an open
  count there, never counted there (a posted count: a rejected or
  withdrawn one is none, as S2e reads it) or last counted there (the
  latest posted count by its date) its interval or more days before
  today; `due_on` is that count's date plus the interval. Never counted
  first, then the longest due, paged by the day (never counted as
  `-infinity`) and the stock row. Creating a count is the normal
  `POST /v1/physical-inventory-documents` with a storage location's
  materials as listed (at most 100 a document); the count's open items
  take them off the list until it is posted.
- **The count planning agent reads them** (decided: an indicator the
  controller set governs over the class the run computes, as SAP's MICN
  plans by MARC-ABCIN): `listCycleCountMaterials` before its session; a
  material with an indicator at the plant is due by its interval, its
  line names the indicator, and it keeps its computed class for the
  rank; the others are as S2e. ADR-0072 As built S2e.
- **Permissions and agents.** `inventory.count_plan.write` maintains
  both (`x-agent: false`), on the never lists of the count planning and
  demand forecasting agents; reads are `inventory.stock.read`'s. Audited
  (`inventory.cycle_count_indicator.create`/`.change`,
  `inventory.cycle_count_material.create`/`.change`, the change with the
  values before and after); no event, as no other context reads them.
- **Locks.** A change takes its row `FOR NO KEY UPDATE`; a material given
  an indicator, or moved to one, takes the indicator's key share through
  the foreign key, which that leaves free: no writer waits for another
  (`TestCycleCountLocks`, both orders).

**Later:** an indicator taken off a material, indicators set from the
ABC analysis (SAP MIBC), float time and a share of counts per period,
counts due per storage location's own indicator, a calendar of planned
counts; the tracker's "later: count variants".

### S2m (2026-11-22)

The demand forecasting agent (ADR-0072 As built S2m,
`internal/demandagent`) forecasts a material's demand at each plant of a
company it is extended to, for a planner to decide; it reads material
documents through this ADR's published read and the confirmed sales
orders through sales' (`listSalesOrders`, which gains `plant_id` and
`material_id`), adds no direction to §3 and moves no stock
(x-agent: false). The method, decided by default from the analytics
context's moving average (ADR-0077 §7, "an arithmetic projection, not a
statistical forecast"), the cash forecast's accuracy (ADR-0051 §11, 100
less WAPE, product owner 2026-11-04) and S2k's demand (ADR-0084 §2), and
computed by the agent's run, not by the model:

- **Weeks** of seven days, the latest ending on the run's day, the
  company's date, which has passed as a cash forecast's value date has;
  17 are read (13 for the average, 4 more for its accuracy). The horizon
  is the 8 weeks from the day after. Weeks, not fiscal months: a run on
  any day has whole buckets, and S2n's monthly view is its own.
- **Demand** at a plant is its goods issues of the material: to a cost
  center (201), a delivery's (601) and an intercompany one (643), less
  their reversals (202, 602), the lines' base quantities negated, by
  posting date; scrapping (551), transfers, receipts and count
  differences are no demand (SAP's consumption, from memory), nor are
  lines in transit.
- **The moving average** of the last 13 weeks' demand, never below
  nothing; a week before the material's first document counts as none.
- **Open orders** are what the plant's confirmed sales orders ordered of
  the material and have not delivered (ordered less delivered, per
  standing item, in the base unit by the material's conversion: S2k's
  demand), in the week of the horizon it is due: the day the customer
  asked for, else the order's date; one due before the horizon in its
  first week, one due after it left out.
- **The forecast** of each week of the horizon is the greater of the
  moving average and the open orders due that week: the orders consume
  the forecast (SAP's planning strategy with consumption, from memory).
- **Accuracy** is 100 less WAPE over the last 4 weeks, each forecast by
  the moving average of the 13 weeks before it: Σ|issued − forecast| /
  Σ|forecast|, the percentage rounded to two decimals half away from
  zero (as `money.AbsPercentOf`) and taken from 100, so it is negative
  when the error exceeds the forecast; none when nothing was forecast.
  The open orders of the past are not known again, so the accuracy is the
  statistical part's.
- Quantities stay exact (`big.Rat`); a line writes them to three
  decimals, half away from zero.

**Later** (the plan's S2m and S2n Later): statistical models beyond the
moving average (exponential smoothing, trend and seasonal models, SAP's
forecast models from memory), the forecast kept per material and plant
and read by planning, forecast versions and their accuracy over time,
promotions and collaborative forecasts; reading only orders with a
quantity still open (a sales order stays `confirmed` once delivered in
full, so the run pages a material's delivered orders too, at a cost that
grows with its history).

### S2n (2026-11-22)

A planner reviews a company's demand forecast against supply by
material group and month and fixes a consensus plan version (plan
§S2n). Decided by default (SAP IBP's S&OP from memory, for the SAP
specialist; ADR-0084 As built S2n has the API and schema):

- **Where it lives**: the planning context `mrp` (ADR-0084 decides
  §S2k–S2n), not controlling's planning versions (T93a, ADR-0050 §8):
  those plan amounts by cost element and account for the budget; S&OP
  plans quantities by material group, unit and month, from the planning
  context's own data (the materials it plans, MRP's proposals). A plan
  version is `mrp.sop_plan`, numbered per company; the consensus plan is
  the company's fixed version with the highest number. Capacity is not
  reviewed (no production context, plan S2n).
- **Scope**: the materials the company plans (planning data at a plant,
  whatever its MRP type) that are active, kept in stock and extended to
  the plant, at those plants; one line per material group, base unit
  (quantities of different units are never added) and calendar month.
- **Horizon**: 1 to 12 calendar months, 3 by default, from the day after
  the company's day (`config.DocumentDay`), the first month from that
  day: SAP IBP's S&OP buckets are months (from memory); S2m's weeks are
  its own.
- **Forecast** of a month at a plant: S2m's rule in months, computed by
  the review, not read from the agent's recommendations (its forecast is
  a recommendation's text for a planner, and the forecast kept per
  material and plant is S2m's Later): the greater of the moving average
  of the plant's goods issues over the 13 weeks (91 days) to the
  company's day, never below nothing, spread over the month's days, and
  the orders due in the month: the confirmed sales orders' undelivered
  quantity (due the day the customer asked for, else the order's date)
  and the intercompany orders the plant supplies and has not issued (due
  on their items' delivery dates, else the order's date); one due before
  the horizon in its first month, one due after it left out. The orders
  consume the forecast, so a line's forecast is never below its orders
  (`sop_plan_line_forecast_covers_orders`). An intercompany order is an
  order, not a negative receipt: its goods issue (643) is in the history
  the moving average is of, so taking it off the supply as well would
  count it twice (review round 1). In this S2n differs from S2m's agent,
  whose open orders are sales orders alone. The agent's recommendation on
  each material stays beside it for the planner (`listRecommendations`).
- **Supply**, all in the first month (receipts carry no date, as a
  planning run counts them, ADR-0084 §6): the stock at the plants'
  storage locations (not in transit), the purchase orders' open
  quantities (intercompany orders received included), stock transport
  orders within the company in transit to the plants (one not yet issued
  moves stock between its plants only) and the requisitions planning runs
  proposed that are not ordered, cancelled or rejected.
- **Projected stock** of a month: its opening stock (the first month's
  the plants' stock, after it the month before's projected stock) and
  receipts less the consensus; computed on read, not stored.
- **Consensus**: starts as the forecast; the planner sets it per line
  while the version is a draft; fixed, the version and its lines change
  no more (database triggers); every step audited. People only
  (x-agent: false): no ADR lets an agent fix a plan.

**Later** (the plan's S2n Later, and the tracker's S&OP variants):
capacity and supply optimisation, scenario comparison, approval of the
consensus plan, receipts and requisitions by their dates, a review per
plant or material, the consensus released to planning (the reorder
point or a forecast MRP reads), disaggregation of a group's consensus to
its materials.

### S2q (2026-11-22)

The WMS connector (S2q1, plan-supply-chain.md §S2q) is the integration
context's connector pattern, as the e-invoicing provider's and the
treasury channel's are; no new ADR, and no import between contexts is
added: integration declares the port it posts through
(`integration.WMSDeliveries`), which the composition root wires to
shipping's published `Pick` and `PostGoodsIssue` (`app.wmsDeliveries`),
so shipping stays a leaf (ADR-0083 §1). Decided by default, from SAP's
decentralised WMS (the delivery distributed by IDoc SHP_OBDLV_SAVE_REPLICA
and confirmed by SHP_OBDLV_CONFIRM_DECENTRAL, from memory, for the
specialist):

- **Which plants.** A `wms` connector names the plants its warehouse
  system manages, by company code and plant code (`settings.plants`); a
  plant is managed by at most one active `wms` connector (409
  `integration.plant_has_wms` on create, change or enable, under a
  per-tenant advisory lock so two writes cannot both take a plant). No
  plant is managed until a connector names it; the pilot has no external
  warehouse (Pilot assumption above).
- **The port.** `integration.Warehouse` is provider-neutral: send a
  message by its id, list the confirmations not yet acknowledged, read
  one (at most 1 MiB), acknowledge it. Its one adapter is the stand-in
  (`wmstest`, its own protocol: `PUT /messages/{id}`, `GET
  /confirmations`, `GET /confirmations/{id}`, `POST
  /confirmations/{id}/acknowledge`), refused in production
  (`integration.adapter_not_certified`), called through the egress client
  (public addresses only, no redirects, 30 s a call; the walkthrough's
  stand-in alone by `ERP_WALKTHROUGH_WMS` in development). Credentials are
  the connector's `secret_ref`, resolved under the tenant's own vault
  namespace with no transaction open (PO 2026-11-16), sent as a bearer
  token. The ERP polls; it opens no endpoint for the warehouse system to
  call (as for e-invoicing, PO 2026-11-09).
- **Outbound, through the outbox.** Shipping publishes
  OutboundDeliveryChanged (ADR-0007); the bus consumer `wms-outbound`
  reads a delivery's first event (its creation, version 1) and, when an
  active connector manages its plant, writes the message
  (`integration.wms_message`, outbound, `pending`) in the tenant's
  transaction, once per delivery (`wms_message_once`, so the event read
  twice adds nothing): the outbox of the call. Its body, message type
  `outbound_delivery` schema version 1, is built from the event alone: the
  message id, the company, plant and storage location codes, the
  delivery's id and number, the order's and customer's numbers (never
  the customer's name, PO 2026-11-05) and each item's number, material
  number, quantity and unit. The exchange (`integration.WMSExchange`,
  modelled on the cash exporter) claims a due message `FOR UPDATE SKIP
  LOCKED`, reads its connector, sends with no transaction open and
  records the outcome guarded by the version its claim wrote; a failed
  call is tried after 1, 5, 15 minutes, 1 and 4 hours, and so is one the
  connector cannot make now (disabled, its secret or settings not
  resolving, of another kind, the stand-in in production), which its
  administrator may put right meanwhile; the sixth failure, the WMS's
  refusal (a 4xx but 408 and 429, `integration.wms_refused`) or a lapsed
  last claim (`integration.wms_outcome_unknown`) fails it, a worklist
  (`GET /v1/wms-messages?status=failed`), not sent again (Later: by
  hand). The message id is
  the WMS's deduplication key, so a lapsed claim's resend is no second
  delivery. A delivery created before its plant had a WMS is not sent.
- **Inbound, posted once by the message's id.** Each poll (every 10 s,
  per active connector with a secret) lists the WMS's confirmations and
  records each under an advisory lock of its connector and message id,
  in the transaction that posts it: an inbound row, unique per connector
  and message id, the inbox. A confirmation of type `goods_issue` (its
  own `message_id`, the `outbound_delivery_id` it was sent and the
  quantity picked of each item in the item's unit; an item it was sent
  and does not name is picked at nothing, whatever a person picked of it
  before) posts, for a delivery
  this connector was sent alone (`integration.wms_unknown_delivery`
  otherwise), the picked quantities and the goods issue through
  shipping's API as the exchange's system principal acting for the
  delivery's company, at the delivery's version as it is, in a savepoint:
  shipping's refusal (or an unreadable, too large or other message, or one jsonb cannot
  keep as received, holding `\u0000`, kept as its text:
  `integration.wms_message_unreadable`, `integration.wms_message_too_large`)
  is recorded `refused` with its problem, nothing of it kept; a failure of
  the machinery records nothing and the next poll takes it again, the
  others taken still (an id over 200 characters, which no row holds, is
  reported and never taken). After that commit the WMS is told
  (`acknowledged_at`); one listed again is acknowledged again, not posted
  twice.
- **What a message is never changes** (`wms_message_kept`): its
  connector, direction, id, kind, delivery, plant and body; an inbound
  one's outcome; its acknowledgement once set; a sent or failed
  outbound one's status. Every change is audited
  (`integration.wms_message.queue`, `.send`, `.retry`, `.fail`, `.post`,
  `.refuse`); no event is published (nothing inside the platform reacts
  to one, and none is offered as a webhook until a consumer asks).
- **Locks** (ADR-0008): a connector's create, change or enable takes its
  row (`FOR NO KEY UPDATE`, but on create; S2q2 lowered it from `FOR
  UPDATE`, so a change of its settings or status does not hold up the
  queue's insert, which takes the connector's key share; its `UPDATE`
  names `name` only on a rename, since under the touch trigger an
  `UPDATE` naming a column of a unique key takes the key lock whatever the
  value, so only a rename holds the queue up) and then the
  tenant's wms advisory lock; the queue's insert takes the connector's
  key share (its foreign key) and nothing else; a send's claim and its record take the message's
  row alone (`FOR UPDATE`, `SKIP LOCKED` to claim); a confirmation's
  record takes the advisory lock of its connector and message id, then
  shipping's locks in shipping's order (the delivery, the valuations and
  stock, the sales order), as a person's goods issue takes them without
  the advisory lock. No two writers take two of these in opposite orders.
- **Read:** `GET /v1/wms-messages` (`integration.connector.read`), newest
  first, by connector, direction, status and delivery, of the companies
  the caller may act in (an inbound message refused before its delivery
  was known, to a caller of every company).

- Migrations 000449_wms_connector (the table, the kind check widened
  NOT VALID) and 000450_validate_wms_connector_kind; schema tests
  `db/wms_message_test.go`. Walkthrough step 160, `step_wms`.

**Split:** S2q2, inbound receipts and their goods receipt reported back,
is its own leaf on this connector, table, port and exchange (below); the
plan's §S2q lists what is Later.

### S2q2 (2026-11-22)

Inbound receipts to the warehouse system and their goods receipt
reported back (plan-supply-chain.md §S2q2), on S2q1's connector,
`integration.wms_message` (its kinds widened by 000453, validated by
000454), queue, exchange, port and stand-in. No new ADR and no import
between contexts: integration declares `integration.WMSOrders`, which
the composition root wires to procurement's published order read
(`procurement.Orders.Order`) and goods receipt (`PostReceipt`), with
materials' numbers and tenancy's org unit codes (`app.WMSOrders`).
Decided by default, from SAP's decentralised WMS (the inbound delivery
distributed to the warehouse and its goods receipt confirmed back, from
memory, for the specialist):

- **Which event sends receipts.** A purchase order's release
  (PurchaseOrderReleased, by `releasePurchaseOrder` or its release
  approval, and a migration run's loaded order): the moment the ERP
  expects the goods, as SAP's inbound delivery is created against the
  released order. The bus consumer `wms-outbound` now reads procurement's
  stream beside shipping's. It reads the order as it is when the event is
  read: no longer released (closed, cancelled) or not found, nothing is
  sent. Its stock items (material, plant and storage location) with a
  quantity still to receive (ordered less received; the over-delivery
  tolerance aside) are what the WMS can put away; non-stock items are
  never sent. Intercompany stock transport orders are purchase orders and
  are sent alike (their receipt waits for the issue, as procurement
  refuses it before). Stock transport orders within a company
  (`procurement.stock_transport_order`, S2h) are not sent (Later).
- **One message per order.** `wms_message_once` (000449) keeps a
  document's outbound message once per kind, so the order is sent once,
  to the WMS managing the plant of its first stock item (by item number)
  at a plant an active WMS manages, with its stock items at that plant;
  items at other plants are not sent (received by a person; the queue's
  audit record names their plants). A message per plant needs that
  index's contract (06 §6.4: no drop while a running version reads it),
  so it is Later. The pilot keeps a company's stock at one plant.
- **The message**, type `inbound_receipt` schema version 1, built from
  the order as read: the message id, the company and plant codes, the
  order's id and number, the supplier's number (from the event, never
  its name, PO 2026-11-05), and each item's number, material number,
  storage location code, open quantity, unit and delivery date. Sent,
  retried and failed exactly as a delivery (S2q above).
- **The goods receipt reported back**, type `goods_receipt`: its own
  `message_id`, the `purchase_order_id` it was sent, and each item it
  names by `item_no` with the quantity received in the item's unit; an
  item it does not name receives nothing. Recorded once per connector and
  message id under S2q1's advisory lock, in the transaction that posts
  it: a goods receipt (101) of those items through procurement as the
  exchange's system principal acting for the order's company, dated the
  company's date (`config.EntityDay`), its reference the message id when
  it fits the 40 characters a reference holds, in a savepoint. Refused,
  nothing kept: an order the connector was not sent
  (`integration.wms_unknown_order`), an item it was not sent
  (`integration.wms_item_not_sent`), an item made a non-stock item since
  (`integration.wms_not_stock`), procurement's refusals
  (`procurement.over_delivery`, `procurement.unit_mismatch`,
  `procurement.order_not_released`, ...), and S2q1's unreadable ones. A
  message whose type is neither `goods_issue` nor `goods_receipt` is
  recorded as a `goods_issue`, refused. The inbound row names the order
  (`document_id`, `document_number`) and the material document.
- **Locks.** The queue's insert as for a delivery (the connector's key
  share); a goods receipt's record takes the advisory lock of its
  connector and message id, then procurement's locks in procurement's
  order (the order items `FOR UPDATE` in item order, then the ledger's
  and inventory's), as a person's goods receipt takes them without the
  advisory lock: no two writers take two of these in opposite orders.
- **Read.** `GET /v1/wms-messages` names a receipt message's order
  (`purchase_order_id`, `purchase_order_number`) and filters by it;
  `outbound_delivery_id` is a delivery's alone.
- **Audit** as S2q1, the document's number under its kind's name
  (`outbound_delivery_number` or `purchase_order_number`) and the kind.

**Later for receipts:** a message per plant; stock transport orders
within a company; changes and cancellations of a sent order; a goods
receipt's own document date, delivery note, batches and serial numbers
from the WMS; returns (122) reported by the WMS.

### S2r (2026-11-23)

The TMS connector (plan-supply-chain.md §S2r): an external transport
management system plans the outbound deliveries of the plants its
connector names and returns the shipment it planned each in, with its
carrier and freight cost. It is S2q's connector pattern again, and runs
on S2q's machinery: the settings, the port and its stand-in, the queue,
the exchange and `integration.wms_message`, a message kind each way
(000462, validated by 000463), as S2q2's receipts are. No new ADR and no
import between contexts: integration declares `integration.TMSDeliveries`,
which the composition root wires to shipping's published
`RecordShipment` (`app.tmsDeliveries`), so shipping stays a leaf
(ADR-0083 §1). Decided by default, from SAP TM's freight order and LE-TRA's
shipment (VTTK) with its shipment cost document (VFKK), from memory, for
the specialist:

- **Which plants and deliveries.** A `tms` connector takes a `wms`
  connector's settings (`adapter` `standin`, `base_url`, `plants` by
  company code) and a `secret_ref`; a plant is planned by one active
  `tms` connector at most (409 `integration.plant_has_tms`, under the
  tenant's tms advisory lock, as for a WMS), and may have a `wms` one
  beside it. A delivery is sent when it is created (OutboundDeliveryChanged
  version 1, read from the bus by `wms-outbound`) at a plant an active
  `tms` connector names, once (`wms_message_once`, its kind
  `transport_request`), beside its WMS message when a `wms` connector
  manages the plant too. A delivery created before its plant had a TMS, or
  while its connector is disabled, is not sent (Later: by hand). The pilot
  has no external transport system (Pilot assumption above).
- **The request**, message type `transport_request` schema version 1: the
  same body as a WMS's delivery (S2q), its type aside: the message id, the
  company, plant and storage location codes, the delivery's id and number,
  the order's and customer's numbers (never the customer's name, PO
  2026-11-05) and each item's number, material number, quantity and unit.
  The ship-to address, weights and volumes and the planned dates are
  Later: the event does not carry them, and a TMS that plans by customer
  number holds the customer's master data (SAP TM's replicated business
  partner, from memory). Sent, retried and failed as a WMS message is.
- **What is returned**, message type `shipment`: its own `message_id`, the
  `outbound_delivery_id` it was sent, `shipment_number` and `carrier_code`
  (1 to 40 characters each, as the transport system names them) and
  `freight_cost` (`amount`, a decimal with exactly its currency's
  minor-unit digits, and `currency`, any ISO 4217 code `ref.currency`
  holds, not converted). Recorded on the delivery (`shipment`: number,
  carrier, freight cost in `money.Minor` with its currency, and when) once
  per connector and message id under S2q1's advisory lock, through
  shipping as the exchange's system principal acting for the delivery's
  company, in a savepoint; the inbound row is `recorded`, never `posted`,
  and names no material document. Refused, nothing recorded: a delivery
  the connector was not sent (`integration.tms_unknown_delivery`), a
  delivery with a shipment already (`shipping.shipment_recorded`, 409: a
  delivery is planned in one shipment, and a change of a reported shipment
  is Later), a number or carrier empty or too long, an unknown currency or
  a negative cost (`shipping.invalid_shipment`), an amount without its
  currency's decimals (`request.invalid_amount`), and a message that is
  not a `shipment` of its own id naming a delivery (S2q1's
  `integration.wms_message_unreadable`, recorded as a `shipment`). The
  exchange's own codes stay the WMS's for both kinds
  (`integration.wms_refused`, `integration.wms_outcome_unknown`,
  `integration.wms_message_unreadable`, `integration.wms_message_too_large`);
  a message whose connector is no longer a `tms` one is a failed attempt
  (`integration.not_a_tms`).
- **When.** A shipment is recorded whatever the delivery's status: a
  transport system plans before the goods issue, but its report may come
  after it, and a freight cost often does (SAP's shipment cost document
  follows the shipment's completion, from memory). The trigger that keeps
  a goods-issued delivery fixed lets its shipment alone be set, once
  (`outbound_delivery_issued_fixed`), and a recorded shipment never
  changes (`outbound_delivery_shipment_kept`).
- **Nothing is posted** (SAP's practice, from memory: freight is settled
  from the shipment cost document to the carrier as a supplier, and
  planned delivery costs may go onto the stock's value at the goods
  receipt; both are "later" in the plan): no journal entry, accrual or
  supplier invoice, and the carrier is not matched to a supplier. Freight
  settlement, carrier selection and shipment costs on the stock value are
  the tracker's "later: transport variants" leaf.
- **The published write.** `shipping.Endpoints.RecordShipment` reads the
  delivery under its row lock (ADR-0008), of a company the caller may act
  in, takes no If-Match (the transport system's report is not a change of
  what its caller read), moves the version, audits
  `shipping.outbound_delivery.record_shipment` and publishes
  OutboundDeliveryChanged, whose payload now carries `shipment` (null
  before; asyncapi). No API operation calls it, so no agent's never list
  changes; a person's entry of a shipment is Later.
- **Locks.** The queue's insert of a transport request takes its
  connector's key share, as a WMS message's; a shipment's record takes the
  advisory lock of its connector and message id, then the delivery's row
  (`FOR UPDATE`), as a person's pick or goods issue takes the delivery
  without the advisory lock; no two writers take two of these in opposite
  orders.
- **Read.** `GET /v1/tms-messages` (`integration.connector.read`) lists a
  transport system's messages as `GET /v1/wms-messages` lists a warehouse
  system's, by connector, direction, status and delivery; each list holds
  its own kinds alone. `GET /v1/outbound-deliveries/{id}` gives the
  delivery's `shipment`.
- **Audit** as S2q1, `integration.wms_message.record` for a recorded
  shipment.

**Later for the TMS connector:** a vendor's adapter; the ship-to address,
weights, volumes and dates in the request; a shipment of several
deliveries and stages; changes and cancellations of a sent delivery or a
reported shipment; a shipment entered by a person; freight settlement,
carrier selection and shipment costs on the stock value (the tracker's
"later: transport variants").

### S2s (2026-11-23)

The trade compliance connector (plan-supply-chain.md §S2s): an external
trade compliance service screens the parties and materials of the
outbound deliveries of the plants its connector names (sanctioned party
lists, export classification) and blocks a delivery it refuses. It is
S2q's connector pattern a third time, on S2q's machinery: the settings,
the port and its stand-in, the queue, the exchange and
`integration.wms_message`, a message kind each way (000467, validated by
000468). No new ADR and no import between contexts: shipping declares the
port it asks whether a plant is screened through (`shipping.Screening`,
wired to `integration.TradeScreening`), and integration the port it
records answers through (`integration.GTSDeliveries`, wired to shipping's
published `RecordScreening` by `app.gtsDeliveries`). Decided by default,
from SAP GTS's compliance management (sanctioned party list screening and
the legal control of export classifications, a delivery blocked in the
feeder system until GTS releases it), from memory, for the specialist:

- **Which deliveries are screened.** Every outbound delivery created at a
  plant an active `gts` connector names (`settings.plants`, a `wms`
  connector's settings; one active `gts` connector a plant, 409
  `integration.plant_has_gts`), screened at its creation: shipping writes
  it with `trade_compliance` `pending` in the request's transaction, and
  its creation's event (OutboundDeliveryChanged, version 1, carrying
  `trade_compliance`) queues the screening request once
  (`screening_request`, `wms_message_once`) for the connector screening
  the plant when the event is read. A delivery created before its plant
  was screened is not screened (Later: by hand). The pilot screens
  nothing (Pilot assumption above).
- **It waits for the answer.** A screened delivery's goods issue is
  refused while its screening is `pending` or `blocked` (409
  `shipping.trade_compliance_block`, naming the service's reason code),
  whether a person posts it or a WMS reports it (S2q1's confirmation is
  recorded refused). Best practice and SAP GTS's: nothing leaves the stock
  unscreened; the goods issue is where the goods leave the company, so it
  is the legal control's point. Picking is not blocked: the warehouse may
  prepare a delivery the service has not answered yet; billing follows
  the goods issue, so it waits with it.
- **The request**, message type `screening_request` schema version 1: the
  same body as a WMS's delivery (S2q), its type aside: the company, plant
  and storage location codes, the delivery's id and number, the order's
  and customer's numbers and each item's number, material number,
  quantity and unit. The parties' names and addresses are not sent (no
  customer name leaves in a message, PO 2026-11-05) and neither are the
  materials' classifications: the service holds the business partners'
  and products' master data by those numbers (SAP GTS's replicated
  business partners and classified products, from memory). A service
  that holds no master data is Later. Sent, retried and failed as a WMS
  message is.
- **The answer**, message type `screening`: its own `message_id`, the
  `outbound_delivery_id` it was sent, `result` `released` or `blocked`,
  optionally `reason_code` (1 to 40 characters, the service's own code,
  such as its list hit), and `screened_at` (RFC 3339, required: when the
  service screened). Recorded on the delivery (`status`, `reason_code`,
  `screened_at` as the service reports it) once per connector and message
  id under
  S2q1's advisory lock, through shipping as the exchange's system
  principal acting for the delivery's company, in a savepoint; the
  inbound row is `recorded`, never `posted`. The service's latest answer
  counts, by `screened_at`, not by when it is read: answers are polled
  one by one, a failed read is taken again at the next poll and two
  replicas may record two answers at once, so an answer screened no later
  than the one the delivery holds is refused under the delivery's row
  lock (409 `shipping.screening_superseded`, recorded refused), and a
  release read late cannot undo a later block. Answers are ordered to the
  microsecond the column keeps (the API shows the time to the second); a
  `screened_at` more than 5 minutes past the ERP's clock is refused
  (`shipping.invalid_screening`), so a service's bad clock cannot date a
  release in the future and supersede every later block. The other side of
  that default: a block from a service whose clock runs more than 5
  minutes fast is refused too, for good (its message id is recorded
  once), and the delivery keeps its earlier answer; the refusal is listed
  (`listGTSMessages?status=refused`, `shipping.invalid_screening`) for an
  operator to act on (the alternative, a block always taken with its time
  capped at the ERP's clock, is the product owner's to choose). While the delivery is not
  goods-issued: a later `released` releases a
  block (SAP GTS's release of a blocked document, in the service), a
  later `blocked` blocks it again, a person's release included. Refused,
  nothing recorded: a delivery the connector was not sent
  (`integration.gts_unknown_delivery`), one whose goods issue is posted
  (`shipping.goods_issued`), another result or a reason code too long
  (`shipping.invalid_screening`), and a message that is not a `screening`
  of its own id naming a delivery (`integration.wms_message_unreadable`,
  recorded as a `screening_result`); a message whose connector is no
  longer a `gts` one is a failed attempt (`integration.not_a_gts`).
- **Released by hand.** A person holding `shipping.trade_compliance.release`
  releases a `pending` or `blocked` delivery
  (`releaseOutboundDeliveryTradeComplianceBlock`, If-Match, once: 409
  `shipping.not_trade_compliance_blocked` for one not screened or released
  already), named with the time (`released_by`, and `released_by_name`
  as the person was named then, as a credit release keeps it; `released_at`), the
  service's reason code and time kept; audited
  (`shipping.outbound_delivery.release_trade_compliance_block`) and
  published. The permission is a trade compliance officer's, apart from
  posting the goods issue, and no seeded role holds it (as
  `sales.order.credit_release`). It covers what the service cannot answer:
  a request that failed for good, or a delivery queued after its
  connector was disabled, which would otherwise wait for ever. SAP GTS
  releases a block in GTS, not in the feeder system (from memory); a
  release in the ERP is the default for a service the ERP cannot reach,
  and a later answer of the service still counts. Human-only
  (`x-agent: false`), and on the demand forecasting agent's never list,
  which holds every write of deliveries (ADR-0072; added in batch 93's
  integration, its TestNever checking every delivery operation).
- **Nothing is posted**: no journal entry, no stock moved. Screening of
  sales orders, purchase orders and business partners, customs
  declarations, preference calculation and licences are Later (the
  tracker's "later: trade variants").
- **Locks** (ADR-0008). A delivery's creation reads the active `gts`
  connectors (no lock: a connector enabled at the same moment screens the
  next delivery); the queue's insert takes the connector's key share, as
  a WMS message's; an answer's record takes the advisory lock of its
  connector and message id, then the delivery's row (`FOR UPDATE`), as a
  person's pick, goods issue or release takes the delivery without the
  advisory lock; the goods issue checks the screening under the
  delivery's row lock, so an answer and a goods issue are serialised (no
  check before the lock that guards it). No two writers take two of these
  in opposite orders.
- **Read.** `GET /v1/gts-messages` (`integration.connector.read`) lists a
  trade compliance service's messages as `GET /v1/tms-messages` lists a
  transport system's; `GET /v1/outbound-deliveries/{id}` gives the
  delivery's `trade_compliance`, and `GET
  /v1/outbound-deliveries?trade_compliance_status=blocked` (or `pending`)
  finds the deliveries waiting.
- **Audit** as S2q1, `integration.wms_message.record` for a recorded
  answer; shipping's `shipping.outbound_delivery.record_screening` names
  the status before.

**Later for the trade compliance connector:** a vendor's adapter; the
parties' names and addresses and the materials' classifications in the
request; screening of sales orders, purchase orders and business
partners; a delivery sent again by hand; customs declarations,
preference calculation and licences.

### S1a2 Descriptions by language (2026-11-24)

The first item of "later: material master variants" (plan-supply-chain.md
§S1a2, lane matdesc): a material's description is its default one, and it
may have one further description per other language (SAP MAKT, one text
per material and language, from memory), Arabic for the UAE pilot.

- **Data (migrations 000484, 000485).** `materials.material` gains
  `description_language`, added with the constant default `en` (no
  rewrite; every material created before was described in English, and
  the previous version's inserts, naming none, take it) and its check
  NOT VALID, validated by 000485 (06 §6.4); it does not change
  (`material_language_fixed`, a trigger of its own, so `material_fixed`
  stays as 000435 left it). `materials.material_description` holds the
  further descriptions, removed by their mark and given back as a unit is
  (nothing is deleted, 000011); none is in the default's language
  (`material_description_not_default`), which holds without a lock on the
  material because that language never changes. The down migration
  refuses, rather than drops, a further description (a removed one too)
  and a default in another language than `en` (06 §6.4; batch 95S
  integration, `TestMaterialDescriptionDownRefused`). **Nothing is
  contracted:** `description` stays the default description's text, so
  today's field, the duplicate check, items by material (S1f) and every
  reader of the published API keep working unchanged.
- **Language tags (decided by default).** A BCP 47 tag of a language with
  an optional script and region in canonical form (`ar`, `ar-AE`,
  `zh-Hant`, `es-419`; the contract's and the database's pattern), its
  subtags known as `golang.org/x/text/language` holds the IANA registry
  (`internal/platform/langtag`, new: no helper checked tags before; the web
  workspace's `i18n` matches locales for its own pages only). A tag not
  in canonical form is refused naming it (`iw` is written `he`; 422
  `materials.unknown_language`), as is an unknown subtag. A business
  partner's ISO 639-1 `language` is such a tag. Alternative: any
  well-formed BCP 47 tag with extensions; refused as a key for a text no
  reader would ask for.
- **Reads (decided by default).** `getMaterial` and `listMaterials` take
  the query parameter `language`: the API has no `Accept-Language`
  convention (only the web workspace reads that header, for its own
  texts), and a parameter keeps a response cacheable by its URL and
  visible in the contract. The description is found by RFC 4647 §3.4
  lookup (`ar-AE`, then `ar`), else the default; `description` answers
  it, `description_language` names its language, `default_language` and
  `descriptions` (the default first, then the others by language, byte
  order) give the rest. A read without `language` is answered as before.
  Responses to `createMaterial` and `updateMaterial`, and a material
  change's `material_description`, give the default.
- **Writes (decided by default).** `createMaterial` takes
  `description_language` (`en` when absent) and `descriptions`;
  `updateMaterial` takes `descriptions`, replacing the further
  descriptions as a whole, an empty list removing them, as
  `alternative_units` does; each language once and none the default's
  (422 `materials.duplicate_language`). The change is governed as the
  other fields are (S1b): pending until approved, audited with the texts
  before and after by language, and `MaterialChangeRequested` carries
  `description_language` (a create's) and `descriptions`. The default's
  language does not change in this slice. The texts are master data, not
  personal data, so the audit records them (ADR-0079 concerns free text
  about people). The duplicate check stays on the default description.
- **Events.** MaterialChanged carries `description_language` and
  `descriptions` (the further ones), not required (ADR-0041).
- **SAP (S2o's load).** MAKT-SPRAS is mapped to a tag: SAP's
  one-character keys E, D, F, S, I, P, N, A, R, T, J and 1 (en, de, fr,
  es, it, pt, nl, ar, ru, tr, ja, zh; T002, from memory, on the
  specialist list), and a two-letter key as its ISO 639-1 code. The text
  the load picked before (English, else the first other key) stays the
  default, in its language (`und`, undetermined, with a warning, for a
  key with no tag); the material's other MAKT texts become its further
  descriptions, a key with no tag or a second key of one language left
  out with a warning. The reconciliation compares every description with
  its language. A later run does not change the descriptions of a
  material an earlier run created (the leaf's remainder).
- **Walkthrough:** step 122 (`step_material`): the bracket in Arabic, read
  in `ar`, `ar-AE` and French, the zinc-plated change carrying Arabic and
  French texts, a further English description refused; `step_migrate`
  shows the steel sheet's German MAKT text as a further description.

**Later for descriptions by language:** changing a material's default
language; documents and outputs (sales and purchasing items, e-invoices)
taking the description in their partner's language; the duplicate check
across languages; a later SAP run's new or changed descriptions of a
material an earlier run created.

### S2l1: suppliers' delivery performance (2026-11-24)

The replenishment agent's supplier choice by lead time and reliability
(plan-supply-chain.md §S2l, S2l1; the agent is ADR-0072 As built S2l1)
reads the suppliers' delivery performance from procurement, which
measures it in SQL from posted documents at the moment of the read,
never by a model: `GET /v1/supplier-delivery-performance`
(`getSupplierDeliveryPerformance`, `procurement.order.read`, published
in Go as `procurement.Endpoints.DeliveryPerformance`). It adds no
direction to §3: procurement reads its own orders and receipts, tenancy's
plant and partner's names, as it did. Decided by default (SAP's vendor
evaluation scores on-time delivery from the goods receipt's date against
the schedule line's delivery date, EKET-EINDT, per receipt, from memory;
the specialist list):

- **A delivery** is a goods receipt item (101) of a released or closed
  order's item of the material that no reversal (102) names, at the plant
  the order item names, or else the one the receipt's header names (an
  item without a storage location names none). A return (122) is no
  delivery and is not netted; a reversed receipt is left out whole, as
  its reversal is of the whole receipt. Each item of a receipt counts
  once: an order item delivered in two receipts is two deliveries, as
  SAP's per-receipt score counts it (from memory).
- **On time** is the receipt's posting date on or before the order
  item's delivery date (`delivery_date`, as it is at the read; a change
  of the date by an order change moves it), as the brief states it. A
  receipt whose item has no delivery date is neither on time nor late:
  `dated_receipts` counts those that have one, and the on-time share is
  of them (absent when none is).
- **Lead times** are calendar days: the planned one the item's delivery
  date less the order date, the observed one the receipt's posting date
  less the order date; each the average over the supplier's receipts (the
  planned over the dated ones), rounded half away from zero to a whole
  day by numeric's `round`, never through a float. The on-time share is a
  whole percent, rounded the same way.
- **The window** is a range of posting dates, both ends counted, by
  default the 365 days ending on the plant's company's date (its time
  zone, T72n): long enough to hold a year's seasons, short enough that an
  old supplier's record does not hide a recent change. A window that ends
  before it starts, or is longer than 1,096 days (three years, a leap day
  among them), is 400 `request.invalid_parameter`, so a read stays
  bounded. The replenishment agent's check asks for the year ending on
  its run's day (`from`, `to`), not the default, so a run reviewed on a
  later day is held to the year it was planned in (ADR-0072 As built
  S2l1).
- **Thin history.** A supplier with fewer than `min_receipts` (3)
  receipts in the window is `thin`; with 3 or more `measured`; with none
  it has no row. The agent prefers a measured supplier over a thin one or
  one with none (ADR-0072 As built S2l1): three receipts are the fewest
  from which an on-time share is more than one or two events. The
  threshold is a constant of the read, stated in every answer, so a
  consumer need not know it.
- **Answer.** Per supplier its number and name (partner's `Names`, the
  name masked as everywhere for a blocked person), in the order of the
  supplier number, then the id; `totals` sum every row (ROLLUP in the
  same statement, so the totals and the rows are one snapshot). A
  report, not paged: one row per supplier that delivered the material to
  the plant in the window; `supplier_id` narrows it to one, and the
  answer echoes it. The totals give the lead times averaged over every
  receipt too, which the rows' rounded figures cannot give. A plant that
  does not exist, an org unit that is no plant, or a plant of a company
  the caller cannot act in is 422 `request.reference_not_found`, as a
  legal entity is (`tenancy.Accessible`).
- **Indexes.** Migration 000487 indexes order items by material
  (`purchase_order_item_by_material`, which listPurchaseOrders'
  `material_id` filter uses too) and 000488 goods movements by the one
  they reverse (`goods_movement_by_reversal`, which a receipt's
  `reversed_by` uses too), both partial and built concurrently.

**Later** (the tracker's replenishment agent variants): a planned
delivery time kept as master data (SAP's info record or the material's
plant data, from memory) and the order dated back from the need by it;
a weighted supplier score with price, quantity reliability and quality.
