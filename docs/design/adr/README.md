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
| [0012](0012-long-running-operations.md) | Long-running work as Temporal workflows behind API operations | proposed |
| [0013](0013-api-style.md) | Resource-oriented REST with OpenAPI 3.1 as the source of truth | proposed |
| [0014](0014-request-pipeline-and-code-generation.md) | Request pipeline, unit of work and server code generation (ogen) | proposed |
