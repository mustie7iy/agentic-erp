# ADR-0011: Hash-chained audit events written in the same transaction

**Status:** proposed

## Context
Auditors need complete, tamper-evident evidence of who did what, including
agents and configuration changes.

## Decision
- `audit.audit_event` is insert-only, partitioned by month, with a hash chain
  per tenant (`prev_hash`, `hash` over a canonical encoding). Each business
  transaction writes its audit event in the same database transaction, so an
  action cannot succeed without its audit record.
- The chain head is periodically anchored to write-once object storage
  (S3 Object Lock) so even a database administrator cannot rewrite history
  unnoticed.
- Per-tenant chains avoid a global serialization point; chain extension takes
  a short row lock on the tenant's chain head.

## Alternatives
- **Audit via CDC/logs after the fact:** can be lost; not same-transaction.

## Consequences
- One extra insert per business transaction.
- Verification is a scan per tenant and period.
