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
