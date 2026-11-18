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

### 8. Inventory account determination (S1d, planned)

The configuration's account determination (`companies[].account_determination`)
gains keys per valuation class for the stock account (SAP BSX), the
offsetting accounts of issues to consumption, scrapping and initial
entries (GBB with its modifiers), price differences (PRD) and
revaluation (UMB); GR/IR stays the existing key (WRX). The SAP specialist
confirms the mapping before S1d is built.

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
  registered as 409 `inventory.insufficient_stock`), so the lines are
  written each location's receipts first (review round 1: a circle of
  transfers, or a chain's reversal, went below zero line by line though
  not in all; `TestCircularTransfers`). `TestIssuesAtOnce` holds the
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
- Walkthrough step 124, `step_goods_movements`; the pilot's settings add
  Maple's `MD` range (`material_document`), as the fixture's companies
  have theirs.
