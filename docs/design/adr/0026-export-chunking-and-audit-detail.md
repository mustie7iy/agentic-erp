# ADR-0026: Chunked exports, and audit details after an erasure

**Status:** proposed

## Context
The offboarding export (ADR-0021) built each table in memory before
storing it, so a tenant's largest table bounded a replica's memory; and the
audit trail an erasure leaves behind keeps the details its entries carried,
which the T17 review listed as a minimization question.

## Decision

**Exports are written in parts.** The worker streams a table's rows and
stores an object every `ChunkRows` rows (10,000 by default), named
`<table>.00001.jsonl`, `.00002.jsonl` and so on; the manifest lists every
part with its rows and hash. Memory per table is one part, whatever the
table's size; a retried export finds the parts already stored.

**Audit details are not rewritten.** The hash of an entry covers its detail;
rewriting details would break the chain the trail exists to keep, and
anchors would disagree. Minimization happens at write time instead: the
convention, checked in review, is that details carry identifiers, statuses,
amounts and operator subjects, not the names, addresses or bank details
those identifiers point to. The erased tenant's trail therefore says what
happened to which object, not what the object contained; the export holds
the content, under its retention.

## Alternatives
- **Multipart uploads to the store.** Needed only for a single part above
  the store's object limit; parts of 10,000 rows stay far below it.
- **Rewriting details to hashes after erasure.** Breaks the chain, or needs
  a chain over detail digests from the start, which would be a new trail
  format for every tenant to answer a question only erased tenants raise.
- **Deleting the trail with the tenant.** Removes the evidence of the
  erasure and of what preceded it; retention of the record outlives the data.

## Consequences
- Manifests carry a `part` per entry; readers of an export concatenate a
  table's parts in order.
- The review checklist gains an item: no personal data in audit details.
- `ChunkRows` is a worker field rather than a setting until a deployment
  needs to tune it.
