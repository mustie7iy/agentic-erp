# Architecture decision records

Each record is immutable once accepted; a later decision supersedes it with a
new record. Status values: proposed, accepted, superseded.

| ADR | Decision | Status |
|---|---|---|
| [0001](0001-modular-monolith.md) | Modular monolith with bounded contexts built to split | proposed |
| [0002](0002-postgresql.md) | PostgreSQL as the system of record | proposed |
| [0003](0003-multi-tenancy-and-cells.md) | Pooled multi-tenancy with row-level security; cells for scale | proposed |
| [0004](0004-identifiers.md) | UUIDv7 ids plus document numbers from number ranges | proposed |
| [0005](0005-money-and-currency.md) | Integer minor units, three currencies on ledger lines | proposed |
| [0006](0006-append-only-ledger.md) | Append-only ledger, derived balances, partitioning by fiscal year | proposed |
| [0007](0007-events-and-outbox.md) | Domain events via transactional outbox, CloudEvents | proposed |
| [0008](0008-concurrency-and-idempotency.md) | Optimistic locking and idempotency keys | proposed |
| [0009](0009-extensibility.md) | Governed custom fields in jsonb | proposed |
| [0010](0010-authentication-authorization.md) | OIDC authentication; permission, company and SoD authorization | proposed |
| [0011](0011-audit-trail.md) | Hash-chained audit events in the same transaction | proposed |
| [0012](0012-long-running-operations.md) | Long-running work as Temporal workflows behind API operations: multi-step, cross-module, human waits, timers and batches; postings stay one ACID activity; ids from business keys | accepted |
| [0013](0013-api-style.md) | Resource-oriented REST with OpenAPI 3.1 as the source of truth | proposed |
| [0014](0014-request-pipeline-and-code-generation.md) | Request pipeline, unit of work and server code generation (ogen) | proposed |
| [0015](0015-control-plane.md) | Control plane for tenants on the admin listener with operator tokens | proposed |
| [0016](0016-object-storage.md) | Write-once object storage behind one port; tenant-prefixed keys; anchors locked | proposed |
| [0017](0017-secrets.md) | Secrets by reference (environment, mounted file, Vault), resolved per replica | proposed |
| [0018](0018-key-management.md) | Per-tenant field-encryption keys wrapped by a master key ring; rotation re-seals | proposed |
| [0019](0019-kms-master.md) | KMS master key in front of the ring; tenant keys move by re-wrapping | proposed |
| [0020](0020-direct-uploads.md) | Direct uploads for documents above the request limit: presigned PUT or a signed path on the API | proposed |
| [0021](0021-offboarding-lifecycle.md) | Offboarding lifecycle: export to locked storage, grace period, erasure; the tenant row and audit trail stay | proposed |
| [0022](0022-tenant-directory-and-router.md) | Global tenant directory in its own database, written by the cells; a router verifies, looks up and forwards | proposed |
| [0023](0023-global-control-plane.md) | Global control plane over the directory: cells register, placement forwards provisioning, moves in three steps | proposed |
| [0024](0024-ambient-aws-credentials.md) | Ambient AWS credentials (environment, web identity, instance metadata) for the S3 store and the KMS master; static keys optional | proposed |
| [0025](0025-scheduled-audit-verification.md) | Scheduled audit verification: every chain recomputed against its anchors in rounds; an alarm once when one stops verifying | proposed |
| [0026](0026-export-chunking-and-audit-detail.md) | Exports written in parts of bounded size; audit details kept as written, minimized at write time, never rewritten | proposed |
| [0027](0027-operator-roles.md) | Operator roles from a token claim: read, provision, operate, admin; no claim means every role in development, none in production | proposed |
| [0028](0028-key-rotation-policy.md) | Tenant keys older than a configured age are rotated by a worker, as an operator would | proposed |
| [0029](0029-reonboarding-from-export.md) | A tenant comes back from its export, verified against the manifest; the row copy of a move between cells | proposed |
| [0030](0030-cross-cell-verification.md) | A tenant's fingerprint (rows per table, audit chain head) on each cell; the global plane compares two cells before a move switches | proposed |
| [0031](0031-per-tenant-identity-providers-and-sessions.md) | Identity providers registered per tenant decide a token's tenant; multi-factor by acr/amr where the tenant demands it; server-side sessions with lifetime and idle timeout | accepted |
