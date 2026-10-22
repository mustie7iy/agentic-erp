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

## Amendment (2026-10-21, T73c): where a request came from
The product owner decided that the API's audit trail records each
person's client address. Each event carries `client_address` (inet): a
person's address in the web workspace as erp-web saw it, otherwise the
client's connection as the router saw it through its load balancer; null
for work no request made (workers, workflows) and for events recorded
before. The router believes erp-web's forwarded address from erp-web's
SPIFFE identity alone, and the cell believes the router's from the
router's alone (ADR-0033, ADR-0052 "Built in T73c").

The address is personal data, so it sits beside the event, outside the
hash chain: the chain covers what happened, and the erasure of a tenant
(ADR-0021) blanks the addresses of its events without breaking the chain
or its anchors (ADR-0026). An event stays insert-only otherwise; the
erasure may change its address alone, to null. The address has the
event's retention. Only people holding `audit.event.read` see it on
`listAuditEvents` (product owner 2026-10-22: people only). Every other
reader, an agent, a service principal or any kind added later, gets each
event without it (the field omitted, not null), and the address is not
even read from the database for them. `reqctx.Masked` is an allow-list of
people, so a new kind of principal is masked until decided otherwise.
