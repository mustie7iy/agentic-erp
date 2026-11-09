# ADR-0029: Re-onboarding a tenant from its export

**Status:** proposed

## Context
An offboarding export (ADR-0021, ADR-0026) holds every row of a tenant and
the head of its audit chain, locked in object storage. Until T24 it could be
read but not loaded: a tenant erased by mistake stayed erased, and a move
between cells (05-scalability §5.4) had no row copy of the platform's own.

## Decision

**The export is the portable form of a tenant.** `erp-server import` reads
a manifest and its parts from the cell's store and writes the tenant into
the cell in one transaction: the tenant row (created, or brought back from
its tombstone when the tenant was erased here; a live tenant is refused),
then every table's rows in passes over foreign keys, from the JSON the
export wrote, through `json_populate_record` with generated columns left to
the database.

**Every step is checked against the manifest.** A part's hash and row count
must match before anything is written; a table that already holds rows of
the tenant refuses the import; after the rows are in, the audit chain head
must equal the one the export recorded. Any mismatch rolls the import back.

**Values the database keeps are restored as exported.** The import sets
`app.importing` to the tenant for its transaction (as the erasure sets
`app.erasing`); a trigger that computes a column on insert keeps the value an
imported row carries while it names the row's tenant: a period's opened
count (ADR-0060 §6, migration 000229). Likewise a trigger that refuses
inserting a row in any state but its first inserts it in the state it was
exported in (migration 000230).

**Outboxes are not imported.** Their events were delivered where they
happened; re-publishing them would replay history to consumers.

## Alternatives
- **Logical replication with a row filter** for moves. Deployment-specific
  and live; the export is a verified snapshot both cells' operators can
  hold in their hands.
- **A dump of SQL.** Ties the export to one database version; JSON rows
  through `json_populate_record` survive added columns (defaults apply).

## Consequences
- A move between cells is: freeze (ADR-0023), export in the source cell,
  import in the destination, `:move` the directory entry, unfreeze; the
  import's checks are the verification step.
- Rows reference other tenants' nothing by construction (ADR-0003), so an
  import never collides with another tenant.
- Partitioned tables need their partitions in the destination (the
  maintenance job creates coming months; older history needs the partitions
  of its years, which the import does not create).

## As built
- **Rows in any lifecycle state** (migration 000230, found by batch58's
  ci-local): `platform.importing(tenant)` is true while `app.importing`
  names the tenant. Under it a period close is inserted signed off, a close
  task done and checked, a parked entry posted, a consolidation run final
  or superseded, a carry-forward run superseded, and a parked entry's or an
  entry template's lines, and a run's members and balances, whatever their
  parent's state; every other insert, every update and the checks that do
  not judge a lifecycle (a carry-forward's chain and equity account, one
  run in progress, one final run) are held as before. Only the importer
  sets the setting; `erp_app` could, but row-level security confines it to
  its own tenant's rows (as 000229 decided).
- **A check that reads rows not imported yet waits.** A trigger such as a
  journal entry's company-keeps-ledger check refuses with a check
  violation, not a foreign key violation, while the rows it reads are in a
  later table; the importer lets a table refused for either wait for the
  next pass, and when a pass inserts nothing it names every waiting table
  with what refused it.
- **Defaults apply.** A row names the columns it carries; one it does not
  name, as in an export written before the column was added, takes the
  column's default (the importer inserts only the named columns), where
  `json_populate_record` alone gave NULL. A period exported before 000227
  thus gets an opened count of 0, as 000227 gave the periods it found.
- **A carry-forward run is erased with its tenant** (000230): its trigger
  refused every delete, so `platform.erase_tenant`, and with it a move's
  erasure of the source cell's copy, failed for a tenant holding one; a
  delete while `app.erasing` names the run's tenant now passes, as
  `platform.forbid_mutation` lets one.
- **Not yet restored** (tracker, ci:batch58): other insert triggers
  judge a row by a state that may have moved on since it was written: a
  journal entry and a clearing need their period open, an approval
  decision its request pending, a purchase order item its order editable,
  a tax return's snapshot its return prepared, a plan line its category
  unlocked, a memo record its liquidity item active; and assets,
  acquisitions, retirements, transfers, depreciation runs and batches,
  down payment requests, GR/IR clearings, tax returns, order changes,
  revaluation runs, variance comments and accrual proposals start in their
  first state. A tenant holding such rows past those states is not yet
  importable.
