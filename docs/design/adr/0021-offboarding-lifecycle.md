# ADR-0021: Offboarding lifecycle: export to locked storage, grace period, erasure

**Status:** proposed

## Context
Offboarding has been a status since T11: the tenant's requests are refused
and nothing is deleted (03-data-model: lifecycles instead of deletes). A
tenant that leaves still has a right to its data and a right to have it
removed; a cell should not carry departed tenants' rows and objects
forever; and the platform's own rules (append-only tables, forced
row-level security, a hash-chained audit trail) must survive the one
deletion they were built to prevent.

## Decision

**Three stages, recorded on the tenant row.** Offboarded → exported
(`exported_at`, `export_ref`, `erase_after`) → erased (`erased_at`). The
tenant row never goes: it is the record that the tenant existed and what
became of its data.

**Export first, to locked objects.** The offboarding worker writes every
table of the tenant (every table with a `tenant_id`, partitioned parents
rather than partitions) as JSON Lines under
`tenants/<id>/exports/<offboarding time>/`, with a manifest listing each
table, its rows and the hash of its file, and the audit chain head at the
time. Objects are locked for the retention period like audit anchors. The
export is keyed by the offboarding time and written once: a round that
stored objects but did not get to record itself finds them and describes
what is stored.

**A grace period, then erasure** (`ERP_OFFBOARD_GRACE`, default 30 days):
the worker erases tenants whose `erase_after` has passed; an operator
erases earlier through `POST /control/v1/tenants/{id}:erase`, guarded by
`If-Match` and recorded as the operator.

**Erasure is one database function and one prefix.**
`platform.erase_tenant(uuid)` runs as the owner, the only role that may
delete, in the tenant's scope so forced row-level security applies, and
deletes from every tenant table but the audit schema, in passes so foreign
keys resolve themselves. Insert-only tables let the erasure through: the
function names the tenant being erased in a transaction-local setting the
mutation guard checks. In the store, every object under the tenant's prefix
goes but the locked ones (exports, audit anchors), which stay until their
retention ends.

**The audit trail stays.** What happened in the tenant, hash-chained and
anchored, is the one record that must outlive the data it describes; the
export's and the erasure's own entries are its last. The outbox events
`TenantExported` and `TenantErased` are published like any other change.

## Alternatives
- **Erase at offboarding.** No export, no grace: a mistaken offboarding
  would be unrecoverable, and the tenant's right to its data unmet.
- **Keep everything.** Cells accumulate departed tenants, and the right to
  erasure is unmet.
- **Erase row by row from the application.** The application role has no
  `DELETE` grants by design; giving it some to erase would weaken the
  rule for every request. One owner-run function keeps the exception in
  one place.
- **Erase the audit trail too.** The trail is the evidence of what the
  tenant did and of the erasure itself; retention of financial records
  outlives the tenant.

## Consequences
- Four columns on `tenancy.tenant`, one function, one worker, one control
  operation, `List` and `Delete` on the store port (the only deletion).
- Audit entries keep their details after the erasure; minimizing what
  entries carry about erased objects is a separate design.
- Large tenants export table by table in memory; streaming exports and
  multipart uploads are for when a tenant's table exceeds a replica's
  memory.

## Amendment (2026-10-16, ADR-0031)

A tenant's identity providers and authentication policy are tenant data and
are exported, imported and fingerprinted with it. Its sessions
(`identity.session`) are not: they are ephemeral, end when the tenant moves,
and their idle-time writes would change the fingerprint while a move is
verified (ADR-0030). They are skipped like the outboxes.

## As built (2026-11-16, batch 76 review)

- **An export reads each table in its primary key's order** (the row's text
  for a table without one, which none has today). A resumed export keeps the
  parts an earlier round stored, so the rows behind part *n* must be the same
  in every round: read in whatever order the plan of the day returned them, a
  resumed export wrote some rows twice and others never, and the erasure
  after it lost those for good.
- **The erasure deletes the rows in the transaction that records it.** The
  worker reads that the tenant is erasable, deletes its objects, then in one
  transaction deletes its rows as the worker role and records the erasure as
  the application role in the tenant's scope (`database.Rescope`), so the
  rows it records are those deleted. Before, the rows went in a transaction
  of their own: a round whose object deletion or record then failed left the
  next round nothing to count, and the erasure was recorded with no rows. The
  objects counted are those deleted and kept by the round that completes the
  erasure: a store failing part-way may already have deleted others, and an
  object store keeps no record of that round.

## As built (2026-11-17, batch 77 review)

- **The erasure keeps the export and the audit anchors by itself, not by the
  store's lock** (coordinator 2026-11-17, ERP practice). It never deletes a
  key under the tenant's `exports/` or `audit/anchors/` and counts each as
  kept (`objects_kept`). The lock could not keep them readable: Object Lock
  needs a versioned bucket, where a DELETE without a version id is not
  refused but writes a delete marker, so the export vanished from reads
  (no reinstatement, ADR-0029) and the verifier found the anchors gone and
  reported the chain tampered; and a store without retention
  (`ERP_OBJECT_RETENTION=0`) deleted them outright. `S3.Delete` therefore
  never answers `ErrLocked`; `Dir` still refuses a locked object, which the
  erasure counts as kept. Production refuses `ERP_OBJECT_RETENTION` of 0 or
  shorter than `ERP_OFFBOARD_GRACE`, so an export stays locked at least until
  the tenant is erased. The S3 test double is versioned as the bucket is: a
  DELETE writes a delete marker and keeps the version, locked or not.
