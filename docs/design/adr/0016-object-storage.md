# ADR-0016: Write-once object storage behind one port

**Status:** proposed

## Context
Captured invoice documents, bank files and audit anchors are files, not
rows (02-architecture, ADR-0011). Until T12 the platform recorded only their
hashes: documents were not kept, bank files were rendered again when needed,
and the chain was never anchored. The design names S3-compatible object
storage; the first client decides the cloud.

## Decision

**One port, two stores.** `internal/platform/blobstore.Store` has `Put`
(once) and `Get`. `Dir` stores files below a directory for development and
tests; `S3` talks to any S3-compatible service over HTTP with Signature
Version 4 implemented in the package (no SDK), path-style addressing and
conditional writes. Configuration picks one (`ERP_OBJECT_DIR` or
`ERP_OBJECT_S3_*`).

**Write once.** A key is never overwritten: `O_EXCL` on disk,
`If-None-Match: *` on S3. Objects that must survive an administrator carry
a compliance-mode retention lock (audit anchors, `ERP_OBJECT_RETENTION`).

**Keys lead with the tenant**: `tenants/<tenant id>/<kind>/<name>`, with
`documents/<sha256>`, `bank-files/<id>.xml` and `audit/anchors/<seq>.json`
as the kinds, so a tenant's objects can be exported or erased together.

**The database row carries the hash; the store carries the bytes.** Every
read compares the two and refuses a mismatch, so storage cannot change what
the platform hands out. Anchors invert this: the locked object is the
reference the chain and the anchor row are checked against.

**Optional.** Without a store the platform runs as before T12 (hashes only,
files rendered again, no anchoring) and says so at start; downloads answer
503.

## Alternatives
- **Files in PostgreSQL (bytea).** Simple, but ties document volume to the
  database's backups, replication and cells, and offers no write-once lock.
- **A cloud SDK per provider.** Large dependencies for three calls; the
  S3 protocol is the common denominator of every provider and MinIO.
- **Presigned uploads from day one.** Needed for documents above 5 MB
  (04 §4.11), left for the upload design.

## Consequences
- A worker and a gauge more (`erp_audit_chains_unanchored`); migration
  000020 grants the worker role its reads.
- Documents and bank files are downloadable, by permission; bank file
  content is never offered to agents.
- Secrets for the store come from the environment until the vault exists.
