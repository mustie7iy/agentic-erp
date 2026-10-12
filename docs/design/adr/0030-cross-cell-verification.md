# ADR-0030: Cross-cell verification of a tenant

**Status:** proposed

## Context
A move between cells (05-scalability §5.4) is: freeze the entry
(ADR-0023), copy the rows (export in the source, import in the destination,
ADR-0029), verify, switch the entry, unfreeze. Until T25 the verification
was whatever the import checked on its way in, visible only in the
destination's logs; the operator switching the entry had nothing to compare
the two cells with, and nothing to show afterwards for why the switch was
safe.

## Decision

**A tenant has a fingerprint.** Each cell's control plane answers
`GET /control/v1/tenants/{id}/fingerprint` with the rows of every table
carrying a `tenant_id` (outboxes left out: their events stay in the cell
they happened in) and the audit chain head, read in one snapshot in the
tenant's scope. The read role may read it; the tenant's version is its
`ETag`.

**The global control plane compares two cells.**
`POST /directory/v1/tenants/{id}:verify` with a `cell` asks the tenant's
cell and the cell named for the fingerprint, with the operator's own token,
and answers both fingerprints, `equal`, and one line per difference: a cell
without the tenant, a table's rows, the audit chain head. The entry is not
changed, in any status: an operator verifies before freezing to see how far
a copy is, and after to switch.

**Cells are told apart from the tenant.** A cell without the tenant is a
difference (the copy has not started), a cell that does not answer is 502
`platform.cell_unreachable`, a cell that refuses the operator is 502
`platform.cell_refused`, a cell that never registered is 503. The
destination must be a registered cell of the tenant's region other than the
tenant's own (422).

## Alternatives
- **Verify in the destination only** (the import's checks). They prove the
  destination matches the manifest, not the source as it is now: rows
  written in the source after the export (a frozen tenant still takes
  reads, and a tenant verified before freezing takes writes) show up only
  in a comparison of the two cells.
- **Hash every row** instead of counting. Exact, and proportional to the
  tenant's size on every call; counts and the audit chain head (which hashes
  every change) catch what a copy misses at the cost of one count per
  table. Row hashing can be added as a deeper mode later.
- **Let the directory `:move` verify on its own.** It would need the
  operator's token for both cells and would turn a routing update into a
  minute-long request; two explicit steps keep the switch instant and the
  evidence in the operator's hands.

## Consequences
- The move sequence has a visible step 3: `erp directory verify -id X
  -cell eu-2` exits non-zero until the cells agree, so a script can wait.
- The fingerprint is one count per table (about a hundred statements) in
  one read-only transaction; large tenants take seconds, which is what a
  move's read-only window is for.
- Outboxes and `computed_at` aside, two cells holding the same tenant give
  the same fingerprint by construction: every table carries `tenant_id`
  (ADR-0003), and the chain head covers every audited change.
