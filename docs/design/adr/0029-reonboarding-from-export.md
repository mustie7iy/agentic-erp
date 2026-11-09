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
count (ADR-0060 §6, migration 000229).

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
